package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"fnos-powerguard/internal/crpsump"
	"fnos-powerguard/internal/powerguard"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "probe":
		err = probe(os.Args[2:])
	case "init":
		err = initialize(os.Args[2:])
	case "apply":
		err = apply(os.Args[2:])
	case "restore":
		err = restore(os.Args[2:])
	case "release-fans":
		err = releaseFans(os.Args[2:])
	case "bind-all-fans":
		err = bindAllFans(os.Args[2:])
	case "publish":
		err = publish(os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tad-module:", err)
		os.Exit(1)
	}
}

func commonFlags(name string, args []string) (*powerguard.Manager, *flag.FlagSet, error) {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	config := set.String("config", "", "config file")
	state := set.String("state", "", "state file")
	root := set.String("sys-root", "/", "alternate system root (testing only)")
	if err := set.Parse(args); err != nil {
		return nil, nil, err
	}
	if *config == "" || *state == "" {
		return nil, nil, errors.New("--config and --state are required")
	}
	return &powerguard.Manager{Root: *root, ConfigPath: *config, StatePath: *state, Version: version}, set, nil
}

func initialize(args []string) error {
	manager, _, err := commonFlags("init", args)
	if err != nil {
		return err
	}
	if err := requireRoot(); err != nil {
		return err
	}
	if _, err := manager.LoadOrCreateConfig(); err != nil {
		return err
	}
	return manager.ApplyCurrent()
}

func apply(args []string) error {
	manager, _, err := commonFlags("apply", args)
	if err != nil {
		return err
	}
	if err := requireRoot(); err != nil {
		return err
	}
	return manager.ApplyCurrent()
}

func restore(args []string) error {
	manager, _, err := commonFlags("restore", args)
	if err != nil {
		return err
	}
	if err := requireRoot(); err != nil {
		return err
	}
	err = manager.Restore()
	if errors.Is(err, powerguard.ErrOriginalStateCPUMismatch) {
		fmt.Fprintln(os.Stderr, "tad-module: skipped restoring power and fan state captured on different hardware")
		return nil
	}
	return err
}

// releaseFans 把风扇控制权交还 BIOS：只处理风扇，不动功耗限制。
// 卸载时跑它，重装后服务读到 Fan.Enabled=false 就不会一启动又接管风扇。
func releaseFans(args []string) error {
	manager, _, err := commonFlags("release-fans", args)
	if err != nil {
		return err
	}
	if err := requireRoot(); err != nil {
		return err
	}
	return manager.ReleaseFans()
}

// bindAllFans 只在全新安装时跑：三条曲线都绑上所有风扇、打开风扇控制，
// 曲线用出厂默认的平衡档。卸载重装时配置还在，不会调用它。
func bindAllFans(args []string) error {
	manager, _, err := commonFlags("bind-all-fans", args)
	if err != nil {
		return err
	}
	if err := requireRoot(); err != nil {
		return err
	}
	return manager.BindAllFansDefault()
}

// publish 把 stage 文件以原子 no-replace 方式发布到精确目标路径:
// 内部直接调用 link(2)(os.Link),对任何已存在目标——普通文件、目录、
// 指向目录的符号链接、指向文件的符号链接——都返回 EEXIST 而失败,
// 不产生嵌套条目、不覆盖目标、不改权限、不删任何东西。成功后 stage 与
// 目标共享同一 inode,stage 的清理(是否 unlink)由调用方决定。
// DSM busybox ln 对目录目标会"移入"(绕过 link(2) 语义),shell 工具集
// 内不存在等价原语,故此子命令是配置/暂存发布的唯一发布通道。
// 用法: tad-module publish <stage> <dest>
func publish(args []string) error {
	set := flag.NewFlagSet("publish", flag.ContinueOnError)
	if err := set.Parse(args); err != nil {
		return err
	}
	rest := set.Args()
	if len(rest) != 2 || rest[0] == "" || rest[1] == "" {
		return errors.New("usage: tad-module publish <stage> <dest>")
	}
	return os.Link(rest[0], rest[1])
}

