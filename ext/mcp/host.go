// Package mcp 把 `ext.ExtHost` 接到一个**本地扩展子进程**上（MCP 的 stdio 传输）。
//
// 它是外挂通道：第三方后端（LSP 语义后端、别的解析器）以独立进程接入，核心只认
// `ext.ExtHost`，换后端不改任何业务代码。与内置后端（`ext/syntax`）的关系是**同一份契约的
// 两种实现**——能力描述符、定位结果、三态判据的形状完全一致，消费方分不出区别，也不需要分。
//
// 协议形态见 `wire.go`：传输与握手是 MCP 标准（`initialize` ＋ `notifications/initialized`），
// 符号能力方法走自有命名空间 `xhunter/*`。
//
// 生命周期（FR-13.5）：**懒启动**（首次调用才拉起）、**随 Hunt 回收**（`Close`）、
// **崩溃可隔离**（进程没了/超时就报"已隔离"，绝不让主循环挂住，INV-3）。
//
// 不可信边界（FR-13.6）：子进程**不继承**当前环境（XHUNTER_* 里可能有凭据），
// 只拿装配层显式列出的那几项；它也无法写工作区——`ExtHost` 没有写方法，它只能回答
// "改哪里"，落盘永远由核心的 `Committer` 执行。
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"xhunter/ext"
	"xhunter/workspace"
)

// defaultCallTimeout 是单次调用的默认上界。一次定位不该拖住一轮推理，
// 而"扩展挂住"在无头场景下是最难诊断的失败（进程在、CPU 不动、日志安静）。
const defaultCallTimeout = 30 * time.Second

// protocolVersion 是握手里报给扩展的 MCP 版本。
const protocolVersion = "2025-06-18"

// 握手里的客户端自述。
const (
	clientName    = "xhunter"
	clientVersion = "1"
)

// stderrLimit 是留下来进错误消息的扩展 stderr 上限：诊断够用即可，
// 扩展把整份日志打到 stderr 时不该把它全搬进一条 error 事件。
const stderrLimit = 4096

// Config 是外挂后端的装配参数。它只有**部署事实**（命令与入参）与指纹素材，
// 没有"后端种类"这种字段——核心不感知后端是什么，只消费能力描述符。
type Config struct {
	// Command 是可执行文件（argv[0]）。为空表示"没配外挂后端"，调用一律报不可用。
	Command string
	Args    []string
	// Dir 是子进程工作目录。
	Dir string
	// Env 是子进程的**全部**环境（默认 nil = 空环境）。不可信边界：不继承当前进程环境，
	// 因此 XHUNTER_* 里的凭据、以及任何别的环境变量都不会漏给扩展（FR-13.6）。
	Env []string
	// Timeout 是单次调用的上界（0 取 defaultCallTimeout）。
	Timeout time.Duration
	// ID / Version / Languages 是能力指纹的素材：`Fingerprint()` **不得**为了拿它们
	// 去启动进程（指纹在装配冻结时就读），所以它们是显式的装配事实，不是问来的。
	ID        string
	Version   string
	Languages []string
}

func (c Config) withDefaults() Config {
	if c.Timeout <= 0 {
		c.Timeout = defaultCallTimeout
	}
	if c.ID == "" {
		c.ID = filepath.Base(c.Command)
	}
	if c.Version == "" {
		c.Version = "unknown"
	}
	return c
}

// Host 是一个外挂扩展进程。
type Host struct {
	cfg Config

	// startMu 护住"只启动一次"；它与 mu **分开**，因为启动过程里要发请求
	// （而发请求要拿 mu）——同一把锁会把自己锁死。
	startMu   sync.Mutex
	started   bool
	startErr  error
	server    serverInfo
	mu        sync.Mutex
	cmd       *exec.Cmd
	in        io.WriteCloser
	nextID    uint64
	pending   map[uint64]chan *rpcResponse
	dead      error
	done      chan struct{}
	closed    bool
	caps      ext.ExtCaps
	capsKnown bool
	stderr    *cappedBuffer
}

// New 装配一个外挂后端。它**不启动进程**（懒启动），也不做网络/文件访问——
// 构造只是记下装配事实。
func New(cfg Config) *Host {
	cfg = cfg.withDefaults()
	h := &Host{
		cfg:     cfg,
		pending: map[uint64]chan *rpcResponse{},
		stderr:  &cappedBuffer{limit: stderrLimit},
	}
	if cfg.Command == "" {
		// 没配命令："这次没有外挂后端"是一条可以带着跑的事实（符号原语给结构化错误），
		// 不是装配缺陷——因此不在构造期报错，而是让调用如实报不可用。
		h.dead = errors.New("外挂扩展未配置命令（XHUNTER_EXT_COMMAND）")
	}
	return h
}

