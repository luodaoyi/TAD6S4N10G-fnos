package powerguard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---- 提取与键名 ----

func TestExtractSerialTemperature(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		want  float64
		valid bool
	}{
		{"bare", "25.6", 25.6, true},
		{"prefixed", "temp:25.6", 25.6, true},
		{"explicit plus", "+25.6", 25.6, true},
		{"negative", "-3.0", -3.0, true},
		{"json", `{"temp":25.63,"hum":50.1}`, 25.63, true},
		{"integer", "T=27", 27, true},
		// 行首地址/序号不在量程或为 0 时跳过，取真正的温度
		{"zero address then temp", "0 25.6", 25.6, true},
		// 已知局限：行首序号本身落在量程内会被当作温度
		{"address then temp", "1 25.6", 1, true},
		{"humidity first", "50,25.6", 50, true},
		// 区间外与哨兵一律拒绝：raw 值、DS18B20 断连 -127、恰好 0.0
		{"zero sentinel", "0.0", 0, false},
		{"disconnected sentinel", "-127.0", 0, false},
		{"below ds18b20 range", "-60.5", 0, false},
		{"raw centi", "1256", 0, false},
		{"over range", "temp:999", 0, false},
		{"no number", "OK", 0, false},
		{"empty", "", 0, false},
	}
	for _, test := range tests {
		got, valid := extractSerialTemperature(test.line)
		if valid != test.valid || (valid && got != test.want) {
			t.Fatalf("%s: extractSerialTemperature(%q)=(%v,%v), want (%v,%v)",
				test.name, test.line, got, valid, test.want, test.valid)
		}
	}
}

func TestSerialSensorKey(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/dev/ttyUSB0", "usb:tty:ttyUSB0"},
		{"/dev/serial/by-id/usb-1a86_USB_Serial-if00-port0", "usb:tty:usb-1a86_USB_Serial-if00-port0"},
		// 超长名称截断到 32 字符，避免传感器键过长（与 usbTempLabel 一致）
		{"/dev/serial/by-id/usb-very-long-name-0123456789abcdef0123456789", "usb:tty:usb-very-long-name-0123456789abc"},
	}
	for _, test := range tests {
		if got := serialSensorKey(test.path); got != test.want {
			t.Fatalf("serialSensorKey(%q)=%q, want %q", test.path, got, test.want)
		}
	}
}

// ---- 配置校验 ----

func TestNormalizeSerialSensorConfigs(t *testing.T) {
	valid := []SerialSensorConfig{
		{Enabled: true, Path: "/dev/ttyUSB0"},
		{Enabled: true, Path: "/dev/ttyACM1", Baud: 4800},
		{Enabled: false, Path: "/dev/ttyUSB2", Baud: 115200}, // 停用行保留已填值
	}
	got, err := NormalizeSerialSensorConfigs(valid)
	if err != nil {
		t.Fatalf("NormalizeSerialSensorConfigs()=%v", err)
	}
	if len(got) != 3 || got[0].Baud != serialDefaultBaud || got[1].Baud != 4800 || got[2].Enabled {
		t.Fatalf("normalized=%+v", got)
	}

	// 空行（前端刚点添加还没选设备）丢弃，不参与重复判定
	got, err = NormalizeSerialSensorConfigs([]SerialSensorConfig{{}, {Enabled: true, Path: "/dev/ttyUSB0"}})
	if err != nil || len(got) != 1 || got[0].Path != "/dev/ttyUSB0" {
		t.Fatalf("empty rows must be dropped: (%+v,%v)", got, err)
	}

	bad := []struct {
		name    string
		configs []SerialSensorConfig
		wantErr string
	}{
		{"enabled without path", []SerialSensorConfig{{Enabled: true}}, "未选择设备"},
		{"non-dev path", []SerialSensorConfig{{Enabled: true, Path: "ttyUSB0", Baud: 9600}}, "/dev/"},
		{"bad baud", []SerialSensorConfig{{Enabled: true, Path: "/dev/ttyUSB0", Baud: 1200}}, "波特率"},
		{"duplicate path", []SerialSensorConfig{
			{Enabled: true, Path: "/dev/ttyUSB0", Baud: 9600},
			{Enabled: false, Path: "/dev/ttyUSB0", Baud: 4800},
		}, "重复"},
	}
	for _, test := range bad {
		_, err := NormalizeSerialSensorConfigs(test.configs)
		if err == nil || !strings.Contains(err.Error(), test.wantErr) {
			t.Fatalf("%s: err=%v, want contains %q", test.name, err, test.wantErr)
		}
	}

	// 行数上限
	over := make([]SerialSensorConfig, serialMaxDevices+1)
	for i := range over {
		over[i] = SerialSensorConfig{Enabled: true, Path: filepath.Join("/dev/tty", "x"+strconv.Itoa(i))}
	}
	if _, err := NormalizeSerialSensorConfigs(over); err == nil || !strings.Contains(err.Error(), "最多") {
		t.Fatalf("over-limit err=%v, want 最多", err)
	}
}

