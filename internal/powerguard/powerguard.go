package powerguard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	Enabled        bool                   `json:"enabled"`
	PL1W           int64                  `json:"pl1_w"`
	PL2W           int64                  `json:"pl2_w"`
	ReapplySeconds int                    `json:"reapply_seconds"`
	Fan            FanConfig              `json:"fan"`
	GPIO           GPIOConfig             `json:"gpio"`
	History        HistoryConfig          `json:"history"`                 // 历史温度：采样开关、数据库大小上限与保留天数
	Log            LogConfig              `json:"log"`                     // 运行日志：大小上限（与历史数据库上限解耦）
	SensorNames    map[string]string      `json:"sensor_names,omitempty"`  // 传感器显示名（键为 hwmon 芯片:标签）
	SensorGroups   map[string]string      `json:"sensor_groups,omitempty"` // 传感器父类归属覆盖（键同上，值 gpu|nic|other；缺省按驱动表）
	Serial         *SerialSensorConfig    `json:"serial,omitempty"`        // 旧版单设备串口配置：读入即迁移进 Serials 并置 nil（见 normalizeConfig）
	Serials        []SerialSensorConfig   `json:"serials,omitempty"`       // USB 串口外置温度传感器（usb_serial.go：文本行源，多设备）
	FanRPMBase     map[string]int         `json:"fan_rpm_base,omitempty"`  // 风扇满转基准（键为风扇 ID；全速运转时按实测自动标定，缺省 2000）
	FanRPMMap      map[string]map[int]int `json:"fan_rpm_map,omitempty"`   // 风扇 PWM→转速特性表（键为风扇 ID,内层键为 16 步长 PWM 档位；稳态工况自动学习）
	UIPrefs        UIPrefsConfig          `json:"ui_prefs"`                // 前端界面偏好（随 status 下发，独立小接口保存）
}

// UIPrefsConfig 纯界面偏好，与功能配置分开存放：历史/风扇等保存接口整段
// 替换各自配置，混进去的界面偏好会被误覆盖。零值表示未设置。
type UIPrefsConfig struct {
	HistoryRangeHours float64 `json:"history_range_hours,omitempty"` // 历史温度时间范围档位（小时）
	FanDebugVisible   bool    `json:"fan_debug_visible,omitempty"`   // 调试页风扇调试卡片是否显示（跨浏览器跟随账号）
	// 历史温度曲线显隐偏好（跨浏览器跟随账号）：父类开关与组内勾选。
	// HistorySeries 值为该父类是否画曲线；HistoryChildren 为组内勾选的子曲线
	// ID 列表（cpu/sata/nvme 的 "__agg__" 是聚合"取最高"项），null/缺省键 =
	// 前端默认（温度组只画聚合线、风扇组全画）；空列表 = 组开但一条不画，
	// 不能丢键——omitempty 只在 map 整体为 nil 时省略字段，内层空列表保留。
	HistorySeries   map[string]bool     `json:"history_series,omitempty"`
	HistoryChildren map[string][]string `json:"history_children,omitempty"`
}

// LogConfig 运行日志的大小设置：与历史数据库大小上限解耦。日志体量小，
// 阈值应小而灵敏——共用大上限会让清理在 1024M 档形同虚设（刷屏时日志
// 可堆到 2×上限才收敛）。
type LogConfig struct {
	MaxSizeMB int64 `json:"max_size_mb"`
}

const (
	logDefaultMaxSizeMB = 16
	logMinMaxSizeMB     = 1
	logMaxMaxSizeMB     = 256
)

func DefaultLogConfig() LogConfig {
	return LogConfig{MaxSizeMB: logDefaultMaxSizeMB}
}

// ClampLogMaxSize 把日志大小上限限制在合理区间，配置文件里的非法值静默归位。
func ClampLogMaxSize(maxSizeMB int64) int64 {
	if maxSizeMB < logMinMaxSizeMB {
		return logMinMaxSizeMB
	}
	if maxSizeMB > logMaxMaxSizeMB {
		return logMaxMaxSizeMB
	}
	return maxSizeMB
}

