package powerguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	gpioConfigVersion = 1
	gpioDebounce      = 100 * time.Millisecond

	GPIOActionNone           = "none"
	GPIOActionLog            = "log"
	GPIOActionRefreshStorage = "refresh_storage"
	GPIOActionSMARTCheck     = "smart_check"
	GPIOActionReapplyPlugin  = "reapply_plugin"
	GPIOActionRefreshSDCard  = "refresh_sdcard"
	GPIOActionScriptPrefix   = "script:"

	gpioScriptMaxCount  = 32
	gpioScriptMaxName   = 64
	gpioScriptMaxBody   = 64 << 10
	gpioScriptOutputMax = 64 << 10
)

var (
	gpioScriptIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	gpioScriptTimeout   = 30 * time.Second
	// ErrGPIOFeedbackUnavailable means that an optional feedback device is not
	// present or cannot be opened. Callers should log and continue with the
	// button action.
	ErrGPIOFeedbackUnavailable = errors.New("gpio feedback is unavailable")
)

type GPIOActions struct {
	Short   string `json:"short"`
	Hold3S  string `json:"hold_3s"`
	Hold9S  string `json:"hold_9s"`
	Hold15S string `json:"hold_15s"`
}

type GPIOButtonConfig struct {
	ID      string      `json:"id"`
	Actions GPIOActions `json:"actions"`
}

type GPIOScript struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Body string `json:"body"`
}

type GPIOConfig struct {
	Version int              `json:"version"`
	Enabled bool             `json:"enabled"`
	Scripts []GPIOScript     `json:"scripts,omitempty"`
	Backups []GPIOBackupJob  `json:"backups,omitempty"` // 图形化配置的备份任务，动作 backup:<id> 引用（backup.go）
	Autos   []GPIOAutoBackup `json:"autos,omitempty"`   // 自动备份规则（打标卷），动作 auto_backup:<id> 引用（backup_auto.go）
	// AutoVolumes NAS 侧保存的打标卷记录（backup_auto.go）：设备拔掉后列表里仍能
	// 看到它、并支持离线取消打标。由打标接口维护，保存按键配置时不允许前端覆盖
	// （见 SaveGPIOConfig）——否则页面一保存就把记录抹掉。
	AutoVolumes []AutoBackupVolumeRecord `json:"auto_volumes,omitempty"`
	Buttons     []GPIOButtonConfig       `json:"buttons"`
	// Migrations 记录已经应用过的一次性默认值补位（gpioDefaultsMigration*）。
	// 有了它，用户手动把某个档位清成「无动作」之后不会被再次填回。
	Migrations []string `json:"migrations,omitempty"`
	// StorageNotify 是否在识别到新插入的存储设备时用蜂鸣器提示（见 storage_notify.go）。
	// 指针类型是为了区分「用户关掉了」与「老配置里还没有这个字段」——后者必须
	// 按默认（开启）处理，否则升级上来的机器会静默失去这个功能。
	StorageNotify *bool `json:"storage_notify,omitempty"`
}

// StorageNotifyEnabled 报告存储设备插入提示音是否开启：字段缺省视为开启。
func (config GPIOConfig) StorageNotifyEnabled() bool {
	return config.StorageNotify == nil || *config.StorageNotify
}

func gpioBoolPointer(value bool) *bool { return &value }

type GPIOButtonStatus struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Port       string `json:"port"`
	Bit        uint   `json:"bit"`
	Pressed    bool   `json:"pressed"`
	HeldMS     int64  `json:"held_ms,omitempty"`
	LastAction string `json:"last_action,omitempty"`
}

type GPIOStatus struct {
	Available bool               `json:"available"`
	Enabled   bool               `json:"enabled"`
	Buttons   []GPIOButtonStatus `json:"buttons"`
	LastEvent string             `json:"last_event,omitempty"`
	LastError string             `json:"last_error,omitempty"`
}

type GPIOEvent struct {
	ButtonID string
	Name     string
	Duration time.Duration
	Stage    string
	Action   string
	Script   *GPIOScript
	Backup   *GPIOBackupJob
	Auto     *GPIOAutoBackup
	Kind     GPIOEventKind
	Feedback *GPIOFeedbackPattern
}

