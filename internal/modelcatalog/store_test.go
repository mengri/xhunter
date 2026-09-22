package modelcatalog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xhunter/providerconfig"
)

// 空目录参数必须解析成**默认目录**（~/.xhunter），而不是"当前目录"。
//
// 这条区分不是风格问题：安装期（`xhunter models update`）把快照写到 ~/.xhunter/models.json，
// 运行期读的必须是同一份。若空值落到 filepath.Join("", name)，运行期读的是 ./models.json——
// 后果一：FR-9.5 的来源①（目录快照提供上限）永不生效；后果二：工作目录里一个同名文件
// 反而决定启动成败。
func TestLoadAndSave_EmptyDirMeansDefaultDirNotWorkingDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	defaultPath := filepath.Join(home, ".xhunter", SnapshotName)

	// 工作目录里先放一份伪造的 models.json：它不该被当成目录快照。
	work := t.TempDir()
	t.Chdir(work)
	const forged = `{"catalog":{"provider":{}}}`
	if err := os.WriteFile(SnapshotName, []byte(forged), 0o644); err != nil {
		t.Fatalf("写工作目录里的同名文件失败：%v", err)
	}

	// ① 默认目录还没有快照：必须报"没有快照"，且提示里给出默认路径——
	//    若这里读到了工作目录那份文件，本断言立刻失败。
	_, err := Load("")
	if !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("默认目录无快照时应报 ErrNoSnapshot，得到 %v", err)
	}
	if !strings.Contains(err.Error(), defaultPath) {
		t.Errorf("错误信息应给出默认路径 %s：%v", defaultPath, err)
	}

	// ② Save("") 写进默认目录，不得碰工作目录里的同名文件。
	want := Snapshot{
		Meta:    Meta{Source: "test-source"},
		Catalog: providerconfig.Config{Provider: map[string]providerconfig.Provider{"p": {Models: map[string]providerconfig.Model{"m": {Limit: providerconfig.Limit{Context: 1000, Output: 100}}}}}},
	}
	if err := Save("", want); err != nil {
		t.Fatalf("Save(\"\") 失败：%v", err)
	}
	if _, err := os.Stat(defaultPath); err != nil {
		t.Errorf("Save(\"\") 应写入默认目录 %s：%v", defaultPath, err)
	}
	if got, err := os.ReadFile(filepath.Join(work, SnapshotName)); err != nil || string(got) != forged {
		t.Errorf("Save(\"\") 不该改写工作目录里的同名文件：%q err=%v", got, err)
	}

	// ③ Load("") 读到的必须是默认目录那份（来源可辨），而不是工作目录那份。
	got, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") 失败：%v", err)
	}
	if got.Meta.Source != "test-source" {
		t.Errorf("读到的不是默认目录的快照：source=%q", got.Meta.Source)
	}
}

// 显式目录仍然是"就用这个目录"：默认目录的解析不该改变显式传参的语义。
func TestLoad_ExplicitDirIsUsedAsGiven(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	explicit := t.TempDir()
	if err := Save(explicit, Snapshot{Catalog: providerconfig.Config{Provider: map[string]providerconfig.Provider{"p": {Models: map[string]providerconfig.Model{"m": {Limit: providerconfig.Limit{Context: 10, Output: 5}}}}}}}); err != nil {
		t.Fatalf("Save 失败：%v", err)
	}
	if _, err := Load(explicit); err != nil {
		t.Fatalf("显式目录应可读：%v", err)
	}
	// 默认目录仍是空的：写入没有落到别处。
	if _, err := Load(""); !errors.Is(err, ErrNoSnapshot) {
		t.Errorf("显式目录的写入不该影响默认目录：%v", err)
	}
}
