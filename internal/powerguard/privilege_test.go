package powerguard

import (
	"errors"
	"testing"
)

func TestMonitoringOnly(t *testing.T) {
	cfg := Config{Enabled: false, Fan: DefaultFanConfig(), GPIO: DefaultGPIOConfig()}
	if !cfg.MonitoringOnly() {
		t.Fatal("expected monitoring-only when power/fan/gpio are off")
	}
	cfg.Enabled = true
	if cfg.MonitoringOnly() {
		t.Fatal("power enabled must require root")
	}
	cfg.Enabled = false
	cfg.Fan.Enabled = true
	if cfg.MonitoringOnly() {
		t.Fatal("fan enabled must require root")
	}
	cfg.Fan.Enabled = false
	cfg.GPIO.Enabled = true
	if cfg.MonitoringOnly() {
		t.Fatal("gpio enabled must require root")
	}
}

func TestRequireElevatedForWritesAllowsMonitoringOnly(t *testing.T) {
	cfg := Config{Enabled: false, Fan: DefaultFanConfig(), GPIO: DefaultGPIOConfig()}
	if err := requireElevatedForWrites(cfg); err != nil {
		t.Fatalf("monitoring-only must not require elevation: %v", err)
	}
}

func TestRequireElevatedForWritesRejectsNonRootWrites(t *testing.T) {
	if elevated() {
		t.Skip("running as root; cannot assert non-root rejection")
	}
	cfg := Config{Enabled: true, PL1W: 15, PL2W: 25, ReapplySeconds: 30, Fan: DefaultFanConfig(), GPIO: DefaultGPIOConfig()}
	err := requireElevatedForWrites(cfg)
	if !errors.Is(err, ErrRootRequired) {
		t.Fatalf("got %v, want ErrRootRequired", err)
	}
}