type GPIOEventKind string

const (
	GPIOEventAction   GPIOEventKind = "action"
	GPIOEventFeedback GPIOEventKind = "feedback"
)

// GPIOFeedbackPattern is confirmation feedback emitted once when a hold
// threshold is reached. The business action remains deferred until release.
type GPIOFeedbackPattern struct {
	Tones   int
	Flashes int
}

func (event GPIOEvent) IsFeedback() bool {
	return event.Kind == GPIOEventFeedback && event.Feedback != nil
}

type gpioButtonSpec struct {
	ID   string
	Name string
	Port int64
	Bit  uint
}

var gpioButtonSpecs = []gpioButtonSpec{
	{ID: "copy", Name: "复制按键", Port: 0xA04, Bit: 6},
	{ID: "network", Name: "网络按键", Port: 0xA00, Bit: 3},
	{ID: "rear_reset", Name: "后置重置按键", Port: 0xA03, Bit: 6},
}

var allowedGPIOActions = map[string]bool{
	GPIOActionNone: true, GPIOActionLog: true,
	GPIOActionRefreshStorage: true, GPIOActionSMARTCheck: true,
	GPIOActionReapplyPlugin: true, GPIOActionRefreshSDCard: true,
}

type gpioButtonRuntime struct {
	initialized  bool
	baselineHigh bool
	rawPressed   bool
	rawChangedAt time.Time
	pressed      bool
	pressedAt    time.Time
	lastAction   string
	feedbackMask uint8
}

type gpioRuntime struct {
	available bool
	buttons   map[string]*gpioButtonRuntime
	lastEvent string
	lastError string
}

// defaultGPIOShortActions 开箱即用的默认绑定：只给网络键的短按绑定「刷新SD卡」
// ——这是换卡后读卡器认不出卡时最常用的一下；其余按键与档位一律无动作，留给用户
// 自己决定（避免「按一下不知道会做什么」）。
var defaultGPIOShortActions = map[string]string{
	"network": GPIOActionRefreshSDCard,
}

const (
	// gpioDefaultsMigrationRefreshSDCard 是「给网络键短按补上刷新SD卡」这一次性
	// 迁移的标记名。只对确实还空着的档位生效，且只做一次。
	gpioDefaultsMigrationRefreshSDCard = "network-short-sdcard"
	// gpioDefaultsMigrationLegacyHold3S 是上一版的迁移标记：那一版把刷新SD卡写在
	// 网络键长按 3 秒上。现在的推荐位置是短按，升级时要把这条由迁移写进去的
	// 绑定收回来，免得同一个动作同时挂在两档上让人以为按错了。
	gpioDefaultsMigrationLegacyHold3S = "network-hold3s-sdcard"
)

func DefaultGPIOConfig() GPIOConfig {
	config := GPIOConfig{Version: gpioConfigVersion, Enabled: true}
	config.Migrations = []string{gpioDefaultsMigrationRefreshSDCard}
	config.StorageNotify = gpioBoolPointer(true)
	for _, spec := range gpioButtonSpecs {
		short := defaultGPIOShortActions[spec.ID]
		if short == "" {
			short = GPIOActionNone
		}
		config.Buttons = append(config.Buttons, GPIOButtonConfig{
			ID: spec.ID,
			Actions: GPIOActions{
				Short: short, Hold3S: GPIOActionNone,
				Hold9S: GPIOActionNone, Hold15S: GPIOActionNone,
			},
		})
	}
	return config
}

