package main

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// 信号必须真的接到上下文的取消上：这是"外部打断"进引擎的唯一通道。
// 没有它，进程被默认处置直接杀掉——没有终态事件、没有清理、退出码也不是 3
// （FR-1.7、AC-6）。
//
// 本用例给**自己**发信号。若 signalContext 没注册处理器，默认处置会就地杀掉
// 测试进程——那本身就是最直接的失败信号：信号根本没被接管。
func TestSignalContext_CancelsOnSignal(t *testing.T) {
	cases := []struct {
		name string
		sig  syscall.Signal
	}{
		{"SIGTERM", syscall.SIGTERM},
		{"SIGINT", syscall.SIGINT},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, stop := signalContext()
			defer stop()

			if err := syscall.Kill(os.Getpid(), tc.sig); err != nil {
				t.Skipf("环境不支持发送信号：%v", err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatalf("%s 没有取消上下文：信号未接到循环的取消入口上", tc.name)
			}
		})
	}
}

// stop() 兼作"取消 + 摘除信号处置"：`signal.NotifyContext` 的 stop 会先取消上下文。
// 装配处因此把它 defer 在 Run 之后——那时 Outcome 已经拿到，取消无害。
// 这条行为值得钉住，免得有人以为 stop 只摘处置、不取消，从而把它放在 Run 之前。
func TestSignalContext_StopCancelsContext(t *testing.T) {
	ctx, stop := signalContext()
	stop()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("stop() 应同时取消上下文（signal.NotifyContext 的既有行为）")
	}
	stop() // 幂等：重复调用不该 panic
}
