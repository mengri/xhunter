package gate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"xhunter/hunt"
)

// runnerEnvWhitelist 是子进程能继承的环境变量**白名单**。
//
// 门禁跑的是仓库自己的命令，但它不该拿到 Xhunter 的运行环境：那里面有模型密钥与 git 凭据。
// 用白名单而不是黑名单——黑名单永远追不上"下一个会泄密的变量名"。
var runnerEnvWhitelist = []string{
	"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR",
	"GOPATH", "GOMODCACHE", "GOCACHE", "GOFLAGS", "GOPROXY", "CARGO_HOME", "NODE_PATH",
}

// runnerEnvFixed 是门禁进程的固定环境：非交互、无颜色、不求凭证提示。
//
// 门禁必须能自己跑完——它等一个永远不来的提示，只会把墙钟烧在等待上，最后以超时收场，
// 而"超时"会被读成环境问题，掩盖真正的结论。
var runnerEnvFixed = []string{"CI=1", "NO_COLOR=1", "GIT_TERMINAL_PROMPT=0"}

// maxCollectedOutput 是单条输出流的收集上限（判据上界 + 1：多读一个字节就能判定"超了"）。
const maxCollectedOutput = 1<<20 + 1

// evidenceHead / evidenceTail 是喂给模型的证据长度（字符）：门禁失败时模型要看得见
// 大致原因，但不该拿到整份构建日志。
const (
	evidenceHead = 2000
	evidenceTail = 2000
)

// Runner 是 `hunt.GateRunner` 的默认实现：**数组直启**跑命令、按门禁声明的判据判定、并缓存
// 同一改动状态下的结论。
//
// 它不认识任何业务概念，也不碰工作区——写盘只在核心，门禁是**验证**性质的动作。
type Runner struct {
	mu    sync.Mutex
	cache map[string]hunt.GateResult
}

// NewRunner 给出一份执行器。缓存以「门禁名 + 改动指纹」为键：模型常反复调同一条门禁，
// 而测试动辄数十秒到数分钟——缓存既省时间，也避免重复调用催生出重复检查点。
func NewRunner() *Runner {
	return &Runner{cache: map[string]hunt.GateResult{}}
}

var _ hunt.GateRunner = (*Runner)(nil)

// Run 执行一条门禁。见 `hunt.GateRunner` 的契约：error 只表示执行失败。
func (r *Runner) Run(ctx context.Context, root string, g hunt.Gate, fingerprint string) (hunt.GateResult, error) {
	// 判据不可判定的条目在这里再挡一次：清单侧已经拦过，但门禁也可能由别处直接调用
	// （收尾补跑走的就是同一条路）。挡在跑命令之前，比跑完才发现判不了好。
	if err := g.Validate(); err != nil {
		return hunt.GateResult{}, fmt.Errorf("门禁 %q 不可用：%w", g.Name, err)
	}
	dir := root
	if g.Dir != "" {
		dir = filepath.Join(root, g.Dir)
	}
	key := g.Name + "\x00" + fingerprint
	if cached, ok := r.lookup(key); ok {
		return cached, nil
	}
	res, err := r.execute(ctx, dir, g)
	if err != nil {
		return hunt.GateResult{}, err
	}
	r.remember(key, res)
	return res, nil
}

func (r *Runner) lookup(key string) (hunt.GateResult, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res, ok := r.cache[key]
	if ok {
		res.Cached = true
	}
	return res, ok
}

func (r *Runner) remember(key string, res hunt.GateResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[key] = res
}

func (r *Runner) execute(ctx context.Context, dir string, g hunt.Gate) (hunt.GateResult, error) {
	if g.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, g.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, g.Argv[0], g.Argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(runnerEnv(), g.Env...)
	cmd.Stdin = nil
	// 独立进程组：超时时要杀的是**整棵进程树**（`npm test` 会拉起一堆子进程），
	// 只杀直接子进程会让孤儿进程继续占着管道，Wait 永远不返回。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 3 * time.Second

	var stdout, stderr limitedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)

	exitCode, startFailed := exitCodeOf(err)
	if startFailed {
		return hunt.GateResult{}, fmt.Errorf("门禁 %q 的命令无法启动：%s（%v）", g.Name, g.Argv[0], err)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return hunt.GateResult{}, fmt.Errorf("门禁 %q 超过 %s 未结束", g.Name, g.Timeout)
	}

	verdict, reason := g.Expect.Judge(stdout.String(), stderr.String(), exitCode)
	if verdict == hunt.ExpectUndecidable {
		// 判不了就是判不了：不猜、不降级成"通过"。
		return hunt.GateResult{}, fmt.Errorf("门禁 %q 无法判定：%s", g.Name, reason)
	}
	return hunt.GateResult{
		Name:       g.Name,
		Passed:     verdict == hunt.ExpectPass,
		ExitCode:   exitCode,
		DurationMS: duration.Milliseconds(),
		Summary:    reason,
		Evidence:   evidence(stdout.String(), stderr.String()),
	}, nil
}

// runnerEnv 组装子进程环境：白名单继承 + 固定项。
func runnerEnv() []string {
	env := make([]string, 0, len(runnerEnvWhitelist)+len(runnerEnvFixed))
	for _, k := range runnerEnvWhitelist {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return append(env, runnerEnvFixed...)
}

// exitCodeOf 从 cmd.Run 的结果里取退出码；第二个返回值表示"命令根本没跑起来"
// （PATH 上找不到、没有执行权限……）——那是环境问题，不是质量结论。
func exitCodeOf(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), false
	}
	return -1, true
}

// evidence 拼出给模型看的证据：头尾保留、中间省略，并标出省略了多少。
func evidence(stdout, stderr string) string {
	var sb strings.Builder
	if strings.TrimSpace(stdout) != "" {
		sb.WriteString("stdout:\n")
		sb.WriteString(clip(stdout))
	}
	if strings.TrimSpace(stderr) != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("stderr:\n")
		sb.WriteString(clip(stderr))
	}
	return sb.String()
}

func clip(s string) string {
	const limit = evidenceHead + evidenceTail
	if len(s) <= limit {
		return s
	}
	return s[:evidenceHead] + fmt.Sprintf("\n……（省略 %d 字符）……\n", len(s)-limit) + s[len(s)-evidenceTail:]
}

// limitedBuffer 收集输出但**不无界**：超过上限就把多余的丢掉并记住"超了"。
type limitedBuffer struct {
	buf  bytes.Buffer
	over bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	room := maxCollectedOutput - b.buf.Len()
	if room <= 0 {
		b.over = true
		return len(p), nil // 收不下的也算"写成功"：否则命令会因 EPIPE 提前失败，判据反而失真。
	}
	if len(p) > room {
		b.buf.Write(p[:room])
		b.over = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) String() string { return b.buf.String() }
