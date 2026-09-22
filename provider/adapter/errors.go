package adapter

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ErrNoContextWindow 表示声明能力时上下文上限未知。
//
// 上限是唯一不允许估算的能力：估高的代价是任务跑到一半硬失败，估低的代价是静默拉低
// 所有同类任务的效率。因此协议实现在声明能力的那一刻就把关——没有明确的上限，
// 它就不该被构造出来（这条检查落在构造期，而不是运行到第一次推理时才发现）。
var ErrNoContextWindow = errors.New("模型上下文上限未配置")

// maxErrBody 是回传的错误体上限：上游可能返回一整页 HTML 错误页，有价值的部分在
// 头几百字节，整段带回去只会把日志与事件流淹掉。
const maxErrBody = 2 << 10

// StatusError 是上游返回的非成功响应。
//
// Retryable 在协议侧判好再交出去，而不是让调用方自己看状态码：判断"重试有没有意义"
// 需要同时看状态码与错误体语义，这个知识属于协议约定，泄漏出去只会被重复实现一遍。
type StatusError struct {
	Status    int
	Body      string
	Retryable bool
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("Provider 返回 %d：%s", e.Status, e.Body)
}

// NewStatusError 按状态码构造：限流与服务端错误重试有意义；请求本身不合法时
// 重试永远得到同一个答案。各协议实现用它把"上游返回了什么"归一成一种形状——
// 本项目不引厂商 SDK，状态码来自 HTTP 响应本身。
func NewStatusError(status int, body string) *StatusError {
	return &StatusError{
		Status:    status,
		Body:      body,
		Retryable: status == http.StatusTooManyRequests || status >= 500,
	}
}

// StatusErrorFrom 读出响应体并构造状态错误。
func StatusErrorFrom(resp *http.Response) *StatusError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	return NewStatusError(resp.StatusCode, strings.TrimSpace(string(body)))
}

// statusCarrier 是"能给出 HTTP 状态码的错误"的最小形状。错误类型由注入的
// HTTP 客户端或协议实现给出，各自的类型不同；用最小接口接住它们，
// 调用方就不必对具体类型做判断。
type statusCarrier interface {
	StatusCode() int
}

// Retryable 判定一个上游错误是否值得重试。
//
// 能拿到状态码就按状态码判（限流与服务端故障有意义，请求不合法则不然）；
// 拿不到就按临时故障处理——连接被拒、读超时、流被截断都属于这一类，
// 重试的代价远小于把一个可恢复的环境问题判成任务失败。
func Retryable(err error) bool {
	var sc statusCarrier
	if errors.As(err, &sc) {
		return NewStatusError(sc.StatusCode(), "").Retryable
	}
	return true
}
