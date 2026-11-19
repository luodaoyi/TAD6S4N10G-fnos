package powerguard

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func noRAPLManager(t *testing.T) (*Manager, Config) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "cpuinfo"), []byte("model name : Intel(R) Core(TM) i3-N305\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Root: root, ConfigPath: filepath.Join(root, "config.json"), StatePath: filepath.Join(root, "state.json")}
	cfg := DefaultConfig(profiles[0])
	cfg.Fan.Enabled = false
	cfg.GPIO.Enabled = false
	return m, cfg
}

func TestAbsentRAPLIsCapabilityNotPeriodicError(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		m, cfg := noRAPLManager(t)
		cfg.Enabled = enabled
		if err := writeJSONAtomic(m.ConfigPath, cfg, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := m.DiscoverPackages(); !errors.Is(err, ErrNoRAPL) {
			t.Fatalf("expected absent capability: %v", err)
		}
		for i := 0; i < 2; i++ {
			if err := m.ApplyCurrent(); err != nil {
				t.Fatalf("enabled=%v periodic apply: %v", enabled, err)
			}
		}
		status := m.Status()
		if !status.Supported || status.PowerSupported || len(status.Packages) != 0 {
			t.Fatalf("bad capability status: supported=%v power=%v", status.Supported, status.PowerSupported)
		}
		if status.LastError != "" {
			t.Fatalf("absent RAPL should not alarm: %s", status.LastError)
		}
		if _, err := os.Stat(m.StatePath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("no hardware state should be created: %v", err)
		}
		cfg.ReapplySeconds = 1
		if err := m.Validate(cfg); err == nil {
			t.Fatal("invalid configuration must still fail")
		}
	}
}

func TestBrokenRAPLZoneStillReportsError(t *testing.T) {
	m, cfg := noRAPLManager(t)
	cfg.Enabled = true
	if err := os.MkdirAll(m.rooted("/sys/class/powercap/intel-rapl:0"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(m.ConfigPath, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.ApplyCurrent(); err == nil || errors.Is(err, ErrNoRAPL) {
		t.Fatalf("broken zone is not absent capability: %v", err)
	}
	if status := m.Status(); !strings.Contains(status.LastError, "read RAPL zone") {
		t.Fatalf("missing actionable error: %s", status.LastError)
	}
}

func TestRAPLAbsenceDoesNotHideOtherErrors(t *testing.T) {
	m, cfg := noRAPLManager(t)
	if err := writeJSONAtomic(m.ConfigPath, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	m.fanLastError = "fan stalled"
	if status := m.Status(); !strings.Contains(status.LastError, "fan stalled") {
		t.Fatal("fan failure was hidden")
	}
	if err := os.WriteFile(m.ConfigPath, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.ApplyCurrent(); err == nil {
		t.Fatal("corrupt config was hidden")
	}
}

func TestRAPLCapabilityKeepsHardwareLimits(t *testing.T) {
	m, cfg := noRAPLManager(t)
	cfg.Enabled = true
	dir := m.rooted("/sys/class/powercap/intel-rapl:0")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"name": "package-0", "constraint_0_name": "long_term", "constraint_0_power_limit_uw": "15000000", "constraint_0_max_power_uw": "10000000"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeJSONAtomic(m.ConfigPath, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.ApplyCurrent(); err == nil || !strings.Contains(err.Error(), "hardware long-term maximum") {
		t.Fatalf("hardware maximum bypassed: %v", err)
	}
	if !m.Status().PowerSupported {
		t.Fatal("real RAPL capability not reported")
	}
	if got, err := readInt(filepath.Join(dir, "constraint_0_power_limit_uw")); err != nil || got != 15000000 {
		t.Fatalf("invalid apply wrote power: %d %v", got, err)
	}
}

func TestApplyRevalidatesNewRAPLSnapshot(t *testing.T) {
	for _, missingLong := range []bool{true, false} {
		m, cfg := noRAPLManager(t)
		cfg.Enabled = true
		if err := m.Validate(cfg); err != nil {
			t.Fatal(err)
		}
		dir := m.rooted("/sys/class/powercap/intel-rapl:0")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "name"), []byte("package-0"), 0600); err != nil {
			t.Fatal(err)
		}
		if !missingLong {
			for name, value := range map[string]string{"constraint_0_name": "long_term", "constraint_0_power_limit_uw": "15000000", "constraint_0_max_power_uw": "10000000"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		state := OriginalState{Version: stateVersion, CPUModel: "Intel(R) Core(TM) i3-N305", Packages: []OriginalPackage{{Name: "package-0", LongUW: 15000000}}}
		if err := writeJSONAtomic(m.StatePath, state, 0600); err != nil {
			t.Fatal(err)
		}
		err := m.applyPowerLocked(cfg)
		if err == nil {
			t.Fatal("newly appeared invalid RAPL snapshot accepted")
		}
		if missingLong && !strings.Contains(err.Error(), "no long_term") {
			t.Fatalf("wrong error: %v", err)
		}
		if !missingLong && !strings.Contains(err.Error(), "hardware long-term maximum") {
			t.Fatalf("wrong error: %v", err)
		}
	}
}

func TestRAPLDiscoveryDirectoryErrorsAreNotAbsence(t *testing.T) {
	m, _ := noRAPLManager(t)
	parent := m.rooted("/sys/class")
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "powercap"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DiscoverPackages(); err == nil || errors.Is(err, ErrNoRAPL) {
		t.Fatalf("directory I/O failure hidden: %v", err)
	}
}
