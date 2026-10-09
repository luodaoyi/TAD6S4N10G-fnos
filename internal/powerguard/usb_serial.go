package powerguard

// USB 转串口外置温度传感器（方案 A：通用文本行源，多设备版）。用户经 USB
// 转串口模块（CH340/CP210x/FTDI 等）接 DIY 温度节点（Arduino/ESP32 +
// DS18B20/SHT30 最常见），固件逐行自发输出含温度数字的文本（"25.6"、
// "temp:25.6"、"+25.6"、`{"temp":25.6}` 均可）；行内第一个落在 [-55,125] 的
// 数字按摄氏度入库（区间取 DS18B20 物理量程，负温合法，-127 断连哨兵与
// 恰好 0.0 拒绝——真机固件按带符号小数输出）。
//
// 与 usb_temper.go 的 HID 温度计分工：那边没有内核驱动、协议一族一条命令，
// 成本在传输层；这里内核白送 /dev/ttyUSB*，成本在协议多样性——所以只做
// "读行取数" 的最大公约数，Modbus/1-wire 等专项协议等真实需求再立项。
//
// 多设备模型：SerialSensorLoop 是监督者，按配置数组与在跑读取器集合做
// diff——保存新配置 kick 后增删对应读取器；每个启用的 (path,baud) 一个
// 常驻 goroutine，以 serialPollInterval 节奏做"开口→读一把→关口"的间歇
// 轮询：串口在绝大部分时间里保持空闲，用户手动调试（cat）或其他程序可以
// 随时打开同一串口，不会长期互抢数据（tty 的字节流只派发给先到的 reader，
// 两个常驻读会互相偷行）。开口后立即清 DTR/RTS（usb_serial_linux.go），
// 避免 Arduino 类板子被 open 复位。读数缓存给 extraTemperatures 并入采样
// 链（key 形如 "usb:tty:ttyUSB0"，自动归「其它」组并享受改名链路）。跨轮
// 停止输出超过 serialStaleAfter 的读数不入采样——拔掉的传感器曲线自然断
// 线，不留冻结值。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	serialDefaultBaud  = 9600             // 波特率缺省值（DIY 固件事实标准）
	serialPollInterval = 5 * time.Second  // 轮询周期：开口→读一把→关口→睡到下轮
	serialReadWindow   = 3 * time.Second  // 每轮的读取窗口：读到行即续命，窗口到即关口
	serialStaleAfter   = 30 * time.Second // 跨轮无新行视为失效，读数不入采样（曲线断线）
	serialIdleWait     = 15 * time.Second // 无启用设备时监督者空转间隔
	serialReconcileGap = 60 * time.Second // 监督者周期对账间隔（kick 之外的兜底）
	serialMaxDevices   = 8                // 配置行数上限（防手滑/脏数据堆积）
)

// serialBaudRates 配置界面与校验共用的波特率白名单。
var serialBaudRates = []int{2400, 4800, 9600, 19200, 38400, 57600, 115200}

// SerialSensorConfig 一个串口温度传感器的配置，Config.Serials 数组元素。
type SerialSensorConfig struct {
	Enabled bool   `json:"enabled"`
	Path    string `json:"path,omitempty"` // 设备路径；优先 /dev/serial/by-id/*（插拔不漂移）
	Baud    int    `json:"baud,omitempty"` // 0 视为 serialDefaultBaud
}

