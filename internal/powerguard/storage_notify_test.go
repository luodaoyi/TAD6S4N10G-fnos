package powerguard

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// errStorageNotifyProbe 模拟一次失败的 /sys/block 采样。
var errStorageNotifyProbe = errors.New("模拟采样失败")

func storageNotifyTestManager(t *testing.T, storageNotify *bool) *Manager {
	t.Helper()
	root := t.TempDir()
	manager := &Manager{Root: root, ConfigPath: filepath.Join(root, "config.json")}
	config := DefaultConfig(Profile{})
	config.GPIO.StorageNotify = storageNotify
	if err := manager.saveConfigLocked(config); err != nil {
		t.Fatalf("准备配置失败: %v", err)
	}
	return manager
}

func testSnapshot(removable, internal map[string]int64) storageDeviceSnapshot {
	return storageDeviceSnapshot{removable: removable, internal: internal}
}

func TestNewBlockNamesFindsNewAndResizedDevices(t *testing.T) {
	previous := map[string]int64{"sda": 1000, "sdb": 2000}
	current := map[string]int64{"sda": 1000, "sdb": 4000, "sdc": 500}
	names := newBlockNames(previous, current)
	if len(names) != 2 || names[0] != "sdb" || names[1] != "sdc" {
		t.Fatalf("应报出换介质的 sdb 和新出现的 sdc，实际 %v", names)
	}
	if got := newBlockNames(current, map[string]int64{"sda": 1000}); len(got) != 0 {
		t.Fatalf("设备消失不该报成插入，实际 %v", got)
	}
}

func TestStorageChimePatterns(t *testing.T) {
	long := storageChimeTones(storageInsertRemovable)
	if len(long) != 1 || long[0].DurationMS <= storageChimeShortMS {
		t.Fatalf("移动存储应当长响一次，实际 %+v", long)
	}
	if got := storageChimeLabel(storageInsertRemovable); got != "长响一次" {
		t.Fatalf("移动存储提示音文案 = %q", got)
	}
	two := storageChimeTones(storageInsertInternal)
	if len(two) != 2 {
		t.Fatalf("内置存储应当连续响两声，实际 %+v", two)
	}
	if two[0].DurationMS >= long[0].DurationMS {
		t.Fatalf("两种节奏必须能听出区别：%+v vs %+v", two, long)
	}
	if got := storageChimeLabel(storageInsertInternal); got != "连续响两声" {
		t.Fatalf("内置存储提示音文案 = %q", got)
	}
	for _, tone := range append(append([]GPIOToneSpec(nil), long...), two...) {
		if tone.FrequencyHz <= 0 || tone.DurationMS <= 0 {
			t.Fatalf("提示音参数必须有效: %+v", tone)
		}
	}
}

// 首拍只建基线：开机时已经在的盘不该响，也不该被记成「插入」。
func TestApplyStorageSnapshotPrimesBaselineSilently(t *testing.T) {
	manager := storageNotifyTestManager(t, nil)
	manager.applyStorageSnapshot(testSnapshot(
		map[string]int64{"sdc": 2048},
		map[string]int64{"sda": 1000000},
	), nil)
	status := manager.StorageInsertNotifyStatus(true)
	if len(status.Recent) != 0 {
		t.Fatalf("首拍不该产生事件，实际 %+v", status.Recent)
	}
	if status.Devices != 2 || status.Removable != 1 || status.Internal != 1 {
		t.Fatalf("基线计数不对: %+v", status)
	}
}

func TestApplyStorageSnapshotRecordsInserts(t *testing.T) {
	manager := storageNotifyTestManager(t, nil)
	manager.applyStorageSnapshot(testSnapshot(
		map[string]int64{"sdc": 2048},
		map[string]int64{"sda": 1000000, "sdb": 2000000},
	), nil)
	// 插一块内置盘 + 读卡器认到卡，同时发生。
	manager.applyStorageSnapshot(testSnapshot(
		map[string]int64{"sdc": 2048, "sdd": 61000000},
		map[string]int64{"sda": 1000000, "sdb": 2000000, "sde": 4000000},
	), nil)

	status := manager.StorageInsertNotifyStatus(true)
	if len(status.Recent) != 2 {
		t.Fatalf("应记录两条事件，实际 %+v", status.Recent)
	}
	// 同一拍里两类都出现：按处理顺序先移动存储、后内置存储。
	if status.Recent[0].Kind != string(storageInsertRemovable) || status.Recent[0].Chime != "长响一次" {
		t.Fatalf("移动存储事件不对: %+v", status.Recent[0])
	}
	if status.Recent[0].KindLabel != "移动存储" {
		t.Fatalf("移动存储标签不对: %+v", status.Recent[0])
	}
	if status.Recent[1].Kind != string(storageInsertInternal) || status.Recent[1].KindLabel != "内置存储" {
		t.Fatalf("内置存储事件不对: %+v", status.Recent[1])
	}
	if status.Recent[1].Chime != "连续响两声" {
		t.Fatalf("内置存储提示音文案不对: %+v", status.Recent[1])
	}
	if len(status.Recent[1].Devices) != 1 || !strings.Contains(status.Recent[1].Devices[0], "sde") {
		t.Fatalf("事件应带上设备描述: %+v", status.Recent[1].Devices)
	}
	if status.Recent[0].DetectedAt.IsZero() {
		t.Fatal("事件应当带时间戳")
	}
}

