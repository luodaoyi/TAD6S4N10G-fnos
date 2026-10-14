package powerguard

// storage_notify.go：识别到新存储设备插入时用机箱蜂鸣器提示。
//
// 为什么值得做：硬盘托架插盘、插 U 盘 / 读卡器都是「人不在电脑前，但想知道有没有
// 认上」的场景。两类事件用不同节奏，闭着眼也能分辨：
//
//	移动存储（U 盘 / 读卡器 / 移动硬盘）→ 长响一次
//	内置存储（机箱里的 SATA / M.2 仓位）→ 连续响两声
//
// 判定「新插入」靠块设备快照的前后差分：只有新出现、或同名设备容量变化（换了介质）
// 才算，拔出不提示。首拍只建立基线——开机时已经在的盘不该响，更不该一次报一串。
// 平台相关的枚举与放音见 storage_notify_linux.go。

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type storageInsertKind string

const (
	storageInsertRemovable storageInsertKind = "removable"
	storageInsertInternal  storageInsertKind = "internal"

	// storageChimeHz 是提示音的频率：比按键确认反馈的 440 Hz 高一些，一听就知道
	// 不是按键动作；storageChimeLongMS / storageChimeShortMS 是两种事件的节奏。
	storageChimeHz      = 880
	storageChimeLongMS  = 800
	storageChimeShortMS = 220
	// storageInsertMaxLog 是界面保留的最近事件条数。
	storageInsertMaxLog = 8
)

// StorageInsertEvent 是一次「识别到新存储设备」的记录，直接下发到界面。
type StorageInsertEvent struct {
	Kind       string    `json:"kind"`
	KindLabel  string    `json:"kind_label"`
	Devices    []string  `json:"devices"`
	Chime      string    `json:"chime"`
	DetectedAt time.Time `json:"detected_at"`
	// Notified 表示这次的提示音确实响出来了（蜂鸣器不在时是 false）。
	Notified bool `json:"notified"`
}

// StorageNotifyStatus 是存储插入提示音的运行状态与最近事件。
type StorageNotifyStatus struct {
	Enabled bool `json:"enabled"`
	// Devices 是当前被监视的块设备数（只统计已读到介质的）。
	Devices   int                  `json:"devices"`
	Removable int                  `json:"removable"`
	Internal  int                  `json:"internal"`
	Recent    []StorageInsertEvent `json:"recent,omitempty"`
	LastError string               `json:"last_error,omitempty"`
}

type storageNotifyState struct {
	primed    bool
	previous  storageDeviceSnapshot
	recent    []StorageInsertEvent
	lastError string
}

// storageDeviceSnapshot 是某一时刻被监视的块设备：按「移动存储 / 内置存储」分开
// 记录设备名与容量（512 字节扇区）。容量用来发现「设备名没变但介质换了」。
type storageDeviceSnapshot struct {
	removable map[string]int64
	internal  map[string]int64
}

// MonitorStorageInserts 是存储插入监视的一拍：采样 → 差分 → 放提示音 → 记事件。
// 由后台循环按固定间隔调用（见 backend/powerguard/main.go 的 storageNotifyLoop）。
func (m *Manager) MonitorStorageInserts() {
	snapshot, scanErr := m.storageDeviceSnapshot()
	m.applyStorageSnapshot(snapshot, scanErr)
}