// ---- 旧格式迁移 ----

// 旧版 config.json 的单设备 serial 对象：读入即迁移进 serials 数组、旧字段
// 置 nil；任何一次保存都把新格式落盘，传感器键不变。
func TestConfigSerialMigration(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	seed := `{"serial":{"enabled":true,"path":"/dev/ttyUSB0","baud":4800}}`
	if err := os.WriteFile(configPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{ConfigPath: configPath}
	m.mu.Lock()
	cfg, err := m.loadConfigLocked()
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Serial != nil {
		t.Fatalf("legacy serial field must be cleared after load, got %+v", cfg.Serial)
	}
	if len(cfg.Serials) != 1 || cfg.Serials[0] != (SerialSensorConfig{Enabled: true, Path: "/dev/ttyUSB0", Baud: 4800}) {
		t.Fatalf("migrated serials=%+v", cfg.Serials)
	}
	// 保存（任意配置保存路径都会走 loadConfigLocked→normalizeConfig）后落盘新格式
	m.mu.Lock()
	err = m.saveConfigLocked(cfg)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(configPath)
	var persisted map[string]any
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted["serial"] != nil {
		t.Fatalf("legacy serial must be gone after save: %s", raw)
	}
	serials, ok := persisted["serials"].([]any)
	if !ok || len(serials) != 1 {
		t.Fatalf("serials array missing after save: %s", raw)
	}
}

// ---- 设备枚举 ----

func TestSerialDeviceCandidates(t *testing.T) {
	root := t.TempDir()
	mkdir := func(rel string) {
		if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mkdir("sys/class/tty/ttyUSB0/device") // 真实 USB 串口
	mkdir("sys/class/tty/ttyACM1/device") // CDC-ACM
	mkdir("sys/class/tty/ttyS0")          // 板载串口：无 device 子目录，应排除
	mkdir("dev/serial/by-id/usb-1a86_USB_Serial-if00-port0")
	mkdir("dev/serial/by-path/pci-0000:00:14.0-usb-0:3:1.0-port0")

	m := &Manager{Root: root}
	devices := m.serialDeviceCandidates()
	paths := make([]string, 0, len(devices))
	labels := make([]string, 0, len(devices))
	for _, device := range devices {
		paths = append(paths, device.Path)
		labels = append(labels, device.Label)
	}
	wantPaths := []string{
		filepath.Join(root, "/dev/serial/by-id/usb-1a86_USB_Serial-if00-port0"),
		filepath.Join(root, "/dev/serial/by-path/pci-0000:00:14.0-usb-0:3:1.0-port0"),
		filepath.Join(root, "/dev/ttyACM1"),
		filepath.Join(root, "/dev/ttyUSB0"),
	}
	wantLabels := []string{"1a86_USB_Serial", "pci-0000:00:14.0-usb-0:3:1.0-port0", "ttyACM1", "ttyUSB0"}
	if strings.Join(paths, "|") != strings.Join(wantPaths, "|") ||
		strings.Join(labels, "|") != strings.Join(wantLabels, "|") {
		t.Fatalf("serialDeviceCandidates()=(%v,%v), want (%v,%v)", paths, labels, wantPaths, wantLabels)
	}
}

// ---- 读取循环 ----

// fakeSerialPort 假串口：按序返回预置数据块，耗尽后转 readErr（或阻塞模拟
// 静默连接）。
type fakeSerialPort struct {
	reads    [][]byte
	readAt   int
	readErr  error
	blocks   chan struct{}
	deadline time.Time
	closed   bool
}

func (f *fakeSerialPort) Read(b []byte) (int, error) {
	if f.readAt < len(f.reads) {
		n := copy(b, f.reads[f.readAt])
		f.readAt++
		return n, nil
	}
	if f.readErr != nil {
		return 0, f.readErr
	}
	if f.blocks != nil {
		// 模拟真实驱动：阻塞中的 Read 也要遵守 deadline 到期返回，
		// 否则循环无法在读间隙观察到 ctx（真实 usb-serial 由 poll 保证）
		if !f.deadline.IsZero() {
			select {
			case <-f.blocks:
				return 0, errors.New("fake port force-closed")
			case <-time.After(time.Until(f.deadline)):
				return 0, os.ErrDeadlineExceeded
			}
		}
		<-f.blocks
		return 0, errors.New("fake port force-closed")
	}
	// VMIN=0/VTIME 到期的安静返回（无数据、无错误）
	return 0, nil
}

func (f *fakeSerialPort) SetReadDeadline(t time.Time) error { f.deadline = t; return nil }

func (f *fakeSerialPort) Close() error {
	f.closed = true
	if f.blocks != nil {
		close(f.blocks)
	}
	return nil
}

func newSerialState(path string) *serialReaderState {
	return &serialReaderState{path: path, key: serialSensorKey(path)}
}

func TestReadSerialWindowCollectsLines(t *testing.T) {
	m := &Manager{}
	state := newSerialState("/dev/ttyUSB0")
	port := &fakeSerialPort{
		// 一行拆多个包到达 + 多种行格式；末尾不留换行的残行不产生读数。
		// reads 耗尽后安静返回：窗口到期正常收尾，不报错。
		reads: [][]byte{[]byte("25."), []byte("6\r\ntemp:26.5\n"), []byte(`{"temp":27.25}` + "\n"), []byte("no newline tail")},
	}
	if err := m.readSerialWindow(context.Background(), port, state, 150*time.Millisecond); err != nil {
		t.Fatalf("readSerialWindow()=%v, want nil on window expiry", err)
	}
	if state.latest.Celsius != 27.25 || state.latest.Key != "usb:tty:ttyUSB0" {
		t.Fatalf("latest=(%q,%v), want (usb:tty:ttyUSB0,27.25)", state.latest.Key, state.latest.Celsius)
	}
}

// 跨轮无数据判定：打开成功但持续没有效行超过 serialStaleAfter 时记
// "无数据"错误；读到行即清。
func TestPollSerialOnceNoData(t *testing.T) {
	m := &Manager{serialReaders: map[string]*serialReaderState{}}
	state := newSerialState("/dev/ttyUSB0")
	state.lastDataAt = time.Now().Add(-2 * serialStaleAfter)
	m.serialReaders["/dev/ttyUSB0"] = state
	previousOpener := openSerialPort
	openSerialPort = func(string, int) (serialPort, error) { return &fakeSerialPort{}, nil }
	t.Cleanup(func() { openSerialPort = previousOpener })

	m.pollSerialOnce(context.Background(), SerialSensorConfig{Enabled: true, Path: "/dev/ttyUSB0"}, state)
	if !strings.Contains(state.lastError, errNoSerialData.Error()) {
		t.Fatalf("lastError=%q, want no-data error", state.lastError)
	}

	// 读到行后错误清除
	state.lastDataAt = time.Now()
	port := &fakeSerialPort{reads: [][]byte{[]byte("+21.5\n")}}
	openSerialPort = func(string, int) (serialPort, error) { return port, nil }
	m.pollSerialOnce(context.Background(), SerialSensorConfig{Enabled: true, Path: "/dev/ttyUSB0"}, state)
	if state.latest.Celsius != 21.5 || state.lastError != "" {
		t.Fatalf("after reading: latest=%v lastError=%q", state.latest.Celsius, state.lastError)
	}
}

// EOF（读取流挂断）按普通读错误上报：轮询模式下下一轮重开即自愈，无需
// 特殊通道；EOF 前已解析的读数保留。
func TestReadSerialWindowEOF(t *testing.T) {
	m := &Manager{}
	state := newSerialState("/dev/ttyUSB0")
	port := &fakeSerialPort{reads: [][]byte{[]byte("25.6\n")}, readErr: io.EOF}
	err := m.readSerialWindow(context.Background(), port, state, time.Second)
	if err == nil || !strings.Contains(err.Error(), "read:") {
		t.Fatalf("readSerialWindow() err=%v, want read error", err)
	}
	if state.latest.Celsius != 25.6 {
		t.Fatalf("EOF before error must keep stored reading, got %v", state.latest.Celsius)
	}
}

func TestReadSerialWindowCancelExits(t *testing.T) {
	m := &Manager{}
	state := newSerialState("/dev/ttyUSB0")
	port := &fakeSerialPort{reads: [][]byte{[]byte("23.4\n")}, blocks: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- m.readSerialWindow(ctx, port, state, time.Second) }()
	waitForSerialCondition(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return !state.latest.At.IsZero()
	})
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("cancel must end loop without error, got %v", err)
		}
	case <-time.After(6 * time.Second): // 阻塞中的 Read 最迟窗口截止后返回
		t.Fatal("cancel did not end readSerialWindow")
	}
}
func TestSaveSerialSensorConfigs(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{ConfigPath: configPath}
	m.mu.Lock()
	m.serialKick = make(chan struct{}, 1)
	m.serialReaders = map[string]*serialReaderState{}
	m.mu.Unlock()

	// 非法配置直接拒绝且不落盘
	if err := m.SaveSerialSensorConfigs([]SerialSensorConfig{{Enabled: true}}); err == nil {
		t.Fatal("enabled without path must fail")
	}
	// 合法保存：波特率归位默认值，kick 触发
	if err := m.SaveSerialSensorConfigs([]SerialSensorConfig{{Enabled: true, Path: "/dev/ttyUSB0"}}); err != nil {
		t.Fatalf("SaveSerialSensorConfigs()=%v", err)
	}
	m.mu.Lock()
	cfg, err := m.loadConfigLocked()
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Serials) != 1 || !cfg.Serials[0].Enabled || cfg.Serials[0].Baud != serialDefaultBaud {
		t.Fatalf("saved serials=%+v", cfg.Serials)
	}
	select {
	case <-m.serialKick:
	default:
		t.Fatal("save must kick the supervisor")
	}
}