// NormalizeSerialSensorConfigs 校验整组串口配置：启用的行必须有 /dev/ 下的
// 设备路径与白名单波特率（0 归位默认值）；全部行的路径不得重复（同一路径
// 两个读取器只会互相抢数据）；行数不超过 serialMaxDevices。停用的行保留
// 已填的路径与波特率，重新启用不必重选；未启用的空行（前端刚点添加还没
// 选设备）直接丢弃。
func NormalizeSerialSensorConfigs(configs []SerialSensorConfig) ([]SerialSensorConfig, error) {
	if len(configs) > serialMaxDevices {
		return nil, fmt.Errorf("串口传感器最多配置 %d 个", serialMaxDevices)
	}
	seen := make(map[string]bool, len(configs))
	for i := range configs {
		configs[i].Path = strings.TrimSpace(configs[i].Path)
		if configs[i].Path == "" {
			if configs[i].Enabled {
				return nil, fmt.Errorf("第 %d 个传感器启用了但未选择设备路径", i+1)
			}
			continue // 未启用的空行（前端刚点添加还没选设备）直接丢弃
		}
		if seen[configs[i].Path] {
			return nil, fmt.Errorf("设备路径重复：%s", configs[i].Path)
		}
		seen[configs[i].Path] = true
		if !configs[i].Enabled {
			continue
		}
		if !strings.HasPrefix(configs[i].Path, "/dev/") {
			return nil, fmt.Errorf("设备路径必须是 /dev/ 下的串口节点: %s", configs[i].Path)
		}
		if configs[i].Baud == 0 {
			configs[i].Baud = serialDefaultBaud
			continue
		}
		if !serialBaudAllowed(configs[i].Baud) {
			return nil, fmt.Errorf("不支持的波特率 %d（可选 %s）", configs[i].Baud, serialBaudList())
		}
	}
	result := make([]SerialSensorConfig, 0, len(configs))
	for _, cfg := range configs {
		if cfg.Path == "" {
			continue
		}
		result = append(result, cfg)
	}
	return result, nil
}

func serialBaudAllowed(baud int) bool {
	for _, rate := range serialBaudRates {
		if rate == baud {
			return true
		}
	}
	return false
}

func serialBaudList() string {
	parts := make([]string, 0, len(serialBaudRates))
	for _, rate := range serialBaudRates {
		parts = append(parts, strconv.Itoa(rate))
	}
	return strings.Join(parts, "/")
}

// rejectSerialPhysicalDuplicates 拒绝多个启用配置指向同一物理串口。
// by-id/by-path 是指向 /dev/ttyUSBN 的符号链接，字符串判重挡不住"同一
// 设备加了三个入口"的配置——多个读取器交错开口/关口同一 tty 会互相打断
// 对方的打开序列（真机出过三条目全部 tiocmset 失败）。路径当前解析不了
// （设备拔出/不可达）时只回退字符串判重，不额外拦截。
func (m *Manager) rejectSerialPhysicalDuplicates(configs []SerialSensorConfig) error {
	firstPath := make(map[string]string, len(configs)) // 解析后的真实设备 → 首个配置路径
	for _, cfg := range configs {
		if !cfg.Enabled || cfg.Path == "" {
			continue
		}
		real, err := filepath.EvalSymlinks(m.rooted(cfg.Path))
		if err != nil {
			continue
		}
		if other, dup := firstPath[real]; dup {
			return fmt.Errorf("多个配置指向同一设备 %s：%s 与 %s", filepath.Base(real), other, cfg.Path)
		}
		firstPath[real] = cfg.Path
	}
	return nil
}

// serialPort 抽象串口的读与关闭，便于离线单测注入假设备。
type serialPort interface {
	Read(b []byte) (int, error)
	SetReadDeadline(t time.Time) error
	Close() error
}

// openSerialPort 打开真实串口并按波特率配置（linux 见 usb_serial_linux.go）；
// 测试替换为假实现。
var openSerialPort = openSerialPortOS

// serialNow 时钟注入点：失效判定（serialStaleAfter）的单测加速。
var serialNow = time.Now

// serialReaderState 一个读取器的运行状态，全部字段随 m.mu 保护；cancel 由
// 监督者持有，done 在读取 goroutine 退出时关闭。
type serialReaderState struct {
	path       string
	key        string
	cancel     context.CancelFunc
	done       chan struct{}
	latest     serialReading // 最近一次有效读数（At 零值 = 尚无数据）
	lastDataAt time.Time     // 最近一次读到有效行（跨轮的无数据判定用）
	open       bool          // 当前轮是否持有已打开的串口
	lastError  string        // 最近一次读取错误（变化才记日志，恢复清空）
}

// serialReading 最近一次有效读数（At 为零值表示尚无数据）。
type serialReading struct {
	Key     string
	Celsius float64
	At      time.Time
}

