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
type eventSink struct {
	events io.Writer
	logs   io.Writer
	start  time.Time // 心跳计时起点；零值表示不计时
}

func (s *eventSink) Emit(ev hunt.ExternalEvent) error {
	line := make(map[string]any, len(ev.Payload)+1)
	for k, v := range ev.Payload {
		line[k] = v
	}
	line["type"] = ev.Type
	b, err := json.Marshal(line)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(s.events, string(b))
	return err
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
