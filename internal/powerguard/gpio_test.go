package powerguard

import (
	"context"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"
)

type fakeGPIOPort struct {
	values []byte
}

func (port *fakeGPIOPort) ReadAt(buffer []byte, offset int64) (int, error) {
	if offset < 0 || int(offset) >= len(port.values) {
		return 0, io.EOF
	}
	buffer[0] = port.values[offset]
	return 1, nil
}

func (port *fakeGPIOPort) set(spec gpioButtonSpec, high bool) {
	if high {
		port.values[spec.Port] |= 1 << spec.Bit
	} else {
		port.values[spec.Port] &^= 1 << spec.Bit
	}
}

func TestDefaultGPIOConfigIsEnabledAndReadyToUse(t *testing.T) {
	config := DefaultGPIOConfig()
	if !config.Enabled || config.Version != gpioConfigVersion || len(config.Buttons) != 3 {
		t.Fatalf("unexpected defaults: %+v", config)
	}
	if err := validateGPIOConfig(config); err != nil {
		t.Fatal(err)
	}
	wantShort := map[string]string{
		"copy":       GPIOActionNone,
		"network":    GPIOActionRefreshSDCard,
		"rear_reset": GPIOActionNone,
	}
	// 开箱即用只保留最常用的一下：网络键短按＝刷新SD卡（换卡后读卡器认不出卡时
	// 随手按一下）。长按三档与其余按键一律留空，免得「按一下不知道会做什么」。
	for _, button := range config.Buttons {
		if button.Actions.Short != wantShort[button.ID] {
			t.Fatalf("default short action is unexpected: %+v", button)
		}
		if button.Actions.Hold3S != GPIOActionNone {
			t.Fatalf("default hold_3s action should stay empty: %+v", button)
		}
		if button.Actions.Hold9S != GPIOActionNone || button.Actions.Hold15S != GPIOActionNone {
			t.Fatalf("default long hold actions should stay empty: %+v", button)
		}
	}
	if config.StorageNotify == nil || !*config.StorageNotify {
		t.Fatal("存储设备插入提示音应当默认开启")
	}
}

func TestNormalizeConfigPreservesShortActions(t *testing.T) {
	config := DefaultConfig(profiles[1])
	config.GPIO.Buttons[0].Actions.Short = GPIOActionLog
	normalizeConfig(&config)
	if config.GPIO.Buttons[0].Actions.Short != GPIOActionLog {
		t.Fatalf("short action was cleared: %+v", config.GPIO.Buttons[0])
	}
}

func TestNormalizeConfigUpgradesUntouchedGPIOConfig(t *testing.T) {
	config := DefaultConfig(profiles[1])
	// 复刻旧版本的默认值：按键映射关闭且四个档位全为无动作。旧版本的配置里既没有
	// 迁移标记也没有提示音字段——「有没有迁移标记」正是区分「从未配置过」与
	// 「用户主动关掉」的依据，所以这里必须一起清掉。
	config.GPIO.Enabled = false
	config.GPIO.Migrations = nil
	config.GPIO.StorageNotify = nil
	for index := range config.GPIO.Buttons {
		config.GPIO.Buttons[index].Actions = GPIOActions{}
	}
	if !normalizeConfig(&config) {
		t.Fatal("untouched gpio config did not trigger migration")
	}
	if !config.GPIO.Enabled {
		t.Fatalf("untouched gpio config was not upgraded: %+v", config.GPIO)
	}
	network, ok := gpioTestButton(config.GPIO, "network")
	if !ok || network.Actions.Short != GPIOActionRefreshSDCard {
		t.Fatalf("网络键短按应被补上刷新SD卡: %+v", network)
	}
	copyButton, _ := gpioTestButton(config.GPIO, "copy")
	if !gpioActionIsEmpty(copyButton.Actions.Short) {
		t.Fatalf("其余按键应保持无动作: %+v", copyButton)
	}
}

func TestNormalizeConfigKeepsDeliberatelyDisabledGPIO(t *testing.T) {
	config := DefaultConfig(profiles[1])
	config.GPIO.Enabled = false
	config.GPIO.Buttons[0].Actions.Hold15S = GPIOActionLog
	normalizeConfig(&config)
	if config.GPIO.Enabled || config.GPIO.Buttons[0].Actions.Hold15S != GPIOActionLog {
		t.Fatalf("deliberate gpio config was overwritten: %+v", config.GPIO)
	}
}