// ClampUIHistoryRangeHours 界面档位只做范围钳制（0.5 小时–30 天），
// 区间外的值按未设置（0）处理；合法档位集合由前端维护，后端不感知。
func ClampUIHistoryRangeHours(hours float64) float64 {
	if hours < 0.5 || hours > 720 {
		return 0
	}
	return hours
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
	Version           string               `json:"version"`
	DeviceName        string               `json:"device_name,omitempty"`
	OSName            string               `json:"os_name,omitempty"`
	OSVersion         string               `json:"os_version,omitempty"`
	CPUModel          string               `json:"cpu_model"`
	Profile           Profile              `json:"profile"`
	Supported         bool                 `json:"supported"`
	Config            Config               `json:"config"`
	EffectiveMaxPL1W  int64                `json:"effective_max_pl1_w"`
	EffectiveMaxPL2W  int64                `json:"effective_max_pl2_w"`
	Packages          []PackageStatus      `json:"packages"`
	Temperatures      []Temperature        `json:"temperatures"`
	CPUTemperature    CPUTemperatureStatus `json:"cpu_temperature"`
	GPURuntime        []string             `json:"gpu_runtime"`
	ExtraTemperatures []Temperature        `json:"extra_temperatures,omitempty"`
	FanControl        FanControlStatus     `json:"fan_control"`
	Storage           StorageStatus        `json:"storage"`
	GPIO              GPIOStatus           `json:"gpio"`
	Serial            SerialSensorInfo     `json:"serial"`
	LastApply         time.Time            `json:"last_apply,omitempty"`
	LastError         string               `json:"last_error,omitempty"`
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

	// 风扇调试模式（运行时状态，不落盘；重启即恢复曲线控制）
	fanDebugTakenOver       map[string]int
	fanDebugUnits           map[string]string // 被接管风扇的调节单位(percent/pwm),与 TakenOver 同键 // 被调试接管的风扇 ID → 调试转速(%);接管的风扇不受任何曲线控制
	fanDebugEmergency       bool
	fanDebugLastError       string
	fanDebugAuto            *fanDebugAutoTest
	fanDebugAutoLoopRunning bool // 递增 goroutine 存活标记(无 Running 条目时退出)

	// 风扇满转基准（RPM 调试模式的换算分母，落盘 Config.FanRPMBase）与
	// PWM→转速特性表（落盘 Config.FanRPMMap）：稳态工况自动学习。窗口只被
	// 学习路径串行访问；表/基准的读写走 m.mu。
	fanRPMBase      map[string]int
	fanRPMBaseLoad  sync.Once         // 无统一构造函数，首次使用时从配置惰性载入
	fanRPMCalib     chan fanRPMSample // DiscoverFans 的非阻塞投递；nil = 尚未启用
	fanRPMCalibOnce sync.Once

	// RPM 闭环（设定目标转速后微调 PWM 直到 |实测-目标| ≤ 容差）：目标复用
	// fanDebugTakenOver（rpm 单位值），活跃集合每 tick 动态判定——切单位、
	// 取消接管、自动递增运行中的风扇自动退出闭环，无需显式注销。
	fanRPMLearned       map[string]map[int]int       // PWM 档位 → 稳态转速（内存，落盘 Config.FanRPMMap）
	fanRPMMapLoad       sync.Once                    // 特性表惰性载入（同 fanRPMBaseLoad）
	fanRPMSlotWin       map[string]*fanRPMSlotWindow // 当前档位的稳态窗口（换档即覆盖）
	fanRPMLoopRunning   bool                         // 闭环 goroutine 存活标记(无活跃目标时退出)
	fanRPMLoopMiss      map[string]int               // 连续不可达计数(达标清零;超限暂停该风扇微调)
	fanRPMLoopLocked    map[string]bool              // 当前是否在容差内(随调试状态下发)
	fanRPMLoopSettling  map[string]bool              // 上一轮刚写过 PWM:本轮只观察不调——风扇转速惯性 2–5 秒大于 tick 2 秒,基于过渡态连环调整必然正负过调震荡
	fanRPMLoopLastDir   map[string]int               // 上一轮微调方向(+1/-1):反向时步长减半,过调回摆逐次收敛
	fanRPMLastSave      time.Time                    // 特性表上次落盘时刻(学习路径写,节流用)
	fanRPMDirty         bool                         // 有未落盘的学习更新(被节流悬着,下个采样点补落盘)
	fanRPMLearnStop     chan struct{}                // 停止学习路径(测试收尾;生产进程退出即亡,恒 nil)
	fanRPMLearnDone     chan struct{}                // 学习 goroutine 退出信号(停止后等待在途写盘完成)
	fanRPMLearnDisabled atomic.Bool                  // 学习已停用(测试收尾置位):挡住启动与投递
	fanRPMLearnStopOnce sync.Once

	// 运行日志器（main 注入）：配置保存等需要用户可见痕迹的动作写这里；
	// 未注入（单测）静默。
	logger *log.Logger

	// 主动标定期间挂起对应风扇的 RPM 闭环（避免闭环微调与全速标定互相打架）
	fanRPMSuspend map[string]bool

	storageMu     sync.RWMutex
	storageScanMu sync.Mutex
	storageStatus StorageStatus
	gpioMu        sync.Mutex
	gpioRuntime   gpioRuntime
	usbLastError  string // USB 温度计最近一次读取错误（变化才记日志），随 m.mu 保护
	// USB HID 温度计读数缓存（usb_temper.go）：读取是秒级阻塞 IO，只能在
	// USBTemperatureLoop 里锁外做；extraTemperatures 只采信新鲜缓存。随 m.mu 保护。
	usbTempReadings []usbTempReading
	usbTempAt       time.Time
	// 日志大小上限的原子快照：写入路径兜底（SizeCappedLogWriter.Write）可能在
	// 持有 m.mu 的 logf 里被触发，绝不能在那里调 LogSettings()（拿 m.mu，
	// 重入死锁）。启动与 SaveLogConfig 时 Store，日志链路只 Load。
	logLimitBytes atomic.Int64

	// USB 串口温度传感器（usb_serial.go）：SerialSensorLoop 监督者与各设备
	// 读取器的运行状态，全部随 m.mu 保护。
	serialKick    chan struct{}                 // 配置保存后通知监督者重新对账（容量 1，非阻塞）
	serialReaders map[string]*serialReaderState // 在跑的读取器（键为设备路径，读取器退出时自行摘除）
}

