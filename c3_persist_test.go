package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// 配置写盘失败时，内存规则不能被提前替换，否则界面回滚后会与磁盘不一致。
func TestSetProcessRuleWriteFailureLeavesMemoryUnchanged(t *testing.T) {
	svc := NewCoreService()
	svc.procRules = map[string]string{
		`path:c:\apps\a\a.exe`: "direct",
	}

	orig := writeFileAtomic
	writeFileAtomic = func(string, []byte, os.FileMode) error {
		return errors.New("disk full")
	}
	t.Cleanup(func() { writeFileAtomic = orig })

	if _, err := svc.SetProcessRule(`path:c:\apps\b\b.exe`, "direct"); err == nil {
		t.Fatal("写盘失败时 SetProcessRule 必须返回错误")
	}
	if len(svc.procRules) != 1 || svc.procRules[`path:c:\apps\a\a.exe`] != "direct" {
		t.Fatalf("写盘失败后内存规则不应变化: %v", svc.procRules)
	}
}

// 原子写：成功后内容完整，且不残留临时文件。
func TestWriteFileAtomicLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("new-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new-content" {
		t.Fatalf("内容不符: %q", got)
	}
	left, err := filepath.Glob(filepath.Join(dir, ".config.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("不应残留临时文件: %v", left)
	}
}
