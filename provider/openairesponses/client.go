// Package openairesponses 实现 OpenAI 的 Responses 协议（自持 wire 与流式分帧）。
//
// 协议版本基线 —— 改动本包前先读这一段：
//
//   - 端点：POST {baseURL}/responses（基址来自配置，本包不预设任何主机）
//   - 版本标识：本协议没有版本头（早期 beta 阶段的头部已不需要），基线记形状本身
//   - 形状基线（2026-09）：请求 {model, instructions, input[
//     {type:message,role,content[{type:input_text|output_text,text}]} |
//     {type:function_call,call_id,name,arguments} |
//     {type:function_call_output,call_id,output}], tools[{type:function,name,description,parameters}], stream}；
//     流是**语义事件**：response.output_text.delta、response.function_call_arguments.delta/done、
//     response.output_item.added/done、response.completed/incomplete/failed、error
//   - 结束语义：**没有统一结束标记**，以 response.completed / response.incomplete /
//     response.failed 收尾；因此没见到它们就按截断上报（容忍缺失会把半截响应伪装成完整回答）。
//     少数网关会在结束事件后再补一个 [DONE]，按同一含义处理
//   - 参数形式：arguments 是**字符串**形式的 JSON（与对话补全一致）
//   - 用量口径：input_tokens 是**全部输入**（已含缓存），缓存读在
//     input_tokens_details.cached_tokens；output_tokens 是全部输出
//   - 需要动本包的场合：事件名或载荷字段变化、结束语义变化、input 条目种类变化
//
// 鉴权不在本包：请求头由调用方按配置拼好后传进来（见 Config.Headers）。
package openairesponses

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"xhunter/llm"
	"xhunter/provider/adapter"
)

// responsesPath 是推理的协议路径。主机来自配置。
const responsesPath = "/responses"

// doneMarkers 是少数网关在结束事件后追加的标记。
var doneMarkers = [][]byte{[]byte("[DONE]")}

// eventBuffer 是事件通道的缓冲深度，只吸收"读取快于消费"的抖动。
const eventBuffer = 16

// defaultResponseHeaderTimeout 是等待响应头的时间上限；不设整体超时，理由同其它协议。
const defaultResponseHeaderTimeout = 60 * time.Second

// Config 是构造协议客户端所需的全部事实，且都已解析完毕。
type Config struct {
	// BaseURL 是端点基址（不含协议路径）。必填：本包不预设主机。
	BaseURL string
	// Model 是上游的模型标识，原样透传。
	Model string
	// Headers 是已解析好的请求头——包括鉴权头。鉴权形状属于部署，不属于协议。
	Headers map[string]string
	// HTTPClient 允许注入代理与超时策略。为 nil 时使用内置默认。
	HTTPClient *http.Client
	// MaxContextTokens 是模型接受的输入上限，用于声明能力。必填，不得估算。
	MaxContextTokens int
}

// Client 是本协议的实现。
type Client struct {
	endpoint string
	model    string
	headers  map[string]string
	client   *http.Client
	caps     llm.Caps
}

// New 构造一个协议客户端。缺端点或缺上限都在这里失败：它们同属"还没开始跑"的问题。
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("缺少 BaseURL：该协议没有默认主机，端点必须显式给出")
	}
	if cfg.MaxContextTokens <= 0 {
		return nil, fmt.Errorf("%w: model %q", adapter.ErrNoContextWindow, cfg.Model)
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: defaultResponseHeaderTimeout,
			MaxIdleConnsPerHost:   4,
		}}
	}

	return &Client{
		endpoint: strings.TrimRight(cfg.BaseURL, "/") + responsesPath,
		model:    cfg.Model,
		headers:  cfg.Headers,
		client:   client,
		caps: llm.Caps{
			MaxContextTokens: cfg.MaxContextTokens,
		},
	}, nil
}

func (c *Client) Capabilities() llm.Caps { return c.caps }

// Infer 发起一次推理，返回可增量消费的流。
func (c *Client) Infer(ctx context.Context, req llm.Request) (llm.Session, error) {
	body, err := c.buildBody(req)
	if err != nil {
		return nil, err
	}

	streamCtx, cancel := context.WithCancel(ctx)
	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("构造请求失败：%w", err)
	}
	c.setHeaders(httpReq)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		cancel()
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("请求被取消：%w", err)
		}
		return nil, fmt.Errorf("请求未能送达：%w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		cancel()
		return nil, adapter.StatusErrorFrom(resp)
	}

	s := &session{ctx: streamCtx, ch: make(chan llm.Event, eventBuffer), cancel: cancel, body: resp.Body}
	go s.run()
	return s, nil
}

