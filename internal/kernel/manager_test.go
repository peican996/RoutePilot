package kernel

import "testing"

// Since 是日志增量拉取的核心：序号相同返回空、落后太多返回整段。
// 这个契约错了会导致前端日志重复或丢失。
func TestLogBufferSince(t *testing.T) {
	b := NewLogBuffer(3)

	b.Add("a")
	b.Add("b")

	lines, seq, _ := b.Since(0)
	if len(lines) != 2 || seq != 2 {
		t.Fatalf("首次全量应为 2 行 seq=2，实际 %d 行 seq=%d", len(lines), seq)
	}

	// 没有新日志
	lines, seq2, _ := b.Since(seq)
	if len(lines) != 0 || seq2 != seq {
		t.Fatalf("无新日志应返回空，实际 %d 行 seq=%d", len(lines), seq2)
	}

	// 新增一条，只应拿到这一条
	b.Add("c")
	lines, seq3, _ := b.Since(seq)
	if len(lines) != 1 || lines[0] != "c" || seq3 != 3 {
		t.Fatalf("增量应只含 c，实际 %v seq=%d", lines, seq3)
	}

	// 缓冲区容量 3，再加两条会把 a 挤掉；序号落后时应返回整段当前缓冲
	b.Add("d")
	b.Add("e")
	lines, seq4, trunc := b.Since(1)
	if len(lines) != 3 || seq4 != 5 {
		t.Fatalf("截断后应返回整段 3 行 seq=5，实际 %d 行 seq=%d", len(lines), seq4)
	}
	if !trunc {
		t.Fatal("游标落后于缓冲起点时必须标记 truncated")
	}

	// 清空后序号归零，前端据此重建
	b.Clear()
	lines, seq5, _ := b.Since(seq4)
	if len(lines) != 0 || seq5 != 0 {
		t.Fatalf("清空后应返回空且 seq=0，实际 %d 行 seq=%d", len(lines), seq5)
	}
}