func probe(args []string) error {	set := flag.NewFlagSet("probe", flag.ContinueOnError)
	root := set.String("sys-root", "/", "alternate system root")
	asJSON := set.Bool("json", false, "JSON output")
	if err := set.Parse(args); err != nil {
		return err
	}
	manager := &powerguard.Manager{Root: *root, Version: version}
	model, err := manager.CPUModel()
	if err != nil {
		return err
	}
	profile, err := powerguard.DetectProfile(model)
	if err != nil {
		return err
	}
	packages, err := manager.DiscoverPackages()
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"cpu_model": model, "profile": profile, "packages": packages})
	}
	fmt.Printf("CPU: %s\nProfile: %s\nRAPL packages: %d\n", model, profile.ID, len(packages))
	return nil
}

// serve 启动守护进程: 应用配置、挂载 HTTP(socket/TCP) 服务与各后台循环, 退出时恢复原始功耗/风扇状态
func serve(args []string) error {
	set := flag.NewFlagSet("serve", flag.ContinueOnError)
	config := set.String("config", "", "config file")
	state := set.String("state", "", "state file")
	socket := set.String("socket", "", "Unix socket")
	httpAddr := set.String("http", "", "optional TCP listen address, e.g. :8123 (DSM deployment)")
	authFile := set.String("auth-file", "", "HTTP Basic auth credentials JSON for the TCP listener")
	dsmAuth := set.Bool("dsm-auth", false, "authenticate TCP requests via the DSM session (authenticate.cgi) and require the administrators group (DSM embedded mode)")
	proxyTokenFile := set.String("proxy-token-file", "", "shared secret file agreed with the trusted reverse proxy (DSM nginx)")
	socketGroup := set.String("socket-group", "www-data", "unix socket group, or 'none' to keep it root-only")
	basePath := set.String("base-path", "", "URL prefix to trim from requests (fnOS default /app/tad-module; DSM embedded /webman/3rdparty/tad-module)")
	webRoot := set.String("web-root", "", "static web root")
	logPath := set.String("log", "", "log file")
	root := set.String("sys-root", "/", "alternate system root (testing only)")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *config == "" || *state == "" || *webRoot == "" {
		return errors.New("--config, --state and --web-root are required")
	}
	if *socket == "" && *httpAddr == "" {
		return errors.New("one of --socket and --http is required")
	}
	if err := requireRoot(); err != nil {
		return err
	}
	logger := log.Default()
	var logFile *os.File
	var manager *powerguard.Manager
	if *logPath != "" {
		if err := os.MkdirAll(filepath.Dir(*logPath), 0o700); err != nil {
			return err
		}
		file, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer func() { _ = logFile.Close() }()
		logFile = file
		// 写入路径的大小兜底：定时轮转（10 分钟）之外，错误刷屏写满超限时
		// 就地截断。**limit 只读原子快照**——Write 可能在持有 m.mu 的 logf 里
		// 被触发（saveConfigLocked 等），调 LogSettings() 会重入 m.mu 死锁。
		// 必须用闭包而非方法值：此刻 manager 变量还是 nil，方法值会在求值时
		// 绑定 nil 接收者（后续给变量赋值无法挽回），闭包则到调用时才取变量
		capped := powerguard.NewSizeCappedLogWriter(file, func() int64 {
			if manager == nil {
				return 0
			}
			return manager.LogLimitBytes()
		})
		logger = log.New(capped, "", log.LstdFlags|log.LUTC)
	}
	manager = &powerguard.Manager{Root: *root, ConfigPath: *config, StatePath: *state, Version: version}
	manager.SetLogger(logger)
	cfg, err := manager.LoadOrCreateConfig()
	if err != nil {
		return err
	}
	// 日志大小上限的原子快照在此初始化（写入路径兜底与定时轮转共用，
	// 后续由 SaveLogConfig 在保存时更新）
	manager.SetLogLimitBytes(int64(cfg.Log.MaxSizeMB) << 20)
	if err := manager.ApplyCurrent(); err != nil {
		logger.Printf("initial apply failed; continuing in degraded mode so configuration can be repaired: %v", err)
	}
	logger.Printf("started version=%s interval=%ds", version, cfg.ReapplySeconds)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	done := make(chan error, 1)
	// 历史存储初始化失败只降级历史功能（/api/history 返回空），绝不影响
	// 风扇控制等主功能——守护进程必须照常运行。
	history, historyErr := powerguard.NewHistoryStore(filepath.Join(filepath.Dir(*state), "history.db"))
	if historyErr != nil {
		logger.Printf("history store disabled: %v", historyErr)
		history = nil
	}
	if history != nil {
		defer history.Close()
		history.SetLogger(logger)
		// 启动即按配置同步保留期与长期记录并清理一次（构造函数只用了默认值）
		if err := history.SyncSettings(cfg.History); err != nil {
			logger.Printf("history settings sync (prune/archive) failed: %v", err)
		}
		go powerguard.HistoryLoop(ctx, manager, logger, history)
	}
	// CRPS PMBus 监控板块 (crpsump)：可选功能，硬件缺失/未配置时仅 CRPS 页面降级，
	// 主功能照常——与 history 同样的失败哲学。配置走独立 crps.json（与主配置同目录），
	// 耗电统计 energy.json 同目录落盘，退出时保存。
	crpsHandler, crpsEnergySave := initCrps(logger, filepath.Join(filepath.Dir(*config), "crps.json"))
	if crpsEnergySave != nil {
		defer crpsEnergySave() // CRPS 耗电统计退出落盘 (ctx.Done 与 server 错误两条路径均覆盖)
	}
	go func() {
		// BasePath: 不传 --base-path 时保持 fnOS 宿主的固定前缀;DSM 嵌入模式
		// 传 /webman/3rdparty/tad-module,与 nginx 反代的前缀一致
		tadBasePath := "/app/tad-module"
		if *basePath != "" {
			tadBasePath = *basePath
		}
		// DSM 桌面嵌入模式:经 DSM 自带 authenticate.cgi 校验会话并要求
		// administrators 组(路径随 sys-root,测试可注入替身)
		var dsmSessionAuth *powerguard.DSMSessionAuth
		if *dsmAuth {
			dsmSessionAuth = powerguard.NewDSMSessionAuth()
			dsmSessionAuth.AuthenticateBin = filepath.Join(*root, strings.TrimPrefix(powerguard.DSMAuthenticateBinPath(), "/"))
			dsmSessionAuth.GroupFile = filepath.Join(*root, "etc/group")
			dsmSessionAuth.SessionFile = filepath.Join(*root, "usr/syno/etc/private/session/current.users")
		}
		server := &powerguard.Server{Manager: manager, Socket: *socket, WebRoot: *webRoot, BasePath: tadBasePath, Logger: logger, History: history, LogPath: *logPath, Crps: crpsHandler,
			// TCP 直连（群晖部署）: DSM 会话鉴权或 Basic 凭据; 非回环绑定
			// 而两者皆无时服务拒绝启动
			HTTPAddr:       *httpAddr,
			AuthFile:       *authFile,
			DSMAuth:        dsmSessionAuth,
			ProxyTokenFile: *proxyTokenFile,
			SocketGroup:    *socketGroup,
			// 网页可访问时滴一声：只在系统刚启动的窗口内响，升级/手动重启服务
			// （此时系统已运行很久）不再重复响。
			OnReady: func() {
				if recentlyBooted(*root) {
					go manager.PowerChime()
				}
			}}
		done <- server.ListenAndServe()
	}()
	// 日志大小清理：独立于历史数据库大小上限（log.max_size_mb，默认 16MB），
	// 超限后内容另存 .1 并就地截断。读取走 manager 的实时配置，用户改上限
	// 后最迟一个检查周期生效。
	if logFile != nil {
		go powerguard.LogRotateLoop(ctx, logger, *logPath, logFile, manager.LogLimitBytes)
	}
	go reapplyLoop(ctx, manager, logger)
	go fanLoop(ctx, manager, logger)
	go storageLoop(ctx, manager, logger)
	go storageActivityLoop(ctx, manager)
	go storageNotifyLoop(ctx, manager, logger)
	go autoBackupClearLoop(ctx, manager, logger)
	go gpioLoop(ctx, manager, logger)
	// 开关机计划：到点触发关机 + 同步 RTC 唤醒（power_schedule.go）
	go powerScheduleLoop(ctx, manager, logger)
	// USB 串口温度传感器读取器：未配置时内部低频空转，常驻无副作用
	go manager.SerialSensorLoop(ctx, logger)
	// USB HID 温度计（TEMPer 家族）：锁外后台读取 + 缓存（阻塞 IO 不进
	// /api/status 持锁路径），启动探测随行
	manager.ProbeUSBTemperatureSensors(logger)
	go manager.USBTemperatureLoop(ctx, logger)

	select {
	case <-ctx.Done():
		logger.Printf("stopping: restoring captured power and fan settings")
		// 软关机/服务停止前滴一声：此时蜂鸣器仍供电，同步播完再恢复。
		// 蜂鸣器缺失或开关关闭时 PowerChime 直接返回，不影响退出流程。
		manager.PowerChime()
		err := manager.Restore()
		if errors.Is(err, powerguard.ErrOriginalStateCPUMismatch) {
			logger.Printf("restore skipped because saved state belongs to different hardware: %v", err)
			return nil
		}
		return err
	case err := <-done:
		logger.Printf("server stopped: restoring captured power and fan settings")
		restoreErr := manager.Restore()
		if errors.Is(restoreErr, powerguard.ErrOriginalStateCPUMismatch) {
			logger.Printf("restore skipped because saved state belongs to different hardware: %v", restoreErr)
			restoreErr = nil
		}
		return errors.Join(err, restoreErr)
	}
}

