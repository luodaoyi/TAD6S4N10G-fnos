package powerguard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const stateVersion = 1

var ErrOriginalStateCPUMismatch = errors.New("original state CPU model does not match current CPU")

type Profile struct {
	ID           string                 `json:"id"`
	Display      string                 `json:"display"`
	DefaultPL1   int64                  `json:"default_pl1_w"`
	DefaultPL2   int64                  `json:"default_pl2_w"`
	MinPL1       int64                  `json:"min_pl1_w"`
	MaxPL1       int64                  `json:"max_pl1_w"`
	MaxPL2       int64                  `json:"max_pl2_w"`
	PowerPresets map[string]PowerPreset `json:"power_presets"`
}

type PowerPreset struct {
	PL1W int64 `json:"pl1_w"`
	PL2W int64 `json:"pl2_w"`
}

var profiles = []Profile{
	{
		ID: "n305", Display: "Intel Core i3-N305", DefaultPL1: 15, DefaultPL2: 15, MinPL1: 6, MaxPL1: 20, MaxPL2: 35,
		PowerPresets: map[string]PowerPreset{
			"saving": {PL1W: 9, PL2W: 15}, "standard": {PL1W: 15, PL2W: 15}, "performance": {PL1W: 20, PL2W: 35},
		},
	},
	{
		ID: "n100", Display: "Intel Processor N100", DefaultPL1: 6, DefaultPL2: 15, MinPL1: 4, MaxPL1: 10, MaxPL2: 25,
		PowerPresets: map[string]PowerPreset{
			"saving": {PL1W: 4, PL2W: 8}, "standard": {PL1W: 6, PL2W: 15}, "performance": {PL1W: 10, PL2W: 25},
		},
	},
	{
		ID: "n150", Display: "Intel Processor N150", DefaultPL1: 6, DefaultPL2: 15, MinPL1: 4, MaxPL1: 15, MaxPL2: 25,
		PowerPresets: map[string]PowerPreset{
			"saving": {PL1W: 4, PL2W: 8}, "standard": {PL1W: 6, PL2W: 15}, "performance": {PL1W: 15, PL2W: 25},
		},
	},
}

type Config struct {
	Enabled        bool       `json:"enabled"`
	PL1W           int64      `json:"pl1_w"`
	PL2W           int64      `json:"pl2_w"`
	ReapplySeconds int        `json:"reapply_seconds"`
	Fan            FanConfig  `json:"fan"`
	GPIO           GPIOConfig `json:"gpio"`
}

type GlobalConfig struct {
	Enabled        bool  `json:"enabled"`
	PL1W           int64 `json:"pl1_w"`
	PL2W           int64 `json:"pl2_w"`
	ReapplySeconds int   `json:"reapply_seconds"`
}

type Constraint struct {
	Index     int    `json:"index"`
	Name      string `json:"name"`
	PowerPath string `json:"-"`
	MaxPath   string `json:"-"`
	CurrentUW int64  `json:"current_uw"`
	MaxUW     int64  `json:"max_uw,omitempty"`
}

type Package struct {
	Path  string      `json:"-"`
	Name  string      `json:"name"`
	Long  *Constraint `json:"long_term,omitempty"`
	Short *Constraint `json:"short_term,omitempty"`
}

type OriginalPackage struct {
	Name    string `json:"name"`
	LongUW  int64  `json:"long_uw"`
	ShortUW *int64 `json:"short_uw,omitempty"`
}

type OriginalState struct {
	Version    int               `json:"version"`
	CPUModel   string            `json:"cpu_model"`
	CapturedAt time.Time         `json:"captured_at"`
	Packages   []OriginalPackage `json:"packages"`
}

type Temperature struct {
	Label   string  `json:"label"`
	Celsius float64 `json:"celsius"`
}

type CPUTemperatureStatus struct {
	Available      bool    `json:"available"`
	DisplayC       float64 `json:"display_c,omitempty"`
	DisplaySource  string  `json:"display_source,omitempty"`
	CoreMaxC       float64 `json:"core_max_c,omitempty"`
	PackageMaxC    float64 `json:"package_max_c,omitempty"`
	CoreSensors    int     `json:"core_sensors"`
	PackageSensors int     `json:"package_sensors"`
}

