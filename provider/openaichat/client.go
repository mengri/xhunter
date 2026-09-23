// Package openaichat 实现 OpenAI 的对话补全协议（自持 wire 与流式分帧）。
//
// 协议版本基线 —— 改动本包前先读这一段：
//
//   - 端点：POST {baseURL}/chat/completions（基址来自配置，本包不预设任何主机）
//   - 版本标识：本协议**没有版本头**，因此基线只能记形状本身
//   - 形状基线（2026-09）：请求 {model, messages[{role,content,tool_calls,tool_call_id}],
//     tools[{type:function,function{name,description,parameters}}], stream, stream_options.include_usage}；
//     分片 {choices[{delta{content,tool_calls[{index,id,function{name,arguments}}]},finish_reason}],usage}；
//     结束标记 data: [DONE]；参数为**字符串**形式的 JSON
//   - 用量口径：prompt_tokens 是**全部输入**（已含缓存），缓存读在
//     prompt_tokens_details.cached_tokens；completion_tokens 是全部输出
//   - 兼容面：自建网关与第三方托管端普遍实现这套形状，因此本包不假设任何厂商专属字段；
//     也因此对"结束标记缺失"采取容忍策略（见过 finish_reason 即按正常结束）
//   - 需要动本包的场合：分片字段改名、工具调用增量的形状变化、结束语义变化
//
// 鉴权不在本包：请求头由调用方按配置拼好后传进来（见 Config.Headers）。
package openaichat

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

// chatPath 是对话补全的协议路径。路径与载荷形状属于协议，主机属于部署。
const chatPath = "/chat/completions"

// doneMarker 是流的正常结束标记。
var doneMarker = []byte("[DONE]")

// eventBuffer 是事件通道的缓冲深度，只吸收"读取快于消费"的抖动。
const eventBuffer = 16

// defaultResponseHeaderTimeout 是等待响应头的时间上限。不设整体超时：
// 流式响应天然长，整体超时会把正常的长生成掐断；挂起由任务级预算与取消兜住。
const defaultResponseHeaderTimeout = 60 * time.Second

// Config 是构造协议客户端所需的全部事实，且都已解析完毕。
type Config struct {
	// BaseURL 是端点基址（不含协议路径）。必填：本包不预设主机。
	BaseURL string
	// Model 是上游的模型标识，原样透传。
	Model string
	// Headers 是已解析好的请求头——包括鉴权头。鉴权形状属于部署，不属于协议：
	// 调用方按配置决定放哪个头、加不加前缀，本包只管原样发出去。
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
			// 一个任务里有几十次调用，连接复用是常态路径而不是优化项。
			MaxIdleConnsPerHost: 4,
		}}
	}

	return &Client{
		endpoint: strings.TrimRight(cfg.BaseURL, "/") + chatPath,
		model:    cfg.Model,
		headers:  cfg.Headers,
		client:   client,
		caps: llm.Caps{
			MaxContextTokens: cfg.MaxContextTokens,
			// 一次响应里的多个调用按位置分别拼装，因此并行调用是被支持的。
		},
	}, nil
}

func (c *Client) Capabilities() llm.Caps { return c.caps }

// Infer 发起一次推理，返回可增量消费的流。
//
// 返回成功只代表"上游接受了这次请求"，不代表会有什么内容：内容、调用、用量、出错
// 全部通过事件流表达，因此调用方必须把"流结束"当作唯一的分界。
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
	msgs, err := toWireMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	return json.Marshal(wireRequest{
		Model:    c.model,
		Messages: msgs,
		Tools:    toWireTools(req.Tools),
		Stream:   true,
		// 用量只在流末额外回报一次；不打开它，预算维度就只剩本地估算。
		StreamOptions: &streamOptions{IncludeUsage: true},
	})
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

// Cancel 中止本次流。重复调用是安全的：取消函数本就如此，而清理只在读取端发生一次。
func (s *session) Cancel() error {
	s.cancel()
	return nil
}

// run 把上游的流翻译成中立事件，直到结束、出错或被取消。
//
// 所有出口都关闭事件通道，因此消费端只需要一个信号——"通道关了"。正常结束与中断的
// 区别由最后一条事件表达，不需要另外查返回码，也就不会出现"忘了查"的漏判。
func (s *session) run() {
	defer close(s.ch)
	defer s.body.Close()
	defer s.cancel()

	calls := adapter.NewCallBuilder()
	reader := adapter.NewSSEReader(s.body)
	sawFinish := false

	for {
		data, ok, err := reader.Next()
		if err != nil {
			s.fail("stream_read_error", "读取响应流失败："+err.Error(), true)
			return
		}
		if !ok {
			// 上游没给结束标记就断了。已经见过结束原因时按正常结束处理：
			// 相当一部分网关省略末尾标记，把这种情况一律当截断会让它们全部不可用。
			if !sawFinish {
				s.fail("stream_truncated", "响应流在结束之前中断", true)
				return
			}
			break
		}
		if bytes.Equal(bytes.TrimSpace(data), doneMarker) {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			s.fail("stream_decode_error", "响应分片不是合法 JSON："+err.Error(), true)
			return
		}
		if chunk.Error != nil {
			// 上游在流内报错：它比任何本地判断都更清楚出了什么事，原样带上。
			s.fail("upstream_error", chunk.Error.Error(), chunk.Error.Retryable())
			return
		}
		if chunk.Usage != nil {
			if !s.emit(llm.Event{Kind: llm.EvUsage, Usage: chunk.Usage.usage()}) {
				return
			}
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				if !s.emit(llm.Event{Kind: llm.EvText, Text: choice.Delta.Content}) {
					return
				}
			}
			for _, d := range choice.Delta.ToolCalls {
				calls.Add(d.Index, d.ID, d.Function.Name, d.Function.Arguments)
			}
			if choice.FinishReason != "" {
				sawFinish = true
			}
		}
	}
	s.finish(calls)
}

func (s *session) emit(ev llm.Event) bool {
	select {
	case s.ch <- ev:
		return true
	case <-s.ctx.Done():
		return false
	}
}

// fail 上报一次流中断。必须显式上报：静默结束会让半截响应被当成完整回答。
func (s *session) fail(kind, msg string, retryable bool) {
	s.emit(llm.Event{Kind: llm.EvError, Err: &llm.Fault{Kind: kind, Message: msg, Retryable: retryable}})
}

// finish 在流正常结束时收尾：先交出拼装好的调用，再给结束标记。
// 顺序不能反——结束标记之后消费端就不再期待内容了。
func (s *session) finish(calls *adapter.CallBuilder) {
	for _, ev := range calls.Events() {
		if !s.emit(ev) {
			return
		}
	}
	s.emit(llm.Event{Kind: llm.EvEnd})
}
