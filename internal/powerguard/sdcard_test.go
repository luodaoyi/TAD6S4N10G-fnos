package powerguard

import (
	"context"
	"testing"
)

// 网络键短按应当开箱即用地绑上「刷新SD卡」——换卡后认不出来时随手按一下，
// 不用记「要按住 3 秒」。
func TestDefaultGPIOConfigBindsSDCardRefreshToNetworkShortPress(t *testing.T) {
	if !allowedGPIOActions[GPIOActionRefreshSDCard] {
		t.Fatal("刷新SD卡动作必须在校验白名单里")
	}
	config := DefaultGPIOConfig()
	if !config.Enabled {
		t.Fatal("默认应当启用按键映射")
	}
	button, ok := gpioTestButton(config, "network")
	if !ok {
		t.Fatal("默认配置里应当有网络键")
	}
	if button.Actions.Short != GPIOActionRefreshSDCard {
		t.Fatalf("网络键短按应默认是刷新SD卡，实际 %q", button.Actions.Short)
	}
	if !gpioActionIsEmpty(button.Actions.Hold3S) {
		t.Fatalf("刷新SD卡不该再挂在长按 3 秒上，实际 %q", button.Actions.Hold3S)
	}
	if !gpioContainsString(config.Migrations, gpioDefaultsMigrationRefreshSDCard) {
		t.Fatal("默认配置应带上迁移标记，避免以后重复补位")
	}
	if err := validateGPIOConfig(config); err != nil {
		t.Fatalf("默认配置应当通过校验: %v", err)
	}
}

// 从上一版升级：那一版把刷新SD卡写在网络键长按 3 秒上（带旧迁移标记）。升级后
// 要把它挪到短按，不能留下「长按还是刷新SD卡」的残留绑定。
func TestNormalizeConfigRelocatesLegacyHold3sRefreshSDCard(t *testing.T) {
	config := Config{GPIO: GPIOConfig{
		Version:    gpioConfigVersion,
		Enabled:    true,
		Migrations: []string{gpioDefaultsMigrationLegacyHold3S},
		Buttons: []GPIOButtonConfig{
			{ID: "copy", Actions: GPIOActions{Short: GPIOActionRefreshStorage}},
			{ID: "network", Actions: GPIOActions{Short: GPIOActionReapplyPlugin, Hold3S: GPIOActionRefreshSDCard}},
			{ID: "rear_reset", Actions: GPIOActions{Short: GPIOActionSMARTCheck}},
		},
	}}
	if !normalizeConfig(&config) {
		t.Fatal("旧版长按绑定应当被搬迁")
	}
	network, _ := gpioTestButton(config.GPIO, "network")
	if network.Actions.Short != GPIOActionRefreshSDCard {
		t.Fatalf("网络键短按应当接手刷新SD卡，实际 %q", network.Actions.Short)
	}
	if !gpioActionIsEmpty(network.Actions.Hold3S) {
		t.Fatalf("长按 3 秒上的旧绑定应当被收回，实际 %q", network.Actions.Hold3S)
	}
	// copy 键没被旧迁移碰过，不受影响。
	copyButton, _ := gpioTestButton(config.GPIO, "copy")
	if copyButton.Actions.Short != GPIOActionRefreshStorage {
		t.Fatalf("其它按键不该被动，实际 %q", copyButton.Actions.Short)
	}
	if err := validateGPIOConfig(config.GPIO); err != nil {
		t.Fatalf("搬迁后的配置应当通过校验: %v", err)
	}

	// 再来一次：已经搬完了，不该再有变化。
	if normalizeConfig(&config) {
		t.Fatal("搬迁应当只发生一次")
	}
}

// 用户把旧版的长按档位改成别的动作之后，就不再认为那是我们的默认值：只收回短按
// 补位（此时短按已被用户占用，于是什么都不动）。
func TestNormalizeConfigKeepsUserChangedHold3s(t *testing.T) {
	config := Config{GPIO: GPIOConfig{
		Version:    gpioConfigVersion,
		Enabled:    true,
		Migrations: []string{gpioDefaultsMigrationLegacyHold3S},
		Buttons: []GPIOButtonConfig{
			{ID: "network", Actions: GPIOActions{Short: GPIOActionLog, Hold3S: GPIOActionRefreshStorage}},
		},
	}}
	normalizeConfig(&config)
	network, _ := gpioTestButton(config.GPIO, "network")
	if network.Actions.Hold3S != GPIOActionRefreshStorage {
		t.Fatalf("用户自己设的长按动作不该被改，实际 %q", network.Actions.Hold3S)
	}
	if network.Actions.Short != GPIOActionLog {
		t.Fatalf("用户自己设的短按动作不该被改，实际 %q", network.Actions.Short)
	}
}

