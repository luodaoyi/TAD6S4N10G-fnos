package powerguard

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFanStateCaptureRejectsDifferentCPU(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "cpuinfo"), []byte("model name : Intel(R) Processor N100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root, StatePath: filepath.Join(root, "original-state.json")}
	state := OriginalState{Version: stateVersion, CPUModel: "Intel(R) Core(TM) i3-N305"}
	if err := writeJSONAtomic(manager.StatePath, state, 0o600); err != nil {
		t.Fatal(err)
	}
	err := manager.captureOriginalFanLocked(FanDevice{ID: "fan1", PWM: 128, Mode: 2})
	if !errors.Is(err, ErrOriginalStateCPUMismatch) {
		t.Fatalf("capture error=%v, want %v", err, ErrOriginalStateCPUMismatch)
	}
	if _, err := os.Stat(manager.fanStatePath()); !os.IsNotExist(err) {
		t.Fatalf("fan state was written for mismatched hardware: %v", err)
	}
}

func TestDefaultFanConfigIsSafeAndDisabled(t *testing.T) {
	cfg := DefaultFanConfig()
	if cfg.Enabled {
		t.Fatal("fan control must be disabled by default")
	}
	if cfg.MinPWMPercent != 40 || cfg.EmergencyTempC != 85 || cfg.PollSeconds != 2 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	// 出厂默认＝系统集成的「平衡」档（8 点 CPU / 8 点 HDD / 4 点 NVMe），
	// 直接和 builtinFanCurves 对齐，改参数时这里不用跟着改数字。
	balanced := builtinFanCurves["balanced"]
	if !fanPointsEqual(cfg.Curve, clampCurvePWM(balanced.CPU, cfg.MinPWMPercent)) {
		t.Fatalf("unexpected default CPU curve: %+v", cfg.Curve)
	}
	if !fanPointsEqual(cfg.HDDCurve, clampCurvePWM(balanced.HDD, cfg.MinPWMPercent)) {
		t.Fatalf("unexpected default HDD curve: %+v", cfg.HDDCurve)
	}
	if !fanPointsEqual(cfg.NVMeCurve, clampCurvePWM(balanced.NVMe, cfg.MinPWMPercent)) {
		t.Fatalf("unexpected default NVMe curve: %+v", cfg.NVMeCurve)
	}
	if len(cfg.Curve) != 8 || len(cfg.HDDCurve) != 8 || len(cfg.NVMeCurve) != 4 {
		t.Fatalf("default curve shape changed: CPU=%d HDD=%d NVMe=%d", len(cfg.Curve), len(cfg.HDDCurve), len(cfg.NVMeCurve))
	}
	if cfg.Curve[0].PWMPercent != 40 || cfg.Curve[len(cfg.Curve)-1].PWMPercent != 100 {
		t.Fatalf("default CPU curve must start at minimum and end at 100%%: %+v", cfg.Curve)
	}
	if len(cfg.HDDSlotIDs) != 6 || len(cfg.NVMeSlotIDs) != 4 {
		t.Fatalf("all storage slots must participate by default: HDD=%v NVMe=%v", cfg.HDDSlotIDs, cfg.NVMeSlotIDs)
	}
}

func TestInterpolatePWMPercent(t *testing.T) {
	cfg := DefaultFanConfig()
	curve := cfg.Curve
	// 出厂默认＝平衡档：20/42/44/60/70/75/80/85 °C → 40/40/47/47/55/65/85/100 %
	tests := []struct {
		temp float64
		want int
	}{
		{30, 40},
		{42, 40},
		{47.5, 47},
		{55, 47},
		{70, 55},
		{75, 65},
		{80, 85},
		{85, 100},
	}
	for _, test := range tests {
		if got := interpolatePWMPercent(curve, test.temp, cfg.MinPWMPercent, cfg.EmergencyTempC); got != test.want {
			t.Errorf("temperature %.1f: got %d%%, want %d%%", test.temp, got, test.want)
		}
	}
}

func TestDiscoverFansRequiresMatchingRPMAndPWMNodes(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon7")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	for name, value := range map[string]string{
		"fan2_input": "0", "pwm2": "26", "pwm2_enable": "2",
		"fan3_input": "1662", "pwm3": "80", "pwm3_enable": "2",
		"fan4_input": "900",
	} {
		writeTestValue(t, filepath.Join(hwmon, name), value)
	}

	fans, err := (&Manager{Root: root}).DiscoverFans()
	if err != nil {
		t.Fatal(err)
	}
	if len(fans) != 2 {
		t.Fatalf("got %d complete fan channels, want 2: %+v", len(fans), fans)
	}
	if !(&Manager{Root: root}).IT87DriverDetected() {
		t.Fatal("IT87 hwmon node was not detected")
	}
	if fans[1].ID != "it8613:hwmon7:fan3" || fans[1].RPM != 1662 || fans[1].Channel != 3 {
		t.Fatalf("unexpected spinning fan: %+v", fans[1])
	}
}

func TestSetFanPWMAndRestore(t *testing.T) {
	dir := t.TempDir()
	pwm := filepath.Join(dir, "pwm3")
	enable := filepath.Join(dir, "pwm3_enable")
	writeTestValue(t, pwm, "80")
	writeTestValue(t, enable, "2")
	fan := FanDevice{ID: "it8613:test:fan3", Channel: 3, PWMPath: pwm, EnablePath: enable}

	if err := setFanPWM(fan, 50); err != nil {
		t.Fatal(err)
	}
	if got, _ := readInt(pwm); got != 128 {
		t.Fatalf("pwm=%d, want 128", got)
	}
	if got, _ := readInt(enable); got != 1 {
		t.Fatalf("mode=%d, want manual mode 1", got)
	}
	if err := restoreFan(fan, originalFan{ID: fan.ID, PWM: 80, Mode: 2}); err != nil {
		t.Fatal(err)
	}
	if got, _ := readInt(pwm); got != 80 {
		t.Fatalf("restored pwm=%d, want 80", got)
	}
	if got, _ := readInt(enable); got != 2 {
		t.Fatalf("restored mode=%d, want automatic mode 2", got)
	}
}

func TestSetFanPWMLeavesDriverFullSpeedMode(t *testing.T) {
	dir := t.TempDir()
	pwm := filepath.Join(dir, "pwm3")
	enable := filepath.Join(dir, "pwm3_enable")
	writeTestValue(t, pwm, "255")
	writeTestValue(t, enable, "0")
	fan := FanDevice{ID: "it8613:test:fan3", Channel: 3, PWMPath: pwm, EnablePath: enable}

	if err := setFanPWM(fan, 70); err != nil {
		t.Fatal(err)
	}
	if got, _ := readInt(pwm); got != 179 {
		t.Fatalf("pwm=%d, want 179", got)
	}
	if got, _ := readInt(enable); got != 1 {
		t.Fatalf("mode=%d, want manual mode 1", got)
	}

	writeTestValue(t, pwm, "255")
	writeTestValue(t, enable, "0")
	if err := setFanPWM(fan, 100); err != nil {
		t.Fatalf("already-full fan should be accepted: %v", err)
	}
}

func TestNormalizeConfigMigratesFanDefaults(t *testing.T) {
	cfg := Config{Enabled: true, PL1W: 15, PL2W: 15, ReapplySeconds: 30}
	if !normalizeConfig(&cfg) {
		t.Fatal("legacy config was not migrated")
	}
	if cfg.Fan.Enabled || cfg.Fan.MinPWMPercent != 40 || len(cfg.Fan.Curve) != 8 || len(cfg.Fan.HDDCurve) != 8 || len(cfg.Fan.NVMeCurve) != 4 || len(cfg.Fan.HDDSlotIDs) != 6 || len(cfg.Fan.NVMeSlotIDs) != 4 {
		t.Fatalf("unexpected migrated fan config: %+v", cfg.Fan)
	}
	if normalizeConfig(&cfg) {
		t.Fatal("normalized config was migrated a second time")
	}
}

// 旧出厂默认（最低转速 60% + 三条曲线都从 60% 起）在升级时整体升到新默认；
// 只要用户动过最低转速或任意一条曲线，就一个字都不改。
func TestNormalizeConfigUpgradesUntouchedLegacyFanDefaults(t *testing.T) {
	legacy := func() FanConfig {
		return FanConfig{
			Enabled:        false,
			MinPWMPercent:  60,
			EmergencyTempC: 85,
			PollSeconds:    2,
			Curve: []FanPoint{
				{TempC: 40, PWMPercent: 60}, {TempC: 55, PWMPercent: 70},
				{TempC: 70, PWMPercent: 85}, {TempC: 80, PWMPercent: 100},
			},
			HDDCurve:    legacyDefaultStorageCurve(),
			NVMeCurve:   legacyDefaultStorageCurve(),
			HDDSlotIDs:  storageSlotIDs("front"),
			NVMeSlotIDs: storageSlotIDs("m2"),
		}
	}
	// 先把 history/log 等无关段归一化，之后只看风扇段是否被动过
	settle := func(fan FanConfig) Config {
		cfg := Config{GPIO: DefaultGPIOConfig()}
		normalizeConfig(&cfg)
		cfg.Fan = fan
		return cfg
	}

	cfg := settle(legacy())
	if !normalizeConfig(&cfg) {
		t.Fatal("未改动的旧默认风扇配置没有被升级")
	}
	if cfg.Fan.MinPWMPercent != 40 || cfg.Fan.Curve[0].PWMPercent != 40 || cfg.Fan.HDDCurve[0].PWMPercent != 40 || cfg.Fan.NVMeCurve[0].PWMPercent != 40 {
		t.Fatalf("旧默认没有整体升到新默认: %+v", cfg.Fan)
	}
	// 升级后就是新的出厂默认（平衡档），逐点对齐
	want := DefaultFanConfig()
	if !fanPointsEqual(cfg.Fan.Curve, want.Curve) || !fanPointsEqual(cfg.Fan.HDDCurve, want.HDDCurve) || !fanPointsEqual(cfg.Fan.NVMeCurve, want.NVMeCurve) {
		t.Fatalf("升级改动了曲线形态: %+v", cfg.Fan)
	}
	if normalizeConfig(&cfg) {
		t.Fatal("升级后二次归一化不该再改")
	}

	// 只改过最低转速 → 曲线原样保留（不能被升级覆盖）
	modified := legacy()
	modified.MinPWMPercent = 55
	cfg = settle(modified)
	if normalizeConfig(&cfg) {
		t.Fatal("改过最低转速的配置不该被升级")
	}
	if cfg.Fan.MinPWMPercent != 55 || cfg.Fan.Curve[0].PWMPercent != 60 {
		t.Fatalf("用户配置被改动: %+v", cfg.Fan)
	}

	// 只改过一条曲线 → 最低转速也保持原样
	modified = legacy()
	modified.NVMeCurve = []FanPoint{{TempC: 30, PWMPercent: 60}, {TempC: 70, PWMPercent: 100}}
	cfg = settle(modified)
	if normalizeConfig(&cfg) {
		t.Fatal("改过曲线的配置不该被升级")
	}
	if cfg.Fan.MinPWMPercent != 60 || cfg.Fan.Curve[0].PWMPercent != 60 {
		t.Fatalf("用户配置被改动: %+v", cfg.Fan)
	}
}

func TestNormalizeConfigMigratesDiskCurveWithoutReplacingCPUCurve(t *testing.T) {
	cfg := Config{Fan: DefaultFanConfig(), GPIO: DefaultGPIOConfig()}
	cfg.Fan.MinPWMPercent = 30
	cfg.Fan.Curve = []FanPoint{{TempC: 40, PWMPercent: 30}, {TempC: 70, PWMPercent: 90}}
	cfg.Fan.DiskCurve = []FanPoint{{TempC: 28, PWMPercent: 30}, {TempC: 48, PWMPercent: 100}}
	cfg.Fan.HDDCurve = nil
	cfg.Fan.NVMeCurve = nil
	if !normalizeConfig(&cfg) {
		t.Fatal("v0.5 config was not migrated")
	}
	if len(cfg.Fan.Curve) != 2 || cfg.Fan.Curve[0].PWMPercent != 30 {
		t.Fatalf("CPU curve was replaced during migration: %+v", cfg.Fan.Curve)
	}
	if len(cfg.Fan.HDDCurve) != 2 || cfg.Fan.HDDCurve[0].TempC != 28 || cfg.Fan.HDDCurve[1].PWMPercent != 100 {
		t.Fatalf("legacy disk curve was not copied to HDD: %+v", cfg.Fan.HDDCurve)
	}
	if !fanPointsEqual(cfg.Fan.NVMeCurve, defaultNVMeFanCurve(30)) {
		t.Fatalf("NVMe curve did not fall back to the balanced default: %+v", cfg.Fan.NVMeCurve)
	}
	if cfg.Fan.DiskCurve != nil {
		t.Fatalf("legacy disk curve was not cleared: %+v", cfg.Fan.DiskCurve)
	}
	if normalizeConfig(&cfg) {
		t.Fatal("migrated config changed a second time")
	}
}