var _ ext.ExtHost = (*Host)(nil)

// ============================================================ 启动与回收

// ensureStarted 懒启动：只有真要问扩展时才拉起进程（FR-13.5）。
// 启动失败会被**记住**：重试一个起不来的命令只会把同一句错误重复 N 遍。
func (h *Host) ensureStarted(ctx context.Context) error {
	h.startMu.Lock()
	defer h.startMu.Unlock()
	if h.started {
		return h.startErr
	}
	h.started = true
	h.startErr = h.start(ctx)
	return h.startErr
}

func (h *Host) start(ctx context.Context) error {
	if h.cfg.Command == "" {
		return h.deadErr()
	}
	cmd := exec.Command(h.cfg.Command, h.cfg.Args...)
	cmd.Dir = h.cfg.Dir
	cmd.Env = h.cfg.Env
	cmd.Stderr = h.stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("外挂扩展的 stdin 不可用：%w", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("外挂扩展的 stdout 不可用：%w", err)
	}
	// 独立进程组：回收与隔离要杀的是**整棵进程树**（`npx` 之类会拉起一堆子进程），
	// 只杀直接子进程会让孤儿继续占着管道、Wait 永远不返回。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("外挂扩展无法启动（%s）：%w", h.cfg.Command, err)
	}

	h.mu.Lock()
	h.cmd, h.in, h.done = cmd, in, make(chan struct{})
	h.mu.Unlock()
	go h.readLoop(out)
	go h.waitLoop(cmd)

	// 握手（MCP 标准两条）：initialize 请求 → notifications/initialized 通知。
	var init initializeResult
	if err := h.request(ctx, methodInitialize, initializeParams{
		ProtocolVersion: protocolVersion,
		Capabilities:    map[string]any{},
		ClientInfo:      clientInfo{Name: clientName, Version: clientVersion},
	}, &init); err != nil {
		// 握手失败 = 进程在跑但说不上话。杀掉它：留着一个半截会话比"没有后端"更糟——
		// 后续调用会一直等在一个永远不会答复的管道上。
		h.isolate(fmt.Errorf("外挂扩展握手失败：%w%v", err, h.stderrTail()))
		return h.deadErr()
	}
	h.mu.Lock()
	h.server = init.ServerInfo
	h.mu.Unlock()
	if err := h.write(rpcRequest{JSONRPC: jsonrpcVersion, Method: notifyInitialized}); err != nil {
		h.isolate(fmt.Errorf("外挂扩展的握手通知写不进去：%w", err))
		return h.deadErr()
	}
	return nil
}

// readLoop 读 stdout 并按 id 把响应交给等待者。对面主动发来的通知（无 id）一律忽略。
func (h *Host) readLoop(r io.Reader) {
	dec := json.NewDecoder(r)
	for {
		var msg rpcResponse
		if err := dec.Decode(&msg); err != nil {
			// 读不动了（进程退出 / 输出不是 JSON）：把所有人叫醒，不让任何一个调用挂住。
			h.failAll(fmt.Errorf("外挂扩展的输出不可解析（%v）%v", err, h.stderrTail()))
			return
		}
		if msg.ID == 0 {
			continue // 通知：不看
		}
		h.mu.Lock()
		ch, ok := h.pending[msg.ID]
		delete(h.pending, msg.ID)
		h.mu.Unlock()
		if !ok {
			continue // 没人等它（超时已被隔离）：丢弃，不阻塞读循环
		}
		ch <- &msg
	}
}

// waitLoop 盯住进程退出：扩展崩了必须**立刻**变成错误，而不是让调用等到超时。
func (h *Host) waitLoop(cmd *exec.Cmd) {
	err := cmd.Wait()
	if err == nil {
		err = errors.New("扩展自行退出")
	}
	h.failAll(fmt.Errorf("外挂扩展进程已退出：%w%v", err, h.stderrTail()))
	h.mu.Lock()
	done := h.done
	h.mu.Unlock()
	if done != nil {
		close(done)
	}
}

// failAll 把 dead 钉成终态并叫醒全部等待者。只记**第一次**的原因：
// 第一次通常就是根因（进程为什么要退出），后面那些是它的回声。
func (h *Host) failAll(err error) {
	h.mu.Lock()
	if h.dead == nil {
		h.dead = err
	}
	waiting := make([]chan *rpcResponse, 0, len(h.pending))
	for id, ch := range h.pending {
		waiting = append(waiting, ch)
		delete(h.pending, id)
	}
	h.mu.Unlock()
	for _, ch := range waiting {
		close(ch)
	}
}

