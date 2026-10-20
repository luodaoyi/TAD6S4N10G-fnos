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
	mu       sync.Mutex
	mem      map[int]byte
	readMask map[int]byte
	failOutb bool // true 时 outb 恒失败 (测试端口写错误向上传播)
	writes   int
}

func newMockPIO() *mockPIO { return &mockPIO{mem: map[int]byte{}, readMask: map[int]byte{}} }

func (m *mockPIO) inb(port int) byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.readMask[port]; ok {
		return v
	}
	return m.mem[port]
}

func (m *mockPIO) outb(port int, v byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOutb {
		return errors.New("mock: 端口写失败")
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
}