func TestNormalizeConfigPreservesExplicitlyDisabledStorageMonitoring(t *testing.T) {
	cfg := Config{Fan: DefaultFanConfig(), GPIO: DefaultGPIOConfig(), Log: DefaultLogConfig()}
	// 已经归一化过的配置：传感器默认显示名早已补过，这里不该再被改
	cfg.SensorNameDefaultsApplied = true
	cfg.Fan.HDDSlotIDs = []string{}
	cfg.Fan.NVMeSlotIDs = []string{}
	cfg.History = HistoryConfig{Enabled: false, MaxSizeMB: 32, RetentionDays: historyDefaultRetentionDays}
	if normalizeConfig(&cfg) {
		t.Fatal("normalized config unexpectedly changed")
	}
	if len(cfg.Fan.HDDSlotIDs) != 0 || len(cfg.Fan.NVMeSlotIDs) != 0 {
		t.Fatalf("disabled storage monitoring was re-enabled: HDD=%v NVMe=%v", cfg.Fan.HDDSlotIDs, cfg.Fan.NVMeSlotIDs)
	}
	if cfg.History != (HistoryConfig{Enabled: false, MaxSizeMB: 32, RetentionDays: historyDefaultRetentionDays}) {
		t.Fatalf("explicit history settings were overwritten: %+v", cfg.History)
	}
}

func TestFanTargetsUseHigherCurve(t *testing.T) {
	cfg := DefaultFanConfig()
	cfg.MinPWMPercent = 30
	cfg.Curve = []FanPoint{{TempC: 40, PWMPercent: 30}, {TempC: 80, PWMPercent: 100}}
	cfg.HDDCurve = defaultStorageFanCurve(cfg.MinPWMPercent)
	cfg.NVMeCurve = []FanPoint{{TempC: 30, PWMPercent: 30}, {TempC: 70, PWMPercent: 90}}

	cpu, hdd, nvme, target := fanTargets(cfg, 60, 25, true, 30, true)
	if cpu != 65 || hdd != 40 || nvme != 30 || target != 65 {
		t.Fatalf("CPU should control cool storage: cpu=%d hdd=%d nvme=%d target=%d", cpu, hdd, nvme, target)
	}
	cpu, hdd, nvme, target = fanTargets(cfg, 45, 35, true, 30, true)
	if cpu != 39 || hdd != 47 || nvme != 30 || target != 47 {
		t.Fatalf("HDD should control: cpu=%d hdd=%d nvme=%d target=%d", cpu, hdd, nvme, target)
	}
	cpu, hdd, nvme, target = fanTargets(cfg, 45, 25, true, 70, true)
	if cpu != 39 || hdd != 40 || nvme != 90 || target != 90 {
		t.Fatalf("NVMe should control: cpu=%d hdd=%d nvme=%d target=%d", cpu, hdd, nvme, target)
	}
	_, hdd, nvme, target = fanTargets(cfg, 45, 0, false, 0, false)
	if hdd != 0 || nvme != 0 || target != 39 {
		t.Fatalf("missing storage temperature should not affect target: hdd=%d nvme=%d target=%d", hdd, nvme, target)
	}
}

func TestMaximumStorageTemperatureByKind(t *testing.T) {
	storage := StorageStatus{Slots: []StorageSlot{
		{ID: "front-1", TemperatureC: 43},
		{ID: "front-2", Kind: "front", TemperatureC: 44},
		{ID: "m2-1", Kind: "m2", TemperatureC: 58},
		{ID: "m2-2", Kind: "m2", TemperatureC: 64},
	}}
	storage.Slots[0].Kind = "front"
	if temperature, available := maximumStorageTemperature(storage, "front", []string{"front-1", "front-2"}); !available || temperature != 44 {
		t.Fatalf("HDD temperature=%.1f available=%v, want 44 true", temperature, available)
	}
	if temperature, available := maximumStorageTemperature(storage, "m2", []string{"m2-1", "m2-2"}); !available || temperature != 64 {
		t.Fatalf("NVMe temperature=%.1f available=%v, want 64 true", temperature, available)
	}
	if temperature, available := maximumStorageTemperature(storage, "m2", []string{"m2-1"}); !available || temperature != 58 {
		t.Fatalf("filtered NVMe temperature=%.1f available=%v, want 58 true", temperature, available)
	}
	if temperature, available := maximumStorageTemperature(storage, "m2", []string{}); available || temperature != 0 {
		t.Fatalf("disabled NVMe slots returned temperature=%.1f available=%v", temperature, available)
	}
}

func TestFanControlSourceReportsAllTiedSources(t *testing.T) {
	if source := fanControlSource(85, 85, 70); source != "cpu+hdd" {
		t.Fatalf("source=%q, want cpu+hdd", source)
	}
	if source := fanControlSource(100, 100, 100); source != "cpu+hdd+nvme" {
		t.Fatalf("source=%q, want cpu+hdd+nvme", source)
	}
}

func TestFanValidationRejectsDecreasingSpeed(t *testing.T) {
	cfg := DefaultFanConfig()
	// 默认 CPU 曲线第 3、4 点都是 47%：把第 4 点压到 40% 就构成递减
	cfg.Curve[3].PWMPercent = 40
	if err := (&Manager{}).validateFanLocked(&cfg); err == nil {
		t.Fatal("decreasing PWM curve was accepted")
	}
}

func TestFanValidationRejectsDecreasingStorageSpeed(t *testing.T) {
	cfg := DefaultFanConfig()
	// 默认 NVMe 曲线是 40/40/60/70：末点压到 50 就构成递减
	cfg.NVMeCurve[3].PWMPercent = 50
	if err := (&Manager{}).validateFanLocked(&cfg); err == nil {
		t.Fatal("decreasing NVMe PWM curve was accepted")
	}
}

func TestFanValidationRejectsInvalidOrDuplicateStorageSlots(t *testing.T) {
	cfg := DefaultFanConfig()
	cfg.HDDSlotIDs = []string{"front-1", "front-1"}
	if err := (&Manager{}).validateFanLocked(&cfg); err == nil {
		t.Fatal("duplicate HDD slot was accepted")
	}
	cfg = DefaultFanConfig()
	cfg.NVMeSlotIDs = []string{"front-1"}
	if err := (&Manager{}).validateFanLocked(&cfg); err == nil {
		t.Fatal("HDD slot was accepted as NVMe slot")
	}
	cfg = DefaultFanConfig()
	cfg.HDDSlotIDs = []string{}
	cfg.NVMeSlotIDs = []string{}
	if err := (&Manager{}).validateFanLocked(&cfg); err != nil {
		t.Fatalf("explicitly disabling storage monitoring was rejected: %v", err)
	}
}

func writeTestValue(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ---- 风扇满转基准自动标定 ----

// 全速读数稳态后按中位数更新满转基准并落盘;非全速/坏读数/未稳态不标定;
// 重启(新 Manager 惰性载入配置)后基准保留;换算与单位上限按新基准走。
func TestFanRPMCalibrationLearnsFullSpeed(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{ConfigPath: configPath}
	t.Cleanup(m.stopFanRPMLearning)
	const id = "it8792:it8792:fan2"
	// 非全速读数不标定
	m.processFanRPMSample(id, 4500, 200)
	if base := m.fanDebugRPMBaseLocked(id); base != fanRPMBaseDefault {
		t.Fatalf("sub-full-speed sample must not calibrate, base=%d", base)
	}
	// 稳态全速窗口(极差 ≤5%):中位数 4500
	for _, rpm := range []int{4480, 4520, 4500, 4490, 4510} {
		m.processFanRPMSample(id, rpm, 255)
	}
	if base := m.fanDebugRPMBaseLocked(id); base != 4500 {
		t.Fatalf("steady full-speed window should calibrate to the median, base=%d", base)
	}
	// 已落盘
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted Config
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.FanRPMBase[id] != 4500 {
		t.Fatalf("calibration must persist to config, got %+v", persisted.FanRPMBase)
	}
	// 窗口混入毛刺(极差超限):不标定,基准不被单点读数带偏
	m.processFanRPMSample(id, 9000, 255)
	m.processFanRPMSample(id, 4495, 255)
	if base := m.fanDebugRPMBaseLocked(id); base != 4500 {
		t.Fatalf("non-steady window must not recalibrate, base=%d", base)
	}
	// 重启语义:新 Manager 从配置惰性载入基准
	restarted := &Manager{ConfigPath: configPath}
	if base := restarted.fanDebugRPMBaseLocked(id); base != 4500 {
		t.Fatalf("restart should load the calibrated base, base=%d", base)
	}
	// 换算与上限按基准:rpm 2250 → 50% → PWM 128;上限钳到 4500
	if got := fanDebugRaw("rpm", 2250, restarted.fanDebugRPMBaseLocked(id)); got != percentToPWM(50) {
		t.Fatalf("rpm conversion must use the calibrated base, got %d", got)
	}
	if got := restarted.fanDebugUnitMaxFor(id, "rpm"); got != 4500 {
		t.Fatalf("rpm unit max must follow the calibrated base, got %d", got)
	}
	// 名义基准(未标定风扇)与旧公式等价:2000 RPM → 100% PWM
	if got := fanDebugRaw("rpm", 2000, fanRPMBaseDefault); got != 255 {
		t.Fatalf("default base must keep 2000 RPM=100%%, got %d", got)
	}
}

// ---- RPM 特性表学习与闭环微调 ----

// 中低档位稳态读数入特性表并落盘;重启(新 Manager)后表保留;
// 设定目标转速时初值查表插值,表外区间用满转基准外推,空表退化为线性。
func TestFanRPMMapLearnsAndInterpolates(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{ConfigPath: configPath}
	t.Cleanup(m.stopFanRPMLearning)
	const id = "it8792:it8792:fan2"
	// 两档稳态:PWM 80 → 1500,PWM 160 → 2500(每档 3 个读数)
	for _, rpm := range []int{1490, 1510, 1500} {
		m.processFanRPMSample(id, rpm, 80)
	}
	m.fanRPMLastSave = time.Time{} // 第二档落盘不受首写节流限制
	for _, rpm := range []int{2490, 2510, 2500} {
		m.processFanRPMSample(id, rpm, 160)
	}
	// 升速中(极差超限)不入表
	m.processFanRPMSample(id, 1800, 120)
	m.processFanRPMSample(id, 2200, 120)
	m.processFanRPMSample(id, 1650, 120)
	// 落盘
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted Config
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	table := persisted.FanRPMMap[id]
	if table == nil || table[80] != 1500 || table[160] != 2500 {
		t.Fatalf("steady slots must be learned, got %+v", persisted.FanRPMMap)
	}
	if _, learned := table[112]; learned {
		t.Fatal("non-steady slot must not be learned")
	}
	// 重启语义:新 Manager 载入表后插值
	restarted := &Manager{ConfigPath: configPath}
	m.mu.Lock()
	defer m.mu.Unlock()
	// 目标 2000:80+ (2000-1500)*(160-80)/(2500-1500) = 120
	if got := restarted.rpmToPWMEstimateLocked(id, 2000); got != 120 {
		t.Fatalf("interpolation: got %d, want 120", got)
	}
	// 表内低段:目标 1500 → 80;低于最小转速也停在 80(最低已知档)
	if got := restarted.rpmToPWMEstimateLocked(id, 1500); got != 80 {
		t.Fatalf("clamp to lowest known slot: got %d, want 80", got)
	}
	// 表外高转速:基准线性外推(2000 默认基准,目标 4000 → 255)
	if got := restarted.rpmToPWMEstimateLocked(id, 4000); got != 255 {
		t.Fatalf("extrapolation above table: got %d, want 255", got)
	}
	// 空表风扇:退化为基准线性(等价旧公式)
	if got := restarted.rpmToPWMEstimateLocked("other:fan9", 1000); got != percentToPWM(50) {
		t.Fatalf("empty table must fall back to linear base, got %d", got)
	}
}

// 闭环单步:超出容差按差值微调 PWM(限幅 ±20),达标静默并标记 locked,
// 连续不可达 15 次暂停,percent 单位不参与,无活跃目标时报告退出。
func TestFanRPMCloseLoopStep(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon7")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	for name, value := range map[string]string{
		"fan2_input": "2000", "pwm2": "128", "pwm2_enable": "1",
		"fan3_input": "1800", "pwm3": "100", "pwm3_enable": "1",
	} {
		writeTestValue(t, filepath.Join(hwmon, name), value)
	}
	m := &Manager{Root: root, StatePath: filepath.Join(root, "state.json")}
	t.Cleanup(m.stopFanRPMLearning)
	const idA, idB = "it8613:hwmon7:fan2", "it8613:hwmon7:fan3"
	m.mu.Lock()
	m.fanDebugTakenOver = map[string]int{idA: 3000, idB: 50}
	m.fanDebugUnits = map[string]string{idA: "rpm", idB: "percent"}
	m.mu.Unlock()

	fan2Input := filepath.Join(hwmon, "fan2_input")
	pwm2 := filepath.Join(hwmon, "pwm2")
	// 第一步:实测 2000 vs 目标 3000,容差 max(90,60)=90 → adj=(1000)*255/2000=127→钳20 → 148
	if !m.stepFanRPMCloseLoop() {
		t.Fatal("rpm target should keep the loop active")
	}
	if got, _ := readInt(pwm2); got != 148 {
		t.Fatalf("first step should write pwm=148, got %d", got)
	}
	if m.fanRPMLoopLocked[idA] {
		t.Fatal("far from target must not be locked")
	}
	// percent 风扇不参与
	if got, _ := readInt(filepath.Join(hwmon, "pwm3")); got != 100 {
		t.Fatalf("percent unit must not be touched, pwm3=%d", got)
	}
	// 连续不可达:推到 15 次上限后暂停写入（写入后隔轮观察使调整轮减半,
	// 循环按 miss 达标退出,带上限防死循环）
	writeTestValue(t, fan2Input, "2000")
	for i := 0; m.fanRPMLoopMiss[idA] < fanRPMCloseLoopMaxMiss && i < fanRPMCloseLoopMaxMiss*4; i++ {
		m.stepFanRPMCloseLoop()
	}
	missBefore := m.fanRPMLoopMiss[idA]
	if missBefore != fanRPMCloseLoopMaxMiss {
		t.Fatalf("miss should cap at %d, got %d", fanRPMCloseLoopMaxMiss, missBefore)
	}
	writeTestValue(t, pwm2, "148") // 复位,暂停后不得再写
	if !m.stepFanRPMCloseLoop() {
		t.Fatal("paused fan is still an active target")
	}
	if got, _ := readInt(pwm2); got != 148 {
		t.Fatalf("paused fan must not be adjusted, pwm=%d", got)
	}
	// 转速进入容差(3000±90):达标,locked,miss 清零
	writeTestValue(t, fan2Input, "3010")
	if !m.stepFanRPMCloseLoop() {
		t.Fatal("still active after locking")
	}
	if !m.fanRPMLoopLocked[idA] || m.fanRPMLoopMiss[idA] != 0 {
		t.Fatalf("in-tolerance reading should lock, locked=%v miss=%d", m.fanRPMLoopLocked[idA], m.fanRPMLoopMiss[idA])
	}
	// 取消接管:无活跃目标,报告退出
	m.mu.Lock()
	delete(m.fanDebugTakenOver, idA)
	delete(m.fanDebugTakenOver, idB)
	m.mu.Unlock()
	if m.stepFanRPMCloseLoop() {
		t.Fatal("no rpm targets should deactivate the loop")
	}
}

