package powerguard

import (
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const debugReportVersion = 1

type debugReport struct {
	ReportVersion int          `json:"report_version"`
	CapturedAt    time.Time    `json:"captured_at"`
	PluginVersion string       `json:"plugin_version,omitempty"`
	Host          debugHost    `json:"host"`
	CPU           debugCPU     `json:"cpu"`
	Power         debugPower   `json:"power"`
	Fan           debugFan     `json:"fan"`
	Storage       debugStorage `json:"storage"`
	GPIO          debugGPIO    `json:"gpio"`
	Checks        []debugCheck `json:"checks"`
	LastError     string       `json:"last_error,omitempty"`
}

type debugHost struct {
	Name      string `json:"name,omitempty"`
	OS        string `json:"os,omitempty"`
	OSVersion string `json:"os_version,omitempty"`
}

type debugCPU struct {
	Model             string               `json:"model,omitempty"`
	Profile           Profile              `json:"profile"`
	Temperature       float64              `json:"temperature_c,omitempty"`
	TemperatureStatus CPUTemperatureStatus `json:"temperature_status"`
	Sensors           []Temperature        `json:"sensors"`
	GPURuntime        []string             `json:"gpu_runtime"`
	Available         bool                 `json:"available"`
}

type debugPower struct {
	Supported        bool            `json:"supported"`
	EffectiveMaxPL1W int64           `json:"effective_max_pl1_w,omitempty"`
	EffectiveMaxPL2W int64           `json:"effective_max_pl2_w,omitempty"`
	Packages         []PackageStatus `json:"packages"`
}

type debugFan struct {
	Available            bool        `json:"available"`
	DriverDetected       bool        `json:"driver_detected"`
	Active               bool        `json:"active"`
	TemperatureC         float64     `json:"temperature_c,omitempty"`
	CPUTemperatureC      float64     `json:"cpu_temperature_c,omitempty"`
	DiskTemperatureC     float64     `json:"disk_temperature_c,omitempty"`
	HDDTemperatureC      float64     `json:"hdd_temperature_c,omitempty"`
	NVMeTemperatureC     float64     `json:"nvme_temperature_c,omitempty"`
	TargetPWMPercent     int         `json:"target_pwm_percent,omitempty"`
	CPUTargetPWMPercent  int         `json:"cpu_target_pwm_percent,omitempty"`
	DiskTargetPWMPercent int         `json:"disk_target_pwm_percent,omitempty"`
	HDDTargetPWMPercent  int         `json:"hdd_target_pwm_percent,omitempty"`
	NVMeTargetPWMPercent int         `json:"nvme_target_pwm_percent,omitempty"`
	ControlSource        string      `json:"control_source,omitempty"`
	Fans                 []FanStatus `json:"fans"`
	LastError            string      `json:"last_error,omitempty"`
}

type debugStorage struct {
	UpdatedAt time.Time   `json:"updated_at,omitempty"`
	Slots     []debugSlot `json:"slots"`
	LastError string      `json:"last_error,omitempty"`
}

type debugSlot struct {
	ID           string  `json:"id"`
	Kind         string  `json:"kind"`
	Slot         int     `json:"slot"`
	State        string  `json:"state"`
	BusPath      string  `json:"bus_path"`
	Activity     string  `json:"activity"`
	Device       string  `json:"device"`
	Model        string  `json:"model"`
	Size         int64   `json:"size"`
	Purpose      string  `json:"purpose"`
	Health       string  `json:"health"`
	TemperatureC float64 `json:"temperature_c"`
	Warning      string  `json:"warning"`
	SMARTError   string  `json:"smart_error"`
}

type debugGPIO struct {
	Available bool              `json:"available"`
	Enabled   bool              `json:"enabled"`
	Buttons   []debugGPIOButton `json:"buttons"`
	LastError string            `json:"last_error,omitempty"`
}

type debugGPIOButton struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Port    string `json:"port"`
	Bit     uint   `json:"bit"`
	Pressed bool   `json:"pressed"`
	HeldMS  int64  `json:"held_ms,omitempty"`
}

type debugCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

var debugSecretPattern = regexp.MustCompile(`(?i)(token|password|passwd|secret|authorization)([[:space:]]*[=:][[:space:]]*)([^,;&[:space:]"'}]+)`)

func (s *Server) handleDebugReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	status := s.Manager.Status()
	storage := s.Manager.StorageStatus()
	writeJSON(w, http.StatusOK, s.Manager.debugReport(status, storage))
}