// SerialSensorLoop 是多设备读取器的监督者：按配置与在跑读取器集合 diff，
// 缺的起、多的停；保存配置 kickSerialReader 后立即重新对账，另有周期对账
// 兜底（防未知路径漏起）。未启用任何设备时低频空转。随 ctx 取消退出并停
// 掉全部读取器。logger 当前仅用于满足启动签名（错误日志走 logf）。
func (m *Manager) SerialSensorLoop(ctx context.Context, _ *log.Logger) {
	m.mu.Lock()
	if m.serialKick == nil {
		m.serialKick = make(chan struct{}, 1)
	}
	if m.serialReaders == nil {
		m.serialReaders = make(map[string]*serialReaderState)
	}
	m.mu.Unlock()
	for ctx.Err() == nil {
		m.reconcileSerialReaders(ctx)
		wait := serialReconcileGap
		m.mu.Lock() // serialSensorConfigsLocked 约定调用方持锁（读配置文件与写盘互斥）
		idle := len(m.serialSensorConfigsLocked()) == 0
		m.mu.Unlock()
		if idle {
			wait = serialIdleWait
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-m.serialKick:
			timer.Stop()
		case <-timer.C:
		}
	}
	// 退出前停掉全部读取器（进程结束/测试收尾），不等待各自的重连周期
	m.mu.Lock()
	for _, state := range m.serialReaders {
		state.cancel()
	}
	states := make([]*serialReaderState, 0, len(m.serialReaders))
	for _, state := range m.serialReaders {
		states = append(states, state)
	}
	m.mu.Unlock()
	for _, state := range states {
		<-state.done
	}
}

// reconcileSerialReaders 对账在跑读取器与启用配置：多的 cancel（不等退出，
// 读取器自带 deadline 上限）、缺的起 goroutine。调用方不持锁。
func (m *Manager) reconcileSerialReaders(ctx context.Context) {
	m.mu.Lock()
	want := make(map[string]SerialSensorConfig)
	for _, cfg := range m.serialSensorConfigsLocked() {
		if cfg.Enabled {
			want[cfg.Path] = cfg
		}
	}
	for path, state := range m.serialReaders {
		if _, ok := want[path]; !ok {
			state.cancel() // 读取器退出时自行从 map 摘除
		}
	}
	start := make([]SerialSensorConfig, 0, len(want))
	for path, cfg := range want {
		if _, running := m.serialReaders[path]; !running {
			start = append(start, cfg)
		}
	}
	m.mu.Unlock()
	for _, cfg := range start {
		go m.runSerialReader(ctx, cfg) // 异步：runSerialReader 内部是常驻循环
	}
}

// runSerialReader 单个设备的轮询循环：每 serialPollInterval 醒来一次，开口
// → 读一把（serialReadWindow 窗口）→ 关口。串口在 ~95% 的时间里保持空闲，
// 用户的手动调试（cat）或其他程序可以随时打开读取，不会长期互抢数据。
// ctx 取消（监督者对账判定该设备不该在跑，或整个服务停止）即退出并从
// m.serialReaders 摘除自己。错误只在变化时记一条日志（不刷屏）。
func (m *Manager) runSerialReader(ctx context.Context, cfg SerialSensorConfig) {
	readerCtx, cancel := context.WithCancel(ctx)
	state := &serialReaderState{
		path:       cfg.Path,
		key:        serialSensorKey(cfg.Path),
		cancel:     cancel,
		done:       make(chan struct{}),
		lastDataAt: serialNow(),
	}
	m.mu.Lock()
	// 对账间隙里同路径可能已有读取器在跑（周期对账与 kick 竞争）：让位退出
	if _, exists := m.serialReaders[cfg.Path]; exists {
		m.mu.Unlock()
		cancel()
		close(state.done)
		return
	}
	m.serialReaders[cfg.Path] = state
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		delete(m.serialReaders, cfg.Path)
		m.mu.Unlock()
		close(state.done)
	}()

	for readerCtx.Err() == nil {
		m.pollSerialOnce(readerCtx, cfg, state)
		if !serialSleep(readerCtx, serialPollInterval) {
			return
		}
	}
}