// TestSaveSerialSensorConfigsPhysicalDuplicate 同一物理设备的多个入口
// （by-id 符号链接与裸 tty 节点解析到同一设备）必须拒绝保存：多个读取器
// 交错开口/关口同一 tty 会互相打断打开序列（真机出过三条目全部 tiocmset
// 失败）。其中一条停用即可放行。
func TestSaveSerialSensorConfigsPhysicalDuplicate(t *testing.T) {
	root := t.TempDir()
	devDir := filepath.Join(root, "dev")
	if err := os.MkdirAll(filepath.Join(devDir, "serial", "by-id"), 0o755); err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(devDir, "ttyUSB0")
	if err := os.WriteFile(node, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../ttyUSB0", filepath.Join(devDir, "serial", "by-id", "usb-1a86_USB_Serial-if00-port0")); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{ConfigPath: configPath, Root: root}
	m.mu.Lock()
	m.serialKick = make(chan struct{}, 1)
	m.serialReaders = map[string]*serialReaderState{}
	m.mu.Unlock()

	err := m.SaveSerialSensorConfigs([]SerialSensorConfig{
		{Enabled: true, Path: "/dev/ttyUSB0"},
		{Enabled: true, Path: "/dev/serial/by-id/usb-1a86_USB_Serial-if00-port0"},
	})
	if err == nil || !strings.Contains(err.Error(), "同一设备") {
		t.Fatalf("physical duplicate must be rejected, got %v", err)
	}
	if err := m.SaveSerialSensorConfigs([]SerialSensorConfig{
		{Enabled: true, Path: "/dev/ttyUSB0"},
		{Enabled: false, Path: "/dev/serial/by-id/usb-1a86_USB_Serial-if00-port0"},
	}); err != nil {
		t.Fatalf("disabled duplicate row must be allowed: %v", err)
	}
	// 两个不同物理设备（ttyUSB0/ttyUSB1）互不影响
	if err := os.WriteFile(filepath.Join(devDir, "ttyUSB1"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.SaveSerialSensorConfigs([]SerialSensorConfig{
		{Enabled: true, Path: "/dev/ttyUSB0"},
		{Enabled: true, Path: "/dev/ttyUSB1"},
	}); err != nil {
		t.Fatalf("distinct devices must be allowed: %v", err)
	}
}

// ---- 监督者与读取器 ----

func TestSerialSensorLoopServesAndDisables(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	seed := `{"serials":[{"enabled":true,"path":"/dev/ttyUSB0","baud":4800},{"enabled":true,"path":"/dev/ttyACM1","baud":9600}]}`
	if err := os.WriteFile(configPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{ConfigPath: configPath}
	ports := map[string]*fakeSerialPort{
		"/dev/ttyUSB0": {reads: [][]byte{[]byte("23.4\n")}, blocks: make(chan struct{})},
		"/dev/ttyACM1": {reads: [][]byte{[]byte("+18.7\n")}, blocks: make(chan struct{})},
	}
	previousOpener := openSerialPort
	openSerialPort = func(path string, baud int) (serialPort, error) {
		want := map[string]int{"/dev/ttyUSB0": 4800, "/dev/ttyACM1": 9600}[path]
		if want == 0 {
			t.Errorf("openSerialPort(%q,%d): unexpected device", path, baud)
			return nil, errors.New("unexpected")
		}
		if baud != want {
			t.Errorf("openSerialPort(%q,%d), want baud %d", path, baud, want)
		}
		return ports[path], nil
	}
	t.Cleanup(func() { openSerialPort = previousOpener })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		m.SerialSensorLoop(ctx, nil)
		close(done)
	}()
	// 两个读取器各自读出温度（间歇轮询下 serialOpen 是瞬态，以读取器
	// 存在与读数为达成条件）
	waitForSerialCondition(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.serialReaders) == 2 &&
			serialReaderCelsius(m, "/dev/ttyUSB0") == 23.4 &&
			serialReaderCelsius(m, "/dev/ttyACM1") == 18.7
	})

	// 防死锁回归：Status() 持 m.mu 调 extraTemperatures → 串口读数检查。
	// 若读数检查内部再取 m.mu（不可重入），这里会永久挂起。
	m.mu.Lock()
	extra := m.extraTemperatures()
	m.mu.Unlock()
	if !containsTemperature(extra, "usb:tty:ttyUSB0", 23.4) ||
		!containsTemperature(extra, "usb:tty:ttyACM1", 18.7) {
		t.Fatalf("extraTemperatures()=%+v, want both usb:tty readings", extra)
	}

	// 停用一个：只有 ttyUSB0 的读取器退出，另一个照常
	if err := m.SaveSerialSensorConfigs([]SerialSensorConfig{
		{Enabled: false, Path: "/dev/ttyUSB0", Baud: 4800},
		{Enabled: true, Path: "/dev/ttyACM1", Baud: 9600},
	}); err != nil {
		t.Fatalf("SaveSerialSensorConfigs()=%v", err)
	}
	waitForSerialCondition(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		_, usb := m.serialReaders["/dev/ttyUSB0"]
		_, acm := m.serialReaders["/dev/ttyACM1"]
		return !usb && acm
	})

	// 全部停用：监督者进入空转，读取器清零
	if err := m.SaveSerialSensorConfigs(nil); err != nil {
		t.Fatalf("SaveSerialSensorConfigs()=%v", err)
	}
	waitForSerialCondition(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.serialReaders) == 0
	})

	cancel()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("loop did not exit after cancel")
	}
}

func serialReaderCelsius(m *Manager, path string) float64 {
	state, ok := m.serialReaders[path]
	if !ok {
		return 0
	}
	return state.latest.Celsius
}

func containsTemperature(list []Temperature, label string, celsius float64) bool {
	for _, item := range list {
		if item.Label == label && item.Celsius == celsius {
			return true
		}
	}
	return false
}

func waitForSerialCondition(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second) // 覆盖阻塞 Read 的 deadline 周期
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