func (m *Manager) debugReport(status Status, storage StorageStatus) debugReport {
	report := debugReport{
		ReportVersion: debugReportVersion,
		CapturedAt:    time.Now(),
		PluginVersion: m.Version,
		Host:          debugHost{Name: status.DeviceName, OS: status.OSName, OSVersion: status.OSVersion},
		CPU: debugCPU{
			Model: status.CPUModel, Profile: status.Profile, Temperature: status.CPUTemperature.DisplayC,
			TemperatureStatus: status.CPUTemperature, Sensors: status.Temperatures, GPURuntime: status.GPURuntime,
			Available: status.CPUTemperature.Available,
		},
		Power: debugPower{Supported: status.Supported, EffectiveMaxPL1W: status.EffectiveMaxPL1W, EffectiveMaxPL2W: status.EffectiveMaxPL2W, Packages: status.Packages},
		Fan: debugFan{
			Available: status.FanControl.Available, DriverDetected: status.FanControl.DriverDetected, Active: status.FanControl.Active,
			TemperatureC: status.FanControl.TemperatureC, CPUTemperatureC: status.FanControl.CPUTemperatureC,
			DiskTemperatureC: status.FanControl.DiskTemperatureC, HDDTemperatureC: status.FanControl.HDDTemperatureC,
			NVMeTemperatureC: status.FanControl.NVMeTemperatureC, TargetPWMPercent: status.FanControl.TargetPWMPercent,
			CPUTargetPWMPercent: status.FanControl.CPUTargetPWMPercent, DiskTargetPWMPercent: status.FanControl.DiskTargetPWMPercent,
			HDDTargetPWMPercent: status.FanControl.HDDTargetPWMPercent, NVMeTargetPWMPercent: status.FanControl.NVMeTargetPWMPercent,
			ControlSource: status.FanControl.ControlSource, Fans: status.FanControl.Fans, LastError: debugError(m, status.FanControl.LastError),
		},
		Storage:   debugStorage{UpdatedAt: storage.UpdatedAt, LastError: debugError(m, storage.LastError)},
		GPIO:      debugGPIO{Available: status.GPIO.Available, Enabled: status.GPIO.Enabled, LastError: debugError(m, status.GPIO.LastError)},
		LastError: debugError(m, status.LastError),
	}
	for _, slot := range storage.Slots {
		report.Storage.Slots = append(report.Storage.Slots, debugSlot{
			ID: slot.ID, Kind: slot.Kind, Slot: slot.Slot, State: slot.State, BusPath: slot.BusPath, Activity: slot.Activity,
			Device: debugDeviceName(slot.Device), Model: slot.Model, Size: slot.SizeBytes, Purpose: slot.Purpose,
			Health: slot.Health, TemperatureC: slot.TemperatureC, Warning: debugError(m, slot.Warning), SMARTError: debugError(m, slot.SMARTError),
		})
	}
	for _, button := range status.GPIO.Buttons {
		report.GPIO.Buttons = append(report.GPIO.Buttons, debugGPIOButton{
			ID: button.ID, Name: button.Name, Port: button.Port, Bit: button.Bit,
			Pressed: button.Pressed, HeldMS: button.HeldMS,
		})
	}
	report.Checks = []debugCheck{
		{Name: "cpu.coretemp", Status: debugCPUCheck(status)},
		{Name: "rapl", Status: debugRAPLCheck(status)},
		{Name: "fan.it87", Status: debugFanCheck(status)},
		{Name: "storage", Status: debugStorageCheck(storage)},
		{Name: "gpio", Status: debugGPIOCheck(status)},
	}
	return report
}

func debugError(m *Manager, value string) string {
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	replacements := []struct{ from, to string }{
		{m.ConfigPath, "<config-path>"}, {m.StatePath, "<state-path>"},
	}
	if root := strings.TrimSpace(m.Root); root != "" && filepath.Clean(root) != string(filepath.Separator) {
		replacements = append(replacements, struct{ from, to string }{root, "<manager-root>"})
	}
	for _, replacement := range replacements {
		if replacement.from != "" {
			value = strings.ReplaceAll(value, replacement.from, replacement.to)
		}
	}
	value = debugSecretPattern.ReplaceAllString(value, `${1}$2<redacted>`)
	value = strings.TrimSpace(value)
	if utf8Value := []rune(value); len(utf8Value) > 512 {
		value = string(utf8Value[:512])
	}
	return value
}

func debugDeviceName(device string) string {
	if device == "" {
		return ""
	}
	return filepath.Base(device)
}

func debugCPUCheck(status Status) string {
	if status.CPUTemperature.Available {
		return "ok"
	}
	return "error"
}

func debugRAPLCheck(status Status) string {
	if len(status.Packages) > 0 {
		return "ok"
	}
	return "error"
}

func debugFanCheck(status Status) string {
	if !status.FanControl.DriverDetected {
		return "error"
	}
	if !status.FanControl.Available {
		return "warn"
	}
	return "ok"
}

func debugStorageCheck(status StorageStatus) string {
	if status.LastError != "" {
		return "error"
	}
	for _, slot := range status.Slots {
		if slot.SMARTError != "" || slot.Warning != "" || slot.State == StorageWarning {
			return "warn"
		}
	}
	return "ok"
}

func debugGPIOCheck(status Status) string {
	if status.GPIO.LastError != "" {
		return "error"
	}
	if !status.GPIO.Available {
		return "warn"
	}
	return "ok"
}