// ---- 主动标定满转基准 ----

// 标定按钮端到端:接管前未接管的风扇 → 全速写 255 → 恒定稳态读数入特性表
// 240 档 + 更新满转基准并落盘 → 恢复未接管状态(交还曲线,PWM 回到标定前值)。
// 闭环挂起:标定进行中该风扇不被闭环微调。
func TestCalibrateFanRPMFullSpeed(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon9")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	writeTestValue(t, filepath.Join(hwmon, "fan2_input"), "4490")
	writeTestValue(t, filepath.Join(hwmon, "pwm2"), "128")
	writeTestValue(t, filepath.Join(hwmon, "pwm2_enable"), "1")
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Root: root, ConfigPath: configPath, StatePath: filepath.Join(root, "state.json")}
	t.Cleanup(m.stopFanRPMLearning) // DiscoverFans 的学习 goroutine 会往 TempDir 写盘,须先于目录清理退出
	const id = "it8613:hwmon9:fan2"
	pwm2 := filepath.Join(hwmon, "pwm2")

	base, err := m.CalibrateFanRPM(id)
	if err != nil {
		t.Fatal(err)
	}
	if base != 4490 {
		t.Fatalf("steady reading 4490 should become the base, got %d", base)
	}
	// 特性表 240 档入表,基准落盘
	m.mu.Lock()
	table := m.fanRPMLearned[id]
	slot240 := table[240]
	m.mu.Unlock()
	if slot240 != 4490 {
		t.Fatalf("calibration must record the top slot, got %d", slot240)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "4490") {
		t.Fatalf("calibration must persist to config: %s", raw)
	}
	// 恢复:未接管,交还曲线,PWM 回到标定前值
	if _, taken := m.fanDebugTakenOver[id]; taken {
		t.Fatal("fan was not taken over before calibration, must be released")
	}
	if m.fanRPMSuspend[id] {
		t.Fatal("suspend flag must be cleared after calibration")
	}
	if got, _ := readInt(pwm2); got != 128 {
		t.Fatalf("original PWM must be restored, got %d", got)
	}
	// 全速确实被写入过(pwm2 在标定中被写成 255,上面已验证恢复)——
	// 直接断言闭环挂起语义:挂起中的风扇不参与闭环,唯一目标被挂起时闭环退出
	m.mu.Lock()
	m.fanDebugTakenOver = map[string]int{id: 3000}
	m.fanDebugUnits = map[string]string{id: "rpm"}
	m.fanRPMSuspend[id] = true
	m.mu.Unlock()
	writeTestValue(t, filepath.Join(hwmon, "fan2_input"), "2000")
	if m.stepFanRPMCloseLoop() {
		t.Fatal("suspended-only fan should deactivate the loop")
	}
	if got, _ := readInt(pwm2); got != 128 {
		t.Fatalf("suspended fan must not be adjusted by the close loop, pwm=%d", got)
	}
}

// ---- 回归:取消接管立即恢复转速;递增间隔不设上限 ----

