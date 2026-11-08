package autostart

import (
	"strings"
	"testing"
	"unicode/utf16"
)

func decodeUTF16LE(t *testing.T, data []byte) string {
	t.Helper()
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xFE {
		t.Fatalf("任务 XML 必须是带 BOM 的 UTF-16 LE")
	}
	units := make([]uint16, 0, (len(data)-2)/2)
	for i := 2; i+1 < len(data); i += 2 {
		units = append(units, uint16(data[i])|uint16(data[i+1])<<8)
	}
	return string(utf16.Decode(units))
}

// 计划任务 XML 用 HighestAvailable 才能让 TUN 随开机静默启动；
// 路径里的特殊字符必须转义，否则 schtasks 会解析失败。
func TestTaskXML(t *testing.T) {
	exe := `C:\Program Files\A&B\NexusLink.exe`
	data, err := taskXMLWithRunLevel(exe, "HighestAvailable")
	if err != nil {
		t.Fatal(err)
	}

	text := decodeUTF16LE(t, data)
	for _, want := range []string{
		"<RunLevel>HighestAvailable</RunLevel>",
		"<Command>C:\\Program Files\\A&amp;B\\NexusLink.exe</Command>",
		"<LogonTrigger>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("任务 XML 缺少 %q", want)
		}
	}
}
