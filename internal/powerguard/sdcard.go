package powerguard

import (
	"context"
	"fmt"
	"time"
)

// sdcard.go：刷新SD卡（重新识别 USB 读卡器）的跨平台外壳。
//
// 为什么需要它：USB 读卡器换 SD 卡之后，卡经常要等很久、甚至要重新插拔才被
// 内核识别——读卡器换了介质不一定会重新上报 LUN，内核也就不会重新扫描。
// 作者原版 RR 插件 tad6s4n10g 的 sdcMain() 用的正是把读卡器 USB 设备的
// authorized 置 0 再置 1（等效「拔掉再插上」）这个办法，这里沿用同一个思路，
// 平台相关的实现见 sdcard_linux.go。

// SdcardRefreshResult 一次刷新的结果，直接序列化给界面。
type SdcardRefreshResult struct {
	Summary string `json:"summary"`
	// Devices 是被重新识别的 USB 存储设备（如 2-1.4）。
	Devices []string `json:"devices,omitempty"`
	// ScannedHosts 是触发重新扫描的 SCSI 控制器数量。
	ScannedHosts int `json:"scanned_hosts,omitempty"`
	// Skipped 记录被跳过的设备及原因（例如上面还挂着正在使用的盘）。
	Skipped []string `json:"skipped,omitempty"`
	// Failed 记录重新识别失败的设备（例如没有权限写 /sys）。
	Failed []string `json:"failed,omitempty"`
	// NewDevices 是刷新后新出现或容量发生变化的块设备（如 sdc1），用来确认卡确实认出来了。
	NewDevices []string `json:"new_devices,omitempty"`
	ElapsedMS  int64    `json:"elapsed_ms,omitempty"`
}

type sdcardRunState struct {
	running     bool
	startedAt   time.Time
	lastSummary string
	lastError   string
}

// StartRefreshSDCard 在后台刷新一次，立即返回。按键动作用它，避免把 GPIO
// 轮询循环堵上好几秒；结束时用与备份一致的蜂鸣 + 闪灯给出回执。
//
// 占位只做一次：这里占好之后直接调 runRefreshSDCardClaimed，不能再走会自己
// 占位的 RunRefreshSDCard，否则会被自己刚设的 running 标记拒掉（按键刷新
// 永远报「上一次刷新SD卡还没结束」）。
func (m *Manager) StartRefreshSDCard() error {
	if err := m.claimSdcardRun(); err != nil {
		return err
	}

	go func() {
		_, err := m.runRefreshSDCardClaimed(context.Background())
		m.feedbackSdcardResult(err)
	}()
	return nil
}

// RunRefreshSDCard 同步刷新一次，供界面按钮做即时反馈（会阻塞到刷新结束）。
func (m *Manager) RunRefreshSDCard(ctx context.Context) (SdcardRefreshResult, error) {
	if err := m.claimSdcardRun(); err != nil {
		return SdcardRefreshResult{}, err
	}
	return m.runRefreshSDCardClaimed(ctx)
}

// claimSdcardRun 占一次位：已经在刷新中就拒绝，返回给调用方的错误文案保持不变。
func (m *Manager) claimSdcardRun() error {
	m.sdcardMu.Lock()
	defer m.sdcardMu.Unlock()
	if m.sdcardRun.running {
		return fmt.Errorf("上一次刷新SD卡还没结束，请稍候")
	}
	m.sdcardRun.running = true
	m.sdcardRun.startedAt = time.Now()
	return nil
}

// runRefreshSDCardClaimed 真正干活的那个：要求调用方已经用 claimSdcardRun
// 占好位（不管同步还是异步入口都只会占一次）。结束时统一复位 running 并写入
// lastSummary / lastError。
func (m *Manager) runRefreshSDCardClaimed(ctx context.Context) (SdcardRefreshResult, error) {
	started := time.Now()
	result, err := m.refreshSDCard(ctx)
	if result.ElapsedMS == 0 {
		result.ElapsedMS = time.Since(started).Milliseconds()
	}
	if err != nil {
		m.logf("刷新SD卡失败：%v", err)
	} else {
		m.logf("刷新SD卡：%s（耗时 %d ms）", result.Summary, result.ElapsedMS)
	}

	m.sdcardMu.Lock()
	m.sdcardRun.running = false
	if err != nil {
		m.sdcardRun.lastError = err.Error()
		m.sdcardRun.lastSummary = ""
	} else {
		m.sdcardRun.lastError = ""
		m.sdcardRun.lastSummary = result.Summary
	}
	m.sdcardMu.Unlock()
	return result, err
}

// SdcardRefreshStatus 返回最近一次刷新的状态，界面上显示结果用。
func (m *Manager) SdcardRefreshStatus() (running bool, summary, lastError string) {
	m.sdcardMu.Lock()
	defer m.sdcardMu.Unlock()
	return m.sdcardRun.running, m.sdcardRun.lastSummary, m.sdcardRun.lastError
}

// feedbackSdcardResult 用与备份一致的方式回执刷新结果：成功闪 3 下 + 滴滴滴 3 声，
// 失败只响一声长鸣，用户不用盯屏幕就知道有没有成功。
func (m *Manager) feedbackSdcardResult(err error) {
	pattern := gpioStandardFeedback()
	stage := "刷新SD卡完成"
	if err != nil {
		pattern = GPIOFeedbackPattern{Tones: 1}
		stage = "刷新SD卡失败"
	}
	_ = m.PlayGPIOFeedback(GPIOEvent{
		ButtonID: "network",
		Name:     "网络按键",
		Stage:    stage,
		Kind:     GPIOEventFeedback,
		Feedback: &pattern,
	})
}

// formatBlockCapacity 把块设备容量（512 字节扇区）排版成人看的大小。
// 跨平台：刷新SD卡的容量判据与存储插入提示的设备描述都要用它。
func formatBlockCapacity(sectors int64) string {
	const (
		gib = 1 << 30
		mib = 1 << 20
		kib = 1 << 10
	)
	bytes := sectors * 512
	switch {
	case bytes >= gib:
		return fmt.Sprintf("%.1f GiB", float64(bytes)/float64(gib))
	case bytes >= mib:
		return fmt.Sprintf("%.0f MiB", float64(bytes)/float64(mib))
	default:
		return fmt.Sprintf("%.0f KiB", float64(bytes)/float64(kib))
	}
}
