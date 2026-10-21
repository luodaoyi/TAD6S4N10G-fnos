package powerguard

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	if cfg.MinPWMPercent != 60 || cfg.EmergencyTempC != 85 || cfg.PollSeconds != 2 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if len(cfg.Curve) != 4 || cfg.Curve[len(cfg.Curve)-1].PWMPercent != 100 {
		t.Fatalf("unexpected default curve: %+v", cfg.Curve)
	}
	for name, curve := range map[string][]FanPoint{"HDD": cfg.HDDCurve, "NVMe": cfg.NVMeCurve} {
		if len(curve) != 3 || curve[0].TempC != 25 || curve[0].PWMPercent != 60 || curve[1].TempC != 35 || curve[1].PWMPercent != 85 || curve[2].TempC != 50 || curve[2].PWMPercent != 100 {
			t.Fatalf("unexpected default %s curve: %+v", name, curve)
		}
	}
	if len(cfg.HDDSlotIDs) != 6 || len(cfg.NVMeSlotIDs) != 4 {
		t.Fatalf("all storage slots must participate by default: HDD=%v NVMe=%v", cfg.HDDSlotIDs, cfg.NVMeSlotIDs)
	}
}

func TestInterpolatePWMPercent(t *testing.T) {
	curve := DefaultFanConfig().Curve
	tests := []struct {
		temp float64
		want int
	}{
		{30, 60},
		{40, 60},
		{47.5, 65},
		{55, 70},
		{70, 85},
		{80, 100},
		{85, 100},
	}
	for _, test := range tests {
		if got := interpolatePWMPercent(curve, test.temp, 60, 85); got != test.want {
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
	if cfg.Fan.Enabled || cfg.Fan.MinPWMPercent != 60 || len(cfg.Fan.Curve) != 4 || len(cfg.Fan.HDDCurve) != 3 || len(cfg.Fan.NVMeCurve) != 3 || len(cfg.Fan.HDDSlotIDs) != 6 || len(cfg.Fan.NVMeSlotIDs) != 4 {
		t.Fatalf("unexpected migrated fan config: %+v", cfg.Fan)
	}
	if normalizeConfig(&cfg) {
		t.Fatal("normalized config was migrated a second time")
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
	if len(cfg.Fan.NVMeCurve) != 3 || cfg.Fan.NVMeCurve[0].PWMPercent != 30 || cfg.Fan.NVMeCurve[1].PWMPercent != 85 {
		t.Fatalf("NVMe curve did not inherit the configured minimum: %+v", cfg.Fan.NVMeCurve)
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
	if cpu != 65 || hdd != 30 || nvme != 30 || target != 65 {
		t.Fatalf("CPU should control cool storage: cpu=%d hdd=%d nvme=%d target=%d", cpu, hdd, nvme, target)
	}
	cpu, hdd, nvme, target = fanTargets(cfg, 45, 35, true, 30, true)
	if cpu != 39 || hdd != 85 || nvme != 30 || target != 85 {
		t.Fatalf("HDD should control: cpu=%d hdd=%d nvme=%d target=%d", cpu, hdd, nvme, target)
	}
	cpu, hdd, nvme, target = fanTargets(cfg, 45, 25, true, 70, true)
	if cpu != 39 || hdd != 30 || nvme != 90 || target != 90 {
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
	cfg.Curve[2].PWMPercent = 40
	if err := (&Manager{}).validateFanLocked(cfg); err == nil {
		t.Fatal("decreasing PWM curve was accepted")
	}
}

func TestFanValidationRejectsDecreasingStorageSpeed(t *testing.T) {
	cfg := DefaultFanConfig()
	cfg.NVMeCurve[2].PWMPercent = 40
	if err := (&Manager{}).validateFanLocked(cfg); err == nil {
		t.Fatal("decreasing NVMe PWM curve was accepted")
	}
}

func TestFanValidationRejectsInvalidOrDuplicateStorageSlots(t *testing.T) {
	cfg := DefaultFanConfig()
	cfg.HDDSlotIDs = []string{"front-1", "front-1"}
	if err := (&Manager{}).validateFanLocked(cfg); err == nil {
		t.Fatal("duplicate HDD slot was accepted")
	}
	cfg = DefaultFanConfig()
	cfg.NVMeSlotIDs = []string{"front-1"}
	if err := (&Manager{}).validateFanLocked(cfg); err == nil {
		t.Fatal("HDD slot was accepted as NVMe slot")
	}
	cfg = DefaultFanConfig()
	cfg.HDDSlotIDs = []string{}
	cfg.NVMeSlotIDs = []string{}
	if err := (&Manager{}).validateFanLocked(cfg); err != nil {
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