// SetLogger 注入运行日志器（main 启动时调用）。
func (m *Manager) SetLogger(logger *log.Logger) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logger = logger
}

// logf 在注入了日志器时记一条；未注入（单测）静默。调用方须持 m.mu。
func (m *Manager) logf(format string, args ...any) {
	if m.logger != nil {
		m.logger.Printf(format, args...)
	}
}

// saveConfigLocked 用户主动保存的统一写盘出口：成功即记一条运行日志。
// 排查"设置没同步"时，日志里有保存记录 = 客户当时确实点了保存；自动学习
// 等内部写盘不走这里（高频且非用户点击，会稀释信号）。调用方须持 m.mu。
func (m *Manager) saveConfigLocked(cfg Config) error {
	if err := writeJSONAtomic(m.ConfigPath, cfg, 0o600); err != nil {
		return err
	}
	m.logf("config.json saved")
	return nil
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
		History: DefaultHistoryConfig(), Log: DefaultLogConfig(),
	}
}

// SaveHistoryConfig 只保存历史温度配置（采样开关、大小上限、保留天数与长期
// 记录），不触碰功耗/风扇状态。保留期与长期记录的即时生效由 HTTP 层调用
// SyncSettings 完成。长期记录目录在保存时验证：可创建 + 可写——配置坏了当场
// 报错，好过运行期才发现归档一直失败。
func (m *Manager) SaveHistoryConfig(history HistoryConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.loadConfigLocked()
	if err != nil {
		m.lastError = err.Error()
		return err
	}
	cfg.History = history
	normalizeConfig(&cfg)
	if cfg.History.ArchiveEnabled {
		dir := filepath.Clean(strings.TrimSpace(cfg.History.ArchiveDir))
		if !filepath.IsAbs(dir) || dir == "." || dir == string(filepath.Separator) {
			return errors.New("长期记录保存位置必须是绝对路径（如 /vol1/1000/长期记录）")
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("长期记录目录不可创建: %w", err)
		}
		probe := filepath.Join(dir, ".tad-archive-probe")
		if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
			return fmt.Errorf("长期记录目录不可写: %w", err)
		}
		_ = os.Remove(probe)
		cfg.History.ArchiveDir = dir
	}
	if err := m.saveConfigLocked(cfg); err != nil {
		m.lastError = err.Error()
		return err
	}
	m.lastError = ""
	return nil
}