// CodeRabbit 第二轮：Version==0 但已经配置过的旧配置不能被整份换成默认值——
// 只凭「没有版本号」判断，会把用户的按键绑定、脚本、提示音开关一起换掉。
// 正确做法是保留全部用户字段、只把版本号补上（validateGPIOConfig 不认 version 0）。
func TestNormalizeConfigKeepsConfiguredGPIOWithoutVersion(t *testing.T) {
	config := DefaultConfig(profiles[1])
	config.GPIO.Version = 0
	config.GPIO.Enabled = false
	config.GPIO.Migrations = nil
	config.GPIO.StorageNotify = nil
	config.GPIO.Scripts = []GPIOScript{{ID: "s1", Name: "自检", Body: "echo ok"}}
	config.GPIO.Buttons[0].Actions.Hold15S = GPIOActionLog
	if !normalizeConfig(&config) {
		t.Fatal("version 0 配置补版本号时应当算一次变更")
	}
	if config.GPIO.Version != gpioConfigVersion {
		t.Fatalf("version 0 应迁移到 %d: %+v", gpioConfigVersion, config.GPIO)
	}
	if config.GPIO.Enabled {
		t.Fatalf("用户主动关掉的按键映射不应被重置: %+v", config.GPIO)
	}
	if len(config.GPIO.Scripts) != 1 || config.GPIO.Scripts[0].Body != "echo ok" {
		t.Fatalf("用户保存的脚本不应被删掉: %+v", config.GPIO.Scripts)
	}
	if config.GPIO.Buttons[0].Actions.Hold15S != GPIOActionLog {
		t.Fatalf("用户配置的档位不应被改写: %+v", config.GPIO.Buttons[0])
	}
	if err := validateGPIOConfig(config.GPIO); err != nil {
		t.Fatalf("只补版本号后的配置应通过校验: %v", err)
	}
}

func TestGPIOValidationRejectsUnknownAction(t *testing.T) {
	config := DefaultGPIOConfig()
	config.Buttons[0].Actions.Hold3S = "shell_command"
	if err := validateGPIOConfig(config); err == nil {
		t.Fatal("unknown GPIO action was accepted")
	}
}

func TestGPIOValidationRejectsUnknownShortAction(t *testing.T) {
	config := DefaultGPIOConfig()
	config.Buttons[0].Actions.Short = "legacy_unsupported_action"
	if err := validateGPIOConfig(config); err == nil {
		t.Fatal("unknown short action was accepted")
	}
}

func TestGPIOActionForDurationDispatchesShortPress(t *testing.T) {
	actions := GPIOActions{Short: GPIOActionLog, Hold3S: "3", Hold9S: "9", Hold15S: "15"}
	stage, action := gpioActionForDuration(actions, 1200*time.Millisecond)
	if stage != "短按" || action != GPIOActionLog {
		t.Fatalf("short press dispatch = %q/%q, want 短按/%q", stage, action, GPIOActionLog)
	}
}

func TestGPIOValidationAcceptsScriptBinding(t *testing.T) {
	config := DefaultGPIOConfig()
	config.Scripts = []GPIOScript{{ID: "backup-1", Name: "备份脚本", Body: "printf 'ok'\n"}}
	config.Buttons[0].Actions.Hold3S = GPIOActionScriptPrefix + "backup-1"
	if err := validateGPIOConfig(config); err != nil {
		t.Fatal(err)
	}
}

func TestGPIOValidationRejectsInvalidScripts(t *testing.T) {
	valid := GPIOScript{ID: "script-1", Name: "测试脚本", Body: "true\n"}
	for _, test := range []struct {
		name   string
		mutate func(*GPIOConfig)
	}{
		{"path id", func(config *GPIOConfig) { config.Scripts[0].ID = "../bad" }},
		{"empty name", func(config *GPIOConfig) { config.Scripts[0].Name = "  " }},
		{"empty body", func(config *GPIOConfig) { config.Scripts[0].Body = "\n" }},
		{"missing binding", func(config *GPIOConfig) { config.Buttons[0].Actions.Hold3S = GPIOActionScriptPrefix + "missing" }},
		{"duplicate id", func(config *GPIOConfig) { config.Scripts = append(config.Scripts, config.Scripts[0]) }},
		{"duplicate name", func(config *GPIOConfig) {
			config.Scripts = append(config.Scripts, GPIOScript{ID: "script-2", Name: " 测试脚本 ", Body: "true\n"})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := DefaultGPIOConfig()
			config.Scripts = []GPIOScript{valid}
			test.mutate(&config)
			if err := validateGPIOConfig(config); err == nil {
				t.Fatal("invalid GPIO script configuration was accepted")
			}
		})
	}
}

