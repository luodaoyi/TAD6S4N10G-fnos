package powerguard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeHidDevice 假 hidraw 设备：记录写入、按序返回预置响应。
type fakeHidDevice struct {
	writes  [][]byte
	reads   [][]byte
	readAt  int
	closed  bool
	failSet error
}

func (f *fakeHidDevice) Write(b []byte) (int, error) {
	if f.failSet != nil {
		return 0, f.failSet
	}
	f.writes = append(f.writes, append([]byte(nil), b...))
	return len(b), nil
}

func (f *fakeHidDevice) Read(b []byte) (int, error) {
	if f.readAt >= len(f.reads) {
		return 0, errors.New("fake hid device exhausted")
	}
	copy(b, f.reads[f.readAt])
	f.readAt++
	return len(b), nil
}

func (f *fakeHidDevice) Close() error {
	f.closed = true
	return nil
}

// injectFakeHidDevices 把 openHidDevice 替换为按设备节点路径分发的假实现。
func injectFakeHidDevices(t *testing.T, devices map[string]*fakeHidDevice) {
	t.Helper()
	previous := openHidDevice
	openHidDevice = func(path string) (hidDevice, error) {
		device, ok := devices[filepath.Base(path)]
		if !ok {
			return nil, fmt.Errorf("open %s: no such fake device", path)
		}
		return device, nil
	}
	t.Cleanup(func() { openHidDevice = previous })
}

// writeHidrawUevent 在临时 sysfs 树里造一个 hidraw 节点。
func writeHidrawUevent(t *testing.T, root, node string, lines ...string) {
	t.Helper()
	dir := filepath.Join(root, "sys", "class", "hidraw", node, "device")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "uevent"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseHidUevent(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		vid     uint16
		pid     uint16
		hidName string
		ok      bool
	}{
		{
			name:    "temper1",
			data:    "DRIVER=hid-generic\nHID_ID=0003:00000C45:00007401\nHID_NAME=RDing TEMPerV1.4\nHID_PHYS=usb-0000:00:14.0-3/input0\n",
			vid:     0x0c45,
			pid:     0x7401,
			hidName: "RDing TEMPerV1.4",
			ok:      true,
		},
		{
			name:    "keyboard",
			data:    "HID_ID=0003:0000046D:0000C31C\nHID_NAME=Logitech Keyboard",
			vid:     0x046d,
			pid:     0xc31c,
			hidName: "Logitech Keyboard",
			ok:      true,
		},
		{name: "missing hid id", data: "HID_NAME=RDing TEMPerV1.4\n", hidName: "RDing TEMPerV1.4"},
		{name: "malformed hid id", data: "HID_ID=0003:zzzz:00007401\n"},
		{name: "empty", data: ""},
	}
	for _, test := range tests {
		vid, pid, name, ok := parseHidUevent(test.data)
		if ok != test.ok || vid != test.vid || pid != test.pid || name != test.hidName {
			t.Fatalf("%s: parseHidUevent()=(%#x,%#x,%q,%v), want (%#x,%#x,%q,%v)",
				test.name, vid, pid, name, ok, test.vid, test.pid, test.hidName, test.ok)
		}
	}
}

func TestUSBTempDecoders(t *testing.T) {
	// 响应样例取自 cylab.be 教程的真实回包 80 80 0b 92 4e 20 00 00，
	// 字节 2/3 大端原始值 0x0b92 = 2962。
	resp := []byte{0x80, 0x80, 0x0b, 0x92, 0x4e, 0x20, 0x00, 0x00}
	tests := []struct {
		name   string
		decode func([]byte) float64
		resp   []byte
		want   float64
	}{
		// FM75：2962×125/32000=11.5703125（pcsensor/TEMPered 同款公式）
		{"fm75", decodeTEMPerFM75, resp, 11.5703125},
		// FM75 负温：raw=0xff8c=-116 → -116×125/32000=-0.453125
		{"fm75 negative", decodeTEMPerFM75, []byte{0x80, 0x80, 0xff, 0x8c, 0, 0, 0, 0}, -0.453125},
		// SHT1x：raw=0x1752=5970 → -39.7+0.01×5970=20.0
		{"sht1x", decodeTEMPerSHT1X, []byte{0x80, 0x80, 0x17, 0x52, 0, 0, 0, 0}, 20.0},
		// 新版固件 1/100 度：2962/100=29.62（cylab.be）
		{"centi", decodeTEMPerCenti, resp, 29.62},
	}
	for _, test := range tests {
		if got := test.decode(test.resp); got != test.want {
			t.Fatalf("%s: decode=%.7f, want %.7f", test.name, got, test.want)
		}
	}
}