// applyGPIORecommendedDefaults 给升级上来的老配置补上推荐默认值，只动「用户
// 从来没配过」的档位，并用 Migrations 保证每个迁移只应用一次：用户把动作改回
// 「无动作」是明确选择，不该被下次启动再填回去。
func applyGPIORecommendedDefaults(config *GPIOConfig) bool {
	if !config.Enabled {
		// 按键映射整体关着，说明用户不打算用它，不要擅自写入动作。
		return false
	}
	changed := relocateLegacyRefreshSDCard(config)
	if gpioContainsString(config.Migrations, gpioDefaultsMigrationRefreshSDCard) {
		return changed
	}
	config.Migrations = append(config.Migrations, gpioDefaultsMigrationRefreshSDCard)
	for index := range config.Buttons {
		button := &config.Buttons[index]
		if !gpioActionIsEmpty(button.Actions.Short) {
			continue
		}
		// 只要用户配过任意一档长按，就认为他在管理按键，不再插手。
		if !gpioActionIsEmpty(button.Actions.Hold3S) || !gpioActionIsEmpty(button.Actions.Hold9S) ||
			!gpioActionIsEmpty(button.Actions.Hold15S) {
			continue
		}
		if recommended := defaultGPIOShortActions[button.ID]; recommended != "" {
			button.Actions.Short = recommended
			changed = true
		}
	}
	return changed
}

// relocateLegacyRefreshSDCard 处理上一版留下的「网络键长按 3 秒＝刷新SD卡」。
// 只有在本机确实带着旧迁移标记时才动手，而且只搬动仍然等于刷新SD卡的那一档
// ——用户自己改成别的动作之后就不再认作我们的默认值。短按档位只在「空着」或
// 「还挂着我方旧默认值（重新应用插件）」时才被占用，用户自己选的动作不受影响。
//
// 两个约束，缺一个都会反复改写用户的绑定：
//   - 处理完就把旧标记摘掉：留着的话每次 normalizeConfig 都会再跑一遍，用户
//     升级后自己把某个键的长按 3 秒设成刷新SD卡，保存时又被我们清掉；
//   - 只动 network 键：旧版的默认值只写在网络键上，别的键出现刷新SD卡一定是
//     用户自己设的，不能碰。
func relocateLegacyRefreshSDCard(config *GPIOConfig) bool {
	if !gpioContainsString(config.Migrations, gpioDefaultsMigrationLegacyHold3S) {
		return false
	}
	kept := make([]string, 0, len(config.Migrations))
	for _, value := range config.Migrations {
		if value != gpioDefaultsMigrationLegacyHold3S {
			kept = append(kept, value)
		}
	}
	config.Migrations = kept
	changed := true
	for index := range config.Buttons {
		button := &config.Buttons[index]
		if button.ID != "network" || button.Actions.Hold3S != GPIOActionRefreshSDCard {
			continue
		}
		button.Actions.Hold3S = GPIOActionNone
		changed = true
		if gpioActionIsEmpty(button.Actions.Short) || button.Actions.Short == GPIOActionReapplyPlugin {
			button.Actions.Short = GPIOActionRefreshSDCard
		}
	}
	return changed
}

func gpioContainsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// gpioConfigLooksUnconfigured 判断一份 GPIO 配置是否仍是「从未配置」的旧默认值：
// 关闭状态且四个档位全部为 none。历史版本的默认值正是这个样子，升级时可以直接
// 换成新的开箱即用默认值；只要用户动过任何一个档位就不再认为未配置。
//
// 除了档位，还要看用户可能留下的其他痕迹：迁移标记、自定义脚本、存储插入提示音
// 开关。它们都说明「用户来过并做过选择」。漏判的后果很重——SaveGPIOConfig 会把
// 这种配置整份换成 DefaultGPIOConfig()，于是用户主动关掉的按键映射被重新打开、
// 保存过的脚本被删掉、关掉的提示音被重开。
func gpioConfigLooksUnconfigured(config GPIOConfig) bool {
	if config.Enabled || len(config.Migrations) > 0 || len(config.Scripts) > 0 ||
		config.StorageNotify != nil ||
		len(config.Backups) > 0 || len(config.Autos) > 0 || len(config.AutoVolumes) > 0 {
		return false
	}
	for _, button := range config.Buttons {
		actions := button.Actions
		if !gpioActionIsEmpty(actions.Short) || !gpioActionIsEmpty(actions.Hold3S) ||
			!gpioActionIsEmpty(actions.Hold9S) || !gpioActionIsEmpty(actions.Hold15S) {
			return false
		}
	}
	return len(config.Buttons) == 0 || len(config.Buttons) == len(gpioButtonSpecs)
}

