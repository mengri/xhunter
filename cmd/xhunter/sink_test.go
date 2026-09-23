package main

import (
	"io"
	"os"
	"testing"
	"time"

	"xhunter/hunt"
)

// 心跳写失败也必须被 Failed() 看到（FR-10.4「事件（含心跳）写入失败时记录并终止」）：
// 通道健康覆盖事件与心跳两条写路径——心跳内部走 Emit，两条路径共用同一个出口与同一份记录。
func TestEventSink_FailedCoversHeartbeatWrites(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道失败：%v", err)
	}
	reader.Close() // 读端关掉：写入必然失败
	defer writer.Close()

	sink := &eventSink{events: writer, logs: io.Discard, start: time.Now()}
	if err := sink.Failed(); err != nil {
		t.Fatalf("还没写就应无错误：%v", err)
	}
	if err := sink.Heartbeat(hunt.PhaseInfer); err == nil {
		t.Error("写入已断的通道必须返回错误")
	}
	if err := sink.Failed(); err == nil {
		t.Error("心跳写失败必须被 Failed() 看到")
	}
}