// HistorySettings 供采样循环读取历史配置；读文件失败按默认值降级（继续采样）。
func (m *Manager) HistorySettings() HistoryConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.loadConfigLocked()
	if err != nil {
		return DefaultHistoryConfig()
	}
	return cfg.History
}

// SaveLogConfig 只保存运行日志的大小设置，不触碰其它配置；非法值由
// normalizeConfig 静默钳制。轮转循环每 10 分钟读一次实时值，保存后最迟
// 一个周期生效。
func (m *Manager) SaveLogConfig(logCfg LogConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.loadConfigLocked()
	if err != nil {
		m.lastError = err.Error()
		return err
	}
	cfg.Log = logCfg
	normalizeConfig(&cfg)
	if err := m.saveConfigLocked(cfg); err != nil {
		m.lastError = err.Error()
		return err
	}
	// 同步日志大小上限的原子快照：写入路径兜底只读原子值，不拿 m.mu
	m.logLimitBytes.Store(int64(cfg.Log.MaxSizeMB) << 20)
	m.lastError = ""
	return nil
}

// LogLimitBytes 返回日志大小上限（字节）的原子快照——给日志写入/轮转链路
// 用，它们可能在持锁路径被触发，不能走 LogSettings()。nil 接收者返回 0
// （不设限）：装配期方法值可能先于 Manager 构造被绑定（见 main.go 的闭包
// 说明），纵深防御让这类错序不至于 panic。
func (m *Manager) LogLimitBytes() int64 {
	if m == nil {
		return 0
	}
	return m.logLimitBytes.Load()
}

// SetLogLimitBytes 更新原子快照（启动装载配置与保存日志配置时调用）。
func (m *Manager) SetLogLimitBytes(bytes int64) {
	m.logLimitBytes.Store(bytes)
}