type PackageStatus struct {
	Name    string `json:"name"`
	PL1W    int64  `json:"pl1_w,omitempty"`
	PL1MaxW int64  `json:"pl1_max_w,omitempty"`
	PL2W    int64  `json:"pl2_w,omitempty"`
	PL2MaxW int64  `json:"pl2_max_w,omitempty"`
	HasPL1  bool   `json:"has_pl1"`
	HasPL2  bool   `json:"has_pl2"`
}

type Status struct {
	Version          string               `json:"version"`
	DeviceName       string               `json:"device_name,omitempty"`
	OSName           string               `json:"os_name,omitempty"`
	OSVersion        string               `json:"os_version,omitempty"`
	CPUModel         string               `json:"cpu_model"`
	Profile          Profile              `json:"profile"`
	Supported        bool                 `json:"supported"`
	Config           Config               `json:"config"`
	EffectiveMaxPL1W int64                `json:"effective_max_pl1_w"`
	EffectiveMaxPL2W int64                `json:"effective_max_pl2_w"`
	Packages         []PackageStatus      `json:"packages"`
	Temperatures     []Temperature        `json:"temperatures"`
	CPUTemperature   CPUTemperatureStatus `json:"cpu_temperature"`
	GPURuntime       []string             `json:"gpu_runtime"`
	FanControl       FanControlStatus     `json:"fan_control"`
	Storage          StorageStatus        `json:"storage"`
	GPIO             GPIOStatus           `json:"gpio"`
	LastApply        time.Time            `json:"last_apply,omitempty"`
	LastError        string               `json:"last_error,omitempty"`
}

type Manager struct {
	Root       string
	ConfigPath string
	StatePath  string
	Version    string

	mu            sync.Mutex
	lastApply     time.Time
	lastError     string
	fanLastApply  time.Time
	fanLastError  string
	fanLastTarget int
	fanLastTemp   float64

	storageMu     sync.RWMutex
	storageScanMu sync.Mutex
	storageStatus StorageStatus
	gpioMu        sync.Mutex
	gpioRuntime   gpioRuntime
}

func DetectProfile(model string) (Profile, error) {
	normalized := strings.ToLower(strings.Join(strings.Fields(model), " "))
	if strings.Contains(normalized, "i3-n305") || strings.Contains(normalized, "i3 n305") {
		return profiles[0], nil
	}
	for _, token := range strings.FieldsFunc(normalized, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		switch token {
		case "n100":
			return profiles[1], nil
		case "n150":
			return profiles[2], nil
		}
	}
	return Profile{}, fmt.Errorf("unsupported CPU %q; only Intel N100, N150 and i3-N305 are supported", strings.TrimSpace(model))
}

func DefaultConfig(profile Profile) Config {
	return Config{
		Enabled: true, PL1W: profile.DefaultPL1, PL2W: profile.DefaultPL2,
		ReapplySeconds: 30, Fan: DefaultFanConfig(), GPIO: DefaultGPIOConfig(),
	}
}

func (m *Manager) CPUModel() (string, error) {
	data, err := os.ReadFile(m.rooted("/proc/cpuinfo"))
	if err != nil {
		return "", fmt.Errorf("read /proc/cpuinfo: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 && strings.TrimSpace(parts[0]) == "model name" {
			model := strings.TrimSpace(parts[1])
			if model != "" {
				return model, nil
			}
		}
	}
	return "", errors.New("CPU model name was not found in /proc/cpuinfo")
}

func (m *Manager) deviceSystemInfo() (string, string, string) {
	deviceName, _ := readTrim(m.rooted("/etc/hostname"))
	if deviceName == "" && (m.Root == "" || m.Root == "/") {
		deviceName, _ = os.Hostname()
	}

	osName, osVersion := "", ""
	if data, err := os.ReadFile(m.rooted("/etc/os-release")); err == nil {
		values := make(map[string]string)
		for _, line := range strings.Split(string(data), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), "\"'")
		}
		osName = values["NAME"]
		if osName == "" {
			osName = values["PRETTY_NAME"]
		}
		osVersion = values["VERSION_ID"]
		if osVersion == "" {
			osVersion = values["VERSION"]
		}
	}
	if data, err := os.ReadFile(m.rooted("/etc/issue")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(strings.ToLower(line), "os version:") {
				continue
			}
			value := strings.TrimSpace(strings.TrimPrefix(line, "OS version:"))
			if strings.HasPrefix(strings.ToLower(value), "fnos") {
				osName = "fnOS"
				osVersion = strings.TrimPrefix(strings.TrimSpace(value[len("fnOS"):]), "v")
			}
			break
		}
	}
	for _, path := range []string{"/etc/fnos-release", "/etc/fnos_version"} {
		if value, err := readTrim(m.rooted(path)); err == nil && value != "" {
			osName = "fnOS"
			if key, parsed, ok := strings.Cut(value, "="); ok {
				if strings.Contains(strings.ToLower(key), "version") {
					value = strings.Trim(strings.TrimSpace(parsed), "\"'")
				}
			}
			osVersion = value
			break
		}
	}
	return deviceName, osName, osVersion
}

