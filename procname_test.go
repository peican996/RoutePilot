package main

import "testing"

// 进程名会被拼进 mihomo 的 PROCESS-NAME,<name> 规则（逗号分隔），
// 所以含逗号或换行的名字必须拒绝，否则一条规则会变成多条。
func TestSanitizeProcName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{in: "chrome.exe", want: "chrome.exe"},
		{in: "  Trae CN.exe  ", want: "Trae CN.exe"},
		{in: "游戏.exe", want: "游戏.exe"},

		// 这些必须被拒绝
		{in: "a,b.exe", want: ""},          // 逗号会把一条规则拆成两条
		{in: "evil\nRULE-SET,x", want: ""}, // 换行注入新规则
		{in: "evil\r\n- PROCESS-NAME", want: ""},
		{in: "path/to/x.exe", want: ""}, // 不该带路径
		{in: `C:\evil.exe`, want: ""},
		{in: "tab\tname.exe", want: ""},
		{in: "nul\x00byte", want: ""},
		{in: "", want: ""},
		{in: "   ", want: ""},
	}

	for _, c := range cases {
		got := sanitizeProcName(c.in)
		if got != c.want {
			t.Errorf("sanitizeProcName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