func reapplyLoop(ctx context.Context, manager *powerguard.Manager, logger *log.Logger) {
	for {
		cfg, err := manager.LoadOrCreateConfig()
		interval := 30 * time.Second
		if err == nil && cfg.ReapplySeconds >= 5 && cfg.ReapplySeconds <= 300 {
			interval = time.Duration(cfg.ReapplySeconds) * time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			if err := manager.ApplyCurrent(); err != nil {
				logger.Printf("reapply failed: %v", err)
			}
		}
	}
}

func fanLoop(ctx context.Context, manager *powerguard.Manager, logger *log.Logger) {
	for {
		cfg, err := manager.LoadOrCreateConfig()
		interval := 2 * time.Second
		if err == nil && cfg.Fan.PollSeconds >= 1 && cfg.Fan.PollSeconds <= 10 {
			interval = time.Duration(cfg.Fan.PollSeconds) * time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			if err := manager.ApplyFanCurrent(); err != nil {
				logger.Printf("fan control failed: %v", err)
			}
		}
	}
}

func storageActivityLoop(ctx context.Context, manager *powerguard.Manager) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			manager.RefreshStorageActivity()
		}
	}
}

// storageNotifyLoop 监视「有没有插入新的存储设备」：移动存储长响一次、内置存储
// 连续响两声（见 internal/powerguard/storage_notify.go）。采样只是 glob /sys/block
// 加几个小文件读，2 秒一拍也几乎没有开销；首拍只建立基线，开机时已经在的盘不响。
func storageNotifyLoop(ctx context.Context, manager *powerguard.Manager, logger *log.Logger) {
	manager.MonitorStorageInserts()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			manager.MonitorStorageInserts()
		}
	}
}

