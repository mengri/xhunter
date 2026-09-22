package modelcatalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"xhunter/providerconfig"
)

// SnapshotName 是快照文件名，位于 ~/.xhunter/ 下。
const SnapshotName = "models.json"

// Meta 记录快照的可追溯信息：数据从哪来、什么时候来的、转出了多少。
// 它也是诊断"为什么某个模型没有上限"的第一手依据。
type Meta struct {
	Source         string    `json:"source"`
	FetchedAt      time.Time `json:"fetched_at"`
	UpstreamBytes  int       `json:"upstream_bytes"`
	UpstreamSHA256 string    `json:"upstream_sha256"`
	Report         Report    `json:"report"`
}

// Snapshot 是落盘的目录形态：生效配置 + 元数据。
type Snapshot struct {
	Meta    Meta                  `json:"_meta"`
	Catalog providerconfig.Config `json:"catalog"`
}

// ErrNoSnapshot 表示本地没有目录快照——需要先执行 `xhunter models update`。
var ErrNoSnapshot = errors.New("本地模型目录不存在")

// DefaultDir 返回默认目录 ~/.xhunter。
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("无法确定用户主目录：%w", err)
	}
	return filepath.Join(home, ".xhunter"), nil
}

// SnapshotPath 返回快照文件的完整路径。
func SnapshotPath(dir string) string { return filepath.Join(dir, SnapshotName) }

// Load 读取本地快照并做生效校验（上限必须齐备，对齐 FR-9.5）。
func Load(dir string) (Snapshot, error) {
	path := SnapshotPath(dir)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Snapshot{}, fmt.Errorf("%w：%s（请执行 xhunter models update）", ErrNoSnapshot, path)
		}
		return Snapshot{}, fmt.Errorf("打开模型目录失败：%w", err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var s Snapshot
	if err := dec.Decode(&s); err != nil {
		return Snapshot{}, fmt.Errorf("解析模型目录失败（%s）：%w", path, err)
	}
	if err := s.Catalog.ValidateResolved(); err != nil {
		return Snapshot{}, fmt.Errorf("模型目录内容无效：%w", err)
	}
	return s, nil
}

// Save 原子写入快照：先写临时文件再改名，避免下载/写入中途崩溃留下半个文件。
func Save(dir string, s Snapshot) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建目录失败（%s）：%w", dir, err)
	}
	buf, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化模型目录失败：%w", err)
	}

	tmp, err := os.CreateTemp(dir, SnapshotName+".tmp-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败：%w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// 失败路径下清理临时文件；成功后改名，此调用自然无效果。
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时文件失败：%w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("落盘失败：%w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败：%w", err)
	}
	if err := os.Rename(tmpName, SnapshotPath(dir)); err != nil {
		return fmt.Errorf("替换模型目录失败：%w", err)
	}
	return nil
}

// Ready 报告目录是否可用，用于启动期前置校验（FR-9.5 的来源①）。
func Ready(dir string) error {
	_, err := Load(dir)
	return err
}
