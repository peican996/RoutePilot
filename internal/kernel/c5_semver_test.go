package kernel

import "testing"

// SemVer 2.0.0 优先级边界：预发布、数字/字母标识符、build metadata、缺省段。
func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"1.19.0", "1.19.0", 0, true},
		{"v1.19.0", "1.19.0", 0, true},
		{"V1.19.0", "v1.19.0", 0, true},
		{"1.19.1", "1.19.0", 1, true},
		{"1.19.0", "1.19.1", -1, true},
		{"1.20.0", "1.19.9", 1, true},
		{"2.0.0", "1.999.999", 1, true},
		{"1.19", "1.19.0", 0, false}, // 严格 SemVer 要求三段核心版本
		{"1.19.0.1", "1.19.0", 0, false},

		// 预发布优先级低于同核心的正式版
		{"1.19.0-alpha", "1.19.0", -1, true},
		{"1.19.0", "1.19.0-beta", 1, true},
		{"1.19.0-alpha", "1.19.0-beta", -1, true},
		{"1.19.0-alpha", "1.19.0-alpha.1", -1, true},
		{"1.19.0-alpha.1", "1.19.0-alpha.2", -1, true},
		{"1.19.0-alpha.2", "1.19.0-alpha.10", -1, true},
		{"1.19.0-rc.1", "1.19.0-rc.2", -1, true},
		{"1.19.0-rc.2", "1.19.0-rc.10", -1, true},
		// 数字标识符优先级低于字母标识符
		{"1.19.0-alpha.beta", "1.19.0-alpha.1", 1, true},
		{"1.19.0-alpha.1", "1.19.0-alpha.beta", -1, true},

		// build metadata 不参与比较
		{"1.19.0+build.5", "1.19.0+build.6", 0, true},
		{"1.19.0-rc.1+build", "1.19.0-rc.1", 0, true},
		{"1.19.0+exp.sha.5114f85", "1.19.0", 0, true},

		// 非法版本
		{"", "1.0.0", 0, false},
		{"1.0.0", "", 0, false},
		{"bad", "1.0.0", 0, false},
		{"1.x.0", "1.0.0", 0, false},
		{"1.0.0-", "1.0.0", 0, false},
	}
	for _, tc := range cases {
		got, ok := compareSemver(tc.a, tc.b)
		if ok != tc.ok {
			t.Fatalf("compareSemver(%q,%q) ok=%v, want %v", tc.a, tc.b, ok, tc.ok)
		}
		if ok && got != tc.want {
			t.Fatalf("compareSemver(%q,%q)=%d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// HasUpdate 语义：只有 latest 严格大于当前版本才算有更新，
// 绝不能把用户已装的更新版本误判为旧版本后引导降级。
func TestVersionNewerSemantics(t *testing.T) {
	cases := []struct {
		cur, latest string
		want        bool
	}{
		{"1.19.0", "1.19.0", false},
		{"v1.19.0", "v1.19.0", false},
		{"1.19.0", "1.19.1", true},
		{"1.19.1", "1.19.0", false},
		{"1.19.0", "1.19.0-rc.1", false},
		{"1.19.0-rc.1", "1.19.0", true},
		{"1.19.0-rc.1", "1.19.0-rc.2", true},
		{"1.19.0+build9", "1.19.0+build10", false},
		{"1.19.0", "1.19.0+build10", false},
		// 无法解析时保守处理：不提示升级
		{"nightly", "1.19.1", false},
		{"1.19.0", "nightly", false},
	}
	for _, tc := range cases {
		if got := versionNewer(tc.cur, tc.latest); got != tc.want {
			t.Fatalf("versionNewer(%q,%q)=%v, want %v", tc.cur, tc.latest, got, tc.want)
		}
	}
}
