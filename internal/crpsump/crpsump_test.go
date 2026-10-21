package crpsump

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain 把运行日志重定向到临时目录, 避免单测触发 Logf 时污染真实 LogPath。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "crpsump-test")
	if err != nil {
		panic(err)
	}
	LogPath = filepath.Join(dir, "psu-monitor-test.log")
	code := m.Run()
	_ = os.RemoveAll(dir) // os.Exit 不跑 defer, 显式清理
	os.Exit(code)
}

// ---------------- 硬件 mock ----------------

// mockPIO 用内存模拟端口空间, 记录 outb 次数 (单测不依赖实机 /dev/port)。
// readMask 模拟输入方向时由外部线路 (上拉/从机) 驱动的电平: 设置后 inb 恒返回该值,
// 否则读回的是主控最后写出的电平, 无法模拟从机 ACK。
type mockPIO struct {
	mu             sync.Mutex
	mem            map[int]byte
	readMask       map[int]byte
	failOutb       bool           // true 时 outb 恒失败 (测试端口写错误向上传播)
	failOutbOnPort int            // 非负时仅该端口 outb 失败 (测试局部写失败, 如 sioWrite 数据口)
	failInb        bool           // true 时 inb 恒失败 (测试端口读错误向上传播)
	failWhen       map[int64]bool // 定点故障注入: key=port<<8|v, 命中一次 outb(port,v) 失败后自动清除
	failOnNth      map[int64]int  // 第 N 次 outb(port,v) 才失败 (同值写多次时精确打击第 N 次)
	matchCount     map[int64]int  // failOnNth 的已匹配计数
	sioExits       int            // 记录 sioExit (outb 0x2E=0xAA) 执行次数
	writes         int
}

func newMockPIO() *mockPIO {
	return &mockPIO{mem: map[int]byte{}, readMask: map[int]byte{}, failOutbOnPort: -1,
		failWhen: map[int64]bool{}, failOnNth: map[int64]int{}, matchCount: map[int64]int{}}
}

// injectOutbFail 定点注入: 仅下一次 outb(port, v) 失败 (测试"仅方向寄存器索引
// 选择失败一次"等精确故障路径, 不影响同端口其他值的写入)
func (m *mockPIO) injectOutbFail(port int, v byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failWhen[int64(port)<<8|int64(v)] = true
}

// injectOutbFailOnNth 第 N 次 outb(port, v) 才注入失败 (如"复位末尾 SDA 释放"
// 与 initDirs 写同一值, 需跳过前几次命中精确打击最后一次)
func (m *mockPIO) injectOutbFailOnNth(port int, v byte, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failOnNth[int64(port)<<8|int64(v)] = n
}

// resetOutbCounts 清零 failOnNth 匹配计数 (测试准备阶段的写入不计入 N)
func (m *mockPIO) resetOutbCounts() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.matchCount = map[int64]int{}
}

func (m *mockPIO) inb(port int) (byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failInb {
		return 0, errors.New("mock: 端口读失败")
	}
	if v, ok := m.readMask[port]; ok {
		return v, nil
	}
	return m.mem[port], nil
}

func (m *mockPIO) outb(port int, v byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOutb || port == m.failOutbOnPort {
		return errors.New("mock: 端口写失败")
	}
	key := int64(port)<<8 | int64(v)
	if m.failWhen[key] {
		delete(m.failWhen, key) // 一次性: 命中后自动恢复, 后续同参数写入正常
		return errors.New("mock: 定点写失败")
	}
	if n, ok := m.failOnNth[key]; ok {
		m.matchCount[key]++
		if m.matchCount[key] == n { // 第 N 次同参数写入才失败 (前 N-1 次放行)
			return errors.New("mock: 定点写失败")
		}
	}
	if port == 0x2E && v == 0xAA {
		m.sioExits++
	}
	m.mem[port] = v
	m.writes++
	return nil
}

func (m *mockPIO) writeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writes
}

func (m *mockPIO) sioExitCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sioExits
}

// stubPortAvailable 替换 /dev/port 可用性探测 (Windows/无 root 环境下恒失败)
func stubPortAvailable(t *testing.T, v bool) {
	t.Helper()
	old := portAvailable
	portAvailable = func() bool { return v }
	t.Cleanup(func() { portAvailable = old })
}

// ---------------- 引脚校验 ----------------

func TestNewBusPinValidation(t *testing.T) {
	// 非法 SCL 名回退默认
	b := NewBus("Bogus", "Pin20", false)
	if scl, _ := b.pinNames(); scl != DefaultSCL {
		t.Fatalf("非法 SCL 应回退 %s, 得到 %s", DefaultSCL, scl)
	}
	// 非法 SDA 名回退默认
	b = NewBus("Pin19", "Bogus", false)
	if _, sda := b.pinNames(); sda != DefaultSDA {
		t.Fatalf("非法 SDA 应回退 %s, 得到 %s", DefaultSDA, sda)
	}
	// SCL/SDA 同针: SDA 回退默认, SCL 保留
	b = NewBus("Pin21", "Pin21", false)
	scl, sda := b.pinNames()
	if scl != "Pin21" || sda != DefaultSDA {
		t.Fatalf("同针时 SDA 应回退 %s, 得到 SCL=%s SDA=%s", DefaultSDA, scl, sda)
	}
}

// 未配置时 NewBus 不得触碰 GPIO 方向寄存器 (无 CRPS 硬件的机器启动不应改 GPIO)
func TestNewBusUnconfiguredSkipsDirectionWrites(t *testing.T) {
	stubPortAvailable(t, true)
	old := defaultPortIO
	defer func() { defaultPortIO = old }()
	m := newMockPIO()
	defaultPortIO = m

	NewBus("Pin19", "Pin20", false)
	if n := m.writeCount(); n != 0 {
		t.Fatalf("configured=false 不应写端口, 实际写 %d 次", n)
	}
	NewBus("Pin19", "Pin20", true)
	if n := m.writeCount(); n == 0 {
		t.Fatal("configured=true 应初始化 GPIO 方向, 实际未写")
	}
}