// preserveGPIOUserData 把「用户自己攒下来、不属于按键档位」的内容原样搬到新配置
// 上：脚本、备份任务、自动备份规则、打标卷记录，以及存储插入提示音的开关。
// 迁移只负责补默认值，不该顺手把这些删掉。
func preserveGPIOUserData(next, previous GPIOConfig) GPIOConfig {
	next.Scripts = append([]GPIOScript(nil), previous.Scripts...)
	next.Backups = append([]GPIOBackupJob(nil), previous.Backups...)
	next.Autos = append([]GPIOAutoBackup(nil), previous.Autos...)
	next.AutoVolumes = append([]AutoBackupVolumeRecord(nil), previous.AutoVolumes...)
	if previous.StorageNotify != nil {
		notify := *previous.StorageNotify
		next.StorageNotify = &notify
	}
	return next
}

// gpioActionIsEmpty 把缺字段的空字符串和显式的 "none" 都视为「无动作」。
func gpioActionIsEmpty(action string) bool {
	return action == "" || action == GPIOActionNone
}

func validateGPIOConfig(config GPIOConfig) error {
	if config.Version != gpioConfigVersion {
		return fmt.Errorf("gpio version must be %d", gpioConfigVersion)
	}
	if len(config.Buttons) != len(gpioButtonSpecs) {
		return fmt.Errorf("gpio buttons must contain exactly %d entries", len(gpioButtonSpecs))
	}
	if len(config.Scripts) > gpioScriptMaxCount {
		return fmt.Errorf("gpio scripts must contain no more than %d entries", gpioScriptMaxCount)
	}
	scripts := make(map[string]GPIOScript, len(config.Scripts))
	names := make(map[string]bool, len(config.Scripts))
	for _, script := range config.Scripts {
		if err := validateGPIOScript(script); err != nil {
			return err
		}
		if _, exists := scripts[script.ID]; exists {
			return fmt.Errorf("duplicate gpio script id %q", script.ID)
		}
		nameKey := strings.ToLower(strings.TrimSpace(script.Name))
		if names[nameKey] {
			return fmt.Errorf("duplicate gpio script name %q", strings.TrimSpace(script.Name))
		}
		scripts[script.ID] = script
		names[nameKey] = true
	}
	if err := validateGPIOBackups(config.Backups); err != nil {
		return err
	}
	if err := validateGPIOAutoBackups(config.Autos); err != nil {
		return err
	}
	if err := validateGPIOAutoVolumeRecords(config.AutoVolumes); err != nil {
		return err
	}
	backups := make(map[string]bool, len(config.Backups))
	for _, job := range config.Backups {
		backups[job.ID] = true
	}
	autos := make(map[string]bool, len(config.Autos))
	for _, rule := range config.Autos {
		autos[rule.ID] = true
	}
	wanted := make(map[string]bool, len(gpioButtonSpecs))
	for _, spec := range gpioButtonSpecs {
		wanted[spec.ID] = true
	}
	seen := make(map[string]bool, len(config.Buttons))
	for _, button := range config.Buttons {
		if !wanted[button.ID] {
			return fmt.Errorf("unknown gpio button %q", button.ID)
		}
		if seen[button.ID] {
			return fmt.Errorf("duplicate gpio button %q", button.ID)
		}
		seen[button.ID] = true
		// 短按（short）与三个长按档位一样参与校验和派发。早期版本曾把短按
		// 静默丢弃（既不校验也不执行），导致「按一下没反应」，现已恢复。
		for stage, action := range map[string]string{
			"short":   button.Actions.Short,
			"hold_3s": button.Actions.Hold3S,
			"hold_9s": button.Actions.Hold9S, "hold_15s": button.Actions.Hold15S,
		} {
			if !allowedGPIOActions[action] {
				scriptID, isScript := gpioScriptIDFromAction(action)
				if isScript {
					if _, exists := scripts[scriptID]; exists {
						continue
					}
					return fmt.Errorf("gpio button %s stage %s references missing script %q", button.ID, stage, scriptID)
				}
				backupID, isBackup := gpioBackupIDFromAction(action)
				if isBackup {
					if backups[backupID] {
						continue
					}
					return fmt.Errorf("gpio button %s stage %s references missing backup job %q", button.ID, stage, backupID)
				}
				autoID, isAuto := gpioAutoBackupIDFromAction(action)
				if isAuto {
					if autos[autoID] {
						continue
					}
					return fmt.Errorf("gpio button %s stage %s references missing auto backup rule %q", button.ID, stage, autoID)
				}
				return fmt.Errorf("gpio button %s stage %s has unsupported action %q", button.ID, stage, action)
			}
		}
	}
	return nil
}

