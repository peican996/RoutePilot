//go:build windows

package autostart

import (
	"errors"
	"strings"
	"testing"
)

// 中文 Windows 上 schtasks 的错误文案是 GBK 编码的。
// 用 UTF-8 字面量匹配会永远失败，把"任务不存在"误判成"查询失败"——
// 后果是自启升级到计划任务这一步永远走不通，开机永远停在非管理员。
// 这组字节就是「错误: 系统找不到指定的文件。」的 GBK 原文。
var gbkTaskNotFound = []byte{
	0xB4, 0xED, 0xCE, 0xF3, 0x3A, 0x20, 0xCF, 0xB5, 0xCD, 0xB3,
	0xD5, 0xD2, 0xB2, 0xBB, 0xB5, 0xBD, 0xD6, 0xB8, 0xB6, 0xA8,
	0xB5, 0xC4, 0xCE, 0xC4, 0xBC, 0xFE, 0xA1, 0xA3,
}

func TestClassifyTaskQueryHandlesGBKNotFound(t *testing.T) {
	st, err := classifyTaskQuery(gbkTaskNotFound, errors.New("exit status 1"))
	if err != nil {
		t.Fatalf("GBK 的「找不到」应当判为任务不存在，却报了错: %v", err)
	}
	if st != taskNotFound {
		t.Fatalf("应当为 taskNotFound，实际 %v", st)
	}
}

// 英文系统的输出不能被这次改动破坏。
func TestClassifyTaskQueryHandlesEnglishNotFound(t *testing.T) {
	out := []byte("ERROR: The system cannot find the file specified.")
	st, err := classifyTaskQuery(out, errors.New("exit status 1"))
	if err != nil {
		t.Fatalf("英文「cannot find」应当判为不存在: %v", err)
	}
	if st != taskNotFound {
		t.Fatalf("应当为 taskNotFound，实际 %v", st)
	}
}

// 真正的故障（权限不足等）必须继续报错，不能被当成"任务不存在"——
// 那会导致再写一份 Run 项，登录时双实例启动。
func TestClassifyTaskQueryKeepsUnknownError(t *testing.T) {
	out := []byte("ERROR: Access is denied.")
	st, err := classifyTaskQuery(out, errors.New("exit status 1"))
	if err == nil {
		t.Fatal("权限错误不应被吞成 taskNotFound")
	}
	if st != taskUnknown {
		t.Fatalf("应当为 taskUnknown，实际 %v", st)
	}
}

// 已经是合法 UTF-8 的内容不能被二次转换搞乱。
func TestDecodeANSIToUTF8PassesThroughUTF8(t *testing.T) {
	in := "查询计划任务失败"
	if got := decodeANSIToUTF8([]byte(in)); got != in {
		t.Fatalf("UTF-8 内容被改动了: %q", got)
	}
}

// GBK 字节要能还原成正确的中文，否则匹配关键字仍然失效。
func TestDecodeANSIToUTF8ConvertsGBK(t *testing.T) {
	got := decodeANSIToUTF8(gbkTaskNotFound)
	if !strings.Contains(got, "找不到") {
		t.Fatalf("GBK 未正确解码，得到: %q", got)
	}
}

// 空输出不能 panic。
func TestDecodeANSIToUTF8Empty(t *testing.T) {
	if got := decodeANSIToUTF8(nil); got != "" {
		t.Fatalf("空输入应为空串，实际 %q", got)
	}
}
