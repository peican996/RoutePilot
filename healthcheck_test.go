package main

import (
	"errors"
	"testing"
	"time"
)

// 探测偶发超时不该判失败——这是"网络不通"误报的根因，锁住复测语义。
func TestProbeTwiceRecoversFromTransientFailure(t *testing.T) {
	calls := 0
	ok := probeTwice(func() (int, error) {
		calls++
		if calls == 1 {
			return -1, errors.New("Timeout")
		}
		return 230, nil
	}, 0)
	if !ok {
		t.Fatal("第一次失败第二次成功，应判链路正常")
	}
	if calls != 2 {
		t.Fatalf("期望复测一次共 2 次调用，实际 %d", calls)
	}
}

func TestProbeTwiceFailsOnlyWhenBothAttemptsFail(t *testing.T) {
	calls := 0
	ok := probeTwice(func() (int, error) {
		calls++
		return -1, errors.New("Timeout")
	}, 0)
	if ok {
		t.Fatal("两次都失败，应判链路异常")
	}
	if calls != 2 {
		t.Fatalf("期望 2 次调用，实际 %d", calls)
	}
}

func TestProbeTwiceTreatsZeroDelayAsFailure(t *testing.T) {
	// mihomo 探测超时返回 {"message":"Timeout"}，解析后 delay=0、err=nil
	calls := 0
	ok := probeTwice(func() (int, error) {
		calls++
		return 0, nil
	}, 0)
	if ok {
		t.Fatal("delay=0 是超时，不能当成功")
	}
	if calls != 2 {
		t.Fatalf("期望 2 次调用，实际 %d", calls)
	}
}

func TestProbeTwiceNoSecondAttemptAfterSuccess(t *testing.T) {
	calls := 0
	ok := probeTwice(func() (int, error) {
		calls++
		return 188, nil
	}, 0)
	if !ok || calls != 1 {
		t.Fatalf("首次成功不应复测：ok=%v calls=%d", ok, calls)
	}
}

func TestProbeTwiceHonorsRetryDelay(t *testing.T) {
	start := time.Now()
	probeTwice(func() (int, error) {
		return -1, errors.New("Timeout")
	}, 50*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("复测前应等待 retryDelay，实际只过 %v", elapsed)
	}
}
