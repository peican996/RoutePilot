package main

import (
	"os"
	"path/filepath"
	"testing"
)

// copyDirAll 是数据目录迁移的兜底路径（rename 跨卷失败时才走）。
// 它必须连子目录一起复制，否则订阅、规则文件会丢。
func TestCopyDirAll(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "moved")

	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(src, "bin")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := copyDirAll(src, dst); err != nil {
		t.Fatalf("复制失败: %v", err)
	}

	for _, rel := range []string{"a.txt", filepath.Join("bin", "b.txt")} {
		b, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			t.Fatalf("缺少文件 %s: %v", rel, err)
		}
		if len(b) == 0 {
			t.Fatalf("文件 %s 内容为空", rel)
		}
	}
}

// 数据目录必须叫 RoutePilot（当前产品名），旧名字只作为迁移来源存在。
func TestDataDirName(t *testing.T) {
	got := filepath.Base(dataDir())
	if got != dataDirName {
		t.Fatalf("数据目录名应为 %q，实际 %q", dataDirName, got)
	}
	if dataDirName == "" {
		t.Fatal("数据目录名不能为空")
	}
	for _, legacy := range legacyDirNames {
		if dataDirName == legacy {
			t.Fatalf("新旧目录名不能相同（%q），否则迁移逻辑会把数据当成已存在", legacy)
		}
	}
}

// R3 回归：复制失败必须返回 error，调用方才会保留旧目录。
// 以前失败一律 continue、永远返回 nil，随后旧目录被删，数据就丢了。
func TestCopyDirAllReportsFailure(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	if err := os.MkdirAll(filepath.Join(src, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "bin", "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 在目标位置放一个同名文件，让子目录创建必然失败
	if err := os.WriteFile(filepath.Join(dst, "bin"), []byte("occupied"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := copyDirAll(src, dst); err == nil {
		t.Fatal("子目录创建失败时必须返回 error，否则调用方会误删旧目录")
	}
}