// readUSBTemperature 必须写入 9 字节报告（报告号 0x00 + 命令载荷），
// 且只解码第二次读取的响应（“问两次”约定）。
func TestReadUSBTemperatureAskedTwice(t *testing.T) {
	device := &fakeHidDevice{
		reads: [][]byte{
			{0x80, 0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, // 第一次只用于唤醒
			{0x80, 0x80, 0x0b, 0x92, 0x4e, 0x20, 0x00, 0x00}, // 第二次才是数据
		},
	}
	celsius, err := readUSBTemperature(device, decodeTEMPerCenti)
	if err != nil {
		t.Fatal(err)
	}
	if celsius != 29.62 {
		t.Fatalf("celsius=%v, want 29.62 (decoded from the second response)", celsius)
	}
	wantReport := append([]byte{0x00}, usbTempReportPayload...)
	if len(device.writes) != 2 {
		t.Fatalf("device should be written twice, got %d", len(device.writes))
	}
	for i, write := range device.writes {
		if !reflect.DeepEqual(write, wantReport) {
			t.Fatalf("write %d = % x, want % x", i, write, wantReport)
		}
	}
	if device.closed {
		t.Fatal("Close is the caller's responsibility, fake should not be closed by readUSBTemperature")
	}
}

func TestReadUSBTemperaturePropagatesErrors(t *testing.T) {
	device := &fakeHidDevice{failSet: errors.New("permission denied")}
	if _, err := readUSBTemperature(device, decodeTEMPerFM75); err == nil {
		t.Fatal("write error must propagate")
	}
	device = &fakeHidDevice{reads: [][]byte{{0, 0, 0, 0, 0, 0, 0, 0}}}
	if _, err := readUSBTemperature(device, decodeTEMPerFM75); err == nil {
		t.Fatal("read error must propagate")
	}
}

// 枚举：sysfs 树解析 + VID:PID 匹配 + 同名消歧；未知设备不打开、零副作用。
func TestUSBTemperatureReadingsEnumeration(t *testing.T) {
	root := t.TempDir()
	writeHidrawUevent(t, root, "hidraw0",
		"DRIVER=hid-generic", "HID_ID=0003:00000C45:00007401", "HID_NAME=RDing TEMPerV1.4")
	writeHidrawUevent(t, root, "hidraw1",
		"HID_ID=0003:00000C45:00007401", "HID_NAME=RDing TEMPerV1.4")
	writeHidrawUevent(t, root, "hidraw2",
		"HID_ID=0003:0000046D:0000C31C", "HID_NAME=Logitech Keyboard")
	writeHidrawUevent(t, root, "hidraw3",
		"HID_ID=0003:0000413D:00002107", "HID_NAME=")

	manager := &Manager{Root: root}
	first := &fakeHidDevice{reads: [][]byte{
		{0x80, 0x80, 0, 0, 0, 0, 0, 0},
		{0x80, 0x80, 0x0b, 0x92, 0, 0, 0, 0}, // 2932 → 11.453125（FM75）
	}}
	second := &fakeHidDevice{reads: [][]byte{
		{0x80, 0x80, 0, 0, 0, 0, 0, 0},
		{0x80, 0x80, 0x0c, 0x1c, 0, 0, 0, 0}, // 3100 → 12.109375（FM75）
	}}
	third := &fakeHidDevice{reads: [][]byte{
		{0x80, 0x80, 0, 0, 0, 0, 0, 0},
		{0x80, 0x80, 0x0b, 0x92, 0, 0, 0, 0}, // 2932 → 29.32（1/100 度）
	}}
	injectFakeHidDevices(t, map[string]*fakeHidDevice{
		"hidraw0": first, "hidraw1": second, "hidraw3": third,
	})

	readings := manager.usbTemperatureReadings()
	want := []usbTempReading{
		{Key: "usb:RDing-TEMPerV1.4", Celsius: 11.5703125},
		{Key: "usb:RDing-TEMPerV1.4#2", Celsius: 12.109375},
		{Key: "usb:TEMPerV3", Celsius: 29.62}, // 无 HID_NAME 时用设备表通用名
	}
	if !reflect.DeepEqual(readings, want) {
		t.Fatalf("readings=%+v, want %+v", readings, want)
	}
	for _, device := range []*fakeHidDevice{first, second, third} {
		if !device.closed {
			t.Fatal("device must be closed after reading")
		}
	}
	if manager.usbLastError != "" {
		t.Fatalf("no error expected, got %q", manager.usbLastError)
	}
}

// 无 hidraw 设备（或全部未知 VID:PID）时返回空且不产生错误日志。
func TestUSBTemperatureReadingsWithoutDevices(t *testing.T) {
	manager := &Manager{Root: t.TempDir()}
	if readings := manager.usbTemperatureReadings(); len(readings) != 0 {
		t.Fatalf("no device expected, got %+v", readings)
	}
	if manager.usbLastError != "" {
		t.Fatalf("no error expected, got %q", manager.usbLastError)
	}
}

// 打开失败（无权限常见）时安静跳过该设备，错误按变化记录。
func TestUSBTemperatureReadingsOpenFailureIsQuiet(t *testing.T) {
	root := t.TempDir()
	writeHidrawUevent(t, root, "hidraw0",
		"HID_ID=0003:00000C45:00007401", "HID_NAME=RDing TEMPerV1.4")
	injectFakeHidDevices(t, map[string]*fakeHidDevice{})
	manager := &Manager{Root: root}
	if readings := manager.usbTemperatureReadings(); len(readings) != 0 {
		t.Fatalf("no reading expected, got %+v", readings)
	}
	if manager.usbLastError == "" {
		t.Fatal("open failure should be recorded for log-on-change")
	}
	// 相同错误重复出现不再变更（不刷日志）；恢复成功后清除
	before := manager.usbLastError
	manager.usbTemperatureReadings()
	if manager.usbLastError != before {
		t.Fatalf("unchanged error should not toggle: %q vs %q", manager.usbLastError, before)
	}
}

func TestUSBTempLabel(t *testing.T) {
	tests := []struct {
		hidName, fallback, want string
	}{
		{"RDing TEMPerV1.4", "TEMPer", "RDing-TEMPerV1.4"},
		{"  ", "TEMPer", "TEMPer"},                                   // 空白名退回通用名
		{"", "TEMPer", "TEMPer"},                                     // 空名退回通用名
		{strings.Repeat("A", 40), "TEMPer", strings.Repeat("A", 32)}, // 截断到 32 字符
	}
	for _, test := range tests {
		if got := usbTempLabel(test.hidName, test.fallback); got != test.want {
			t.Fatalf("usbTempLabel(%q,%q)=%q, want %q", test.hidName, test.fallback, got, test.want)
		}
	}
}

// USB 传感器并入 extraTemperatures 后走完整采样链路：key 前缀 usb:，
// 归类「其它」，出现在历史传感器列表并支持改名与父类覆盖。生产链路里缓存
// 由 USBTemperatureLoop/ProbeUSBTemperatureSensors 写入，这里直接填充等价。
func TestUSBTemperatureSensorsFlowIntoSampling(t *testing.T) {
	root := t.TempDir()
	writeHidrawUevent(t, root, "hidraw0",
		"HID_ID=0003:00000C45:00007401", "HID_NAME=RDing TEMPerV1.4")
	injectFakeHidDevices(t, map[string]*fakeHidDevice{
		"hidraw0": {reads: [][]byte{
			{0x80, 0x80, 0, 0, 0, 0, 0, 0},
			{0x80, 0x80, 0x0b, 0x92, 0, 0, 0, 0},
		}},
	})
	manager := &Manager{Root: root}
	// 模拟后台循环：锁外读取 + 锁内写缓存（读数新鲜期内 extraTemperatures 采信）
	readings := manager.usbTemperatureReadings()
	manager.mu.Lock()
	manager.usbTempReadings = readings
	manager.usbTempAt = time.Now()
	manager.mu.Unlock()
	temps := manager.extraTemperatures()
	found := false
	for _, temp := range temps {
		if temp.Label == "usb:RDing-TEMPerV1.4" {
			found = true
			if temp.Celsius != 11.5703125 {
				t.Fatalf("usb sensor celsius=%v, want 11.5703125", temp.Celsius)
			}
		}
	}
	if !found {
		t.Fatalf("usb sensor missing from extraTemperatures: %+v", temps)
	}

	// 采样点分类：usb: 前缀 → 「其它」组
	status := Status{ExtraTemperatures: temps}
	sample := SampleFromStatus(&status, time.Unix(1700000003, 0))
	var group string
	for _, sensor := range sample.Sensors {
		if sensor.Key == "usb:RDing-TEMPerV1.4" {
			group = sensor.Group
		}
	}
	if group != "other" {
		t.Fatalf("usb sensor group=%q, want other", group)
	}
	// 父类覆盖链路（SensorGroups）对 usb: 键生效
	ApplySensorGroupOverrides([]HistorySample{sample}, map[string]string{"usb:RDing-TEMPerV1.4": "nic"})
	for _, sensor := range sample.Sensors {
		if sensor.Key == "usb:RDing-TEMPerV1.4" && sensor.Group != "nic" {
			t.Fatalf("group override should apply to usb sensor, got %q", sensor.Group)
		}
	}
}

// 内核侧 hwmon 驱动（如 out-of-tree "temper"）的 USB 温度传感器走通用
// hwmon 收集路径，归类固定「其它」。
func TestClassifySensorLabelHwmonUSBDriver(t *testing.T) {
	if got := classifySensorLabel("temper:temp1"); got != "other" {
		t.Fatalf("temper hwmon chip should classify as other, got %q", got)
	}
}