func (m *Manager) LoadOrCreateConfig() (Config, error) {
	model, err := m.CPUModel()
	if err != nil {
		return Config{}, err
	}
	profile, err := DetectProfile(model)
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(m.ConfigPath)
	if errors.Is(err, fs.ErrNotExist) {
		cfg := DefaultConfig(profile)
		if err := writeJSONAtomic(m.ConfigPath, cfg, 0o600); err != nil {
			return Config{}, err
		}
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if normalizeConfig(&cfg) {
		if err := writeJSONAtomic(m.ConfigPath, cfg, 0o600); err != nil {
			return Config{}, fmt.Errorf("migrate config: %w", err)
		}
	}
	return cfg, nil
}

func (m *Manager) SaveAndApply(cfg Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	normalizeConfig(&cfg)
	previous, previousErr := m.loadConfigLocked()
	if err := m.validateLocked(cfg); err != nil {
		m.lastError = err.Error()
		return err
	}
	if err := writeJSONAtomic(m.ConfigPath, cfg, 0o600); err != nil {
		m.lastError = err.Error()
		return err
	}
	var errs []error
	if previousErr == nil && previous.Fan.Enabled && !cfg.Fan.Enabled {
		errs = append(errs, m.restoreFansLocked())
	}
	errs = append(errs, m.applyLocked(cfg))
	err := errors.Join(errs...)
	if err != nil {
		m.lastError = err.Error()
		return err
	}
	m.lastError = ""
	m.lastApply = time.Now()
	return nil
}

func (m *Manager) SaveGlobalConfig(global GlobalConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.loadConfigLocked()
	if err != nil {
		m.lastError = err.Error()
		return err
	}
	cfg.Enabled = global.Enabled
	cfg.PL1W = global.PL1W
	cfg.PL2W = global.PL2W
	cfg.ReapplySeconds = global.ReapplySeconds
	if err := m.validateGlobalLocked(cfg); err != nil {
		m.lastError = err.Error()
		return err
	}
	if err := writeJSONAtomic(m.ConfigPath, cfg, 0o600); err != nil {
		m.lastError = err.Error()
		return err
	}
	if cfg.Enabled {
		err = m.applyPowerLocked(cfg)
	} else {
		err = m.restorePowerLocked()
	}
	if err != nil {
		m.lastError = err.Error()
		return err
	}
	m.lastError = ""
	m.lastApply = time.Now()
	return nil
}

func (m *Manager) SaveFanConfig(fan FanConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.loadConfigLocked()
	if err != nil {
		m.lastError = err.Error()
		return err
	}
	previous := cfg.Fan
	cfg.Fan = fan
	normalizeConfig(&cfg)
	if err := m.validateFanLocked(cfg.Fan); err != nil {
		m.lastError = err.Error()
		return err
	}
	if err := writeJSONAtomic(m.ConfigPath, cfg, 0o600); err != nil {
		m.lastError = err.Error()
		return err
	}
	if previous.Enabled && !cfg.Fan.Enabled {
		err = m.restoreFansLocked()
	} else if cfg.Fan.Enabled {
		err = m.applyFanLocked(cfg.Fan)
	}
	if err != nil {
		m.lastError = err.Error()
		return err
	}
	m.lastError = ""
	m.lastApply = time.Now()
	return nil
}

func (m *Manager) SaveGPIOConfig(gpio GPIOConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.loadConfigLocked()
	if err != nil {
		m.lastError = err.Error()
		return err
	}
	cfg.GPIO = gpio
	normalizeConfig(&cfg)
	if err := validateGPIOConfig(cfg.GPIO); err != nil {
		m.lastError = err.Error()
		return err
	}
	if err := writeJSONAtomic(m.ConfigPath, cfg, 0o600); err != nil {
		m.lastError = err.Error()
		return err
	}
	m.lastError = ""
	return nil
}

func (m *Manager) ApplyCurrent() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.loadConfigLocked()
	if err == nil {
		err = m.validateLocked(cfg)
	}
	if err == nil {
		err = m.applyLocked(cfg)
	}
	if err != nil {
		m.lastError = err.Error()
		return err
	}
	m.lastError = ""
	m.lastApply = time.Now()
	return nil
}

