package powerguard

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
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
	cfg := Config{Fan: DefaultFanConfig(), GPIO: DefaultGPIOConfig()}
	cfg.Fan.HDDSlotIDs = []string{}
	cfg.Fan.NVMeSlotIDs = []string{}
	if normalizeConfig(&cfg) {
		t.Fatal("normalized config unexpectedly changed")
	}
	if len(cfg.Fan.HDDSlotIDs) != 0 || len(cfg.Fan.NVMeSlotIDs) != 0 {
		t.Fatalf("disabled storage monitoring was re-enabled: HDD=%v NVMe=%v", cfg.Fan.HDDSlotIDs, cfg.Fan.NVMeSlotIDs)
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
