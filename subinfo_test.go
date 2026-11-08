package main

import (
	"reflect"
	"testing"
)

// 切换订阅时必须从配置里重新统计节点/组/规则数量。
// 之前的实现只恢复 meta，切完订阅设置页会一直显示 0 节点。
func TestApplySubConfigCounts(t *testing.T) {
	cfg := map[string]any{
		"proxies":      []any{map[string]any{"name": "a"}, map[string]any{"name": "b"}},
		"proxy-groups": []any{map[string]any{"name": "PROXY"}},
		"rules":        []any{"MATCH,PROXY", "GEOIP,CN,DIRECT"},
	}
	info := SubInfo{URL: "https://example.com/sub"}

	applySubConfigCounts(&info, cfg)

	if info.Nodes != 2 || info.Groups != 1 || info.Rules != 2 {
		t.Fatalf("统计结果不符：nodes=%d groups=%d rules=%d", info.Nodes, info.Groups, info.Rules)
	}
	if info.URL != "https://example.com/sub" {
		t.Fatalf("统计不应改动其他元信息，URL 变成 %q", info.URL)
	}
}

// 缺字段或类型不对时返回 0，而不是 panic。
func TestApplySubConfigCountsMissingFields(t *testing.T) {
	cases := []map[string]any{
		{},
		{"proxies": "not-an-array"},
		{"proxies": nil},
	}
	for _, cfg := range cases {
		info := SubInfo{}
		applySubConfigCounts(&info, cfg)
		if !reflect.DeepEqual(info, SubInfo{}) {
			t.Fatalf("空配置应保持零值，实际 %+v", info)
		}
	}
}