// 关掉开关之后仍然记录事件（界面要看得到），只是不放音。
func TestApplyStorageSnapshotDisabledStillRecordsWithoutChime(t *testing.T) {
	disabled := false
	manager := storageNotifyTestManager(t, &disabled)
	manager.applyStorageSnapshot(testSnapshot(map[string]int64{}, map[string]int64{"sda": 1}), nil)
	manager.applyStorageSnapshot(testSnapshot(map[string]int64{"sdc": 999}, map[string]int64{"sda": 1}), nil)

	status := manager.StorageInsertNotifyStatus(false)
	if status.Enabled {
		t.Fatal("状态应当如实反映开关是关的")
	}
	if len(status.Recent) != 1 {
		t.Fatalf("关了开关也要记事件，实际 %+v", status.Recent)
	}
	if status.Recent[0].Notified {
		t.Fatal("开关关着时不该标记为已提示")
	}
}

// 采样失败不能把快照当成「设备全没了」：下次成功时会把所有盘当新增。
func TestApplyStorageSnapshotScanErrorKeepsBaseline(t *testing.T) {
	manager := storageNotifyTestManager(t, nil)
	baseline := testSnapshot(map[string]int64{}, map[string]int64{"sda": 1000})
	manager.applyStorageSnapshot(baseline, nil)
	manager.applyStorageSnapshot(storageDeviceSnapshot{}, errStorageNotifyProbe)
	if status := manager.StorageInsertNotifyStatus(true); status.LastError == "" {
		t.Fatal("采样失败应当记下原因")
	}
	manager.applyStorageSnapshot(baseline, nil)
	status := manager.StorageInsertNotifyStatus(true)
	if len(status.Recent) != 0 {
		t.Fatalf("采样恢复后不该把已在的盘当新增，实际 %+v", status.Recent)
	}
	if status.LastError != "" {
		t.Fatalf("采样恢复后应清掉错误，实际 %q", status.LastError)
	}
}

func TestApplyStorageSnapshotKeepsRecentBounded(t *testing.T) {
	manager := storageNotifyTestManager(t, nil)
	manager.applyStorageSnapshot(testSnapshot(map[string]int64{}, map[string]int64{}), nil)
	for i := 0; i < storageInsertMaxLog+3; i++ {
		manager.applyStorageSnapshot(testSnapshot(
			map[string]int64{},
			map[string]int64{"sdb": int64(i + 1)},
		), nil)
	}
	status := manager.StorageInsertNotifyStatus(true)
	if len(status.Recent) != storageInsertMaxLog {
		t.Fatalf("最近事件应被截断到 %d 条，实际 %d", storageInsertMaxLog, len(status.Recent))
	}
	if time.Since(status.Recent[0].DetectedAt) > time.Minute {
		t.Fatalf("最新事件的时间戳不对: %+v", status.Recent[0])
	}
}

func TestStorageNotifyEnabledDefaultsOnWhenConfigMissing(t *testing.T) {
	// 配置读不出来时按默认（开启）：这只是提示音，不该静默失效。
	manager := &Manager{Root: t.TempDir(), ConfigPath: filepath.Join(t.TempDir(), "missing.json")}
	if !manager.storageNotifyEnabled() {
		t.Fatal("缺少配置时应默认开启")
	}
	enabled := true
	if !storageNotifyTestManager(t, &enabled).storageNotifyEnabled() {
		t.Fatal("显式开启时应当报告开启")
	}
	disabled := false
	if storageNotifyTestManager(t, &disabled).storageNotifyEnabled() {
		t.Fatal("显式关闭时应当报告关闭")
	}
	if !(GPIOConfig{}).StorageNotifyEnabled() {
		t.Fatal("老配置没有该字段时应视为开启")
	}
}