func validateGPIOScript(script GPIOScript) error {
	if !gpioScriptIDPattern.MatchString(script.ID) {
		return fmt.Errorf("gpio script id %q must use 1-64 ASCII letters, numbers, dot, underscore or hyphen", script.ID)
	}
	name := strings.TrimSpace(script.Name)
	if name == "" || utf8.RuneCountInString(name) > gpioScriptMaxName {
		return fmt.Errorf("gpio script %q name must contain 1-%d characters", script.ID, gpioScriptMaxName)
	}
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return fmt.Errorf("gpio script %q name contains control characters", script.ID)
	}
	if strings.TrimSpace(script.Body) == "" {
		return fmt.Errorf("gpio script %q body must not be empty", script.ID)
	}
	if len(script.Body) > gpioScriptMaxBody {
		return fmt.Errorf("gpio script %q body exceeds %d bytes", script.ID, gpioScriptMaxBody)
	}
	if strings.ContainsRune(script.Body, '\x00') {
		return fmt.Errorf("gpio script %q body contains a NUL byte", script.ID)
	}
	return nil
}

func gpioScriptIDFromAction(action string) (string, bool) {
	if !strings.HasPrefix(action, GPIOActionScriptPrefix) {
		return "", false
	}
	return strings.TrimPrefix(action, GPIOActionScriptPrefix), true
}

func gpioScriptForAction(config GPIOConfig, action string) *GPIOScript {
	id, ok := gpioScriptIDFromAction(action)
	if !ok {
		return nil
	}
	for _, script := range config.Scripts {
		if script.ID == id {
			copy := script
			return &copy
		}
	}
	return nil
}

func (m *Manager) GPIOPortPath() string {
	return m.rooted("/dev/port")
}

func (m *Manager) ResetGPIO(enabled, available bool, cause error) {
	m.gpioMu.Lock()
	defer m.gpioMu.Unlock()
	m.gpioRuntime.available = available
	m.gpioRuntime.buttons = nil
	if cause != nil {
		m.gpioRuntime.lastError = cause.Error()
	} else if !enabled {
		m.gpioRuntime.lastError = ""
	}
}