// autoBackupClearLoop 执行挂起的「离线取消打标」：设备重新插上后删掉卷内的打标
// 标记（backup_auto.go 的 ProcessAutoBackupClears）。15 秒一拍，没有挂起项时
// 只读一次配置就返回，开销可以忽略。
func autoBackupClearLoop(ctx context.Context, manager *powerguard.Manager, logger *log.Logger) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleared, err := manager.ProcessAutoBackupClears()
			if err != nil {
				logger.Printf("auto backup clear failed: %v", err)
			}
			if cleared > 0 {
				logger.Printf("auto backup: cleared %d pending volume marker(s)", cleared)
			}
		}
	}
}

func storageLoop(ctx context.Context, manager *powerguard.Manager, logger *log.Logger) {
	refresh := func() {
		status := manager.RefreshStorage(false)
		if status.LastError != "" {
			logger.Printf("storage refresh partially failed: %s", status.LastError)
		}
	}
	refresh()
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}

// 开关机计划：启动先对账一次（同步 RTC 唤醒），之后每 30 秒检查——
// 到点触发计划关机、wakealarm 与开机计划不一致时重写。
func powerScheduleLoop(ctx context.Context, manager *powerguard.Manager, logger *log.Logger) {
	manager.PowerScheduleTick(ctx)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			manager.PowerScheduleTick(ctx)
		}
	}
}

