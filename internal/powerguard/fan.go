package powerguard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const fanStateVersion = 1

type FanPoint struct {
	TempC      float64 `json:"temp_c"`
	PWMPercent int     `json:"pwm_percent"`
}

// FanCurveBackup 进入关停档前的三条曲线快照。关停到达 5 分钟上限自动退出时用
// 它恢复用户原来的曲线——关停期间落盘的是全 0% 节点，直接退出会因校验失败而
// 回不到普通档。手动退出关停时快照一并清掉，不重复使用。
type FanCurveBackup struct {
	Curve     []FanPoint `json:"curve"`
	HDDCurve  []FanPoint `json:"hdd_curve"`
	NVMeCurve []FanPoint `json:"nvme_curve"`
}

type FanConfig struct {
	Enabled        bool     `json:"enabled"`
	DeviceID       string   `json:"device_id,omitempty"`
	CPUFanIDs      []string `json:"cpu_fan_ids"`
	HDDFanIDs      []string `json:"hdd_fan_ids"`
	NVMeFanIDs     []string `json:"nvme_fan_ids"`
	MinPWMPercent  int      `json:"min_pwm_percent"`
	EmergencyTempC float64  `json:"emergency_temp_c"`
	PollSeconds    int      `json:"poll_seconds"`
	// OffMode 是「关停」档：绑定风扇一律写 0%，优先级高于最低转速与曲线数值；
	// 维修安全策略下即使达到紧急温度也不起转，只蜂鸣报警，避免伤人。
	// 关停最长持续 fanOffModeMaxDuration，到期自动恢复 OffModeBackup 里的曲线。
	OffMode       bool            `json:"off_mode,omitempty"`
	OffModeBackup *FanCurveBackup `json:"off_mode_backup,omitempty"`
	Curve         []FanPoint      `json:"curve"`
	DiskCurve     []FanPoint      `json:"disk_curve,omitempty"`
	HDDCurve      []FanPoint      `json:"hdd_curve"`
	NVMeCurve     []FanPoint      `json:"nvme_curve"`
	HDDSlotIDs    []string        `json:"hdd_slot_ids"`
	NVMeSlotIDs   []string        `json:"nvme_slot_ids"`
}

type FanDevice struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Channel    int    `json:"channel"`
	RPM        int64  `json:"rpm"`
	PWM        int64  `json:"pwm"`
	Mode       int64  `json:"mode"`
	InputPath  string `json:"-"`
	PWMPath    string `json:"-"`
	EnablePath string `json:"-"`
}

type FanStatus struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Channel        int      `json:"channel"`
	RPM            int64    `json:"rpm"`
	PWM            int64    `json:"pwm"`
	PWMPercent     int      `json:"pwm_percent"`
	Mode           int64    `json:"mode"`
	Selected       bool     `json:"selected"`
	ControlSources []string `json:"control_sources,omitempty"`
}

type FanControlStatus struct {
	Available            bool        `json:"available"`
	DriverDetected       bool        `json:"driver_detected"`
	Active               bool        `json:"active"`
	OffMode              bool        `json:"off_mode,omitempty"`
	AlarmMuted           bool        `json:"alarm_muted,omitempty"`
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
	LastApply            time.Time   `json:"last_apply,omitempty"`
	LastError            string      `json:"last_error,omitempty"`
}

type originalFan struct {
	ID   string `json:"id"`
	PWM  int64  `json:"pwm"`
	Mode int64  `json:"mode"`
}

type originalFanState struct {
	Version    int           `json:"version"`
	CapturedAt time.Time     `json:"captured_at"`
	Fans       []originalFan `json:"fans"`
}

// defaultMinPWMPercent 是最低转速的出厂默认值（0.10.37 起由 60 降为 40）。
// 校验下限仍是 30（硬性防停转），这里只是新建配置时的默认档。
const defaultMinPWMPercent = 40

// builtinFanCurves 是系统集成的三档曲线参数（静音 / 平衡 / 性能），
// 与前端 app.js 的 CURVE_PRESETS 数值一一对应，改一边必须同步另一边。
// 出厂默认取「平衡」这一档。
var builtinFanCurves = map[string]struct {
	CPU  []FanPoint
	HDD  []FanPoint
	NVMe []FanPoint
}{
	"quiet": {
		CPU: []FanPoint{
			{TempC: 20, PWMPercent: 40}, {TempC: 42, PWMPercent: 40},
			{TempC: 44, PWMPercent: 43}, {TempC: 60, PWMPercent: 43},
			{TempC: 70, PWMPercent: 50}, {TempC: 75, PWMPercent: 60},
			{TempC: 80, PWMPercent: 80}, {TempC: 85, PWMPercent: 100},
		},
		HDD: []FanPoint{
			{TempC: 20, PWMPercent: 40}, {TempC: 30, PWMPercent: 40},
			{TempC: 35, PWMPercent: 43}, {TempC: 45, PWMPercent: 43},
			{TempC: 50, PWMPercent: 55}, {TempC: 52, PWMPercent: 70},
			{TempC: 55, PWMPercent: 80}, {TempC: 60, PWMPercent: 90},
		},
		NVMe: []FanPoint{
			{TempC: 20, PWMPercent: 40}, {TempC: 45, PWMPercent: 40},
			{TempC: 55, PWMPercent: 55}, {TempC: 60, PWMPercent: 60},
		},
	},
	"balanced": {
		CPU: []FanPoint{
			{TempC: 20, PWMPercent: 40}, {TempC: 42, PWMPercent: 40},
			{TempC: 44, PWMPercent: 47}, {TempC: 60, PWMPercent: 47},
			{TempC: 70, PWMPercent: 55}, {TempC: 75, PWMPercent: 65},
			{TempC: 80, PWMPercent: 85}, {TempC: 85, PWMPercent: 100},
		},
		HDD: []FanPoint{
			{TempC: 20, PWMPercent: 40}, {TempC: 30, PWMPercent: 40},
			{TempC: 35, PWMPercent: 47}, {TempC: 45, PWMPercent: 47},
			{TempC: 50, PWMPercent: 60}, {TempC: 52, PWMPercent: 75},
			{TempC: 55, PWMPercent: 80}, {TempC: 60, PWMPercent: 100},
		},
		NVMe: []FanPoint{
			{TempC: 20, PWMPercent: 40}, {TempC: 45, PWMPercent: 40},
			{TempC: 55, PWMPercent: 60}, {TempC: 60, PWMPercent: 70},
		},
	},
	"performance": {
		CPU: []FanPoint{
			{TempC: 20, PWMPercent: 47}, {TempC: 45, PWMPercent: 47},
			{TempC: 50, PWMPercent: 60}, {TempC: 55, PWMPercent: 70},
			{TempC: 60, PWMPercent: 85}, {TempC: 85, PWMPercent: 100},
		},
		HDD: []FanPoint{
			{TempC: 20, PWMPercent: 40}, {TempC: 30, PWMPercent: 40},
			{TempC: 35, PWMPercent: 47}, {TempC: 40, PWMPercent: 50},
			{TempC: 50, PWMPercent: 65}, {TempC: 52, PWMPercent: 80},
			{TempC: 55, PWMPercent: 90}, {TempC: 60, PWMPercent: 100},
		},
		NVMe: []FanPoint{
			{TempC: 20, PWMPercent: 40}, {TempC: 45, PWMPercent: 40},
			{TempC: 55, PWMPercent: 65}, {TempC: 60, PWMPercent: 75},
		},
	},
}

// clampCurvePWM 把一条曲线的转速整体抬到最低转速以上（校验下限 30 是硬性的）。
// 只抬高不降低，因此曲线的单调不减仍然成立。
func clampCurvePWM(points []FanPoint, minimum int) []FanPoint {
	minimum = max(30, min(minimum, 100))
	out := make([]FanPoint, len(points))
	for i, point := range points {
		pwm := point.PWMPercent
		if pwm < minimum {
			pwm = minimum
		}
		out[i] = FanPoint{TempC: point.TempC, PWMPercent: pwm}
	}
	return out
}

func DefaultFanConfig() FanConfig {
	balanced := builtinFanCurves["balanced"]
	config := FanConfig{
		Enabled:        false,
		MinPWMPercent:  defaultMinPWMPercent,
		EmergencyTempC: 85,
		PollSeconds:    2,
	}
	config.Curve = clampCurvePWM(balanced.CPU, config.MinPWMPercent)
	config.HDDCurve = clampCurvePWM(balanced.HDD, config.MinPWMPercent)
	config.NVMeCurve = clampCurvePWM(balanced.NVMe, config.MinPWMPercent)
	config.HDDSlotIDs = storageSlotIDs("front")
	config.NVMeSlotIDs = storageSlotIDs("m2")
	return config
}

func storageSlotIDs(kind string) []string {
	var ids []string
	for _, spec := range storageSlotSpecs {
		if spec.Kind == kind {
			ids = append(ids, spec.ID)
		}
	}
	return ids
}

// defaultStorageFanCurve 是机械硬盘曲线的出厂默认（平衡档），按最低转速收口。
func defaultStorageFanCurve(minimum int) []FanPoint {
	return clampCurvePWM(builtinFanCurves["balanced"].HDD, minimum)
}

// defaultNVMeFanCurve 是 NVMe 曲线的出厂默认（平衡档）。NVMe 温度范围与机械盘
// 不同（待机温度更高、无 25°C 以下的盘体温），缺省回退必须用自己的曲线，
// 不能错用 HDD 曲线。
func defaultNVMeFanCurve(minimum int) []FanPoint {
	return clampCurvePWM(builtinFanCurves["balanced"].NVMe, minimum)
}

// 0.10.36 及以前的出厂默认（最低转速 60%、三条曲线都从 60% 起）。
// 只用于识别「一个字都没改过」的旧默认，见 migrateLegacyFanDefaults。
var legacyDefaultCPUCurve = []FanPoint{
	{TempC: 40, PWMPercent: 60},
	{TempC: 55, PWMPercent: 70},
	{TempC: 70, PWMPercent: 85},
	{TempC: 80, PWMPercent: 100},
}

func legacyDefaultStorageCurve() []FanPoint {
	return []FanPoint{
		{TempC: 25, PWMPercent: 60},
		{TempC: 35, PWMPercent: 85},
		{TempC: 50, PWMPercent: 100},
	}
}