// /dev/port 不可达: ok=false 降级 no-gpio, 且不写方向寄存器
func TestNewBusPortUnavailableDegraded(t *testing.T) {
	stubPortAvailable(t, false)
	old := defaultPortIO
	defer func() { defaultPortIO = old }()
	m := newMockPIO()
	defaultPortIO = m
	b := NewBus("Pin19", "Pin20", true)
	if b.isOK() {
		t.Fatal("/dev/port 不可达时 ok 应为 false")
	}
	if n := m.writeCount(); n != 0 {
		t.Fatalf("硬件不可达时不应写方向寄存器, 实际写 %d 次", n)
	}
}

// 手动 10% 是固件回自动温控的触发值: setFan 须如实记为 auto
func TestSetFanManualFloorBecomesAuto(t *testing.T) {
	stubPortAvailable(t, true)
	b := NewBus("Pin19", "Pin20", false)
	mock := newMockPIO()
	mock.readMask[GPPins["Pin20"].Port] = 0 // 从机恒拉低 SDA: ACK 恒真, 数据读回 0
	b.pio = mock
	if ok, _ := b.reconfigure(nil, nil); !ok {
		t.Fatal("reconfigure 失败")
	}
	if !b.setFan(context.Background(), "manual", 10) {
		t.Fatal("setFan 应成功 (mock 总线 ACK 恒真)")
	}
	if m := b.fanModeNow(); m != "auto" {
		t.Fatalf("手动 10%% 模式应记为 auto, 得到 %s", m)
	}
	if !b.setFan(context.Background(), "manual", 50) {
		t.Fatal("setFan 应成功 (mock 总线 ACK 恒真)")
	}
	if m := b.fanModeNow(); m != "manual" {
		t.Fatalf("手动 50%% 模式应记为 manual, 得到 %s", m)
	}
}

func TestSwitchPinRejectsInvalid(t *testing.T) {
	b := NewBus("Pin19", "Pin20", false)
	if ok, _ := b.switchPin("Nope", "Pin20"); ok {
		t.Fatal("未知 SCL 引脚应被拒绝")
	}
	if ok, _ := b.switchPin("Pin19", "Nope"); ok {
		t.Fatal("未知 SDA 引脚应被拒绝")
	}
	if ok, _ := b.switchPin("Pin21", "Pin21"); ok {
		t.Fatal("SCL/SDA 同针应被拒绝")
	}
}

func TestReconfigure(t *testing.T) {
	stubPortAvailable(t, true)
	b := NewBus("Pin19", "Pin20", false)
	mock := newMockPIO()
	b.pio = mock

	// 非法针被拒, 且保持未配置状态
	bad := "Nope"
	if ok, _ := b.reconfigure(&bad, nil); ok {
		t.Fatal("reconfigure 应拒绝非法引脚")
	}
	if b.isConfigured() {
		t.Fatal("非法 reconfigure 后不应标记为已配置")
	}
	// 合法保存: 标记已配置并写方向寄存器
	if ok, msg := b.reconfigure(nil, nil); !ok {
		t.Fatalf("合法 reconfigure 应成功: %s", msg)
	}
	if !b.isConfigured() {
		t.Fatal("合法 reconfigure 后应标记为已配置")
	}
	if mock.writeCount() == 0 {
		t.Fatal("reconfigure 应写入 GPIO 方向寄存器")
	}
}

// ---------------- PMBus 解码 ----------------

func TestLinear11(t *testing.T) {
	cases := []struct {
		raw  uint16
		want float64
	}{
		{0xF9CC, 230.0}, // exp=-1, mant=460  (典型 READ_VIN)
		{0x02D0, 720.0}, // exp=0,  mant=720
		{0xEFE8, -3.0},  // exp=-3, mant=-24  (负尾数)
		{0x0000, 0.0},
	}
	for _, c := range cases {
		if got := linear11(c.raw); got != c.want {
			t.Errorf("linear11(0x%04X) = %v, 期望 %v", c.raw, got, c.want)
		}
	}
}

func TestDecodeVout(t *testing.T) {
	u16 := func(v uint16) *uint16 { return &v }
	// VOUT_MODE=0x17 -> Linear 模式 exp=-9; raw=6144 -> 6144*2^-9 = 12.0
	if got := decodeVout(u16(0x17), u16(6144)); got == nil || *got != 12.0 {
		t.Fatalf("decodeVout(0x17, 6144) = %v, 期望 12.0", got)
	}
	// bit7=1 非 Linear 模式 -> nil
	if got := decodeVout(u16(0x97), u16(6144)); got != nil {
		t.Fatalf("非 Linear 模式应返回 nil, 得到 %v", *got)
	}
	// raw 缺失 -> nil
	if got := decodeVout(u16(0x17), nil); got != nil {
		t.Fatal("raw 为 nil 应返回 nil")
	}
}

func TestCRC8PEC(t *testing.T) {
	// CRC-8/SMBus (poly 0x07) 标准校验向量: "123456789" -> 0xF4
	if got := crc8('1', []byte("23456789")); got != 0xF4 {
		t.Fatalf("crc8 校验向量 = 0x%02X, 期望 0xF4", got)
	}
}

// ---------------- 耗电统计 ----------------

func TestEnergySampleAndReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "energy.json")
	e := NewEnergy(path)

	// 100W 采样 15s -> 100 * 15/3600 ≈ 0.417 Wh (round3)
	e.Sample(100, 15*time.Second)
	snap := e.Snapshot()
	total, _ := snap["total_wh"].(float64)
	if math.Abs(total-0.417) > 0.001 {
		t.Fatalf("积分结果 total_wh=%v, 期望 ≈0.417", total)
	}
	if today, _ := snap["today_wh"].(float64); today != total {
		t.Fatalf("today_wh=%v 应等于 total_wh=%v", today, total)
	}
	if pin, _ := snap["pin_w"].(float64); pin != 100 {
		t.Fatalf("pin_w=%v, 期望 100", pin)
	}
	// 间隔超过 EnergyMaxGap: 只刷新时间基准, 不计电量
	e.Sample(100, EnergyMaxGap+time.Minute)
	total2, _ := e.Snapshot()["total_wh"].(float64)
	if total2 != total {
		t.Fatalf("超限间隔不应积分: %v -> %v", total, total2)
	}
	// Reset 清零并落盘
	e.Reset()
	snap = e.Snapshot()
	if v, _ := snap["total_wh"].(float64); v != 0 {
		t.Fatalf("Reset 后 total_wh=%v, 期望 0", v)
	}
	if days, _ := snap["days"].([]map[string]any); len(days) != 0 {
		t.Fatalf("Reset 后按日明细应为空, 得到 %d 条", len(days))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Reset 应落盘 energy.json: %v", err)
	}
}

// ---------------- 端口写失败传播 ----------------

// 端口写失败必须向上传播: reconfigure 报失败, 读返回 nil, 风扇写返回 false,
// 不允许静默成功 (方向缓存也不得更新)
func TestPortWriteFailurePropagates(t *testing.T) {
	stubPortAvailable(t, true)
	b := NewBus("Pin19", "Pin20", false)
	mock := newMockPIO()
	mock.readMask[GPPins["Pin20"].Port] = 0
	b.pio = mock
	mock.failOutb = true
	if ok, msg := b.reconfigure(nil, nil); ok {
		t.Fatal("端口写失败时 reconfigure 应报失败")
	} else if !strings.Contains(msg, "GPIO") {
		t.Fatalf("reconfigure 失败信息应说明 GPIO 写入失败, 实际: %s", msg)
	}
	if b.isConfigured() {
		t.Fatal("reconfigure 失败不得标记 configured")
	}
	if w := b.readWord(Cmd["READ_VIN"]); w != nil {
		t.Fatal("端口写失败时 readWord 应返回 nil")
	}
	if b.setFan(context.Background(), "manual", 50) {
		t.Fatal("端口写失败时 setFan 应返回 false")
	}
	if b.sclOut || b.sdaOut {
		t.Fatal("方向写失败不得更新方向缓存")
	}
}

// setPinDir 中途 sioWrite 失败也必须执行 sioExit 退出 SuperIO 配置模式
func TestSetPinDirExitsSioOnWriteError(t *testing.T) {
	mock := newMockPIO()
	mock.failOutbOnPort = 0x2F // sioWrite 数据口写全部失败
	b := NewBus("Pin19", "Pin20", false)
	b.pio = mock
	if err := b.setPinDir("Pin19", b.sclCfg, true); err == nil {
		t.Fatal("sioWrite 失败应返回错误")
	}
	if mock.sioExitCount() == 0 {
		t.Fatal("写失败时也必须 sioExit, 否则 SuperIO 滞留配置模式")
	}
}

// 方向寄存器索引选择 (outb 0x2E=DirReg) 单次失败: setPinDir 必须报错且禁止写回。
// 失败后数据口 0x2F 残留的是上一步 LDN 选择的旧值 0x07, 若忽略错误继续 inb 并
// 写回, 就会把 0x07 当方向值污染 0xC9——这正是要防的同组针脚方向破坏路径
func TestSioReadIndexSelectErrorNoWriteback(t *testing.T) {
	b := NewBus("Pin19", "Pin20", false)
	mock := newMockPIO()
	b.pio = mock
	mock.injectOutbFail(0x2E, byte(GPPins["Pin19"].DirReg)) // 仅索引选择失败一次
	if err := b.setPinDir("Pin19", b.sclCfg, true); err == nil {
		t.Fatal("索引选择失败必须返回错误")
	}
	if v := mock.mem[0x2F]; v != 0x07 {
		t.Fatalf("方向数据不得写回数据口, mem[0x2F]=%#x (期望仅 LDN 选择残留值 0x07)", v)
	}
	if mock.mem[GPPins["Pin19"].DirReg] != 0 {
		t.Fatalf("方向寄存器 0x%02X 不得被写入, 实际 %#x", GPPins["Pin19"].DirReg, mock.mem[GPPins["Pin19"].DirReg])
	}
	if b.isDriven("Pin19") {
		t.Fatal("读失败不得更新 driven 追踪")
	}
}

// 方向寄存器数据口读取 (inb 0x2F) 失败: setPinDir 必须报错, 禁止写回方向寄存器、
// 禁止更新方向缓存与 driven 追踪
func TestSioReadInbErrorNoWriteback(t *testing.T) {
	b := NewBus("Pin19", "Pin20", false)
	mock := newMockPIO()
	mock.failInb = true
	b.pio = mock
	if err := b.setPinDir("Pin19", b.sclCfg, true); err == nil {
		t.Fatal("方向寄存器读取失败必须返回错误")
	}
	if v := mock.mem[0x2F]; v != 0x07 {
		t.Fatalf("读失败时方向数据不得写回, mem[0x2F]=%#x (期望仅 LDN 选择值 0x07)", v)
	}
	if b.isDriven("Pin19") {
		t.Fatal("读失败不得更新 driven 追踪")
	}
	if b.sclOut {
		t.Fatal("读失败不得更新方向缓存")
	}
	if mock.sioExitCount() == 0 {
		t.Fatal("读失败时也必须 sioExit, 否则 SuperIO 滞留配置模式")
	}
}