func TestApplyGPIORecommendedDefaults(t *testing.T) {
	// 老配置：启用着按键映射，但四个档位都还空着（连短按也没配过）。
	legacy := GPIOConfig{Version: gpioConfigVersion, Enabled: true}
	for _, spec := range gpioButtonSpecs {
		legacy.Buttons = append(legacy.Buttons, GPIOButtonConfig{ID: spec.ID})
	}
	legacy.Backups = []GPIOBackupJob{{
		ID: "photos", Name: "照片", Mode: BackupModeCopy,
		Source: BackupLocation{Kind: "path", Path: "/vol1/1000/照片"},
		Target: BackupLocation{Kind: "path", Path: "/vol1/backup"},
	}}

	// 走真实的加载归一化路径：老配置里四个档位干脆没有字段。
	config := Config{GPIO: legacy}
	if !normalizeConfig(&config) {
		t.Fatal("应补上推荐默认值")
	}
	legacy = config.GPIO
	network, _ := gpioTestButton(legacy, "network")
	if network.Actions.Short != GPIOActionRefreshSDCard {
		t.Fatalf("网络键短按应被补上刷新SD卡，实际 %q", network.Actions.Short)
	}
	if !gpioActionIsEmpty(network.Actions.Hold3S) {
		t.Fatalf("长按档位不该被填，实际 %q", network.Actions.Hold3S)
	}
	copyButton, _ := gpioTestButton(legacy, "copy")
	if !gpioActionIsEmpty(copyButton.Actions.Short) {
		t.Fatalf("除网络键外不该补任何默认动作，实际 %q", copyButton.Actions.Short)
	}
	if len(legacy.Backups) != 1 {
		t.Fatal("补默认值不能弄丢备份任务")
	}
	if err := validateGPIOConfig(legacy); err != nil {
		t.Fatalf("补完的配置应当通过校验: %v", err)
	}

	// 用户随后把这一档改成「无动作」：那是明确选择，再加载时不该被填回去。
	for index := range legacy.Buttons {
		if legacy.Buttons[index].ID == "network" {
			legacy.Buttons[index].Actions.Short = GPIOActionNone
		}
	}
	config = Config{GPIO: legacy}
	normalizeConfig(&config)
	network, _ = gpioTestButton(config.GPIO, "network")
	if network.Actions.Short != GPIOActionNone {
		t.Fatalf("用户清空过的档位不该被填回，实际 %q", network.Actions.Short)
	}
}

func TestApplyGPIORecommendedDefaultsRespectsUserChoices(t *testing.T) {
	cases := []struct {
		name   string
		config GPIOConfig
		want   string
	}{
		{
			name:   "用户已经设过短按",
			config: gpioTestConfig(true, map[string]GPIOActions{"network": {Short: GPIOActionLog}}),
			want:   GPIOActionLog,
		},
		{
			name:   "用户配过其它档位，说明他在自己管理按键",
			config: gpioTestConfig(true, map[string]GPIOActions{"network": {Hold9S: GPIOActionLog}}),
			want:   "",
		},
		{
			name:   "按键映射整体关着",
			config: gpioTestConfig(false, nil),
			want:   "",
		},
	}
	for _, test := range cases {
		config := test.config
		applyGPIORecommendedDefaults(&config)
		button, _ := gpioTestButton(config, "network")
		if test.want == "" {
			if !gpioActionIsEmpty(button.Actions.Short) {
				t.Fatalf("%s：不该写入动作，实际 %q", test.name, button.Actions.Short)
			}
			continue
		}
		if button.Actions.Short != test.want {
			t.Fatalf("%s：短按应保持 %q，实际 %q", test.name, test.want, button.Actions.Short)
		}
		// 无论哪种情况，短按都不该被写到长按档位上。
		if !gpioActionIsEmpty(button.Actions.Hold3S) {
			t.Fatalf("%s：长按 3 秒不该被动，实际 %q", test.name, button.Actions.Hold3S)
		}
	}
}

// 刷新是硬件相关操作，一次只允许一个在跑：按键连按不该叠起一堆重授权。
func TestStartRefreshSDCardRejectsConcurrentRun(t *testing.T) {
	manager := &Manager{}
	manager.sdcardRun.running = true
	if err := manager.StartRefreshSDCard(); err == nil {
		t.Fatal("已有刷新在跑时应当拒绝")
	}
	if _, err := manager.RunRefreshSDCard(context.Background()); err == nil {
		t.Fatal("同步入口同样应当被拒绝")
	}
	manager.sdcardMu.Lock()
	manager.sdcardRun.running = false
	manager.sdcardRun.lastSummary = "已重新识别 1 个 USB 读卡器（2-1.4）"
	manager.sdcardMu.Unlock()
	running, summary, lastErr := manager.SdcardRefreshStatus()
	if running || lastErr != "" || summary == "" {
		t.Fatalf("状态读取不正确: running=%v summary=%q err=%q", running, summary, lastErr)
	}
}

