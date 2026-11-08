package main

import (
	"testing"

	"proxdeck/internal/subscription"
)

// 切换订阅 / 自动更新后必须使用解析结果里的计数。
// 之前的实现只恢复 meta，切完订阅设置页会一直显示 0 节点。
func TestSubInfoFromResultCounts(t *testing.T) {
	res := &subscription.Result{
		Nodes: []subscription.NodeInfo{
			{Name: "a", Type: "ss"},
			{Name: "b", Type: "trojan"},
		},
		Groups: []string{"PROXY"},
		Rules:  2,
	}
	meta := subMeta{URL: "https://example.com/sub", Name: "test", Upload: 1}

	info := subInfoFromResult(meta, res)

	if info.Nodes != 2 || info.Groups != 1 || info.Rules != 2 {
		t.Fatalf("统计结果不符：nodes=%d groups=%d rules=%d", info.Nodes, info.Groups, info.Rules)
	}
	if info.URL != meta.URL || info.Name != meta.Name || info.Upload != meta.Upload {
		t.Fatalf("元信息应保持不变，实际 %+v", info)
	}
}

func TestSubInfoFromResultEmpty(t *testing.T) {
	info := subInfoFromResult(subMeta{}, &subscription.Result{})
	if info.Nodes != 0 || info.Groups != 0 || info.Rules != 0 {
		t.Fatalf("空结果应全部为 0，实际 %+v", info)
	}
}