func (m *Manager) Restore() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.restoreLocked()
	if err != nil {
		m.lastError = err.Error()
		return err
	}
	m.lastError = ""
	return nil
}

func (m *Manager) DisableAndRestore() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.loadConfigLocked()
	if err != nil {
		return err
	}
	cfg.Enabled = false
	cfg.Fan.Enabled = false
	cfg.GPIO.Enabled = false
	if err := m.validateLocked(cfg); err != nil {
		return err
	}
	if err := writeJSONAtomic(m.ConfigPath, cfg, 0o600); err != nil {
		return err
	}
	if err := m.restoreLocked(); err != nil {
		m.lastError = err.Error()
		return err
	}
	m.lastError = ""
	return nil
}

func (m *Manager) Validate(cfg Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.validateLocked(cfg)
}

func (m *Manager) validateLocked(cfg Config) error {
	if err := m.validateGlobalLocked(cfg); err != nil {
		return err
	}
	if err := m.validateFanLocked(cfg.Fan); err != nil {
		return err
	}
	return validateGPIOConfig(cfg.GPIO)
}

func (m *Manager) validateGlobalLocked(cfg Config) error {
	model, err := m.CPUModel()
	if err != nil {
		return err
	}
	profile, err := DetectProfile(model)
	if err != nil {
		return err
	}
	if cfg.ReapplySeconds < 5 || cfg.ReapplySeconds > 300 {
		return errors.New("reapply_seconds must be between 5 and 300")
	}
	if cfg.PL1W < profile.MinPL1 || cfg.PL1W > profile.MaxPL1 {
		return fmt.Errorf("PL1 must be between %dW and %dW for %s", profile.MinPL1, profile.MaxPL1, profile.Display)
	}
	if cfg.PL2W < cfg.PL1W || cfg.PL2W > profile.MaxPL2 {
		return fmt.Errorf("PL2 must be at least PL1 and no more than %dW for %s", profile.MaxPL2, profile.Display)
	}
	packages, err := m.DiscoverPackages()
	if err != nil {
		return err
	}
	for _, pkg := range packages {
		if pkg.Long == nil {
			return fmt.Errorf("%s has no long_term RAPL constraint", pkg.Name)
		}
		if pkg.Long.MaxUW > 0 && cfg.PL1W*1_000_000 > pkg.Long.MaxUW {
			return fmt.Errorf("PL1 %dW exceeds %s hardware long-term maximum %dW", cfg.PL1W, pkg.Name, pkg.Long.MaxUW/1_000_000)
		}
		if pkg.Short != nil && pkg.Short.MaxUW > 0 && cfg.PL2W*1_000_000 > pkg.Short.MaxUW {
			return fmt.Errorf("PL2 %dW exceeds %s hardware short-term maximum %dW", cfg.PL2W, pkg.Name, pkg.Short.MaxUW/1_000_000)
		}
	}
	return nil
}

