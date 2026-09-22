package main

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"xhunter/hunt"
)

// eventSink 把结构化事件与人类可读日志分开写出：事件流给程序消费（每行一条 JSON），
// 日志给人看。
//
// 事件行的形状是外部契约：`{"type": "<事件名>", ...payload}`——载荷摊平到顶层，
// 调用方读字段不必多走一层；键名小写，与使用手册的事件清单一一对应。
//
// 信封四字段（`type` / `bounty_id` / `trace_id` / `ts`）由 sink 统一盖在每一行上
// （FR-11.3、INV-5）：调用点只交业务载荷，"每个事件都带信封"因此是结构事实，
// 不靠每个发出点各自记得。信封字段最后写入，业务载荷覆盖不了它们。
type eventSink struct {
	events   io.Writer
	logs     io.Writer
	start    time.Time // 心跳计时起点；零值表示不计时
	bountyID string
	traceID  string
	now      func() time.Time // 可注入，便于测试断言稳定时间戳

	// writeErr 记住第一次写失败：通道断裂必须让进程以环境错误收敛（FR-10.4），
	// 而写失败的现场只在 sink 里，所以由它记着、由装配层在收尾后取。
	writeErr error
}

func (s *eventSink) Emit(ev hunt.ExternalEvent) error {
	line := make(map[string]any, len(ev.Payload)+4)
	for k, v := range ev.Payload {
		line[k] = v
	}
	line["type"] = ev.Type
	line["bounty_id"] = s.bountyID
	line["trace_id"] = s.traceID
	line["ts"] = s.clock().UTC().Format(time.RFC3339)
	b, err := json.Marshal(line)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(s.events, string(b)); err != nil {
		s.noteWriteErr(err)
		return err
	}
	return nil
}

// noteWriteErr 只记住第一次失败：第一次的原因才是根因，后续重试失败是它的后果。
func (s *eventSink) noteWriteErr(err error) {
	if s.writeErr == nil {
		s.writeErr = err
	}
}

// Failed 报告事件通道是否断过。
func (s *eventSink) Failed() error { return s.writeErr }

func (s *eventSink) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *eventSink) Log(level, msg string, kv ...any) {
	fmt.Fprintf(s.logs, "[%s] %s", level, msg)
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(s.logs, " %v=%v", kv[i], kv[i+1])
	}
	fmt.Fprintln(s.logs)
}

// Heartbeat 输出一次心跳：长任务「还活着」的唯一凭据，因此不能退化成静默丢弃。
func (s *eventSink) Heartbeat(p hunt.Phase) error {
	payload := map[string]any{"phase": string(p)}
	if !s.start.IsZero() {
		payload["elapsed_ms"] = time.Since(s.start).Milliseconds()
	}
	return s.Emit(hunt.ExternalEvent{Type: "heartbeat", Payload: payload})
}
