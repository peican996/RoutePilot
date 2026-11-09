package kernel

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

var errProbe = errors.New("probe failed")

func fakePE(tag byte) []byte {
	b := make([]byte, 0x200)
	b[0], b[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(b[0x3c:0x40], 0x80)
	copy(b[0x80:], []byte{'P', 'E', 0, 0})
	b[0x100] = tag
	return b
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestValidatePEFile(t *testing.T) {
	if err := validatePE(fakePE(1)); err != nil {
		t.Fatalf("合法 PE 头应通过: %v", err)
	}
	for name, data := range map[string][]byte{
		"空文件":     {},
		"无 MZ":    []byte("not-a-pe"),
		"偏移越界":    append([]byte{'M', 'Z'}, make([]byte, 0x40)...),
		"MZ 无 PE": append([]byte{'M', 'Z'}, make([]byte, 0x100)...),
	} {
		if name == "偏移越界" {
			binary.LittleEndian.PutUint32(data[0x3c:0x40], 0xFFFF)
		}
		if err := validatePE(data); err == nil {
			t.Fatalf("%s 应被拒绝", name)
		}
	}
}

func TestInstallBinaryBackupAndRollback(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	target := filepath.Join(dir, "bin", "mihomo.exe")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	oldExe := fakePE(1)
	newExe := fakePE(2)
	if err := os.WriteFile(target, oldExe, 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := m.installBinary(newExe)
	if err != nil {
		t.Fatalf("安装新内核失败: %v", err)
	}
	if res.BackupPath == "" {
		t.Fatal("应生成旧内核备份")
	}
	if got := readFile(t, target); string(got) != string(newExe) {
		t.Fatal("目标文件应为新内核")
	}
	if got := readFile(t, res.BackupPath); string(got) != string(oldExe) {
		t.Fatal("备份内容应为旧内核")
	}
	if m.LastBackupPath() != res.BackupPath {
		t.Fatal("Manager 应记录备份路径")
	}
	if !IsCustomCore(dir) {
		t.Fatal("升级后应写入自定义内核标记")
	}

	if err := m.RollbackBinary(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if got := readFile(t, target); string(got) != string(oldExe) {
		t.Fatal("回滚后目标文件应为旧内核")
	}
	if _, err := os.Stat(res.BackupPath); err != nil {
		t.Fatalf("回滚后应保留备份文件: %v", err)
	}
}

func TestRollbackWithoutBackupFails(t *testing.T) {
	m := NewManager(t.TempDir())
	if err := m.RollbackBinary(); err == nil {
		t.Fatal("没有备份时回滚必须报错")
	}
}

// 替换阶段被占用时：旧内核必须保持可用，备份也必须存在。
func TestInstallBinaryRenameFailureKeepsOld(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	target := filepath.Join(dir, "bin", "mihomo.exe")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	oldExe := fakePE(1)
	if err := os.WriteFile(target, oldExe, 0o755); err != nil {
		t.Fatal(err)
	}

	// 独占打开目标文件（不共享 delete），模拟内核尚未退出/文件被占用。
	p, err := windows.UTF16PtrFromString(target)
	if err != nil {
		t.Fatal(err)
	}
	// 允许其他进程读取（备份可以完成），但拒绝共享 delete（替换会失败）。
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
	}()

	if _, err := m.installBinary(fakePE(2)); err == nil {
		t.Fatal("目标被占用时替换必须失败")
	}
	// 先释放独占句柄，才能读取文件验证内容。
	_ = windows.CloseHandle(h)
	h = 0

	if got := readFile(t, target); string(got) != string(oldExe) {
		t.Fatal("替换失败后旧内核必须保持原样")
	}
	if got := readFile(t, target+".bak"); string(got) != string(oldExe) {
		t.Fatal("替换失败后应保留旧内核备份")
	}
}

func TestDownloadRejectsMissingDigest(t *testing.T) {
	m := NewManager(t.TempDir())
	_, err := m.DownloadAndReplace("https://github.com/MetaCubeX/mihomo/releases/download/x.zip", "", 0, time.Second)
	if err == nil {
		t.Fatal("缺少摘要时必须拒绝升级")
	}
}

func TestUpgradeRecordRoundTrip(t *testing.T) {
	m := NewManager(t.TempDir())
	rec := UpgradeRecord{
		BeforeVersion: "v1.0.0",
		TargetVersion: "v1.1.0",
		Hash:          "abcdef123456",
		BackupPath:    `C:\data\bin\mihomo.exe.bak`,
		ReplacedAt:    time.Now().Format(time.RFC3339),
	}
	if err := m.SaveUpgradeRecord(rec); err != nil {
		t.Fatal(err)
	}
	got, err := m.LastUpgradeRecord()
	if err != nil {
		t.Fatal(err)
	}
	if got.TargetVersion != rec.TargetVersion || got.BeforeVersion != rec.BeforeVersion {
		t.Fatalf("升级记录不一致: %+v", got)
	}
}

// 新内核无法启动时，回滚备份后旧内核必须还能正常启动。
func TestFailedNewKernelRollsBackToOld(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(binDir, "mihomo.exe")

	oldExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	oldBytes, err := os.ReadFile(oldExe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, oldBytes, 0o755); err != nil {
		t.Fatal(err)
	}

	m := NewManager(dir)
	m.exePath = target
	if _, err := m.installBinary(fakePE(9)); err != nil {
		t.Fatal(err)
	}

	// 假内核头无法被 Windows 加载；就绪探测确保不会误判为成功。
	m.SetStartupValidation("127.0.0.1:1", func(_ context.Context) error {
		return errProbe
	}, 300*time.Millisecond)
	if err := m.Start("dummy.yaml"); err == nil {
		_ = m.Stop()
		t.Fatal("无效的新内核不应启动成功")
	}

	if err := m.RollbackBinary(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	m.SetStartupValidation("", nil, 0)
	t.Setenv("ROUTEPILOT_FAKE_KERNEL", "sleep")
	if err := m.Start("dummy.yaml"); err != nil {
		t.Fatalf("回滚后的旧内核应能启动: %v", err)
	}
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
}
