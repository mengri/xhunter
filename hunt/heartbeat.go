package hunt

import (
	"context"
	"sync"
	"time"
)

// defaultHeartbeatInterval 是任务级心跳的默认间隔。装配层不传（0）即用它——「不配」是取默认，
// 不是「关掉心跳」：长任务在平台侧必须能看见"还活着"。
const defaultHeartbeatInterval = 30 * time.Second

// startHeartbeat 按固定间隔上报任务级心跳（phase 每次取当前阶段），直到 ctx 取消或 stop 被调用。
//
// 为什么归业务（Session）掌管生命周期，而不是命令行的 Run 外圈：Finalize 也在 Run 内跑——
// 时钟若滴答到 Run 返回为止，一条心跳会落在 hunt_end 之后，而 hunt_end 必须是最后一条事件。
// 正确边界是「Prepare 末尾起、Finalize 的终态事件块之前停」。
//
// stop 幂等且**阻塞到心跳 goroutine 真正退出**：这样"停完之后一条心跳也不会再发"是结构保证，
// 而不是靠时序运气——否则一个在途的心跳仍可能落到 hunt_end 之后。sink == nil 时是空操作。
// 心跳的写失败忽略：写失败由出口自己记着、由既有的通道收口处理，这里不终止任何东西。
func startHeartbeat(ctx context.Context, sink EventSink, interval time.Duration, phase func() Phase) (stop func()) {
	if sink == nil {
		return func() {}
	}
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}

	done := make(chan struct{})
	stopped := make(chan struct{})
	var once sync.Once
	stop = func() {
		once.Do(func() { close(done) })
		<-stopped
	}

	go func() {
		defer close(stopped)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-t.C:
				_ = sink.Heartbeat(phase())
			}
		}
	}()
	return stop
}