func gpioLoop(ctx context.Context, manager *powerguard.Manager, logger *log.Logger) {
	actions := make(chan powerguard.GPIOEvent, 4)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-actions:
				output, err := manager.ExecuteGPIOActionContext(ctx, event)
				if output != "" {
					logger.Printf("gpio script output button=%s action=%s: %s", event.ButtonID, event.Action, output)
				}
				if err != nil {
					logger.Printf("gpio action failed button=%s action=%s: %v", event.ButtonID, event.Action, err)
				}
			}
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var port *os.File
	defer func() {
		if port != nil {
			_ = port.Close()
		}
	}()
	var config powerguard.GPIOConfig
	var nextConfigLoad time.Time
	var lastLoggedError string
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if !now.Before(nextConfigLoad) {
				cfg, err := manager.LoadOrCreateConfig()
				if err != nil {
					if err.Error() != lastLoggedError {
						logger.Printf("gpio config failed: %v", err)
						lastLoggedError = err.Error()
					}
					nextConfigLoad = now.Add(time.Second)
					continue
				}
				config = cfg.GPIO
				nextConfigLoad = now.Add(time.Second)
			}
			if !config.Enabled {
				if port != nil {
					_ = port.Close()
					port = nil
				}
				manager.ResetGPIO(false, false, nil)
				lastLoggedError = ""
				continue
			}
			if port == nil {
				var err error
				port, err = os.Open(manager.GPIOPortPath())
				if err != nil {
					manager.ResetGPIO(true, false, err)
					if err.Error() != lastLoggedError {
						logger.Printf("gpio open failed: %v", err)
						lastLoggedError = err.Error()
					}
					continue
				}
			}
			events, err := manager.PollGPIO(config, port, now)
			if err != nil {
				if err.Error() != lastLoggedError {
					logger.Printf("gpio poll failed: %v", err)
					lastLoggedError = err.Error()
				}
				_ = port.Close()
				port = nil
				continue
			}
			lastLoggedError = ""
			for _, event := range events {
				if event.IsFeedback() {
					logger.Printf("gpio hold confirmed button=%s stage=%s", event.ButtonID, event.Stage)
					go func(feedback powerguard.GPIOEvent) {
						if err := manager.PlayGPIOFeedback(feedback); err != nil {
							logger.Printf("gpio feedback unavailable button=%s stage=%s: %v", feedback.ButtonID, feedback.Stage, err)
						}
					}(event)
					continue
				}
				logger.Printf("gpio event button=%s stage=%s duration=%s action=%s", event.ButtonID, event.Stage, event.Duration.Round(100*time.Millisecond), event.Action)
				select {
				case actions <- event:
				default:
					err := errors.New("gpio action queue is full")
					manager.SetGPIOActionError(err)
					logger.Printf("gpio action dropped button=%s action=%s: %v", event.ButtonID, event.Action, err)
				}
			}
		}
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: tad-module <serve|probe|init|apply|restore|release-fans|bind-all-fans|publish|version> [options]")
}