// pollSerialOnce 执行一轮"开口→读→关口"：打开失败记错误等下轮；打开成功
// 后在窗口内读行，EOF/IO 错误也只记账不重试（下一轮重开即自愈）；窗口结束
// 检查跨轮无数据时长，超过 serialStaleAfter 记"无数据"错误——传感器坏/
// 波特率错时给用户可见反馈，读到行即清。
func (m *Manager) pollSerialOnce(ctx context.Context, cfg SerialSensorConfig, state *serialReaderState) {
	port, err := openSerialPort(cfg.Path, cfg.Baud)
	if err != nil {
		m.noteSerialReaderError(state, fmt.Errorf("%s: %w", cfg.Path, err))
		return
	}
	defer func() {
		_ = port.Close() // 间歇模式的关键：读完就关口，串口归还给系统
		m.setSerialReaderOpen(state, false)
	}()
	m.setSerialReaderOpen(state, true)
	windowStart := serialNow()
	if err := m.readSerialWindow(ctx, port, state, serialReadWindow); err != nil && ctx.Err() == nil {
		// EOF 等瞬断在轮询模式下无需特殊处理：下一轮重开即自愈
		m.noteSerialReaderError(state, err)
	}
	m.mu.Lock()
	lastData := state.lastDataAt
	m.mu.Unlock()
	if lastData.After(windowStart) {
		// 本轮读到过有效行：数据链路健康，清掉历史错误（如瞬断/无数据）
		m.noteSerialReaderError(state, nil)
	} else if serialNow().Sub(lastData) > serialStaleAfter {
		m.noteSerialReaderError(state, fmt.Errorf("%s: %d 秒无数据: %w", state.key, int(serialStaleAfter.Seconds()), errNoSerialData))
	}
}

// readSerialWindow 在 window 时长内持续读串口并解析行，窗口到期或 ctx 取消
// 返回。跨 Read 攒行（传感器一行可能分多个包到达），每行交给
// extractSerialTemperature，有效读数刷新 state.latest 与 state.lastDataAt。
// 单次 Read 的 deadline 设为窗口截止：不支持 deadline 的内核由 VTIME 兜底。
func (m *Manager) readSerialWindow(ctx context.Context, port serialPort, state *serialReaderState, window time.Duration) error {
	deadline := serialNow().Add(window)
	var pending []byte
	buf := make([]byte, 256)
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		if remaining := deadline.Sub(serialNow()); remaining <= 0 {
			return nil
		} else if err := port.SetReadDeadline(deadline); err != nil {
			// 个别内核不支持 deadline：依赖 VTIME 兜底（usb_serial_linux.go）
			_ = err
		}
		n, err := port.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			for {
				idx := bytes.IndexAny(pending, "\r\n")
				if idx < 0 {
					break
				}
				line := string(pending[:idx])
				pending = pending[idx+1:]
				if celsius, ok := extractSerialTemperature(line); ok {
					now := serialNow()
					m.mu.Lock()
					state.latest = serialReading{Key: state.key, Celsius: celsius, At: now}
					state.lastDataAt = now
					m.mu.Unlock()
				}
			}
			// 迟迟不见换行的脏数据（波特率不匹配常见乱码）防积压
			if len(pending) > 4096 {
				pending = pending[:0]
			}
		}
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue // 窗口内的行间静默属正常：继续等下一包
			}
			if n == 0 {
				return fmt.Errorf("%s: read: %w", state.key, err)
			}
			// 已带回部分数据的错（EOF 常见带尾巴）：本行处理完，错误照报
			return fmt.Errorf("%s: read: %w", state.key, err)
		}
		if n == 0 {
			// 真实内核的 VMIN=0/VTIME 到期会阻塞约 0.5s 再返回；个别驱动
			// 立即返回 0 字节，小睡防忙转
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// errNoSerialData 打开成功但跨轮持续无数据的失效原因，与 IO 错误区分开。
var errNoSerialData = errors.New("no data")

// serialTempPattern 数字片段（允许正负号与小数；ParseFloat 兼容 "+25.6"）。
// 取值时再过滤合理区间，因为 "0 25.6"（地址+温度）这类行首数字不是温度。
var serialTempPattern = regexp.MustCompile(`[+-]?\d+(?:\.\d+)?`)

// extractSerialTemperature 取行内第一个落在 [-55,125] 的数字当摄氏温度。
// 区间取 DS18B20 的物理量程：真机固件按带符号小数输出（stty 4800 下
// `cat /dev/ttyUSB0` 得 "+25.6"/"-3.0"），负温是合法值；-127（DS18B20 断连
// 哨兵）与区间外的 raw/错位值（1256、999）一并拒绝。恰好 0.0 也拒：DIY
// 固件常用 0 做错误哨兵，且"地址+温度"行式（"0 25.6"）的行首 0 不是温度。
func extractSerialTemperature(line string) (float64, bool) {
	for _, match := range serialTempPattern.FindAllString(line, -1) {
		value, err := strconv.ParseFloat(match, 64)
		if err == nil && value != 0 && value >= -55 && value <= 125 {
			return value, true
		}
	}
	return 0, false
}

// serialSensorKey 由设备路径派生稳定传感器键（usb:tty:<名称>）。by-id 路径
// 末段对同一物理设备稳定；/dev/ttyUSB* 编号会随插拔漂移，设备下拉已优先
// 展示 by-id。名称截断到 32 字符，与 usbTempLabel 的约束一致。
func serialSensorKey(path string) string {
	label := filepath.Base(path)
	if len(label) > 32 {
		label = label[:32]
	}
	return "usb:tty:" + label
}

// serialSensorConfigsLocked 返回当前串口配置数组（原样，含停用行）。调用方
// 须持 m.mu。
func (m *Manager) serialSensorConfigsLocked() []SerialSensorConfig {
	cfg, err := m.loadConfigLocked()
	if err != nil {
		return nil
	}
	return cfg.Serials
}

// serialTemperatureReadingsLocked 返回全部仍新鲜的串口读数（供
// extraTemperatures 并入采样链）。停止输出超过 serialStaleAfter 即失效——
// 采样链不展示冻结值，曲线在传感器拔掉后自然断线。调用方须持 m.mu
// （extraTemperatures 的调用条件；内部再取锁会与 Status() 的持锁重入死锁）。
func (m *Manager) serialTemperatureReadingsLocked() []Temperature {
	var result []Temperature
	for _, state := range m.serialReaders {
		if state.latest.At.IsZero() || serialNow().Sub(state.latest.At) > serialStaleAfter {
			continue
		}
		result = append(result, Temperature{Label: state.latest.Key, Celsius: state.latest.Celsius})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Label < result[j].Label })
	return result
}

