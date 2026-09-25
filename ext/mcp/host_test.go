package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"xhunter/ext"
)

// 本文件钉住外挂通道的四件事：懒启动与映射、**起不来/崩了/挂住**三种失败都不得让主循环
// 挂住或失败、回收要杀整棵进程树、以及不可信边界（子进程拿不到我们的环境）。
//
// 假扩展不用外部命令：**测试二进制重入自己**（`os.Args[0]` ＋ 一个开关环境变量），
// 因此用例不依赖 PATH 上装了什么，也不依赖 shell——跨平台、无外部依赖（NFR-1/NFR-8）。

// fakeEnv 是重入开关：设了它，测试二进制就变成假扩展进程（不跑任何用例）。
const fakeEnv = "MCP_FAKE_MODE"

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeEnv); mode != "" {
		serveFake(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// serveFake 是一个够用的 MCP 假扩展：握手 ＋ 四个能力方法，按 mode 演不同的故障。
func serveFake(mode string) {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for in.Scan() {
		line := in.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			return // 输入不是 JSON：没什么可做的，退出（父侧会把它读成"输出断了吗"）
		}
		switch req.Method {
		case methodInitialize:
			reply(req.ID, initializeResult{
				ProtocolVersion: protocolVersion,
				ServerInfo:      serverInfo{Name: "fake", Version: "1"},
			})
		case methodCapabilities:
			if mode == "env" {
				// 把子进程**实际拿到的环境**原样送回来，供"不可信边界"用例检查。
				reply(req.ID, capsResult{Available: true, Languages: os.Environ(),
					Precision: string(ext.PrecisionSemantic)})
				continue
			}
			reply(req.ID, capsResult{Available: true, Languages: []string{"go", "py"},
				CanResolve: true, Precision: string(ext.PrecisionSemantic)})
		case methodLocate:
			switch mode {
			case "crash":
				os.Exit(3) // 收到请求就崩：模拟扩展进程死在调用里
			case "hang":
				time.Sleep(2 * time.Minute) // 永不答复：模拟扩展挂住（父侧必须自己走开）
				os.Exit(0)
			}
			reply(req.ID, locateResult{
				File: "a.go", Range: byteRangeWire{Start: 10, End: 42},
				Sites:        []siteWire{{File: "a.go", Range: byteRangeWire{Start: 10, End: 42}}},
				FilesChanged: 1, Occurrences: 2, Unknown: true,
				Precision: string(ext.PrecisionSemantic),
			})
		case methodParse:
			if mode == "unknown" {
				// 一个我们不认识的取值：按"没判过"处理，绝不猜成 broken。
				reply(req.ID, parseResult{Verdict: "whatever"})
				continue
			}
			reply(req.ID, parseResult{Verdict: "ok"})
		case methodEnclose:
			if mode == "unknown" {
				reply(req.ID, encloseResult{Found: false})
				continue
			}
			reply(req.ID, encloseResult{Found: true, File: "a.go",
				Range: byteRangeWire{Start: 0, End: 30}, Precision: string(ext.PrecisionSemantic)})
		default:
			replyErr(req.ID, -32601, "unknown method "+req.Method)
		}
	}
}

func reply(id uint64, result any) {
	raw, _ := json.Marshal(result)
	line, _ := json.Marshal(rpcResponse{JSONRPC: jsonrpcVersion, ID: id, Result: raw})
	os.Stdout.Write(append(line, '\n'))
}

func replyErr(id uint64, code int, msg string) {
	line, _ := json.Marshal(rpcResponse{JSONRPC: jsonrpcVersion, ID: id,
		Error: &rpcError{Code: code, Message: msg}})
	os.Stdout.Write(append(line, '\n'))
}

// newFakeHost 给出一台指向假扩展的宿主。mode 决定假扩展演哪种故障。
func newFakeHost(t *testing.T, mode string, timeout time.Duration) *Host {
	t.Helper()
	h := New(Config{
		Command:   os.Args[0],
		Timeout:   timeout,
		ID:        "fake",
		Version:   "1",
		Languages: []string{"go"},
		Env:       []string{fakeEnv + "=" + mode},
	})
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// 懒启动 ＋ 映射：构造不拉进程，首次调用才拉；定位结果按契约逐项落位。
func TestHost_StartsLazilyAndLocates(t *testing.T) {
	h := newFakeHost(t, "ok", 5*time.Second)
	if h.cmd != nil {
		t.Fatal("构造不该启动进程（懒启动，FR-13.5）")
	}
	got, err := h.Locate(context.Background(), ext.LocateRequest{Symbol: "Alpha", All: true})
	if err != nil {
		t.Fatalf("定位失败：%v", err)
	}
	if got.File != "a.go" || got.ByteRange.Start != 10 || got.ByteRange.End != 42 {
		t.Errorf("声明区间 = %s [%d,%d)，期望 a.go [10,42)", got.File, got.ByteRange.Start, got.ByteRange.End)
	}
	if len(got.Sites) != 1 || got.Sites[0].ByteRange.Start != 10 {
		t.Errorf("出现点应与声明同源：%+v", got.Sites)
	}
	if got.Impact.FilesChanged != 1 || got.Impact.Occurrences != 2 || !got.Impact.Unknown {
		t.Errorf("规模 = %+v，期望 1/2/unknown", got.Impact)
	}
	if got.Precision != ext.PrecisionSemantic {
		t.Errorf("精度 = %q，期望由后端自述的 semantic", got.Precision)
	}
	if h.cmd == nil {
		t.Error("首次调用后进程应已拉起")
	}
}

// 握手真的走了 MCP 的 `initialize`：证据是扩展自述的 serverInfo 进了能力指纹。
func TestHost_HandshakeCarriesTheServerSelfDescription(t *testing.T) {
	h := newFakeHost(t, "ok", 5*time.Second)
	caps := h.Capabilities(context.Background())
	if !caps.Available || !caps.CanResolve || len(caps.Languages) != 2 {
		t.Fatalf("能力描述符 = %+v，期望可用且带两种语言", caps)
	}
	joined := strings.Join(h.Fingerprint(), "|")
	if !strings.Contains(joined, "server:fake@1") {
		t.Errorf("指纹应带上扩展自述（说明握手真的发生了）：%v", h.Fingerprint())
	}
	if !strings.Contains(joined, "lang:go,py") || !strings.Contains(joined, "precision:semantic") {
		t.Errorf("指纹应覆盖能力问过之后的语言与精度：%v", h.Fingerprint())
	}
}

// 没配命令 = "这次没有外挂后端"：所有调用如实报不可用，**不是**装配缺陷（不 panic、不失败任务）。
func TestHost_UnconfiguredCommandIsUnavailable(t *testing.T) {
	h := New(Config{})
	caps := h.Capabilities(context.Background())
	if caps.Available {
		t.Error("没配命令时能力应报不可用")
	}
	if _, err := h.Locate(context.Background(), ext.LocateRequest{Symbol: "A"}); err == nil {
		t.Error("没配命令时定位应报错")
	}
	if v := h.Parse(context.Background(), "a.go"); v != ext.ParseUnknown {
		t.Errorf("没配命令时语法判据应报「没判过」，实际 %v", v)
	}
	if _, _, err := h.Enclose(context.Background(), ext.EncloseRequest{File: "a.go"}); err == nil {
		t.Error("没配命令时封闭性判定应报错")
	}
	if err := h.Close(); err != nil {
		t.Errorf("回收一个没起过的后端不该失败：%v", err)
	}
}

// 命令起不来：报"无法启动"并如实不可用——那是**可判定**的事实，任务不失败（FR-13.4）。
func TestHost_UnstartableCommandIsUnavailableNotFatal(t *testing.T) {
	h := New(Config{Command: "/nonexistent/xhunter-ext", Timeout: 2 * time.Second})
	t.Cleanup(func() { _ = h.Close() })
	_, err := h.Locate(context.Background(), ext.LocateRequest{Symbol: "A"})
	if err == nil {
		t.Fatal("命令不存在时定位应报错")
	}
	if !strings.Contains(err.Error(), "无法启动") {
		t.Errorf("错误应说清是启动失败：%v", err)
	}
	if h.Capabilities(context.Background()).Available {
		t.Error("起不来就该报不可用，不能让符号能力假装在线")
	}
}

// 扩展崩在调用里：**立刻**变成错误，而不是让调用方等到超时（INV-3：不得静默挂起）。
func TestHost_CrashDoesNotHangTheLoop(t *testing.T) {
	h := newFakeHost(t, "crash", 30*time.Second) // 超时故意放大：证明不是靠超时兜住的
	start := time.Now()
	if _, err := h.Locate(context.Background(), ext.LocateRequest{Symbol: "A"}); err == nil {
		t.Fatal("扩展崩溃时定位应报错")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("崩溃应在秒级返回，实际 %s（说明在等超时）", elapsed)
	}
	// 崩过一次就是终态：后续调用同样立刻报错，不再尝试与之说话。
	start = time.Now()
	if _, err := h.Locate(context.Background(), ext.LocateRequest{Symbol: "A"}); err == nil {
		t.Fatal("已崩溃的后端不该再给出定位")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("后续调用应立刻返回，实际 %s", elapsed)
	}
}

// 扩展挂住：超时即**隔离**（杀掉进程并钉成终态），后续调用不再各等一遍超时。
func TestHost_TimeoutIsolatesTheExtension(t *testing.T) {
	const timeout = 400 * time.Millisecond
	h := newFakeHost(t, "hang", timeout)
	start := time.Now()
	if _, err := h.Locate(context.Background(), ext.LocateRequest{Symbol: "A"}); err == nil {
		t.Fatal("挂住的扩展应报错")
	} else if !strings.Contains(err.Error(), "已隔离") {
		t.Errorf("超时应显式说清已隔离：%v", err)
	}
	first := time.Since(start)
	if first > 5*time.Second {
		t.Errorf("超时就该在超时量级返回，实际 %s", first)
	}
	// 隔离之后立刻返回：否则每一次调用都要等满超时，一轮推理会被拖垮。
	start = time.Now()
	if _, err := h.Locate(context.Background(), ext.LocateRequest{Symbol: "A"}); err == nil {
		t.Fatal("已隔离的后端不该再给出定位")
	}
	if elapsed := time.Since(start); elapsed > timeout {
		t.Errorf("已隔离后应立刻返回，实际 %s（超时 %s）", elapsed, timeout)
	}
}

// 回收：杀的是**整棵进程树**，且等它真的退出（不留孤儿占着管道）。
func TestHost_CloseReclaimsProcess(t *testing.T) {
	h := newFakeHost(t, "ok", 5*time.Second)
	if _, err := h.Locate(context.Background(), ext.LocateRequest{Symbol: "A"}); err != nil {
		t.Fatalf("先拉起进程：%v", err)
	}
	h.mu.Lock()
	pid := h.cmd.Process.Pid
	h.mu.Unlock()
	if err := h.Close(); err != nil {
		t.Fatalf("回收失败：%v", err)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Errorf("进程 %d 仍在：回收没杀干净", pid)
	}
	// 幂等：收尾与兜底路径都可能各调一次。
	if err := h.Close(); err != nil {
		t.Errorf("重复回收不该失败：%v", err)
	}
}

// 能力指纹**不得**为了拿素材去启动进程：它在装配冻结时就读，为它拉起子进程
// 会把"根本没用过符号能力"的运行也拖上一个进程。
func TestHost_FingerprintDoesNotStartTheProcess(t *testing.T) {
	h := newFakeHost(t, "ok", 5*time.Second)
	got := h.Fingerprint()
	if h.cmd != nil {
		t.Fatal("取指纹启动了进程")
	}
	if len(got) == 0 {
		t.Fatal("指纹不该为空：装了什么就报什么")
	}
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "ext:fake") || !strings.Contains(joined, "lang:go") {
		t.Errorf("指纹应含装配事实（标识与语言）：%v", got)
	}
	if strings.Contains(joined, "server:") {
		t.Errorf("没握过手就不该有扩展自述：%v", got)
	}
}

// 不可信边界（FR-13.6）：子进程**不继承**我们的环境——XHUNTER_* 里可能有凭据。
func TestHost_UntrustedBoundaryDoesNotInheritEnv(t *testing.T) {
	t.Setenv("XHUNTER_CANARY_SECRET", "s3cr3t")
	h := newFakeHost(t, "env", 5*time.Second)
	caps := h.Capabilities(context.Background()) // 假扩展把它的 os.Environ() 放在 languages 里送回
	if len(caps.Languages) == 0 {
		t.Fatal("假扩展没把自己的环境送回来：这条用例没压到被测路径")
	}
	for _, e := range caps.Languages {
		if strings.Contains(e, "XHUNTER_") {
			t.Errorf("子进程不该看到任何 XHUNTER_*：%q", e)
		}
	}
	if len(caps.Languages) != 1 {
		t.Errorf("子进程环境应只有装配层显式给的那一项，实际 %d 项：%q", len(caps.Languages), caps.Languages)
	}
}

// 三态不被压扁：扩展给了一个不认识的取值 / 明确说"没有"，都按"没判过 / 没有"处理，
// 绝不猜成"语法不完整"——那会让检查点钉在从未验证过的改动上。
func TestHost_UnknownVerdictIsNotReadAsBroken(t *testing.T) {
	h := newFakeHost(t, "unknown", 5*time.Second)
	if v := h.Parse(context.Background(), "a.go"); v != ext.ParseUnknown {
		t.Errorf("不认识的取值应报「没判过」，实际 %v", v)
	}
	p, found, err := h.Enclose(context.Background(), ext.EncloseRequest{File: "a.go"})
	if err != nil {
		t.Fatalf("判过但没找到 ≠ 判不了：%v", err)
	}
	if found {
		t.Errorf("扩展明确说没找到，实际 found=%v（%+v）", found, p)
	}
}

// 判据与定位都可用时，形状与内置后端一致：消费方（`hunt/symbolic`、结构判据）
// 分不出这是外挂还是内置的——也不需要分。
func TestHost_ParseAndEncloseAreMappedFromTheWire(t *testing.T) {
	h := newFakeHost(t, "ok", 5*time.Second)
	if v := h.Parse(context.Background(), "a.go"); v != ext.ParseOK {
		t.Errorf("verdict=ok 应映射为 ParseOK，实际 %v", v)
	}
	p, found, err := h.Enclose(context.Background(), ext.EncloseRequest{File: "a.go"})
	if err != nil || !found {
		t.Fatalf("found=true 应映射为「有声明包含它」：found=%v err=%v", found, err)
	}
	if p.File != "a.go" || p.ByteRange.Start != 0 || p.ByteRange.End != 30 {
		t.Errorf("区间 = %s [%d,%d)，期望 a.go [0,30)", p.File, p.ByteRange.Start, p.ByteRange.End)
	}
	if p.Precision != ext.PrecisionSemantic {
		t.Errorf("精度 = %q，期望来自线路", p.Precision)
	}
}
