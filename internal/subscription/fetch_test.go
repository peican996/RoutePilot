package subscription

import "testing"

// subscription-userinfo 头解析：格式五花八门，缺字段、带空格、
// 非数字都要能兜住，否则流量与到期时间会显示成乱值。
func TestParseUserInfo(t *testing.T) {
	cases := []struct {
		name                    string
		in                      string
		up, down, total, expire int64
	}{
		{"完整", "upload=100; download=200; total=1000; expire=1700000000", 100, 200, 1000, 1700000000},
		{"带空格", " upload = 5 ; download = 6 ", 5, 6, 0, 0},
		{"缺字段", "total=500", 0, 0, 500, 0},
		{"空串", "", 0, 0, 0, 0},
		{"非数字", "upload=abc; download=7", 0, 7, 0, 0},
		{"多余分号", "upload=1;;download=2;", 1, 2, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up, down, total, expire := parseUserInfo(c.in)
			if up != c.up || down != c.down || total != c.total || expire != c.expire {
				t.Fatalf("得到 (%d,%d,%d,%d)，期望 (%d,%d,%d,%d)",
					up, down, total, expire, c.up, c.down, c.total, c.expire)
			}
		})
	}
}

// 证书类错误必须被识别出来，才能给出"可忽略证书重试"的提示；
// 普通网络错误不应误判。
func TestIsTLSError(t *testing.T) {
	if !isTLSError(errString("Get \"https://x\": tls: failed to verify certificate: x509: certificate signed by unknown authority")) {
		t.Fatal("x509 错误应被识别为 TLS 错误")
	}
	if isTLSError(errString("dial tcp 1.2.3.4:443: i/o timeout")) {
		t.Fatal("超时不应被识别为 TLS 错误")
	}
	if isTLSError(nil) {
		t.Fatal("nil 不应被识别为 TLS 错误")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