func TestGPIOPollEmitsShortPressFeedbackAndAction(t *testing.T) {
	config := DefaultGPIOConfig()
	config.Enabled = true
	config.Buttons[0].Actions.Short = GPIOActionLog
	port := &fakeGPIOPort{values: make([]byte, 0xA05)}
	for _, spec := range gpioButtonSpecs {
		port.set(spec, true)
	}
	manager := &Manager{}
	start := time.Unix(100, 0)
	if events, err := manager.PollGPIO(config, port, start); err != nil || len(events) != 0 {
		t.Fatalf("initial poll: events=%+v err=%v", events, err)
	}

	copyButton := gpioButtonSpecs[0]
	port.set(copyButton, false)
	_, _ = manager.PollGPIO(config, port, start.Add(10*time.Millisecond))
	_, _ = manager.PollGPIO(config, port, start.Add(120*time.Millisecond))
	port.set(copyButton, true)
	_, _ = manager.PollGPIO(config, port, start.Add(time.Second))
	events, err := manager.PollGPIO(config, port, start.Add(1120*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("short press produced %d GPIO events: %+v", len(events), events)
	}
	if !events[0].IsFeedback() || events[0].Stage != gpioStageShortPress ||
		events[0].Feedback.Tones != gpioFeedbackTones || events[0].Feedback.Flashes != gpioFeedbackFlashes {
		t.Fatalf("unexpected short press feedback: %+v", events[0])
	}
	if events[1].ButtonID != "copy" || events[1].Stage != gpioStageShortPress || events[1].Action != GPIOActionLog || events[1].Kind != GPIOEventAction {
		t.Fatalf("unexpected short press action event: %+v", events[1])
	}
}

func TestGPIOActionThresholds(t *testing.T) {
	actions := GPIOActions{Short: "s", Hold3S: "3", Hold9S: "9", Hold15S: "15"}
	for _, test := range []struct {
		duration time.Duration
		want     string
	}{
		{0, "s"}, {2 * time.Second, "s"}, {3 * time.Second, "3"},
		{9 * time.Second, "9"}, {15 * time.Second, "15"},
	} {
		_, got := gpioActionForDuration(actions, test.duration)
		if got != test.want {
			t.Fatalf("duration %s got %q, want %q", test.duration, got, test.want)
		}
	}
}

func TestGPIOPollConfirmsThresholdOnceAndResolvesBoundScriptOnRelease(t *testing.T) {
	config := DefaultGPIOConfig()
	config.Enabled = true
	config.Scripts = []GPIOScript{{ID: "hello", Name: "问候", Body: "printf hello\n"}}
	config.Buttons[0].Actions.Hold3S = GPIOActionScriptPrefix + "hello"
	port := &fakeGPIOPort{values: make([]byte, 0xA05)}
	for _, spec := range gpioButtonSpecs {
		port.set(spec, true)
	}
	manager := &Manager{}
	start := time.Unix(200, 0)
	_, _ = manager.PollGPIO(config, port, start)
	copyButton := gpioButtonSpecs[0]
	port.set(copyButton, false)
	_, _ = manager.PollGPIO(config, port, start.Add(10*time.Millisecond))
	_, _ = manager.PollGPIO(config, port, start.Add(120*time.Millisecond))
	confirmations, err := manager.PollGPIO(config, port, start.Add(3120*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(confirmations) != 1 || !confirmations[0].IsFeedback() || confirmations[0].Stage != "长按 3 秒" || confirmations[0].Feedback.Tones != gpioFeedbackTones {
		t.Fatalf("unexpected GPIO confirmation: %+v", confirmations)
	}
	confirmations, err = manager.PollGPIO(config, port, start.Add(3220*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(confirmations) != 0 {
		t.Fatalf("threshold was confirmed more than once: %+v", confirmations)
	}
	port.set(copyButton, true)
	_, _ = manager.PollGPIO(config, port, start.Add(3230*time.Millisecond))
	events, err := manager.PollGPIO(config, port, start.Add(3340*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != GPIOEventAction || events[0].Script == nil || events[0].Script.ID != "hello" || events[0].Script.Body != "printf hello\n" {
		t.Fatalf("script was not resolved into GPIO event: %+v", events)
	}
}

func TestGPIOFeedbackThresholdsShareStandardPattern(t *testing.T) {
	confirmations := gpioFeedbackForDuration(16*time.Second, 0)
	if len(confirmations) != 3 {
		t.Fatalf("got %d confirmations, want 3", len(confirmations))
	}
	for i, want := range []string{"长按 3 秒", "长按 9 秒", "长按 15 秒"} {
		if confirmations[i].stage != want {
			t.Fatalf("confirmation %d stage = %q, want %q", i, confirmations[i].stage, want)
		}
		if confirmations[i].pattern.Tones != gpioFeedbackTones || confirmations[i].pattern.Flashes != gpioFeedbackFlashes {
			t.Fatalf("confirmation %d pattern = %+v, want %d tones / %d flashes",
				i, confirmations[i].pattern, gpioFeedbackTones, gpioFeedbackFlashes)
		}
	}
	if got := gpioFeedbackForDuration(16*time.Second, 0x7); len(got) != 0 {
		t.Fatalf("already-confirmed thresholds produced feedback: %+v", got)
	}
}

type fakeGPIOTone struct {
	count int
	err   error
}

func (tone *fakeGPIOTone) PlayTones(count int) error {
	tone.count = count
	return tone.err
}

type fakeGPIOLED struct {
	count int
	err   error
}

func (led *fakeGPIOLED) Flash(count int) error {
	led.count = count
	return led.err
}

func TestRunGPIOFeedbackAttemptsBothOutputs(t *testing.T) {
	tone := &fakeGPIOTone{err: io.ErrClosedPipe}
	led := &fakeGPIOLED{}
	err := runGPIOFeedback(GPIOFeedbackPattern{Tones: 2, Flashes: 3}, tone, led)
	if err == nil {
		t.Fatal("tone error was lost")
	}
	if tone.count != 2 || led.count != 3 {
		t.Fatalf("feedback calls = tone %d, led %d", tone.count, led.count)
	}
}

func TestExecuteGPIOScriptUsesBashAndEventEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/bin/bash execution is verified by Linux CI")
	}
	event := GPIOEvent{
		ButtonID: "copy", Stage: "短按", Duration: 1250 * time.Millisecond,
		Action: GPIOActionScriptPrefix + "env-test",
		Script: &GPIOScript{ID: "env-test", Name: "环境测试", Body: `printf '%s|%s|%s|%s' "$TAD_GPIO_BUTTON_ID" "$TAD_GPIO_STAGE" "$TAD_GPIO_DURATION_MS" "$TAD_GPIO_SCRIPT_NAME"`},
	}
	output, err := executeGPIOScript(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "copy|短按|1250|环境测试") {
		t.Fatalf("unexpected script output %q", output)
	}
}

// 用户主动关掉按键映射（四个档位全为无动作）之后保存，必须保持关闭。这种配置以前
// 会被当成「从未配置」，保存时整份换成默认值——映射自己开回来、脚本被删、提示音被重开。
func TestSaveGPIOConfigKeepsUserDisabledMapping(t *testing.T) {
	manager := autoBackupFakeManager(t)
	disabled := gpioTestConfig(false, nil)
	disabled.Migrations = []string{gpioDefaultsMigrationRefreshSDCard}
	disabled.StorageNotify = gpioBoolPointer(false)
	disabled.Scripts = []GPIOScript{{ID: "s1", Name: "自检", Body: "echo ok"}}
	if err := manager.SaveGPIOConfig(disabled); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	config, err := manager.loadConfigLocked()
	if err != nil {
		t.Fatal(err)
	}
	if config.GPIO.Enabled {
		t.Fatal("用户主动关掉的按键映射不该被默认值重新打开")
	}
	if len(config.GPIO.Scripts) != 1 || config.GPIO.Scripts[0].ID != "s1" {
		t.Fatalf("保存过的脚本不该被删: %+v", config.GPIO.Scripts)
	}
	if config.GPIO.StorageNotifyEnabled() {
		t.Fatal("关掉的存储插入提示音不该被重开")
	}
	if network, ok := gpioTestButton(config.GPIO, "network"); !ok || !gpioActionIsEmpty(network.Actions.Short) {
		t.Fatalf("关掉映射后不该被补上默认动作: %+v", network.Actions)
	}
}

// 迁移补默认值时，用户攒下的脚本与提示音开关必须原样带过去。
func TestPreserveGPIOUserDataKeepsScriptsAndNotify(t *testing.T) {
	previous := GPIOConfig{
		Scripts:       []GPIOScript{{ID: "s1", Name: "自检", Body: "echo ok"}},
		StorageNotify: gpioBoolPointer(false),
		Backups: []GPIOBackupJob{{
			ID: "b1", Name: "照片", Mode: BackupModeCopy,
			Source: BackupLocation{Kind: "path", Path: "/vol1/src"},
			Target: BackupLocation{Kind: "path", Path: "/vol1/dst"},
		}},
	}
	merged := preserveGPIOUserData(DefaultGPIOConfig(), previous)
	if len(merged.Scripts) != 1 || merged.Scripts[0].ID != "s1" {
		t.Fatalf("脚本应当被保留: %+v", merged.Scripts)
	}
	if merged.StorageNotifyEnabled() {
		t.Fatal("关掉的提示音应当被保留成关闭")
	}
	if len(merged.Backups) != 1 {
		t.Fatalf("备份任务应当被保留: %+v", merged.Backups)
	}
	// 换新默认值是给「档位」补位，不该把新默认值本身也带丢。
	if !merged.Enabled || merged.Migrations == nil {
		t.Fatalf("新默认值本身应当保留: %+v", merged)
	}
	// 指针不能与原配置共享：改动合并结果不该反向污染来源。
	*merged.StorageNotify = true
	if *previous.StorageNotify {
		t.Fatal("提示音开关应当是独立副本")
	}
}
