package powerguard

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDetectProfile(t *testing.T) {
	tests := []struct {
		model   string
		want    string
		wantPL1 int64
		wantPL2 int64
	}{
		{"Intel(R) Core(TM) i3-N305", "n305", 15, 15},
		{"Intel(R) Processor N100", "n100", 6, 15},
		{"Intel N100 CPU @ 0.80GHz", "n100", 6, 15},
	}
	for _, test := range tests {
		profile, err := DetectProfile(test.model)
		if err != nil {
			t.Fatalf("DetectProfile(%q): %v", test.model, err)
		}
		if profile.ID != test.want {
			t.Fatalf("DetectProfile(%q)=%q, want %q", test.model, profile.ID, test.want)
		}
		if profile.DefaultPL1 != test.wantPL1 || profile.DefaultPL2 != test.wantPL2 {
			t.Fatalf("DetectProfile(%q) defaults=%d/%d, want %d/%d", test.model, profile.DefaultPL1, profile.DefaultPL2, test.wantPL1, test.wantPL2)
		}
	}
	if _, err := DetectProfile("Intel Core i5-12500"); err == nil {
		t.Fatal("unsupported model was accepted")
	}
}

func TestApplyPackageAndVerify(t *testing.T) {
	dir := t.TempDir()
	longPath := filepath.Join(dir, "long")
	shortPath := filepath.Join(dir, "short")
	for _, path := range []string{longPath, shortPath} {
		if err := os.WriteFile(path, []byte("25000000"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pkg := Package{
		Name:  "package-0",
		Long:  &Constraint{PowerPath: longPath, CurrentUW: 25_000_000},
		Short: &Constraint{PowerPath: shortPath, CurrentUW: 35_000_000},
	}
	if err := applyPackage(pkg, 15_000_000, 25_000_000); err != nil {
		t.Fatal(err)
	}
	if got, _ := readInt(longPath); got != 15_000_000 {
		t.Fatalf("PL1=%d", got)
	}
	if got, _ := readInt(shortPath); got != 25_000_000 {
		t.Fatalf("PL2=%d", got)
	}
}

func TestWriteJSONAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	cfg := Config{Enabled: true, PL1W: 15, PL2W: 25, ReapplySeconds: 30}
	if err := writeJSONAtomic(path, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatal("JSON file is empty or lacks final newline")
	}
}

func TestScopedConfigSavesPreserveOtherSections(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot create the intel-rapl:0 fixture path")
	}
	root := t.TempDir()
	procDir := filepath.Join(root, "proc")
	raplDir := filepath.Join(root, "sys", "devices", "virtual", "powercap", "intel-rapl", "intel-rapl:0")
	for _, dir := range []string{procDir, raplDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(procDir, "cpuinfo"), []byte("model name : Intel(R) Core(TM) i3-N305\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"name": "package-0", "constraint_0_name": "long_term", "constraint_0_power_limit_uw": "15000000",
		"constraint_0_max_power_uw": "20000000", "constraint_1_name": "short_term",
		"constraint_1_power_limit_uw": "15000000", "constraint_1_max_power_uw": "35000000",
	}
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(raplDir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	manager := &Manager{
		Root: root, ConfigPath: filepath.Join(root, "config.json"), StatePath: filepath.Join(root, "state.json"),
	}
	cfg := DefaultConfig(profiles[0])
	cfg.Enabled = false
	cfg.GPIO.Buttons[0].Actions.Hold3S = GPIOActionLog
	if err := writeJSONAtomic(manager.ConfigPath, cfg, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := manager.SaveGlobalConfig(GlobalConfig{Enabled: false, PL1W: 12, PL2W: 18, ReapplySeconds: 45}); err != nil {
		t.Fatal(err)
	}
	afterGlobal, err := manager.loadConfigLocked()
	if err != nil {
		t.Fatal(err)
	}
	if afterGlobal.Fan.MinPWMPercent != cfg.Fan.MinPWMPercent || afterGlobal.GPIO.Buttons[0].Actions.Hold3S != GPIOActionLog {
		t.Fatalf("global save changed another section: %+v", afterGlobal)
	}

	newFan := afterGlobal.Fan
	newFan.MinPWMPercent = 35
	for index := range newFan.Curve {
		newFan.Curve[index].PWMPercent = max(newFan.Curve[index].PWMPercent, 35)
	}
	for index := range newFan.HDDCurve {
		newFan.HDDCurve[index].PWMPercent = max(newFan.HDDCurve[index].PWMPercent, 35)
	}
	for index := range newFan.NVMeCurve {
		newFan.NVMeCurve[index].PWMPercent = max(newFan.NVMeCurve[index].PWMPercent, 35)
	}
	if err := manager.SaveFanConfig(newFan); err != nil {
		t.Fatal(err)
	}
	afterFan, err := manager.loadConfigLocked()
	if err != nil {
		t.Fatal(err)
	}
	if afterFan.PL1W != 12 || afterFan.GPIO.Buttons[0].Actions.Hold3S != GPIOActionLog {
		t.Fatalf("fan save changed another section: %+v", afterFan)
	}

	newGPIO := afterFan.GPIO
	newGPIO.Enabled = true
	if err := manager.SaveGPIOConfig(newGPIO); err != nil {
		t.Fatal(err)
	}
	afterGPIO, err := manager.loadConfigLocked()
	if err != nil {
		t.Fatal(err)
	}
	if afterGPIO.PL1W != 12 || afterGPIO.Fan.MinPWMPercent != 35 || !afterGPIO.GPIO.Enabled {
		t.Fatalf("GPIO save changed another section: %+v", afterGPIO)
	}
}

func TestSummarizeCPUTemperaturesPrefersRRCoreMaximum(t *testing.T) {
	status := summarizeCPUTemperatures([]Temperature{
		{Label: "Package id 0", Celsius: 95},
		{Label: "Core 0", Celsius: 72},
		{Label: "Core 1", Celsius: 78},
		{Label: "Package id 1", Celsius: 91},
		{Label: "Core 2", Celsius: 75},
	})
	if !status.Available || status.DisplaySource != "core_max_rr" || status.DisplayC != 78 {
		t.Fatalf("unexpected display temperature: %+v", status)
	}
	if status.CoreMaxC != 78 || status.PackageMaxC != 95 || status.CoreSensors != 3 || status.PackageSensors != 2 {
		t.Fatalf("unexpected temperature summary: %+v", status)
	}
}

func TestSummarizeCPUTemperaturesFallsBackToPackage(t *testing.T) {
	status := summarizeCPUTemperatures([]Temperature{{Label: "Package id 0", Celsius: 64}})
	if !status.Available || status.DisplaySource != "package_fallback" || status.DisplayC != 64 {
		t.Fatalf("unexpected package fallback: %+v", status)
	}
}

func TestOriginalStateCPUValidation(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "cpuinfo"), []byte("model name : Intel(R) Processor N100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root, StatePath: filepath.Join(root, "state.json")}

	matching := OriginalState{Version: stateVersion, CPUModel: "  intel(r) PROCESSOR   n100  "}
	if err := manager.validateOriginalStateCPU(matching); err != nil {
		t.Fatalf("matching normalized CPU model was rejected: %v", err)
	}

	mismatched := OriginalState{Version: stateVersion, CPUModel: "Intel(R) Core(TM) i3-N305"}
	err := manager.validateOriginalStateCPU(mismatched)
	if !errors.Is(err, ErrOriginalStateCPUMismatch) {
		t.Fatalf("mismatched CPU model error=%v, want %v", err, ErrOriginalStateCPUMismatch)
	}
	if !strings.Contains(err.Error(), mismatched.CPUModel) {
		t.Fatalf("mismatch error lacks saved model: %v", err)
	}
}

func TestCaptureAndRestoreRejectOriginalStateFromOtherCPU(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot create the intel-rapl:0 fixture path")
	}
	root := t.TempDir()
	procDir := filepath.Join(root, "proc")
	raplDir := filepath.Join(root, "sys", "devices", "virtual", "powercap", "intel-rapl", "intel-rapl:0")
	for _, dir := range []string{procDir, raplDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(procDir, "cpuinfo"), []byte("model name : Intel(R) Processor N100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"name": "package-0", "constraint_0_name": "long_term", "constraint_0_power_limit_uw": "6000000",
		"constraint_1_name": "short_term", "constraint_1_power_limit_uw": "15000000",
	}
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(raplDir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager := &Manager{Root: root, StatePath: filepath.Join(root, "state.json")}
	state := OriginalState{
		Version: stateVersion, CPUModel: "Intel(R) Core(TM) i3-N305", CapturedAt: time.Now(),
		Packages: []OriginalPackage{{Name: "package-0", LongUW: 15_000_000}},
	}
	if err := writeJSONAtomic(manager.StatePath, state, 0o600); err != nil {
		t.Fatal(err)
	}
	packages, err := manager.DiscoverPackages()
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.captureOriginalLocked(packages); !errors.Is(err, ErrOriginalStateCPUMismatch) {
		t.Fatalf("capture error=%v, want %v", err, ErrOriginalStateCPUMismatch)
	}
	if err := manager.restorePowerLocked(); !errors.Is(err, ErrOriginalStateCPUMismatch) {
		t.Fatalf("restore error=%v, want %v", err, ErrOriginalStateCPUMismatch)
	}
	if got, err := readInt(filepath.Join(raplDir, "constraint_0_power_limit_uw")); err != nil || got != 6_000_000 {
		t.Fatalf("restore changed current hardware power limit: got=%d err=%v", got, err)
	}
}