func (m *Manager) PollGPIO(config GPIOConfig, reader io.ReaderAt, now time.Time) ([]GPIOEvent, error) {
	if err := validateGPIOConfig(config); err != nil {
		return nil, err
	}
	if !config.Enabled {
		m.ResetGPIO(false, false, nil)
		return nil, nil
	}
	if reader == nil {
		err := errors.New("gpio /dev/port reader is unavailable")
		m.ResetGPIO(true, false, err)
		return nil, err
	}

	m.gpioMu.Lock()
	defer m.gpioMu.Unlock()
	if m.gpioRuntime.buttons == nil {
		m.gpioRuntime.buttons = make(map[string]*gpioButtonRuntime, len(gpioButtonSpecs))
	}
	m.gpioRuntime.available = true
	m.gpioRuntime.lastError = ""
	actions := gpioActionMap(config)
	var events []GPIOEvent
	for _, spec := range gpioButtonSpecs {
		var value [1]byte
		if _, err := reader.ReadAt(value[:], spec.Port); err != nil {
			m.gpioRuntime.available = false
			m.gpioRuntime.lastError = fmt.Sprintf("read GPIO port 0x%X: %v", spec.Port, err)
			return events, errors.New(m.gpioRuntime.lastError)
		}
		high := value[0]&(1<<spec.Bit) != 0
		state := m.gpioRuntime.buttons[spec.ID]
		if state == nil {
			state = &gpioButtonRuntime{}
			m.gpioRuntime.buttons[spec.ID] = state
		}
		if !state.initialized {
			state.initialized = true
			state.baselineHigh = high
			state.rawChangedAt = now
			continue
		}
		rawPressed := high != state.baselineHigh
		if rawPressed != state.rawPressed {
			state.rawPressed = rawPressed
			state.rawChangedAt = now
			continue
		}
		if now.Sub(state.rawChangedAt) < gpioDebounce {
			continue
		}
		if rawPressed != state.pressed {
			state.pressed = rawPressed
			if rawPressed {
				state.pressedAt = now
				state.feedbackMask = 0
				continue
			}
		}
		if state.pressed {
			for _, confirmation := range gpioFeedbackForDuration(now.Sub(state.pressedAt), state.feedbackMask) {
				state.feedbackMask |= confirmation.mask
				events = append(events, GPIOEvent{
					ButtonID: spec.ID,
					Name:     spec.Name,
					Duration: now.Sub(state.pressedAt),
					Stage:    confirmation.stage,
					Kind:     GPIOEventFeedback,
					Feedback: &confirmation.pattern,
				})
				m.gpioRuntime.lastEvent = fmt.Sprintf("%s %s（%.1f 秒）→ 已确认", spec.Name, confirmation.stage, now.Sub(state.pressedAt).Seconds())
			}
			continue
		}
		if state.pressedAt.IsZero() {
			continue
		}
		duration := now.Sub(state.pressedAt)
		stage, action := gpioActionForDuration(actions[spec.ID], duration)
		state.pressedAt = time.Time{}
		state.feedbackMask = 0
		if stage == "" {
			continue
		}
		state.lastAction = action
		if stage == gpioStageShortPress {
			// 短按没有按住期的阈值确认，在松开时补一次与长按一致的反馈，
			// 保证「每次按键操作都闪 3 下 + 滴滴滴 3 声」。
			pattern := gpioStandardFeedback()
			events = append(events, GPIOEvent{
				ButtonID: spec.ID,
				Name:     spec.Name,
				Duration: duration,
				Stage:    stage,
				Kind:     GPIOEventFeedback,
				Feedback: &pattern,
			})
		}
		event := GPIOEvent{ButtonID: spec.ID, Name: spec.Name, Duration: duration, Stage: stage, Action: action, Kind: GPIOEventAction}
		event.Script = gpioScriptForAction(config, action)
		event.Backup = gpioBackupForAction(config, action)
		event.Auto = gpioAutoBackupForAction(config, action)
		m.gpioRuntime.lastEvent = fmt.Sprintf("%s %s（%.1f 秒）→ %s", spec.Name, stage, duration.Seconds(), gpioActionDisplay(config, action))
		events = append(events, event)
	}
	return events, nil
}

func gpioActionMap(config GPIOConfig) map[string]GPIOActions {
	result := make(map[string]GPIOActions, len(config.Buttons))
	for _, button := range config.Buttons {
		result[button.ID] = button.Actions
	}
	return result
}

func gpioActionForDuration(actions GPIOActions, duration time.Duration) (string, string) {
	switch {
	case duration >= 15*time.Second:
		return "长按 15 秒", actions.Hold15S
	case duration >= 9*time.Second:
		return "长按 9 秒", actions.Hold9S
	case duration >= 3*time.Second:
		return "长按 3 秒", actions.Hold3S
	default:
		// 短按：按下后经过去抖、还没到 3 秒就松开
		return gpioStageShortPress, actions.Short
	}
}

type gpioHoldConfirmation struct {
	mask    uint8
	stage   string
	pattern GPIOFeedbackPattern
}

// 所有档位统一使用同一套确认反馈（闪 3 下 + 滴滴滴 3 声），与 RR 官方插件
// tad6s4n10g 的 btnLed 3 / btnBeep 3 表现一致。
const (
	gpioFeedbackTones   = 3
	gpioFeedbackFlashes = 3
	gpioStageShortPress = "短按"
)