// 释放接管必须立即把 PWM 写回接管时的存档值:被调试风扇不在曲线列表时
// 没有任何后台路径会再碰它,不写回就永久保持调试转速(用户实测)。
func TestFanDebugTakeoverReleaseRestoresPWM(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon8")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	writeTestValue(t, filepath.Join(hwmon, "fan2_input"), "2000")
	writeTestValue(t, filepath.Join(hwmon, "pwm2"), "100")
	writeTestValue(t, filepath.Join(hwmon, "pwm2_enable"), "2") // 接管前是 BIOS 自动温控
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Root: root, ConfigPath: configPath, StatePath: filepath.Join(root, "state.json")}
	t.Cleanup(m.stopFanRPMLearning) // DiscoverFans 的学习 goroutine 会往 TempDir 写盘,须先于目录清理退出
	const id = "it8613:hwmon8:fan2"
	pwm2 := filepath.Join(hwmon, "pwm2")
	pwm2Enable := filepath.Join(hwmon, "pwm2_enable")

	if err := m.SetFanDebugTakeover(id, true); err != nil {
		t.Fatal(err)
	}
	if err := m.SetFanDebugValue(id, 0, "percent"); err != nil {
		t.Fatal(err)
	}
	if got, _ := readInt(pwm2); got != 0 {
		t.Fatalf("debug value should drive the fan to pwm 0, got %d", got)
	}
	// 切到 rpm 单位再释放：单位与闭环状态必须清干净——残留的 rpm 会让下次
	// 接管把百分比当转速解释（闭环降速到近停转）
	if err := m.SetFanDebugValue(id, 1000, "rpm"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetFanDebugTakeover(id, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := readInt(pwm2); got != 100 {
		t.Fatalf("release must restore the captured PWM immediately, got %d", got)
	}
	if got, _ := readInt(pwm2Enable); got != 2 {
		t.Fatalf("release must restore the original pwm_enable mode (BIOS auto), got %d", got)
	}
	if _, taken := m.fanDebugTakenOver[id]; taken {
		t.Fatal("release must clear the takeover state")
	}
	if _, unitLeft := m.fanDebugUnits[id]; unitLeft {
		t.Fatalf("release must clear the unit, got %q", m.fanDebugUnits[id])
	}
	if _, missLeft := m.fanRPMLoopMiss[id]; missLeft {
		t.Fatal("release must clear the close-loop miss counter")
	}
	// 重新接管：单位必须重置为 percent（接管值的语义就是百分比）
	if err := m.SetFanDebugTakeover(id, true); err != nil {
		t.Fatal(err)
	}
	if m.fanDebugUnits[id] != "percent" {
		t.Fatalf("takeover must reset the unit to percent, got %q", m.fanDebugUnits[id])
	}
	if err := m.SetFanDebugTakeover(id, false); err != nil {
		t.Fatal(err)
	}
}

// 紧急满速兜底期间 RPM 闭环必须让位：不得把 100% 拉回目标转速（闭环保持
// 存活，兜底解除后自动恢复监控）。
func TestFanRPMLoopYieldsToEmergency(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon8")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	writeTestValue(t, filepath.Join(hwmon, "fan2_input"), "2000") // 实测远超目标,闭环本想降 PWM
	writeTestValue(t, filepath.Join(hwmon, "pwm2"), "255")        // 兜底刚写完满速
	writeTestValue(t, filepath.Join(hwmon, "pwm2_enable"), "1")
	m := &Manager{Root: root, StatePath: filepath.Join(root, "state.json")}
	t.Cleanup(m.stopFanRPMLearning)
	const id = "it8613:hwmon8:fan2"
	m.mu.Lock()
	m.fanDebugTakenOver = map[string]int{id: 1000}
	m.fanDebugUnits = map[string]string{id: "rpm"}
	m.fanDebugEmergency = true
	m.mu.Unlock()
	pwm2 := filepath.Join(hwmon, "pwm2")
	if alive := m.stepFanRPMCloseLoop(); !alive {
		t.Fatal("close loop must stay alive during emergency (targets still active)")
	}
	if got, _ := readInt(pwm2); got != 255 {
		t.Fatalf("emergency full-speed must not be pulled back by the close loop, got %d", got)
	}
}

// 兜底解除（CPU 温度回落）时把被接管风扇按各自调试值写回——percent/pwm
// 单位的风扇在兜底期间停在 100%，不写回会与调试显示脱节。
func TestFanDebugEmergencyReleaseRestoresDebugValue(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon8")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	writeTestValue(t, filepath.Join(hwmon, "fan2_input"), "2000")
	writeTestValue(t, filepath.Join(hwmon, "pwm2_enable"), "1")
	cpu := filepath.Join(root, "sys", "class", "hwmon", "hwmon9")
	if err := os.MkdirAll(cpu, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(cpu, "name"), "coretemp")
	writeTestValue(t, filepath.Join(cpu, "temp1_input"), "50000") // 50°C < 85 阈值 → 解除兜底
	m := &Manager{Root: root, StatePath: filepath.Join(root, "state.json")}
	const id = "it8613:hwmon8:fan2"
	m.mu.Lock()
	m.fanDebugTakenOver = map[string]int{id: 50} // percent
	m.fanDebugUnits = map[string]string{id: "percent"}
	m.fanDebugEmergency = true
	m.mu.Unlock()
	pwm2 := filepath.Join(hwmon, "pwm2")
	writeTestValue(t, pwm2, "255") // 兜底满速的最后写入
	if err := m.applyFanDebugEmergencyLocked(FanConfig{EmergencyTempC: 85}); err != nil {
		t.Fatal(err)
	}
	if got, _ := readInt(pwm2); got != int64(percentToPWM(50)) {
		t.Fatalf("emergency release must rewrite the debug value, want %d got %d", percentToPWM(50), got)
	}
	if m.fanDebugEmergency {
		t.Fatal("emergency flag must clear when temperature drops below the threshold")
	}
}

// RPM 闭环防震荡三件套的行为锁定：写入后隔轮观察（风扇转速惯性 2–5 秒
// 大于 tick 2 秒,基于过渡态连环调整是正负来回震荡的根源）、近端小步
// 细逼近、反向步长减半（过调回摆逐次收敛）。
func TestFanRPMLoopAntiOscillation(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon7")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	writeTestValue(t, filepath.Join(hwmon, "fan2_input"), "2000")
	writeTestValue(t, filepath.Join(hwmon, "pwm2"), "128")
	writeTestValue(t, filepath.Join(hwmon, "pwm2_enable"), "1")
	m := &Manager{Root: root, StatePath: filepath.Join(root, "state.json")}
	t.Cleanup(m.stopFanRPMLearning)
	const id = "it8613:hwmon7:fan2"
	m.mu.Lock()
	m.fanDebugTakenOver = map[string]int{id: 1600}
	m.fanDebugUnits = map[string]string{id: "rpm"}
	m.mu.Unlock()
	fan2Input := filepath.Join(hwmon, "fan2_input")
	pwm2 := filepath.Join(hwmon, "pwm2")

	// 远端第一步：目标 1600 实测 2000（容差 max(48,60)=60）→ adjust=-51 钳
	// -20 → 108,写入并进入隔轮观察
	m.stepFanRPMCloseLoop()
	if got, _ := readInt(pwm2); got != 108 {
		t.Fatalf("first step should write pwm=108, got %d", got)
	}
	if !m.fanRPMLoopSettling[id] {
		t.Fatal("adjustment must arm the settling round")
	}
	// 第二步（转速还没跟上,读数仍是 2000）：观察轮,不得再写
	m.stepFanRPMCloseLoop()
	if got, _ := readInt(pwm2); got != 108 {
		t.Fatalf("settling round must not adjust, pwm=%d", got)
	}
	if m.fanRPMLoopSettling[id] {
		t.Fatal("settling flag must clear after the observation round")
	}
	// 第三步（转速部分回落 1700,误差 100 < 2×容差 120）：近端小步,仅 -2
	writeTestValue(t, fan2Input, "1700")
	m.stepFanRPMCloseLoop()
	if got, _ := readInt(pwm2); got != 106 {
		t.Fatalf("near-target step must be capped at -2 (pwm 106), got %d", got)
	}
	// 第四步（转速进容差 1550）：达标锁定,miss 清零
	writeTestValue(t, fan2Input, "1550")
	m.stepFanRPMCloseLoop()
	if !m.fanRPMLoopLocked[id] || m.fanRPMLoopMiss[id] != 0 {
		t.Fatalf("within tolerance must lock and reset miss, locked=%v miss=%d", m.fanRPMLoopLocked[id], m.fanRPMLoopMiss[id])
	}

	// 反向减半：上一轮方向为 -1,现在读数骤降到 1000（大幅欠速）→ adjust
	// +76 钳 +20,与上轮反向 → 减半为 +10
	writeTestValue(t, fan2Input, "1000")
	writeTestValue(t, pwm2, "106")
	m.stepFanRPMCloseLoop()
	if got, _ := readInt(pwm2); got != 116 {
		t.Fatalf("reverse direction must halve the step (+10 → 116), got %d", got)
	}
	if m.fanRPMLoopLastDir[id] != 1 {
		t.Fatalf("direction bookkeeping should record +1, got %d", m.fanRPMLoopLastDir[id])
	}
}

// 递增间隔不设上限(旧实现限 1–120):>120 接受,0 报错,荒谬大值钳到
// 溢出护栏(百年)防 Duration 回绕。
func TestFanDebugAutoIntervalUnbounded(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon8")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	writeTestValue(t, filepath.Join(hwmon, "fan2_input"), "2000")
	writeTestValue(t, filepath.Join(hwmon, "pwm2"), "100")
	writeTestValue(t, filepath.Join(hwmon, "pwm2_enable"), "1")
	m := &Manager{Root: root, StatePath: filepath.Join(root, "state.json")}
	const id = "it8613:hwmon8:fan2"
	if err := m.SetFanDebugTakeover(id, true); err != nil {
		t.Fatal(err)
	}
	// 3600 秒(1 小时)合法
	if err := m.SetFanDebugAuto(id, true, 5, 3600, "percent"); err != nil {
		t.Fatalf("interval beyond the old 120s cap must be accepted: %v", err)
	}
	m.mu.Lock()
	got := m.fanDebugAuto.entries[id].Interval
	m.mu.Unlock()
	if got != 3600 {
		t.Fatalf("interval should be stored verbatim, got %d", got)
	}
	// 0 报错
	if err := m.SetFanDebugAuto(id, true, 5, 0, "percent"); err == nil {
		t.Fatal("interval 0 must be rejected")
	}
	// 荒谬大值钳到溢出护栏
	if err := m.SetFanDebugAuto(id, true, 5, 9999999999, "percent"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	got = m.fanDebugAuto.entries[id].Interval
	m.mu.Unlock()
	if got != fanAutoIntervalMax {
		t.Fatalf("absurd interval must clamp to the overflow guard, got %d", got)
	}
}

// 自动测试运行中锁定参数:调试值与主动标定都拒绝(前端禁用控件之外的
// 后端兜底——直连 API 的改动会破坏递增进程)。
func TestFanDebugAutoRunningLocksParams(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon8")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	writeTestValue(t, filepath.Join(hwmon, "fan2_input"), "2000")
	writeTestValue(t, filepath.Join(hwmon, "pwm2"), "100")
	writeTestValue(t, filepath.Join(hwmon, "pwm2_enable"), "1")
	m := &Manager{Root: root, StatePath: filepath.Join(root, "state.json")}
	const id = "it8613:hwmon8:fan2"
	if err := m.SetFanDebugTakeover(id, true); err != nil {
		t.Fatal(err)
	}
	if err := m.SetFanDebugAuto(id, true, 5, 10, "percent"); err != nil {
		t.Fatal(err)
	}
	// 标定必然破坏递增(写 255 全速):运行中拒绝
	if _, err := m.CalibrateFanRPM(id); err == nil || !strings.Contains(err.Error(), "自动测试进行中") {
		t.Fatalf("calibration must be rejected while auto test runs, got %v", err)
	}
	// 调试值接口运行中不拒:单位切换的等比换算是无损操作(见
	// TestFanDebugUnitSwitchMidRamp),锁定由前端禁用控件承担
	if err := m.SetFanDebugValue(id, 50, "percent"); err != nil {
		t.Fatalf("unit-resync path must stay open mid-ramp, got %v", err)
	}
}

// ---- 回归:闭环顶格冻结(目标超出风扇能力时不再回撤试探) ----

// 目标转速高于实际可达:PWM 顶到 255 后闭环必须冻结(保持全速、miss 拉满),
// 不得因读数噪声"降档再试"形成 98↔90 振荡;读数偶发进容差可解冻;
// 真超速(读数>目标)仍允许下调。
func TestFanRPMCloseLoopStopsAtFullScale(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon8")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	writeTestValue(t, filepath.Join(hwmon, "fan2_input"), "2600")
	writeTestValue(t, filepath.Join(hwmon, "pwm2"), "255")
	writeTestValue(t, filepath.Join(hwmon, "pwm2_enable"), "1")
	m := &Manager{Root: root, StatePath: filepath.Join(root, "state.json")}
	const id = "it8613:hwmon8:fan2"
	m.mu.Lock()
	m.fanDebugTakenOver = map[string]int{id: 3000} // 目标 3000,实际全速 ~2600,容差 90
	m.fanDebugUnits = map[string]string{id: "rpm"}
	m.mu.Unlock()
	pwm2 := filepath.Join(hwmon, "pwm2")
	fan2 := filepath.Join(hwmon, "fan2_input")

	// 顶格欠速:一步进入冻结,PWM 保持 255
	if !m.stepFanRPMCloseLoop() {
		t.Fatal("frozen fan is still an active target")
	}
	if got, _ := readInt(pwm2); got != 255 {
		t.Fatalf("full-scale PWM must be held, got %d", got)
	}
	m.mu.Lock()
	miss, locked := m.fanRPMLoopMiss[id], m.fanRPMLoopLocked[id]
	m.mu.Unlock()
	if miss != fanRPMCloseLoopMaxMiss || !locked {
		t.Fatalf("full-scale undershoot must freeze immediately, miss=%d locked=%v", miss, locked)
	}
	// 冻结中读数继续欠速:不写不动
	writeTestValue(t, fan2, "2580")
	m.stepFanRPMCloseLoop()
	if got, _ := readInt(pwm2); got != 255 {
		t.Fatalf("frozen loop must not touch PWM, got %d", got)
	}
	// 读数偶发进容差:解冻(miss 清零)
	writeTestValue(t, fan2, "3010")
	m.stepFanRPMCloseLoop()
	m.mu.Lock()
	miss = m.fanRPMLoopMiss[id]
	m.mu.Unlock()
	if miss != 0 {
		t.Fatalf("in-tolerance reading should unfreeze, miss=%d", miss)
	}
	// 真超速(读数 > 目标+容差):允许下调,不冻结
	writeTestValue(t, fan2, "3400")
	if !m.stepFanRPMCloseLoop() {
		t.Fatal("overshoot should keep the loop active for correction")
	}
	if got, _ := readInt(pwm2); got != 235 {
		t.Fatalf("overshoot must step PWM down by max adjustment, got %d", got)
	}
}

// 自动递增到单位上限:Done 且 PWM 顶格(既有行为,防回归)。
func TestFanDebugAutoRampDoneAtFullScale(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon8")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	writeTestValue(t, filepath.Join(hwmon, "fan2_input"), "2000")
	writeTestValue(t, filepath.Join(hwmon, "pwm2"), "90")
	writeTestValue(t, filepath.Join(hwmon, "pwm2_enable"), "1")
	m := &Manager{Root: root, StatePath: filepath.Join(root, "state.json")}
	const id = "it8613:hwmon8:fan2"
	if err := m.SetFanDebugTakeover(id, true); err != nil {
		t.Fatal(err)
	}
	// 从 95 起步、步进 10、间隔 1 秒:一波即到 100 上限
	if err := m.SetFanDebugValue(id, 95, "percent"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetFanDebugAuto(id, true, 10, 1, "percent"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var entry fanDebugAutoEntry
	for {
		time.Sleep(100 * time.Millisecond)
		m.mu.Lock()
		entry = m.fanDebugAuto.entries[id]
		m.mu.Unlock()
		if entry.Done || time.Now().After(deadline) {
			break
		}
	}
	pwm2 := filepath.Join(hwmon, "pwm2")
	if got, _ := readInt(pwm2); got != 255 {
		t.Fatalf("ramp to 100%% should write full-scale PWM, got %d", got)
	}
	if !entry.Done || entry.Running {
		t.Fatalf("ramp at unit max should be done, got %+v", entry)
	}
}

// 卸载 / 重装：release-fans 必须把风扇写回接管前的 BIOS 自动模式，
// 同时把配置里的风扇控制关掉——否则重装后服务一启动又会抢回控制权。
func TestReleaseFansHandsControlBackToBIOS(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	pwm1 := filepath.Join(hwmon, "pwm1")
	enable1 := filepath.Join(hwmon, "pwm1_enable")

	// 接管前存档：BIOS 自动模式(2) + PWM 102
	state := originalFanState{
		Version:    fanStateVersion,
		CapturedAt: time.Now(),
		Fans:       []originalFan{{ID: "it8613:hwmon3:fan1", PWM: 102, Mode: 2}},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.fanStatePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}

	// 当前是模块接管中：手动模式 + 自定义 PWM
	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	if err := manager.SaveFanConfig(cfg); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, enable1, "1")
	writeTestValue(t, pwm1, "200")

	if err := manager.ReleaseFans(); err != nil {
		t.Fatalf("release fans: %v", err)
	}

	// 硬件：pwm_enable 回到 BIOS 自动档，PWM 回到接管前的值
	if got, _ := readInt(enable1); got != 2 {
		t.Fatalf("pwm1_enable=%d, want 2 (BIOS auto)", got)
	}
	if got, _ := readInt(pwm1); got != 102 {
		t.Fatalf("pwm1=%d, want 102 (captured original)", got)
	}

	// 配置：风扇控制落盘为关闭，重装后不会自动接管
	raw, err := os.ReadFile(manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Fan.Enabled {
		t.Fatal("fan control must be disabled on disk after release")
	}
	if len(saved.Fan.CPUFanIDs) != 1 || saved.Fan.CPUFanIDs[0] != "it8613:hwmon3:fan1" {
		t.Fatalf("release must not throw away fan bindings: %+v", saved.Fan.CPUFanIDs)
	}
}

// 没有接管过（没有原始状态存档）时 release 也要安全：配置关掉、不报错。
func TestReleaseFansWithoutCapturedState(t *testing.T) {
	manager := newFanDebugTestManager(t)
	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	if err := manager.SaveFanConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReleaseFans(); err != nil {
		t.Fatalf("release without captured state should be a no-op, got %v", err)
	}
	raw, err := os.ReadFile(manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Fan.Enabled {
		t.Fatal("fan control must be disabled even without a captured state")
	}
}

// 全新安装：三条曲线都要绑到所有有转速反馈的风扇、打开风扇控制、用平衡档。
func TestBindAllFansDefaultEnablesEveryCurve(t *testing.T) {
	manager := newFanDebugTestManager(t)
	if err := manager.BindAllFansDefault(); err != nil {
		t.Fatalf("bind all fans: %v", err)
	}
	raw, err := os.ReadFile(manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Fan.Enabled {
		t.Fatal("fresh install should enable fan control by default")
	}
	// fixture 里 fan1 有 1200 RPM、fan2 是 0 RPM（未接），只绑有反馈的那个
	for name, ids := range map[string][]string{"cpu": saved.Fan.CPUFanIDs, "hdd": saved.Fan.HDDFanIDs, "nvme": saved.Fan.NVMeFanIDs} {
		if len(ids) != 1 || ids[0] != "it8613:hwmon3:fan1" {
			t.Fatalf("%s fan ids = %v, want [it8613:hwmon3:fan1]", name, ids)
		}
	}
	if len(saved.Fan.HDDSlotIDs) != 6 || len(saved.Fan.NVMeSlotIDs) != 4 {
		t.Fatalf("all storage slots must be bound: HDD=%v NVMe=%v", saved.Fan.HDDSlotIDs, saved.Fan.NVMeSlotIDs)
	}
	balanced := builtinFanCurves["balanced"]
	if !fanPointsEqual(saved.Fan.Curve, clampCurvePWM(balanced.CPU, saved.Fan.MinPWMPercent)) {
		t.Fatalf("fresh install must use the balanced CPU curve: %+v", saved.Fan.Curve)
	}
}

// 关停档：曲线允许整条 0%（最低转速 40% 让位），下发时确实写 0。
func TestOffModeAllowsZeroPWMAndWritesZero(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	pwm1 := filepath.Join(hwmon, "pwm1")

	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.OffMode = true
	cfg.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	for i := range cfg.Curve {
		cfg.Curve[i].PWMPercent = 0
	}
	for i := range cfg.HDDCurve {
		cfg.HDDCurve[i].PWMPercent = 0
	}
	for i := range cfg.NVMeCurve {
		cfg.NVMeCurve[i].PWMPercent = 0
	}
	if err := manager.SaveFanConfig(cfg); err != nil {
		t.Fatalf("关停档的 0%% 曲线必须被接受: %v", err)
	}
	if got, _ := readInt(pwm1); got != 0 {
		t.Fatalf("关停档应写 0，实际 pwm1=%d", got)
	}

	// 落盘的 off_mode 要在，重启后仍按关停跑
	raw, err := os.ReadFile(manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Fan.OffMode {
		t.Fatal("off_mode 必须落盘，否则重启就退出关停档")
	}
}

// 不开关停档时，同样一条 0% 曲线仍要被最低转速拦下。
func TestZeroPWMWithoutOffModeRejected(t *testing.T) {
	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	for i := range cfg.Curve {
		cfg.Curve[i].PWMPercent = 0
	}
	if err := (&Manager{}).validateFanLocked(&cfg); err == nil {
		t.Fatal("非关停档的 0% 曲线必须被拒绝")
	}
}

// 维修安全策略：关停档是用户手动开启的维修/检修状态（两重确认+顶部常驻警告），
// 超高温度下风扇也绝不能起转——突然起转可能伤到正在操作的人——保持停转，改用
// 蜂鸣器急促报警 + 界面错误状态；保存配置本身必须成功（否则高温设备开不了关停档）。
func TestOffModeEmergencyKeepsFansStoppedAndAlarms(t *testing.T) {
	manager := newFanDebugTestManager(t)
	coretemp := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon9")
	writeTestValue(t, filepath.Join(coretemp, "temp1_input"), "90000")
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	pwm1 := filepath.Join(hwmon, "pwm1")
	writeTestValue(t, pwm1, "10")

	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.OffMode = true
	cfg.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	for i := range cfg.Curve {
		cfg.Curve[i].PWMPercent = 0
	}
	for i := range cfg.HDDCurve {
		cfg.HDDCurve[i].PWMPercent = 0
	}
	for i := range cfg.NVMeCurve {
		cfg.NVMeCurve[i].PWMPercent = 0
	}
	if err := manager.SaveFanConfig(cfg); err != nil {
		t.Fatalf("关停档超温是正当维修场景，保存必须成功: %v", err)
	}
	if got, _ := readInt(pwm1); got != 0 {
		t.Fatalf("维修安全策略：紧急温度下风扇必须保持停转，实际 pwm1=%d", got)
	}
	manager.mu.Lock()
	alarm := manager.fanLastError
	manager.mu.Unlock()
	if !strings.Contains(alarm, "紧急温度") || !strings.Contains(alarm, "维修安全策略") {
		t.Fatalf("必须保持紧急温度报警状态，实际 %q", alarm)
	}

	// 温度回落：报警清除，风扇保持停转。
	writeTestValue(t, filepath.Join(coretemp, "temp1_input"), "50000")
	if err := manager.ApplyFanCurrent(); err != nil {
		t.Fatalf("温度回落后不应再报警: %v", err)
	}
	manager.mu.Lock()
	alarm = manager.fanLastError
	manager.mu.Unlock()
	if alarm != "" {
		t.Fatalf("温度回落后报警应清除，实际 %q", alarm)
	}
	if got, _ := readInt(pwm1); got != 0 {
		t.Fatalf("关停档风扇应保持停转，实际 pwm1=%d", got)
	}
}

// 关停档真的把风扇停住之后，转速反馈变 0 是预期结果：不能再被「无有效转速」判死。
// 否则周期性应用会走 fail-safe 把风扇拉满，表现就是「关停之后隔一会儿又起转」。
func TestOffModeKeepsWorkingWhenFanReportsZeroRPM(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	pwm1 := filepath.Join(hwmon, "pwm1")
	writeTestValue(t, filepath.Join(hwmon, "fan1_input"), "0") // 停转后的真实反馈

	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.OffMode = true
	cfg.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	for i := range cfg.Curve {
		cfg.Curve[i].PWMPercent = 0
	}
	for i := range cfg.HDDCurve {
		cfg.HDDCurve[i].PWMPercent = 0
	}
	for i := range cfg.NVMeCurve {
		cfg.NVMeCurve[i].PWMPercent = 0
	}
	if err := manager.SaveFanConfig(cfg); err != nil {
		t.Fatalf("关停档不该因为 0 转被判死: %v", err)
	}
	if got, _ := readInt(pwm1); got != 0 {
		t.Fatalf("关停档应写 0，实际 pwm1=%d", got)
	}
	// 周期性应用同样不能失败：失败会触发 fail-safe 把风扇拉满
	if err := manager.ApplyFanCurrent(); err != nil {
		t.Fatalf("周期性应用不能失败（会 fail-safe 拉满）: %v", err)
	}
	if got, _ := readInt(pwm1); got != 0 {
		t.Fatalf("周期性应用之后仍应保持 0，实际 pwm1=%d", got)
	}

	status := manager.fanStatusLocked(cfg)
	if !status.OffMode {
		t.Fatal("状态里要带 off_mode，前端才能把 0 转说成关停而不是接线故障")
	}
	if status.TargetPWMPercent != 0 {
		t.Fatalf("关停档的目标转速应是 0%%，实际 %d%%", status.TargetPWMPercent)
	}
	if status.CPUTargetPWMPercent != 0 {
		t.Fatalf("关停档的 CPU 目标应是 0%%，实际 %d%%", status.CPUTargetPWMPercent)
	}
	if status.ControlSource != "" {
		t.Fatalf("关停档不该报控制来源，实际 %q", status.ControlSource)
	}
}

// 反过来：不开「关停」时，0 转的风扇仍然要被拦住（不能因为上面的放宽把保护拆掉）。
func TestZeroRPMFanStillRejectedWithoutOffMode(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	writeTestValue(t, filepath.Join(hwmon, "fan1_input"), "0")

	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	// 非关停档下选中的风扇 0 转，保存时自动剔除而不是报错——用户反插接口后
	// 旧通道不再阻塞配置保存。但全部剔除后没有可用风扇时，必须报错。
	if err := (&Manager{Root: manager.Root}).validateFanLocked(&cfg); err == nil {
		t.Fatal("所有选中风扇都 0 转时必须报错")
	}
}

// 所有选中的风扇都 0 转时，保存必须报错——不能静默保存一个「没有可用风扇」的配置。
func TestAllSelectedFansZeroRPMRejected(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	writeTestValue(t, filepath.Join(hwmon, "fan1_input"), "0")

	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	if err := (&Manager{Root: manager.Root}).validateFanLocked(&cfg); err == nil {
		t.Fatal("所有选中风扇都 0 转时必须报错")
	}
}

// 部分风扇 0 转、部分正常时，只剔除 0 转的，保留正常的。
func TestPartialZeroRPMFanFiltered(t *testing.T) {
	manager := newFanDebugTestManager(t)

	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.CPUFanIDs = []string{"it8613:hwmon3:fan1", "it8613:hwmon3:fan2"}
	if err := (&Manager{Root: manager.Root}).validateFanLocked(&cfg); err != nil {
		t.Fatalf("部分风扇 0 转时应自动剔除而非报错: %v", err)
	}
	if len(cfg.CPUFanIDs) != 1 || cfg.CPUFanIDs[0] != "it8613:hwmon3:fan1" {
		t.Fatalf("应只保留有转速的风扇，实际 %+v", cfg.CPUFanIDs)
	}
}

// 关停把风扇停住（RPM=0）之后切回正常档，必须能重新起转：
// 保存不能因为「此刻 0 转」被拒（否则永远回不去），下发要真的把 PWM 拉起来，
// 起转那几秒里周期性下发也不能判失败走 fail-safe。
func TestLeavingOffModeSpinsFansBackUp(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	pwm1 := filepath.Join(hwmon, "pwm1")
	fan1 := filepath.Join(hwmon, "fan1_input")

	off := DefaultFanConfig()
	off.Enabled = true
	off.OffMode = true
	off.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	for i := range off.Curve {
		off.Curve[i].PWMPercent = 0
	}
	for i := range off.HDDCurve {
		off.HDDCurve[i].PWMPercent = 0
	}
	for i := range off.NVMeCurve {
		off.NVMeCurve[i].PWMPercent = 0
	}
	if err := manager.SaveFanConfig(off); err != nil {
		t.Fatalf("save off: %v", err)
	}
	writeTestValue(t, fan1, "0") // 风扇真的停住了

	on := DefaultFanConfig()
	on.Enabled = true
	on.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	for i := range on.Curve {
		on.Curve[i].PWMPercent = 100
	}
	if err := manager.SaveFanConfig(on); err != nil {
		t.Fatalf("关停之后切回正常档必须能保存: %v", err)
	}
	if got, _ := readInt(pwm1); got != 255 {
		t.Fatalf("切回正常档要把 PWM 拉满，实际 pwm1=%d", got)
	}

	// 风扇还在起转、转速仍是 0 的这段时间，周期性下发不能失败（失败会 fail-safe）
	if err := manager.ApplyFanCurrent(); err != nil {
		t.Fatalf("起转过渡期的周期性下发不能失败: %v", err)
	}
	if got, _ := readInt(pwm1); got != 255 {
		t.Fatalf("周期性下发应保持满速，实际 pwm1=%d", got)
	}

	// 转速回来后一切照旧
	writeTestValue(t, fan1, "1900")
	if err := manager.ApplyFanCurrent(); err != nil {
		t.Fatalf("转速恢复后下发仍应成功: %v", err)
	}
}

// 关停档最长 5 分钟：到期后周期性下发必须自动退出关停、恢复关停前曲线并起转，
// 以免检修结束忘记退出导致硬盘/NVMe 长时间无风散热。
func TestOffModeAutoResumesAfterFiveMinutes(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	pwm1 := filepath.Join(hwmon, "pwm1")
	fan1 := filepath.Join(hwmon, "fan1_input")

	base := DefaultFanConfig()
	base.Enabled = true
	base.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	for i := range base.Curve {
		base.Curve[i].PWMPercent = 100
	}
	if err := manager.SaveFanConfig(base); err != nil {
		t.Fatalf("save base: %v", err)
	}

	off := DefaultFanConfig()
	off.Enabled = true
	off.OffMode = true
	off.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	for i := range off.Curve {
		off.Curve[i].PWMPercent = 0
	}
	for i := range off.HDDCurve {
		off.HDDCurve[i].PWMPercent = 0
	}
	for i := range off.NVMeCurve {
		off.NVMeCurve[i].PWMPercent = 0
	}
	if err := manager.SaveFanConfig(off); err != nil {
		t.Fatalf("save off: %v", err)
	}
	writeTestValue(t, fan1, "0") // 关停后风扇真的停住了

	// 关停期间界面再次保存（不带快照）：服务端必须沿用关停前的曲线快照
	again := off
	again.OffModeBackup = nil
	if err := manager.SaveFanConfig(again); err != nil {
		t.Fatalf("关停期间再次保存: %v", err)
	}

	manager.mu.Lock()
	saved, err := manager.loadConfigLocked()
	manager.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Fan.OffModeBackup == nil || len(saved.Fan.OffModeBackup.Curve) == 0 {
		t.Fatal("进入关停档必须留下关停前的曲线快照")
	}
	if got := saved.Fan.OffModeBackup.Curve[0].PWMPercent; got != 100 {
		t.Fatalf("快照应保存关停前的 100%% 曲线，实际 %d", got)
	}

	// 未到 5 分钟：保持关停
	if err := manager.ApplyFanCurrent(); err != nil {
		t.Fatalf("关停未到期时下发不应失败: %v", err)
	}
	if got, _ := readInt(pwm1); got != 0 {
		t.Fatalf("关停未到期应保持 0，实际 pwm1=%d", got)
	}

	// 把进入时刻拨到 5 分钟上限之前：模拟检修结束、关停超时。
	manager.mu.Lock()
	manager.fanOffModeSince = time.Now().Add(-fanOffModeMaxDuration - time.Second)
	manager.mu.Unlock()

	if err := manager.ApplyFanCurrent(); err != nil {
		t.Fatalf("关停到期后应自动恢复曲线并起转: %v", err)
	}
	manager.mu.Lock()
	saved, err = manager.loadConfigLocked()
	manager.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Fan.OffMode {
		t.Fatal("到期后 off_mode 必须落盘为 false")
	}
	if saved.Fan.OffModeBackup != nil {
		t.Fatal("恢复原曲线后快照必须清掉")
	}
	if got := saved.Fan.Curve[0].PWMPercent; got != 100 {
		t.Fatalf("到期后应恢复关停前的 100%% 曲线，实际 %d", got)
	}
	if got, _ := readInt(pwm1); got != 255 {
		t.Fatalf("到期后风扇应重新起转，实际 pwm1=%d", got)
	}
}

// 手动退出关停（界面回传恢复后的曲线）时快照必须清掉，且不得覆盖界面显式提交的曲线。
func TestOffModeBackupClearedOnManualExit(t *testing.T) {
	manager := newFanDebugTestManager(t)

	base := DefaultFanConfig()
	base.Enabled = true
	base.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	if err := manager.SaveFanConfig(base); err != nil {
		t.Fatalf("save base: %v", err)
	}
	off := DefaultFanConfig()
	off.Enabled = true
	off.OffMode = true
	off.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	for i := range off.Curve {
		off.Curve[i].PWMPercent = 0
	}
	for i := range off.HDDCurve {
		off.HDDCurve[i].PWMPercent = 0
	}
	for i := range off.NVMeCurve {
		off.NVMeCurve[i].PWMPercent = 0
	}
	if err := manager.SaveFanConfig(off); err != nil {
		t.Fatalf("save off: %v", err)
	}

	back := DefaultFanConfig()
	back.Enabled = true
	back.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	for i := range back.Curve {
		back.Curve[i].PWMPercent = 80
	}
	if err := manager.SaveFanConfig(back); err != nil {
		t.Fatalf("手动退出关停必须能保存: %v", err)
	}

	manager.mu.Lock()
	saved, err := manager.loadConfigLocked()
	manager.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Fan.OffMode || saved.Fan.OffModeBackup != nil {
		t.Fatalf("退出关停后 off_mode/快照都必须清掉: %+v", saved.Fan)
	}
	if got := saved.Fan.Curve[0].PWMPercent; got != 80 {
		t.Fatalf("手动退出应保留界面回传的曲线，实际 %d", got)
	}
}

// 起转宽限窗口不是永久豁免：离开关停档 5 分钟后仍然 0 转的通道必须重新按故障
// 处理（下发 fail-safe 满速、保存被拒）。旧的「见过一次转速就永久免检」会让
// 拔掉/损坏的风扇永远不被发现。
func TestZeroRPMRejectedAfterSpinUpGraceExpires(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	pwm1 := filepath.Join(hwmon, "pwm1")
	fan1 := filepath.Join(hwmon, "fan1_input")

	off := DefaultFanConfig()
	off.Enabled = true
	off.OffMode = true
	off.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	for i := range off.Curve {
		off.Curve[i].PWMPercent = 0
	}
	for i := range off.HDDCurve {
		off.HDDCurve[i].PWMPercent = 0
	}
	for i := range off.NVMeCurve {
		off.NVMeCurve[i].PWMPercent = 0
	}
	if err := manager.SaveFanConfig(off); err != nil {
		t.Fatalf("save off: %v", err)
	}
	writeTestValue(t, fan1, "0") // 关停把风扇停住了

	on := DefaultFanConfig()
	on.Enabled = true
	on.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	if err := manager.SaveFanConfig(on); err != nil {
		t.Fatalf("宽限期内切回正常档必须能保存: %v", err)
	}

	// 把宽限期拨到 5 分钟之前：模拟用户离开关停档很久之后风扇仍然没有转起来。
	fixed := time.Now()
	manager.mu.Lock()
	manager.nowFn = func() time.Time { return fixed }
	manager.fanOffExitedAt = fixed.Add(-fanSpinUpGraceWindow - time.Second)
	manager.mu.Unlock()

	// 宽限期过后仍然 0 转：这是唯一选中的风扇，全部停转 = 真故障，运行时必须
	// 保持错误状态报警（但不 fail-safe 拉满——往空通道写 100% 无意义）。
	beforeApply, _ := readInt(pwm1)
	if err := manager.ApplyFanCurrent(); err == nil {
		t.Fatal("宽限期过后所有选中风扇 0 转必须报错报警")
	}
	manager.mu.Lock()
	alarm := manager.fanLastError
	manager.mu.Unlock()
	if alarm == "" {
		t.Fatal("全部停转时 fanLastError 必须保持报警状态")
	}
	// 风扇保持原样（不 fail-safe 拉满）
	if got, _ := readInt(pwm1); got != beforeApply {
		t.Fatalf("0 转风扇不应被拉满，pwm1 从 %d 被改写为 %d", beforeApply, got)
	}
	// 保存时重新构建配置（上次的 on 已被修改）
	on2 := DefaultFanConfig()
	on2.Enabled = true
	on2.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	if err := manager.SaveFanConfig(on2); err == nil {
		t.Fatal("宽限期过后保存应报错（全部剔除后无可用风扇）")
	}
}

// 起转宽限的边界：31 秒、4 分 59 秒仍算「起转中」，0 转速不报警；正好 5 分钟
// 与 5 分 1 秒都恢复严格检查。时钟通过 manager.nowFn 注入，断言不依赖真实等待。
func TestSpinUpGraceBoundary(t *testing.T) {
	cases := []struct {
		name    string
		elapsed time.Duration
		wantErr bool
	}{
		{name: "31 秒仍在宽限", elapsed: 31 * time.Second},
		{name: "4 分 59 秒仍在宽限", elapsed: fanSpinUpGraceWindow - time.Second},
		{name: "正好 5 分钟恢复检查", elapsed: fanSpinUpGraceWindow, wantErr: true},
		{name: "5 分 1 秒恢复检查", elapsed: fanSpinUpGraceWindow + time.Second, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager := newFanDebugTestManager(t)
			hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
			fan1 := filepath.Join(hwmon, "fan1_input")

			off := DefaultFanConfig()
			off.Enabled = true
			off.OffMode = true
			off.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
			for i := range off.Curve {
				off.Curve[i].PWMPercent = 0
			}
			for i := range off.HDDCurve {
				off.HDDCurve[i].PWMPercent = 0
			}
			for i := range off.NVMeCurve {
				off.NVMeCurve[i].PWMPercent = 0
			}
			if err := manager.SaveFanConfig(off); err != nil {
				t.Fatalf("save off: %v", err)
			}
			writeTestValue(t, fan1, "0") // 关停把风扇停住了

			on := DefaultFanConfig()
			on.Enabled = true
			on.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
			if err := manager.SaveFanConfig(on); err != nil {
				t.Fatalf("save on: %v", err)
			}

			// 注入固定时钟并直接拨到目标边界，模拟关停退出后已经过去的时间。
			fixed := time.Now()
			manager.mu.Lock()
			manager.nowFn = func() time.Time { return fixed }
			manager.fanOffExitedAt = fixed.Add(-tc.elapsed)
			manager.fanOffWasActive = false
			manager.mu.Unlock()

			err := manager.ApplyFanCurrent()
			manager.mu.Lock()
			alarm := manager.fanLastError
			manager.mu.Unlock()
			if tc.wantErr {
				if err == nil || alarm == "" {
					t.Fatalf("超出宽限后全部风扇 0 转必须报错并保持报警: err=%v alarm=%q", err, alarm)
				}
				return
			}
			if err != nil {
				t.Fatalf("宽限期内 0 转速不应报错: %v", err)
			}
			if alarm != "" {
				t.Fatalf("宽限期内不应留下风扇错误: %q", alarm)
			}
		})
	}
}

// 宽限期从「关停 → 普通」真正生效的那一刻起算：之后重复保存普通配置不能重置
// 计时，否则反复点保存就能无限延长 0 转豁免。
func TestRepeatedNormalSavesDoNotResetSpinUpGrace(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	fan1 := filepath.Join(hwmon, "fan1_input")

	off := DefaultFanConfig()
	off.Enabled = true
	off.OffMode = true
	off.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	for i := range off.Curve {
		off.Curve[i].PWMPercent = 0
	}
	for i := range off.HDDCurve {
		off.HDDCurve[i].PWMPercent = 0
	}
	for i := range off.NVMeCurve {
		off.NVMeCurve[i].PWMPercent = 0
	}
	if err := manager.SaveFanConfig(off); err != nil {
		t.Fatalf("save off: %v", err)
	}
	writeTestValue(t, fan1, "0")

	on := DefaultFanConfig()
	on.Enabled = true
	on.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	if err := manager.SaveFanConfig(on); err != nil {
		t.Fatalf("save on: %v", err)
	}

	fixed := time.Now()
	want := fixed.Add(-4 * time.Minute)
	manager.mu.Lock()
	manager.nowFn = func() time.Time { return fixed }
	manager.fanOffExitedAt = want
	manager.mu.Unlock()

	for i := 0; i < 3; i++ {
		next := DefaultFanConfig()
		next.Enabled = true
		next.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
		if err := manager.SaveFanConfig(next); err != nil {
			t.Fatalf("第 %d 次重复保存应仍在宽限期内: %v", i+1, err)
		}
	}
	manager.mu.Lock()
	exitedAt := manager.fanOffExitedAt
	manager.mu.Unlock()
	if !exitedAt.Equal(want) {
		t.Fatalf("重复保存不得重置起转宽限计时: got %v want %v", exitedAt, want)
	}
}

// 被拒绝的关停配置不得留下假的关停状态：校验在任何运行状态修改之前完成，
// 保存失败时 fanOffWasActive / fanOffExitedAt 都必须保持原样——否则下一轮普通
// 模式会误以为「刚退出关停」而开启 5 分钟起转宽限，把真正的 0 转通道当成起转中
// 的风扇重新下发曲线目标，故障风扇就此被豁免。
func TestRejectedOffModeConfigLeavesNoPhantomTransition(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	pwm1 := filepath.Join(hwmon, "pwm1")
	fan1 := filepath.Join(hwmon, "fan1_input")

	// 先保存一份正常配置：fan1 受曲线控制（45°C → 47%）。
	base := DefaultFanConfig()
	base.Enabled = true
	base.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	if err := manager.SaveFanConfig(base); err != nil {
		t.Fatalf("save base: %v", err)
	}

	// 提交无效配置（关停档 + 非法 MinPWMPercent=29）：保存必须失败，
	// 且不得留下「关停档曾生效」的假状态。
	bad := DefaultFanConfig()
	bad.Enabled = true
	bad.OffMode = true
	bad.MinPWMPercent = 29
	bad.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	if err := manager.SaveFanConfig(bad); err == nil {
		t.Fatal("MinPWMPercent=29 的关停配置必须被拒绝")
	}
	manager.mu.Lock()
	wasActive := manager.fanOffWasActive
	exitedAt := manager.fanOffExitedAt
	manager.mu.Unlock()
	if wasActive {
		t.Fatal("被拒绝的关停配置不得把 fanOffWasActive 置为 true")
	}
	if !exitedAt.IsZero() {
		t.Fatalf("被拒绝的关停配置不得留下关停退出时刻: %v", exitedAt)
	}
	// 磁盘上的配置也仍是旧的正常配置（关停档未生效、未落盘）。
	manager.mu.Lock()
	onDisk, err := manager.loadConfigLocked()
	manager.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Fan.OffMode || onDisk.Fan.MinPWMPercent != base.MinPWMPercent {
		t.Fatalf("被拒绝的配置不得落盘: %+v", onDisk.Fan)
	}

	// 风扇此刻 0 转：不存在假的关停宽限时它是唯一选中风扇，全部停转按故障报警
	// （返回错误、fanLastError 保持），绝不能把它当成「起转中」重新下发曲线目标。
	// 先把 pwm1 拨成一个曲线算不出来的值，真假宽限一试便知。
	writeTestValue(t, fan1, "0")
	writeTestValue(t, pwm1, "30")
	if err := manager.ApplyFanCurrent(); err == nil {
		t.Fatal("所有选中风扇 0 转必须报错报警")
	}
	if got, _ := readInt(pwm1); got != 30 {
		t.Fatalf("0 转风扇不应被改写，pwm1 从 30 变为 %d（假的关停宽限让它被当成起转中的风扇）", got)
	}
	manager.mu.Lock()
	exitedAt = manager.fanOffExitedAt
	manager.mu.Unlock()
	if !exitedAt.IsZero() {
		t.Fatalf("宽限状态不得因被拒绝的配置而改变: %v", exitedAt)
	}
}

// 两个风扇接口全部停转 = 真故障（卡死/掉线/测速线坏）：运行时必须保持错误
// 状态报警，绝不能清掉错误让界面看起来一切正常；但不 fail-safe 拉满（往空
// 通道写 100% 无意义，且真故障时该通道也已失控）。手动关停档维修场景走不到
// 这里：关停档跳过 0 转检查，由 TestOffModeKeepsWorkingWhenFanReportsZeroRPM
// 守住。风扇恢复后报警由成功路径清除。
func TestAllFansZeroRPMAlarmsAtRuntime(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	pwm1 := filepath.Join(hwmon, "pwm1")
	fan1 := filepath.Join(hwmon, "fan1_input")

	// 选中两个接口保存：fan2 未接（0 转）被自动剔除，fan1 正常受控。
	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.CPUFanIDs = []string{"it8613:hwmon3:fan1", "it8613:hwmon3:fan2"}
	if err := manager.SaveFanConfig(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	// fan1 也停转：两个接口全部 0 转 = 真故障，必须报错并保持报警状态。
	writeTestValue(t, fan1, "0")
	if err := manager.ApplyFanCurrent(); err == nil {
		t.Fatal("所有选中风扇 0 转必须报错报警")
	}
	manager.mu.Lock()
	alarm := manager.fanLastError
	manager.mu.Unlock()
	if !strings.Contains(alarm, "所有选中的风扇都读不到转速") {
		t.Fatalf("fanLastError 应保持全部停转报警，实际 %q", alarm)
	}
	if got, _ := readInt(pwm1); got == 255 {
		t.Fatal("不做 fail-safe 拉满，pwm1 不应被写成 100%")
	}

	// 风扇恢复：报警清除，曲线控制恢复。
	writeTestValue(t, fan1, "1900")
	if err := manager.ApplyFanCurrent(); err != nil {
		t.Fatalf("风扇恢复后应正常受控: %v", err)
	}
	manager.mu.Lock()
	alarm = manager.fanLastError
	manager.mu.Unlock()
	if alarm != "" {
		t.Fatalf("风扇恢复后报警应清除，实际 %q", alarm)
	}
}

// offModeZeroCurveConfig 关停档用的全 0% 曲线配置：关停档不看曲线，但保存校验
// 仍要求曲线本身合法。
func offModeZeroCurveConfig(fanIDs ...string) FanConfig {
	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.OffMode = true
	cfg.CPUFanIDs = fanIDs
	for i := range cfg.Curve {
		cfg.Curve[i].PWMPercent = 0
	}
	for i := range cfg.HDDCurve {
		cfg.HDDCurve[i].PWMPercent = 0
	}
	for i := range cfg.NVMeCurve {
		cfg.NVMeCurve[i].PWMPercent = 0
	}
	return cfg
}

// recordFanPWMWrites 记录测试期间每一笔 PWM 原始写入，返回读取已记录写入的函数。
func recordFanPWMWrites(t *testing.T) func() []int {
	t.Helper()
	var mu sync.Mutex
	var writes []int
	hook := func(fan FanDevice, raw int) {
		mu.Lock()
		writes = append(writes, raw)
		mu.Unlock()
	}
	// atomic 存储(与生产读侧的 atomic.Pointer 配对):测试 goroutine 中途
	// 安装钩子,主测试 goroutine 并发读,go test -race 要求原子访问
	fanPWMWriteHook.Store(&hook)
	t.Cleanup(func() { fanPWMWriteHook.Store(nil) })
	return func() []int {
		mu.Lock()
		defer mu.Unlock()
		return append([]int(nil), writes...)
	}
}

// assertNoSpinWrites 校验整条路径上没有出现过任何非零 PWM 写入。只看最后一次
// PWM 值会漏掉「中途被 fail-safe 拉到 100% 再写回 0」这种同样会伤人的情况。
func assertNoSpinWrites(t *testing.T, writes []int) {
	t.Helper()
	for _, raw := range writes {
		if raw != 0 {
			t.Fatalf("关停档维修期间出现非零 PWM 写入 %d（全部写入：%v）", raw, writes)
		}
	}
}

// 关停档维修期间，任何故障路径都不许让仍工作的另一路风扇意外起转：某通道测速
// 节点读不到时 DiscoverFans 直接漏掉它，校验报「配置的风扇找不到」，旧实现会
// 顺势走 fail-safe 把另一路拉到 100%。这里核对整条路径上的全部 PWM 写入。
func TestOffModeFailurePathNeverSpinsAnotherFan(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	pwm1 := filepath.Join(hwmon, "pwm1")
	fan2 := filepath.Join(hwmon, "fan2_input")
	writeTestValue(t, fan2, "1300")
	writeTestValue(t, filepath.Join(hwmon, "pwm2"), "128")
	writeTestValue(t, filepath.Join(hwmon, "pwm2_enable"), "2")

	cfg := offModeZeroCurveConfig("it8613:hwmon3:fan1", "it8613:hwmon3:fan2")
	if err := manager.SaveFanConfig(cfg); err != nil {
		t.Fatalf("save off mode: %v", err)
	}
	if got, _ := readInt(pwm1); got != 0 {
		t.Fatalf("关停档应立即写 0，实际 pwm1=%d", got)
	}

	// 注入一路测速读取失败：整个通道从 DiscoverFans 里消失。
	if err := os.Remove(fan2); err != nil {
		t.Fatal(err)
	}
	writes := recordFanPWMWrites(t)
	err := manager.ApplyFanCurrent()
	if err == nil {
		t.Fatal("通道消失必须报错，不能静默当作正常")
	}
	if !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("错误应说明配置的风扇找不到，实际 %v", err)
	}
	manager.mu.Lock()
	alarm := manager.fanLastError
	manager.mu.Unlock()
	if !strings.Contains(alarm, "was not found") {
		t.Fatalf("fanLastError 应保持该故障，实际 %q", alarm)
	}
	assertNoSpinWrites(t, writes())
	if got, _ := readInt(pwm1); got != 0 {
		t.Fatalf("关停档另一路风扇必须保持停转，实际 pwm1=%d", got)
	}
}

// 关停档下调试接管、调速、递增、标定一律不得把风扇转起来；CPU 超温时调试紧急
// 兜底（100%）也必须让位，改为保持停转 + 报警。
func TestOffModeBlocksDebugWritesAndEmergencySpinUp(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	pwm1 := filepath.Join(hwmon, "pwm1")
	const id = "it8613:hwmon3:fan1"

	cfg := offModeZeroCurveConfig(id)
	if err := manager.SaveFanConfig(cfg); err != nil {
		t.Fatalf("save off mode: %v", err)
	}
	if err := manager.SetFanDebugTakeover(id, true); err != nil {
		t.Fatalf("关停档仍应允许（不写 PWM 的）接管: %v", err)
	}
	writes := recordFanPWMWrites(t)
	if err := manager.SetFanDebugValue(id, 80, "percent"); !errors.Is(err, errFanOffModeBusy) {
		t.Fatalf("关停档必须拒绝调试调速，实际 %v", err)
	}
	if err := manager.SetFanDebugAuto(id, true, 5, 2, "percent"); !errors.Is(err, errFanOffModeBusy) {
		t.Fatalf("关停档必须拒绝自动递增，实际 %v", err)
	}
	if err := manager.StartFanDebugAutoBatch(map[string]fanDebugAutoEntry{
		id: {Step: 5, Interval: 2, Unit: "percent"},
	}); !errors.Is(err, errFanOffModeBusy) {
		t.Fatalf("关停档必须拒绝批量递增，实际 %v", err)
	}
	if _, err := manager.CalibrateFanRPM(id); !errors.Is(err, errFanOffModeBusy) {
		t.Fatalf("关停档必须拒绝标定（会写 255），实际 %v", err)
	}
	assertNoSpinWrites(t, writes())

	// CPU 超温：调试紧急兜底不得生效。
	writeTestValue(t, filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon9", "temp1_input"), "90000")
	writes = recordFanPWMWrites(t)
	err := manager.ApplyFanCurrent()
	if !errors.Is(err, errOffModeAlarm) {
		t.Fatalf("超温应报关停档报警，实际 %v", err)
	}
	manager.mu.Lock()
	emergency := manager.fanDebugEmergency
	manager.mu.Unlock()
	if emergency {
		t.Fatal("关停档下调试紧急兜底必须为 false")
	}
	assertNoSpinWrites(t, writes())
	if got, _ := readInt(pwm1); got != 0 {
		t.Fatalf("关停档超温也必须保持停转，实际 pwm1=%d", got)
	}
}

// RPM 闭环与自动递增都是后台 goroutine 写 PWM 的隐蔽路径：关停档下只能保持
// 存活，不得把刚写成 0% 的风扇按目标转速重新拉起来。
func TestOffModeStopsDebugLoopsFromWritingPWM(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	const id = "it8613:hwmon3:fan1"

	cfg := offModeZeroCurveConfig(id)
	if err := manager.SaveFanConfig(cfg); err != nil {
		t.Fatalf("save off mode: %v", err)
	}
	// 直接构造「rpm 单位 + 已接管 + 目标高于实测」的闭环状态：不走调试接口
	// （关停档下已被拒绝），模拟关停前就存在的接管。
	manager.mu.Lock()
	manager.fanDebugTakenOver = map[string]int{id: 1500}
	manager.fanDebugUnits = map[string]string{id: "rpm"}
	manager.mu.Unlock()

	writes := recordFanPWMWrites(t)
	if !manager.stepFanRPMCloseLoop() {
		t.Fatal("关停档下闭环应保持存活，退出后自动恢复")
	}
	assertNoSpinWrites(t, writes())
	if got, _ := readInt(filepath.Join(hwmon, "pwm1")); got != 0 {
		t.Fatalf("闭环不得改写关停档的 0%%，实际 pwm1=%d", got)
	}
}

// 释放调试接管时 restoreFan 会把原始非零 PWM 写回去，再由 ApplyFanCurrent
// 覆盖成 0——中间那笔非零写入虽然最终被清掉，维修关停期间依然算起转，必须整个
// 跳过 restoreFan，只让 ApplyFanCurrent 处理。回归：关停档下 release takeover
// 全程不得出现任何非零 PWM 写入。
func TestOffModeReleaseTakeoverNeverRestoresNonZeroPWM(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	pwm1 := filepath.Join(hwmon, "pwm1")
	const id = "it8613:hwmon3:fan1"

	// 先让风扇有一个非零的正常运行值，用作接管时 capture 的原始状态。
	writeTestValue(t, filepath.Join(hwmon, "pwm1"), "102")
	writeTestValue(t, filepath.Join(hwmon, "pwm1_enable"), "2")

	cfg := DefaultConfig(Profile{}).Fan
	cfg.Enabled = true
	cfg.CPUFanIDs = []string{id}
	cfg.OffMode = false
	cfg.Curve = clampCurvePWM(curve40to90(), cfg.MinPWMPercent)
	if err := manager.SaveFanConfig(cfg); err != nil {
		t.Fatalf("save baseline config: %v", err)
	}

	if err := manager.SetFanDebugTakeover(id, true); err != nil {
		t.Fatalf("takeover should succeed before off-mode: %v", err)
	}

	// 进入关停档：ApplyFanCurrent 把 PWM 写成 0。
	off := offModeZeroCurveConfig(id)
	if err := manager.SaveFanConfig(off); err != nil {
		t.Fatalf("save off-mode: %v", err)
	}
	if got, _ := readInt(pwm1); got != 0 {
		t.Fatalf("关停档应立即写 0，实际 pwm1=%d", got)
	}

	writes := recordFanPWMWrites(t)
	if err := manager.SetFanDebugTakeover(id, false); err != nil {
		t.Fatalf("release takeover should succeed even in off-mode: %v", err)
	}
	assertNoSpinWrites(t, writes())
	if got, _ := readInt(pwm1); got != 0 {
		t.Fatalf("释放接管后仍必须保持停转，实际 pwm1=%d", got)
	}
}

// 校准期间用户可能启用关停档（另一个 goroutine 跑 SaveFanConfig + ApplyFanCurrent
// 打断采样），结束后 restoreAfterCalibrateLocked 不能再把原始非零 PWM 写回去。
// 这里用 goroutine 在 CalibrateFanRPM 释放锁开始采样后介入 OffMode，
// 模拟真实的并发路径；断言：最终 PWM 为 0、OffMode 生效后的写入全为 0、
// 全速基准没被错误落盘。
func TestOffModeMidCalibrationRestoresZeroPWM(t *testing.T) {
	m := newFanDebugTestManager(t)
	hwmon := filepath.Join(m.Root, "sys", "class", "hwmon", "hwmon3")
	pwm1 := filepath.Join(hwmon, "pwm1")
	const id = "it8613:hwmon3:fan1"

	cfg := DefaultConfig(Profile{}).Fan
	cfg.Enabled = true
	cfg.CPUFanIDs = []string{id}
	cfg.OffMode = false
	cfg.Curve = clampCurvePWM(curve40to90(), cfg.MinPWMPercent)
	if err := m.SaveFanConfig(cfg); err != nil {
		t.Fatalf("save baseline: %v", err)
	}

	// 让接管在 calibrate 之前就生效，原始状态 capture 一个非零 PWM。
	if err := m.SetFanDebugTakeover(id, true); err != nil {
		t.Fatalf("pre-calibrate takeover: %v", err)
	}

	// 先开始校准（会写 255，这是关停前的全速标定准备，可接受）。
	done := make(chan struct{})
	var afterOffWrites []int
	go func() {
		defer close(done)
		time.Sleep(100 * time.Millisecond)
		// 关停档启用后，ApplyFanCurrent 把 PWM 强制写 0；
		// 此时开始记录写入，只看关停后的副作用。
		stop := recordFanPWMWrites(t)
		off := offModeZeroCurveConfig(id)
		_ = m.SaveFanConfig(off)
		time.Sleep(50 * time.Millisecond)
		afterOffWrites = stop()
	}()

	_, calibErr := m.CalibrateFanRPM(id)
	<-done
	if calibErr != nil {
		t.Fatalf("校准不应报错：%v", calibErr)
	}

	// 关停档启用后的所有 PWM 写入必须全为 0：包括 ApplyFanCurrent 的 0、
	// 以及 restoreAfterCalibrateLocked 必须写 0 而不是 originalPWM。
	for _, w := range afterOffWrites {
		if w != 0 {
			t.Fatalf("关停档生效后仍出现非零 PWM %d（序列：%v）", w, afterOffWrites)
		}
	}

	// 关键断言：最终 PWM 必须是 0，不能是 originalPWM（接管时的 102）或 255。
	if got, _ := readInt(pwm1); got != 0 {
		t.Fatalf("校准被关停档打断后必须停在 0%%，实际 pwm1=%d", got)
	}

	// 全速基准不应落盘：采样过程 ApplyFanCurrent 已把 PWM 从 255 写成 0，
	// 读数不再可信。
	m.mu.Lock()
	table := m.fanRPMLearned[id]
	m.mu.Unlock()
	if table != nil && table[240] != 0 {
		t.Fatalf("关停档打断的校准不应落盘全速基准，实际 slot240=%d", table[240])
	}
}

// 关停档在采样期间被开启又关闭：采样结束时的状态已回到普通档，只看结束状态
// 会漏判，被 0 转打断过的读数仍然入表。按进入代次作废——期间进入过关停档
// （哪怕已经退出）的全速基准一律不落盘。
func TestOffModeEnteredAndExitedMidCalibrationDoesNotRecord(t *testing.T) {
	m := newFanDebugTestManager(t)
	const id = "it8613:hwmon3:fan1"

	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.CPUFanIDs = []string{id}
	if err := m.SaveFanConfig(cfg); err != nil {
		t.Fatalf("save baseline: %v", err)
	}

	// 采样开始后（标定已释放锁）先切进关停档，再切回普通档：结束时 fanOffModeActiveLocked
	// 为 false，只能靠代次发现「期间进入过」。
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(100 * time.Millisecond)
		if err := m.SaveFanConfig(offModeZeroCurveConfig(id)); err != nil {
			t.Errorf("进入关停档失败: %v", err)
			return
		}
		time.Sleep(50 * time.Millisecond)
		if err := m.SaveFanConfig(cfg); err != nil {
			t.Errorf("退出关停档失败: %v", err)
		}
	}()

	_, calibErr := m.CalibrateFanRPM(id)
	<-done
	if calibErr != nil {
		t.Fatalf("校准不应报错：%v", calibErr)
	}

	m.mu.Lock()
	gen := m.fanOffGen
	table := m.fanRPMLearned[id]
	m.mu.Unlock()
	if gen == 0 {
		t.Fatal("期间进入过关停档，代次必须递增")
	}
	if table != nil && table[240] != 0 {
		t.Fatalf("关停档期间被 0 转打断的标定不得落盘，实际 slot240=%d", table[240])
	}
}

// curve40to90 测试用 CPU 曲线：20°C→40%, 80°C→90%。
func curve40to90() []FanPoint {
	return []FanPoint{
		{TempC: 20, PWMPercent: 40},
		{TempC: 80, PWMPercent: 90},
	}
}

// TestOffModeAlarmMuteSilencesChimeUntilOffModeExit 关停档的 20 秒故障/超温
// 蜂鸣报警可以临时静音：静音期间不响也不推进节流；在 20 秒节流窗口内解除
// 静音必须立即补响，而不是继续被节流；退出关停档后静音自动解除。
func TestOffModeAlarmMuteSilencesChimeUntilOffModeExit(t *testing.T) {
	manager := newFanDebugTestManager(t)
	const id = "it8613:hwmon3:fan1"
	logBuf := &bytes.Buffer{}
	manager.SetLogger(log.New(logBuf, "", 0))

	t0 := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	now := t0
	manager.nowFn = func() time.Time { return now }
	chimes := 0
	fanAlarmChimeHook = func([]GPIOToneSpec) { chimes++ }
	t.Cleanup(func() { fanAlarmChimeHook = nil })

	// 真报警场景：CPU 温度越过紧急阈值，关停档只报警、风扇保持停转。
	coretemp := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon9")
	writeTestValue(t, filepath.Join(coretemp, "temp1_input"), "90000")
	if err := manager.SaveFanConfig(offModeZeroCurveConfig(id)); err != nil {
		t.Fatalf("关停档超温是正当维修场景，保存必须成功: %v", err)
	}
	if chimes != 1 {
		t.Fatalf("首次报警应立即蜂鸣一次，实际 %d 次", chimes)
	}
	if !strings.Contains(logBuf.String(), "已蜂鸣报警") {
		t.Fatalf("真的响了之后日志要写「已蜂鸣报警」，实际 %q", logBuf.String())
	}

	// 20 秒节流窗口内：不重复响。
	now = t0.Add(5 * time.Second)
	// 关停档报警期间 ApplyFanCurrent 会返回 errOffModeAlarm（只报警不起转），
	// 这是预期结果，测试只需确认没有其它错误。
	if err := manager.ApplyFanCurrent(); err != nil && !errors.Is(err, errOffModeAlarm) {
		t.Fatalf("apply: %v", err)
	}
	if chimes != 1 {
		t.Fatalf("20 秒内不得重复蜂鸣，实际 %d 次", chimes)
	}

	// 静音后持续报警：不响、不推进节流时刻，日志明确写「已临时静音」。
	if err := manager.SetFanAlarmMute(true); err != nil {
		t.Fatalf("关停档下应允许临时静音: %v", err)
	}
	now = t0.Add(8 * time.Second)
	// 关停档报警期间 ApplyFanCurrent 会返回 errOffModeAlarm（只报警不起转），
	// 这是预期结果，测试只需确认没有其它错误。
	if err := manager.ApplyFanCurrent(); err != nil && !errors.Is(err, errOffModeAlarm) {
		t.Fatalf("apply: %v", err)
	}
	if chimes != 1 {
		t.Fatalf("静音期间不得蜂鸣，实际 %d 次", chimes)
	}
	manager.mu.Lock()
	chimeAt := manager.fanOffAlarmChimeAt
	manager.mu.Unlock()
	if !chimeAt.Equal(t0) {
		t.Fatalf("静音期间不得推进节流时刻，实际 %v，期望 %v", chimeAt, t0)
	}
	if !strings.Contains(logBuf.String(), "蜂鸣已临时静音") {
		t.Fatalf("静音时日志必须说明已静音，实际 %q", logBuf.String())
	}

	// 关键时序：距离上次真响不足 20 秒就解除静音，仍必须立刻补响。
	now = t0.Add(10 * time.Second)
	if err := manager.SetFanAlarmMute(false); err != nil {
		t.Fatalf("解除静音失败: %v", err)
	}
	if chimes != 2 {
		t.Fatalf("解除静音后应立即补响一次（不能被节流挡住），实际 %d 次", chimes)
	}
	manager.mu.Lock()
	chimeAt = manager.fanOffAlarmChimeAt
	manager.mu.Unlock()
	if !chimeAt.Equal(now) {
		t.Fatalf("补响应把节流时刻更新到解除时刻，实际 %v，期望 %v", chimeAt, now)
	}

	// 补响后仍按 20 秒节流；满 20 秒后恢复重响。
	now = t0.Add(12 * time.Second)
	// 关停档报警期间 ApplyFanCurrent 会返回 errOffModeAlarm（只报警不起转），
	// 这是预期结果，测试只需确认没有其它错误。
	if err := manager.ApplyFanCurrent(); err != nil && !errors.Is(err, errOffModeAlarm) {
		t.Fatalf("apply: %v", err)
	}
	if chimes != 2 {
		t.Fatalf("补响后的节流窗口内不得重复蜂鸣，实际 %d 次", chimes)
	}
	now = t0.Add(31 * time.Second)
	// 关停档报警期间 ApplyFanCurrent 会返回 errOffModeAlarm（只报警不起转），
	// 这是预期结果，测试只需确认没有其它错误。
	if err := manager.ApplyFanCurrent(); err != nil && !errors.Is(err, errOffModeAlarm) {
		t.Fatalf("apply: %v", err)
	}
	if chimes != 3 {
		t.Fatalf("距上次蜂鸣满 20 秒应重响，实际 %d 次", chimes)
	}

	// 再次静音后退出关停档：开关必须自动恢复，下一次真故障照常报警。
	if err := manager.SetFanAlarmMute(true); err != nil {
		t.Fatalf("再次静音失败: %v", err)
	}
	writeTestValue(t, filepath.Join(coretemp, "temp1_input"), "50000")
	on := DefaultFanConfig()
	on.Enabled = true
	on.CPUFanIDs = []string{id}
	if err := manager.SaveFanConfig(on); err != nil {
		t.Fatalf("save normal: %v", err)
	}
	if manager.Status().FanControl.AlarmMuted {
		t.Fatal("退出关停档后静音开关必须自动解除")
	}

	// 静音已解除：再次进入关停并真报警时必须照常蜂鸣。
	writeTestValue(t, filepath.Join(coretemp, "temp1_input"), "90000")
	if err := manager.SaveFanConfig(offModeZeroCurveConfig(id)); err != nil {
		t.Fatalf("重新进入关停档: %v", err)
	}
	if chimes != 4 {
		t.Fatalf("退出关停后静音必须失效，真报警要再次蜂鸣，实际 %d 次", chimes)
	}
}

// TestFanAlarmMuteRejectedOutsideOffMode 非关停档下没有 20 秒报警可静音，
// 接口必须直接拒绝，且不得改变风扇控制（过温策略照旧）。
func TestFanAlarmMuteRejectedOutsideOffMode(t *testing.T) {
	manager := newFanDebugTestManager(t)
	const id = "it8613:hwmon3:fan1"
	pwm1 := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3", "pwm1")

	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.CPUFanIDs = []string{id}
	cfg.Curve = clampCurvePWM(curve40to90(), cfg.MinPWMPercent)
	if err := manager.SaveFanConfig(cfg); err != nil {
		t.Fatalf("save normal: %v", err)
	}
	before, _ := readInt(pwm1)
	for _, muted := range []bool{true, false} {
		if err := manager.SetFanAlarmMute(muted); err == nil {
			t.Fatalf("非关停档下静音(%v)必须被拒绝", muted)
		}
	}
	if manager.Status().FanControl.AlarmMuted {
		t.Fatal("被拒绝的静音请求不得改变运行状态")
	}
	if after, _ := readInt(pwm1); after != before {
		t.Fatalf("被拒绝的静音请求不得改变风扇控制，pwm %d → %d", before, after)
	}
}

// 关停档报警不能只盯 CPU：前置机械硬盘达到 55°C 上限同样要报警。维修安全
// 策略不变——只报警/蜂鸣，风扇保持停转；温度回落后报警清除。
func TestOffModeAlarmsOnHDDOver55C(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	pwm1 := filepath.Join(hwmon, "pwm1")

	manager.storageMu.Lock()
	manager.storageStatus = StorageStatus{Slots: []StorageSlot{
		{ID: "front-1", Kind: "front", Slot: 1, State: StorageUsed, TemperatureC: 56},
		{ID: "front-2", Kind: "front", Slot: 2, State: StorageUsed, TemperatureC: 40},
		{ID: "m2-1", Kind: "m2", Slot: 1, State: StorageUsed, TemperatureC: 30},
	}}
	manager.storageMu.Unlock()

	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.OffMode = true
	cfg.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	for i := range cfg.Curve {
		cfg.Curve[i].PWMPercent = 0
	}
	for i := range cfg.HDDCurve {
		cfg.HDDCurve[i].PWMPercent = 0
	}
	for i := range cfg.NVMeCurve {
		cfg.NVMeCurve[i].PWMPercent = 0
	}
	if err := manager.SaveFanConfig(cfg); err != nil {
		t.Fatalf("关停档下盘温超限是正当维修场景，保存必须成功: %v", err)
	}
	if got, _ := readInt(pwm1); got != 0 {
		t.Fatalf("维修安全策略：盘温超限时风扇必须保持停转，实际 pwm1=%d", got)
	}
	manager.mu.Lock()
	alarm := manager.fanLastError
	manager.mu.Unlock()
	if !strings.Contains(alarm, "机械硬盘温度 56.0°C") || !strings.Contains(alarm, "SATA 1") {
		t.Fatalf("盘温超过 55°C 必须单独报警并指明槽位，实际 %q", alarm)
	}

	// 回落到 54°C：报警清除，风扇保持停转。
	manager.storageMu.Lock()
	manager.storageStatus.Slots[0].TemperatureC = 54
	manager.storageMu.Unlock()
	if err := manager.ApplyFanCurrent(); err != nil {
		t.Fatalf("盘温回落后不应再报警: %v", err)
	}
	manager.mu.Lock()
	alarm = manager.fanLastError
	manager.mu.Unlock()
	if alarm != "" {
		t.Fatalf("盘温回落后报警应清除，实际 %q", alarm)
	}
	if got, _ := readInt(pwm1); got != 0 {
		t.Fatalf("关停档风扇应保持停转，实际 pwm1=%d", got)
	}
}