func (m *Manager) setSerialReaderOpen(state *serialReaderState, open bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state.open = open
}

// noteSerialReaderError 记录/清除一个读取器的错误，错误变化时打一条日志
// （与 noteUSBTempError 同款：每分钟重连不刷屏，恢复即清空）。
func (m *Manager) noteSerialReaderError(state *serialReaderState, err error) {
	message := ""
	if err != nil {
		message = err.Error()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if message == state.lastError {
		return
	}
	state.lastError = message
	if err != nil {
		m.logf("serial temperature sensor read failed: %v (logging once until the error changes)", err)
	}
}

// serialSleep 在 ctx 取消或超时后返回；ctx 已结束返回 false。
func serialSleep(ctx context.Context, wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// kickSerialReader 非阻塞通知监督者重新对账（增删读取器）；未运行时无事
// 发生。
func (m *Manager) kickSerialReader() {
	m.mu.Lock()
	kick := m.serialKick
	m.mu.Unlock()
	if kick == nil {
		return
	}
	select {
	case kick <- struct{}{}:
	default:
	}
}

// SaveSerialSensorConfigs 校验并保存整组串口传感器配置，踢监督者立即按新
// 配置增删读取器（写盘走 saveConfigLocked，自动记 "config.json saved" 运行
// 日志）。
func (m *Manager) SaveSerialSensorConfigs(configs []SerialSensorConfig) error {
	configs, err := NormalizeSerialSensorConfigs(configs)
	if err != nil {
		return err
	}
	if err := m.rejectSerialPhysicalDuplicates(configs); err != nil {
		return err
	}
	m.mu.Lock()
	current, err := m.loadConfigLocked()
	if err != nil {
		m.lastError = err.Error()
		m.mu.Unlock()
		return err
	}
	current.Serials = configs
	current.Serial = nil // 旧单设备字段清空，落盘即完成新格式迁移
	if err := m.saveConfigLocked(current); err != nil {
		m.lastError = err.Error()
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()
	m.kickSerialReader()
	return nil
}

// SerialSensorConfigStatus 一个配置行的运行态，嵌入配置本体的 JSON 字段。
// LastAt 用指针：time.Time 的 omitempty 挡不住零值结构体，无读数时会下发
// 0001-01-01，前端一减渲染成"63927157204 秒前"。
type SerialSensorConfigStatus struct {
	SerialSensorConfig
	Open        bool       `json:"open"`
	Key         string     `json:"key,omitempty"`
	LastCelsius float64    `json:"last_celsius,omitempty"`
	LastAt      *time.Time `json:"last_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
}

// SerialSensorInfo 是随 /api/status 下发的串口传感器整体状态。
type SerialSensorInfo struct {
	Configs []SerialSensorConfigStatus `json:"configs"`
	Devices []SerialDeviceInfo         `json:"devices"`
}

// SerialDeviceInfo 是设备下拉里的一个候选串口。
type SerialDeviceInfo struct {
	Path  string `json:"path"`
	Label string `json:"label"`
}

// serialStatusLocked 汇总串口传感器整体状态（配置行含各自运行态 + 候选设
// 备枚举）。调用方须持 m.mu（与 Status() 一致）；设备枚举是纯 sysfs/目录
// 读取，与 Status 现有的 DiscoverPackages 同量级开销。
func (m *Manager) serialStatusLocked(configs []SerialSensorConfig) SerialSensorInfo {
	info := SerialSensorInfo{Devices: m.serialDeviceCandidates()}
	for _, cfg := range configs {
		item := SerialSensorConfigStatus{SerialSensorConfig: cfg, Key: serialSensorKey(cfg.Path)}
		if state, running := m.serialReaders[cfg.Path]; running {
			item.Open = state.open
			item.LastError = state.lastError
			if !state.latest.At.IsZero() {
				item.LastCelsius = state.latest.Celsius
				at := state.latest.At
				item.LastAt = &at
			}
		}
		info.Configs = append(info.Configs, item)
	}
	return info
}

// serialDeviceCandidates 枚举可用的候选串口，按稳定性排序展示：by-id（名
// 字含 VID:PID 与序列号，插拔编号不漂移）→ by-path（按物理插口，多个同型
// 号无序列号适配器唯一的稳定区分）→ /sys/class/tty 的 ttyUSB*/ttyACM*
// （有 device 子目录 = 真实硬件，排除板载 ttyS* 虚拟口）。同一物理设备会在
// 多个入口重复出现，指向同一硬件，选稳定的入口即可。
func (m *Manager) serialDeviceCandidates() []SerialDeviceInfo {
	var result []SerialDeviceInfo
	seen := make(map[string]bool)
	add := func(path, label string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		result = append(result, SerialDeviceInfo{Path: path, Label: label})
	}
	byID, _ := os.ReadDir(m.rooted("/dev/serial/by-id"))
	for _, entry := range byID {
		name := entry.Name()
		label := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(name, "usb-"), "-if00-port0"), "-if00")
		if label == "" {
			label = name
		}
		add(m.rooted(filepath.Join("/dev/serial/by-id", name)), label)
	}
	byPath, _ := os.ReadDir(m.rooted("/dev/serial/by-path"))
	for _, entry := range byPath {
		name := entry.Name()
		add(m.rooted(filepath.Join("/dev/serial/by-path", name)), name)
	}
	for _, pattern := range []string{"/sys/class/tty/ttyUSB*", "/sys/class/tty/ttyACM*"} {
		nodes, _ := filepath.Glob(m.rooted(pattern))
		sort.Strings(nodes)
		for _, node := range nodes {
			if _, err := os.Stat(filepath.Join(node, "device")); err != nil {
				continue
			}
			name := filepath.Base(node)
			add(m.rooted(filepath.Join("/dev", name)), name)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Label < result[j].Label })
	return result
}