func gpioStandardFeedback() GPIOFeedbackPattern {
	return GPIOFeedbackPattern{Tones: gpioFeedbackTones, Flashes: gpioFeedbackFlashes}
}

func gpioFeedbackForDuration(duration time.Duration, confirmed uint8) []gpioHoldConfirmation {
	thresholds := []gpioHoldConfirmation{
		{mask: 1 << 0, stage: "长按 3 秒", pattern: gpioStandardFeedback()},
		{mask: 1 << 1, stage: "长按 9 秒", pattern: gpioStandardFeedback()},
		{mask: 1 << 2, stage: "长按 15 秒", pattern: gpioStandardFeedback()},
	}
	minimums := []time.Duration{3 * time.Second, 9 * time.Second, 15 * time.Second}
	var result []gpioHoldConfirmation
	for i, threshold := range thresholds {
		if duration >= minimums[i] && confirmed&threshold.mask == 0 {
			result = append(result, threshold)
		}
	}
	return result
}

func (m *Manager) ExecuteGPIOAction(event GPIOEvent) error {
	_, err := m.ExecuteGPIOActionContext(context.Background(), event)
	return err
}

func (m *Manager) ExecuteGPIOActionContext(ctx context.Context, event GPIOEvent) (string, error) {
	var err error
	var output string
	switch event.Action {
	case GPIOActionNone, GPIOActionLog:
	case GPIOActionRefreshStorage:
		m.RefreshStorage(false)
	case GPIOActionSMARTCheck:
		m.RefreshStorage(true)
	case GPIOActionReapplyPlugin:
		err = m.ApplyCurrent()
	case GPIOActionRefreshSDCard:
		// 重新识别读卡器要等内核枚举，好几秒；按键轮询循环不能被它堵住，
		// 所以放到后台跑，结束时用蜂鸣 + 闪灯回执。
		if startErr := m.StartRefreshSDCard(); startErr != nil {
			err = startErr
		} else {
			output = "正在刷新SD卡…"
		}
	default:
		if _, isScript := gpioScriptIDFromAction(event.Action); isScript && event.Script != nil {
			output, err = executeGPIOScript(ctx, event)
		} else if job := event.Backup; job != nil {
			// 备份在后台跑：按键应该立刻有响应，长任务不能把轮询循环堵住。
			// 结束时会用蜂鸣 + 闪灯给出回执。
			err = m.StartBackupJob(*job, false)
			if err == nil {
				output = fmt.Sprintf("已开始备份：%s", job.Name)
			}
		} else if rule := event.Auto; rule != nil {
			// 自动备份同样在后台跑：扫描打标卷可能要处理多块盘。
			err = m.StartAutoBackupJob(*rule, false)
			if err == nil {
				output = fmt.Sprintf("已开始自动备份：%s", rule.Name)
			}
		} else if _, isBackup := gpioBackupIDFromAction(event.Action); isBackup {
			err = fmt.Errorf("备份任务不存在或已被删除：%s", event.Action)
		} else if _, isAuto := gpioAutoBackupIDFromAction(event.Action); isAuto {
			err = fmt.Errorf("自动备份规则不存在或已被删除：%s", event.Action)
		} else {
			err = fmt.Errorf("unsupported gpio action %q", event.Action)
		}
	}
	m.gpioMu.Lock()
	if err != nil {
		m.gpioRuntime.lastError = err.Error()
	} else {
		m.gpioRuntime.lastError = ""
	}
	m.gpioMu.Unlock()
	return output, err
}

func (m *Manager) SetGPIOActionError(err error) {
	m.gpioMu.Lock()
	defer m.gpioMu.Unlock()
	if err == nil {
		m.gpioRuntime.lastError = ""
		return
	}
	m.gpioRuntime.lastError = err.Error()
}

type cappedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (buffer *cappedBuffer) Write(data []byte) (int, error) {
	written := len(data)
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 {
		buffer.truncated = true
		return written, nil
	}
	if len(data) > remaining {
		_, _ = buffer.Buffer.Write(data[:remaining])
		buffer.truncated = true
		return written, nil
	}
	_, _ = buffer.Buffer.Write(data)
	return written, nil
}

func executeGPIOScript(parent context.Context, event GPIOEvent) (string, error) {
	if event.Script == nil {
		return "", errors.New("gpio script is missing")
	}
	ctx, cancel := context.WithTimeout(parent, gpioScriptTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/bash", "--noprofile", "--norc")
	command.Dir = "/"
	command.Env = append(os.Environ(),
		"TAD_GPIO_BUTTON_ID="+event.ButtonID,
		"TAD_GPIO_STAGE="+event.Stage,
		"TAD_GPIO_DURATION_MS="+strconv.FormatInt(event.Duration.Milliseconds(), 10),
		"TAD_GPIO_SCRIPT_NAME="+event.Script.Name,
	)
	body := strings.ReplaceAll(event.Script.Body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	command.Stdin = strings.NewReader(body)
	output := &cappedBuffer{limit: gpioScriptOutputMax}
	command.Stdout = output
	command.Stderr = output
	err := command.Run()
	text := strings.TrimSpace(output.String())
	if output.truncated {
		text += "\n[输出已截断]"
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return text, fmt.Errorf("gpio script %q timed out after %s", event.Script.Name, gpioScriptTimeout)
	}
	if err != nil {
		return text, fmt.Errorf("gpio script %q failed: %w", event.Script.Name, err)
	}
	return text, nil
}

func (m *Manager) GPIOStatus(config GPIOConfig) GPIOStatus {
	m.gpioMu.Lock()
	defer m.gpioMu.Unlock()
	status := GPIOStatus{
		Available: m.gpioRuntime.available,
		Enabled:   config.Enabled,
		LastEvent: m.gpioRuntime.lastEvent,
		LastError: m.gpioRuntime.lastError,
	}
	if !config.Enabled {
		if _, err := os.Stat(m.GPIOPortPath()); err == nil {
			status.Available = true
		}
	}
	now := time.Now()
	for _, spec := range gpioButtonSpecs {
		button := GPIOButtonStatus{
			ID: spec.ID, Name: spec.Name, Port: fmt.Sprintf("0x%X", spec.Port), Bit: spec.Bit,
		}
		if runtime := m.gpioRuntime.buttons[spec.ID]; runtime != nil {
			button.Pressed = runtime.pressed
			button.LastAction = runtime.lastAction
			if runtime.pressed && !runtime.pressedAt.IsZero() {
				button.HeldMS = now.Sub(runtime.pressedAt).Milliseconds()
			}
		}
		status.Buttons = append(status.Buttons, button)
	}
	sort.Slice(status.Buttons, func(i, j int) bool { return status.Buttons[i].ID < status.Buttons[j].ID })
	return status
}

func GPIOActionDisplay(action string) string {
	switch action {
	case GPIOActionNone:
		return "无动作"
	case GPIOActionLog:
		return "仅记录日志"
	case GPIOActionRefreshStorage:
		return "刷新硬盘仓位"
	case GPIOActionSMARTCheck:
		return "刷新仓位并检查 SMART"
	case GPIOActionReapplyPlugin:
		return "重新应用插件配置"
	default:
		if id, ok := gpioScriptIDFromAction(action); ok {
			return "脚本 " + id
		}
		if id, ok := gpioBackupIDFromAction(action); ok {
			return "备份 " + id
		}
		if id, ok := gpioAutoBackupIDFromAction(action); ok {
			return "自动备份 " + id
		}
		return strings.TrimSpace(action)
	}
}

func gpioActionDisplay(config GPIOConfig, action string) string {
	if script := gpioScriptForAction(config, action); script != nil {
		return "脚本：" + script.Name
	}
	if job := gpioBackupForAction(config, action); job != nil {
		return "备份：" + job.Name
	}
	if rule := gpioAutoBackupForAction(config, action); rule != nil {
		return "自动备份：" + rule.Name
	}
	return GPIOActionDisplay(action)
}
