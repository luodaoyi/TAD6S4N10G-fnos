package main

import (
	"errors"
	"testing"
	"time"
)

// 回归: 同一分钟内 A → B → A, 第二个 A 必须被节流 (旧字符串比较实现会放行刷屏)。
func TestErrorLogThrottleAlternatingWithinWindow(t *testing.T) {
	current := time.Unix(0, 0)
	errA := errors.New("open failed: device busy")
	errB := errors.New("poll failed: io error")
	throttle := newErrorLogThrottle(func() time.Time { return current })

	steps := []struct {
		err   error
		allow bool
	}{
		{errA, true},  // 首次 A 记录
		{errB, true},  // 换 B 记录
		{errA, false}, // 回到 A, 仍在 1 分钟窗口内 -> 抑制
		{errB, false}, // 回到 B, 同样抑制
	}
	for i, s := range steps {
		if got := throttle.allow(s.err); got != s.allow {
			t.Fatalf("step %d: allow=%v, want %v", i, got, s.allow)
		}
	}
	// 窗口过后 A 重新放行
	current = current.Add(61 * time.Second)
	if !throttle.allow(errA) {
		t.Fatal("窗口结束后应重新记录")
	}
}

func TestErrorLogThrottleReset(t *testing.T) {
	current := time.Unix(0, 0)
	errA := errors.New("open failed: device busy")
	throttle := newErrorLogThrottle(func() time.Time { return current })

	if !throttle.allow(errA) {
		t.Fatal("首次应记录")
	}
	throttle.reset()
	if !throttle.allow(errA) {
		t.Fatal("reset 后同错误应立即重新记录")
	}
}
