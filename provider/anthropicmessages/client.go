// Package anthropicmessages 实现 Anthropic 的 Messages 协议（自持 wire 与流式分帧）。
//
// 协议版本基线 —— 改动本包前先读这一段：
//
//   - 端点：POST {baseURL}/messages（基址来自配置，本包不预设任何主机）
//   - 版本头：`anthropic-version: 2023-06-01`（本包默认发送；部署侧可用配置头覆盖，
//     升级协议版本因此是改配置而不是改代码）
//   - 形状基线（2026-09）：请求 {model, max_tokens(**必填**), system(顶层字段),
//     messages[{role,content[blocks]}], tools[{name,description,input_schema}], stream}；
//     助手侧工具调用是 `tool_use` 内容块（`input` 是**对象**），工具结果是
//     **用户消息里的 `tool_result` 块**（可带 is_error）；
//     事件序列 message_start → content_block_start → content_block_delta
//     （text_delta / input_json_delta.partial_json / thinking_delta）→ content_block_stop
//     → message_delta（stop_reason + usage.output_tokens）→ message_stop；另有 ping 与 error
//   - 结束语义：严格以 message_stop 收尾；没有它就按截断上报——容忍缺失会把半截响应
//     伪装成完整回答，比直接失败危险得多
//   - 需要动本包的场合：内容块种类增加、事件名或载荷变化、版本头升级到不兼容形态
//
// 鉴权不在本包：请求头由调用方按配置拼好后传进来（见 Config.Headers）——
// 本协议惯用 `x-api-key`（裸值），但那是部署约定，不是本包的代码。
package anthropicmessages

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

// messagesPath 是对话补全的协议路径。主机来自配置。
const messagesPath = "/messages"

// defaultVersion 是缺省协议版本。版本头是本协议的硬要求，缺了会被拒；
// 部署方需要更新的版本时用配置头覆盖即可。
const defaultVersion = "2023-06-01"

// eventBuffer 是事件通道的缓冲深度，只吸收"读取快于消费"的抖动。
const eventBuffer = 16

// defaultResponseHeaderTimeout 是等待响应头的时间上限；不设整体超时，理由同其它协议。
const defaultResponseHeaderTimeout = 60 * time.Second

// errNoMaxOutput 表示生成上限未配置。本协议的请求体要求 max_tokens，
// 因此它不是可选项：没有它请求不成立，而静默填一个数字等于替部署方决定预算。
var errNoMaxOutput = errors.New("本协议要求生成上限（max_tokens），未配置")

// Config 是构造协议客户端所需的全部事实，且都已解析完毕。
type Config struct {
	// BaseURL 是端点基址（不含协议路径）。必填：本包不预设主机。
	BaseURL string
	// Model 是上游的模型标识，原样透传。
	Model string
	// Headers 是已解析好的请求头——包括鉴权头（本协议惯用 x-api-key）。
	// 也可用来覆盖 anthropic-version。
	Headers map[string]string
	// HTTPClient 允许注入代理与超时策略。为 nil 时使用内置默认。
	HTTPClient *http.Client
	// MaxContextTokens 是模型接受的输入上限，用于声明能力。必填，不得估算。
	MaxContextTokens int
	// MaxOutputTokens 是请求体的 max_tokens，本协议必填。
	MaxOutputTokens int
}

// Client 是本协议的实现。
type Client struct {
	endpoint  string
	model     string
	headers   map[string]string
	client    *http.Client
	caps      llm.Caps
	maxOutput int
}

// New 构造一个协议客户端。
//
// 缺端点、缺模型标识、缺上限（上下文或生成）都在这里失败：它们同属"还没开始跑"
// 的一类问题，留到运行中途才暴露，代价是已经消耗掉的轮次与预算全部作废。
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("缺少 BaseURL：该协议没有默认主机，端点必须显式给出")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("缺少模型标识：请求体必须给出 model")
	}
	if cfg.MaxContextTokens <= 0 {
		return nil, fmt.Errorf("%w: model %q", adapter.ErrNoContextWindow, cfg.Model)
	}
	if cfg.MaxOutputTokens <= 0 {
		return nil, fmt.Errorf("%w: model %q", errNoMaxOutput, cfg.Model)
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
		endpoint:  strings.TrimRight(cfg.BaseURL, "/") + messagesPath,
		model:     cfg.Model,
		headers:   cfg.Headers,
		client:    client,
		maxOutput: cfg.MaxOutputTokens,
		caps: llm.Caps{
			MaxContextTokens:  cfg.MaxContextTokens,
			ParallelToolCalls: true,
		},
	}, nil
}

func (c *Client) Capabilities() llm.Caps { return c.caps }

// Infer 发起一次推理，返回可增量消费的流。
func (c *Client) Infer(ctx context.Context, req llm.Request) (llm.Session, error) {
	wire, err := toWire(c.model, c.maxOutput, req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("请求体序列化失败：%w", err)
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

func (c *Client) setHeaders(r *http.Request) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "text/event-stream")
	r.Header.Set("anthropic-version", defaultVersion)
	// 配置头最后覆盖：协议头（含版本）是默认值，部署侧给的才是权威。
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

// run 把上游的流翻译成中立事件，直到结束、出错或被取消。
//
// 本协议有明确的终止事件（message_stop），因此没有它就按截断上报。
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
			s.fail("stream_truncated", "响应流在 message_stop 之前中断", true)
			return
		}

		var ev streamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			s.fail("stream_decode_error", "响应分片不是合法 JSON："+err.Error(), true)
			return
		}

		switch ev.Type {
		case "message_start":
			// 输入用量在这里，输出用量要到 message_delta 才给。
			if ev.Message != nil && ev.Message.Usage != nil {
				u := llm.Usage{InputTokens: ev.Message.Usage.InputTokens, OutputTokens: ev.Message.Usage.OutputTokens}
				if !s.emit(llm.Event{Kind: llm.EvUsage, Usage: u}) {
					return
				}
			}

		case "content_block_start":
			if ev.Block != nil && ev.Block.Type == "tool_use" {
				calls.Add(ev.Index, ev.Block.ID, ev.Block.Name, "")
				// 有些上游在起始事件里就给完整参数、之后不再逐片吐，因此记一份备选。
				if in := bytes.TrimSpace(ev.Block.Input); len(in) > 0 && !bytes.Equal(in, []byte("{}")) {
					calls.SetFull(ev.Index, string(in))
				}
			}

		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" && !s.emit(llm.Event{Kind: llm.EvText, Text: ev.Delta.Text}) {
					return
				}
			case "input_json_delta":
				calls.Add(ev.Index, "", "", ev.Delta.PartialJSON)
			}
			// 思考块等其它增量没有中立槽位，直接丢弃：混进正文会把"模型的推理过程"
			// 变成"模型的回答"。要保留它，应先给中立事件加槽位。

		case "message_delta":
			if ev.Usage != nil {
				u := llm.Usage{OutputTokens: ev.Usage.OutputTokens}
				if !s.emit(llm.Event{Kind: llm.EvUsage, Usage: u}) {
					return
				}
			}

		case "message_stop":
			s.finish(calls)
			return

		case "error":
			if ev.Error != nil {
				s.fail("upstream_error", ev.Error.Error(), ev.Error.Retryable())
				return
			}
		}
		// ping 与未知事件忽略：上游加事件不该让读取失败。
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