// initCrps 初始化 CRPS PMBus 监控板块 (可选功能)。配置读独立文件 cfgPath
// (scl_pin/sda_pin, 缺省 Pin19/Pin20; 仅两个字段齐全才视为已在界面配置过)，
// 耗电统计落盘 <cfgPath 同目录>/energy.json。NewBus 对非法引脚名内部回退默认值、
// 硬件不可达在请求时降级为 no-gpio，因此本函数不返回错误，CRPS 始终挂载。
// 日志沿用 crpsump 自有的 /tmp/psu-monitor.log (自带按大小轮转)，不并入主日志。
func initCrps(logger *log.Logger, cfgPath string) (http.Handler, func()) {
	sclPin, sdaPin := crpsump.DefaultSCL, crpsump.DefaultSDA
	configured := false
	if data, err := os.ReadFile(cfgPath); err == nil {
		var cfg struct {
			Scl string `json:"scl_pin"`
			Sda string `json:"sda_pin"`
		}
		if json.Unmarshal(data, &cfg) == nil {
			if cfg.Scl != "" {
				sclPin = cfg.Scl
			}
			if cfg.Sda != "" {
				sdaPin = cfg.Sda
			}
			// 仅完整引脚对(界面保存/探测成功才会双双写盘)才算已配置:
			// 空文件 {} 或缺字段不得在启动时触发 GPIO 方向初始化
			configured = cfg.Scl != "" && cfg.Sda != ""
		}
	}
	// 与宿主对齐: 写接口走宿主同一套管理员鉴权 (X-Trim-Isadmin),
	// /dev/port 访问与宿主按键轮询/LED 反馈共用同一把锁。须在 NewBus 前注入
	// (configured=true 时 NewBus 即写 GPIO 方向)。
	crpsump.AdminCheck = powerguard.IsAdminRequest
	crpsump.PortIOLocker = powerguard.PortIOLocker()
	// 日志放到 crps.json 同目录 (宿主状态目录), 不使用全局可写的 /tmp
	crpsump.LogPath = filepath.Join(filepath.Dir(cfgPath), "crps.log")
	bus := crpsump.NewBus(sclPin, sdaPin, configured)
	energy := crpsump.StartEnergy(bus, filepath.Join(filepath.Dir(cfgPath), "energy.json"))
	crpsump.StartStatusRefresher(bus) // 状态快照后台刷新: CRPS 页面首开即有数据
	crpsump.LogStartup(bus, sclPin, sdaPin)
	logger.Printf("crps module enabled config=%s energy=%s", cfgPath, filepath.Join(filepath.Dir(cfgPath), "energy.json"))
	srv := &crpsump.AppServer{Bus: bus, ConfigPath: cfgPath}
	return srv.APIHandler(), energy.Save
}

// bootChimeWindow 开机提示音的判定窗口：只有系统刚启动（uptime 小于该值）才响。
// 服务随升级/手动重启也会走到就绪回调，但那时 uptime 早已超出窗口，不会重复滴。
const bootChimeWindow = 10 * time.Minute

// recentlyBooted 读取 sys-root 下的 /proc/uptime 判断系统是否刚启动；读不到时
// 一律按「不是刚开机」处理，宁可不响也不要在服务重启时误响。
func recentlyBooted(root string) bool {
	data, err := os.ReadFile(filepath.Join(root, "proc", "uptime"))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return false
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return false
	}
	return seconds < bootChimeWindow.Seconds()
}