func (m *Manager) DiscoverPackages() ([]Package, error) {
	patterns := []string{
		m.rooted("/sys/devices/virtual/powercap/intel-rapl/intel-rapl:*"),
		m.rooted("/sys/class/powercap/intel-rapl:*"),
	}
	seen := make(map[string]bool)
	var packages []Package
	for _, pattern := range patterns {
		paths, _ := filepath.Glob(pattern)
		for _, path := range paths {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				resolved = path
			}
			if seen[resolved] {
				continue
			}
			name, err := readTrim(filepath.Join(path, "name"))
			if err != nil || !strings.HasPrefix(name, "package-") {
				continue
			}
			pkg := Package{Path: path, Name: name}
			for i := 0; i < 8; i++ {
				constraintName, err := readTrim(filepath.Join(path, fmt.Sprintf("constraint_%d_name", i)))
				if err != nil {
					continue
				}
				powerPath := filepath.Join(path, fmt.Sprintf("constraint_%d_power_limit_uw", i))
				current, err := readInt(powerPath)
				if err != nil {
					continue
				}
				maxPath := filepath.Join(path, fmt.Sprintf("constraint_%d_max_power_uw", i))
				maximum, _ := readInt(maxPath)
				constraint := &Constraint{Index: i, Name: constraintName, PowerPath: powerPath, MaxPath: maxPath, CurrentUW: current, MaxUW: maximum}
				switch constraintName {
				case "long_term":
					pkg.Long = constraint
				case "short_term":
					pkg.Short = constraint
				}
			}
			seen[resolved] = true
			packages = append(packages, pkg)
		}
	}
	if len(packages) == 0 {
		return nil, errors.New("no Intel RAPL package power zone was found")
	}
	sort.Slice(packages, func(i, j int) bool { return packages[i].Name < packages[j].Name })
	return packages, nil
}

func (m *Manager) applyLocked(cfg Config) error {
	var errs []error
	if cfg.Enabled {
		errs = append(errs, m.applyPowerLocked(cfg))
	} else {
		errs = append(errs, m.restorePowerLocked())
	}
	if cfg.Fan.Enabled {
		errs = append(errs, m.applyFanLocked(cfg.Fan))
	}
	return errors.Join(errs...)
}

func (m *Manager) applyPowerLocked(cfg Config) error {
	packages, err := m.DiscoverPackages()
	if err != nil {
		return err
	}
	if err := m.captureOriginalLocked(packages); err != nil {
		return err
	}
	type snapshot struct {
		pkg     Package
		longUW  int64
		shortUW *int64
	}
	var before []snapshot
	for _, pkg := range packages {
		item := snapshot{pkg: pkg, longUW: pkg.Long.CurrentUW}
		if pkg.Short != nil {
			value := pkg.Short.CurrentUW
			item.shortUW = &value
		}
		before = append(before, item)
	}
	desiredLong := cfg.PL1W * 1_000_000
	desiredShort := cfg.PL2W * 1_000_000
	for _, item := range before {
		if err := applyPackage(item.pkg, desiredLong, desiredShort); err != nil {
			for _, rollback := range before {
				_ = applyPackage(rollback.pkg, rollback.longUW, valueOr(rollback.shortUW, rollback.longUW))
			}
			return err
		}
	}
	return nil
}

func applyPackage(pkg Package, desiredLong, desiredShort int64) error {
	if pkg.Long == nil {
		return fmt.Errorf("%s has no long_term constraint", pkg.Name)
	}
	writeLong := func() error { return writeAndVerify(pkg.Long.PowerPath, desiredLong) }
	writeShort := func() error {
		if pkg.Short == nil {
			return nil
		}
		return writeAndVerify(pkg.Short.PowerPath, desiredShort)
	}
	if pkg.Short != nil && desiredShort < pkg.Long.CurrentUW {
		if err := writeLong(); err != nil {
			return fmt.Errorf("set %s PL1: %w", pkg.Name, err)
		}
		if err := writeShort(); err != nil {
			return fmt.Errorf("set %s PL2: %w", pkg.Name, err)
		}
		return nil
	}
	if err := writeShort(); err != nil {
		return fmt.Errorf("set %s PL2: %w", pkg.Name, err)
	}
	if err := writeLong(); err != nil {
		return fmt.Errorf("set %s PL1: %w", pkg.Name, err)
	}
	return nil
}

func (m *Manager) captureOriginalLocked(packages []Package) error {
	if _, err := os.Stat(m.StatePath); err == nil {
		state, err := m.loadOriginalState()
		if err != nil {
			return err
		}
		if state.Version != stateVersion {
			return fmt.Errorf("unsupported state version %d", state.Version)
		}
		return m.validateOriginalStateCPU(state)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat state: %w", err)
	}
	model, err := m.CPUModel()
	if err != nil {
		return err
	}
	state := OriginalState{Version: stateVersion, CPUModel: model, CapturedAt: time.Now()}
	for _, pkg := range packages {
		if pkg.Long == nil {
			continue
		}
		original := OriginalPackage{Name: pkg.Name, LongUW: pkg.Long.CurrentUW}
		if pkg.Short != nil {
			value := pkg.Short.CurrentUW
			original.ShortUW = &value
		}
		state.Packages = append(state.Packages, original)
	}
	if len(state.Packages) == 0 {
		return errors.New("no original RAPL values could be captured")
	}
	return writeJSONAtomic(m.StatePath, state, 0o600)
}

