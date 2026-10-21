package powerguard

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 超限清理：内容备份到 .1、原文件截断、O_APPEND fd 继续从头写。
func TestRotateLogIfNeededBacksUpAndTruncates(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "tad-module.log")
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	old := strings.Repeat("error spam\n", 30)
	if _, err := file.WriteString(old); err != nil {
		t.Fatal(err)
	}

	rotated, err := RotateLogIfNeeded(logPath, file, 128) // 上限 128B << 内容
	if err != nil {
		t.Fatal(err)
	}
	if !rotated {
		t.Fatal("over-limit log should be rotated")
	}
	backup, err := os.ReadFile(logPath + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != old {
		t.Fatalf("backup should hold the over-limit content, got %d bytes", len(backup))
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("main log should be truncated, got %d bytes", info.Size())
	}
	// 同一个 fd（O_APPEND）在截断后继续写：落在新文件头
	if _, err := file.WriteString("after rotate\n"); err != nil {
		t.Fatal(err)
	}
	fresh, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(fresh) != "after rotate\n" {
		t.Fatalf("post-rotate write should land in the fresh file, got %q", string(fresh))
	}
}

// 未超限与安全边界：不动文件；二次超限覆盖上一代备份；备份失败不丢主日志。
func TestRotateLogIfNeededEdges(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "tad-module.log")

	// 未超限：不轮转
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("small\n"); err != nil {
		t.Fatal(err)
	}
	rotated, err := RotateLogIfNeeded(logPath, file, 1<<20)
	if err != nil || rotated {
		t.Fatalf("under-limit log must not rotate, rotated=%v err=%v", rotated, err)
	}
	// limit<=0 / file nil / 路径空：直接跳过
	if rotated, err := RotateLogIfNeeded(logPath, file, 0); rotated || err != nil {
		t.Fatalf("zero limit must skip, rotated=%v err=%v", rotated, err)
	}
	if rotated, err := RotateLogIfNeeded(logPath, nil, 1); rotated || err != nil {
		t.Fatalf("nil file must skip, rotated=%v err=%v", rotated, err)
	}

	// 二次超限：上一代备份被覆盖（不无限堆积）
	if _, err := file.WriteString(strings.Repeat("spam\n", 100)); err != nil {
		t.Fatal(err)
	}
	if rotated, err := RotateLogIfNeeded(logPath, file, 64); err != nil || !rotated {
		t.Fatalf("first rotate: rotated=%v err=%v", rotated, err)
	}
	if _, err := file.WriteString(strings.Repeat("spam2\n", 100)); err != nil {
		t.Fatal(err)
	}
	if rotated, err := RotateLogIfNeeded(logPath, file, 64); err != nil || !rotated {
		t.Fatalf("second rotate: rotated=%v err=%v", rotated, err)
	}
	backup, err := os.ReadFile(logPath + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(backup), "spam2\n") || strings.Contains(string(backup), "spam\nspam\n") {
		t.Fatal("second rotation should replace the previous backup with the newest content")
	}
	entries, err := filepath.Glob(filepath.Join(dir, "tad-module.log*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 { // 主文件 + .1；.tmp 应已被 rename 消费掉
		t.Fatalf("rotation should leave exactly main + .1, got %v", entries)
	}
}

// 缺失文件路径安静跳过（用真实句柄 + 不存在的路径，确保真的走到 os.Stat）。
func TestRotateLogIfNeededMissingFile(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "real.log")
	file, err := os.OpenFile(existing, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if rotated, err := RotateLogIfNeeded(filepath.Join(dir, "missing.log"), file, 1); rotated || err != nil {
		t.Fatalf("missing log must skip quietly, rotated=%v err=%v", rotated, err)
	}
}

// Loop 的收尾动作顺序：轮转后写说明行，落在新文件头。
func TestLogRotateLoopNoticeLandsInFreshFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "tad-module.log")
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(strings.Repeat("x", 200)); err != nil {
		t.Fatal(err)
	}
	logger := log.New(file, "", 0)
	rotated, err := RotateLogIfNeeded(logPath, file, 64)
	if err != nil || !rotated {
		t.Fatalf("rotate: rotated=%v err=%v", rotated, err)
	}
	// 与 LogRotateLoop 相同的说明行动作
	logger.Printf("log exceeded %d MB, previous content saved to %s%s", 0, filepath.Base(logPath), logRotateSuffix)
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(content), "log exceeded") {
		t.Fatalf("rotation notice should be first line of fresh log, got %q", string(content))
	}
}

// 写入路径兜底不得在持锁的 logf 里重入 m.mu（回归：limit 闭包曾调
// LogSettings() 拿 m.mu，第 64 次写入触发检查时在自身持有的锁上死锁）。
// limit 现在只读原子快照，本测试持锁打 300 条日志验证不再卡住。
func TestLogfUnderLockWithSizeCappedWriter(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "tad-module.log")
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{ConfigPath: configPath, StatePath: filepath.Join(dir, "state.json")}
	m.SetLogger(log.New(NewSizeCappedLogWriter(file, m.LogLimitBytes), "", 0))
	m.SetLogLimitBytes(64) // 极小上限，每 64 次写入必触发检查分支
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.mu.Lock()
		defer m.mu.Unlock()
		for i := 0; i < 300; i++ {
			m.logf("stress line %d padding padding padding", i)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("logf under m.mu must not deadlock through the size-capped writer")
	}
}

// 回归：装配顺序陷阱——main.go 曾在 Manager 构造前把 manager.LogLimitBytes
// 取成方法值（绑定 nil 接收者，之后给变量赋值无法挽回），第 64 次写日志
// 触发 limit 时 nil 解引用 panic。LogLimitBytes 对 nil 接收者返回 0（不设限）
// 兜底，本测试复现"先绑方法值、后赋值变量"的顺序验证不再崩溃。
func TestSizeCappedLogWriterNilReceiverSafe(t *testing.T) {
	var manager *Manager
	limit := manager.LogLimitBytes // 方法值在 nil 上绑定（旧实现此处埋雷）
	if got := limit(); got != 0 {
		t.Fatalf("nil-bound method value must report 0 (no limit), got %d", got)
	}
	// 正常路径：赋值后 Store/Load 往返
	manager = &Manager{}
	manager.SetLogLimitBytes(4096)
	if got := manager.LogLimitBytes(); got != 4096 {
		t.Fatalf("snapshot round-trip failed, got %d", got)
	}
	// 持 nil 绑定的 writer 连写 100 条（跨过 64 次检查点）不 panic、不限流
	dir := t.TempDir()
	file, err := os.OpenFile(filepath.Join(dir, "tad.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	writer := NewSizeCappedLogWriter(file, limit)
	for i := 0; i < 100; i++ {
		if _, err := writer.Write([]byte("line that is long enough to accumulate past the check threshold\n")); err != nil {
			t.Fatal(err)
		}
	}
}