// LogSettings 供日志轮转循环读取大小上限；读文件失败按默认值降级。
func (m *Manager) LogSettings() LogConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.loadConfigLocked()
	if err != nil {
		return DefaultLogConfig()
	}
	return cfg.Log
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
	if err := m.saveConfigLocked(cfg); err != nil {
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

// sensorGroupValues：SensorGroups 覆盖允许的父类值。
var sensorGroupValues = map[string]bool{"gpu": true, "nic": true, "other": true}

// SaveSensorSettings 保存传感器显示名与父类归属覆盖：裁剪空白、丢弃空值与
// 非法父类、限制条数与长度，一次写盘。
func (m *Manager) SaveSensorSettings(names, groups map[string]string) error {
	cleanedNames := make(map[string]string, len(names))
	for key, value := range names {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" || len(key) > 80 || len([]rune(value)) > 40 {
			continue
		}
		cleanedNames[key] = value
		if len(cleanedNames) >= 200 {
			break
		}
	}
	cleanedGroups := make(map[string]string, len(groups))
	for key, value := range groups {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || len(key) > 80 || !sensorGroupValues[value] {
			continue
		}
		cleanedGroups[key] = value
		if len(cleanedGroups) >= 200 {
			break
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.loadConfigLocked()
	if err != nil {
		m.lastError = err.Error()
		return err
	}
	cfg.SensorNames = cleanedNames
	cfg.SensorGroups = cleanedGroups
	if err := m.saveConfigLocked(cfg); err != nil {
		m.lastError = err.Error()
		return err
	}
	return nil
}

// SaveUIPrefs 只保存前端界面偏好（当前：历史温度范围档位），一次写盘；
// 档位区间外按未设置处理，静默清零。
func (m *Manager) SaveUIPrefs(prefs UIPrefsConfig) error {
	prefs.HistoryRangeHours = ClampUIHistoryRangeHours(prefs.HistoryRangeHours)
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.loadConfigLocked()
	if err != nil {
		m.lastError = err.Error()
		return err
	}
	cfg.UIPrefs = prefs
	if err := m.saveConfigLocked(cfg); err != nil {
		m.lastError = err.Error()
		return err
	}
	return nil
}

// SensorGroupOverrides 返回用户配置的父类归属覆盖（读历史数据时应用）。
func (m *Manager) SensorGroupOverrides() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.loadConfigLocked()
	if err != nil {
		return nil
	}
	return cfg.SensorGroups
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
	if err := m.saveConfigLocked(cfg); err != nil {
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
	if err := m.saveConfigLocked(cfg); err != nil {
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
	if err := m.saveConfigLocked(cfg); err != nil {
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
	if err := m.saveConfigLocked(cfg); err != nil {
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
	status.Serial = m.serialStatusLocked(cfg.Serials)
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
	status.ExtraTemperatures = m.extraTemperatures()
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

// knownGPUDrivers：常见核显/独显驱动名，hwmon 温度归类到「GPU」组。
var knownGPUDrivers = map[string]bool{
	"i915": true, "xe": true, "amdgpu": true, "radeon": true,
	"nouveau": true, "nvidia": true, "panfrost": true, "mgag200": true,
}

// knownNICDrivers：常见有线/无线网卡驱动名（含 mlx5 RoCE/IB 卡），用于把
// hwmon 温度归类到「网卡」组。
var knownNICDrivers = map[string]bool{
	"igc": true, "igb": true, "ixgbe": true, "i40e": true, "e1000e": true, "e1000": true,
	"r8169": true, "atlantic": true, "atl1c": true, "tg3": true, "bnxt_en": true, "mlx5": true,
	"brcmfmac": true, "iwlwifi": true, "mt7921e": true, "rtw88_8822ce": true, "rtw89_pci": true,
}

// knownUSBTempDrivers：外置 USB 温度传感器在内核侧有 hwmon 驱动时注册的
// 芯片名。主线内核目前没有 TEMPer 系列的驱动，常见的是 out-of-tree 模块
// （如 ElementalWarrior/hwmon-temper，注册名 "temper"）。这些芯片经由
// extraTemperatures 的通用收集路径进入历史传感器列表（标签 "temper:temp1"
// 之类），归类固定「其它」，但改名（SensorNames）与父类归属覆盖
// （SensorGroups）链路对它们与普通 hwmon 传感器完全一致，无需特殊处理。
var knownUSBTempDrivers = map[string]bool{
	"temper": true, // hwmon-temper out-of-tree 模块（PCsensor TEMPer USB stick）
}

// extraTemperatures 收集 coretemp 之外的全部 hwmon 温度（网卡、主板 Super IO、
// ACPI 温区等），Label 以芯片名做前缀供前端分组。GPU（amdgpu/i915）单列。
// 硬盘芯片（nvme/drivetemp）除外：盘温只走槽位采样（history_slots），
// hwmon 读数与 SATA/NVMe 组的单盘曲线重复。
// 末尾并入无内核驱动的 USB 温度计（TEMPer 系列，key 前缀 "usb:"）与 USB
// 串口温度传感器（key 前缀 "usb:tty:"），同样归「其它」组，共享改名与父类
// 归属覆盖链路。
func (m *Manager) extraTemperatures() []Temperature {
	namePaths, _ := filepath.Glob(m.rooted("/sys/class/hwmon/hwmon*/name"))
	var result []Temperature
	seen := make(map[string]int)
	for _, namePath := range namePaths {
		name, err := readTrim(namePath)
		if err != nil || name == "coretemp" || name == "nvme" || name == "drivetemp" {
			continue
		}
		// 同名芯片（多块 NVMe/多张 mlx5）加 #N 后缀：标签是 history_sensors
		// 主键的一部分，重名会让整个采样点写库失败。
		seen[name]++
		prefix := name
		if n := seen[name]; n > 1 {
			prefix = fmt.Sprintf("%s#%d", name, n)
		}
		dir := filepath.Dir(namePath)
		inputs, _ := filepath.Glob(filepath.Join(dir, "temp*_input"))
		for _, input := range inputs {
			base := strings.TrimSuffix(filepath.Base(input), "_input")
			label, _ := readTrim(filepath.Join(dir, base+"_label"))
			if label == "" {
				label = base
			}
			value, err := readInt(input)
			if err != nil || value <= 0 {
				continue
			}
			result = append(result, Temperature{Label: fmt.Sprintf("%s:%s", prefix, label), Celsius: float64(value) / 1000})
		}
	}
	// USB HID 温度计走缓存（后台循环锁外读取，见 usb_temper.go）：过期的
	// 缓存直接弃用，设备拔掉后曲线自然断线
	if !m.usbTempAt.IsZero() && time.Since(m.usbTempAt) <= usbTempStaleAfter {
		for _, reading := range m.usbTempReadings {
			result = append(result, Temperature{Label: reading.Key, Celsius: reading.Celsius})
		}
	}
	result = append(result, m.serialTemperatureReadingsLocked()...)
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
