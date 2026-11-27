package main

import (
	"sync"
	"testing"
	"time"
)

// 回归 (review 三轮): 必须调用生产实际使用的清理入口 newCRPSExitCallback, 用替身
// 覆盖「服务启动失败、无预取消且有采样在途」形态, 断言 Save 严格发生在采样退出
// 之后。生产顺序被改回去 (先 Save/后 cancel) 时本测试即失败——不需要真机。
func TestCRPSExitCallbackOrder(t *testing.T) {
	// 可控在途采样替身: samplerDone 由替身采样 goroutine 关闭, 模拟真实
	// StartEnergy 的 stop (等 ctx 取消后采样 goroutine 退出)。回调执行时刻
	// 采样仍在途 (未预取消)。
	ctxCanceled := make(chan struct{})
	samplerDone := make(chan struct{})
	var seqMu sync.Mutex
	var events []string

	stop := func() {
		<-samplerDone // 等 ctx 取消后采样 goroutine 退出 (join 语义)
		seqMu.Lock()
		events = append(events, "samplerExited")
		seqMu.Unlock()
	}
	save := func() {
		seqMu.Lock()
		events = append(events, "saved")
		seqMu.Unlock()
	}
	// 替身采样 goroutine: 只等 ctx 取消, 模拟 energySampler 对 ctx.Done 的响应
	go func() {
		<-ctxCanceled
		close(samplerDone)
	}()

	exit := newCRPSExitCallback(
		func() { close(ctxCanceled) },
		stop,
		save,
	)

	done := make(chan struct{})
	go func() { exit(); close(done) }() // 信号未到, cancel 未被外部调用
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("生产退出回调未在 5s 内返回: 启动失败路径死锁")
	}

	seqMu.Lock()
	defer seqMu.Unlock()
	if len(events) != 2 {
		t.Fatalf("回调事件序列异常: %v", events)
	}
	if events[0] != "samplerExited" || events[1] != "saved" {
		t.Fatalf("顺序错误: %v, Save 必须发生在采样退出之后", events)
	}
}

// 幂等: 宿主正常退出与 server 错误两条路径可能重复调用回调。
func TestCRPSExitCallbackIdempotentRepeat(t *testing.T) {
	var mu sync.Mutex
	cancels, joins, saves := 0, 0, 0
	exit := newCRPSExitCallback(
		func() { mu.Lock(); cancels++; mu.Unlock() },
		func() { mu.Lock(); joins++; mu.Unlock() },
		func() { mu.Lock(); saves++; mu.Unlock() },
	)
	exit()
	exit()
	mu.Lock()
	defer mu.Unlock()
	if cancels != 2 || joins != 2 || saves != 2 {
		t.Fatalf("重复调用应完整重放: cancel=%d join=%d save=%d", cancels, joins, saves)
	}
}
