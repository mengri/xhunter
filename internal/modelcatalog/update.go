package modelcatalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// UpdateOptions 是更新目录的参数。
type UpdateOptions struct {
	// Source 是上游地址，空则用 DefaultSource。
	Source string
	// Dir 是快照目录，空则用 DefaultDir()。
	Dir string
	// Timeout 是单次请求超时，0 则用 60s。
	Timeout time.Duration
	// OutputReserve 是上游未区分输入输出时的输出预留，0 则用 DefaultOutputReserve。
	OutputReserve int
	// Client 可注入自定义 HTTP 客户端（测试用）。
	Client *http.Client
}

// Update 拉取上游目录、转换并原子写入本地快照。
//
// 这是**安装期与 CLI 期**的动作，运行期不联网：Xhunter 启动只读本地快照。
// 因此网络失败属于环境问题（退出码 2），而不是任务失败。
func Update(ctx context.Context, opts UpdateOptions) (Snapshot, error) {
	source := opts.Source
	if source == "" {
		source = DefaultSource
	}
	dir := opts.Dir
	if dir == "" {
		var err error
		if dir, err = DefaultDir(); err != nil {
			return Snapshot{}, err
		}
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	body, err := fetch(ctx, opts.Client, source, timeout)
	if err != nil {
		return Snapshot{}, err
	}

	var raw map[string]Entry
	if err := json.Unmarshal(body, &raw); err != nil {
		return Snapshot{}, fmt.Errorf("上游目录格式无法解析：%w", err)
	}
	reserve := opts.OutputReserve
	if reserve <= 0 {
		reserve = DefaultOutputReserve
	}
	cfg, rep, err := Convert(raw, reserve)
	if err != nil {
		return Snapshot{}, err
	}

	sum := sha256.Sum256(body)
	snap := Snapshot{
		Meta: Meta{
			Source:         source,
			FetchedAt:      time.Now().UTC(),
			UpstreamBytes:  len(body),
			UpstreamSHA256: hex.EncodeToString(sum[:]),
			Report:         rep,
		},
		Catalog: cfg,
	}
	if err := Save(dir, snap); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

func fetch(ctx context.Context, client *http.Client, source string, timeout time.Duration) ([]byte, error) {
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, source, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败：%w", err)
	}
	req.Header.Set("User-Agent", "xhunter-model-catalog")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取模型目录失败（%s）：%w", source, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("拉取模型目录失败：HTTP %d（%s）", resp.StatusCode, source)
	}

	// 限制读取上限，避免误配地址把磁盘写满。
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxUpstreamBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败：%w", err)
	}
	if len(body) > MaxUpstreamBytes {
		return nil, fmt.Errorf("响应超过 %d 字节上限，疑为地址错误", MaxUpstreamBytes)
	}
	return body, nil
}