// applyStorageSnapshot 是采样之后的全部分支。拆出来是为了能直接喂快照做测试
// （假 /sys 根在 Windows 上起不来）。
func (m *Manager) applyStorageSnapshot(snapshot storageDeviceSnapshot, scanErr error) {
	// 先拿 enabled：它在 m.mu 里，锁序必须是 m.mu → storageNotifyMu。
	enabled := m.storageNotifyEnabled()

	m.storageNotifyMu.Lock()
	if scanErr != nil {
		// 采样失败时不要拿空快照去差分：那样会把「读不到」当成「设备全部消失」，
		// 等恢复正常时又把所有盘当成新插入，一次性乱响。
		m.storageNotify.lastError = scanErr.Error()
		m.storageNotifyMu.Unlock()
		return
	}
	m.storageNotify.lastError = ""
	if !m.storageNotify.primed {
		m.storageNotify.primed = true
		m.storageNotify.previous = snapshot
		m.storageNotifyMu.Unlock()
		return
	}
	previous := m.storageNotify.previous
	m.storageNotify.previous = snapshot
	removable := newBlockNames(previous.removable, snapshot.removable)
	internal := newBlockNames(previous.internal, snapshot.internal)
	m.storageNotifyMu.Unlock()

	type pendingInsert struct {
		kind    storageInsertKind
		sizes   map[string]int64
		devices []string
	}
	var pending []pendingInsert
	if len(removable) > 0 {
		pending = append(pending, pendingInsert{kind: storageInsertRemovable, sizes: snapshot.removable, devices: removable})
	}
	if len(internal) > 0 {
		pending = append(pending, pendingInsert{kind: storageInsertInternal, sizes: snapshot.internal, devices: internal})
	}
	if len(pending) == 0 {
		return
	}

	events := make([]StorageInsertEvent, 0, len(pending))
	for _, item := range pending {
		event := storageInsertEvent(item.kind, item.sizes, item.devices)
		if enabled {
			if chimeErr := m.PlayStorageChime(storageChimeTones(item.kind)); chimeErr != nil {
				// 提示音是尽力而为的：蜂鸣器不在也不该影响别的功能。
				m.logf("存储插入提示音失败：%v", chimeErr)
			} else {
				event.Notified = true
			}
		}
		m.logf("识别到新插入的%s：%s（%s）", event.KindLabel,
			strings.Join(event.Devices, "、"), event.Chime)
		events = append(events, event)
	}

	m.storageNotifyMu.Lock()
	m.storageNotify.recent = append(events, m.storageNotify.recent...)
	if len(m.storageNotify.recent) > storageInsertMaxLog {
		m.storageNotify.recent = m.storageNotify.recent[:storageInsertMaxLog]
	}
	m.storageNotifyMu.Unlock()
}

// StorageInsertNotifyStatus 返回存储插入提示音的状态。enabled 由调用方传入
// （Status() 手里已经有配置），这样本函数只需要自己的锁，不会与 m.mu 交叉。
func (m *Manager) StorageInsertNotifyStatus(enabled bool) StorageNotifyStatus {
	m.storageNotifyMu.Lock()
	defer m.storageNotifyMu.Unlock()
	status := StorageNotifyStatus{
		Enabled:   enabled,
		Removable: len(m.storageNotify.previous.removable),
		Internal:  len(m.storageNotify.previous.internal),
		Recent:    append([]StorageInsertEvent(nil), m.storageNotify.recent...),
		LastError: m.storageNotify.lastError,
	}
	status.Devices = status.Removable + status.Internal
	return status
}

// storageNotifyEnabled 读取配置里的开关。配置读不出来时按默认（开启）处理——
// 这只是个提示音，不该因为读不到配置就静默失效。
func (m *Manager) storageNotifyEnabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.loadConfigLocked()
	if err != nil {
		return true
	}
	return cfg.GPIO.StorageNotifyEnabled()
}

// newBlockNames 返回新出现或容量发生变化的块设备名。容量变了说明同一个设备名
// 底下的介质被换掉了（读卡器换卡、托架换盘），同样算一次插入。
func newBlockNames(previous, current map[string]int64) []string {
	var names []string
	for name, size := range current {
		if old, existed := previous[name]; !existed || old != size {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// storageChimeTones 给出两类事件的提示音编排：移动存储长响一次、内置存储连续响两声。
func storageChimeTones(kind storageInsertKind) []GPIOToneSpec {
	if kind == storageInsertInternal {
		return []GPIOToneSpec{
			{FrequencyHz: storageChimeHz, DurationMS: storageChimeShortMS},
			{FrequencyHz: storageChimeHz, DurationMS: storageChimeShortMS},
		}
	}
	return []GPIOToneSpec{{FrequencyHz: storageChimeHz, DurationMS: storageChimeLongMS}}
}

func storageChimeLabel(kind storageInsertKind) string {
	if kind == storageInsertInternal {
		return "连续响两声"
	}
	return "长响一次"
}

func storageInsertKindLabel(kind storageInsertKind) string {
	if kind == storageInsertInternal {
		return "内置存储"
	}
	return "移动存储"
}

func storageInsertEvent(kind storageInsertKind, sizes map[string]int64, names []string) StorageInsertEvent {
	devices := make([]string, 0, len(names))
	for _, name := range names {
		devices = append(devices, fmt.Sprintf("%s（%s）", name, formatBlockCapacity(sizes[name])))
	}
	return StorageInsertEvent{
		Kind:       string(kind),
		KindLabel:  storageInsertKindLabel(kind),
		Devices:    devices,
		Chime:      storageChimeLabel(kind),
		DetectedAt: time.Now(),
	}
}
