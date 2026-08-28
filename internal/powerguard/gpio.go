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
	Version int                `json:"version"`
	Enabled bool               `json:"enabled"`
	Scripts []GPIOScript       `json:"scripts,omitempty"`
	Buttons []GPIOButtonConfig `json:"buttons"`
}

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
	GPIOActionReapplyPlugin: true,
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

func DefaultGPIOConfig() GPIOConfig {
	config := GPIOConfig{Version: gpioConfigVersion, Enabled: false}
	for _, spec := range gpioButtonSpecs {
		config.Buttons = append(config.Buttons, GPIOButtonConfig{
			ID: spec.ID,
			Actions: GPIOActions{
				Short: GPIOActionNone, Hold3S: GPIOActionNone,
				Hold9S: GPIOActionNone, Hold15S: GPIOActionNone,
			},
		})
	}
	return config
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
		// Short is intentionally retained for decoding configurations written by
		// earlier releases, but is never validated or executed.
		for stage, action := range map[string]string{
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
		event := GPIOEvent{ButtonID: spec.ID, Name: spec.Name, Duration: duration, Stage: stage, Action: action, Kind: GPIOEventAction}
		event.Script = gpioScriptForAction(config, action)
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
		return "", GPIOActionNone
	}
}

type gpioHoldConfirmation struct {
	mask    uint8
	stage   string
	pattern GPIOFeedbackPattern
}

func gpioFeedbackForDuration(duration time.Duration, confirmed uint8) []gpioHoldConfirmation {
	thresholds := []gpioHoldConfirmation{
		{mask: 1 << 0, stage: "长按 3 秒", pattern: GPIOFeedbackPattern{Tones: 1, Flashes: 1}},
		{mask: 1 << 1, stage: "长按 9 秒", pattern: GPIOFeedbackPattern{Tones: 2, Flashes: 2}},
		{mask: 1 << 2, stage: "长按 15 秒", pattern: GPIOFeedbackPattern{Tones: 3, Flashes: 3}},
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
	default:
		if _, isScript := gpioScriptIDFromAction(event.Action); isScript && event.Script != nil {
			output, err = executeGPIOScript(ctx, event)
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
		return strings.TrimSpace(action)
	}
}

func gpioActionDisplay(config GPIOConfig, action string) string {
	if script := gpioScriptForAction(config, action); script != nil {
		return "脚本：" + script.Name
	}
	return GPIOActionDisplay(action)
}
