package kernel

import "testing"

// SemVer 2.0.0 严格格式校验：非法版本不得被当作有效版本参与比较。
func TestParseSemverStrictInvalid(t *testing.T) {
	invalid := []string{
		"",                                 // 空
		"01.02.03",                         // 核心版本前导零
		"1.0",                              // 缺段
		"1.0.0.1",                          // 多余段
		"1..0",                             // 空段
		"v",                                // 只有前缀
		"1.0.0-01",                         // 数字预发布前导零
		"1.0.0-alpha..beta",                // 空预发布标识符
		"1.0.0-",                           // 空预发布
		"1.0.0+",                           // 空 build metadata
		"1.0.0+build+other",                // 多个 '+'
		"1.0.0-中文",                         // 非 ASCII
		"1.0.0-!",                          // 非法字符
		"1.0.0-alpha_1",                    // 下划线非法
		"1.0.0+build_meta",                 // build 下划线非法
		"99999999999999999999999999.0.0",   // 核心数字溢出
		"1.0.0-99999999999999999999999999", // 数字预发布溢出
	}
	for _, v := range invalid {
		if _, ok := parseSemver(v); ok {
			t.Fatalf("非法版本被接受: %q", v)
		}
	}
}

func TestParseSemverStrictValid(t *testing.T) {
	valid := []string{
		"0.0.0",
		"1.19.0",
		"v1.19.0",
		"V1.19.0",
		"1.0.0-alpha",
		"1.0.0-alpha.1",
		"1.0.0-0A",      // 字母数字混合，前导 0 允许
		"1.0.0-x-y-z",   // 连字符允许
		"1.0.0+build.5", // build 允许前导零
		"1.0.0+build.001",
		"1.0.0-rc.1+build.9",
	}
	for _, v := range valid {
		if _, ok := parseSemver(v); !ok {
			t.Fatalf("合法版本被拒绝: %q", v)
		}
	}
}

// 非法版本不得触发升级提示（保守处理，避免降级）。
func TestVersionNewerRejectsInvalid(t *testing.T) {
	cases := []struct{ cur, latest string }{
		{"nightly", "1.19.1"},
		{"1.19.0", "nightly"},
		{"01.02.03", "1.19.1"},
		{"1.19.0", "1.19.1-01"},
		{"1.19.0", "1.19.1+base+extra"},
	}
	for _, c := range cases {
		if versionNewer(c.cur, c.latest) {
			t.Fatalf("非法版本不应提示升级: cur=%q latest=%q", c.cur, c.latest)
		}
	}
}