func (c *Client) buildBody(req llm.Request) ([]byte, error) {
	wire, err := toWire(c.model, req)
	if err != nil {
		return nil, err
	}
	return json.Marshal(wire)
}

func (c *Client) setHeaders(r *http.Request) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "text/event-stream")
	// 配置头最后覆盖：协议头是默认值，部署侧给的才是权威。
	for k, v := range c.headers {
		r.Header.Set(k, v)
	}
}

// ============================================================ 会话

type session struct {
	ctx    context.Context
	ch     chan llm.Event
	cancel context.CancelFunc
	body   io.ReadCloser
}

func (s *session) Events() <-chan llm.Event { return s.ch }

func (s *session) Cancel() error {
	s.cancel()
	return nil
}

// run 把上游的语义事件翻译成中立事件，直到结束、出错或被取消。
func (s *session) run() {
	defer close(s.ch)
	defer s.body.Close()
	defer s.cancel()

	calls := adapter.NewCallBuilder()
	reader := adapter.NewSSEReader(s.body)

	for {
		data, ok, err := reader.Next()
		if err != nil {
			s.fail("stream_read_error", "读取响应流失败："+err.Error(), true)
			return
		}
		if !ok {
			s.fail("stream_truncated", "响应流在 completed 之前中断", true)
			return
		}
		if isDoneMarker(data) {
			s.finish(calls)
			return
		}

		var ev streamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			s.fail("stream_decode_error", "响应分片不是合法 JSON："+err.Error(), true)
			return
		}

		switch ev.Type {
		case "response.output_text.delta":
			if ev.Delta != "" && !s.emit(llm.Event{Kind: llm.EvText, Text: ev.Delta}) {
				return
			}

		case "response.function_call_arguments.delta":
			// 参数分片：按位置配对累积，含义由使用方解释。
			calls.Add(ev.OutputIndex, "", "", ev.Delta)

		case "response.function_call_arguments.done":
			if ev.Arguments != "" {
				calls.SetFull(ev.OutputIndex, ev.Arguments)
			}

		case "response.output_item.added", "response.output_item.done":
			// 条目里带标识与函数名；done 还可能带完整参数（不逐片吐的上游）。
			if ev.Item != nil && ev.Item.Type == "function_call" {
				calls.Add(ev.OutputIndex, ev.Item.CallID, ev.Item.Name, "")
				if ev.Item.Arguments != "" {
					calls.SetFull(ev.OutputIndex, ev.Item.Arguments)
				}
			}

		case "response.completed":
			if ev.Response != nil && ev.Response.Usage != nil {
				u := llm.Usage{
					InputTokens:  ev.Response.Usage.InputTokens,
					OutputTokens: ev.Response.Usage.OutputTokens,
				}
				if d := ev.Response.Usage.InputTokensDetails; d != nil {
					u.CachedInputTokens = d.CachedTokens
				}
				if !s.emit(llm.Event{Kind: llm.EvUsage, Usage: u}) {
					return
				}
			}
			s.finish(calls)
			return

		case "response.incomplete":
			// 输出不完整（例如达到上限）：它仍是一次完整的响应，只是内容被截断。
			// 当错误上报会把它变成环境问题，而真正该做的是调高上限。
			s.finish(calls)
			return

		case "response.failed":
			retryable := false
			msg := "上游报告生成失败"
			if ev.Response != nil && ev.Response.Error != nil {
				msg = ev.Response.Error.Error()
				retryable = ev.Response.Error.Retryable()
			}
			s.fail("response_failed", msg, retryable)
			return

		case "error":
			if e := ev.asError(); e != nil {
				s.fail("upstream_error", e.Error(), e.Retryable())
				return
			}
		}
		// 其余事件（创建、进行中、内容部件、推理摘要、内置工具调用等）没有中立槽位，
		// 忽略：上游加事件类型不该让读取失败。
	}
}

func (s *session) emit(ev llm.Event) bool {
	select {
	case s.ch <- ev:
		return true
	case <-s.ctx.Done():
		return false
	}
}

func (s *session) fail(kind, msg string, retryable bool) {
	s.emit(llm.Event{Kind: llm.EvError, Err: &llm.Fault{Kind: kind, Message: msg, Retryable: retryable}})
}

func (s *session) finish(calls *adapter.CallBuilder) {
	for _, ev := range calls.Events() {
		if !s.emit(ev) {
			return
		}
	}
	s.emit(llm.Event{Kind: llm.EvEnd})
}

func isDoneMarker(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	for _, m := range doneMarkers {
		if bytes.Equal(trimmed, m) {
			return true
		}
	}
	return false
}
