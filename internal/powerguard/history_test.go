package powerguard

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func historyTestStatus() Status {
	return Status{
		CPUTemperature: CPUTemperatureStatus{Available: true, DisplayC: 55.5},
		Storage: StorageStatus{Slots: []StorageSlot{
			{ID: "front-1", Kind: "front", TemperatureC: 41.5},
			{ID: "front-2", Kind: "front", TemperatureC: 39.0},
			{ID: "m2-1", Kind: "m2", TemperatureC: 48.0},
			{ID: "m2-2", Kind: "m2"},
			{ID: "front-3", Kind: "front", State: StorageEmpty},
		}},
		FanControl: FanControlStatus{Fans: []FanStatus{
			{ID: "fan", RPM: 1320, PWMPercent: 60},
			{ID: "dead", RPM: 0, PWMPercent: 0},
		}},
	}
}

func TestSampleFromStatusTakesHottestSlotPerKind(t *testing.T) {
	now := time.Unix(1700000000, 0)
	sample := SampleFromStatus(statusPtr(historyTestStatus()), now)
	if sample.TS != now.Unix() {
		t.Fatalf("ts: got %d, want %d", sample.TS, now.Unix())
	}
	if sample.CPUC != 55.5 {
		t.Fatalf("cpu: got %v, want 55.5", sample.CPUC)
	}
	if sample.HDDC != 41.5 {
		t.Fatalf("hdd: got %v, want 41.5 (hottest front slot)", sample.HDDC)
	}
	if sample.NVMeC != 48.0 {
		t.Fatalf("nvme: got %v, want 48", sample.NVMeC)
	}
	if len(sample.Fans) != 1 || sample.Fans[0].ID != "fan" || sample.Fans[0].RPM != 1320 || sample.Fans[0].PWMPercent != 60 {
		t.Fatalf("fans: dead channel should be excluded, got %+v", sample.Fans)
	}
}

func TestSampleFromStatusNilStatusKeepsTimestamp(t *testing.T) {
	now := time.Unix(1700000001, 0)
	sample := SampleFromStatus(nil, now)
	if sample.TS != now.Unix() || sample.CPUC != 0 || sample.HDDC != 0 || sample.NVMeC != 0 || len(sample.Fans) != 0 {
		t.Fatalf("empty sample: got %+v", sample)
	}
}

func TestSampleFromStatusWithoutCPUAvailability(t *testing.T) {
	st := historyTestStatus()
	st.CPUTemperature.Available = false
	sample := SampleFromStatus(&st, time.Now())
	if sample.CPUC != 0 {
		t.Fatalf("cpu should stay zero when unavailable, got %v", sample.CPUC)
	}
}

func TestSampleFromStatusClassifiesExtraSensors(t *testing.T) {
	st := historyTestStatus()
	st.ExtraTemperatures = []Temperature{
		{Label: "igc:PHY", Celsius: 62},
		{Label: "i915:temp1", Celsius: 36},
		{Label: "acpitz:temp1", Celsius: 27.8},
		{Label: "spd5118:temp1", Celsius: 35.5},
	}
	sample := SampleFromStatus(statusPtr(st), time.Unix(1700000002, 0))
	groups := map[string]string{}
	for _, sensor := range sample.Sensors {
		if sensor.Group == "cpu" {
			continue
		}
		groups[sensor.Key] = sensor.Group
	}
	if groups["igc:PHY"] != "nic" {
		t.Fatalf("igc should be nic, got %q", groups["igc:PHY"])
	}
	if groups["i915:temp1"] != "gpu" {
		t.Fatalf("i915 should be gpu, got %q", groups["i915:temp1"])
	}
	if groups["acpitz:temp1"] != "other" {
		t.Fatalf("acpitz should be other, got %q", groups["acpitz:temp1"])
	}
	if groups["spd5118:temp1"] != "other" {
		t.Fatalf("spd5118 should be other, got %q", groups["spd5118:temp1"])
	}
}

func statusPtr(st Status) *Status {
	return &st
}