// 切针时释放旧引脚失败: 必须报失败且不覆盖引脚配置, 旧脚保持 driven 追踪
// (下次切换重试释放), 绝不报成功却把旧脚遗忘在输出状态
func TestSwitchPinReleaseFailureNoSuccess(t *testing.T) {
	stubPortAvailable(t, true)
	b := NewBus("Pin19", "Pin20", false)
	mock := newMockPIO()
	mock.readMask[GPPins["Pin20"].Port] = 0
	b.pio = mock
	if ok, _ := b.reconfigure(nil, nil); !ok { // Pin19/Pin20, driven={Pin19}
		t.Fatal("reconfigure 失败")
	}
	mock.injectOutbFail(0x2E, byte(GPPins["Pin19"].DirReg)) // 释放 Pin19 的索引选择失败
	ok, msg := b.switchPin("Pin21", "Pin22")
	if ok {
		t.Fatal("释放旧引脚失败时 switchPin 不得报成功")
	}
	if !strings.Contains(msg, "释放旧引脚 Pin19") {
		t.Fatalf("错误信息应指明释放失败的旧引脚, 实际: %s", msg)
	}
	if scl, sda := b.pinNames(); scl != "Pin19" || sda != "Pin20" {
		t.Fatalf("释放失败不得覆盖引脚配置, 得到 SCL=%s SDA=%s", scl, sda)
	}
	if !b.isDriven("Pin19") {
		t.Fatal("旧脚必须保持 driven 追踪, 待下次切换重试释放")
	}
}

// initDirs 第二步 (SDA 释放) 失败: 必须回滚第一步已设为输出的 SCL, 不遗留
// 半初始化输出脚; 回滚也失败时 SCL 保持 driven 追踪待重试 (可重试清理状态)
func TestInitDirsPartialFailureRollback(t *testing.T) {
	// 回滚成功路径: SCL 释放回输入, driven 清空, 方向缓存同步回退
	b := NewBus("Pin19", "Pin20", false)
	mock := newMockPIO()
	b.pio = mock
	mock.injectOutbFail(0x2E, byte(GPPins["Pin20"].DirReg)) // SDA 方向寄存器索引选择失败
	if err := b.initDirs(); err == nil {
		t.Fatal("SDA 方向设置失败时 initDirs 应返回错误")
	}
	if b.isDriven("Pin19") {
		t.Fatal("initDirs 失败应回滚 SCL (driven 不再含 Pin19)")
	}
	if b.sclOut {
		t.Fatal("initDirs 失败应回滚 SCL 方向缓存")
	}
	if mock.mem[0x2F] != 0x03 {
		t.Fatalf("SCL 方向寄存器应已释放 bit2 (0x07&^0x04=0x03), 实际 %#x (mock 数据口 0x2F)",
			mock.mem[0x2F])
	}

	// 回滚失败路径: SCL 保持 driven 追踪, 后续 switchPin 会重试释放, 不丢失清理状态
	b2 := NewBus("Pin19", "Pin20", false)
	mock2 := newMockPIO()
	b2.pio = mock2
	mock2.injectOutbFail(0x2E, byte(GPPins["Pin20"].DirReg)) // SDA 失败 -> 触发回滚
	mock2.injectOutbFail(0x2F, 0x03)                         // 回滚写回值 (0x07&^0x04=0x03) 失败
	if err := b2.initDirs(); err == nil {
		t.Fatal("initDirs 应返回错误")
	}
	if !b2.isDriven("Pin19") {
		t.Fatal("回滚失败时 SCL 应保持 driven 追踪待重试释放")
	}
}