func (m *Manager) restoreLocked() error {
	return errors.Join(m.restorePowerLocked(), m.restoreFansLocked())
}

func (m *Manager) restorePowerLocked() error {
	state, err := m.loadOriginalState()
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if state.Version != stateVersion {
		return fmt.Errorf("unsupported state version %d", state.Version)
	}
	if err := m.validateOriginalStateCPU(state); err != nil {
		return err
	}
	packages, err := m.DiscoverPackages()
	if err != nil {
		return err
	}
	byName := make(map[string]Package, len(packages))
	for _, pkg := range packages {
		byName[pkg.Name] = pkg
	}
	var errs []error
	for _, original := range state.Packages {
		pkg, ok := byName[original.Name]
		if !ok {
			errs = append(errs, fmt.Errorf("RAPL package %s is no longer present", original.Name))
			continue
		}
		if err := applyPackage(pkg, original.LongUW, valueOr(original.ShortUW, original.LongUW)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) loadOriginalState() (OriginalState, error) {
	data, err := os.ReadFile(m.StatePath)
	if err != nil {
		return OriginalState{}, fmt.Errorf("read state: %w", err)
	}
	var state OriginalState
	if err := json.Unmarshal(data, &state); err != nil {
		return OriginalState{}, fmt.Errorf("decode state: %w", err)
	}
	return state, nil
}

func (m *Manager) validateOriginalStateCPU(state OriginalState) error {
	current, err := m.CPUModel()
	if err != nil {
		return err
	}
	savedModel := normalizeCPUModel(state.CPUModel)
	currentModel := normalizeCPUModel(current)
	if savedModel == "" {
		return errors.New("original state has no CPU model; refusing to reuse it")
	}
	if savedModel != currentModel {
		return fmt.Errorf("%w: state=%q current=%q", ErrOriginalStateCPUMismatch, state.CPUModel, current)
	}
	return nil
}

func (m *Manager) validateSavedHardwareIfAvailable() error {
	state, err := m.loadOriginalState()
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if state.Version != stateVersion {
		return fmt.Errorf("unsupported state version %d", state.Version)
	}
	return m.validateOriginalStateCPU(state)
}

func normalizeCPUModel(model string) string {
	return strings.ToLower(strings.Join(strings.Fields(model), " "))
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := Status{Version: m.Version, LastApply: m.lastApply, LastError: m.lastError}
	status.DeviceName, status.OSName, status.OSVersion = m.deviceSystemInfo()
	if m.fanLastError != "" {
		status.LastError = combineError(status.LastError, errors.New(m.fanLastError))
	}
	model, err := m.CPUModel()
	if err != nil {
		status.LastError = combineError(status.LastError, err)
		return status
	}
	status.CPUModel = model
	profile, err := DetectProfile(model)
	if err != nil {
		status.LastError = combineError(status.LastError, err)
		return status
	}
	status.Profile = profile
	status.Supported = true
	cfg, err := m.loadConfigLocked()
	if err != nil {
		status.LastError = combineError(status.LastError, err)
	} else {
		status.Config = cfg
	}
	packages, err := m.DiscoverPackages()
	if err != nil {
		status.LastError = combineError(status.LastError, err)
	} else {
		status.EffectiveMaxPL1W = profile.MaxPL1
		status.EffectiveMaxPL2W = profile.MaxPL2
		for _, pkg := range packages {
			item := PackageStatus{Name: pkg.Name}
			if pkg.Long != nil {
				item.HasPL1 = true
				item.PL1W = pkg.Long.CurrentUW / 1_000_000
				item.PL1MaxW = pkg.Long.MaxUW / 1_000_000
				if item.PL1MaxW > 0 && item.PL1MaxW < status.EffectiveMaxPL1W {
					status.EffectiveMaxPL1W = item.PL1MaxW
				}
			}
			if pkg.Short != nil {
				item.HasPL2 = true
				item.PL2W = pkg.Short.CurrentUW / 1_000_000
				item.PL2MaxW = pkg.Short.MaxUW / 1_000_000
				if item.PL2MaxW > 0 && item.PL2MaxW < status.EffectiveMaxPL2W {
					status.EffectiveMaxPL2W = item.PL2MaxW
				}
			}
			status.Packages = append(status.Packages, item)
		}
	}
	status.Temperatures = m.temperatures()
	status.CPUTemperature = summarizeCPUTemperatures(status.Temperatures)
	status.GPURuntime = m.gpuRuntime()
	status.FanControl = m.fanStatusLocked(status.Config.Fan)
	status.Storage = m.StorageStatus()
	status.GPIO = m.GPIOStatus(status.Config.GPIO)
	return status
}

func (m *Manager) loadConfigLocked() (Config, error) {
	data, err := os.ReadFile(m.ConfigPath)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	normalizeConfig(&cfg)
	return cfg, nil
}

func (m *Manager) temperatures() []Temperature {
	namePaths, _ := filepath.Glob(m.rooted("/sys/class/hwmon/hwmon*/name"))
	var result []Temperature
	for _, namePath := range namePaths {
		name, err := readTrim(namePath)
		if err != nil || name != "coretemp" {
			continue
		}
		dir := filepath.Dir(namePath)
		inputs, _ := filepath.Glob(filepath.Join(dir, "temp*_input"))
		for _, input := range inputs {
			base := strings.TrimSuffix(filepath.Base(input), "_input")
			label, _ := readTrim(filepath.Join(dir, base+"_label"))
			if label == "" {
				label = name + "/" + base
			}
			value, err := readInt(input)
			if err == nil {
				result = append(result, Temperature{Label: label, Celsius: float64(value) / 1000})
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Label < result[j].Label })
	return result
}

func summarizeCPUTemperatures(temperatures []Temperature) CPUTemperatureStatus {
	var result CPUTemperatureStatus
	for _, temperature := range temperatures {
		switch {
		case strings.HasPrefix(temperature.Label, "Core "):
			if result.CoreSensors == 0 || temperature.Celsius > result.CoreMaxC {
				result.CoreMaxC = temperature.Celsius
			}
			result.CoreSensors++
		case strings.HasPrefix(temperature.Label, "Package id "):
			if result.PackageSensors == 0 || temperature.Celsius > result.PackageMaxC {
				result.PackageMaxC = temperature.Celsius
			}
			result.PackageSensors++
		}
	}
	if result.CoreSensors > 0 {
		result.Available = true
		result.DisplayC = result.CoreMaxC
		result.DisplaySource = "core_max_rr"
	} else if result.PackageSensors > 0 {
		result.Available = true
		result.DisplayC = result.PackageMaxC
		result.DisplaySource = "package_fallback"
	}
	return result
}

func (m *Manager) gpuRuntime() []string {
	paths, _ := filepath.Glob(m.rooted("/sys/class/drm/card*/device/power/runtime_status"))
	var result []string
	for _, path := range paths {
		value, err := readTrim(path)
		if err == nil {
			card := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path))))
			result = append(result, card+": "+value)
		}
	}
	sort.Strings(result)
	return result
}

func (m *Manager) rooted(path string) string {
	if m.Root == "" || m.Root == "/" {
		return path
	}
	return filepath.Join(m.Root, filepath.FromSlash(strings.TrimPrefix(path, "/")))
}

func writeAndVerify(path string, value int64) error {
	if err := os.WriteFile(path, []byte(strconv.FormatInt(value, 10)), 0o600); err != nil {
		return err
	}
	actual, err := readInt(path)
	if err != nil {
		return err
	}
	if actual != value {
		return fmt.Errorf("kernel returned %d instead of %d", actual, value)
	}
	return nil
}

func writeJSONAtomic(path string, value any, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tad-module-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func readTrim(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func readInt(path string) (int64, error) {
	value, err := readTrim(path)
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}
	return parsed, nil
}

func valueOr(value *int64, fallback int64) int64 {
	if value == nil {
		return fallback
	}
	return *value
}

func combineError(existing string, err error) string {
	if err == nil {
		return existing
	}
	if existing == "" {
		return err.Error()
	}
	return existing + "; " + err.Error()
}