func newTestStore(t *testing.T) *HistoryStore {
	t.Helper()
	store, err := NewHistoryStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestHistoryStoreAppendPersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	store, err := NewHistoryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-2 * time.Minute)
	want := []HistorySample{
		{TS: base.Unix(), CPUC: 50, HDDC: 40, NVMeC: 45, Fans: []HistoryFanSample{{ID: "fan", RPM: 1200, PWMPercent: 50}}},
		{TS: base.Add(time.Minute).Unix(), CPUC: 51},
	}
	for _, sample := range want {
		if err := store.Append(sample); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	store.Close()

	reloaded, err := NewHistoryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	samples, interval, err := reloaded.Aggregated(context.Background(), reloaded.maxRangeHours(), 2000, base.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if interval != 60 {
		t.Fatalf("interval: got %d, want 60", interval)
	}
	if len(samples) != len(want) {
		t.Fatalf("reloaded len: got %d, want %d", len(samples), len(want))
	}
	for i := range want {
		if samples[i].TS != want[i].TS || samples[i].CPUC != want[i].CPUC || samples[i].HDDC != want[i].HDDC || samples[i].NVMeC != want[i].NVMeC {
			t.Fatalf("sample %d: got %+v, want %+v", i, samples[i], want[i])
		}
	}
	if len(samples[0].Fans) != 1 || samples[0].Fans[0].RPM != 1200 || samples[0].Fans[0].PWMPercent != 50 {
		t.Fatalf("fans: got %+v", samples[0].Fans)
	}
}

func TestHistoryStoreUpsertOverwritesSameTimestamp(t *testing.T) {
	store := newTestStore(t)
	ts := time.Unix(1700000000, 0)
	if err := store.Append(HistorySample{TS: ts.Unix(), CPUC: 50, Fans: []HistoryFanSample{{ID: "fan", RPM: 1000, PWMPercent: 40}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(HistorySample{TS: ts.Unix(), CPUC: 52, Fans: []HistoryFanSample{{ID: "fan", RPM: 1400, PWMPercent: 62}}}); err != nil {
		t.Fatal(err)
	}
	samples, _, err := store.Aggregated(context.Background(), store.maxRangeHours(), 2000, ts.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].CPUC != 52 {
		t.Fatalf("upsert should keep one row with new values: %+v", samples)
	}
	if len(samples[0].Fans) != 1 || samples[0].Fans[0].RPM != 1400 || samples[0].Fans[0].PWMPercent != 62 {
		t.Fatalf("fan rows should be replaced: %+v", samples[0].Fans)
	}
}

func TestHistoryAggregatedPeaksAndStride(t *testing.T) {
	store := newTestStore(t)
	now := time.Unix(1700000000, 0)
	// 600 个点、每分钟一个，其中插一个 99 度尖峰 + 风扇峰值
	for i := 600; i >= 1; i-- {
		sample := HistorySample{TS: now.Unix() - int64(i*60), CPUC: 50}
		if i == 300 {
			sample.CPUC = 99
			sample.Fans = []HistoryFanSample{{ID: "fan", RPM: 2000, PWMPercent: 90}}
		}
		if err := store.Append(sample); err != nil {
			t.Fatal(err)
		}
	}
	samples, interval, err := store.Aggregated(context.Background(), 10, 100, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) > 100 {
		t.Fatalf("aggregated len %d exceeds maxPoints 100", len(samples))
	}
	if interval <= 60 {
		t.Fatalf("aggregated interval should exceed base interval, got %d", interval)
	}
	peak := 0.0
	fanPWM := 0
	for _, sample := range samples {
		if sample.CPUC > peak {
			peak = sample.CPUC
		}
		for _, fan := range sample.Fans {
			if fan.PWMPercent > fanPWM {
				fanPWM = fan.PWMPercent
			}
		}
	}
	if peak != 99 {
		t.Fatalf("peak must survive aggregation, got %v", peak)
	}
	if fanPWM != 90 {
		t.Fatalf("fan peak must survive aggregation, got %d", fanPWM)
	}
}

func TestHistoryAggregatedRangeFilter(t *testing.T) {
	store := newTestStore(t)
	now := time.Unix(1700000000, 0)
	// 一条 2 小时前的旧数据 + 一条 5 分钟前的新数据
	if err := store.Append(HistorySample{TS: now.Unix() - 7200, CPUC: 30}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(HistorySample{TS: now.Unix() - 300, CPUC: 55}); err != nil {
		t.Fatal(err)
	}
	samples, _, err := store.Aggregated(context.Background(), 1, 2000, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].CPUC != 55 {
		t.Fatalf("range query should only return recent sample: %+v", samples)
	}
}

func TestHistoryPruneRemovesExpiredRows(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	if err := store.Append(HistorySample{TS: now.Add(-40 * 24 * time.Hour).Unix(), CPUC: 30}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(HistorySample{TS: now.Unix(), CPUC: 55}); err != nil {
		t.Fatal(err)
	}
	if err := store.Prune(now); err != nil {
		t.Fatal(err)
	}
	samples, _, err := store.Aggregated(context.Background(), store.maxRangeHours(), 2000, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].CPUC != 55 {
		t.Fatalf("expired rows should be pruned: %+v", samples)
	}
}

func TestHistoryPruneKeepsTwoDayBufferBeyondDisplayWindow(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	// 展示窗口 30 天之外、存储 32 天之内：31 天前的数据必须保留
	if err := store.Append(HistorySample{TS: now.Add(-31 * 24 * time.Hour).Unix(), CPUC: 31}); err != nil {
		t.Fatal(err)
	}
	// 超过存储 32 天：轮转删除
	if err := store.Append(HistorySample{TS: now.Add(-33 * 24 * time.Hour).Unix(), CPUC: 33}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(HistorySample{TS: now.Add(-29 * 24 * time.Hour).Unix(), CPUC: 29}); err != nil {
		t.Fatal(err)
	}
	if err := store.Prune(now); err != nil {
		t.Fatal(err)
	}
	// 展示窗口 30 天：Aggregated 只能看到 29d，31d 落在窗口外但仍在库中
	samples, _, err := store.Aggregated(context.Background(), store.maxRangeHours(), 2000, now)
	if err != nil {
		t.Fatal(err)
	}
	visible := make([]float64, 0, len(samples))
	for _, sample := range samples {
		if sample.CPUC > 0 {
			visible = append(visible, sample.CPUC)
		}
	}
	if len(visible) != 1 || visible[0] != 29 {
		t.Fatalf("display window should only see the 29d sample, got: %v", visible)
	}
	// 直接查库：31d 必须保留（2 天缓冲），33d 必须已轮转删除
	var surviving int64
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM history WHERE cpu_c = 31`).Scan(&surviving); err != nil {
		t.Fatal(err)
	}
	if surviving != 1 {
		t.Fatalf("31d sample must survive within 32-day storage buffer, got %d rows", surviving)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM history WHERE cpu_c = 33`).Scan(&surviving); err != nil {
		t.Fatal(err)
	}
	if surviving != 0 {
		t.Fatalf("33d sample must rotate beyond 32-day retention, got %d rows", surviving)
	}
}

func TestHistoryAggregatedClampsBadParams(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	// 30 分钟前（0.5h 下限内）与 2 小时前（下限外）各一个点
	if err := store.Append(HistorySample{TS: now.Unix() - 7200, CPUC: 20}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(HistorySample{TS: now.Unix() - 600, CPUC: 50}); err != nil {
		t.Fatal(err)
	}
	samples, _, err := store.Aggregated(context.Background(), 0, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if samples == nil {
		t.Fatal("aggregated should never return nil")
	}
	if len(samples) != 1 || samples[0].CPUC != 50 {
		t.Fatalf("range=0 should clamp to 0.5h and exclude 2h-old sample: %+v", samples)
	}
}

func TestHistoryAggregatedMultipleFansPerIDPeaks(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	// 12 个点、两个常驻风扇 + 一个只在偶数点出现的风扇，聚合后各自保留峰值
	for i := 12; i >= 1; i-- {
		sample := HistorySample{TS: now.Unix() - int64(i*60)}
		sample.Fans = []HistoryFanSample{
			{ID: "fan0", RPM: int64(1000 + i), PWMPercent: 40 + i},
			{ID: "fan1", RPM: int64(2000 - i), PWMPercent: 60 - i},
		}
		if i%2 == 0 {
			sample.Fans = append(sample.Fans, HistoryFanSample{ID: "fan2", RPM: 3000 + int64(i), PWMPercent: 80})
		}
		if err := store.Append(sample); err != nil {
			t.Fatal(err)
		}
	}
	samples, _, err := store.Aggregated(context.Background(), 1, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) == 0 {
		t.Fatal("no samples returned")
	}
	fans := map[string]HistoryFanSample{}
	for _, fan := range samples[len(samples)-1].Fans {
		fans[fan.ID] = fan
	}
	if len(fans) != 3 {
		t.Fatalf("expect 3 fan ids, got %d: %+v", len(fans), samples[len(samples)-1].Fans)
	}
	// 最后一个桶由 i=2（有 fan2）与 i=1 两个采样聚合而成
	if fans["fan0"].RPM != 1002 || fans["fan0"].PWMPercent != 42 {
		t.Fatalf("fan0 peak wrong: %+v", fans["fan0"])
	}
	if fans["fan1"].RPM != 1999 || fans["fan1"].PWMPercent != 59 {
		t.Fatalf("fan1 peak wrong: %+v", fans["fan1"])
	}
	if fans["fan2"].RPM != 3002 || fans["fan2"].PWMPercent != 80 {
		t.Fatalf("fan2 peak wrong: %+v", fans["fan2"])
	}
}

func TestHistoryStorePerDiskTemperaturesPersistAndAggregate(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	// 12 个点：front-2 温度爬升，front-1 恒定；聚合后保留各盘峰值
	for i := 12; i >= 1; i-- {
		sample := HistorySample{TS: now.Unix() - int64(i*60), CPUC: 50}
		sample.Disks = []HistoryDiskSample{
			{ID: "front-2", TemperatureC: float64(40 + i)},
			{ID: "front-1", TemperatureC: 39},
		}
		if err := store.Append(sample); err != nil {
			t.Fatal(err)
		}
	}
	samples, _, err := store.Aggregated(context.Background(), 1, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	last := samples[len(samples)-1]
	if len(last.Disks) != 2 {
		t.Fatalf("expect 2 disks, got %+v", last.Disks)
	}
	byID := map[string]float64{}
	for _, disk := range last.Disks {
		byID[disk.ID] = disk.TemperatureC
	}
	if byID["front-2"] != 42 {
		t.Fatalf("front-2 peak should be 42, got %v", byID["front-2"])
	}
	if byID["front-1"] != 39 {
		t.Fatalf("front-1 should stay 39, got %v", byID["front-1"])
	}

	// 换个库重开验证落盘
	path := filepath.Join(t.TempDir(), "history.db")
	persist, err := NewHistoryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	sample := HistorySample{TS: now.Unix(), CPUC: 50, Disks: []HistoryDiskSample{{ID: "m2-1", TemperatureC: 47.5}}}
	if err := persist.Append(sample); err != nil {
		t.Fatal(err)
	}
	persist.Close()
	reloaded, err := NewHistoryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	reloadedSamples, _, err := reloaded.Aggregated(context.Background(), 1, 2000, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(reloadedSamples) != 1 || len(reloadedSamples[0].Disks) != 1 || reloadedSamples[0].Disks[0].ID != "m2-1" || reloadedSamples[0].Disks[0].TemperatureC != 47.5 {
		t.Fatalf("per-disk reload mismatch: %+v", reloadedSamples)
	}
}

func TestHistoryAggregatedHalfHourRange(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	// 31 个点覆盖 30 分钟；更早放一个 1 小时前的点
	if err := store.Append(HistorySample{TS: now.Unix() - 3600, CPUC: 20}); err != nil {
		t.Fatal(err)
	}
	for i := 30; i >= 0; i-- {
		if err := store.Append(HistorySample{TS: now.Unix() - int64(i*60), CPUC: 50}); err != nil {
			t.Fatal(err)
		}
	}
	samples, interval, err := store.Aggregated(context.Background(), 0.5, 2000, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 31 {
		t.Fatalf("half-hour range should return 31 samples, got %d", len(samples))
	}
	for _, sample := range samples {
		if sample.TS < now.Unix()-1800-60 {
			t.Fatalf("sample older than 30min returned: %d", sample.TS)
		}
	}
	if interval != 60 {
		t.Fatalf("interval: got %d, want 60", interval)
	}
}

func TestHistoryExportSQLiteSnapshot(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	if err := store.Append(HistorySample{TS: now.Unix(), CPUC: 52, Fans: []HistoryFanSample{{ID: "fan3", RPM: 1200, PWMPercent: 45}}, Disks: []HistoryDiskSample{{ID: "front-2", TemperatureC: 41}}, Sensors: []HistorySensorSample{{Group: "other", Key: "acpitz:temp1", C: 28}}}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := store.ExportSQLite(context.Background(), &buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("export produced empty output")
	}
	// 快照应能作为独立数据库重新打开并读出数据
	snapPath := filepath.Join(t.TempDir(), "snap.db")
	if err := os.WriteFile(snapPath, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := NewHistoryStore(snapPath)
	if err != nil {
		t.Fatalf("reopen snapshot: %v", err)
	}
	defer snap.Close()
	samples, _, err := snap.Aggregated(context.Background(), snap.maxRangeHours(), 2000, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].CPUC != 52 || len(samples[0].Fans) != 1 || len(samples[0].Disks) != 1 || len(samples[0].Sensors) != 1 {
		t.Fatalf("snapshot data mismatch: %+v", samples)
	}
}

func TestHistoryExportCSVWideTable(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	if err := store.Append(HistorySample{
		TS: now.Unix(), CPUC: 52.5, HDDC: 41, NVMeC: 48,
		Fans:    []HistoryFanSample{{ID: "it87:fan3", RPM: 1200, PWMPercent: 45}},
		Disks:   []HistoryDiskSample{{ID: "front-2", TemperatureC: 41.2}},
		Sensors: []HistorySensorSample{{Group: "cpu", Key: "Core 0", C: 52.5}, {Group: "other", Key: "acpitz:temp1", C: 28}},
	}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := store.WriteCSV(context.Background(), &buf); err != nil {
		t.Fatalf("csv: %v", err)
	}
	content := buf.String()
	if !strings.HasPrefix(content, "\xef\xbb\xbfts,time,cpu_c,hdd_c,nvme_c") {
		t.Fatalf("csv should start with BOM and fixed header, got: %q", content[:60])
	}
	for _, want := range []string{"fan_it87:fan3_rpm", "fan_it87:fan3_pwm", "disk_front-2_c", "sensor_cpu_Core 0_c", "sensor_other_acpitz:temp1_c", "1200", "45", "41.2", "52.5"} {
		if !strings.Contains(content, want) {
			t.Fatalf("csv missing %q:\n%s", want, content)
		}
	}
	// 第二个采样缺风扇数据时对应列应为空
	if err := store.Append(HistorySample{TS: now.Add(time.Minute).Unix(), CPUC: 53}); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := store.WriteCSV(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	headerFields := strings.Split(strings.Split(buf.String(), "\n")[0], ",")
	fanRpmIdx := -1
	for i, name := range headerFields {
		if strings.HasSuffix(name, "_rpm") {
			fanRpmIdx = i
			break
		}
	}
	if fanRpmIdx < 0 {
		t.Fatalf("header missing fan rpm column: %v", headerFields)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expect header + 2 rows, got %d", len(lines))
	}
	lastFields := strings.Split(lines[2], ",")
	if lastFields[fanRpmIdx] != "" || lastFields[fanRpmIdx+1] != "" {
		t.Fatalf("second sample has no fan data, fan columns should be empty: %v", lastFields)
	}
}

func TestSaveSensorSettingsPersistsAndCleans(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "proc", "cpuinfo"), []byte("model name : Intel(R) Processor N100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: dir, ConfigPath: filepath.Join(dir, "config.json"), StatePath: filepath.Join(dir, "state.json"), Version: "test"}
	if _, err := manager.LoadOrCreateConfig(); err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveSensorSettings(
		map[string]string{
			"it8613:temp1":          "主板温度",
			"  acpitz:temp1  ":      "  ACPI 温区 ",
			"igc:PHY":               "",
			strings.Repeat("x", 81): "过长键应被丢弃",
		},
		map[string]string{
			"  mlx5:temp1 ":         " nic ",
			"i915:temp1":            "other",
			"acpitz:temp1":          "cpu",   // 非法父类：丢弃
			"it8613:temp1":          "bogus", // 非法父类：丢弃
			"":                      "nic",   // 空键：丢弃
			strings.Repeat("y", 81): "nic",   // 过长键：丢弃
		}); err != nil {
		t.Fatal(err)
	}
	cfg, err := manager.LoadOrCreateConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SensorNames["it8613:temp1"] != "主板温度" {
		t.Fatalf("normal name missing: %+v", cfg.SensorNames)
	}
	if cfg.SensorNames["acpitz:temp1"] != "ACPI 温区" {
		t.Fatalf("key/value should be trimmed: %+v", cfg.SensorNames)
	}
	if _, exists := cfg.SensorNames["igc:PHY"]; exists {
		t.Fatalf("empty value should be dropped: %+v", cfg.SensorNames)
	}
	if _, exists := cfg.SensorNames[strings.Repeat("x", 81)]; exists {
		t.Fatalf("oversized key should be dropped: %+v", cfg.SensorNames)
	}
	if cfg.SensorGroups["mlx5:temp1"] != "nic" || cfg.SensorGroups["i915:temp1"] != "other" {
		t.Fatalf("valid groups missing: %+v", cfg.SensorGroups)
	}
	if len(cfg.SensorGroups) != 2 {
		t.Fatalf("invalid/empty/oversized groups should be dropped: %+v", cfg.SensorGroups)
	}
	// 清空场景：传空 map 应清掉已有名称与归属
	if err := manager.SaveSensorSettings(map[string]string{}, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	cfg, err = manager.LoadOrCreateConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.SensorNames) != 0 || len(cfg.SensorGroups) != 0 {
		t.Fatalf("empty save should clear names and groups: %+v %+v", cfg.SensorNames, cfg.SensorGroups)
	}
}

func TestHistorySizeLimitPrunesOldestDays(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	base := now.Add(-6 * 24 * time.Hour)
	// 6 天逐小时采样，每个点带风扇/盘位/传感器行，撑起可观测的库体积
	for i := 0; i <= 144; i++ {
		sample := HistorySample{
			TS:      base.Add(time.Duration(i) * time.Hour).Unix(),
			CPUC:    40 + float64(i%10),
			Fans:    []HistoryFanSample{{ID: "it8613:it87.2608:fan2", RPM: 900, PWMPercent: 40}},
			Disks:   []HistoryDiskSample{{ID: "front-2", TemperatureC: 41.5}},
			Sensors: []HistorySensorSample{{Group: "cpu", Key: "Core 0", C: 45}},
		}
		if err := store.Append(sample); err != nil {
			t.Fatal(err)
		}
	}
	newest := base.Add(144 * time.Hour).Unix()
	before := store.dbSizeBytes()
	if before <= 1<<20 {
		t.Fatalf("test dataset should exceed the 1MB limit, got %d bytes", before)
	}
	if err := store.PruneIfNeeded(now, HistoryConfig{MaxSizeMB: 1, RetentionDays: historyDefaultRetentionDays}); err != nil { // 1MB 上限：MB→字节换算 + 按天删最旧
		t.Fatal(err)
	}
	if store.dbSizeBytes() > 1<<20 {
		t.Fatalf("size limit not enforced: %d bytes", store.dbSizeBytes())
	}
	samples, _, err := store.Aggregated(context.Background(), store.maxRangeHours(), 2000, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) == 0 || samples[len(samples)-1].TS != newest {
		t.Fatalf("newest sample must survive size pruning: %+v", samples)
	}
	if samples[0].TS <= base.Unix() {
		t.Fatalf("pruning should remove from the oldest side: kept ts=%d", samples[0].TS)
	}
	// 0 表示不启用大小限制，此时不得报错也不得继续删除
	kept := len(samples)
	if err := store.PruneIfNeeded(now, HistoryConfig{MaxSizeMB: 0, RetentionDays: historyDefaultRetentionDays}); err != nil {
		t.Fatal(err)
	}
	samples, _, _ = store.Aggregated(context.Background(), store.maxRangeHours(), 2000, now)
	if len(samples) != kept {
		t.Fatalf("disabled size limit must not prune: before=%d after=%d", kept, len(samples))
	}
}

func TestHistoryLoopSkipsAppendWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	procDir := filepath.Join(dir, "proc")
	if err := os.MkdirAll(procDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procDir, "cpuinfo"), []byte("model name : Intel(R) Processor N100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	statePath := filepath.Join(dir, "state.json")
	manager := &Manager{Root: dir, ConfigPath: configPath, StatePath: statePath}
	if _, err := manager.LoadOrCreateConfig(); err != nil {
		t.Fatalf("config setup: %v", err)
	}
	if !manager.HistorySettings().Enabled {
		t.Fatal("history should default to enabled")
	}
	if err := manager.SaveHistoryConfig(HistoryConfig{Enabled: false, MaxSizeMB: 64}); err != nil {
		t.Fatal(err)
	}
	if manager.HistorySettings().Enabled {
		t.Fatal("history should be disabled after save")
	}
	store := newTestStore(t)
	logger := log.New(os.Stderr, "", 0)
	for i := 0; i < 3; i++ {
		store.appendAndLog(context.Background(), manager, logger)
	}
	samples, _, err := store.Aggregated(context.Background(), store.maxRangeHours(), 2000, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 0 {
		t.Fatalf("disabled history must not write samples: %+v", samples)
	}
	// 重新启用后恢复写入
	if err := manager.SaveHistoryConfig(HistoryConfig{Enabled: true, MaxSizeMB: 64}); err != nil {
		t.Fatal(err)
	}
	store.appendAndLog(context.Background(), manager, logger)
	samples, _, err = store.Aggregated(context.Background(), store.maxRangeHours(), 2000, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 {
		t.Fatalf("re-enabled history should append again: %+v", samples)
	}
}

// 同名芯片（多 NVMe/多 mlx5）在 extraTemperatures 里带 #N 后缀，
// 分组识别必须剥掉后缀再查驱动表（mlx5 归「网卡」组）。
func TestSampleFromStatusGroupsNumberedDuplicateChips(t *testing.T) {
	status := &Status{ExtraTemperatures: []Temperature{
		{Label: "i915:temp1", Celsius: 42},
		{Label: "i915#2:temp1", Celsius: 41},
		{Label: "mlx5:temp1", Celsius: 64},
		{Label: "mlx5#2:temp1", Celsius: 63},
	}}
	sample := SampleFromStatus(status, time.Unix(1000, 0))
	groups := map[string]string{}
	for _, sensor := range sample.Sensors {
		groups[sensor.Key] = sensor.Group
	}
	want := map[string]string{
		"i915:temp1": "gpu", "i915#2:temp1": "gpu",
		"mlx5:temp1": "nic", "mlx5#2:temp1": "nic",
	}
	if !reflect.DeepEqual(groups, want) {
		t.Fatalf("groups=%v, want %v", groups, want)
	}
}

// ReclassifySensorGroups 按当前规则重写父类并给出默认映射：
// 旧库中 mlx5 存的 "other" 要整体迁到 "nic"，驱动表调整不断裂历史曲线。
func TestReclassifySensorGroups(t *testing.T) {
	samples := []HistorySample{{
		Sensors: []HistorySensorSample{
			{Group: "other", Key: "mlx5:temp1", C: 64},   // 旧规则写入
			{Group: "other", Key: "mlx5#2:temp1", C: 63}, // 旧规则写入
			{Group: "cpu", Key: "Core 0", C: 45},
			{Group: "other", Key: "i915#2:temp1", C: 42},
			{Group: "other", Key: "acpitz:temp1", C: 28},
		},
	}}
	defaults := ReclassifySensorGroups(samples)
	want := []HistorySensorSample{
		{Group: "nic", Key: "mlx5:temp1", C: 64},
		{Group: "nic", Key: "mlx5#2:temp1", C: 63},
		{Group: "cpu", Key: "Core 0", C: 45},
		{Group: "gpu", Key: "i915#2:temp1", C: 42},
		{Group: "other", Key: "acpitz:temp1", C: 28},
	}
	if !reflect.DeepEqual(samples[0].Sensors, want) {
		t.Fatalf("sensors=%+v, want %+v", samples[0].Sensors, want)
	}
	wantDefaults := map[string]string{
		"mlx5:temp1": "nic", "mlx5#2:temp1": "nic", "Core 0": "cpu",
		"i915#2:temp1": "gpu", "acpitz:temp1": "other",
	}
	if !reflect.DeepEqual(defaults, wantDefaults) {
		t.Fatalf("defaults=%v, want %v", defaults, wantDefaults)
	}
}

// ApplySensorGroupOverrides 在查询结果上整体改写父类归属：
// 数据库保持默认分组，改回覆盖配置即恢复。
func TestApplySensorGroupOverrides(t *testing.T) {
	samples := []HistorySample{{
		Sensors: []HistorySensorSample{
			{Group: "other", Key: "mlx5:temp1", C: 64},
			{Group: "cpu", Key: "Core 0", C: 45},
			{Group: "gpu", Key: "i915:temp1", C: 42},
		},
	}}
	ApplySensorGroupOverrides(samples, map[string]string{"mlx5:temp1": "nic", "i915:temp1": "other", "Core 0": "nic"})
	want := []HistorySensorSample{
		{Group: "nic", Key: "mlx5:temp1", C: 64},
		{Group: "nic", Key: "Core 0", C: 45},
		{Group: "other", Key: "i915:temp1", C: 42},
	}
	if !reflect.DeepEqual(samples[0].Sensors, want) {
		t.Fatalf("sensors=%+v, want %+v", samples[0].Sensors, want)
	}
	ApplySensorGroupOverrides(samples, nil)
	if samples[0].Sensors[0].Group != "nic" {
		t.Fatalf("nil overrides must not touch samples: %+v", samples[0].Sensors)
	}
}

// history_sensors 主键是 (ts, grp, key)：即使上游仍产出重复键
// （旧数据回放、同名芯片），Append 也不得让整个采样点回滚丢失。
func TestAppendToleratesDuplicateSensorKeys(t *testing.T) {
	store, err := NewHistoryStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sample := HistorySample{
		TS: time.Now().Unix(), CPUC: 50,
		Sensors: []HistorySensorSample{
			{Group: "other", Key: "nvme:temp1", C: 39.85},
			{Group: "other", Key: "nvme:temp1", C: 43.85},
		},
	}
	if err := store.Append(sample); err != nil {
		t.Fatalf("append duplicate sensor keys: %v", err)
	}
	samples, _, err := store.Aggregated(context.Background(), 1, 480, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].CPUC != 50 {
		t.Fatalf("sample lost after duplicate-key append: %+v", samples)
	}
}

// ---- 保留期（retention_days）可配置 ----

func TestClampHistoryRetentionDays(t *testing.T) {
	tests := []struct {
		in, want int
	}{
		{-3, historyMinRetentionDays},
		{0, historyMinRetentionDays},
		{1, 1},
		{30, 30},
		{90, 90},
		{91, 91}, // 不设产品上限：91 天起原样保留
		{1000, 1000},
		{36500, 36500},
		{40000, historyMaxRetentionDays},   // 防溢出护栏：荒谬大值钳到 100 年
		{1 << 30, historyMaxRetentionDays}, // 溢出量级：必须钳住，否则 cutoff 变负清空数据
	}
	for _, test := range tests {
		if got := ClampHistoryRetentionDays(test.in); got != test.want {
			t.Fatalf("ClampHistoryRetentionDays(%d)=%d, want %d", test.in, got, test.want)
		}
	}
}

// SyncSettings 立即按“配置天数 + 2 天缓冲”清理，并把展示窗口上限
// 收紧为 配置天数×24（不含缓冲）。
func TestSyncSettingsPrunesImmediatelyAndShrinksWindow(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	appendCPUC := func(age time.Duration, value float64) {
		t.Helper()
		if err := store.Append(HistorySample{TS: now.Add(-age).Unix(), CPUC: value}); err != nil {
			t.Fatal(err)
		}
	}
	appendCPUC(5*24*time.Hour, 5) // 超过 2+2 天：必须删除
	appendCPUC(84*time.Hour, 35)  // 3.5 天：在存储（4 天）内必须保留，但在窗口（2 天）外
	appendCPUC(time.Hour, 51)     // 窗口内
	if err := store.SyncSettings(HistoryConfig{RetentionDays: 2}); err != nil {
		t.Fatal(err)
	}
	if store.retentionDays != 2 {
		t.Fatalf("retentionDays=%d, want 2", store.retentionDays)
	}
	var surviving int64
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM history WHERE cpu_c = 5`).Scan(&surviving); err != nil {
		t.Fatal(err)
	}
	if surviving != 0 {
		t.Fatalf("5d sample must be pruned by 2d+2d retention, got %d rows", surviving)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM history WHERE cpu_c = 35`).Scan(&surviving); err != nil {
		t.Fatal(err)
	}
	if surviving != 1 {
		t.Fatalf("3.5d sample must survive within 2d+2d storage buffer, got %d rows", surviving)
	}
	// 展示窗口上限 = 2×24h：请求 720h 也只能看到窗口内的 1 个点
	samples, _, err := store.Aggregated(context.Background(), 720, 2000, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].CPUC != 51 {
		t.Fatalf("display window should clamp to retention days, got %+v", samples)
	}
}

// PruneIfNeeded 每次调用都同步保留天数；日期清理仍按 1 小时节流，到期后按
// “配置天数 + 2 天缓冲”删除（配置变更最迟下个采样点生效）。
func TestPruneIfNeededSyncsRetentionDays(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	if err := store.Append(HistorySample{TS: now.Add(-40 * 24 * time.Hour).Unix(), CPUC: 40}); err != nil {
		t.Fatal(err)
	}
	// 构造函数不再 Prune（用户设置要等 SyncSettings/首次 PruneIfNeeded 才
	// 生效,构造时删会按默认 30 天误伤更长保留期）,手动置 lastPrune 模拟
	// "刚清理过":本次只同步天数,不触发日期删除
	store.lastPrune = now
	if err := store.PruneIfNeeded(now, HistoryConfig{MaxSizeMB: 0, RetentionDays: 7}); err != nil {
		t.Fatal(err)
	}
	if store.retentionDays != 7 {
		t.Fatalf("retentionDays=%d, want 7", store.retentionDays)
	}
	var surviving int64
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM history WHERE cpu_c = 40`).Scan(&surviving); err != nil {
		t.Fatal(err)
	}
	if surviving != 1 {
		t.Fatalf("date prune should stay throttled within an hour, got %d rows", surviving)
	}
	// 距上次清理超过 1 小时：按 7+2 天清理
	store.lastPrune = now.Add(-2 * time.Hour)
	if err := store.PruneIfNeeded(now, HistoryConfig{MaxSizeMB: 0, RetentionDays: 7}); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM history WHERE cpu_c = 40`).Scan(&surviving); err != nil {
		t.Fatal(err)
	}
	if surviving != 0 {
		t.Fatalf("40d sample must be pruned by 7d+2d retention, got %d rows", surviving)
	}
}

// 旧配置文件没有 retention_days 字段：解析为 0 后静默归位默认 30，
// 并回写配置文件；非法值在保存时钳制。
func TestHistoryRetentionDaysLegacyConfigAndClamping(t *testing.T) {
	manager := newHistoryTestManager(t)
	// 直接写入旧版配置（无 retention_days 字段），模拟升级场景
	legacy := `{"enabled":true,"pl1_w":6,"pl2_w":15,"reapply_seconds":30,"history":{"enabled":true,"max_size_mb":32}}`
	if err := os.WriteFile(manager.ConfigPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.LoadOrCreateConfig(); err != nil {
		t.Fatal(err)
	}
	if got := manager.HistorySettings().RetentionDays; got != historyDefaultRetentionDays {
		t.Fatalf("legacy config retention=%d, want %d", got, historyDefaultRetentionDays)
	}
	data, err := os.ReadFile(manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"retention_days"`) {
		t.Fatalf("migrated config should persist retention_days:\n%s", data)
	}
	// 保存接口对非法值做与 max_size_mb 相同风格的静默钳制
	for _, test := range []struct{ in, want int }{{-5, historyMinRetentionDays}, {0, historyDefaultRetentionDays}, {100, 100}, {100000, historyMaxRetentionDays}} {
		if err := manager.SaveHistoryConfig(HistoryConfig{Enabled: true, MaxSizeMB: 32, RetentionDays: test.in}); err != nil {
			t.Fatal(err)
		}
		if got := manager.HistorySettings().RetentionDays; got != test.want {
			t.Fatalf("SaveHistoryConfig(retention=%d) persisted %d, want %d", test.in, got, test.want)
		}
	}
}

// ---- 清空历史数据库 ----

func TestHistoryStoreClearEmptiesAllTablesAndKeepsSchema(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	sample := HistorySample{
		TS: now.Unix(), CPUC: 50,
		Fans:    []HistoryFanSample{{ID: "fan", RPM: 1200, PWMPercent: 50}},
		Disks:   []HistoryDiskSample{{ID: "front-1", TemperatureC: 41}},
		Sensors: []HistorySensorSample{{Group: "cpu", Key: "Core 0", C: 50}},
	}
	if err := store.Append(sample); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(HistorySample{TS: now.Add(-time.Minute).Unix(), CPUC: 49}); err != nil {
		t.Fatal(err)
	}
	if err := store.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"history", "history_fans", "history_slots", "history_sensors"} {
		var count int64
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s should be empty after Clear, got %d rows", table, count)
		}
	}
	samples, _, err := store.Aggregated(context.Background(), store.maxRangeHours(), 2000, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 0 {
		t.Fatalf("aggregated should return nothing after Clear: %+v", samples)
	}
	// schema 保留：清空后可立即继续采样，四张表都能再写入
	if err := store.Append(sample); err != nil {
		t.Fatal(err)
	}
	samples, _, err = store.Aggregated(context.Background(), store.maxRangeHours(), 2000, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].CPUC != 50 || len(samples[0].Fans) != 1 || len(samples[0].Disks) != 1 || len(samples[0].Sensors) != 1 {
		t.Fatalf("sample after Clear mismatch: %+v", samples)
	}
}

func newHistoryTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "proc", "cpuinfo"), []byte("model name : Intel(R) Processor N100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: dir, ConfigPath: filepath.Join(dir, "config.json"), StatePath: filepath.Join(dir, "state.json"), Version: "test"}
	if _, err := manager.LoadOrCreateConfig(); err != nil {
		t.Fatalf("config setup: %v", err)
	}
	return manager
}

// /api/history/clear：方法守卫 + 管理员鉴权与 handleHistoryConfig 一致，
// 成功返回 {"ok": true} 并清空数据。
func TestHandleHistoryClear(t *testing.T) {
	manager := newHistoryTestManager(t)
	store := newTestStore(t)
	server := &Server{Manager: manager, History: store, Logger: log.New(os.Stderr, "", 0)}
	if err := store.Append(HistorySample{TS: time.Now().Unix(), CPUC: 50}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		method  string
		admin   bool
		status  int
		wantOK  bool
		emptied bool
	}{
		{"get rejected", http.MethodGet, true, http.StatusMethodNotAllowed, false, false},
		{"non-admin rejected", http.MethodPost, false, http.StatusForbidden, false, false},
		{"admin post clears", http.MethodPost, true, http.StatusOK, true, true},
	}
	for _, test := range tests {
		req := httptest.NewRequest(test.method, "/api/history/clear", nil)
		if test.admin {
			req.Header.Set("X-Trim-Isadmin", "true")
		}
		rec := httptest.NewRecorder()
		server.handleHistoryClear(rec, req)
		if rec.Code != test.status {
			t.Fatalf("%s: status=%d, want %d (body %s)", test.name, rec.Code, test.status, rec.Body.String())
		}
		if test.wantOK {
			var payload map[string]bool
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("%s: decode response: %v", test.name, err)
			}
			if payload["ok"] != true {
				t.Fatalf("%s: response should be {\"ok\": true}, got %s", test.name, rec.Body.String())
			}
		}
		if test.emptied {
			var count int64
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("%s: history should be empty, got %d rows", test.name, count)
			}
		}
	}
}

// /api/history 的 max_points 查询参数：窗口式回看（30 分钟–24 小时档拉取
// 数倍跨度数据）靠大 max_points 保住窗口内密度。缺省仍按 480 点聚合上限，
// 非法值回落缺省（parseFloatQuery 语义）。
func TestHandleHistoryMaxPoints(t *testing.T) {
	manager := newHistoryTestManager(t)
	store := newTestStore(t)
	server := &Server{Manager: manager, History: store, Logger: log.New(os.Stderr, "", 0)}
	now := time.Now().Unix()
	for i := 0; i < 600; i++ {
		if err := store.Append(HistorySample{TS: now - int64(600-i)*60, CPUC: float64(40 + i%10)}); err != nil {
			t.Fatal(err)
		}
	}
	fetch := func(url string) int {
		req := httptest.NewRequest(http.MethodGet, url, nil)
		rec := httptest.NewRecorder()
		server.handleHistory(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", url, rec.Code, rec.Body.String())
		}
		var payload historyFile
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("GET %s: decode response: %v", url, err)
		}
		return len(payload.Samples)
	}
	if got := fetch("/api/history?range=24"); got > 480 {
		t.Fatalf("default max_points should cap at 480, got %d", got)
	}
	if got := fetch("/api/history?range=24&max_points=2000"); got != 600 {
		t.Fatalf("max_points=2000 should return all 600 minute samples un-aggregated, got %d", got)
	}
	if got := fetch("/api/history?range=24&max_points=not-a-number"); got > 480 {
		t.Fatalf("invalid max_points should fall back to 480 cap, got %d", got)
	}
	// NaN 绕过一切数值比较（ParseFloat 接受），必须按无效参数回落缺省，
	// 否则聚合步长算出 ≤1 直接返回全部原始采样；Inf 同理按无效处理。
	if got := fetch("/api/history?range=24&max_points=NaN"); got > 480 {
		t.Fatalf("NaN max_points should fall back to 480 cap, got %d", got)
	}
	if got := fetch("/api/history?range=24&max_points=Inf"); got > 480 {
		t.Fatalf("Inf max_points should fall back to 480 cap, got %d", got)
	}
}

// /api/config/history 保存后立即按新保留期清理一次（存储保留 9 天，
// 10 天前的数据当场删除），且响应仍是 Status()。
func TestHandleHistoryConfigAppliesRetentionImmediately(t *testing.T) {
	manager := newHistoryTestManager(t)
	store := newTestStore(t)
	now := time.Now()
	if err := store.Append(HistorySample{TS: now.Add(-10 * 24 * time.Hour).Unix(), CPUC: 10}); err != nil {
		t.Fatal(err)
	}
	server := &Server{Manager: manager, History: store, Logger: log.New(os.Stderr, "", 0)}
	req := httptest.NewRequest(http.MethodPost, "/api/config/history", strings.NewReader(`{"enabled":true,"max_size_mb":64,"retention_days":7}`))
	req.Header.Set("X-Trim-Isadmin", "true")
	rec := httptest.NewRecorder()
	server.handleHistoryConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var status Status
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("response should stay Status-shaped: %v", err)
	}
	if got := manager.HistorySettings().RetentionDays; got != 7 {
		t.Fatalf("saved retention=%d, want 7", got)
	}
	if store.retentionDays != 7 {
		t.Fatalf("store retention=%d, want 7", store.retentionDays)
	}
	var surviving int64
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM history WHERE cpu_c = 10`).Scan(&surviving); err != nil {
		t.Fatal(err)
	}
	if surviving != 0 {
		t.Fatalf("10d sample must be pruned immediately after saving 7d retention, got %d rows", surviving)
	}
}

// SaveUIPrefs：合法档位落盘、区间外清零；保存传感器名等其它配置段不受影响。
func TestSaveUIPrefsPersistsAndClamps(t *testing.T) {
	manager := newHistoryTestManager(t)
	if err := manager.SaveSensorSettings(map[string]string{"mlx5:temp1": "万兆卡"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveUIPrefs(UIPrefsConfig{HistoryRangeHours: 6}); err != nil {
		t.Fatal(err)
	}
	cfg, err := manager.LoadOrCreateConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UIPrefs.HistoryRangeHours != 6 {
		t.Fatalf("ui pref should persist, got %+v", cfg.UIPrefs)
	}
	if cfg.SensorNames["mlx5:temp1"] != "万兆卡" {
		t.Fatalf("saving ui prefs must not wipe sensor names: %+v", cfg.SensorNames)
	}
	// 区间外按未设置处理：负数/0/超 30 天全部清零
	for _, invalid := range []float64{-1, 0, 0.2, 720.5, 10000} {
		if got := ClampUIHistoryRangeHours(invalid); got != 0 {
			t.Fatalf("ClampUIHistoryRangeHours(%v) = %v, want 0", invalid, got)
		}
	}
	if err := manager.SaveUIPrefs(UIPrefsConfig{HistoryRangeHours: 720}); err != nil {
		t.Fatal(err)
	}
	cfg, err = manager.LoadOrCreateConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UIPrefs.HistoryRangeHours != 720 {
		t.Fatalf("30d stop should persist, got %+v", cfg.UIPrefs)
	}
}

// 风扇调试卡显隐偏好落服务端(ui_prefs),保存成功写运行日志留痕;
// 未注入日志器时静默不报错。
func TestSaveUIPrefsFanDebugVisibleAndLog(t *testing.T) {
	manager := newHistoryTestManager(t)
	var logBuf bytes.Buffer
	manager.SetLogger(log.New(&logBuf, "", 0))
	// 开 + 档位一起存(前端整段替换语义)
	if err := manager.SaveUIPrefs(UIPrefsConfig{HistoryRangeHours: 24, FanDebugVisible: true}); err != nil {
		t.Fatal(err)
	}
	cfg, err := manager.LoadOrCreateConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.UIPrefs.FanDebugVisible || cfg.UIPrefs.HistoryRangeHours != 24 {
		t.Fatalf("fan_debug_visible should persist alongside range, got %+v", cfg.UIPrefs)
	}
	if !strings.Contains(logBuf.String(), "config.json saved") {
		t.Fatalf("save must leave a trace in the run log, got %q", logBuf.String())
	}
	// 关:字段回落 false(omitempty 后从 JSON 省略,解码即 false)
	if err := manager.SaveUIPrefs(UIPrefsConfig{HistoryRangeHours: 24}); err != nil {
		t.Fatal(err)
	}
	cfg, err = manager.LoadOrCreateConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UIPrefs.FanDebugVisible {
		t.Fatal("fan_debug_visible should turn off when saved as false")
	}
	// 关闭后再次保存:同样留下"保存发生过"的痕迹(日志只记保存事实,
	// 不记字段值——排查"客户说没同步"只需要时间戳)
	logBuf.Reset()
	if err := manager.SaveUIPrefs(UIPrefsConfig{HistoryRangeHours: 24}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logBuf.String(), "config.json saved") {
		t.Fatalf("second save should be logged as well, got %q", logBuf.String())
	}
}

// /api/config/ui-prefs：方法守卫 + 管理员鉴权同其它配置接口，成功返回 Status。
func TestHandleUIPrefsConfig(t *testing.T) {
	manager := newHistoryTestManager(t)
	server := &Server{Manager: manager, Logger: log.New(os.Stderr, "", 0)}
	tests := []struct {
		name   string
		method string
		admin  bool
		body   string
		status int
		saved  float64
	}{
		{"get rejected", http.MethodGet, true, "", http.StatusMethodNotAllowed, 0},
		{"non-admin rejected", http.MethodPost, false, `{"history_range_hours":2}`, http.StatusForbidden, 0},
		{"admin post saves", http.MethodPost, true, `{"history_range_hours":2}`, http.StatusOK, 2},
		{"out of range zeroed", http.MethodPost, true, `{"history_range_hours":9000}`, http.StatusOK, 0},
	}
	for _, test := range tests {
		var body io.Reader
		if test.body != "" {
			body = strings.NewReader(test.body)
		}
		req := httptest.NewRequest(test.method, "/api/config/ui-prefs", body)
		if test.admin {
			req.Header.Set("X-Trim-Isadmin", "true")
		}
		rec := httptest.NewRecorder()
		server.handleUIPrefsConfig(rec, req)
		if rec.Code != test.status {
			t.Fatalf("%s: status=%d, want %d (body %s)", test.name, rec.Code, test.status, rec.Body.String())
		}
		if test.saved != 0 || test.name == "out of range zeroed" {
			cfg, err := manager.LoadOrCreateConfig()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.UIPrefs.HistoryRangeHours != test.saved {
				t.Fatalf("%s: stored ui pref = %v, want %v", test.name, cfg.UIPrefs.HistoryRangeHours, test.saved)
			}
		}
	}
}

// 流式 CSV 归并连接的边界：子表孤儿行（ts 不在主表，老库的 foreign_keys 是
// 连接级 PRAGMA、无法完全排除）既不成列也不成行，导出不报错。
func TestHistoryExportCSVSkipsOrphanSubRows(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	if err := store.Append(HistorySample{TS: now.Unix(), CPUC: 50, Fans: []HistoryFanSample{{ID: "it87:fan1", RPM: 900, PWMPercent: 30}}}); err != nil {
		t.Fatal(err)
	}
	// 直接往子表塞孤儿行（ts 不在主表）：ID/键独有，走列发现与归并两条路径。
	// FK 生效时插不进孤儿行（这本身就是运行中库的保障），这里按连接关掉
	// PRAGMA 模拟"老库历史遗留"。
	conn, err := store.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys = OFF`); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `INSERT INTO history_fans (ts, fan_id, rpm, pwm_percent) VALUES (?, 'ghost:fan9', 1, 1)`, now.Add(time.Hour).Unix()); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `INSERT INTO history_sensors (ts, grp, key, c) VALUES (?, 'other', 'orphan', 99)`, now.Add(2*time.Hour).Unix()); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	conn.Close()
	var buf bytes.Buffer
	if err := store.WriteCSV(context.Background(), &buf); err != nil {
		t.Fatalf("csv: %v", err)
	}
	content := buf.String()
	if strings.Contains(content, "ghost:fan9") {
		t.Fatalf("orphan fan (ts not in main) must not become a column:\n%s", content)
	}
	// 按键名断言而不是值（99）：裸子串会撞上采样 ts 里的数字（如
	// 1791993195），测试随墙钟间歇性失败。
	if strings.Contains(content, "orphan") {
		t.Fatalf("orphan sensor must not leak into export:\n%s", content)
	}
	if !strings.Contains(content, "it87:fan1_rpm") || !strings.Contains(content, "900") {
		t.Fatalf("real data must survive orphan handling:\n%s", content)
	}
	lines := strings.Split(strings.TrimSpace(content), "\n")
	if len(lines) != 2 {
		t.Fatalf("expect header + 1 data row, got %d:\n%s", len(lines), content)
	}
}

// 范围导出（writeCSVLocked，月度归档复用）：只导出 [lo, hi) 的采样，列头
// 同样限定在范围内——归档单个月份时，其它月份出现过的 ID 不该混进列头；
// 全库导出（公开 WriteCSV）行为不变。
func TestWriteCSVRangeFiltersRowsAndColumns(t *testing.T) {
	store := newTestStore(t)
	base := time.Date(2026, 9, 15, 12, 0, 0, 0, time.Local)
	insert := func(at time.Time, fanID string) {
		t.Helper()
		sample := HistorySample{TS: at.Unix(), CPUC: 50}
		if fanID != "" {
			sample.Fans = []HistoryFanSample{{ID: fanID, RPM: 1000, PWMPercent: 40}}
		}
		if err := store.Append(sample); err != nil {
			t.Fatal(err)
		}
	}
	insert(base.Add(-24*time.Hour), "old:fan") // 范围外（9/14）
	insert(base, "sep:fan")                    // 范围内（9/15）
	insert(base.Add(24*time.Hour), "oct:fan")  // 范围外（9/16）
	lo := base.Add(-time.Minute).Unix()
	hi := base.Add(time.Minute).Unix()
	var buf bytes.Buffer
	if err := store.writeCSVLocked(context.Background(), &buf, lo, hi); err != nil {
		t.Fatalf("range csv: %v", err)
	}
	content := buf.String()
	lines := strings.Split(strings.TrimSpace(content), "\n")
	if len(lines) != 2 {
		t.Fatalf("expect header + 1 in-range data row, got %d:\n%s", len(lines), content)
	}
	if strings.Contains(content, "old:fan") || strings.Contains(content, "oct:fan") {
		t.Fatalf("out-of-range IDs must not appear as rows or columns:\n%s", content)
	}
	if !strings.Contains(content, "sep:fan_rpm") || !strings.Contains(content, "1000") {
		t.Fatalf("in-range data must survive:\n%s", content)
	}
	buf.Reset()
	if err := store.WriteCSV(context.Background(), &buf); err != nil {
		t.Fatalf("full csv: %v", err)
	}
	full := buf.String()
	fullLines := strings.Split(strings.TrimSpace(full), "\n")
	if len(fullLines) != 4 {
		t.Fatalf("full export should keep all 3 samples, got %d lines:\n%s", len(fullLines), full)
	}
	for _, column := range []string{"old:fan_rpm", "sep:fan_rpm", "oct:fan_rpm"} {
		if !strings.Contains(full, column) {
			t.Fatalf("full export missing column %s:\n%s", column, full)
		}
	}
}

// ---- 长期记录（归档后再清理） ----

// readArchiveCSV 解压月度归档文件（宽表 CSV + gzip，2026-10 起取代 SQLite
// 月文件）并返回全部文本（含 BOM 与表头）。
func readArchiveCSV(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// archiveCSVDataRows 归档文件的数据行数（不含表头）。宽表字段都是数值与
// 时间文本、不含内嵌换行，按行计数安全。
func archiveCSVDataRows(t *testing.T, path string) int {
	t.Helper()
	content := strings.TrimPrefix(readArchiveCSV(t, path), "\ufeff")
	content = strings.TrimRight(content, "\n")
	if content == "" {
		return 0
	}
	return strings.Count(content, "\n")
}

// 开启长期记录后，日期清理先把待删数据按月归档成独立压缩 CSV 文件
// （tad-history-YYYYMM.csv.gz），主库再删除；跨月数据各归各的文件。
func TestArchiveBeforePruneMonthlyFiles(t *testing.T) {
	store := newTestStore(t)
	archiveDir := filepath.Join(t.TempDir(), "archive")
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local)
	base := now.Add(-10 * 24 * time.Hour) // 2026-09-25 12:00，跨 9/10 两个月
	var tsList []int64
	for ts := base; !ts.After(now); ts = ts.Add(time.Hour) {
		sample := HistorySample{TS: ts.Unix(), CPUC: 50,
			Fans:  []HistoryFanSample{{ID: "f1", RPM: 1000, PWMPercent: 40}},
			Disks: []HistoryDiskSample{{ID: "d1", TemperatureC: 40}}}
		if err := store.Append(sample); err != nil {
			t.Fatal(err)
		}
		tsList = append(tsList, ts.Unix())
	}
	// 直接设字段 + Prune(now)：SyncSettings 用真实 time.Now()，测试日期会漂
	store.retentionDays = 5
	store.archiveEnabled = true
	store.archiveDir = archiveDir
	if err := store.Prune(now); err != nil {
		t.Fatal(err)
	}
	cutoff := now.Add(-7 * 24 * time.Hour).Unix()
	var wantSep int
	for _, ts := range tsList {
		if ts < cutoff {
			wantSep++
		}
	}
	// 9 月的旧数据归档成 202609 文件；10 月数据都还在保留期内，不该有 202610 文件
	sepPath := filepath.Join(archiveDir, "tad-history-202609.csv.gz")
	if _, err := os.Stat(sepPath); err != nil {
		t.Fatalf("September archive missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(archiveDir, "tad-history-202610.csv.gz")); !os.IsNotExist(err) {
		t.Fatalf("October archive should not exist (nothing pruned from October), err=%v", err)
	}
	// CSV 数据行数 = 9 月过期采样数；列头带风扇/盘位列；每行首列 ts 都在
	// cutoff 之前（归档不得包含保留窗口内的数据）。
	lines := strings.Split(strings.TrimRight(strings.TrimPrefix(readArchiveCSV(t, sepPath), "\ufeff"), "\n"), "\n")
	if len(lines)-1 != wantSep {
		t.Fatalf("archive should hold %d samples, got %d data rows", wantSep, len(lines)-1)
	}
	if !strings.Contains(lines[0], "fan_f1_rpm") || !strings.Contains(lines[0], "disk_d1_c") {
		t.Fatalf("archive header should carry fan/disk columns, got %s", lines[0])
	}
	for _, line := range lines[1:] {
		tsField, _, _ := strings.Cut(line, ",")
		ts, err := strconv.ParseInt(tsField, 10, 64)
		if err != nil {
			t.Fatalf("archive row must start with unix ts, got %q", line)
		}
		if ts >= cutoff {
			t.Fatalf("archive must not contain rows inside the retention window, ts=%d", ts)
		}
	}
	var mainCount int64
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&mainCount); err != nil {
		t.Fatal(err)
	}
	if mainCount != int64(len(tsList)-wantSep) {
		t.Fatalf("main should keep only in-window samples: got %d, want %d", mainCount, len(tsList)-wantSep)
	}
}

// SaveHistoryConfig 对长期记录目录的保存时校验：开关开了必须有绝对路径且可
// 创建可写；关闭时目录字段清空。
func TestSaveHistoryConfigArchiveValidation(t *testing.T) {
	manager := newHistoryTestManager(t)
	save := func(history HistoryConfig) error {
		return manager.SaveHistoryConfig(history)
	}
	if err := save(HistoryConfig{Enabled: true, MaxSizeMB: 64, RetentionDays: 30, ArchiveEnabled: true}); err == nil || !strings.Contains(err.Error(), "绝对路径") {
		t.Fatalf("enabled without dir should fail with abs-path error, got %v", err)
	}
	if err := save(HistoryConfig{Enabled: true, MaxSizeMB: 64, RetentionDays: 30, ArchiveEnabled: true, ArchiveDir: "relative/dir"}); err == nil || !strings.Contains(err.Error(), "绝对路径") {
		t.Fatalf("relative dir should fail, got %v", err)
	}
	valid := filepath.Join(t.TempDir(), "archive")
	if err := save(HistoryConfig{Enabled: true, MaxSizeMB: 64, RetentionDays: 30, ArchiveEnabled: true, ArchiveDir: valid + "/深层级"}); err != nil {
		t.Fatalf("valid dir should save: %v", err)
	}
	cfg, err := manager.LoadOrCreateConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.History.ArchiveEnabled || cfg.History.ArchiveDir != filepath.Join(valid, "深层级") {
		t.Fatalf("archive config not persisted: %+v", cfg.History)
	}
	if err := save(HistoryConfig{Enabled: true, MaxSizeMB: 64, RetentionDays: 30, ArchiveEnabled: false, ArchiveDir: valid}); err != nil {
		t.Fatal(err)
	}
	cfg, err = manager.LoadOrCreateConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.History.ArchiveDir != "" {
		t.Fatalf("disabling archive should clear dir, got %q", cfg.History.ArchiveDir)
	}
}

// 长期记录持续失败（目录不可写）时**永久暂停删除保数据**——归档失败绝不
// 静默丢弃，错误持续上抛由日志暴露；目录修好（SyncSettings）后失败计数清零、
// 恢复归档并正常清理过期数据。
func TestArchiveFailureNeverDeletes(t *testing.T) {
	store := newTestStore(t)
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := store.Append(HistorySample{TS: now.Add(-40 * 24 * time.Hour).Unix(), CPUC: 40}); err != nil {
		t.Fatal(err)
	}
	store.retentionDays = 5
	store.archiveEnabled = true
	store.archiveDir = blocked
	count := func() int64 {
		t.Helper()
		var n int64
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := store.Prune(now); err == nil {
		t.Fatal("archive failure should pause pruning and surface the error")
	}
	if count() != 1 {
		t.Fatal("data must be kept while archiving fails")
	}
	for i := 1; i <= 30; i++ {
		if err := store.Prune(now.Add(time.Duration(i) * time.Hour)); err == nil {
			t.Fatalf("round %d: archive failure must keep surfacing the error", i)
		}
	}
	if count() != 1 {
		t.Fatal("archive failure must never delete data, regardless of streak length")
	}
	if store.archiveFailStreak != 31 {
		t.Fatalf("failure streak should keep counting, got %d", store.archiveFailStreak)
	}
	// 用户修好目录重新保存：失败计数清零，归档恢复，过期数据被归档+删除
	fixed := filepath.Join(t.TempDir(), "archive")
	if err := store.SyncSettings(HistoryConfig{Enabled: true, MaxSizeMB: 64, RetentionDays: 5, ArchiveEnabled: true, ArchiveDir: fixed}); err != nil {
		t.Fatal(err)
	}
	if store.archiveFailStreak != 0 {
		t.Fatalf("SyncSettings should reset the failure streak, got %d", store.archiveFailStreak)
	}
	if count() != 0 {
		t.Fatal("after recovery the expired sample should be archived and pruned")
	}
}

// ---- 用户问的组合边界（大小上限 × 保存天数 × 长期记录） ----

// 边界1：大小上限最小（8MB）+ 保存 1 天 + 长期记录开。
// 1 天保留让主库只剩 ~3 天数据（几百 KB），大小清理永远够不着 8MB；
// 过期数据按月归档后再删。整个组合正常收敛，互不干扰。
func TestEdgeTinyCapShortRetentionArchiveOn(t *testing.T) {
	store := newTestStore(t)
	archiveDir := filepath.Join(t.TempDir(), "archive")
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local)
	base := now.Add(-5 * 24 * time.Hour)
	for ts := base; !ts.After(now); ts = ts.Add(time.Hour) {
		if err := store.Append(HistorySample{TS: ts.Unix(), CPUC: 50}); err != nil {
			t.Fatal(err)
		}
	}
	store.retentionDays = 1
	store.archiveEnabled = true
	store.archiveDir = archiveDir
	if err := store.Prune(now); err != nil {
		t.Fatal(err)
	}
	cutoff := now.Add(-3 * 24 * time.Hour).Unix()
	var mainCount, mainOld int64
	if err := store.db.QueryRow(`SELECT COUNT(*), SUM(ts < ?) FROM history`, cutoff).Scan(&mainCount, &mainOld); err != nil {
		t.Fatal(err)
	}
	if mainOld != 0 || mainCount == 0 {
		t.Fatalf("main should hold only the 3-day window, got %d rows (%d old)", mainCount, mainOld)
	}
	entries, _ := filepath.Glob(filepath.Join(archiveDir, "tad-history-*.csv.gz"))
	if len(entries) == 0 {
		t.Fatal("expired days should be archived before deletion")
	}
	if store.dbSizeBytes() > 8<<20 {
		t.Fatalf("db should stay far below the 8MB cap, got %d", store.dbSizeBytes())
	}
}

// 边界2：保存天数巨大（365000 被钳到 36500）+ 长期记录开 + 小上限。
// 日期清理永不触发；数据涨到上限后由大小清理接管——旧的一天先归档成
// 月文件再删，主库有界、归档持续累积（这正是长期记录的用途）。
func TestEdgeHugeRetentionArchiveOnSizeDrivesPrune(t *testing.T) {
	store := newTestStore(t)
	archiveDir := filepath.Join(t.TempDir(), "archive")
	if got := ClampHistoryRetentionDays(365000); got != historyMaxRetentionDays {
		t.Fatalf("365000 days must clamp to the overflow guard, got %d", got)
	}
	now := time.Now()
	// 满配形态灌到超过 1MB（≈5000+ 采样点），小时级跨 5+ 天
	base := now.Add(-6 * 24 * time.Hour)
	inserted := 0
	for ts := base; inserted < 5600; ts = ts.Add(time.Minute) {
		sample := HistorySample{TS: ts.Unix(), CPUC: 50,
			Fans:    []HistoryFanSample{{ID: "f1", RPM: 1000, PWMPercent: 40}, {ID: "f2", RPM: 1100, PWMPercent: 45}},
			Disks:   []HistoryDiskSample{{ID: "d1", TemperatureC: 40}, {ID: "d2", TemperatureC: 41}},
			Sensors: []HistorySensorSample{{Group: "cpu", Key: "Core 0", C: 50}, {Group: "other", Key: "acpi", C: 30}}}
		if err := store.Append(sample); err != nil {
			t.Fatal(err)
		}
		inserted++
	}
	if store.dbSizeBytes() <= 1<<20 {
		t.Fatalf("dataset should exceed the 1MB test cap, got %d", store.dbSizeBytes())
	}
	store.retentionDays = historyMaxRetentionDays
	store.archiveEnabled = true
	store.archiveDir = archiveDir
	if err := store.PruneIfNeeded(now.Add(time.Hour), HistoryConfig{MaxSizeMB: 1, RetentionDays: historyMaxRetentionDays, ArchiveEnabled: true, ArchiveDir: archiveDir}); err != nil {
		t.Fatal(err)
	}
	entries, _ := filepath.Glob(filepath.Join(archiveDir, "tad-history-*.csv.gz"))
	if len(entries) == 0 {
		t.Fatal("size-driven prune should archive expired days when retention is huge")
	}
	if store.dbSizeBytes() > 1<<20+2<<20 {
		t.Fatalf("db should be bounded near the cap after size pruning, got %d", store.dbSizeBytes())
	}
	var remaining int64
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining == 0 || int(remaining) >= inserted {
		t.Fatalf("some oldest data should be archived+deleted, remaining=%d inserted=%d", remaining, inserted)
	}
}

// 边界4（对照）：同样的巨大保留天数 + 小上限，但长期记录关——大小清理直接删，
// 不产生任何归档文件（数据按设计永久丢弃）。
func TestEdgeHugeRetentionArchiveOffSizePruneDeletesSilently(t *testing.T) {
	store := newTestStore(t)
	archiveDir := filepath.Join(t.TempDir(), "archive")
	now := time.Now()
	base := now.Add(-6 * 24 * time.Hour)
	for i := 0; i < 5600; i++ {
		sample := HistorySample{TS: base.Add(time.Duration(i) * time.Minute).Unix(), CPUC: 50,
			Fans:    []HistoryFanSample{{ID: "f1", RPM: 1000, PWMPercent: 40}, {ID: "f2", RPM: 1100, PWMPercent: 45}},
			Disks:   []HistoryDiskSample{{ID: "d1", TemperatureC: 40}, {ID: "d2", TemperatureC: 41}},
			Sensors: []HistorySensorSample{{Group: "cpu", Key: "Core 0", C: 50}, {Group: "other", Key: "acpi", C: 30}}}
		if err := store.Append(sample); err != nil {
			t.Fatal(err)
		}
	}
	store.retentionDays = historyMaxRetentionDays
	store.archiveEnabled = false
	store.archiveDir = archiveDir
	if err := store.PruneIfNeeded(now.Add(time.Hour), HistoryConfig{MaxSizeMB: 1, RetentionDays: historyMaxRetentionDays, ArchiveDir: archiveDir}); err != nil {
		t.Fatal(err)
	}
	entries, _ := filepath.Glob(filepath.Join(archiveDir, "*.csv.gz"))
	if len(entries) != 0 {
		t.Fatalf("archive off must not create archive files, got %v", entries)
	}
	var remaining int64
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining == 0 || int(remaining) >= 5600 {
		t.Fatalf("size prune should still delete without archiving, remaining=%d", remaining)
	}
}

// 同一自然月被多次冲刷：后写批次与已有月文件按 ts 归并去重（同 ts 保留
// 新值，等价旧 SQLite 方案 INSERT OR REPLACE 的重跑幂等），列头取并集。
func TestArchiveSameMonthMergeDedupAndColumns(t *testing.T) {
	store := newTestStore(t)
	archiveDir := filepath.Join(t.TempDir(), "archive")
	store.archiveEnabled = true
	store.archiveDir = archiveDir
	day := func(d, h int) time.Time {
		return time.Date(2026, 10, d, h, 0, 0, 0, time.Local)
	}
	appendFan := func(at time.Time, fanID string, rpm int64) {
		t.Helper()
		sample := HistorySample{TS: at.Unix(), CPUC: 50,
			Fans: []HistoryFanSample{{ID: fanID, RPM: rpm, PWMPercent: 40}}}
		if err := store.Append(sample); err != nil {
			t.Fatal(err)
		}
	}
	lo, hi := day(1, 0).Unix(), day(2, 0).Unix()
	for h := 0; h < 5; h++ {
		appendFan(day(1, h), "a:fan", 1000)
	}
	if err := store.archiveBetween(lo, hi); err != nil {
		t.Fatal(err)
	}
	// 第二批：B 风扇；其中一个 ts 与第一批重叠（主库 upsert 后该时刻只剩 B 值）
	appendFan(day(1, 3), "b:fan", 2000)
	appendFan(day(1, 5), "b:fan", 2000)
	appendFan(day(1, 6), "b:fan", 2000)
	if err := store.archiveBetween(lo, hi); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(archiveDir, "tad-history-202610.csv.gz")
	content := strings.TrimPrefix(readArchiveCSV(t, path), "\ufeff")
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(lines) != 8 { // 表头 + 7 个唯一 ts
		t.Fatalf("merged month file should hold header + 7 rows, got %d:\n%s", len(lines), content)
	}
	columns := strings.Split(lines[0], ",")
	indexOf := func(name string) int {
		for i, c := range columns {
			if c == name {
				return i
			}
		}
		return -1
	}
	aRPM, bRPM := indexOf("fan_a:fan_rpm"), indexOf("fan_b:fan_rpm")
	if aRPM < 0 || bRPM < 0 {
		t.Fatalf("merged header must carry both fan columns, got %s", lines[0])
	}
	// 03:00 行属于两批：归并后必须保留新批次的值（fan_b 有值、fan_a 空）
	rowTS := day(1, 3).Unix()
	for _, line := range lines[1:] {
		fields := strings.Split(line, ",")
		ts, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			t.Fatalf("row must start with unix ts: %q", line)
		}
		if ts != rowTS {
			continue
		}
		if fields[bRPM] != "2000" || fields[aRPM] != "" {
			t.Fatalf("overwritten ts must keep the newer batch: fan_a=%q fan_b=%q", fields[aRPM], fields[bRPM])
		}
		return
	}
	t.Fatalf("row for ts %d missing", rowTS)
}

// 归档缓冲（SSD）+ 冲刷（HDD）：数据照常按"当前时刻"落库（现实中不存在
// 低于水位的行），过期后先留在主库当缓冲，攒到跨自然月或体量达阈值才一次性
// 写入归档盘并删除——HDD 平时可持续休眠。
func TestArchiveFlushBuffering(t *testing.T) {
	store := newTestStore(t)
	archiveDir := filepath.Join(t.TempDir(), "archive")
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local)
	store.retentionDays = 1 // cutoff = now-3d
	store.archiveEnabled = true
	store.archiveDir = archiveDir
	insert := func(at time.Time) {
		t.Helper()
		if err := store.Append(HistorySample{TS: at.Unix(), CPUC: 50}); err != nil {
			t.Fatal(err)
		}
	}
	mainCount := func() int64 {
		t.Helper()
		var n int64
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	archiveRows := func() int64 {
		t.Helper()
		return int64(archiveCSVDataRows(t, filepath.Join(archiveDir, "tad-history-202610.csv.gz")))
	}
	// 首轮(水位未建立):已过期的数据立即冲刷归档+删除
	for i := 0; i < 5; i++ {
		insert(now.Add(-4 * 24 * time.Hour).Add(time.Duration(i) * time.Hour))
	}
	if err := store.Prune(now); err != nil {
		t.Fatal(err)
	}
	if archiveRows() != 5 || mainCount() != 0 {
		t.Fatalf("first flush: archive=%d main=%d, want 5/0", archiveRows(), mainCount())
	}
	// 照常落库(窗口内);当它随时间过期后,同月且体量未达阈值 → 继续缓冲
	for i := 0; i < 5; i++ {
		insert(now.Add(-time.Duration(i) * time.Hour))
	}
	if err := store.Prune(now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if archiveRows() != 5 {
		t.Fatalf("buffering phase must not write the HDD, archive=%d", archiveRows())
	}
	if mainCount() != 5 {
		t.Fatalf("in-window rows must stay in main, main=%d", mainCount())
	}
	// 过期后仍未达阈值:继续缓冲(数据不丢)
	if err := store.Prune(now.Add(4 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if archiveRows() != 5 || mainCount() != 5 {
		t.Fatalf("under-threshold buffering: archive=%d main=%d, want 5/5", archiveRows(), mainCount())
	}
	// 跨自然月:一次性把缓冲冲进归档盘并删除
	if err := store.Prune(now.Add(35 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if archiveRows() != 10 || mainCount() != 0 {
		t.Fatalf("month-rollover flush: archive=%d main=%d, want 10/0", archiveRows(), mainCount())
	}
}

// ---- 用户追问的冲刷版组合边界 ----

// 边界1：最小上限（8MB，用户嘴里的"1M"会被钳到这）+ 1 天保留 + 长期记录开。
// 首轮冲刷后过期数据在 SSD 缓冲；缓冲+窗口顶到大小上限时，大小清理提前
// 冲刷（HDD 写入频率从"每月"提前到"每约一周"，仍是大块单写）。
func TestEdgeSmallCapArchiveOnEarlyFlushBySize(t *testing.T) {
	store := newTestStore(t)
	archiveDir := filepath.Join(t.TempDir(), "archive")
	now := time.Now()
	base := now.Add(-6 * 24 * time.Hour)
	for i := 0; i < 5600; i++ {
		sample := HistorySample{TS: base.Add(time.Duration(i) * time.Minute).Unix(), CPUC: 50,
			Fans:    []HistoryFanSample{{ID: "f1", RPM: 1000, PWMPercent: 40}, {ID: "f2", RPM: 1100, PWMPercent: 45}},
			Disks:   []HistoryDiskSample{{ID: "d1", TemperatureC: 40}, {ID: "d2", TemperatureC: 41}},
			Sensors: []HistorySensorSample{{Group: "cpu", Key: "Core 0", C: 50}, {Group: "other", Key: "acpi", C: 30}}}
		if err := store.Append(sample); err != nil {
			t.Fatal(err)
		}
	}
	store.retentionDays = 1
	store.archiveEnabled = true
	store.archiveDir = archiveDir
	// 首轮：水位未建立，立即冲刷（把已过期的部分写走）
	if err := store.Prune(now); err != nil {
		t.Fatal(err)
	}
	first := store.dbSizeBytes()
	// 同月且未达 42MB 行数阈值：继续缓冲（不碰 HDD）
	if err := store.PruneIfNeeded(now.Add(time.Hour), HistoryConfig{MaxSizeMB: 1, RetentionDays: 1, ArchiveEnabled: true, ArchiveDir: archiveDir}); err != nil {
		t.Fatal(err)
	}
	// 大小上限（1MB 测试档，路径与 8MB 完全一致）压过来：提前冲刷+收缩
	if err := store.PruneIfNeeded(now.Add(2*time.Hour), HistoryConfig{MaxSizeMB: 1, RetentionDays: 1, ArchiveEnabled: true, ArchiveDir: archiveDir}); err != nil {
		t.Fatal(err)
	}
	if store.dbSizeBytes() > first && store.dbSizeBytes() > 1<<20+3<<20 {
		t.Fatalf("size pressure should trigger early flush, db=%d (first=%d)", store.dbSizeBytes(), first)
	}
	entries, _ := filepath.Glob(filepath.Join(archiveDir, "tad-history-*.csv.gz"))
	if len(entries) == 0 {
		t.Fatal("archive files should exist after early flush")
	}
}

// 边界2：巨大保留天数（365000 被钳 36500）+ 长期记录开。
// cutoff 和水位都在极远的历史里：跨月条件必须比较 cutoff 的月份而不是
// now 的月份，否则每小时都误判"该冲刷"空跑全库扫描。这里锁定：首轮之后
// 同小时的第二轮不再产生任何冲刷动作（无归档文件、无数据变动）。
func TestEdgeHugeRetentionArchiveOnNoFlushChurn(t *testing.T) {
	store := newTestStore(t)
	archiveDir := filepath.Join(t.TempDir(), "archive")
	now := time.Now()
	if err := store.Append(HistorySample{TS: now.Add(-time.Hour).Unix(), CPUC: 50}); err != nil {
		t.Fatal(err)
	}
	store.retentionDays = historyMaxRetentionDays
	store.archiveEnabled = true
	store.archiveDir = archiveDir
	// 首轮：水位建立（指向极远的过去），没有数据过期，归档目录应为空
	if err := store.Prune(now); err != nil {
		t.Fatal(err)
	}
	entries, _ := filepath.Glob(filepath.Join(archiveDir, "*.csv.gz"))
	if len(entries) != 0 {
		t.Fatalf("nothing expired yet, no archive files expected, got %v", entries)
	}
	if store.archiveFlushDue(now.Add(time.Hour), now.Add(time.Hour).Add(-time.Duration(historyMaxRetentionDays+2)*24*time.Hour).Unix()) {
		// cutoff 仍与水位同月（都在极远过去）：不得判定需要冲刷
		t.Fatal("month condition must compare against cutoff, not now")
	}
	// 第二轮：无数据变化 → 无归档文件、采样原样保留
	if err := store.Prune(now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	entries, _ = filepath.Glob(filepath.Join(archiveDir, "*.csv.gz"))
	if len(entries) != 0 {
		t.Fatalf("huge retention must not produce archive churn, got %v", entries)
	}
	var count int64
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("sample must be kept under huge retention, got %d", count)
	}
}

// FlushArchive（清空前的补冲刷）：把水位到当前 cutoff 的过期缓冲一次性写进
// 归档盘并从主库删除；长期记录关闭或保留期极长时是空操作。
func TestFlushArchivePendingBuffer(t *testing.T) {
	store := newTestStore(t)
	archiveDir := filepath.Join(t.TempDir(), "archive")
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local)
	store.retentionDays = 1 // cutoff = now-3d
	store.archiveEnabled = true
	store.archiveDir = archiveDir
	// 首轮定时冲刷:老数据走掉,水位建立
	for i := 0; i < 5; i++ {
		if err := store.Append(HistorySample{TS: now.Add(-4 * 24 * time.Hour).Add(time.Duration(i) * time.Hour).Unix(), CPUC: 50}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Prune(now); err != nil {
		t.Fatal(err)
	}
	// 缓冲期 3 行:首轮冲刷时还在窗口内(水位=cutoff1 之后)、随后过期进入
	// 待冲刷区间 [cutoff1, cutoff2)
	for i := 0; i < 3; i++ {
		if err := store.Append(HistorySample{TS: now.Add(-3 * 24 * time.Hour).Add(time.Duration(30+i*10) * time.Minute).Unix(), CPUC: 51}); err != nil {
			t.Fatal(err)
		}
	}
	flushed, err := store.FlushArchive(now.Add(time.Hour))
	if err != nil || !flushed {
		t.Fatalf("manual flush: flushed=%v err=%v", flushed, err)
	}
	archived := archiveCSVDataRows(t, filepath.Join(archiveDir, "tad-history-202610.csv.gz"))
	if archived != 8 {
		t.Fatalf("archive should hold 5+3=8 samples after manual flush, got %d", archived)
	}
	var mainCount int64
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&mainCount); err != nil {
		t.Fatal(err)
	}
	if mainCount != 0 {
		t.Fatalf("flushed buffer should be deleted from main, got %d rows", mainCount)
	}
	// 长期记录关闭:空操作
	store2 := newTestStore(t)
	if err := store2.Append(HistorySample{TS: now.Add(-time.Hour).Unix(), CPUC: 50}); err != nil {
		t.Fatal(err)
	}
	flushed, err = store2.FlushArchive(now)
	if err != nil || flushed {
		t.Fatalf("archive-off flush should be a no-op, flushed=%v err=%v", flushed, err)
	}
	var n int64
	if err := store2.db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("no-op flush must not delete, got %d rows", n)
	}
}

// 过期窗口里没有行（刚开启/保存过设置水位归零，数据全在保留窗口内）时，
// 补冲刷必须如实返回 false：不写文件、不动数据，前端据此不提示"已补归档"。
// 水位照常推进，空窗被消费。
func TestFlushArchiveNothingExpiredNoFlushClaim(t *testing.T) {
	store := newTestStore(t)
	archiveDir := filepath.Join(t.TempDir(), "archive")
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local)
	store.retentionDays = 30
	store.archiveEnabled = true
	store.archiveDir = archiveDir
	// 数据全部落在保留窗口内（cutoff = now-32 天）
	if err := store.Append(HistorySample{TS: now.Add(-24 * time.Hour).Unix(), CPUC: 50}); err != nil {
		t.Fatal(err)
	}
	flushed, err := store.FlushArchive(now)
	if err != nil {
		t.Fatal(err)
	}
	if flushed {
		t.Fatal("empty expired window must not claim a flush")
	}
	if entries, _ := filepath.Glob(filepath.Join(archiveDir, "*")); len(entries) != 0 {
		t.Fatalf("empty flush must not write archive files, got %v", entries)
	}
	var count int64
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("in-window data must be untouched, got %d rows", count)
	}
	if store.archiveWatermark == 0 {
		t.Fatal("empty window should still advance the watermark")
	}
	// 反向对照：数据真的过期后，同一接口要报 flushed=true 且产出文件
	store.archiveWatermark = 0
	if err := store.Append(HistorySample{TS: now.Add(-40 * 24 * time.Hour).Unix(), CPUC: 45}); err != nil {
		t.Fatal(err)
	}
	if flushed, err := store.FlushArchive(now); err != nil || !flushed {
		t.Fatalf("expired rows must flush: flushed=%v err=%v", flushed, err)
	}
	if entries, _ := filepath.Glob(filepath.Join(archiveDir, "*.csv.gz")); len(entries) == 0 {
		t.Fatal("flush with expired rows should produce an archive file")
	}
}

// /api/history/archive：方法守卫 + 管理员鉴权同其它配置接口。
func TestHandleHistoryArchive(t *testing.T) {
	manager := newHistoryTestManager(t)
	store := newTestStore(t)
	server := &Server{Manager: manager, History: store, Logger: log.New(os.Stderr, "", 0)}
	tests := []struct {
		name   string
		method string
		admin  bool
		status int
	}{
		{"get rejected", http.MethodGet, true, http.StatusMethodNotAllowed},
		{"non-admin rejected", http.MethodPost, false, http.StatusForbidden},
		{"admin post ok", http.MethodPost, true, http.StatusOK},
	}
	for _, test := range tests {
		req := httptest.NewRequest(test.method, "/api/history/archive", nil)
		if test.admin {
			req.Header.Set("X-Trim-Isadmin", "true")
		}
		rec := httptest.NewRecorder()
		server.handleHistoryArchive(rec, req)
		if rec.Code != test.status {
			t.Fatalf("%s: status=%d, want %d (body %s)", test.name, rec.Code, test.status, rec.Body.String())
		}
	}
}

// ---- 运行日志：独立大小设置与清理/导出 ----

func TestClampLogMaxSize(t *testing.T) {
	for _, test := range []struct{ in, want int64 }{{-5, 1}, {0, 1}, {1, 1}, {16, 16}, {256, 256}, {257, 256}, {9999, 256}} {
		if got := ClampLogMaxSize(test.in); got != test.want {
			t.Fatalf("ClampLogMaxSize(%d)=%d, want %d", test.in, got, test.want)
		}
	}
}

func TestSaveLogConfigPersistsAndNormalizes(t *testing.T) {
	manager := newHistoryTestManager(t)
	if err := manager.SaveLogConfig(LogConfig{MaxSizeMB: 32}); err != nil {
		t.Fatal(err)
	}
	if got := manager.LogSettings().MaxSizeMB; got != 32 {
		t.Fatalf("log max = %d, want 32", got)
	}
	if err := manager.SaveLogConfig(LogConfig{MaxSizeMB: 9999}); err != nil {
		t.Fatal(err)
	}
	if got := manager.LogSettings().MaxSizeMB; got != logMaxMaxSizeMB {
		t.Fatalf("9999 should clamp to %d, got %d", logMaxMaxSizeMB, got)
	}
	// 保存日志设置不影响历史配置
	if got := manager.HistorySettings().MaxSizeMB; got != 64 {
		t.Fatalf("history config should be untouched, max_size=%d", got)
	}
	// 旧配置迁移：log 段缺失时归一为默认
	cfg, err := manager.LoadOrCreateConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Log = LogConfig{}
	if err := writeJSONAtomic(manager.ConfigPath, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := manager.LogSettings().MaxSizeMB; got != logDefaultMaxSizeMB {
		t.Fatalf("missing log section should default to %d, got %d", logDefaultMaxSizeMB, got)
	}
}

func TestHandleLogClearAndExport(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "tad-module.log")
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("old line\n"); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if err := os.WriteFile(logPath+".1", []byte("older backup\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := newHistoryTestManager(t)
	server := &Server{Manager: manager, History: nil, LogPath: logPath, Logger: log.New(os.Stderr, "", 0)}

	// 清空：截断主文件、删两代备份
	req := httptest.NewRequest(http.MethodPost, "/api/log/clear", nil)
	req.Header.Set("X-Trim-Isadmin", "true")
	rec := httptest.NewRecorder()
	server.handleLogClear(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s", rec.Code, rec.Body.String())
	}
	info, err := os.Stat(logPath)
	if err != nil || info.Size() != 0 {
		t.Fatalf("main log should be truncated, size=%d err=%v", info.Size(), err)
	}
	if _, err := os.Stat(logPath + ".1"); !os.IsNotExist(err) {
		t.Fatalf(".1 backup should be removed, err=%v", err)
	}

	// 导出：.1 与主文件按序拼接（此刻都为空/不存在，应 200 且空体）。
	// 导出仅限管理员（含设备路径等环境细节），请求须带管理员头
	req = httptest.NewRequest(http.MethodGet, "/api/log/export", nil)
	req.Header.Set("X-Trim-Isadmin", "true")
	rec = httptest.NewRecorder()
	server.handleLogExport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("export status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 重新写入内容后导出应包含全部行
	file, err = os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("current line\n"); err != nil {
		t.Fatal(err)
	}
	file.Close()
	req = httptest.NewRequest(http.MethodGet, "/api/log/export", nil)
	req.Header.Set("X-Trim-Isadmin", "true")
	rec = httptest.NewRecorder()
	server.handleLogExport(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "current line") {
		t.Fatalf("export should stream main log, status=%d body=%q", rec.Code, rec.Body.String())
	}

	// 方法守卫与非文件模式
	req = httptest.NewRequest(http.MethodPost, "/api/log/export", nil)
	req.Header.Set("X-Trim-Isadmin", "true")
	rec = httptest.NewRecorder()
	server.handleLogExport(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("export POST should 405, got %d", rec.Code)
	}
	empty := &Server{Manager: manager, LogPath: "", Logger: log.New(os.Stderr, "", 0)}
	req = httptest.NewRequest(http.MethodGet, "/api/log/export", nil)
	req.Header.Set("X-Trim-Isadmin", "true")
	rec = httptest.NewRecorder()
	empty.handleLogExport(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("stderr-mode export should 404, got %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/log/clear", nil)
	req.Header.Set("X-Trim-Isadmin", "true")
	rec = httptest.NewRecorder()
	empty.handleLogClear(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stderr-mode clear should be ok (no-op), got %d", rec.Code)
	}
}

// ---- 风扇调试控制（按风扇接管;含 0 转风扇;手动转速;自动递增） ----

func newFanDebugTestManager(t *testing.T) *Manager {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "cpuinfo"), []byte("model name : Intel(R) Processor N100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon3")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	writeTestValue(t, filepath.Join(hwmon, "fan1_input"), "1200")
	writeTestValue(t, filepath.Join(hwmon, "pwm1"), "102")
	writeTestValue(t, filepath.Join(hwmon, "pwm1_enable"), "2")
	writeTestValue(t, filepath.Join(hwmon, "fan2_input"), "0") // 0 转风扇(未接)
	writeTestValue(t, filepath.Join(hwmon, "pwm2"), "0")
	writeTestValue(t, filepath.Join(hwmon, "pwm2_enable"), "0")
	coretemp := filepath.Join(root, "sys", "class", "hwmon", "hwmon9")
	if err := os.MkdirAll(coretemp, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(coretemp, "name"), "coretemp")
	writeTestValue(t, filepath.Join(coretemp, "temp1_input"), "45000")
	manager := &Manager{Root: root, ConfigPath: filepath.Join(root, "config.json"), StatePath: filepath.Join(root, "state.json"), Version: "test"}
	t.Cleanup(manager.stopFanRPMLearning) // 停学习路径写盘,防 TempDir 清理竞争
	if _, err := manager.LoadOrCreateConfig(); err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestFanDebugListsAllFansIncludingZeroRPM(t *testing.T) {
	manager := newFanDebugTestManager(t)
	state := manager.FanDebugState()
	if len(state.Fans) != 2 {
		t.Fatalf("should list all fans including 0-RPM, got %d", len(state.Fans))
	}
	if state.Fans[1].RPM != 0 {
		t.Fatalf("0-RPM fan must be included, got %+v", state.Fans[1])
	}
}

func TestFanDebugTakeoverAndPercent(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	pwm1 := filepath.Join(hwmon, "pwm1")
	pwm2 := filepath.Join(hwmon, "pwm2")

	// 启用风扇控制并绑定 fan1:未接管时它受曲线控制;接管期间曲线跳过它
	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	if err := manager.SaveFanConfig(cfg); err != nil {
		t.Fatal(err)
	}
	// 未接管时手动设置被拒绝
	if err := manager.SetFanDebugValue("it8613:hwmon3:fan1", 50, "percent"); err == nil {
		t.Fatal("percent set must fail when the fan is not taken over")
	}
	// 接管 fan1:从当前转速无缝接管(102 → 40%),fan2 不受影响
	if err := manager.SetFanDebugTakeover("it8613:hwmon3:fan1", true); err != nil {
		t.Fatal(err)
	}
	if got := manager.fanDebugTakenOver["it8613:hwmon3:fan1"]; got != 63 {
		t.Fatalf("takeover should start at curve-applied percent (63 for 45C), got %d", got)
	}
	if err := manager.SetFanDebugValue("it8613:hwmon3:fan1", 50, "percent"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(pwm1); strings.TrimSpace(string(got)) != "128" {
		t.Fatalf("pwm1 = %s, want 128 (50%%)", got)
	}
	// fan2(未接管)不受调试影响
	if got, _ := os.ReadFile(pwm2); strings.TrimSpace(string(got)) != "0" {
		t.Fatalf("untaken fan must stay untouched, pwm2=%s", got)
	}
	// 调试期间 ApplyFanCurrent 不得覆盖被接管风扇(曲线控制暂停对该风扇生效)
	if err := manager.ApplyFanCurrent(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(pwm1); strings.TrimSpace(string(got)) != "128" {
		t.Fatalf("taken-over fan must resist curve apply, pwm1=%s", got)
	}
	// 释放:立即回到曲线控制(测试环境有 coretemp 45°C → CPU 曲线 40-55 段
	// 插值 63%,pwm = 63*255/100 ≈ 161)
	if err := manager.SetFanDebugTakeover("it8613:hwmon3:fan1", false); err != nil {
		t.Fatal(err)
	}
	if err := manager.ApplyFanCurrent(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(pwm1); strings.TrimSpace(string(got)) != "161" {
		t.Fatalf("released fan should apply CPU curve for 45C (63 to 161), got %s", got)
	}
}

func TestFanDebugAutoRampStopsAtHundred(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	if err := manager.SetFanDebugTakeover("it8613:hwmon3:fan1", true); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetFanDebugValue("it8613:hwmon3:fan1", 20, "percent"); err != nil {
		t.Fatal(err)
	}
	// 基准=手动 20%,每秒 +60%:20 → 80 → 100 停。带截止时间轮询代替固定
	// 睡眠（CI 调度抖动下固定 2.6s 可能还没跑完两步递增）
	if err := manager.SetFanDebugAuto("it8613:hwmon3:fan1", true, 60, 1, "percent"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		state := manager.FanDebugState()
		fan := state.Fans[0]
		if !fan.AutoRunning && fan.AutoDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("auto test did not finish in time: %+v", fan)
		}
		time.Sleep(50 * time.Millisecond)
	}
	state := manager.FanDebugState()
	fan := state.Fans[0]
	if fan.AutoDone != true || fan.PWMPercent != 100 {
		t.Fatalf("fan should be done at 100%%, got %+v", fan)
	}
	pwm1, err := os.ReadFile(filepath.Join(hwmon, "pwm1"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(pwm1)) != "255" {
		t.Fatalf("final pwm should be 255 (100%%), got %s", pwm1)
	}
	// 手动停止:跑完的风扇可重新开启;停止保留步进/间隔便于原样重开
	if err := manager.SetFanDebugAuto("it8613:hwmon3:fan1", true, 90, 1, "percent"); err != nil {
		t.Fatal(err)
	}
	manager.SetFanDebugAuto("it8613:hwmon3:fan1", false, 0, 0, "")
	entry := manager.fanDebugAuto.entries["it8613:hwmon3:fan1"]
	if entry.Running {
		t.Fatal("stop should clear running flag")
	}
	if entry.Step != 90 || entry.Interval != 1 {
		t.Fatalf("stop should keep step/interval for restart, got %+v", entry)
	}
}

// 跑到上限后再开:基准归零重跑,而不是开着立刻又完成。
func TestFanDebugAutoRestartFromBottomAfterDone(t *testing.T) {
	manager := newFanDebugTestManager(t)
	if err := manager.SetFanDebugTakeover("it8613:hwmon3:fan1", true); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetFanDebugValue("it8613:hwmon3:fan1", 100, "percent"); err != nil {
		t.Fatal(err)
	}
	// 基准=100(已在上限):开启应归零,下一次递增 0+60=60 继续跑
	if err := manager.SetFanDebugAuto("it8613:hwmon3:fan1", true, 60, 1, "percent"); err != nil {
		t.Fatal(err)
	}
	if base := manager.fanDebugTakenOver["it8613:hwmon3:fan1"]; base != 0 {
		t.Fatalf("base at unit max should reset to 0 on restart, got %d", base)
	}
	state := manager.FanDebugState()
	if !state.AutoRunning {
		t.Fatal("restarted auto should be running")
	}
	if state.Fans[0].AutoDone {
		t.Fatal("restart from bottom should not be done immediately")
	}
	manager.SetFanDebugAuto("it8613:hwmon3:fan1", false, 0, 0, "")
}

// 释放接管应同时终止该风扇的自动递增:条目删除、状态行不再计入。
func TestFanDebugTakeoverReleaseStopsAuto(t *testing.T) {
	manager := newFanDebugTestManager(t)
	if err := manager.SetFanDebugTakeover("it8613:hwmon3:fan1", true); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetFanDebugValue("it8613:hwmon3:fan1", 20, "percent"); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetFanDebugAuto("it8613:hwmon3:fan1", true, 10, 5, "percent"); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetFanDebugTakeover("it8613:hwmon3:fan1", false); err != nil {
		t.Fatal(err)
	}
	state := manager.FanDebugState()
	fan := state.Fans[0]
	if fan.AutoRunning || fan.AutoDone || fan.AutoStep != 0 {
		t.Fatalf("release should drop the fan's auto entry, got %+v", fan)
	}
	if state.AutoRunning {
		t.Fatal("state.auto_running should be false after release")
	}
}

// 两个风扇各自步进/间隔独立推进:快风扇先到 100% 并完成,慢风扇继续。
func TestFanDebugAutoPerFanIndependence(t *testing.T) {
	manager := newFanDebugTestManager(t)
	if err := manager.SetFanDebugTakeover("it8613:hwmon3:fan1", true); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetFanDebugTakeover("it8613:hwmon3:fan2", true); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetFanDebugValue("it8613:hwmon3:fan1", 20, "percent"); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetFanDebugValue("it8613:hwmon3:fan2", 50, "percent"); err != nil {
		t.Fatal(err)
	}
	entries := map[string]fanDebugAutoEntry{
		"it8613:hwmon3:fan1": {Step: 60, Interval: 1},
		"it8613:hwmon3:fan2": {Step: 5, Interval: 1},
	}
	if err := manager.StartFanDebugAutoBatch(entries); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2600 * time.Millisecond)
	state := manager.FanDebugState()
	byID := map[string]FanDebugFan{}
	for _, fan := range state.Fans {
		byID[fan.ID] = fan
	}
	f1 := byID["it8613:hwmon3:fan1"]
	f2 := byID["it8613:hwmon3:fan2"]
	if f1.AutoDone != true || f1.PWMPercent != 100 {
		t.Fatalf("fast fan should be done at 100%%, got %+v", f1)
	}
	if f2.AutoDone == true || f2.PWMPercent <= 50 {
		t.Fatalf("slow fan should still be ramping (step 5/s), got %+v", f2)
	}
	if !state.AutoRunning {
		t.Fatal("state.auto_running should stay true while the slow fan is still ramping")
	}
}

// 参数校验:步进 0/超上限、未接管风扇的条目均应拒绝。
func TestFanDebugValidation(t *testing.T) {
	manager := newFanDebugTestManager(t)
	if err := manager.SetFanDebugTakeover("it8613:hwmon3:fan1", true); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id   string
		step int
	}{
		{"it8613:hwmon3:fan1", 0}, {"it8613:hwmon3:fan1", 101},
	} {
		entries := map[string]fanDebugAutoEntry{test.id: {Step: test.step, Interval: 10}}
		if err := manager.StartFanDebugAutoBatch(entries); err == nil {
			t.Fatalf("StartFanDebugAutoBatch(step=%d) should fail", test.step)
		}
	}
	// 未接管任何风扇时自动测试拒绝
	entries := map[string]fanDebugAutoEntry{"ghost:fan9": {Step: 30, Interval: 10}}
	if err := manager.StartFanDebugAutoBatch(entries); err == nil {
		t.Fatal("auto test without taken-over fans should fail")
	}
	// rpm 单位的边界:步进 0 与超 2000 均拒绝
	if err := manager.SetFanDebugAuto("it8613:hwmon3:fan1", true, 0, 10, "rpm"); err == nil {
		t.Fatal("rpm step 0 should fail")
	}
	if err := manager.SetFanDebugAuto("it8613:hwmon3:fan1", true, 2001, 10, "rpm"); err == nil {
		t.Fatal("rpm step 2001 should fail")
	}
}

// rpm 单位:1000 RPM 按 2000=100% 换算写硬件;递增到 rpm 上限完成并写满 PWM。
func TestFanDebugRpmUnit(t *testing.T) {
	manager := newFanDebugTestManager(t)
	manager.stopFanRPMLearning() // 桩风扇转速不随 PWM 变,学习会真标定 base 干扰上限断言
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	if err := manager.SetFanDebugTakeover("it8613:hwmon3:fan1", true); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetFanDebugValue("it8613:hwmon3:fan1", 1000, "rpm"); err != nil {
		t.Fatal(err)
	}
	pwm1, err := os.ReadFile(filepath.Join(hwmon, "pwm1"))
	if err != nil {
		t.Fatal(err)
	}
	// 1000 RPM = 50% → percentToPWM(50)
	if got, want := strings.TrimSpace(string(pwm1)), fmt.Sprintf("%d", percentToPWM(50)); got != want {
		t.Fatalf("1000 rpm should write pwm %s (50%%), got %s", want, got)
	}
	state := manager.FanDebugState()
	if fan := state.Fans[0]; fan.DebugUnit != "rpm" || fan.DebugPercent != 1000 {
		t.Fatalf("state should keep rpm value 1000, got %+v", fan)
	}
	// 递增到 rpm 上限(2000)完成:1900 + 200/秒 → 约 1 秒内到顶
	if err := manager.SetFanDebugValue("it8613:hwmon3:fan1", 1900, "rpm"); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetFanDebugAuto("it8613:hwmon3:fan1", true, 200, 1, "rpm"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for state = manager.FanDebugState(); !state.Fans[0].AutoDone; state = manager.FanDebugState() {
		if time.Now().After(deadline) {
			t.Fatalf("rpm ramp should finish at 2000 rpm, got %+v", state.Fans[0])
		}
		time.Sleep(100 * time.Millisecond)
	}
	if state.Fans[0].DebugPercent != 2000 {
		t.Fatalf("done value should be 2000 rpm, got %+v", state.Fans[0])
	}
	pwm1, err = os.ReadFile(filepath.Join(hwmon, "pwm1"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(pwm1)) != "255" {
		t.Fatalf("2000 rpm should write pwm 255, got %s", strings.TrimSpace(string(pwm1)))
	}
	manager.SetFanDebugAuto("it8613:hwmon3:fan1", false, 0, 0, "")
}

// 递增中切换单位:基准与递增判定跟随新单位(1000 RPM=50% 时切 percent,继续按 % 递增)。
func TestFanDebugUnitSwitchMidRamp(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	if err := manager.SetFanDebugTakeover("it8613:hwmon3:fan1", true); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetFanDebugValue("it8613:hwmon3:fan1", 1000, "rpm"); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetFanDebugAuto("it8613:hwmon3:fan1", true, 200, 1, "rpm"); err != nil {
		t.Fatal(err)
	}
	// 递增进行中把单位(与等比基准值)切到 percent:1000 RPM → 50%
	if err := manager.SetFanDebugValue("it8613:hwmon3:fan1", 50, "percent"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2600 * time.Millisecond)
	state := manager.FanDebugState()
	fan := state.Fans[0]
	if fan.AutoDone || fan.DebugUnit != "percent" {
		t.Fatalf("ramp should continue in percent after unit switch, got %+v", fan)
	}
	// 50% + 200 rpm 换算步进(切单位时前端会把步进换算成 10%)×2 秒 ≈ 70%
	if fan.DebugPercent <= 50 || fan.DebugPercent >= 100 {
		t.Fatalf("ramp should progress from 50%% in percent unit, got %d", fan.DebugPercent)
	}
	pwm1, err := os.ReadFile(filepath.Join(hwmon, "pwm1"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(pwm1)); got == "0" {
		t.Fatalf("pwm should keep ramping after unit switch, got %s", got)
	}
	manager.SetFanDebugAuto("it8613:hwmon3:fan1", false, 0, 0, "")
}

func TestHandleFansDebugEndpoints(t *testing.T) {
	manager := newFanDebugTestManager(t)
	server := &Server{Manager: manager, History: nil, LogPath: "", Logger: log.New(os.Stderr, "", 0)}
	call := func(method, path, body string, admin bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if body != "" {
			req = httptest.NewRequest(method, path, strings.NewReader(body))
		}
		if admin {
			req.Header.Set("X-Trim-Isadmin", "true")
		}
		rec := httptest.NewRecorder()
		switch path {
		case "/api/fans/debug":
			server.handleFansDebug(rec, req)
		case "/api/fans/debug/takeover":
			server.handleFansDebugTakeover(rec, req)
		case "/api/fans/debug/pwm":
			server.handleFansDebugPWM(rec, req)
		case "/api/fans/debug/auto":
			server.handleFansDebugAuto(rec, req)
		case "/api/fans/debug/auto/stop":
			server.handleFansDebugAutoStop(rec, req)
		}
		return rec
	}
	// 非管理员
	if rec := call(http.MethodGet, "/api/fans/debug", "", false); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin GET should 403, got %d", rec.Code)
	}
	// 方法守卫
	if rec := call(http.MethodPost, "/api/fans/debug", "", true); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST state should 405, got %d", rec.Code)
	}
	// 接管 + 手动 + 自动 + 停止(管理员)
	if rec := call(http.MethodPost, "/api/fans/debug/takeover", `{"id":"it8613:hwmon3:fan1","taken":true}`, true); rec.Code != http.StatusOK {
		t.Fatalf("takeover status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := call(http.MethodPost, "/api/fans/debug/pwm", `{"id":"it8613:hwmon3:fan1","value":77,"unit":"percent"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("pwm status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := manager.fanDebugTakenOver["it8613:hwmon3:fan1"]; got != 77 {
		t.Fatalf("percent=%d, want 77", got)
	}
	if rec := call(http.MethodPost, "/api/fans/debug/auto", `{"fans":[{"id":"it8613:hwmon3:fan1","step":10,"interval":1,"unit":"percent"}]}`, true); rec.Code != http.StatusOK {
		t.Fatalf("auto status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := call(http.MethodPost, "/api/fans/debug/auto/stop", `{"id":"it8613:hwmon3:fan1"}`, true); rec.Code != http.StatusOK {
		t.Fatalf("auto stop status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// SaveUIPrefs：历史曲线显隐偏好（父类开关 + 组内勾选）随整段保存往返，
// 空子列表保留（组开但一条不画），false 开关值不被 omitempty 丢键。
func TestSaveUIPrefsHistorySeriesVisibility(t *testing.T) {
	manager := newHistoryTestManager(t)
	prefs := UIPrefsConfig{
		HistoryRangeHours: 12,
		HistorySeries:     map[string]bool{"cpu": true, "gpu": false, "fan": true},
		HistoryChildren: map[string][]string{
			"cpu": {"__agg__"},
			"fan": {"it8792:it8792:fan1", "it8792:it8792:fan2"},
			"nic": {}, // 组开但一条不勾：空列表必须原样保留
		},
	}
	if err := manager.SaveUIPrefs(prefs); err != nil {
		t.Fatal(err)
	}
	cfg, err := manager.LoadOrCreateConfig()
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.UIPrefs
	if got.HistoryRangeHours != 12 {
		t.Fatalf("range hours = %v, want 12", got.HistoryRangeHours)
	}
	if !got.HistorySeries["cpu"] || got.HistorySeries["gpu"] || !got.HistorySeries["fan"] {
		t.Fatalf("history_series = %v, want cpu/fan on and gpu off", got.HistorySeries)
	}
	if _, ok := got.HistorySeries["sata"]; ok {
		t.Fatal("unset groups should stay absent (frontend default applies)")
	}
	if len(got.HistoryChildren["cpu"]) != 1 || got.HistoryChildren["cpu"][0] != "__agg__" {
		t.Fatalf("cpu children = %v, want [__agg__]", got.HistoryChildren["cpu"])
	}
	if len(got.HistoryChildren["fan"]) != 2 {
		t.Fatalf("fan children = %v, want 2 entries", got.HistoryChildren["fan"])
	}
	if list, ok := got.HistoryChildren["nic"]; !ok || len(list) != 0 {
		t.Fatalf("empty child list must round-trip (group on, nothing drawn), got %v ok=%v", list, ok)
	}
}

// 运行日志导出仅限管理员（日志含设备路径等环境细节），未带管理员头一律 403。
func TestHandleLogExportAdminOnly(t *testing.T) {
	manager := newHistoryTestManager(t)
	server := &Server{Manager: manager, Logger: log.New(os.Stderr, "", 0)}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "tad-module.log")
	if err := os.WriteFile(logPath, []byte("2026-10-10T00:00:00Z started\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server.LogPath = logPath
	req := httptest.NewRequest(http.MethodGet, "/api/log/export", nil)
	rec := httptest.NewRecorder()
	server.handleLogExport(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin export must be rejected with 403, got %d (body %s)", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/log/export", nil)
	req.Header.Set("X-Trim-Isadmin", "true")
	rec = httptest.NewRecorder()
	server.handleLogExport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin export must succeed, got %d (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "started") {
		t.Fatalf("export should stream the log content, got %q", rec.Body.String())
	}
}
