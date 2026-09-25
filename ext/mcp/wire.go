package mcp

import (
	"encoding/json"
	"fmt"
)

// 线格式：MCP 的 stdio 传输是 stdin/stdout 上**按行分隔的 JSON-RPC 2.0** 消息。
//
// 刻意不用 LSP 那种 `Content-Length` 帧头——MCP 的 stdio 传输就是换行分隔的裸 JSON，
// 一行一条。读侧因此直接用 `json.Decoder`：它按 JSON 值的边界切分，换行只是给人看的
// 分隔符，多一个少一个都不影响解析。
const jsonrpcVersion = "2.0"

// 握手方法（MCP 标准名字，不改）：扩展认的是这两个名字。
const (
	methodInitialize = "initialize"
	// notifyInitialized 是握手第二条，它是**通知**（无 id），对面不应答复。
	notifyInitialized = "notifications/initialized"
)

// 符号能力的方法名。**这是自有命名空间**，不是 MCP 标准方法：
//
// MCP 的标准面（tools / resources / prompts）是"给模型用"的形状——它回答"有哪些工具、
// 工具怎么调"，而这里要的是"目标文件 ＋ 字节区间"这种**可直接落盘**的定位结论；把符号
// 定位塞进 `tools/call` 只会让两边都变形。因此：传输与握手走 MCP，能力方法是 Xhunter 的。
// 这条边界写在这里，是因为换协议形态（自有最小协议 vs MCP）会同时改这两组常量。
const (
	methodCapabilities = "xhunter/capabilities"
	methodLocate       = "xhunter/locate"
	methodParse        = "xhunter/parse"
	methodEnclose      = "xhunter/enclose"
)

// rpcRequest 是一条请求（或通知）。
//
// ID 为 0 时按 `omitempty` 省略，即**通知**（握手第二条就是它）：JSON-RPC 的通知没有 id、
// 对面也不该答复。响应侧的 id 从 1 起，因此 0 不可能与某次调用撞上。
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse 既装响应（有 id）也装对面主动发来的通知（有 method、无 id）。
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError 是 JSON-RPC 的错误对象。它实现 error，调用点因此可以把它当作
// "扩展明确说了不"直接上抛——扩展的业务性拒绝与协议错误走同一条路，消费方只看到 error。
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("扩展返回错误（code %d）：%s", e.Code, e.Message)
}

// ---------------------------------------------------------------- 握手

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// initializeParams 是 MCP 标准的 `initialize` 参数。
type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      clientInfo     `json:"clientInfo"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      serverInfo     `json:"serverInfo"`
}

// ---------------------------------------------------------------- 能力

type capsResult struct {
	Available  bool     `json:"available"`
	Languages  []string `json:"languages"`
	CanResolve bool     `json:"can_resolve"`
	Precision  string   `json:"precision"`
}

// ---------------------------------------------------------------- 定位

type locateParams struct {
	Symbol string `json:"symbol"`
	File   string `json:"file,omitempty"`
	All    bool   `json:"all,omitempty"`
}

// byteRangeWire 是半开区间 [Start, End) 的线格式。字段名与 `workspace.ByteRange` 一致，
// 因此它同时就是对外文档里的形状——不另造一套叫法。
type byteRangeWire struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type siteWire struct {
	File  string        `json:"file"`
	Range byteRangeWire `json:"range"`
}

type locateResult struct {
	File         string        `json:"file"`
	Range        byteRangeWire `json:"range"`
	Sites        []siteWire    `json:"sites,omitempty"`
	FilesChanged int           `json:"files_changed"`
	Occurrences  int           `json:"occurrences"`
	Unknown      bool          `json:"unknown"`
	Precision    string        `json:"precision"`
}

// ---------------------------------------------------------------- 结构判据

type parseParams struct {
	File string `json:"file"`
}

// parseResult.Verdict 是三态的字符串形态：ok / broken / unknown。
// 未知取值一律按 unknown（"没判过"），绝不猜成 broken——那会让检查点钉在从未验证的改动上。
type parseResult struct {
	Verdict string `json:"verdict"`
}

type encloseParams struct {
	File  string        `json:"file"`
	Range byteRangeWire `json:"range"`
}

// encloseResult.Found 是**结论**（有没有声明包含这处区间），它与"这次判不了"
// 必须分开：后者由 JSON-RPC 的 error 表达（消费方拿到的就是 error）。
type encloseResult struct {
	Found     bool          `json:"found"`
	File      string        `json:"file"`
	Range     byteRangeWire `json:"range"`
	Sites     []siteWire    `json:"sites,omitempty"`
	Precision string        `json:"precision"`
}
