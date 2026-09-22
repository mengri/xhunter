package adapter

import (
	"bufio"
	"bytes"
	"io"
	"strings"
)

// maxSSELine 是单行的读取上限。模型输出的一小块也可能很大，但不该无上限：
// 上游若发来一条不含换行的巨流，无上限缓冲会把它整个吃进内存。
const maxSSELine = 1 << 20

// SSEReader 是 text/event-stream 的最小分帧读取器：按空行切事件，只取 data 字段。
//
// 只实现协议里必需的那部分——字段名后可以没有空格、多个 data 行按换行拼接、
// 注释行（冒号开头）忽略，不认识的字段直接跳过。上游加字段不该让读取失败，
// 这也是它比"按行 JSON 解析"更稳妥的地方：分帧与载荷是两层。
//
// 它不属于任何一家协议：凡是以事件流分帧的响应都长这样。载荷怎么解、
// 怎么翻译成中立事件，那才是各协议包的事。
type SSEReader struct {
	sc *bufio.Scanner
}

func NewSSEReader(r io.Reader) *SSEReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxSSELine)
	return &SSEReader{sc: sc}
}

// Next 返回下一个事件的数据。ok 为 false 表示流已结束。
//
// 结束而不带尾随空行时，也把已攒下的内容当作一个事件返回——上游未必给空行收尾，
// 丢掉最后一个事件会让"最后一块内容"静默消失。
func (s *SSEReader) Next() (data []byte, ok bool, err error) {
	var buf bytes.Buffer
	for {
		if !s.sc.Scan() {
			if err := s.sc.Err(); err != nil {
				return nil, false, err
			}
			if buf.Len() > 0 {
				return buf.Bytes(), true, nil
			}
			return nil, false, nil
		}

		line := strings.TrimSuffix(s.sc.Text(), "\r")
		if line == "" {
			// 事件之间的空行本身不是事件。
			if buf.Len() == 0 {
				continue
			}
			return buf.Bytes(), true, nil
		}
		if strings.HasPrefix(line, ":") {
			continue
		}

		name, value, _ := strings.Cut(line, ":")
		if name != "data" {
			continue
		}
		if buf.Len() > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString(strings.TrimPrefix(value, " "))
	}
}