func fanPointsEqual(a, b []FanPoint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// migrateLegacyFanDefaults 把「完全没动过」的旧出厂默认整体升到新出厂默认
// （最低转速 60% → 40%）。最低转速或三条曲线里任何一项被改过就整体保持原样，
// 免得把用户自己调过的转速悄悄改掉。新旧默认值都在这里判定，改默认值时同步更新。
func migrateLegacyFanDefaults(fan *FanConfig) bool {
	if fan.MinPWMPercent != 60 {
		return false
	}
	legacyStorage := legacyDefaultStorageCurve()
	if !fanPointsEqual(fan.Curve, legacyDefaultCPUCurve) ||
		!fanPointsEqual(fan.HDDCurve, legacyStorage) ||
		!fanPointsEqual(fan.NVMeCurve, legacyStorage) {
		return false
	}
	defaults := DefaultFanConfig()
	fan.MinPWMPercent = defaults.MinPWMPercent
	fan.Curve = defaults.Curve
	fan.HDDCurve = defaults.HDDCurve
	fan.NVMeCurve = defaults.NVMeCurve
	return true
}

func normalizeConfig(cfg *Config) bool {
	changed := false
	// 串口传感器旧格式迁移：单设备对象 serial → 数组 serials。读入即迁移
	// 并把旧字段置 nil，任何一次配置保存都会把新格式落盘；传感器键由路径
	// 派生（usb:tty:<basename>），迁移不改键，历史曲线不断线。
	if cfg.Serial != nil {
		if len(cfg.Serials) == 0 && (cfg.Serial.Path != "" || cfg.Serial.Enabled) {
			cfg.Serials = []SerialSensorConfig{*cfg.Serial}
		}
		cfg.Serial = nil
		changed = true
	}
	if cfg.Fan.Curve == nil {
		enabled := cfg.Fan.Enabled
		deviceID := cfg.Fan.DeviceID
		cpuFanIDs := append([]string(nil), cfg.Fan.CPUFanIDs...)
		hddFanIDs := append([]string(nil), cfg.Fan.HDDFanIDs...)
		nvmeFanIDs := append([]string(nil), cfg.Fan.NVMeFanIDs...)
		cfg.Fan = DefaultFanConfig()
		cfg.Fan.Enabled = enabled
		cfg.Fan.DeviceID = deviceID
		cfg.Fan.CPUFanIDs = cpuFanIDs
		cfg.Fan.HDDFanIDs = hddFanIDs
		cfg.Fan.NVMeFanIDs = nvmeFanIDs
		changed = true
	}
	if cfg.Fan.HDDCurve == nil {
		if cfg.Fan.DiskCurve != nil {
			cfg.Fan.HDDCurve = append([]FanPoint(nil), cfg.Fan.DiskCurve...)
		} else {
			cfg.Fan.HDDCurve = defaultStorageFanCurve(cfg.Fan.MinPWMPercent)
		}
		changed = true
	}
	if cfg.Fan.NVMeCurve == nil {
		cfg.Fan.NVMeCurve = defaultNVMeFanCurve(cfg.Fan.MinPWMPercent)
		changed = true
	}
	if cfg.Fan.HDDSlotIDs == nil {
		cfg.Fan.HDDSlotIDs = storageSlotIDs("front")
		changed = true
	}
	if cfg.Fan.NVMeSlotIDs == nil {
		cfg.Fan.NVMeSlotIDs = storageSlotIDs("m2")
		changed = true
	}
	if cfg.Fan.DiskCurve != nil {
		cfg.Fan.DiskCurve = nil
		changed = true
	}
	if migrateLegacyFanDefaults(&cfg.Fan) {
		changed = true
	}
	if normalizeFanSelections(&cfg.Fan) {
		changed = true
	}
	if cfg.GPIO.Version == 0 {
		// 老得连版本号都没有。只有确实没配置过的才整份换成新默认值（用户攒下的
		// 脚本、备份任务、打标卷记录与提示音开关仍要原样带过去）；已经配置过的
		// 一字不动地保留——只补版本号，否则后续 validateGPIOConfig 会拒绝
		// version 0。
		if gpioConfigLooksUnconfigured(cfg.GPIO) {
			cfg.GPIO = preserveGPIOUserData(DefaultGPIOConfig(), cfg.GPIO)
		} else {
			cfg.GPIO.Version = gpioConfigVersion
		}
		changed = true
	} else if gpioConfigLooksUnconfigured(cfg.GPIO) {
		// 旧版本的默认值是「关闭 + 四个档位全为无动作」，从未配置过的机器
		// 升级后自动套用新的开箱即用默认值，避免用户以为按键坏了。
		cfg.GPIO = preserveGPIOUserData(DefaultGPIOConfig(), cfg.GPIO)
		changed = true
	}
	if applyGPIORecommendedDefaults(&cfg.GPIO) {
		// 已配置过的老配置：只给还空着的档位补推荐动作（网络键短按＝刷新SD卡），
		// 用户自己填过的不动，且靠 Migrations 保证只补一次。
		changed = true
	}
	if cfg.History.MaxSizeMB == 0 {
		// 旧版本配置文件没有 history 段：默认启用采样并套用默认大小上限与保留期
		cfg.History = DefaultHistoryConfig()
		changed = true
	} else if clamped := ClampHistoryMaxSize(cfg.History.MaxSizeMB); clamped != cfg.History.MaxSizeMB {
		cfg.History.MaxSizeMB = clamped
		changed = true
	}
	if cfg.History.RetentionDays == 0 {
		// 旧配置没有 retention_days 字段：解析后为 0，静默归位为默认保留期
		cfg.History.RetentionDays = historyDefaultRetentionDays
		changed = true
	} else if clamped := ClampHistoryRetentionDays(cfg.History.RetentionDays); clamped != cfg.History.RetentionDays {
		cfg.History.RetentionDays = clamped
		changed = true
	}
	if !cfg.History.ArchiveEnabled && cfg.History.ArchiveDir != "" {
		// 长期记录关闭时清掉目录，避免配置里残留无意义路径
		cfg.History.ArchiveDir = ""
		changed = true
	}
	if cfg.Log.MaxSizeMB == 0 {
		// 旧配置没有 log 段：默认日志大小上限
		cfg.Log = DefaultLogConfig()
		changed = true
	} else if clamped := ClampLogMaxSize(cfg.Log.MaxSizeMB); clamped != cfg.Log.MaxSizeMB {
		cfg.Log.MaxSizeMB = clamped
		changed = true
	}
	for index := range cfg.GPIO.Buttons {
		// 短按动作与长按同样保留，不再被归一化清除；缺字段的空字符串统一写成
		// none，否则校验会把它当成「不支持的动作」而拒绝整份配置。
		for _, stage := range []*string{
			&cfg.GPIO.Buttons[index].Actions.Short,
			&cfg.GPIO.Buttons[index].Actions.Hold3S,
			&cfg.GPIO.Buttons[index].Actions.Hold9S,
			&cfg.GPIO.Buttons[index].Actions.Hold15S,
		} {
			if *stage == "" {
				*stage = GPIOActionNone
				changed = true
			}
		}
	}
	if applyDefaultSensorNames(cfg) {
		// 传感器显示名只补一次：把常见芯片默认名（主板PCB / BIOS虚拟温度 /
		// CX-4 LX#1 等）写进配置，用户之后清空或改名都不再被覆盖。
		changed = true
	}
	if applySensorNameRenames(cfg) {
		// 0.10.46 之前固定下来的「PCB温度」跟着改名，免得老机器停在旧名字上。
		changed = true
	}
	return changed
}

func normalizeFanSelections(cfg *FanConfig) bool {
	changed := false
	if cfg.DeviceID != "" && cfg.CPUFanIDs == nil && cfg.HDDFanIDs == nil && cfg.NVMeFanIDs == nil {
		cfg.CPUFanIDs = []string{cfg.DeviceID}
		cfg.HDDFanIDs = []string{cfg.DeviceID}
		cfg.NVMeFanIDs = []string{cfg.DeviceID}
		changed = true
	}
	for _, ids := range []*[]string{&cfg.CPUFanIDs, &cfg.HDDFanIDs, &cfg.NVMeFanIDs} {
		normalized := uniqueNonEmpty(*ids)
		if len(normalized) != len(*ids) {
			changed = true
		}
		*ids = normalized
	}
	return changed
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func selectedFanSources(cfg FanConfig) map[string][]string {
	result := make(map[string][]string)
	for source, ids := range map[string][]string{"cpu": cfg.CPUFanIDs, "hdd": cfg.HDDFanIDs, "nvme": cfg.NVMeFanIDs} {
		for _, id := range ids {
			result[id] = append(result[id], source)
		}
	}
	return result
}

// fanSpinUpGraceWindow 关停档退出后允许风扇从 0 转自然起转的宽限期。
// 风扇从停转到转速反馈恢复通常只需几秒到几十秒，但个别风扇/供电恢复较慢，
// 宽限给到 5 分钟（与关停档 5 分钟上限是两件独立的事）；期间读到的 0 转速
// 不能当「通道接不到风扇」，宽限期过后仍然 0 转的通道按故障处理。
const fanSpinUpGraceWindow = 5 * time.Minute

// fanOffModeMaxDuration 关停档最长持续时间：到期自动恢复原曲线并起转，避免
// 检修结束后忘记退出导致硬盘/NVMe 长时间无风散热。
const fanOffModeMaxDuration = 5 * time.Minute

// noteOffModeStateLocked 记录关停档的进出状态：进入时记下起点供 5 分钟上限
// 判定，退出时记下起转宽限的起点。旧的「曾经读到过转速」永久标记（fanLiveRPM）
// 已废弃——风扇拔掉后仍会豁免 0 转故障。
func (m *Manager) noteOffModeStateLocked(offMode bool) {
	if offMode {
		if !m.fanOffWasActive {
			m.fanOffWasActive = true
			m.fanOffModeSince = m.now()
		}
		return
	}
	if m.fanOffWasActive {
		m.fanOffWasActive = false
		m.fanOffExitedAt = m.now()
	}
	// 退出关停档即解除报警静音：下一轮真出故障时蜂鸣必须恢复，不能让一次
	// 临时静音永久生效。
	m.fanAlarmMuted = false
	m.fanOffModeSince = time.Time{}
}

// fanOffModeExpiredLocked 报告关停档是否已到达 5 分钟上限。
func (m *Manager) fanOffModeExpiredLocked() bool {
	if m.fanOffModeSince.IsZero() {
		return false
	}
	return m.now().Sub(m.fanOffModeSince) >= fanOffModeMaxDuration
}

// captureOffModeBackup 进入关停档时保存上一条生效曲线的快照，供 5 分钟上限
// 自动恢复；退出关停（手动或自动）时清掉快照。手动退出由界面回传恢复后的
// 曲线，这里不覆盖显式提交的曲线。
func captureOffModeBackup(next *FanConfig, previous FanConfig) {
	if !next.OffMode {
		next.OffModeBackup = nil
		return
	}
	if next.OffModeBackup != nil {
		return
	}
	if previous.OffMode {
		// 关停期间再次保存：界面不会回传快照，沿用服务端留的关停前曲线。
		next.OffModeBackup = previous.OffModeBackup
		return
	}
	next.OffModeBackup = &FanCurveBackup{
		Curve:     append([]FanPoint(nil), previous.Curve...),
		HDDCurve:  append([]FanPoint(nil), previous.HDDCurve...),
		NVMeCurve: append([]FanPoint(nil), previous.NVMeCurve...),
	}
}

// applyOffModeBackup 用快照覆盖三条曲线；任一档快照无效就那一档保持原样。
func applyOffModeBackup(fan *FanConfig, backup *FanCurveBackup) {
	if len(backup.Curve) >= 2 {
		fan.Curve = backup.Curve
	}
	if len(backup.HDDCurve) >= 2 {
		fan.HDDCurve = backup.HDDCurve
	}
	if len(backup.NVMeCurve) >= 2 {
		fan.NVMeCurve = backup.NVMeCurve
	}
}

// leaveOffModeLocked 关停到达 5 分钟上限：恢复关停前曲线、退出关停档并落盘，
// 由调用方紧接着按普通模式下发，风扇自动起转。快照缺失时把 0% 抬到最低转速，
// 保证恢复后的曲线能通过普通模式校验。
func (m *Manager) leaveOffModeLocked(cfg *Config) error {
	fan := &cfg.Fan
	fan.OffMode = false
	if backup := fan.OffModeBackup; backup != nil {
		applyOffModeBackup(fan, backup)
		fan.OffModeBackup = nil
	}
	raiseCurveFloor(fan.Curve, fan.MinPWMPercent)
	raiseCurveFloor(fan.DiskCurve, fan.MinPWMPercent)
	raiseCurveFloor(fan.HDDCurve, fan.MinPWMPercent)
	raiseCurveFloor(fan.NVMeCurve, fan.MinPWMPercent)
	if err := m.saveConfigLocked(*cfg); err != nil {
		return err
	}
	m.logf("fan: 关停档已到 5 分钟上限，已自动恢复原曲线并起转")
	return nil
}

// raiseCurveFloor 把关停期间写成 0% 的曲线节点抬到最低转速，作为快照缺失时的兜底。
func raiseCurveFloor(curve []FanPoint, minimum int) {
	for i := range curve {
		if curve[i].PWMPercent < minimum {
			curve[i].PWMPercent = minimum
		}
	}
}

// now 统一时钟入口：默认 time.Now，测试通过 Manager.nowFn 注入固定时刻，
// 让起转宽限/关停上限的边界用例可确定断言，不必真实等待几分钟。
func (m *Manager) now() time.Time {
	if m.nowFn != nil {
		return m.nowFn()
	}
	return time.Now()
}

// fanSpinUpGraceLocked 报告当前是否仍在关停退出后的起转宽限期内。
func (m *Manager) fanSpinUpGraceLocked() bool {
	if m.fanOffExitedAt.IsZero() {
		return false
	}
	return m.now().Sub(m.fanOffExitedAt) < fanSpinUpGraceWindow
}

// errFanOffModeBusy 关停档（维修/检修）下拒绝调试调速、递增、标定这类主动
// 写 PWM 的操作：关停档优先级最高，任何写入路径都不许让风扇转起来。
var errFanOffModeBusy = errors.New("风扇处于「关停」档（维修/检修），已禁止主动调速；请先取消关停")

// fanOffModeActiveLocked 报告当前配置是否处于「关停」档。配置读不出来时沿用
// 上一次实际生效的关停状态（与 ApplyFanCurrent 的故障处理口径一致）。
func (m *Manager) fanOffModeActiveLocked() bool {
	cfg, err := m.loadConfigLocked()
	if err != nil {
		return m.fanOffWasActive
	}
	return cfg.Fan.Enabled && cfg.Fan.OffMode
}

// errNoUsableFan 所有选中的风扇都读不到转速：保存配置时拒绝（防止静默存下
// 无风扇可用的配置）；运行时周期应用保持错误状态报警——机器有两个风扇接口，
// 全部停转就是真故障（风扇卡死/线被扯掉/测速线坏），绝不能清掉错误装正常。
// 单个通道 0 转仍按跳过处理（接口未接风扇或被反插），不触发本错误。
var errNoUsableFan = errors.New("所有选中的风扇都读不到转速（可能未接风扇或接口被反插），请检查接线后重新选择")

// errOffModeAlarm 关停档（维修模式）下检测到紧急温度或温度源失效：按维修安全
// 策略风扇保持停转不起转——关停档是用户手动开启的检修状态（两重确认+顶部常驻
// 警告），此刻风扇突然起转可能伤到正在操作的人——只保持界面错误状态并用蜂鸣器
// 急促报警。该错误不触发 fail-safe 拉满，也不阻止配置保存。
var errOffModeAlarm = errors.New("关停档维修安全报警")

const (
	// fanOffAlarmChimeHz 报警音频率：高于按键确认（440Hz）与存储提示（880Hz），
	// 一听就知道是报警；连续 fanOffAlarmChimeBeeps 声短促音。
	fanOffAlarmChimeHz    = 1760
	fanOffAlarmChimeMS    = 150
	fanOffAlarmChimeBeeps = 5
	// fanOffAlarmChimeInterval 蜂鸣报警节流：报警持续期间每隔这么久重响一次。
	fanOffAlarmChimeInterval = 20 * time.Second
	// offModeHDDCriticalC 关停档下前置机械硬盘的温度上限：风扇停转后磁盘升温
	// 往往早于 CPU 到达危险区，超过（含达到）该值即报警。只报警，不改变停转策略。
	offModeHDDCriticalC = 55.0
)

// fanAlarmChimeHook 测试钩子：非 nil 时替代真实 GPIO 蜂鸣，用于断言报警音
// 到底有没有播放（关停档报警只允许在该响的时候响）。运行时保持 nil。
var fanAlarmChimeHook func(tones []GPIOToneSpec)

// chimeOffModeAlarmLocked 关停档超温/温度源失效的蜂鸣报警：急促高频短音，后台
// 播放不阻塞控制循环；蜂鸣器不在时尽力而为（错误被忽略，界面报警状态仍在）。
// 用户临时静音期间只跳过蜂鸣，节流时刻不更新，解除静音后立即响一声。
// 返回值表示本次是否真的播放了报警音，调用方据此决定日志措辞。
func (m *Manager) chimeOffModeAlarmLocked() bool {
	if m.fanAlarmMuted {
		return false
	}
	now := m.now()
	if !m.fanOffAlarmChimeAt.IsZero() && now.Sub(m.fanOffAlarmChimeAt) < fanOffAlarmChimeInterval {
		return false
	}
	m.fanOffAlarmChimeAt = now
	tones := make([]GPIOToneSpec, fanOffAlarmChimeBeeps)
	for i := range tones {
		tones[i] = GPIOToneSpec{FrequencyHz: fanOffAlarmChimeHz, DurationMS: fanOffAlarmChimeMS}
	}
	if fanAlarmChimeHook != nil {
		fanAlarmChimeHook(tones)
		return true
	}
	go func() { _ = m.PlayStorageChime(tones) }()
	return true
}

// SetFanAlarmMute 临时静音/恢复关停档的故障与超温蜂鸣报警。只影响蜂鸣器：
// 界面错误状态、日志与风扇停转策略都不变；退出关停档时自动解除（见
// noteOffModeStateLocked）。关停档未生效时开关没有意义，直接拒绝以便界面
// 明确提示。
func (m *Manager) SetFanAlarmMute(muted bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.fanOffModeActiveLocked() {
		return errors.New("仅在「关停」档下可以临时静音故障/超温报警")
	}
	m.fanAlarmMuted = muted
	if muted {
		return nil
	}
	// 解除静音要立刻补响：清零 20 秒节流，否则「刚响过 → 静音 → 20 秒内解除」
	// 仍会被节流窗口挡掉；当前仍在报警时当场响一次，不等下一轮控制周期。
	m.fanOffAlarmChimeAt = time.Time{}
	if m.fanOffAlarmActive {
		m.chimeOffModeAlarmLocked()
	}
	return nil
}

func (m *Manager) validateFanLocked(cfg *FanConfig) error {
	if cfg.MinPWMPercent < 30 || cfg.MinPWMPercent > 100 {
		return errors.New("fan min_pwm_percent must be between 30 and 100")
	}
	if cfg.EmergencyTempC < 70 || cfg.EmergencyTempC > 100 {
		return errors.New("fan emergency_temp_c must be between 70 and 100")
	}
	if cfg.PollSeconds < 1 || cfg.PollSeconds > 10 {
		return errors.New("fan poll_seconds must be between 1 and 10")
	}
	if err := validateFanCurve("CPU", cfg.Curve, cfg.MinPWMPercent, cfg.EmergencyTempC, cfg.OffMode); err != nil {
		return err
	}
	if err := validateFanCurve("HDD", cfg.HDDCurve, cfg.MinPWMPercent, cfg.EmergencyTempC, cfg.OffMode); err != nil {
		return err
	}
	if err := validateFanCurve("NVMe", cfg.NVMeCurve, cfg.MinPWMPercent, cfg.EmergencyTempC, cfg.OffMode); err != nil {
		return err
	}
	if err := validateFanSlotIDs("HDD", "front", cfg.HDDSlotIDs); err != nil {
		return err
	}
	if err := validateFanSlotIDs("NVMe", "m2", cfg.NVMeSlotIDs); err != nil {
		return err
	}
	if !cfg.Enabled {
		return nil
	}
	selected := selectedFanSources(*cfg)
	if len(selected) == 0 {
		return errors.New("at least one fan must be selected when fan control is enabled")
	}
	fans, err := m.DiscoverFans()
	if err != nil {
		return err
	}
	available := make(map[string]FanDevice, len(fans))
	for _, fan := range fans {
		available[fan.ID] = fan
	}
	// 起转宽限只依据「已接受并实际生效」的关停状态，待校验的配置不得修改运行
	// 状态（被拒绝的配置不能留下假的关停转换）：fanSpinUpGraceLocked 覆盖退出
	// 关停后的 5 分钟窗口；(m.fanOffWasActive && !cfg.OffMode) 覆盖「当前生效的是
	// 关停档、本次保存要切回普通档」这一跳——此刻风扇必然还是 0 转，不能判死。
	// 真正的关停转换记录只发生在 applyFanLocked。
	grace := m.fanSpinUpGraceLocked() || (m.fanOffWasActive && !cfg.OffMode)
	var dead []string
	for id := range selected {
		fan, ok := available[id]
		if !ok {
			return fmt.Errorf("configured fan %s was not found", id)
		}
		// 关停档里风扇本来就该停转，0 转速是预期结果而不是故障：这里再判死会让
		// 周期性应用走进 fail-safe 把风扇拉满，表现就是「关停之后隔一会儿又起转」。
		if cfg.OffMode {
			continue
		}
		// 刚退出关停档的风扇从 0 转自然起转需要几秒到几十秒，宽限期内读到的
		// 0 转速不算「通道接不到风扇」；宽限期过后仍然 0 转才按故障处理。
		if fan.RPM <= 0 && !grace {
			dead = append(dead, id)
		}
	}
	// 选中的风扇里读不到转速的（接口没接风扇、风扇被拔掉或反插到别的通道），
	// 自动从配置里剔除而不是报错——用户反插接口后旧通道不再阻塞保存。
	sort.Strings(dead) // map 遍历顺序不稳定，排序后日志去重键才一致
	m.logFanIssueLocked(&m.fanFilteredLogKey, dead, "风扇 %s 读不到转速，已从配置中自动屏蔽（可能未接风扇或接口被反插）")
	if len(dead) > 0 {
		cfg.CPUFanIDs = removeStringSlice(cfg.CPUFanIDs, dead)
		cfg.HDDFanIDs = removeStringSlice(cfg.HDDFanIDs, dead)
		cfg.NVMeFanIDs = removeStringSlice(cfg.NVMeFanIDs, dead)
		selected = selectedFanSources(*cfg)
		if len(selected) == 0 {
			return errNoUsableFan
		}
	}
	return nil
}

// logFanIssueLocked 风扇问题日志去重：控制周期每几秒跑一轮，同一组风扇的同一
// 问题只在首次（或集合变化后）写一次日志；ids 为空时重置去重键，风扇恢复后
// 再次出现问题会重新记录。format 必须且只能含一个 %s（用于风扇 ID 列表）。
func (m *Manager) logFanIssueLocked(key *string, ids []string, format string) {
	if len(ids) == 0 {
		*key = ""
		return
	}
	joined := strings.Join(ids, "、")
	if *key == joined {
		return
	}
	*key = joined
	m.logf(format, joined)
}

// removeStringSlice 从切片里删除指定元素，返回新切片。
func removeStringSlice(slice, remove []string) []string {
	if len(remove) == 0 {
		return slice
	}
	blocked := make(map[string]bool, len(remove))
	for _, id := range remove {
		blocked[id] = true
	}
	result := make([]string, 0, len(slice))
	for _, id := range slice {
		if !blocked[id] {
			result = append(result, id)
		}
	}
	return result
}

func validateFanSlotIDs(name, kind string, ids []string) error {
	allowed := make(map[string]bool)
	for _, spec := range storageSlotSpecs {
		if spec.Kind == kind {
			allowed[spec.ID] = true
		}
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !allowed[id] {
			return fmt.Errorf("fan %s slot %q is not a supported %s slot", name, id, kind)
		}
		if seen[id] {
			return fmt.Errorf("fan %s slot %q is duplicated", name, id)
		}
		seen[id] = true
	}
	return nil
}

// validateFanCurve 校验曲线形状。offMode 是「关停」档：此时曲线允许整条为 0%
// 且不再受最低转速约束（关停的优先级就是要高于最低转速），其余规则照旧。
func validateFanCurve(name string, curve []FanPoint, minimum int, emergencyTempC float64, offMode bool) error {
	if len(curve) < 2 || len(curve) > 8 {
		return fmt.Errorf("fan %s curve must contain between 2 and 8 points", name)
	}
	floor := minimum
	if offMode {
		floor = 0
	}
	previousTemp := -1.0
	previousPWM := -1
	for i, point := range curve {
		if point.TempC < 20 || point.TempC > 100 {
			return fmt.Errorf("fan %s curve point %d temperature must be between 20 and 100", name, i+1)
		}
		if point.TempC <= previousTemp {
			return fmt.Errorf("fan %s curve temperatures must be strictly increasing", name)
		}
		if point.PWMPercent < floor || point.PWMPercent > 100 {
			return fmt.Errorf("fan %s curve point %d PWM must be between minimum PWM and 100", name, i+1)
		}
		if point.PWMPercent < previousPWM {
			return fmt.Errorf("fan %s curve PWM values must not decrease as temperature rises", name)
		}
		previousTemp = point.TempC
		previousPWM = point.PWMPercent
	}
	if emergencyTempC < curve[len(curve)-1].TempC {
		return fmt.Errorf("fan emergency temperature must not be below the last %s curve point", name)
	}
	return nil
}

func (m *Manager) DiscoverFans() ([]FanDevice, error) {
	namePaths, _ := filepath.Glob(m.rooted("/sys/class/hwmon/hwmon*/name"))
	var fans []FanDevice
	for _, namePath := range namePaths {
		name, err := readTrim(namePath)
		if err != nil || !isIT87Name(name) {
			continue
		}
		dir := filepath.Dir(namePath)
		devicePath, err := filepath.EvalSymlinks(filepath.Join(dir, "device"))
		if err != nil {
			devicePath = dir
		}
		deviceName := filepath.Base(devicePath)
		inputs, _ := filepath.Glob(filepath.Join(dir, "fan*_input"))
		for _, inputPath := range inputs {
			base := filepath.Base(inputPath)
			channelText := strings.TrimSuffix(strings.TrimPrefix(base, "fan"), "_input")
			channel, err := strconv.Atoi(channelText)
			if err != nil || channel < 1 {
				continue
			}
			pwmPath := filepath.Join(dir, fmt.Sprintf("pwm%d", channel))
			enablePath := filepath.Join(dir, fmt.Sprintf("pwm%d_enable", channel))
			if _, err := os.Stat(pwmPath); err != nil {
				continue
			}
			if _, err := os.Stat(enablePath); err != nil {
				continue
			}
			rpm, rpmErr := readInt(inputPath)
			pwm, pwmErr := readInt(pwmPath)
			mode, modeErr := readInt(enablePath)
			if rpmErr != nil || pwmErr != nil || modeErr != nil {
				continue
			}
			fans = append(fans, FanDevice{
				ID:         fmt.Sprintf("%s:%s:fan%d", name, deviceName, channel),
				Name:       name,
				Channel:    channel,
				RPM:        rpm,
				PWM:        pwm,
				Mode:       mode,
				InputPath:  inputPath,
				PWMPath:    pwmPath,
				EnablePath: enablePath,
			})
		}
	}
	sort.Slice(fans, func(i, j int) bool { return fans[i].ID < fans[j].ID })
	for _, fan := range fans {
		m.offerFanRPMSample(fan.ID, int(fan.RPM), int(fan.PWM))
	}
	return fans, nil
}

// ---- 风扇转速特性学习与 RPM 闭环 ----

// it87 硬件只接受 PWM 占空比，"转速 RPM"调试单位需要一条 PWM→转速 的映射
// 才有意义。映射从实测学得：温控循环与调试操作持续产生 (PWM, 转速) 读数，
// 同一 PWM 档位的读数攒满窗口且极差足够小（真稳态）即取中位数入表并落盘——
// 风扇被曲线或调试驱动过的每个工况都会自动充实特性表。设定转速时先查表
// （插值，"大概转速"），再由闭环按反馈转速微调 PWM（|实测-目标| ≤ 容差）。
// 高档位稳态同时维护满转基准（RPM 上限与兜底换算的分母）。
const (
	fanRPMBaseDefault      = 2000 // 未标定时的名义满转转速
	fanRPMSlotStep         = 16   // 特性表档位步长（PWM），0..240 共 16 档；241–255 归 240 档（转速差异可忽略）
	fanRPMCalibMinRPM      = 300  // 低于此按停转/坏读数丢弃（失速区不入表）
	fanRPMCalibMaxRPM      = 20000
	fanRPMSlotWinSize      = 3                // 档位稳态判定窗口（同一档位最近 N 个读数）
	fanRPMSlotSpreadPct    = 4                // 窗口内极差 ≤4% 才认为稳态（升速中/抖动不入表）
	fanRPMMapDeltaPct      = 2                // 与表现值差 ≤2% 不更新，避免反复写盘
	fanRPMSaveMinInterval  = 30 * time.Second // 特性表落盘限频(内存更新不受限)
	fanRPMCloseLoopTick    = 2 * time.Second
	fanRPMCloseLoopTolPct  = 3  // 容差：|实测-目标| ≤ 目标的 3%
	fanRPMCloseLoopMinTol  = 60 // 容差下限（转速读数本身有 ~±30 量化噪声）
	fanRPMCloseLoopMaxAdj  = 20 // 单次微调的最大 PWM 步长（防震荡）
	fanRPMCloseLoopMaxMiss = 15 // 连续不可达次数上限，超过即暂停微调（防永久抖动）
	fanRPMCloseLoopNearAdj = 2  // 近端（误差 < 2×容差）微调步长上限：小步细逼近，防大步跨过目标后回摆

	// 自动递增间隔的溢出护栏（百年，防 time.Duration 纳秒乘法回绕），不是
	// 产品意义上的上限——间隔本身不设限，下限 1 秒。
	fanAutoIntervalMax = 3155760000
)

// clampFanAutoInterval 把间隔钳进 [1, 百年]：只防荒谬大值撑爆 Duration。
func clampFanAutoInterval(seconds int) int {
	if seconds < 1 {
		return 1
	}
	if seconds > fanAutoIntervalMax {
		return fanAutoIntervalMax
	}
	return seconds
}

type fanRPMSample struct {
	id  string
	rpm int
	pwm int
}

// fanRPMSlotWindow 当前档位的稳态窗口：同一时刻每风扇只有一个档位在采样，
// PWM 换档即整体覆盖，天然避免跨工况混样。
type fanRPMSlotWindow struct {
	slot    int
	samples []int
}

// offerFanRPMSample 把读数非阻塞地投递给学习 goroutine：DiscoverFans 有时在
// 持有 m.mu 的路径上被调用，这里绝不能等；通道满时丢弃（稳态工况采样频繁，
// 丢几个不影响判定）。
func (m *Manager) offerFanRPMSample(id string, rpm, pwm int) {
	if m.fanRPMLearnDisabled.Load() {
		return
	}
	if m.fanRPMCalib == nil {
		m.fanRPMCalibOnce.Do(func() {
			m.fanRPMLearnStop = make(chan struct{})
			m.fanRPMLearnDone = make(chan struct{})
			m.fanRPMCalib = make(chan fanRPMSample, 64)
			go m.runFanRPMCalibration()
		})
	}
	select {
	case m.fanRPMCalib <- fanRPMSample{id: id, rpm: rpm, pwm: pwm}:
	default:
	}
}

func (m *Manager) runFanRPMCalibration() {
	defer func() {
		if m.fanRPMLearnDone != nil {
			close(m.fanRPMLearnDone)
		}
	}()
	for {
		select {
		case <-m.fanRPMLearnStop:
			return
		case sample := <-m.fanRPMCalib:
			m.processFanRPMSample(sample.id, sample.rpm, sample.pwm)
		}
	}
}

// stopFanRPMLearning 停止特性表学习并等待在途样本处理完。测试收尾专用:
// 防残余 goroutine 与 t.TempDir 清理竞争写盘;生产进程退出即亡,不调用。
func (m *Manager) stopFanRPMLearning() {
	// 先挡住启动与投递(无论学习是否已启动),再对已启动的 goroutine 关停并
	// 等待在途样本处理完。
	m.fanRPMLearnDisabled.Store(true)
	m.mu.Lock()
	stop, done := m.fanRPMLearnStop, m.fanRPMLearnDone
	m.mu.Unlock()
	if stop != nil {
		m.fanRPMLearnStopOnce.Do(func() { close(stop) })
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
}

// processFanRPMSample 消费一个读数：合理性过滤后按档位攒窗，窗口满且极差
// 足够小视为稳态，取中位数经 recordFanRPMData 入表。窗口只被学习路径串行
// 访问，不经 m.mu；特性表/基准/配置的读写走 m.mu。
func (m *Manager) processFanRPMSample(id string, rpm, pwm int) {
	if rpm < fanRPMCalibMinRPM || rpm > fanRPMCalibMaxRPM {
		return
	}
	slot := pwm / fanRPMSlotStep * fanRPMSlotStep
	if slot > 240 {
		slot = 240 // 241–255 的转速差异可忽略，并入 240 档
	}
	if m.fanRPMSlotWin == nil {
		m.fanRPMSlotWin = map[string]*fanRPMSlotWindow{}
	}
	win := m.fanRPMSlotWin[id]
	if win == nil || win.slot != slot {
		win = &fanRPMSlotWindow{slot: slot}
		m.fanRPMSlotWin[id] = win
	}
	win.samples = append(win.samples, rpm)
	if len(win.samples) > fanRPMSlotWinSize {
		win.samples = win.samples[len(win.samples)-fanRPMSlotWinSize:]
	}
	if len(win.samples) < fanRPMSlotWinSize {
		return
	}
	sorted := append([]int(nil), win.samples...)
	sort.Ints(sorted)
	median := sorted[len(sorted)/2]
	lo, hi := sorted[0], sorted[len(sorted)-1]
	if (hi-lo)*100 > median*fanRPMSlotSpreadPct {
		return // 升速中或读数抖动，未稳态
	}
	m.recordFanRPMData(id, slot, median, false, nil)
}

// recordFanRPMData 稳态中位数入特性表（高档位同时维护满转基准）；与现值差
// 超过阈值才写，内存先行（学习即时生效），落盘尽力而为（失败时下次稳态
// 重试）。调用方不持 m.mu。
// recordFanRPMData 记录一档稳态转速（特性表 + 高档位顺带维护满转基准）。
// 落盘节流:内存已更新,写盘限频——生产防高频写配置,也避免测试结束后残余
// goroutine 往已清理的临时目录写文件。断言落盘的测试先把 fanRPMLastSave
// 清零。force=true 绕过节流立即落盘（主动标定路径：标定是用户可见操作，
// 结果必须当场持久化,不能被 30 秒节流吞掉重启丢失）。fanRPMDirty 标记
// 被节流的更新,下一个采样点补落盘——相同读数会在 2% 判定处早退,不补
// 这次更新就永远悬在内存里。
// expectedOffGen 非 nil 时按关停档代次把关：只有采样开始后一次都没进入过关停档
// （且此刻也没处在关停档）才允许入表。检查与写入在同一把锁内完成，避免
// 「刚核对完就被切进关停档」的窗口。
func (m *Manager) recordFanRPMData(id string, slot, rpm int, force bool, expectedOffGen *uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if expectedOffGen != nil && (*expectedOffGen != m.fanOffGen || m.fanOffModeActiveLocked()) {
		return
	}
	if m.fanRPMDirty && !force && !m.fanRPMLastSave.IsZero() &&
		time.Since(m.fanRPMLastSave) >= fanRPMSaveMinInterval {
		m.persistFanRPMDataLocked()
	}
	// 先完成磁盘特性表的惰性载入：重启后首次学习若直接建空表，落盘会把该
	// 风扇已存的多档表覆盖成单档；而且后续第一次 fanRPMLearnedLocked 的
	// 载入还会用磁盘内容顶掉内存里未落盘的学习结果
	m.fanRPMLearnedLocked(id)
	if m.fanRPMLearned == nil {
		m.fanRPMLearned = map[string]map[int]int{}
	}
	table := m.fanRPMLearned[id]
	if table == nil {
		table = map[int]int{}
		m.fanRPMLearned[id] = table
	}
	if existing := table[slot]; existing > 0 {
		delta := rpm - existing
		if delta < 0 {
			delta = -delta
		}
		if delta*100 <= existing*fanRPMMapDeltaPct {
			return // 表里已有可信值，不打扰
		}
	}
	table[slot] = rpm
	m.fanRPMDirty = true
	// 高档位顺带维护满转基准（同为稳态实测值，与标定语义一致）
	if slot >= 240 {
		current := m.fanDebugRPMBaseLocked(id)
		delta := rpm - current
		if delta < 0 {
			delta = -delta
		}
		if delta*100 > current*fanRPMMapDeltaPct {
			if m.fanRPMBase == nil {
				m.fanRPMBase = map[string]int{}
			}
			m.fanRPMBase[id] = rpm
		}
	}
	if force || m.fanRPMLastSave.IsZero() || time.Since(m.fanRPMLastSave) >= fanRPMSaveMinInterval {
		m.persistFanRPMDataLocked()
	}
}

// persistFanRPMDataLocked 把特性表与满转基准**全量**落盘（不是只写触发这次
// 调用的风扇——别台风扇被节流悬着的更新一并带走），成功即清 dirty。调用
// 方须持 m.mu。
func (m *Manager) persistFanRPMDataLocked() {
	cfg, err := m.loadConfigLocked()
	if err != nil {
		return
	}
	if cfg.FanRPMBase == nil {
		cfg.FanRPMBase = map[string]int{}
	}
	if cfg.FanRPMMap == nil {
		cfg.FanRPMMap = map[string]map[int]int{}
	}
	for fanID, fanTable := range m.fanRPMLearned {
		cfg.FanRPMMap[fanID] = fanTable
	}
	for fanID, base := range m.fanRPMBase {
		if base > 0 {
			cfg.FanRPMBase[fanID] = base
		}
	}
	if writeJSONAtomic(m.ConfigPath, cfg, 0o600) == nil {
		m.fanRPMLastSave = time.Now()
		m.fanRPMDirty = false
	}
}

// fanDebugRPMBaseLocked 返回该风扇的满转基准（未标定用默认值）。调用方须持
// m.mu；首次调用从配置载入标定表。
func (m *Manager) fanDebugRPMBaseLocked(id string) int {
	m.fanRPMBaseLoad.Do(func() {
		m.fanRPMBase = map[string]int{}
		if cfg, err := m.loadConfigLocked(); err == nil {
			for fanID, base := range cfg.FanRPMBase {
				if base > 0 {
					m.fanRPMBase[fanID] = base
				}
			}
		}
	})
	if base := m.fanRPMBase[id]; base > 0 {
		return base
	}
	return fanRPMBaseDefault
}

// fanRPMLearnedLocked 返回该风扇的特性表（惰性从配置载入）。调用方须持 m.mu。
func (m *Manager) fanRPMLearnedLocked(id string) map[int]int {
	m.fanRPMBaseLoad.Do(func() {
		m.fanRPMBase = map[string]int{}
		if cfg, err := m.loadConfigLocked(); err == nil {
			for fanID, base := range cfg.FanRPMBase {
				if base > 0 {
					m.fanRPMBase[fanID] = base
				}
			}
		}
	})
	m.fanRPMMapLoad.Do(func() {
		m.fanRPMLearned = map[string]map[int]int{}
		if cfg, err := m.loadConfigLocked(); err == nil {
			for fanID, table := range cfg.FanRPMMap {
				clean := map[int]int{}
				for slot, rpm := range table {
					if slot >= 0 && slot <= 240 && rpm > 0 {
						clean[slot] = rpm
					}
				}
				if len(clean) > 0 {
					m.fanRPMLearned[fanID] = clean
				}
			}
		}
	})
	return m.fanRPMLearned[id]
}

// rpmToPWMEstimateLocked 设定目标转速时的 PWM 初值：特性表里夹逼插值（
// "大概转速"），表外区间用满转基准线性外推，空表退化为纯基准线性（与旧
// fanDebugRaw 行为一致）。调用方须持 m.mu。
func (m *Manager) rpmToPWMEstimateLocked(id string, target int) int {
	table := m.fanRPMLearnedLocked(id)
	if len(table) == 0 {
		base := m.fanDebugRPMBaseLocked(id)
		return clampInt((target*255+base/2)/base, 0, 255)
	}
	// (转速, 档位) 按转速升序，找夹逼对线性插值
	type point struct{ rpm, slot int }
	points := make([]point, 0, len(table))
	for slot, rpm := range table {
		points = append(points, point{rpm, slot})
	}
	sort.Slice(points, func(i, j int) bool { return points[i].rpm < points[j].rpm })
	if target <= 0 {
		return 0
	}
	if target <= points[0].rpm {
		// 表内最低档以下：(0,0)–最低档线性插值（与 pwmToRPMEstimateLocked 互逆）
		return points[0].slot * target / points[0].rpm
	}
	last := points[len(points)-1]
	if target >= last.rpm {
		// 表外高转速：在 (最高档, 实测) 与 (255, 满转基准) 之间线性插值，
		// 与 pwmToRPMEstimateLocked 用同一外推规则，表顶两侧连续
		topRPM := max(m.fanDebugRPMBaseLocked(id), last.rpm)
		if target >= topRPM || topRPM == last.rpm {
			return 255
		}
		return clampInt(last.slot+((target-last.rpm)*(255-last.slot)+(topRPM-last.rpm)/2)/(topRPM-last.rpm), 0, 255)
	}
	for i := 1; i < len(points); i++ {
		hi := points[i]
		lo := points[i-1]
		if target <= hi.rpm {
			// 转速→PWM 反插值；相邻档转速相同（平台区）时任取低档
			if hi.rpm == lo.rpm {
				return lo.slot
			}
			return lo.slot + (target-lo.rpm)*(hi.slot-lo.slot)/(hi.rpm-lo.rpm)
		}
	}
	return last.slot
}

// ---- RPM 闭环微调 ----

// ensureFanRPMCloseLoop 保证闭环 goroutine 存活；无活跃目标时自动退出。
func (m *Manager) ensureFanRPMCloseLoop() {
	if m.fanRPMLoopRunning {
		return
	}
	m.fanRPMLoopRunning = true
	go m.runFanRPMCloseLoop()
}

// runFanRPMCloseLoop 每 tick 读一次反馈转速：|实测-目标| ≤ 容差即静默保持
// （监控漂移），否则按差值比例微调 PWM（限幅防震荡）。活跃集合动态判定——
// 只有"接管中 + rpm 单位 + 自动递增未运行"的风扇参与；集合为空 goroutine
// 退出，下次设定转速时重新拉起。同一 PWM 的物理转速会随温度/电压缓漂，达标
// 后仍保持监控是闭环的意义所在。
func (m *Manager) runFanRPMCloseLoop() {
	ticker := time.NewTicker(fanRPMCloseLoopTick)
	defer ticker.Stop()
	for range ticker.C {
		if !m.stepFanRPMCloseLoop() {
			return
		}
	}
}

// stepFanRPMCloseLoop 单次闭环步：|实测-目标| ≤ 容差即静默保持（监控漂移），
// 否则按差值比例微调 PWM（限幅防震荡）。活跃集合动态判定——只有"接管中 +
// rpm 单位 + 自动递增未运行"的风扇参与；返回是否仍有活跃目标。
func (m *Manager) stepFanRPMCloseLoop() bool {
	{
		m.mu.Lock()
		fans, ferr := m.DiscoverFans()
		if ferr != nil {
			// 偶发失败保持存活等下个 tick 重试:直接退出会让闭环与目标脱节,
			// 直到用户重新设定转速才恢复
			m.fanDebugLastError = ferr.Error()
			m.mu.Unlock()
			return true
		}
		// 关停档维修期间闭环只保持存活、绝不写 PWM：关停档优先级最高，
		// 否则闭环会把刚被写成 0% 的风扇按目标转速重新拉起来。
		offMode := m.fanOffModeActiveLocked()
		active := false
		if m.fanRPMLoopMiss == nil {
			m.fanRPMLoopMiss = map[string]int{}
		}
		if m.fanRPMLoopLocked == nil {
			m.fanRPMLoopLocked = map[string]bool{}
		}
		if m.fanRPMLoopSettling == nil {
			m.fanRPMLoopSettling = map[string]bool{}
		}
		if m.fanRPMLoopLastDir == nil {
			m.fanRPMLoopLastDir = map[string]int{}
		}
		var errs []error
		for i := range fans {
			fan := &fans[i]
			id := fan.ID
			if m.fanDebugUnits[id] != "rpm" {
				continue
			}
			target, taken := m.fanDebugTakenOver[id]
			if !taken {
				continue
			}
			if m.fanRPMSuspend[id] {
				continue // 主动标定进行中，闭环让位（满速采样与微调不能同写）
			}
			if offMode {
				active = true
				continue // 关停档：保持存活但不写 PWM（见上方 offMode 注释）
			}
			if m.fanDebugEmergency {
				// CPU 过温紧急满速兜底最高优先级：闭环暂停微调（保持
				// active 继续 tick，兜底解除后自动恢复监控），绝不把
				// 满速拉回目标转速
				active = true
				continue
			}
			if m.fanDebugAuto != nil {
				if entry, ok := m.fanDebugAuto.entries[id]; ok && entry.Running {
					continue // 自动递增优先，闭环让位
				}
			}
			active = true
			tolerance := target * fanRPMCloseLoopTolPct / 100
			if tolerance < fanRPMCloseLoopMinTol {
				tolerance = fanRPMCloseLoopMinTol
			}
			actual := int(fan.RPM)
			delta := actual - target
			if delta < 0 {
				delta = -delta
			}
			if delta <= tolerance {
				m.fanRPMLoopMiss[id] = 0
				m.fanRPMLoopLocked[id] = true
				m.fanRPMLoopSettling[id] = false // 转速到位,无需再等上一轮调整的响应
				continue                         // 达标：保持现状，继续监控漂移
			}
			m.fanRPMLoopLocked[id] = false
			if m.fanRPMLoopMiss[id] >= fanRPMCloseLoopMaxMiss {
				continue // 连续不可达（目标在失速区/超量程），暂停避免永久抖动
			}
			if m.fanRPMLoopSettling[id] {
				// 上一轮刚写过 PWM：风扇转速惯性 2–5 秒 > tick 2 秒,此刻
				// 读到的还是过渡态——立刻再调会在旧调整未生效时叠加新调整,
				// 连环同向后必然过调、再反向回摆（用户实测正负来回震荡很久
				// 才收敛）。本轮只观察,给风扇一个完整 tick 响应上一轮。
				m.fanRPMLoopSettling[id] = false
				continue
			}
			base := m.fanDebugRPMBaseLocked(id)
			adjust := (target - actual) * 255 / base
			adjust = clampInt(adjust, -fanRPMCloseLoopMaxAdj, fanRPMCloseLoopMaxAdj)
			// 近端（误差 < 2×容差）小步细逼近：满步 ±20 PWM 对应一两百转,
			// 一步跨过目标就反向再调,是回摆震荡的另一半来源
			if delta < tolerance*2 {
				adjust = clampInt(adjust, -fanRPMCloseLoopNearAdj, fanRPMCloseLoopNearAdj)
			}
			// 与上一轮方向相反（过调回摆）：步长减半,摆幅逐次收敛
			if lastDir := m.fanRPMLoopLastDir[id]; lastDir != 0 && (lastDir > 0) != (adjust > 0) {
				adjust /= 2
			}
			// PWM 已顶格仍欠速：目标超出风扇能力，保持 255 不再回撤重试——
			// 读数噪声会让"加满→超调→降档→欠速"无限循环（用户实测：PWM
			// 到 98 又降回 90 重新试探）。直接进入暂停态；读数偶发进容差
			// 仍会解冻（miss 清零）。
			if adjust > 0 && int(fan.PWM) >= 255 {
				m.fanRPMLoopMiss[id] = fanRPMCloseLoopMaxMiss
				m.fanRPMLoopLocked[id] = true
				continue
			}
			m.fanRPMLoopMiss[id]++
			if adjust != 0 {
				dir := 1
				if adjust < 0 {
					dir = -1
				}
				m.fanRPMLoopLastDir[id] = dir
			}
			next := clampInt(int(fan.PWM)+adjust, 0, 255)
			if next != int(fan.PWM) {
				if err := setFanPWMRaw(*fan, next); err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", id, err))
				} else {
					m.fanRPMLoopSettling[id] = true // 下轮只观察,等转速响应
				}
			}
		}
		if len(errs) > 0 {
			m.fanDebugLastError = errors.Join(errs...).Error()
		}
		if !active {
			m.fanRPMLoopRunning = false
			m.mu.Unlock()
			return false
		}
		m.mu.Unlock()
		return true
	}
}

func isIT87Name(name string) bool {
	return name == "it87" || strings.HasPrefix(name, "it8")
}

func (m *Manager) IT87DriverDetected() bool {
	namePaths, _ := filepath.Glob(m.rooted("/sys/class/hwmon/hwmon*/name"))
	for _, namePath := range namePaths {
		name, err := readTrim(namePath)
		if err == nil && isIT87Name(name) {
			return true
		}
	}
	return false
}

func (m *Manager) ApplyFanCurrent() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.loadConfigLocked()
	if err != nil {
		// 配置读不出来时按「上次实际生效的关停状态」处理故障：关停档维修期间
		// 任何故障路径都不许把风扇转起来。
		return m.setFanFailureLocked(err, m.fanOffWasActive)
	}
	// 关停档最长 5 分钟：到期自动恢复关停前的曲线并退出关停，随后按普通模式
	// 下发，风扇无需用户操作就会重新起转。
	if cfg.Fan.Enabled && cfg.Fan.OffMode && m.fanOffModeExpiredLocked() {
		if err := m.leaveOffModeLocked(&cfg); err != nil {
			return m.setFanFailureLocked(err, true)
		}
	}
	offMode := cfg.Fan.Enabled && cfg.Fan.OffMode
	// 风扇调试接管：被接管的风扇脱离一切曲线控制(applyFanLocked 中锁定),
	// 未接管的风扇照常受曲线控制。紧急温度兜底覆盖所有风扇(含被接管)。
	// 关停档除外：维修安全策略要求一律停转，兜底会把被接管的风扇拉到 100%，
	// 与「关停期间绝不起转」冲突。这里清掉兜底标记，随后由 applyFanLocked 的
	// 关停分支把所有通道（含被调试接管的）写成 0%。
	if offMode {
		m.fanDebugEmergency = false
	} else if err := m.applyFanDebugEmergencyLocked(cfg.Fan); err != nil {
		return err
	}
	if !cfg.Fan.Enabled {
		m.fanLastError = ""
		return nil
	}
	if err := m.validateFanLocked(&cfg.Fan); err != nil {
		if errors.Is(err, errNoUsableFan) {
			// 运行时周期应用：所有选中风扇都读不到转速 = 全部停转。机器有两个
			// 风扇接口，都停转就是真故障（卡死/掉线/测速线坏），必须保持错误
			// 状态报警——周期循环丢弃返回值（不刷日志），报警经 fanLastError
			// 持久呈现在界面上，风扇恢复后由成功路径清除。不做 fail-safe 拉满：
			// 往空通道写 100% 无意义，而关停档维修场景走不到这里（OffMode
			// 跳过 0 转检查）。单个通道 0 转仍按跳过处理，不触发本分支。
			m.fanLastError = err.Error()
			return err
		}
		// 关停档里任何校验失败（例如某通道测速读不到，DiscoverFans 直接漏掉它）
		// 都不得走 fail-safe：那会把仍在工作的另一路风扇突然转起来。
		return m.setFanFailureLocked(err, offMode)
	}
	if err := m.applyFanLocked(cfg.Fan); err != nil {
		m.fanLastError = err.Error()
		return err
	}
	m.fanLastError = ""
	return nil
}

// setFanFailureLocked 统一处理风扇控制路径上的失败。
// offMode 为真时只记录错误、绝不写 PWM：维修关停期间任何故障（含某通道测速
// 读不到导致校验失败）都不许把仍在工作的另一路风扇拉起来，fail-safe 的 100%
// 写入在这条分支上属于伤人风险，必须让位给关停档。
func (m *Manager) setFanFailureLocked(cause error, offMode bool) error {
	if offMode {
		m.fanLastError = cause.Error()
		return cause
	}
	failSafeErr := m.failSafeCapturedFansLocked()
	err := errors.Join(cause, failSafeErr)
	m.fanLastError = err.Error()
	return err
}

// applyFanDebugEmergencyLocked 有风扇被调试接管时的紧急满速兜底：CPU
// （coretemp 最大值）超过紧急温度时强制全部风扇 100% 并标记覆盖（调试转速
// 被覆盖,状态里显著提示;闭环/递增/手动调速在 emergency 期间全部让位）;
// 磁盘温度不参与兜底判定（读盘会唤醒休眠盘）。无接管时清除标记并放行常规
// 曲线控制。兜底解除（true→false）时把被接管风扇按各自调试值写回——
// percent/pwm 单位的风扇在兜底期间停在 100%，不写回会与调试显示脱节。
func (m *Manager) applyFanDebugEmergencyLocked(cfg FanConfig) error {
	wasEmergency := m.fanDebugEmergency
	if len(m.fanDebugTakenOver) == 0 {
		m.fanDebugEmergency = false
		return nil
	}
	temperature, err := m.controlTemperature()
	if err == nil && temperature >= cfg.EmergencyTempC {
		fans, ferr := m.DiscoverFans()
		if ferr != nil {
			return ferr
		}
		var errs []error
		for i := range fans {
			if err := setFanPWM(fans[i], 100); err != nil {
				errs = append(errs, err)
			}
		}
		m.fanDebugEmergency = true
		return errors.Join(errs...)
	}
	m.fanDebugEmergency = false
	if !wasEmergency {
		return nil
	}
	fans, ferr := m.DiscoverFans()
	if ferr != nil {
		return ferr
	}
	byID := make(map[string]FanDevice, len(fans))
	for _, fan := range fans {
		byID[fan.ID] = fan
	}
	for id, value := range m.fanDebugTakenOver {
		fan, ok := byID[id]
		if !ok {
			continue
		}
		unit := m.fanDebugUnits[id]
		if unit == "" {
			unit = "percent"
		}
		if err := setFanPWMRaw(fan, fanDebugRaw(unit, value, m.fanDebugRPMBaseLocked(id))); err != nil {
			m.fanDebugLastError = fmt.Sprintf("fan %s emergency-release restore: %v", id, err)
		}
	}
	return nil
}

func (m *Manager) applyFanLocked(cfg FanConfig) error {
	fans, err := m.DiscoverFans()
	if err != nil {
		return err
	}
	m.noteOffModeStateLocked(cfg.OffMode)
	selected := selectedFanSources(cfg)
	byID := make(map[string]FanDevice, len(fans))
	for _, fan := range fans {
		byID[fan.ID] = fan
	}
	for id := range selected {
		fan, ok := byID[id]
		if !ok {
			return fmt.Errorf("configured fan %s was not found", id)
		}
		if err := m.captureOriginalFanLocked(fan); err != nil {
			return err
		}
	}

	cpuTemperature, cpuErr := m.controlTemperature()
	storage := m.StorageStatus()
	hddTemperature, hddAvailable := maximumStorageTemperature(storage, "front", cfg.HDDSlotIDs)
	nvmeTemperature, nvmeAvailable := maximumStorageTemperature(storage, "m2", cfg.NVMeSlotIDs)
	cpuTarget := 0
	if cpuErr == nil {
		cpuTarget = interpolatePWMPercent(cfg.Curve, cpuTemperature, cfg.MinPWMPercent, cfg.EmergencyTempC)
	}
	hddTarget := 0
	if hddAvailable {
		hddTarget = interpolatePWMPercent(cfg.HDDCurve, hddTemperature, cfg.MinPWMPercent, cfg.EmergencyTempC)
	}
	nvmeTarget := 0
	if nvmeAvailable {
		nvmeTarget = interpolatePWMPercent(cfg.NVMeCurve, nvmeTemperature, cfg.MinPWMPercent, cfg.EmergencyTempC)
	}
	emergency := (len(cfg.CPUFanIDs) > 0 && cpuErr == nil && cpuTemperature >= cfg.EmergencyTempC) ||
		(len(cfg.HDDFanIDs) > 0 && hddAvailable && hddTemperature >= cfg.EmergencyTempC) ||
		(len(cfg.NVMeFanIDs) > 0 && nvmeAvailable && nvmeTemperature >= cfg.EmergencyTempC)

	lastTarget := 0
	var errs []error
	var skipped []string   // 本轮因 0 转速被跳过的风扇（汇总后去重写一次日志）
	var offAlarms []string // 关停档维修安全报警（汇总后统一报错+蜂鸣，风扇保持停转）
	if cfg.OffMode && emergency {
		hottest := 0.0
		if cpuErr == nil {
			hottest = max(hottest, cpuTemperature)
		}
		if hddAvailable {
			hottest = max(hottest, hddTemperature)
		}
		if nvmeAvailable {
			hottest = max(hottest, nvmeTemperature)
		}
		offAlarms = append(offAlarms, fmt.Sprintf("关停档下检测到紧急温度 %.1f°C（阈值 %.1f°C），按维修安全策略风扇保持停转，请立即处理", hottest, cfg.EmergencyTempC))
	}
	// 机械硬盘单独一条 55°C 上限：关停档停转后最先升温的是盘，CPU 可能还正常，
	// 不能只盯 CPU。前置 SATA 盘全部纳入（不局限于曲线勾选的槽位）。
	if cfg.OffMode {
		if slot, ok := hottestFrontSlotOver(storage, offModeHDDCriticalC); ok {
			offAlarms = append(offAlarms, fmt.Sprintf(
				"关停档下机械硬盘温度 %.1f°C 达到 %.0f°C 上限（SATA %d），按维修安全策略风扇保持停转，请立即处理",
				slot.TemperatureC, offModeHDDCriticalC, slot.Slot))
		}
	}
	for id, sources := range selected {
		fan := byID[id]
		if _, taken := m.fanDebugTakenOver[id]; taken && !cfg.OffMode {
			// 被调试接管:脱离曲线控制,由调试接口锁定转速。
			// 关停档例外:维修安全策略要求一律停转,调试锁定也要让位,
			// 由下面的关停分支统一写 0%（否则调试值会让风扇继续转）。
			continue
		}
		// 关停档（维修/检修模式，开启有两重确认+顶部常驻警告）：直接写 0%，
		// 不再看最低转速与曲线。维修安全策略：紧急温度、温度源失效都不再翻
		// 满速——检修过程中风扇突然起转可能伤到正在操作的人——一律保持停转，
		// 改为界面错误状态 + 蜂鸣器急促报警（见 errOffModeAlarm）。
		if cfg.OffMode {
			sourceAvailable := false
			for _, source := range sources {
				switch source {
				case "cpu":
					sourceAvailable = sourceAvailable || cpuErr == nil
				case "hdd":
					sourceAvailable = sourceAvailable || hddAvailable
				case "nvme":
					sourceAvailable = sourceAvailable || nvmeAvailable
				}
			}
			target := 0
			if !sourceAvailable {
				offAlarms = append(offAlarms, fmt.Sprintf("关停档下风扇 %s 选中的温度源全部不可用，按维修安全策略保持停转", id))
			}
			if err := setFanPWM(fan, target); err != nil {
				errs = append(errs, fmt.Errorf("set fan %s to %d%%: %w", id, target, err))
				continue
			}
			lastTarget = max(lastTarget, target)
			continue
		}
		// 刚退出关停档的风扇从 0 转自然起转需要几秒到几十秒，宽限期内读到的
		// 0 转速不算「通道接不到风扇」；宽限期过后仍然 0 转的通道按故障处理——
		// 不再报错拉满，而是直接跳过（该接口可能没接风扇或被反插到别的通道）。
		if fan.RPM <= 0 && !m.fanSpinUpGraceLocked() {
			skipped = append(skipped, id)
			continue
		}
		target := 0
		if emergency {
			target = 100
		} else {
			for _, source := range sources {
				switch source {
				case "cpu":
					target = max(target, cpuTarget)
				case "hdd":
					target = max(target, hddTarget)
				case "nvme":
					target = max(target, nvmeTarget)
				}
			}
		}
		if target == 0 {
			target = 100
			errs = append(errs, fmt.Errorf("no selected temperature source is available for fan %s; forced to full speed", id))
		}
		if err := setFanPWM(fan, target); err != nil {
			errs = append(errs, fmt.Errorf("set fan %s to %d%%: %w", id, target, err), setFanPWM(fan, 100))
			continue
		}
		lastTarget = max(lastTarget, target)
	}
	sort.Strings(skipped)
	m.logFanIssueLocked(&m.fanSkippedLogKey, skipped, "风扇 %s 读不到转速，本次控制跳过（可能未接风扇或接口被反插）")
	if err := errors.Join(errs...); err != nil {
		return err
	}
	m.fanLastApply = time.Now()
	m.fanLastTarget = lastTarget
	if cpuErr == nil {
		m.fanLastTemp = cpuTemperature
	}
	if len(offAlarms) > 0 {
		// 关停档维修安全报警：风扇已按 0% 写完保持停转，这里只做报警——日志
		// 去重、蜂鸣节流、错误经 errOffModeAlarm 上抛（调用方置 fanLastError，
		// 不走 fail-safe 拉满，也不阻塞配置保存）。
		msg := strings.Join(offAlarms, "；")
		m.fanOffAlarmActive = true
		played := m.chimeOffModeAlarmLocked()
		chimeState := ""
		switch {
		case m.fanAlarmMuted:
			chimeState = "蜂鸣已临时静音，界面报警保留"
		case played:
			chimeState = "已蜂鸣报警"
		}
		// 节流窗口内的周期不写日志：既不刷屏，也不会把没发声写成「已蜂鸣报警」。
		if chimeState != "" {
			key := msg + "|" + chimeState
			if key != m.fanOffAlarmLogKey {
				m.fanOffAlarmLogKey = key
				m.logf("fan: %s（%s）", msg, chimeState)
			}
		}
		return fmt.Errorf("%w: %s", errOffModeAlarm, msg)
	}
	m.fanOffAlarmChimeAt = time.Time{} // 无报警时清零节流，下次报警立即响
	m.fanOffAlarmActive = false
	m.fanOffAlarmLogKey = ""
	return nil
}

func setFanPWM(fan FanDevice, percent int) error {
	return setFanPWMRaw(fan, percentToPWM(percent))
}

// fanPWMWriteHook 测试钩子：非 nil 时记录每一笔 PWM 原始写入（0–255）。
// 回归用例需要核对「整条路径上有没有出现过非零写入」，只看最后一次 PWM 值会
// 漏掉中途被 fail-safe 拉到 100% 又再写回 0 的情况。
// atomic.Pointer:测试在并行 goroutine 中安装钩子(setFanPWMRaw 在主测试
// goroutine 读),go test -race 报数据竞争,必须原子访问。
var fanPWMWriteHook atomic.Pointer[func(fan FanDevice, raw int)]

// setFanPWMRaw 直接按原始占空比(0–255)写入;百分比调用方先换算。
func setFanPWMRaw(fan FanDevice, raw int) error {
	if raw < 0 || raw > 255 {
		return fmt.Errorf("invalid fan PWM raw %d", raw)
	}
	if hook := fanPWMWriteHook.Load(); hook != nil {
		(*hook)(fan, raw)
	}
	currentPWM, err := readInt(fan.PWMPath)
	if err != nil {
		return fmt.Errorf("read pwm%d: %w", fan.Channel, err)
	}
	currentMode, err := readInt(fan.EnablePath)
	if err != nil {
		return fmt.Errorf("read pwm%d mode: %w", fan.Channel, err)
	}
	if raw == 255 && currentPWM == 255 && currentMode == 0 {
		return nil
	}
	if raw < 255 && currentMode == 0 {
		// This IT87 driver represents full speed as mode 0 and refuses mode 1
		// while PWM is still 255. Lowering PWM first atomically re-enters manual mode.
		if err := writeAndVerify(fan.PWMPath, int64(raw)); err != nil {
			return fmt.Errorf("leave pwm%d full-speed mode: %w", fan.Channel, err)
		}
	}
	if err := writeAndVerify(fan.EnablePath, 1); err != nil {
		return fmt.Errorf("switch pwm%d to manual mode: %w", fan.Channel, err)
	}
	if err := writeAndVerify(fan.PWMPath, int64(raw)); err != nil {
		return fmt.Errorf("write pwm%d: %w", fan.Channel, err)
	}
	mode, err := readInt(fan.EnablePath)
	if err != nil {
		return err
	}
	if raw < 255 && mode != 1 {
		return fmt.Errorf("pwm%d mode changed to %d instead of manual mode", fan.Channel, mode)
	}
	if raw == 255 && mode != 0 && mode != 1 {
		return fmt.Errorf("pwm%d returned unexpected full-speed mode %d", fan.Channel, mode)
	}
	return nil
}

func percentToPWM(percent int) int {
	return int(math.Round(float64(percent) * 255 / 100))
}

func pwmToPercent(pwm int64) int {
	return int(math.Round(float64(pwm) * 100 / 255))
}

func interpolatePWMPercent(points []FanPoint, tempC float64, minimum int, emergencyTempC float64) int {
	if tempC >= emergencyTempC {
		return 100
	}
	value := points[0].PWMPercent
	if tempC <= points[0].TempC {
		value = points[0].PWMPercent
	} else if tempC >= points[len(points)-1].TempC {
		value = points[len(points)-1].PWMPercent
	} else {
		for i := 1; i < len(points); i++ {
			if tempC > points[i].TempC {
				continue
			}
			left, right := points[i-1], points[i]
			ratio := (tempC - left.TempC) / (right.TempC - left.TempC)
			value = int(math.Round(float64(left.PWMPercent) + ratio*float64(right.PWMPercent-left.PWMPercent)))
			break
		}
	}
	if value < minimum {
		value = minimum
	}
	if value > 100 {
		value = 100
	}
	return value
}

func fanTargets(cfg FanConfig, cpuTemperature, hddTemperature float64, hddAvailable bool, nvmeTemperature float64, nvmeAvailable bool) (int, int, int, int) {
	if cfg.OffMode {
		// 关停档维修安全策略：一律写 0%——紧急温度也不再翻满速（改为蜂鸣报警，
		// 见 errOffModeAlarm），检修过程中风扇突然起转可能伤到正在操作的人。
		return 0, 0, 0, 0
	}
	cpuTarget := interpolatePWMPercent(cfg.Curve, cpuTemperature, cfg.MinPWMPercent, cfg.EmergencyTempC)
	hddTarget := 0
	if hddAvailable {
		hddTarget = interpolatePWMPercent(cfg.HDDCurve, hddTemperature, cfg.MinPWMPercent, cfg.EmergencyTempC)
	}
	nvmeTarget := 0
	if nvmeAvailable {
		nvmeTarget = interpolatePWMPercent(cfg.NVMeCurve, nvmeTemperature, cfg.MinPWMPercent, cfg.EmergencyTempC)
	}
	return cpuTarget, hddTarget, nvmeTarget, max(cpuTarget, hddTarget, nvmeTarget)
}

func maximumStorageTemperature(status StorageStatus, kind string, slotIDs []string) (float64, bool) {
	selected := make(map[string]bool, len(slotIDs))
	for _, id := range slotIDs {
		selected[id] = true
	}
	maximum := 0.0
	available := false
	for _, slot := range status.Slots {
		if slot.Kind != kind || !selected[slot.ID] || slot.TemperatureC <= 0 {
			continue
		}
		if !available || slot.TemperatureC > maximum {
			maximum = slot.TemperatureC
			available = true
		}
	}
	return maximum, available
}

// hottestFrontSlotOver 返回前置机械硬盘中温度达到上限的最热槽位。休眠/未识别
// 的盘没有有效温度，直接跳过；不限于曲线勾选的槽位——关停档停转时所有在位的
// 机械盘都有过热风险。
func hottestFrontSlotOver(status StorageStatus, limitC float64) (StorageSlot, bool) {
	var hottest StorageSlot
	found := false
	for _, slot := range status.Slots {
		if slot.Kind != "front" || slot.TemperatureC < limitC {
			continue
		}
		if !found || slot.TemperatureC > hottest.TemperatureC {
			hottest = slot
			found = true
		}
	}
	return hottest, found
}

func fanControlSource(cpuTarget, hddTarget, nvmeTarget int) string {
	target := max(cpuTarget, hddTarget, nvmeTarget)
	if target <= 0 {
		return ""
	}
	var sources []string
	if cpuTarget == target {
		sources = append(sources, "cpu")
	}
	if hddTarget == target {
		sources = append(sources, "hdd")
	}
	if nvmeTarget == target {
		sources = append(sources, "nvme")
	}
	return strings.Join(sources, "+")
}

func (m *Manager) controlTemperature() (float64, error) {
	temperatures := m.temperatures()
	if len(temperatures) == 0 {
		return 0, errors.New("no readable coretemp sensor was found")
	}
	maximum := temperatures[0].Celsius
	for _, temperature := range temperatures[1:] {
		if temperature.Celsius > maximum {
			maximum = temperature.Celsius
		}
	}
	return maximum, nil
}

func (m *Manager) fanStatePath() string {
	return filepath.Join(filepath.Dir(m.StatePath), "original-fan-state.json")
}

func (m *Manager) captureOriginalFanLocked(fan FanDevice) error {
	if err := m.validateSavedHardwareIfAvailable(); err != nil {
		return err
	}
	state, err := m.loadFanStateLocked()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if errors.Is(err, fs.ErrNotExist) {
		state = originalFanState{Version: fanStateVersion, CapturedAt: time.Now()}
	}
	for _, original := range state.Fans {
		if original.ID == fan.ID {
			return nil
		}
	}
	state.Fans = append(state.Fans, originalFan{ID: fan.ID, PWM: fan.PWM, Mode: fan.Mode})
	return writeJSONAtomic(m.fanStatePath(), state, 0o600)
}

func (m *Manager) loadFanStateLocked() (originalFanState, error) {
	data, err := os.ReadFile(m.fanStatePath())
	if err != nil {
		return originalFanState{}, err
	}
	var state originalFanState
	if err := jsonUnmarshalStrict(data, &state); err != nil {
		return originalFanState{}, fmt.Errorf("decode fan state: %w", err)
	}
	if state.Version != fanStateVersion {
		return originalFanState{}, fmt.Errorf("unsupported fan state version %d", state.Version)
	}
	return state, nil
}

func (m *Manager) restoreFansLocked() error {
	state, err := m.loadFanStateLocked()
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := m.validateSavedHardwareIfAvailable(); err != nil {
		return err
	}
	fans, err := m.DiscoverFans()
	if err != nil {
		return err
	}
	byID := make(map[string]FanDevice, len(fans))
	for _, fan := range fans {
		byID[fan.ID] = fan
	}
	var errs []error
	for _, original := range state.Fans {
		fan, ok := byID[original.ID]
		if !ok {
			errs = append(errs, fmt.Errorf("fan %s is no longer present", original.ID))
			continue
		}
		if err := restoreFan(fan, original); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func restoreFan(fan FanDevice, original originalFan) error {
	// mode 不设上界：it87 的 pwm_enable 里 2–6 都表示某种 BIOS 自动策略，
	// 恢复时必须原样写回（校验 >2 会把"交还回 BIOS 温控"错误地判为非法）
	if original.Mode < 0 || original.PWM < 0 || original.PWM > 255 {
		return fmt.Errorf("saved state for fan %s is invalid", fan.ID)
	}
	if err := writeAndVerify(fan.EnablePath, 1); err != nil {
		return fmt.Errorf("restore fan %s manual mode: %w", fan.ID, err)
	}
	if err := writeAndVerify(fan.PWMPath, original.PWM); err != nil {
		return fmt.Errorf("restore fan %s PWM: %w", fan.ID, err)
	}
	if err := writeAndVerify(fan.EnablePath, original.Mode); err != nil {
		return fmt.Errorf("restore fan %s mode: %w", fan.ID, err)
	}
	return nil
}

func (m *Manager) failSafeCapturedFansLocked() error {
	state, err := m.loadFanStateLocked()
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := m.validateSavedHardwareIfAvailable(); err != nil {
		return err
	}
	fans, err := m.DiscoverFans()
	if err != nil {
		return err
	}
	byID := make(map[string]FanDevice, len(fans))
	for _, fan := range fans {
		byID[fan.ID] = fan
	}
	var errs []error
	for _, original := range state.Fans {
		fan, ok := byID[original.ID]
		if !ok {
			errs = append(errs, fmt.Errorf("fan %s is no longer present for fail-safe", original.ID))
			continue
		}
		if err := setFanPWM(fan, 100); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) fanStatusLocked(cfg FanConfig) FanControlStatus {
	result := FanControlStatus{
		Active:           cfg.Enabled,
		DriverDetected:   m.IT87DriverDetected(),
		OffMode:          cfg.OffMode,
		AlarmMuted:       m.fanAlarmMuted,
		LastApply:        m.fanLastApply,
		LastError:        m.fanLastError,
		TargetPWMPercent: m.fanLastTarget,
		TemperatureC:     m.fanLastTemp,
	}
	fans, err := m.DiscoverFans()
	if err != nil {
		result.LastError = combineError(result.LastError, err)
		return result
	}
	selected := selectedFanSources(cfg)
	for _, fan := range fans {
		if fan.RPM > 0 {
			result.Available = true
		}
		result.Fans = append(result.Fans, FanStatus{
			ID:             fan.ID,
			Name:           fan.Name,
			Channel:        fan.Channel,
			RPM:            fan.RPM,
			PWM:            fan.PWM,
			PWMPercent:     pwmToPercent(fan.PWM),
			Mode:           fan.Mode,
			Selected:       len(selected[fan.ID]) > 0,
			ControlSources: append([]string(nil), selected[fan.ID]...),
		})
	}
	cpuTemperature, cpuErr := m.controlTemperature()
	if cpuErr == nil {
		result.TemperatureC = cpuTemperature
		result.CPUTemperatureC = cpuTemperature
		if len(cfg.Curve) >= 2 {
			result.CPUTargetPWMPercent = interpolatePWMPercent(cfg.Curve, cpuTemperature, cfg.MinPWMPercent, cfg.EmergencyTempC)
		}
	} else if cfg.Enabled {
		result.LastError = combineError(result.LastError, cpuErr)
	}
	storage := m.StorageStatus()
	hddTemperature, hddAvailable := maximumStorageTemperature(storage, "front", cfg.HDDSlotIDs)
	if hddAvailable {
		result.HDDTemperatureC = hddTemperature
		if len(cfg.HDDCurve) >= 2 {
			result.HDDTargetPWMPercent = interpolatePWMPercent(cfg.HDDCurve, hddTemperature, cfg.MinPWMPercent, cfg.EmergencyTempC)
		}
	}
	nvmeTemperature, nvmeAvailable := maximumStorageTemperature(storage, "m2", cfg.NVMeSlotIDs)
	if nvmeAvailable {
		result.NVMeTemperatureC = nvmeTemperature
		if len(cfg.NVMeCurve) >= 2 {
			result.NVMeTargetPWMPercent = interpolatePWMPercent(cfg.NVMeCurve, nvmeTemperature, cfg.MinPWMPercent, cfg.EmergencyTempC)
		}
	}
	result.DiskTemperatureC = max(result.HDDTemperatureC, result.NVMeTemperatureC)
	if cfg.OffMode {
		// 关停档实际下发恒为 0%（维修安全策略：紧急温度/温度源失效也只蜂鸣
		// 报警、保持停转，见 errOffModeAlarm）。别用「曲线 + 最低转速」算出来
		// 的 40% 当目标，否则界面显示 40% 而风扇其实停着，两边互相打架。
		result.CPUTargetPWMPercent = 0
		result.HDDTargetPWMPercent = 0
		result.NVMeTargetPWMPercent = 0
		result.DiskTargetPWMPercent = 0
		result.TargetPWMPercent = 0
		result.ControlSource = ""
		return result
	}
	result.DiskTargetPWMPercent = max(result.HDDTargetPWMPercent, result.NVMeTargetPWMPercent)
	result.TargetPWMPercent = max(result.CPUTargetPWMPercent, result.HDDTargetPWMPercent, result.NVMeTargetPWMPercent)
	result.ControlSource = fanControlSource(result.CPUTargetPWMPercent, result.HDDTargetPWMPercent, result.NVMeTargetPWMPercent)
	return result
}

func jsonUnmarshalStrict(data []byte, value any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

// ---- 风扇调试控制（调试页勾选显示；接管粒度为单个风扇） ----

func clampInt(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

type fanDebugAutoEntry struct {
	Step     int    // 每次递增量(单位随 Unit)
	Interval int    // 秒
	Unit     string // "percent"(转速%) 或 "pwm"(原始占空比)
	LastRamp time.Time
	Running  bool // 该风扇的自动递增进行中
	Done     bool // 已到该单位上限
}

// fanDebugAutoTest 持有全部风扇的自动递增状态;goroutine 在有风扇递增时存活。
type fanDebugAutoTest struct {
	stop    chan struct{}
	entries map[string]fanDebugAutoEntry // 风扇 ID → 各自的递增参数与进度
}

// SetFanDebugTakeover 接管/释放单个风扇：接管后该风扇脱离一切温控曲线
// （曲线目标不再写入），转速仅由调试接口驱动；释放后立即回到曲线控制。
// 接管瞬间捕获 BIOS 原始状态，供插件停止/卸载时恢复。
func (m *Manager) SetFanDebugTakeover(id string, taken bool) error {
	m.mu.Lock()
	fans, err := m.DiscoverFans()
	if err != nil {
		m.mu.Unlock()
		return err
	}
	var target *FanDevice
	for i := range fans {
		if fans[i].ID == id {
			target = &fans[i]
			break
		}
	}
	if target == nil {
		m.mu.Unlock()
		return fmt.Errorf("fan %s was not found", id)
	}
	if m.fanDebugTakenOver == nil {
		m.fanDebugTakenOver = map[string]int{}
	}
	if m.fanDebugUnits == nil {
		m.fanDebugUnits = map[string]string{}
	}
	handBack := false
	if taken {
		if _, ok := m.fanDebugTakenOver[id]; !ok {
			if err := m.captureOriginalFanLocked(*target); err != nil {
				m.mu.Unlock()
				return err
			}
		}
		// 从当前转速无缝接管，避免转速跳变。**单位重置为 percent**：接管值
		// 的语义就是百分比——若沿用上一次接管留下的 rpm 单位，闭环会把 40%
		// 当 40 RPM 把风扇降到近停转，紧急兜底解除回写也会算错
		m.fanDebugTakenOver[id] = pwmToPercent(target.PWM)
		m.fanDebugUnits[id] = "percent"
	} else {
		delete(m.fanDebugTakenOver, id)
		// 单位与闭环状态一并清干净：残留的 rpm 单位会让下一次接管沿旧单位
		// 解释百分比值；残留的 miss/锁定/隔轮观察状态则会让闭环对已交还的
		// 风扇带着旧计数工作（nil map 的 delete 是安全 no-op）
		delete(m.fanDebugUnits, id)
		delete(m.fanRPMLoopMiss, id)
		delete(m.fanRPMLoopLocked, id)
		delete(m.fanRPMLoopSettling, id)
		delete(m.fanRPMLoopLastDir, id)
		// 释放接管同时终止该风扇的自动递增:条目删除,行内开关随之复位
		if m.fanDebugAuto != nil {
			delete(m.fanDebugAuto.entries, id)
		}
		// 立即交还控制,不等曲线循环下一拍。**完整恢复存档的原始状态**
		// (PWM + pwm*_enable 模式)——只写 PWM 的话,原本由 BIOS 自动温控
		// (mode≥2)的风扇会卡死在固定转速。曲线开着时函数尾部的
		// ApplyFanCurrent 随即按曲线接管(enable=1);风扇控制停用/不在
		// 曲线列表的风扇则真正回到接管前的硬件模式。
		// 关停档例外:OffMode 期间绝不写非零 PWM——restoreFan 会把原始非零
		// 转速写回去,再由 ApplyFanCurrent 覆盖成 0,中间那笔写入与「关停档
		// 一律不起转」冲突,必须整个跳过,让 ApplyFanCurrent 直接写 0%。
		if !m.fanOffModeActiveLocked() {
			if original, ok := m.originalFanLocked(id); ok {
				if err := restoreFan(*target, original); err != nil {
					// 恢复失败不阻塞交还:曲线开着时 ApplyFanCurrent 兜底接管
					m.fanDebugLastError = fmt.Sprintf("fan %s hand-back restore: %v", id, err)
				}
			}
		}
		handBack = true
	}
	m.mu.Unlock()
	if handBack {
		if cfg, cfgErr := m.loadConfigLocked(); cfgErr == nil && cfg.Fan.Enabled {
			_ = m.ApplyFanCurrent()
		}
	}
	return nil
}

// originalFanPWMLocked 返回风扇第一次被接管/纳入控制时存档的 PWM;无存档
// 返回 -1。调用方须持 m.mu。
func (m *Manager) originalFanPWMLocked(id string) (int64, error) {
	state, err := m.loadFanStateLocked()
	if err != nil {
		return -1, err
	}
	for _, original := range state.Fans {
		if original.ID == id {
			return original.PWM, nil
		}
	}
	return -1, nil
}

// originalFanLocked 返回风扇接管前存档的完整原始状态(PWM+pwm*_enable 模式);
// 无存档返回 ok=false。交还接管时用它把硬件恢复到接管前的真实模式。调用方
// 须持 m.mu。
func (m *Manager) originalFanLocked(id string) (originalFan, bool) {
	state, err := m.loadFanStateLocked()
	if err != nil {
		return originalFan{}, false
	}
	for _, original := range state.Fans {
		if original.ID == id {
			return original, true
		}
	}
	return originalFan{}, false
}

// fanDebugUnitMax 返回单位对应的调试值上限:pwm 0–255,percent 0–100。
// rpm 的上限是每风扇的满转基准,走 fanDebugUnitMaxFor。
func fanDebugUnitMax(unit string) int {
	if unit == "pwm" {
		return 255
	}
	if unit == "rpm" {
		return fanRPMBaseDefault
	}
	return 100
}

// fanDebugUnitMaxFor 单位上限的每风扇版:rpm 用该风扇的满转基准(实测标定),
// 其余单位与风扇无关。调用方须持 m.mu。
func (m *Manager) fanDebugUnitMaxFor(id, unit string) int {
	if unit == "rpm" {
		return m.fanDebugRPMBaseLocked(id)
	}
	return fanDebugUnitMax(unit)
}

// fanDebugRaw 把单位值换算为写入硬件的原始 PWM。rpm 按满转基准线性换算
// (value/基准×100 再转 PWM;基准 2000 时即旧的 (value+10)/20)。
func fanDebugRaw(unit string, value, rpmBase int) int {
	if unit == "pwm" {
		return value
	}
	if unit == "rpm" {
		return percentToPWM((value*100 + rpmBase/2) / rpmBase)
	}
	return percentToPWM(value)
}

// SetFanDebugValue 设定单个被接管风扇的调试值。value 的单位由 unit 决定:
// "rpm" 为转速(0–该风扇满转基准,按基准=100% 换算),"percent" 为百分比
// (0–100),"pwm" 为原始占空比(0–255)。value 存储与自动递增都按该单位进行。
func (m *Manager) SetFanDebugValue(id string, value int, unit string) error {
	if unit != "percent" && unit != "pwm" && unit != "rpm" {
		return fmt.Errorf("未知的调节单位 %q", unit)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fanOffModeActiveLocked() {
		// 关停档维修期间不接受任何主动调速：调试锁定的转速会让风扇转起来。
		return errFanOffModeBusy
	}
	if m.fanDebugEmergency {
		return fmt.Errorf("CPU 温度达到紧急阈值，风扇已强制满速，调速暂不可用")
	}
	value = clampInt(value, 0, m.fanDebugUnitMaxFor(id, unit))
	if _, ok := m.fanDebugTakenOver[id]; !ok {
		return fmt.Errorf("风扇 %s 未被接管", id)
	}
	// 运行中的参数锁定由前端禁用控件承担;这里不做拒绝——运行中切换单位
	// 走本函数做等比换算,是无损操作(见 TestFanDebugUnitSwitchMidRamp)。
	fans, err := m.DiscoverFans()
	if err != nil {
		return err
	}
	// rpm 单位：特性表插值得初值（"大概转速"），闭环随后按反馈微调到容差内；
	// 其余单位保持精确换算。新目标重置闭环计数,给收敛重新计时。
	var raw int
	if unit == "rpm" {
		raw = m.rpmToPWMEstimateLocked(id, value)
		if m.fanRPMLoopMiss == nil {
			m.fanRPMLoopMiss = map[string]int{}
		}
		if m.fanRPMLoopLocked == nil {
			m.fanRPMLoopLocked = map[string]bool{}
		}
		m.fanRPMLoopMiss[id] = 0
		m.fanRPMLoopLocked[id] = false
		m.ensureFanRPMCloseLoop()
	} else {
		raw = fanDebugRaw(unit, value, m.fanDebugRPMBaseLocked(id))
	}
	var errs []error
	for i := range fans {
		if fans[i].ID != id {
			continue
		}
		if err := setFanPWMRaw(fans[i], raw); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", fans[i].ID, err))
		}
	}
	m.fanDebugTakenOver[id] = value
	oldUnit := m.fanDebugUnits[id]
	m.fanDebugUnits[id] = unit
	// 递增条目在跑时同步单位并把步进等比换算(200 RPM → 10%),递增循环以 fanDebugUnits 为准
	if m.fanDebugAuto != nil {
		if entry, ok := m.fanDebugAuto.entries[id]; ok {
			if oldUnit == "" {
				oldUnit = entry.Unit
			}
			if oldUnit != "" && oldUnit != unit && fanDebugUnitMax(oldUnit) > 0 {
				entry.Step = entry.Step * m.fanDebugUnitMaxFor(id, unit) / m.fanDebugUnitMaxFor(id, oldUnit)
				if entry.Step < 1 {
					entry.Step = 1
				}
			}
			entry.Unit = unit
			m.fanDebugAuto.entries[id] = entry
		}
	}
	if len(errs) > 0 {
		m.fanDebugLastError = errors.Join(errs...).Error()
		return errors.Join(errs...)
	}
	m.fanDebugLastError = ""
	return nil
}

// fanDebugValueToPWMLocked 把某单位的调试值换算为物理 PWM(0–255)。rpm 优先
// 用标定特性表插值,无表时按满转基准(未标定为默认 2000)线性换算。调用方须持 m.mu。
func (m *Manager) fanDebugValueToPWMLocked(id string, value int, unit string) int {
	switch unit {
	case "pwm":
		return clampInt(value, 0, 255)
	case "rpm":
		return m.rpmToPWMEstimateLocked(id, value)
	default:
		return clampInt(percentToPWM(clampInt(value, 0, 100)), 0, 255)
	}
}

// pwmToRPMEstimateLocked 是 rpmToPWMEstimateLocked 的反向:特性表内按档位夹逼
// 插值,表外与空表按满转基准线性换算。调用方须持 m.mu。
func (m *Manager) pwmToRPMEstimateLocked(id string, pwm int) int {
	pwm = clampInt(pwm, 0, 255)
	base := m.fanDebugRPMBaseLocked(id)
	linear := (pwm*base + 127) / 255
	table := m.fanRPMLearnedLocked(id)
	if len(table) == 0 {
		return linear
	}
	slots := make([]int, 0, len(table))
	for slot := range table {
		slots = append(slots, slot)
	}
	sort.Ints(slots)
	if rpm, ok := table[pwm]; ok {
		return rpm
	}
	if pwm < slots[0] {
		// 低于表内最低档:在 (0,0) 与最低档之间线性插值
		lo := slots[0]
		return table[lo] * pwm / lo
	}
	last := slots[len(slots)-1]
	if pwm > last {
		// 表外高档:在 (最高档, 实测) 与 (255, 满转基准) 之间线性插值,
		// 与 rpmToPWMEstimateLocked 同一外推规则,表顶两侧连续
		topRPM := max(base, table[last])
		return table[last] + ((pwm-last)*(topRPM-table[last])+(255-last)/2)/(255-last)
	}
	for i := 1; i < len(slots); i++ {
		lo, hi := slots[i-1], slots[i]
		if pwm < hi {
			return table[lo] + (pwm-lo)*(table[hi]-table[lo])/(hi-lo)
		}
	}
	return linear
}

// convertFanDebugValueLocked 把调试值从 from 单位换算到 to 单位,保持同一
// 物理输出(经 PWM 中转)。调用方须持 m.mu。
func (m *Manager) convertFanDebugValueLocked(id string, value int, from, to string) int {
	if from == "" {
		from = "percent"
	}
	if from == to {
		return value
	}
	pwm := m.fanDebugValueToPWMLocked(id, value, from)
	switch to {
	case "pwm":
		return pwm
	case "rpm":
		return clampInt(m.pwmToRPMEstimateLocked(id, pwm), 0, m.fanDebugUnitMaxFor(id, "rpm"))
	default:
		return clampInt(pwmToPercent(int64(pwm)), 0, 100)
	}
}

// convertFanDebugTakenOverLocked 单位变化时把 fanDebugTakenOver[id] 换算到
// newUnit;切到 rpm 时重置闭环计数让其按新目标重新收敛。调用方须持 m.mu。
func (m *Manager) convertFanDebugTakenOverLocked(id, newUnit string) {
	value, taken := m.fanDebugTakenOver[id]
	if !taken {
		return
	}
	oldUnit := m.fanDebugUnits[id]
	if oldUnit == "" {
		oldUnit = "percent"
	}
	if oldUnit == newUnit {
		return
	}
	m.fanDebugTakenOver[id] = m.convertFanDebugValueLocked(id, value, oldUnit, newUnit)
	if newUnit == "rpm" {
		if m.fanRPMLoopMiss == nil {
			m.fanRPMLoopMiss = map[string]int{}
		}
		if m.fanRPMLoopLocked == nil {
			m.fanRPMLoopLocked = map[string]bool{}
		}
		m.fanRPMLoopMiss[id] = 0
		m.fanRPMLoopLocked[id] = false
	}
}

// SetFanDebugAuto 单风扇自动递增的开/关:开启后以该风扇当前调试转速为基础,
// 每 interval 秒 +step 递增到单位上限自动完成(停在上限);关闭即停在当前转速
// 并保留步进/间隔,便于原样重新打开。各风扇互相独立——一个风扇递增时另一个
// 可以手动测试。上次已跑到上限时再次开启会归零重跑。
func (m *Manager) SetFanDebugAuto(id string, running bool, step, intervalSeconds int, unit string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fanDebugAuto == nil {
		m.fanDebugAuto = &fanDebugAutoTest{entries: map[string]fanDebugAutoEntry{}}
	}
	if m.fanDebugAuto.entries == nil {
		m.fanDebugAuto.entries = map[string]fanDebugAutoEntry{}
	}
	if unit != "pwm" && unit != "rpm" {
		unit = "percent"
	}
	// 关闭总是允许(幂等);开启要求风扇已接管且参数在该单位范围内
	if running {
		if m.fanOffModeActiveLocked() {
			return errFanOffModeBusy
		}
		if _, taken := m.fanDebugTakenOver[id]; !taken {
			return fmt.Errorf("风扇 %s 未被接管,请先勾选接管", id)
		}
		if step < 1 || step > m.fanDebugUnitMaxFor(id, unit) {
			return fmt.Errorf("风扇 %s 的递增转速需在 1–%d 之间", id, m.fanDebugUnitMaxFor(id, unit))
		}
		if intervalSeconds < 1 {
			return fmt.Errorf("风扇 %s 的递增间隔需至少 1 秒", id)
		}
		intervalSeconds = clampFanAutoInterval(intervalSeconds)
	}
	entry := m.fanDebugAuto.entries[id]
	if running {
		// 单位切换(百分比 ↔ rpm ↔ pwm)时先把已接管的基准值换算到新单位,
		// 否则 50% 会被当成 50 RPM,风扇骤降到极低转速(#30)
		m.convertFanDebugTakenOverLocked(id, unit)
		// 基准已在单位上限(上次跑完):归零重跑,避免开了立刻又完成
		if base, ok := m.fanDebugTakenOver[id]; ok && base >= m.fanDebugUnitMaxFor(id, unit) {
			m.fanDebugTakenOver[id] = 0
		}
		entry.Step = step
		entry.Interval = intervalSeconds
		entry.Unit = unit
		entry.LastRamp = time.Now()
		entry.Done = false
		entry.Running = true
		if m.fanDebugUnits == nil {
			m.fanDebugUnits = map[string]string{}
		}
		m.fanDebugUnits[id] = unit
	} else {
		entry.Running = false
		entry.Done = false
	}
	m.fanDebugAuto.entries[id] = entry
	if running {
		m.ensureFanDebugAutoLoop()
	}
	return nil
}

// ensureFanDebugAutoLoop 保证递增 goroutine 存活;无风扇递增时自动退出。
func (m *Manager) ensureFanDebugAutoLoop() {
	if m.fanDebugAutoLoopRunning {
		return
	}
	m.fanDebugAutoLoopRunning = true
	stop := make(chan struct{})
	m.fanDebugAuto.stop = stop
	go m.runFanDebugAuto(stop)
}

// runFanDebugAuto 500ms tick 一次:对每个 Running 且未 Done 的风扇,距上次
// 递增满 interval 秒 → 值 +step(封顶单位上限),写 PWM 并推进;全部完成或
// 无 Running 条目时 goroutine 退出。
func (m *Manager) runFanDebugAuto(stop chan struct{}) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		m.mu.Lock()
		now := time.Now()
		fans, ferr := m.DiscoverFans()
		if ferr != nil {
			// 偶发失败保持存活等下个 tick 重试:直接退出会让 goroutine 标记
			// 与 entries.Running 脱节——状态显示递增中,实际已无人推进
			m.fanDebugLastError = ferr.Error()
			m.mu.Unlock()
			continue
		}
		anyRunning := false
		// 关停档维修期间自动递增只保持存活、绝不写 PWM（关停档优先级最高）。
		offMode := m.fanOffModeActiveLocked()
		var errs []error
		for id, entry := range m.fanDebugAuto.entries {
			if !entry.Running || entry.Done {
				continue
			}
			if offMode {
				anyRunning = true
				continue
			}
			if m.fanDebugEmergency {
				// 紧急满速兜底最高优先级：递增暂停推进（不写 PWM、
				// LastRamp 不动），兜底解除后从暂停点继续
				anyRunning = true
				continue
			}
			if now.Sub(entry.LastRamp) < time.Duration(entry.Interval)*time.Second {
				anyRunning = true
				continue
			}
			unit := m.fanDebugUnits[id]
			if unit == "" {
				unit = entry.Unit
			}
			unitMax := m.fanDebugUnitMaxFor(id, unit)
			value := m.fanDebugTakenOver[id] + entry.Step
			if value > unitMax {
				value = unitMax
			}
			// rpm 步进同样查特性表取初值(大概转速),其余单位精确换算
			var raw int
			if unit == "rpm" {
				raw = m.rpmToPWMEstimateLocked(id, value)
			} else {
				raw = fanDebugRaw(unit, value, m.fanDebugRPMBaseLocked(id))
			}
			for i := range fans {
				if fans[i].ID == id {
					if err := setFanPWMRaw(fans[i], raw); err != nil {
						errs = append(errs, fmt.Errorf("%s: %w", id, err))
					}
					break
				}
			}
			m.fanDebugTakenOver[id] = value
			entry.LastRamp = now
			// PWM 已顶格:风扇到硬件上限,RPM 单位下继续按设定值空转只会
			// "转不上去一直挣扎"(设定值虚高过实际可达),提前完成并保持
			// 全速;完成后的闭环另有顶格冻结兜底
			if raw >= 255 {
				entry.Done = true
				entry.Running = false
			} else if value >= unitMax {
				entry.Done = true
				entry.Running = false
			} else {
				anyRunning = true
			}
			m.fanDebugAuto.entries[id] = entry
		}
		if len(errs) > 0 {
			m.fanDebugLastError = errors.Join(errs...).Error()
		}
		if !anyRunning {
			m.fanDebugAutoLoopRunning = false
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
	}
}

// stopFanDebugAutoLocked 停止全部风扇的自动递增,保持当前转速。
func (m *Manager) stopFanDebugAutoLocked() {
	if m.fanDebugAuto != nil {
		for id, entry := range m.fanDebugAuto.entries {
			entry.Running = false
			m.fanDebugAuto.entries[id] = entry
		}
	}
}

// StartFanDebugAutoBatch 批量启动多个风扇的自动递增(条目须已接管且参数合法,
// 否则整批报错不启动)。
func (m *Manager) StartFanDebugAutoBatch(entries map[string]fanDebugAutoEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fanOffModeActiveLocked() {
		return errFanOffModeBusy
	}
	if m.fanDebugAuto == nil {
		m.fanDebugAuto = &fanDebugAutoTest{entries: map[string]fanDebugAutoEntry{}}
	}
	if m.fanDebugAuto.entries == nil {
		m.fanDebugAuto.entries = map[string]fanDebugAutoEntry{}
	}
	for id, entry := range entries {
		if _, taken := m.fanDebugTakenOver[id]; !taken {
			return fmt.Errorf("风扇 %s 未被接管,请先勾选接管", id)
		}
		if entry.Step < 1 || entry.Step > m.fanDebugUnitMaxFor(id, entry.Unit) {
			return fmt.Errorf("风扇 %s 的递增转速超出该单位上限", id)
		}
		if entry.Interval < 1 {
			return fmt.Errorf("风扇 %s 的递增间隔需至少 1 秒", id)
		}
	}
	m.stopFanDebugAutoLocked()
	now := time.Now()
	cleaned := make(map[string]fanDebugAutoEntry, len(entries))
	for id, entry := range entries {
		// 钳制必须写回 cleaned(这里存的是拷贝):range 迭代变量在第一个
		// 循环里的赋值改不到 map 里的原值
		entry.Interval = clampFanAutoInterval(entry.Interval)
		entry.LastRamp = now
		entry.Running = true
		cleaned[id] = entry
	}
	m.fanDebugAuto.entries = cleaned
	m.ensureFanDebugAutoLoop()
	return nil
}

// StopFanDebugAuto 停止自动递增测试，保持当前转速。
func (m *Manager) StopFanDebugAuto() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopFanDebugAutoLocked()
}

type FanDebugFan struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Channel      int    `json:"channel"`
	RPM          int64  `json:"rpm"`
	PWMPercent   int    `json:"pwm_percent"`
	Mode         int64  `json:"mode"`
	TakenOver    bool   `json:"taken_over"`
	DebugPercent int    `json:"debug_percent,omitempty"`
	DebugUnit    string `json:"debug_unit,omitempty"`    // 该风扇调试值的单位(percent/pwm),未设置按 percent
	RPMMax       int    `json:"rpm_max"`                 // 该风扇的满转基准(RPM):调试转速上限与 rpm 换算分母,未标定为 2000
	RPMTracking  bool   `json:"rpm_tracking,omitempty"`  // rpm 闭环跟踪中(目标已设定且未因不可达暂停)
	RPMLocked    bool   `json:"rpm_locked,omitempty"`    // rpm 闭环达标(|实测-目标| 在容差内)
	AutoStep     int    `json:"auto_step,omitempty"`     // 自动递增:每 Interval 秒 +Step
	AutoInterval int    `json:"auto_interval,omitempty"` // 自动递增间隔(秒)
	AutoRunning  bool   `json:"auto_running"`            // 自动递增进行中
	AutoDone     bool   `json:"auto_done,omitempty"`     // 已到该单位上限
}

type FanDebugState struct {
	Emergency   bool          `json:"emergency"`
	AutoRunning bool          `json:"auto_running"` // 任一风扇的自动递增进行中
	LastError   string        `json:"last_error,omitempty"`
	Fans        []FanDebugFan `json:"fans"`
}

// ---- 主动标定满转基准 ----

const (
	fanCalibSampleInterval = 500 * time.Millisecond // 稳态采样间隔
	fanCalibSteadyWindow   = 4                      // 最近 N 个读数极差 ≤ 阈值即稳态
	fanCalibSpreadPct      = 3                      // 稳态极差阈值（闭环容差级别，比被动学习略紧）
	fanCalibMinSamples     = 3                      // 超时兜底所需的最少全速读数
	fanCalibTimeout        = 9 * time.Second        // 风扇升速一般 1~3 秒，9 秒覆盖慢扇
)

// CalibrateFanRPM 主动标定单个风扇的满转基准：全速运转至读数稳态（最多约
// 9 秒），稳态中位数经特性表入档（240 档 + 更新满转基准并落盘），随后恢复
// 标定前的控制状态——原接管恢复原调试值与原 PWM，原未接管交还曲线控制。
// 标定期间该风扇的 RPM 闭环挂起，避免微调与满速采样互相打架。返回新基准。
func (m *Manager) CalibrateFanRPM(id string) (int, error) {
	m.mu.Lock()
	if m.fanOffModeActiveLocked() {
		// 标定要写 255（全速），关停档维修期间会把风扇转起来，直接拒绝。
		m.mu.Unlock()
		return 0, errFanOffModeBusy
	}
	if m.fanDebugEmergency {
		// 紧急满速兜底期间不启动标定：标定结束会恢复标定前转速，覆盖兜底
		m.mu.Unlock()
		return 0, fmt.Errorf("CPU 温度达到紧急阈值，风扇满速兜底中，暂不可标定")
	}
	if m.fanDebugAuto != nil {
		if entry, ok := m.fanDebugAuto.entries[id]; ok && entry.Running {
			m.mu.Unlock()
			return 0, fmt.Errorf("风扇 %s 的自动测试进行中,请先关闭该行的自动测试再标定", id)
		}
	}
	fans, _ := m.DiscoverFans()
	var target *FanDevice
	for i := range fans {
		if fans[i].ID == id {
			target = &fans[i]
			break
		}
	}
	if target == nil {
		m.mu.Unlock()
		return 0, fmt.Errorf("fan %s was not found", id)
	}
	// 记录标定前的控制状态，结束时原样恢复
	originalPWM := target.PWM
	originalValue, wasTaken := m.fanDebugTakenOver[id]
	originalUnit := m.fanDebugUnits[id]
	if !wasTaken {
		// 临时接管：从当前转速无缝进入全速（capture 保证释放后可恢复）
		if err := m.captureOriginalFanLocked(*target); err != nil {
			m.mu.Unlock()
			return 0, err
		}
		if m.fanDebugTakenOver == nil {
			m.fanDebugTakenOver = map[string]int{}
		}
		m.fanDebugTakenOver[id] = pwmToPercent(target.PWM)
	}
	if m.fanRPMSuspend == nil {
		m.fanRPMSuspend = map[string]bool{}
	}
	m.fanRPMSuspend[id] = true
	writeErr := setFanPWMRaw(*target, 255)
	if writeErr != nil {
		m.restoreAfterCalibrateLocked(id, wasTaken, originalValue, originalUnit, originalPWM)
		m.mu.Unlock()
		return 0, fmt.Errorf("全速写入失败: %w", writeErr)
	}
	// 记下采样开始时的关停档代次：期间若切进关停档（PWM 被写成 0），读数作废。
	offGen := m.fanOffGen
	m.mu.Unlock()

	median, samples, calibErr := m.sampleFanRPMSteady(id)
	// 关停档可能在采样期间被开启又关闭，结束时再看状态会漏掉。入表时在同一把
	// 锁内核对代次：期间进入过关停档的全速基准不可信，不能落盘。
	if calibErr == nil {
		m.recordFanRPMData(id, 240, median, true, &offGen) // 主动标定:绕过节流立即落盘 // 入特性表最高档并维护满转基准（自带加锁）
	}
	m.mu.Lock()
	m.restoreAfterCalibrateLocked(id, wasTaken, originalValue, originalUnit, originalPWM)
	base := m.fanDebugRPMBaseLocked(id)
	m.mu.Unlock()
	if calibErr != nil {
		return 0, calibErr
	}
	_ = samples
	return base, nil
}

// sampleFanRPMSteady 全速读数循环：每 500ms 读一次，最近 4 个读数极差 ≤3%
// 即稳态返回中位数；超时则退而求其次用已有读数的中位数（至少 fanCalibMinSamples
// 个），否则报错。调用方不持 m.mu。
func (m *Manager) sampleFanRPMSteady(id string) (int, int, error) {
	deadline := time.Now().Add(fanCalibTimeout)
	var buf []int
	for {
		time.Sleep(fanCalibSampleInterval)
		m.mu.Lock()
		fans, err := m.DiscoverFans()
		m.mu.Unlock()
		if err == nil {
			for i := range fans {
				if fans[i].ID != id {
					continue
				}
				if rpm := int(fans[i].RPM); rpm >= fanRPMCalibMinRPM && rpm <= fanRPMCalibMaxRPM {
					buf = append(buf, rpm)
					if len(buf) > fanCalibSteadyWindow {
						buf = buf[len(buf)-fanCalibSteadyWindow:]
					}
				}
				break
			}
		}
		if len(buf) >= fanCalibSteadyWindow {
			sorted := append([]int(nil), buf...)
			sort.Ints(sorted)
			lo, hi, median := sorted[0], sorted[len(sorted)-1], sorted[len(sorted)/2]
			if (hi-lo)*100 <= median*fanCalibSpreadPct {
				return median, len(buf), nil
			}
		}
		if time.Now().After(deadline) {
			if len(buf) >= fanCalibMinSamples {
				sorted := append([]int(nil), buf...)
				sort.Ints(sorted)
				return sorted[len(sorted)/2], len(buf), nil
			}
			return 0, len(buf), fmt.Errorf("转速读数不稳定（%d 个有效读数），无法标定；请检查风扇后重试", len(buf))
		}
	}
}

// restoreAfterCalibrateLocked 恢复标定前的控制状态。两个分支都立即写回
// 原始 PWM:接管分支重建调试值,未接管分支交还曲线控制——若控制已停用,
// 不写回会让风扇停在标定的全速上。调用方须持 m.mu。
// 关停档例外:标定期间另一个 goroutine 可能启用了 OffMode。此时原始 PWM
// 可能非零,直接写回会让维修关停期间风扇转起来,必须改成写 0;全速采样的
// 入档也不再可信,调用方已在外层判断 calibErr && !wasOff 才落盘。
func (m *Manager) restoreAfterCalibrateLocked(id string, wasTaken bool, originalValue int, originalUnit string, originalPWM int64) {
	delete(m.fanRPMSuspend, id)
	offMode := m.fanOffModeActiveLocked()
	fans, err := m.DiscoverFans()
	if err == nil {
		for i := range fans {
			if fans[i].ID == id {
				targetPWM := int(originalPWM)
				if offMode {
					targetPWM = 0
				}
				_ = setFanPWMRaw(fans[i], targetPWM)
				break
			}
		}
	}
	if wasTaken {
		m.fanDebugTakenOver[id] = originalValue
		if originalUnit != "" {
			if m.fanDebugUnits == nil {
				m.fanDebugUnits = map[string]string{}
			}
			m.fanDebugUnits[id] = originalUnit
		}
		return
	}
	// 标定前未接管：交还曲线控制（原始状态已在接管时存档）。单位与闭环
	// 状态同样要清干净（与 SetFanDebugTakeover 的释放分支同一理由）
	delete(m.fanDebugTakenOver, id)
	delete(m.fanDebugUnits, id)
	delete(m.fanRPMLoopMiss, id)
	delete(m.fanRPMLoopLocked, id)
	delete(m.fanRPMLoopSettling, id)
	delete(m.fanRPMLoopLastDir, id)
	if m.fanDebugAuto != nil {
		delete(m.fanDebugAuto.entries, id)
	}
}

// FanDebugState 汇总调试状态与全部已发现风扇（按接口枚举，0 转也列出）。
func (m *Manager) FanDebugState() FanDebugState {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := FanDebugState{
		Emergency: m.fanDebugEmergency,
		LastError: m.fanDebugLastError,
		Fans:      []FanDebugFan{},
	}
	_ = m.fanDebugAuto
	fans, err := m.DiscoverFans()
	if err != nil {
		state.LastError = err.Error()
		return state
	}
	for _, fan := range fans {
		item := FanDebugFan{
			ID:         fan.ID,
			Name:       fan.Name,
			Channel:    fan.Channel,
			RPM:        fan.RPM,
			PWMPercent: pwmToPercent(fan.PWM),
			Mode:       fan.Mode,
			RPMMax:     m.fanDebugRPMBaseLocked(fan.ID),
		}
		if m.fanDebugAuto != nil {
			if entry, ok := m.fanDebugAuto.entries[fan.ID]; ok {
				item.AutoStep = entry.Step
				item.AutoInterval = entry.Interval
				item.AutoRunning = entry.Running
				item.AutoDone = entry.Done
				if entry.Running {
					state.AutoRunning = true
				}
			}
		}
		if unit, ok := m.fanDebugUnits[fan.ID]; ok {
			item.DebugUnit = unit
			if unit == "rpm" {
				if _, taken := m.fanDebugTakenOver[fan.ID]; taken {
					// rpm 闭环状态:目标已设定即跟踪中;连续不可达超限时暂停
					item.RPMTracking = m.fanRPMLoopMiss[fan.ID] < fanRPMCloseLoopMaxMiss
					item.RPMLocked = m.fanRPMLoopLocked[fan.ID]
				}
			}
		}
		if percent, ok := m.fanDebugTakenOver[fan.ID]; ok {
			item.TakenOver = true
			item.DebugPercent = percent
		}
		state.Fans = append(state.Fans, item)
	}
	return state
}