// detect 接口: 找到接线但 reconfigure 失败时必须回 ok:false + apply-failed,
// 不得无视应用失败无条件报成功
func TestDetectApplyFailure(t *testing.T) {
	stubAdmin(t, true)
	stubPortAvailable(t, true)
	b := NewBus("Pin19", "Pin20", false)
	mock := newMockPIO()
	mock.failOutb = true // 方向写全失败: 应用必败
	b.pio = mock
	srv := &AppServer{Bus: b, DetectFn: func(context.Context) (string, string, error) { return "Pin19", "Pin20", nil }}
	req := httptest.NewRequest(http.MethodPost, "/api/crps/detect", nil)
	rec := httptest.NewRecorder()
	srv.APIHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200, 得到 %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"found":true`) || !strings.Contains(body, `"ok":false`) {
		t.Fatalf("应 found=true 且 ok=false, 得到 %s", body)
	}
	if !strings.Contains(body, "apply-failed") {
		t.Fatalf("应含 apply-failed 错误码, 得到 %s", body)
	}
	if b.isConfigured() {
		t.Fatal("应用失败不得标记 configured")
	}
}

// detect 接口: 应用成功但配置写盘失败时必须回 saved:false 并附告警,
// 不能只记服务端日志让界面误以为已持久化
func TestDetectSaveFailure(t *testing.T) {
	stubAdmin(t, true)
	stubPortAvailable(t, true)
	b := NewBus("Pin19", "Pin20", false)
	mock := newMockPIO()
	mock.readMask[GPPins["Pin20"].Port] = 0
	b.pio = mock
	srv := &AppServer{
		Bus:        b,
		ConfigPath: filepath.Join(t.TempDir(), "no-such-dir", "crps.json"), // 目录不存在: 写盘必败
		DetectFn:   func(context.Context) (string, string, error) { return "Pin19", "Pin20", nil },
	}
	req := httptest.NewRequest(http.MethodPost, "/api/crps/detect", nil)
	rec := httptest.NewRecorder()
	srv.APIHandler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `"ok":true`) || !strings.Contains(body, `"saved":false`) {
		t.Fatalf("应 ok=true 且 saved=false, 得到 %s", body)
	}
	if !strings.Contains(body, "保存失败") {
		t.Fatalf("应把保存失败明确返回给界面, 得到 %s", body)
	}
	if !b.isConfigured() {
		t.Fatal("应用成功应标记 configured")
	}
}

// 非法引脚配置即使 configured=true 也不得初始化方向 (垃圾 crps.json 安全要求)
func TestNewBusInvalidPinsStayUnconfigured(t *testing.T) {
	stubPortAvailable(t, true)
	for _, tc := range [][2]string{{"Bogus", "Pin20"}, {"Pin19", "Bogus"}, {"Pin21", "Pin21"}} {
		b := NewBus(tc[0], tc[1], true)
		if b.isConfigured() {
			t.Fatalf("非法配置 %v 应保持未配置", tc)
		}
		if len(b.drivenPins()) != 0 {
			t.Fatalf("非法配置 %v 不得写方向寄存器 (driven 追踪应为空)", tc)
		}
	}
}

// ---------------- autoDetect 收尾 restore ----------------

// 未配置总线探测收尾: 只释放扫描期间驱动过的针脚, 不得驱动任何引脚 (含原 SCL)。
// 旧实现 restore 无条件 switchPin(orig)+busReset, 会把未配置机器的默认 SCL
// 驱动为输出, 违反"未配置不碰 GPIO 方向"的安全前提 (探测取消/无匹配同样适用)。
func TestAutoDetectRestoreUnconfiguredReleasesAllDriven(t *testing.T) {
	stubPortAvailable(t, true)
	b := NewBus("Pin19", "Pin20", false)
	b.pio = newMockPIO()

	// 模拟扫描中途状态: Pin19/Pin21 已被驱动为输出 (driven 追踪在案)
	b.mu.Lock()
	if err := b.setPinDir("Pin19", GPPins["Pin19"], true); err != nil {
		t.Fatalf("预置驱动 Pin19 失败: %v", err)
	}
	if err := b.setPinDir("Pin21", GPPins["Pin21"], true); err != nil {
		t.Fatalf("预置驱动 Pin21 失败: %v", err)
	}
	b.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消: 第一轮 sleepCtx 即失败, 直接走 restore
	scl, sda, err := b.autoDetect(ctx)
	if scl != "" || sda != "" {
		t.Fatalf("已取消的探测不应命中接线, 得到 %s/%s", scl, sda)
	}
	if err != nil {
		t.Fatalf("清理成功时错误应为 nil, 得到 %v", err)
	}
	if pins := b.drivenPins(); len(pins) != 0 {
		t.Fatalf("未配置收尾应释放全部已驱动针脚, 仍残留 %v", pins)
	}
	if b.isConfigured() {
		t.Fatal("探测收尾不得改变 configured 状态")
	}
}

// 未配置收尾释放失败必须如实保留 driven 追踪 (可重试), 不得假报已恢复;
// 故障清除后重试应全部释放。
func TestAutoDetectRestoreUnconfiguredFailureKeepsTracking(t *testing.T) {
	stubPortAvailable(t, true)
	b := NewBus("Pin19", "Pin20", false)
	mock := newMockPIO()
	b.pio = mock

	b.mu.Lock()
	if err := b.setPinDir("Pin21", GPPins["Pin21"], true); err != nil {
		t.Fatalf("预置驱动 Pin21 失败: %v", err)
	}
	b.mu.Unlock()

	// 数据口 0x2F 写全失败: 释放必败, driven 追踪应保留, 清理错误如实返回
	mock.failOutbOnPort = 0x2F
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := b.autoDetect(ctx)
	if err == nil {
		t.Fatal("释放失败时必须返回清理错误, 不得静默吞掉")
	}
	if !strings.Contains(err.Error(), "Pin21") {
		t.Fatalf("清理错误应点名失败针脚 Pin21, 得到 %v", err)
	}
	if pins := b.drivenPins(); len(pins) == 0 {
		t.Fatal("释放失败时不得清空 driven 追踪 (假报已恢复)")
	}

	// 故障清除后重试: 全部释放, 错误消失
	mock.failOutbOnPort = -1
	if _, _, err := b.autoDetect(ctx); err != nil {
		t.Fatalf("故障清除后重试清理应成功, 得到 %v", err)
	}
	if pins := b.drivenPins(); len(pins) != 0 {
		t.Fatalf("故障清除后重试应释放全部针脚, 仍残留 %v", pins)
	}
}

// 已配置总线探测收尾: 恢复原引脚并保持 SCL 驱动, 扫描残留针脚全部释放,
// configured 状态保持不变。
func TestAutoDetectRestoreConfiguredRestoresOriginalPins(t *testing.T) {
	stubPortAvailable(t, true)
	b := NewBus("Pin19", "Pin20", false)
	b.pio = newMockPIO()
	if ok, msg := b.reconfigure(nil, nil); !ok {
		t.Fatalf("reconfigure 应成功: %s", msg)
	}
	// 模拟扫描残留: Pin21 已被驱动
	b.mu.Lock()
	if err := b.setPinDir("Pin21", GPPins["Pin21"], true); err != nil {
		t.Fatalf("预置驱动 Pin21 失败: %v", err)
	}
	b.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := b.autoDetect(ctx); err != nil {
		t.Fatalf("已配置收尾应成功 (switchPin+busReset 均无故障), 得到 %v", err)
	}

	if scl, sda := b.pinNames(); scl != "Pin19" || sda != "Pin20" {
		t.Fatalf("已配置收尾应恢复原引脚, 得到 %s/%s", scl, sda)
	}
	pins := b.drivenPins()
	if len(pins) != 1 || pins[0] != "Pin19" {
		t.Fatalf("收尾后应仅剩原 SCL (Pin19) 保持驱动, 实际 %v", pins)
	}
	if !b.isConfigured() {
		t.Fatal("已配置收尾不得清除 configured 状态")
	}
}

// 已配置收尾的两条失败路径都必须经 autoDetect 第三返回值上报:
//  1. switchPin 恢复原引脚失败 (failOutb 全写失败) -> "恢复原引脚失败"
//  2. switchPin 成功但 busReset 末尾 sdaRelease 端口写失败 -> "总线复位失败",
//     SDA 滞留输出须保留在 driven 追踪; 故障清除后重试应释放并消除错误。
// 复位末尾的释放与 initDirs 的 SDA 置输入写同一值 (Pin20: 0xCD 寄存器清 bit0
// => sioWrite 数据口 0x2F 写 0x06), 用 failOnNth 跳过 initDirs 那次精确打击复位末尾。
func TestAutoDetectRestoreConfiguredFailurePropagates(t *testing.T) {
	stubPortAvailable(t, true)

	// 路径 1: switchPin 失败
	b1 := NewBus("Pin19", "Pin20", false)
	m1 := newMockPIO()
	b1.pio = m1
	if ok, msg := b1.reconfigure(nil, nil); !ok {
		t.Fatalf("reconfigure 应成功: %s", msg)
	}
	m1.failOutb = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := b1.autoDetect(ctx); err == nil || !strings.Contains(err.Error(), "恢复原引脚失败") {
		t.Fatalf("switchPin 失败应报 '恢复原引脚失败', 得到 %v", err)
	}

	// 路径 2: busReset 末尾 sdaRelease 失败
	b := NewBus("Pin19", "Pin20", false)
	mock := newMockPIO()
	b.pio = mock
	if ok, msg := b.reconfigure(nil, nil); !ok {
		t.Fatalf("reconfigure 应成功: %s", msg)
	}
	mock.resetOutbCounts()                  // reconfigure 准备阶段的写入不计入
	mock.injectOutbFailOnNth(0x2F, 0x06, 2) // 第 1 次=initDirs 置 SDA 输入(放行), 第 2 次=复位末尾释放(失败)
	if _, _, err := b.autoDetect(ctx); err == nil || !strings.Contains(err.Error(), "总线复位失败") {
		t.Fatalf("busReset 失败应报 '总线复位失败', 得到 %v", err)
	}
	pins := b.drivenPins()
	stuck := false
	for _, p := range pins {
		if p == "Pin20" {
			stuck = true
		}
	}
	if !stuck {
		t.Fatalf("SDA 释放失败应保留 Pin20 在 driven 追踪 (滞留输出), 实际 %v", pins)
	}
	// 故障已一次性消耗: 重试应完成收尾, 释放 SDA 并消除错误
	if _, _, err := b.autoDetect(ctx); err != nil {
		t.Fatalf("故障清除后重试收尾应成功, 得到 %v", err)
	}
	pins = b.drivenPins()
	if len(pins) != 1 || pins[0] != "Pin19" {
		t.Fatalf("重试后应仅剩原 SCL (Pin19) 保持驱动, 实际 %v", pins)
	}
	if !b.isConfigured() {
		t.Fatal("已配置收尾不得清除 configured 状态")
	}
}

// detect 接口走真实 autoDetect (不注入 DetectFn): 已配置总线复位末尾 SDA 释放
// 失败时, 响应必须带 cleanup_failed:true 且不回内部针脚细节; 故障消耗后重试
// 恢复为普通未命中提示。用已取消的请求 ctx 让探测第一轮即走收尾。
func TestDetectCleanupFailureFromBusReset(t *testing.T) {
	stubAdmin(t, true)
	stubPortAvailable(t, true)
	b := NewBus("Pin19", "Pin20", false)
	mock := newMockPIO()
	b.pio = mock
	if ok, msg := b.reconfigure(nil, nil); !ok {
		t.Fatalf("reconfigure 应成功: %s", msg)
	}
	mock.resetOutbCounts()
	mock.injectOutbFailOnNth(0x2F, 0x06, 2)
	srv := &AppServer{Bus: b}

	post := func() string {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodPost, "/api/crps/detect", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		srv.APIHandler().ServeHTTP(rec, req)
		return rec.Body.String()
	}

	body := post()
	if !strings.Contains(body, `"cleanup_failed":true`) {
		t.Fatalf("复位清理失败应带 cleanup_failed:true, 得到 %s", body)
	}
	if strings.Contains(body, "Pin20") || strings.Contains(body, "0x06") {
		t.Fatalf("响应不得泄露内部针脚/寄存器细节, 得到 %s", body)
	}

	body = post() // 一次性故障已消耗: 重试清理成功, 回到普通未命中
	if !strings.Contains(body, "未探测到电源接线") || strings.Contains(body, "cleanup_failed") {
		t.Fatalf("重试清理成功应回普通未命中提示, 得到 %s", body)
	}
	if pins := b.drivenPins(); len(pins) != 1 || pins[0] != "Pin19" {
		t.Fatalf("重试后应仅剩原 SCL 保持驱动, 实际 %v", pins)
	}
}

// 恢复收尾与后台读总线并发: 方向写入经 b.mu 串行 (switchPin/busReset 自取锁,
// 未配置分支持 b.mu 调 setPinDir), 不得死锁; -race 下同时验证无数据竞争。
func TestAutoDetectRestoreConcurrentWithReads(t *testing.T) {
	stubPortAvailable(t, true)
	b := NewBus("Pin19", "Pin20", false)
	b.pio = newMockPIO()
	if ok, msg := b.reconfigure(nil, nil); !ok {
		t.Fatalf("reconfigure 应成功: %s", msg)
	}
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer close(done)
		b.autoDetect(ctx) // 50ms 后取消, 收尾走已配置 restore 路径
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		_ = b.readWord(Cmd["READ_VIN"]) // 与 restore 并发的总线读
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("autoDetect 与并发读总线发生死锁")
	}
}

// detect 接口: 探测未命中时, 收尾清理成功与清理失败必须给出不同响应——
// 清理失败意味着针脚可能仍为输出, 调用方需要知道必须重试,
// 不能与 "未找到且已清理完成" 混为同一句提示。
func TestDetectCleanupFailureDistinctFromCleanMiss(t *testing.T) {
	stubAdmin(t, true)
	stubPortAvailable(t, true)

	// 情形 1: 未命中 + 清理完成 -> 原有提示, 无 cleanup_failed 标记
	b := NewBus("Pin19", "Pin20", false)
	b.pio = newMockPIO()
	srv := &AppServer{
		Bus:      b,
		DetectFn: func(context.Context) (string, string, error) { return "", "", nil },
	}
	req := httptest.NewRequest(http.MethodPost, "/api/crps/detect", nil)
	rec := httptest.NewRecorder()
	srv.APIHandler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "未探测到电源接线") {
		t.Fatalf("清理完成的未命中应回原提示, 得到 %s", body)
	}
	if strings.Contains(body, "cleanup_failed") {
		t.Fatalf("清理完成不得带 cleanup_failed 标记, 得到 %s", body)
	}

	// 情形 2: 未命中 + 清理失败 -> cleanup_failed:true + 明确清理失败提示
	b2 := NewBus("Pin19", "Pin20", false)
	b2.pio = newMockPIO()
	srv2 := &AppServer{
		Bus: b2,
		DetectFn: func(context.Context) (string, string, error) {
			return "", "", errors.New("针脚 Pin21 未能释放回输入")
		},
	}
	rec2 := httptest.NewRecorder()
	srv2.APIHandler().ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/api/crps/detect", nil))
	body2 := rec2.Body.String()
	if !strings.Contains(body2, `"cleanup_failed":true`) {
		t.Fatalf("清理失败应带 cleanup_failed:true, 得到 %s", body2)
	}
	if !strings.Contains(body2, "清理 GPIO 失败") {
		t.Fatalf("清理失败应含明确清理失败提示, 得到 %s", body2)
	}
	if strings.Contains(body2, "Pin21") {
		t.Fatalf("响应不得泄露内部针脚细节 (只记服务端日志), 得到 %s", body2)
	}
	if strings.Contains(body2, "未探测到电源接线") {
		t.Fatalf("清理失败不得与未找到混淆, 得到 %s", body2)
	}
}

// 未配置探测扫描中途中断: 扫描会把引脚字段切到候选组合 (如 Pin19/Pin21),
// 收尾必须还原为探测前 orig——否则残留组合会让 reconfigure 只传一侧引脚时
// 拼出错误搭档。readMask 强制数据口读回全 1 (NACK) 使所有组合都不命中,
// 等扫描切走字段后取消, 验证收尾还原字段并释放全部驱动针脚。
func TestAutoDetectRestoresPinFieldsAfterScanResidue(t *testing.T) {
	stubPortAvailable(t, true)
	b := NewBus("Pin19", "Pin20", false)
	mock := newMockPIO()
	mock.readMask[0xA01] = 0xFF // Pin19/Pin21 数据口: 读回全 1 => SDA 恒高 => NACK => 不命中
	mock.readMask[0xA05] = 0xFF // Pin20/Pin22 数据口: 同上
	b.pio = mock

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = b.autoDetect(ctx)
	}()
	// 等扫描把引脚字段切离 orig (第 2 个候选组合 switchPin 成功后)
	moved := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if s, d := b.pinNames(); s != "Pin19" || d != "Pin20" {
			moved = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !moved {
		t.Fatal("扫描未在 3s 内切换引脚字段 (mock 行为与预期不符)")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("取消后 autoDetect 未及时返回")
	}
	// 收尾: 字段还原 orig, 驱动针脚全部释放, 未配置状态不变
	if s, d := b.pinNames(); s != "Pin19" || d != "Pin20" {
		t.Fatalf("收尾应还原引脚字段为 orig Pin19/Pin20, 得到 %s/%s", s, d)
	}
	if pins := b.drivenPins(); len(pins) != 0 {
		t.Fatalf("收尾应释放全部驱动针脚, 仍残留 %v", pins)
	}
	if b.isConfigured() {
		t.Fatal("探测收尾不得改变 configured 状态")
	}
}

// Reset 后首个 Sample 只建时间基准不积分 (采样器 dt 跨越清零时刻)
func TestEnergyResetSkipsFirstSample(t *testing.T) {
	e := NewEnergy(filepath.Join(t.TempDir(), "energy.json"))
	e.Sample(500, 15*time.Second) // 先有累计
	if err := e.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	e.Sample(500, 15*time.Second) // 清零后首个采样: 不积分
	if total, _ := e.Snapshot()["total_wh"].(float64); total != 0 {
		t.Fatalf("清零后首个采样不应积分, total_wh=%v", total)
	}
	e.Sample(500, 15*time.Second) // 第二个采样: 正常积分
	if total, _ := e.Snapshot()["total_wh"].(float64); total <= 0 {
		t.Fatal("清零后第二个采样应正常积分")
	}
}

// 目录不可写时 Reset 返回错误 (HTTP 层据此回 500, 不谎报成功)
func TestEnergyResetSaveError(t *testing.T) {
	e := NewEnergy(filepath.Join(t.TempDir(), "no-such-dir", "energy.json"))
	if err := e.Reset(); err == nil {
		t.Fatal("目录不存在时 Reset 应返回保存错误")
	}
}

// ---------------- HTTP 鉴权与 body 限长 ----------------

// 替换 AdminCheck 并在测试结束后恢复
func stubAdmin(t *testing.T, allow bool) {
	t.Helper()
	old := AdminCheck
	AdminCheck = func(*http.Request) bool { return allow }
	t.Cleanup(func() { AdminCheck = old })
}

func TestPostRequiresAdmin(t *testing.T) {
	stubAdmin(t, false)
	srv := &AppServer{} // Bus 为 nil: 鉴权必须先于任何总线访问
	h := srv.APIHandler()
	for _, ep := range []string{"fan", "config", "detect", "energy"} {
		req := httptest.NewRequest(http.MethodPost, "/api/crps/"+ep, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST /api/crps/%s 无管理员应 403, 得到 %d", ep, rec.Code)
		}
	}
}

func TestLogDownloadRequiresAdmin(t *testing.T) {
	stubAdmin(t, false)
	srv := &AppServer{}
	h := srv.APIHandler()
	req := httptest.NewRequest(http.MethodGet, "/api/crps/log", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /api/crps/log 无管理员应 403, 得到 %d", rec.Code)
	}
}

func TestGetEndpointsOpenWithoutAdmin(t *testing.T) {
	stubAdmin(t, false)
	srv := &AppServer{}
	h := srv.APIHandler()
	// 状态查询对非管理员保持开放 (只读)
	req := httptest.NewRequest(http.MethodGet, "/api/crps/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/crps/status 应 200, 得到 %d", rec.Code)
	}
}

// 管理员时写接口正常走到业务逻辑 (Bus 未配置 -> gpio-not-configured 而非 403)
func TestPostWithAdminReachesHandler(t *testing.T) {
	stubAdmin(t, true)
	srv := &AppServer{Bus: NewBus("Pin19", "Pin20", false)}
	h := srv.APIHandler()
	req := httptest.NewRequest(http.MethodPost, "/api/crps/fan", strings.NewReader(`{"pct":50}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员 POST 应 200, 得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "gpio-not-configured") {
		t.Fatalf("未配置总线应报 gpio-not-configured, 得到 %s", rec.Body.String())
	}
}

