package procid

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizePathCanonical(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"普通路径", `C:\Apps\Chrome\chrome.exe`, `C:\Apps\Chrome\chrome.exe`},
		{"统一分隔符", `C:/Apps/Chrome/chrome.exe`, `C:\Apps\Chrome\chrome.exe`},
		{"去掉冗余段", `C:\Apps\.\Chrome\..\Chrome\chrome.exe`, `C:\Apps\Chrome\chrome.exe`},
		{"长路径前缀", `\\?\C:\Apps\Chrome\chrome.exe`, `C:\Apps\Chrome\chrome.exe`},
		{"UNC 长路径", `\\?\UNC\server\share\app.exe`, `\\server\share\app.exe`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NormalizePath(c.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("NormalizePath(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestNormalizePathEnvAndRelative(t *testing.T) {
	t.Setenv("ROUTEPILOT_TEST_DIR", `D:\Portable\App`)
	for _, in := range []string{`%ROUTEPILOT_TEST_DIR%\app.exe`, `${ROUTEPILOT_TEST_DIR}\app.exe`, `$ROUTEPILOT_TEST_DIR\app.exe`} {
		got, err := NormalizePath(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != `D:\Portable\App\app.exe` {
			t.Fatalf("%q 展开结果不符: %q", in, got)
		}
	}

	got, err := NormalizePath("app.exe")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("相对路径应转为绝对路径，实际 %q", got)
	}
}

func TestNormalizePathRejectsRuleBreakingChars(t *testing.T) {
	for _, p := range []string{`C:\Apps\A,B\app.exe`, "C:\\Apps\\A\nB\\app.exe", "C:\\Apps\\A\tB\\app.exe"} {
		if _, err := NormalizePath(p); err == nil {
			t.Fatalf("包含规则分隔符的路径应被拒绝: %q", p)
		}
	}
}

func TestFromProcessPrefersPath(t *testing.T) {
	id := FromProcess("chrome.exe", `C:\Apps\Chrome\chrome.exe`)
	if id.Kind != KindPath {
		t.Fatalf("应优先使用路径身份，实际 %s", id.Kind)
	}
	if id.Key != `path:c:\apps\chrome\chrome.exe` {
		t.Fatalf("键不符: %q", id.Key)
	}
	payload, err := id.RulePayload()
	if err != nil {
		t.Fatal(err)
	}
	if payload != `PROCESS-PATH,C:\Apps\Chrome\chrome.exe` {
		t.Fatalf("payload 不符: %q", payload)
	}
}

func TestSameNameDifferentDirsAreDistinct(t *testing.T) {
	a := FromProcess("chrome.exe", `C:\Apps\A\chrome.exe`)
	b := FromProcess("chrome.exe", `D:\Portable\B\chrome.exe`)
	if a.Key == b.Key || a.Equal(b) {
		t.Fatalf("同名不同目录不应视为同一身份: %q vs %q", a.Key, b.Key)
	}
	if !strings.EqualFold(a.Name, b.Name) {
		t.Fatal("basename 应相同")
	}
}

func TestPathIdentityIgnoresCase(t *testing.T) {
	a := FromProcess("chrome.exe", `C:\Apps\Chrome\chrome.exe`)
	b := FromProcess("CHROME.EXE", `c:\apps\chrome\CHROME.EXE`)
	if !a.Equal(b) || a.Key != b.Key {
		t.Fatalf("Windows 路径应大小写不敏感: %q vs %q", a.Key, b.Key)
	}
}

func TestFromProcessFallsBackToName(t *testing.T) {
	id := FromProcess("chrome.exe", "")
	if id.Kind != KindName || id.Key != "name:chrome.exe" || !id.Degraded {
		t.Fatalf("缺路径时应降级为名称身份: %+v", id)
	}
	payload, err := id.RulePayload()
	if err != nil {
		t.Fatal(err)
	}
	if payload != "PROCESS-NAME,chrome.exe" {
		t.Fatalf("payload 不符: %q", payload)
	}
}

func TestMalformedPathFallsBackWithReason(t *testing.T) {
	id := FromProcess("chrome.exe", `C:\Apps\A,B\chrome.exe`)
	if id.Kind != KindName || !id.Degraded || id.DegradeReason == "" {
		t.Fatalf("非法路径应降级并给出原因: %+v", id)
	}
}

func TestInvalidNameRejected(t *testing.T) {
	for _, name := range []string{"", "   ", "a,b.exe", "a\nb.exe", "a\tb.exe"} {
		if id := FromProcess(name, ""); id.Valid() {
			t.Fatalf("非法进程名不应生成身份: %q -> %+v", name, id)
		}
	}
}

func TestParseKeyLegacyCompatibility(t *testing.T) {
	id, ok := ParseKey("Chrome.EXE")
	if !ok {
		t.Fatal("旧版名称键应可解析")
	}
	if id.Kind != KindName || id.Key != "name:chrome.exe" {
		t.Fatalf("旧键迁移结果不符: %+v", id)
	}
	if !id.Degraded {
		t.Fatal("旧版键应标记为降级来源")
	}

	pathID, ok := ParseKey(`path:C:\Apps\Chrome\chrome.exe`)
	if !ok || pathID.Kind != KindPath {
		t.Fatalf("路径键解析失败: %+v", pathID)
	}
	if _, ok := ParseKey(`path:C:\Apps\A,B\chrome.exe`); ok {
		t.Fatal("含逗号的路径键应被拒绝")
	}
}

func TestRuleRendering(t *testing.T) {
	pathID := FromProcess("app.exe", `C:\Apps\app.exe`)
	rule, err := pathID.Rule("NodeA")
	if err != nil {
		t.Fatal(err)
	}
	if rule != `PROCESS-PATH,C:\Apps\app.exe,NodeA` {
		t.Fatalf("路径规则不符: %q", rule)
	}

	nameID := FromProcess("app.exe", "")
	rule, err = nameID.Rule("NodeA")
	if err != nil {
		t.Fatal(err)
	}
	if rule != "PROCESS-NAME,app.exe,NodeA" {
		t.Fatalf("名称规则不符: %q", rule)
	}
}