// isolate 由调用侧主动隔离（超时）：杀掉进程，并把这次的原因钉成 dead。
func (h *Host) isolate(err error) {
	h.mu.Lock()
	if h.dead == nil {
		h.dead = err
	}
	cmd := h.cmd
	done := h.done
	h.mu.Unlock()
	fail := func(e error) {
		h.mu.Lock()
		if h.dead == nil {
			h.dead = e
		}
		h.mu.Unlock()
	}
	if cmd == nil || cmd.Process == nil {
		fail(err)
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	fail(err)
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second): // 回收不该自己挂住：杀完就走
		}
	}
}

func (h *Host) deadErr() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dead == nil {
		return nil
	}
	return h.dead
}

func (h *Host) stderrTail() string {
	s := strings.TrimSpace(h.stderr.String())
	if s == "" {
		return ""
	}
	return "；扩展 stderr：" + s
}

// ============================================================ 一次调用

// request 发一条请求并等答复。它**不**做懒启动（启动自己就走它，否则递归）。
func (h *Host) request(ctx context.Context, method string, params any, out any) error {
	if err := h.deadErr(); err != nil {
		return err
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("外挂扩展的 %s 参数无法序列化：%w", method, err)
	}
	id, ch, err := h.register()
	if err != nil {
		return err
	}
	if err := h.write(rpcRequest{JSONRPC: jsonrpcVersion, ID: id, Method: method, Params: raw}); err != nil {
		h.drop(id)
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, h.cfg.Timeout)
	defer cancel()
	select {
	case resp, ok := <-ch:
		if !ok {
			// 通道被关 = 进程没了/输出断了：dead 里记着原因。
			if err := h.deadErr(); err != nil {
				return err
			}
			return fmt.Errorf("外挂扩展在 %s 期间失联", method)
		}
		if resp.Error != nil {
			return resp.Error
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("外挂扩展的 %s 结果不可解析：%w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		h.drop(id)
		// 超时即**隔离**：半截的协议状态不可复用（那次调用的答复可能永远在路上），
		// 与其每次都等满超时，不如把进程杀掉、把失败钉成终态——主循环绝不挂住（INV-3）。
		timeout := fmt.Errorf("外挂扩展 %s 超过 %s 未响应，已隔离", method, h.cfg.Timeout)
		h.isolate(timeout)
		return timeout
	}
}

// call 是给对外方法用的入口：先确保进程在，再发请求。
func (h *Host) call(ctx context.Context, method string, params any, out any) error {
	if err := h.ensureStarted(ctx); err != nil {
		return err
	}
	return h.request(ctx, method, params, out)
}

func (h *Host) register() (uint64, chan *rpcResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, nil, errors.New("外挂扩展已回收")
	}
	if h.in == nil {
		return 0, nil, h.deadErr()
	}
	h.nextID++
	id := h.nextID
	ch := make(chan *rpcResponse, 1)
	h.pending[id] = ch
	return id, ch, nil
}

func (h *Host) drop(id uint64) {
	h.mu.Lock()
	delete(h.pending, id)
	h.mu.Unlock()
}

// write 写一条消息。持 mu 是为了让"编码 ＋ 写"成为原子动作——两个并发调用
// 各自写半行 JSON 会让对面解析失败（bufio 之外没有别的保护）。
func (h *Host) write(msg rpcRequest) error {
	line, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("外挂扩展的消息无法序列化：%w", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.in == nil {
		if h.dead != nil {
			return h.dead
		}
		return errors.New("外挂扩展未启动")
	}
	if _, err := h.in.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("外挂扩展的 stdin 写失败：%w", err)
	}
	return nil
}

// ============================================================ ext.ExtHost

// Capabilities 问能力描述符。拿不到就报**不可用**（`ExtCaps{}`）——
// 那是可判定的事实，消费方据此给结构化错误，而不是让符号能力假装在线。
func (h *Host) Capabilities(ctx context.Context) ext.ExtCaps {
	h.mu.Lock()
	known, caps := h.capsKnown, h.caps
	h.mu.Unlock()
	if known {
		return caps
	}
	var got capsResult
	if err := h.call(ctx, methodCapabilities, map[string]any{}, &got); err != nil {
		return ext.ExtCaps{}
	}
	caps = ext.ExtCaps{
		Available:  got.Available,
		Languages:  got.Languages,
		CanResolve: got.CanResolve,
		Precision:  ext.Precision(got.Precision),
	}
	h.mu.Lock()
	h.caps, h.capsKnown = caps, true
	h.mu.Unlock()
	return caps
}

// Locate 定位符号：扩展只回答"在哪"，落盘由核心执行（FR-13.3）。
func (h *Host) Locate(ctx context.Context, req ext.LocateRequest) (ext.Prepared, error) {
	var got locateResult
	if err := h.call(ctx, methodLocate, locateParams{Symbol: req.Symbol, File: req.File, All: req.All}, &got); err != nil {
		return ext.Prepared{}, err
	}
	p := ext.Prepared{
		File:      got.File,
		ByteRange: workspace.ByteRange{Start: got.Range.Start, End: got.Range.End},
		Impact: ext.Impact{
			FilesChanged: got.FilesChanged,
			Occurrences:  got.Occurrences,
			Unknown:      got.Unknown,
		},
		Precision: ext.Precision(got.Precision),
	}
	for _, s := range got.Sites {
		p.Sites = append(p.Sites, ext.Site{
			File:      s.File,
			ByteRange: workspace.ByteRange{Start: s.Range.Start, End: s.Range.End},
		})
	}
	return p, nil
}

// Parse 问"这个文件语法完整吗"。拿不到结论时报 **ParseUnknown**（"没判过"）——
// 扩展不可用与"语法不完整"是两句话，混起来会让检查点钉在从未验证过的改动上。
func (h *Host) Parse(ctx context.Context, file string) ext.ParseVerdict {
	var got parseResult
	if err := h.call(ctx, methodParse, parseParams{File: file}, &got); err != nil {
		return ext.ParseUnknown
	}
	switch got.Verdict {
	case "ok":
		return ext.ParseOK
	case "broken":
		return ext.ParseBroken
	default:
		return ext.ParseUnknown
	}
}

// Enclose 问"这处区间被哪个声明包含"。found=false 是**结论**（确实没有），
// error 是"这次判不了"——两者不得混（结构判据靠它区分"不提交"与"没判过"）。
func (h *Host) Enclose(ctx context.Context, req ext.EncloseRequest) (ext.Prepared, bool, error) {
	var got encloseResult
	params := encloseParams{
		File:  req.File,
		Range: byteRangeWire{Start: req.ByteRange.Start, End: req.ByteRange.End},
	}
	if err := h.call(ctx, methodEnclose, params, &got); err != nil {
		return ext.Prepared{}, false, err
	}
	if !got.Found {
		return ext.Prepared{}, false, nil
	}
	p := ext.Prepared{
		File:      got.File,
		ByteRange: workspace.ByteRange{Start: got.Range.Start, End: got.Range.End},
		Precision: ext.Precision(got.Precision),
	}
	for _, s := range got.Sites {
		p.Sites = append(p.Sites, ext.Site{
			File:      s.File,
			ByteRange: workspace.ByteRange{Start: s.Range.Start, End: s.Range.End},
		})
	}
	return p, true, nil
}

// Fingerprint 给出能力指纹。**它绝不启动进程**——指纹在装配冻结时就读（进会话材料与
// 生效配置快照），为一个诊断字段去拉起子进程，会把"没用过符号能力"的运行也拖上一个进程。
// 因此素材来自装配事实；能力问过之后才把后端自述的语言与精度补进去。
func (h *Host) Fingerprint() []string {
	out := []string{"ext:" + h.cfg.ID, "version:" + h.cfg.Version}
	h.mu.Lock()
	langs, known, prec := h.cfg.Languages, h.capsKnown, ""
	if known {
		if len(h.caps.Languages) > 0 {
			langs = h.caps.Languages
		}
		prec = string(h.caps.Precision)
	}
	server := h.server
	h.mu.Unlock()
	if server.Name != "" {
		out = append(out, "server:"+server.Name+"@"+server.Version)
	}
	if len(langs) > 0 {
		out = append(out, "lang:"+strings.Join(langs, ","))
	}
	if prec != "" {
		out = append(out, "precision:"+prec)
	}
	return out
}

// Close 回收扩展进程（FR-13.5）：杀**整棵进程树**并等到它真的退出。
// 幂等：Hunt 收尾与任何兜底路径都可能各调一次。
func (h *Host) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	cmd, done := h.cmd, h.done
	h.mu.Unlock()

	h.failAll(errors.New("外挂扩展已回收"))
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			return errors.New("外挂扩展回收超时：进程组未退出")
		}
	}
	return nil
}

// cappedBuffer 是留作诊断的有界缓冲：够了就丢，不让扩展的日志把内存吃掉。
type cappedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() >= b.limit {
		return len(p), nil // 已满：照单全收（不让写端失败），只是不留
	}
	if room := b.limit - b.buf.Len(); len(p) > room {
		p = p[:room]
	}
	return b.buf.Write(p)
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
