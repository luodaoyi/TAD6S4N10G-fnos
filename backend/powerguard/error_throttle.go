package main

import (
	"sync"
	"time"
)

// errorLogThrottle 按「同错误文本 1 分钟内只记一次」节流错误日志。纯字符串比较
// (只记 last) 压不住交替错误: A→B→A 时第二个 A 与 last 不同会被放行刷屏。
// 节流器按错误文本各自计时 (小环形表足够 gpioLoop 的两三个错误种类, 超表淘汰最旧)。
// now 注入便于测试用固定时钟。
type errorLogThrottle struct {
	now    func() time.Time
	mu     sync.Mutex
	window time.Duration
	seen   map[string]time.Time
	order  []string // 淘汰用: 按首次记录顺序
	cap    int
}

func newErrorLogThrottle(now func() time.Time) *errorLogThrottle {
	return &errorLogThrottle{now: now, window: time.Minute, seen: map[string]time.Time{}, cap: 8}
}

// allow 报告该错误本次是否应记录: 同文本仍在窗口内则抑制, 否则记录并刷新起点。
func (t *errorLogThrottle) allow(err error) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	key := err.Error()
	if at, ok := t.seen[key]; ok && now.Sub(at) < t.window {
		return false
	}
	if _, ok := t.seen[key]; !ok {
		if len(t.order) >= t.cap { // 淘汰最旧, 防止错误种类爆炸时无界增长
			delete(t.seen, t.order[0])
			t.order = t.order[1:]
		}
		t.order = append(t.order, key)
	}
	t.seen[key] = now
	return true
}

// reset 清空节流状态 (如 gpio 禁用后恢复)。
func (t *errorLogThrottle) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seen = map[string]time.Time{}
	t.order = nil
}
