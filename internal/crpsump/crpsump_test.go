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
	sioExits       int            // 记录 sioExit (outb 0x2E=0xAA) 执行次数
	writes         int
}

func newMockPIO() *mockPIO {
	return &mockPIO{mem: map[int]byte{}, readMask: map[int]byte{}, failOutbOnPort: -1,
		failWhen: map[int64]bool{}}
}

// injectOutbFail 定点注入: 仅下一次 outb(port, v) 失败 (测试"仅方向寄存器索引
// 选择失败一次"等精确故障路径, 不影响同端口其他值的写入)
func (m *mockPIO) injectOutbFail(port int, v byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failWhen[int64(port)<<8|int64(v)] = true
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
	srv := &AppServer{Bus: b, DetectFn: func(context.Context) (string, string) { return "Pin19", "Pin20" }}
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
		DetectFn:   func(context.Context) (string, string) { return "Pin19", "Pin20" },
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