// 风扇占比钳制到 10-100 (mock 总线下所有读回 0, ACK 恒成功)
func TestFanPctClamp(t *testing.T) {
	stubAdmin(t, true)
	stubPortAvailable(t, true)
	b := NewBus("Pin19", "Pin20", false)
	mock := newMockPIO()
	mock.readMask[GPPins["Pin20"].Port] = 0 // 从机恒拉低 SDA: ACK 恒真
	b.pio = mock
	if ok, _ := b.reconfigure(nil, nil); !ok {
		t.Fatal("reconfigure 失败")
	}
	srv := &AppServer{Bus: b}
	h := srv.APIHandler()
	for body, want := range map[string]int{`{"pct":250}`: 100, `{"pct":5}`: 10} {
		req := httptest.NewRequest(http.MethodPost, "/api/crps/fan", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s -> %d, 期望 200", body, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"pct":`+strconv.Itoa(want)) {
			t.Fatalf("%s 应钳到 %d, 得到 %s", body, want, rec.Body.String())
		}
	}
}

func TestReadJSONBodyLimit(t *testing.T) {
	// 超限 body -> *http.MaxBytesError (413 语义), 调用方拒绝执行
	big := strings.Repeat(" ", jsonBodyMaxBytes+1024)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(big))
	if _, err := readJSONBody(httptest.NewRecorder(), req); err == nil {
		t.Fatal("超限 body 应返回错误")
	} else {
		var mbe *http.MaxBytesError
		if !errors.As(err, &mbe) {
			t.Fatalf("超限错误应为 *http.MaxBytesError, 得到 %T: %v", err, err)
		}
	}
	// 非法 JSON -> 解析错误 (400 语义)
	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{oops"))
	if _, err := readJSONBody(httptest.NewRecorder(), req); err == nil {
		t.Fatal("非法 JSON 应返回错误")
	}
	// 正常小 body 照常解析
	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"action":"reset"}`))
	m, err := readJSONBody(httptest.NewRecorder(), req)
	if err != nil || m["action"] != "reset" {
		t.Fatalf("正常 body 解析失败: %v err=%v", m, err)
	}
}

// POST 带非法/超限 JSON body: 回 400/413 并停止处理, 不得以默认值执行写操作
func TestPostRejectsBadBody(t *testing.T) {
	stubAdmin(t, true)
	stubPortAvailable(t, true)
	b := NewBus("Pin19", "Pin20", false)
	mock := newMockPIO()
	mock.readMask[GPPins["Pin20"].Port] = 0
	b.pio = mock
	if ok, _ := b.reconfigure(nil, nil); !ok {
		t.Fatal("reconfigure 失败")
	}
	srv := &AppServer{Bus: b}
	h := srv.APIHandler()

	req := httptest.NewRequest(http.MethodPost, "/api/crps/fan", strings.NewReader("{oops"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 应回 400, 得到 %d", rec.Code)
	}

	big := strings.NewReader(`{"pct":50,"pad":"` + strings.Repeat("x", jsonBodyMaxBytes) + `"}`)
	req = httptest.NewRequest(http.MethodPost, "/api/crps/fan", big)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限 body 应回 413, 得到 %d", rec.Code)
	}

	// JSON null: Unmarshal 不报错但 m 为 nil, 须回 400 而非空响应 200
	req = httptest.NewRequest(http.MethodPost, "/api/crps/fan", strings.NewReader("null"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("null body 应回 400, 得到 %d", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("null body 不应得到空响应")
	}
}