// 旧标记只处理一次，而且只动 network 键：用户升级后自己把某个键的长按 3 秒设成
// 刷新SD卡，保存时不该被我们再清掉。
func TestRelocateLegacyRefreshSDCardRunsOnceAndOnlyTouchesNetwork(t *testing.T) {
	config := GPIOConfig{
		Version:    gpioConfigVersion,
		Enabled:    true,
		Migrations: []string{gpioDefaultsMigrationLegacyHold3S},
		Buttons: []GPIOButtonConfig{
			{ID: "network", Actions: GPIOActions{Hold3S: GPIOActionRefreshSDCard}},
			{ID: "copy", Actions: GPIOActions{Short: GPIOActionLog, Hold3S: GPIOActionRefreshSDCard}},
		},
	}
	if !relocateLegacyRefreshSDCard(&config) {
		t.Fatal("带着旧标记时应当执行搬迁")
	}
	if gpioContainsString(config.Migrations, gpioDefaultsMigrationLegacyHold3S) {
		t.Fatal("搬迁完必须把旧标记摘掉，否则每次归一化都会重跑")
	}
	network, _ := gpioTestButton(config, "network")
	if !gpioActionIsEmpty(network.Actions.Hold3S) || network.Actions.Short != GPIOActionRefreshSDCard {
		t.Fatalf("网络键应当从长按搬到短按，实际 short=%q hold3s=%q", network.Actions.Short, network.Actions.Hold3S)
	}
	copyButton, _ := gpioTestButton(config, "copy")
	if copyButton.Actions.Hold3S != GPIOActionRefreshSDCard || copyButton.Actions.Short != GPIOActionLog {
		t.Fatalf("别的按键不属于这次迁移，不该被动: %+v", copyButton.Actions)
	}

	// 用户随后自己把网络键长按 3 秒设成刷新SD卡：标记已摘，不该再被改写。
	network.Actions.Hold3S = GPIOActionRefreshSDCard
	for index := range config.Buttons {
		if config.Buttons[index].ID == "network" {
			config.Buttons[index] = network
		}
	}
	if relocateLegacyRefreshSDCard(&config) {
		t.Fatal("搬迁只能做一次")
	}
	if network, _ = gpioTestButton(config, "network"); network.Actions.Hold3S != GPIOActionRefreshSDCard {
		t.Fatalf("用户自己设的长按不该被清掉，实际 %q", network.Actions.Hold3S)
	}
}

// 「看起来没配置过」不能只看四个档位：用户主动关掉按键映射、但留过迁移标记、脚本
// 或提示音开关，都说明他来过。被判成未配置就会被默认值整份顶掉——映射被重开、
// 脚本被删、提示音被重开。
func TestGPIOConfigLooksUnconfiguredRespectsUserTraces(t *testing.T) {
	empty := gpioTestConfig(false, nil)
	if !gpioConfigLooksUnconfigured(empty) {
		t.Fatal("关闭且四档全空的老配置应当被判为未配置")
	}

	withMigration := empty
	withMigration.Migrations = []string{gpioDefaultsMigrationRefreshSDCard}
	if gpioConfigLooksUnconfigured(withMigration) {
		t.Fatal("带迁移标记说明用户来过，不能当成未配置")
	}

	withScript := empty
	withScript.Scripts = []GPIOScript{{ID: "s1", Name: "自检", Body: "echo ok"}}
	if gpioConfigLooksUnconfigured(withScript) {
		t.Fatal("保存过脚本说明用户来过，不能当成未配置")
	}

	notifyOff := empty
	notifyOff.StorageNotify = gpioBoolPointer(false)
	if gpioConfigLooksUnconfigured(notifyOff) {
		t.Fatal("关掉过提示音说明用户来过，不能当成未配置")
	}
}

func gpioTestButton(config GPIOConfig, id string) (GPIOButtonConfig, bool) {
	for _, button := range config.Buttons {
		if button.ID == id {
			return button, true
		}
	}
	return GPIOButtonConfig{}, false
}

func gpioTestConfig(enabled bool, actions map[string]GPIOActions) GPIOConfig {
	config := GPIOConfig{Version: gpioConfigVersion, Enabled: enabled}
	for _, spec := range gpioButtonSpecs {
		entry := GPIOActions{}
		if actions != nil {
			if override, ok := actions[spec.ID]; ok {
				entry = override
			}
		}
		config.Buttons = append(config.Buttons, GPIOButtonConfig{ID: spec.ID, Actions: entry})
	}
	return config
}
