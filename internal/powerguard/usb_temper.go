package powerguard

// 外置 USB 温度传感器（TEMPer 系列）支持。大量廉价 USB 温度计（RDing
// TEMPer1 等）没有内核驱动，只以 HID 设备出现在 /dev/hidraw*；本文件用
// 纯标准库（sysfs 枚举 + read/write）按公开协议读取温度，并入
// extraTemperatures 采样链路（group="other"，key 形如 "usb:TEMPer"）。
//
// 协议来源（公开实现互相印证）：
//   - pcsensor.c（shakemid/pcsensor-temper）：FM75 解码 raw*125/32000、
//     SHT1x 解码 -39.7+0.01*raw，设备表 0c45:7401/0c45:7402。
//   - TEMPered（edorfaus/TEMPered）：hidraw 写入 9 字节 = 报告号 0x00 +
//     载荷 01 80 33 01 00 00 00 00；FM75 高/低字节偏移 2/3。
//   - temper-python（padelt）：同一命令 "asked twice"（只问一次设备会在
//     下次访问时卡住）。
//   - cylab.be / maikel.tiny-host.nl 教程：新版固件按 1/100 度上报。
//
// 诚实说明：真机行为无法在开发环境验证，以上均按公开协议实现，需真机确认
// （尤其 413d:2107 的解码公式，pcsensor.cn 在不同硬件版本间改过协议）。

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// hidRawReadTimeout 是单次 hidraw 读超时：设备枚举成功但不响应时快速失败。
// hidraw 支持 poll，os.File 的 deadline 对其生效；个别内核不支持时
// SetReadDeadline 报错并被忽略。
const hidRawReadTimeout = 1500 * time.Millisecond

// USB HID 温度计的读取节奏与缓存新鲜期：读取是秒级阻塞 IO，只能在后台循环
// 里锁外做（持 m.mu 读会让 /api/status 被无响应设备卡 ~3 秒）；采样链路的
// extraTemperatures 只读缓存，超 usbTempStaleAfter 未刷新即弃用——设备拔掉
// 后曲线自然断线，不留冻结值。
const (
	usbTempInterval   = 30 * time.Second
	usbTempStaleAfter = 90 * time.Second
)

// usbTempReportPayload 是读取温度的 8 字节命令载荷（不含报告号前缀）。
var usbTempReportPayload = []byte{0x01, 0x80, 0x33, 0x01, 0x00, 0x00, 0x00, 0x00}

// hidDevice 抽象 hidraw 设备的读写，便于离线单测注入假设备。
type hidDevice interface {
	Write(b []byte) (int, error)
	Read(b []byte) (int, error)
	Close() error
}

// hidrawFile 用 os.File 实现 hidDevice：hidraw 的 write() 首字节是报告号
// （这些设备用无编号报告，固定 0x00），read() 每次返回一个输入报告。
type hidrawFile struct {
	file *os.File
}

func (d hidrawFile) Write(b []byte) (int, error) { return d.file.Write(b) }
func (d hidrawFile) Read(b []byte) (int, error) {
	_ = d.file.SetReadDeadline(time.Now().Add(hidRawReadTimeout))
	return d.file.Read(b)
}
func (d hidrawFile) Close() error { return d.file.Close() }

// openHidDevice 打开真实 /dev/hidraw* 设备；测试替换为假实现。
// 无设备或无权限（常见：非 root、udev 规则未放行）时返回错误，由调用方
// 安静降级。
var openHidDevice = func(path string) (hidDevice, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return hidrawFile{file: file}, nil
}

// usbTempDevice 描述一个已知型号：VID:PID → 显示名与响应解码函数。
type usbTempDevice struct {
	name   string
	decode func(resp []byte) float64
}

// usbTempDevices 已知设备表。键为小写 "vid:pid"（十六进制 4 位）。
var usbTempDevices = map[string]usbTempDevice{
	// TEMPer1/TEMPer2（RDing，FM75 兼容传感器）：内置传感器在响应字节 2/3，
	// 大端有符号原始值按 FM75 数据手册 ×125/32000（即 ÷256）。外置探头在
	// 字节 4/5，采样只取内置传感器保持一键一曲线。
	"0c45:7401": {name: "TEMPer", decode: decodeTEMPerFM75},
	// TEMPerHUM（SHT1x 传感器，带湿度）：温度 = -39.7 + 0.01×raw（字节 2/3）。
	"0c45:7402": {name: "TEMPerHUM", decode: decodeTEMPerSHT1X},
	// TEMPerGold_V3.x 等新版固件：同一命令，响应按 1/100 度上报（cylab.be
	// 教程：0b 92 → 29.32°C）。解码变体最多的一族，需真机验证。
	"413d:2107": {name: "TEMPerV3", decode: decodeTEMPerCenti},
}

// decodeTEMPerFM75 解码 FM75 兼容传感器（0c45:7401）。
func decodeTEMPerFM75(resp []byte) float64 {
	raw := int16(resp[2])<<8 | int16(resp[3])
	return float64(raw) * 125.0 / 32000.0
}

// decodeTEMPerSHT1X 解码 SHT1x 传感器（0c45:7402）。
func decodeTEMPerSHT1X(resp []byte) float64 {
	raw := int16(resp[2])<<8 | int16(resp[3])
	return -39.7 + 0.01*float64(raw)
}

// decodeTEMPerCenti 解码按 1/100 度上报的新版固件（413d:2107 等）。
func decodeTEMPerCenti(resp []byte) float64 {
	raw := int16(resp[2])<<8 | int16(resp[3])
	return float64(raw) / 100.0
}

// parseHidUevent 解析 /sys/class/hidraw/hidrawN/device/uevent：
// HID_ID=0003:00000C45:00007401（总线:厂商:产品，十六进制）与 HID_NAME。
// 总线 0003 = USB。返回厂商/产品 ID 与设备名。
func parseHidUevent(data string) (vid, pid uint16, name string, ok bool) {
	for _, line := range strings.Split(data, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch key {
		case "HID_ID":
			fields := strings.Split(value, ":")
			if len(fields) != 3 {
				return 0, 0, "", false
			}
			// 前导 0 去掉后按 16 位解析；超长/非法值视为不可识别
			vendor, errV := strconv.ParseUint(strings.TrimLeft(fields[1], "0xX "), 16, 16)
			product, errP := strconv.ParseUint(strings.TrimLeft(fields[2], "0xX "), 16, 16)
			if errV != nil || errP != nil {
				return 0, 0, "", false
			}
			vid, pid = uint16(vendor), uint16(product)
			ok = true
		case "HID_NAME":
			name = value
		}
	}
	return vid, pid, name, ok
}

// hidDeviceID 把 VID:PID 格式化为设备表键（小写十六进制 4 位）。
func hidDeviceID(vid, pid uint16) string {
	return fmt.Sprintf("%04x:%04x", vid, pid)
}

// readUSBTemperature 对单个设备执行“问两次”读取（temper-python：只问一次
// 设备会在下次访问时卡住）：第一次写+读用于唤醒，丢弃；第二次的响应才解码。
func readUSBTemperature(device hidDevice, decode func([]byte) float64) (float64, error) {
	report := append([]byte{0x00}, usbTempReportPayload...) // 首字节为报告号 0x00
	var resp []byte
	for round := 0; round < 2; round++ {
		if _, err := device.Write(report); err != nil {
			return 0, fmt.Errorf("write report: %w", err)
		}
		buf := make([]byte, 8)
		n, err := device.Read(buf)
		if err != nil {
			return 0, fmt.Errorf("read report: %w", err)
		}
		if n < len(buf) {
			return 0, fmt.Errorf("short read: %d bytes", n)
		}
		resp = buf
	}
	if resp[0] == 0 && resp[2] == 0 && resp[3] == 0 {
		return 0, fmt.Errorf("empty response % x", resp)
	}
	return decode(resp), nil
}

// usbTemperatureReadings 枚举 /sys/class/hidraw/*/device/uevent 找出已知
// USB 温度计并读取温度。key 形如 "usb:TEMPer"（芯片名固定 usb:，同名设备加
// #N 后缀）；无设备/无权限时安静返回空。**不要持 m.mu 调用**：单轮"问两次"
// 读取在设备无响应时阻塞 ~3 秒（调用方是 USBTemperatureLoop 等锁外路径，
// 错误状态由本函数尾部在锁内更新）。
func (m *Manager) usbTemperatureReadings() []usbTempReading {
	uevents, _ := filepath.Glob(m.rooted("/sys/class/hidraw/hidraw*/device/uevent"))
	var readings []usbTempReading
	var lastErr error
	seen := make(map[string]int)
	for _, uevent := range uevents {
		data, err := os.ReadFile(uevent)
		if err != nil {
			continue
		}
		vid, pid, hidName, ok := parseHidUevent(string(data))
		if !ok {
			continue
		}
		spec, known := usbTempDevices[hidDeviceID(vid, pid)]
		if !known {
			continue // 键盘/鼠标等其它 HID 设备：不打开、零副作用
		}
		// /sys/class/hidraw/hidrawN/device/uevent → /dev/hidrawN
		node := filepath.Base(filepath.Dir(filepath.Dir(uevent)))
		device, err := openHidDevice(m.rooted(filepath.Join("/dev", node)))
		if err != nil {
			lastErr = err
			continue
		}
		celsius, err := readUSBTemperature(device, spec.decode)
		device.Close()
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", spec.name, err)
			continue
		}
		if celsius <= 0 || celsius > 125 {
			// 合理性过滤：解码错位（比如按 FM75 解了 V3 固件）常给出离谱值
			continue
		}
		label := fmt.Sprintf("usb:%s", usbTempLabel(hidName, spec.name))
		seen[label]++
		if n := seen[label]; n > 1 {
			label = fmt.Sprintf("%s#%d", label, n) // 同名设备消歧，键是传感器主键的一部分
		}
		readings = append(readings, usbTempReading{Key: label, Celsius: celsius})
	}
	// 本轮读到任一有效值即视为恢复（清除历史错误）；否则保留最后错误供
	// 变化时记日志
	if len(readings) > 0 {
		lastErr = nil
	}
	m.mu.Lock()
	m.noteUSBTempError(lastErr)
	m.mu.Unlock()
	sort.Slice(readings, func(i, j int) bool { return readings[i].Key < readings[j].Key })
	return readings
}

// usbTempReading 是一次 USB 温度读取结果。
type usbTempReading struct {
	Key     string
	Celsius float64
}

// usbTempLabel 优先用 sysfs 的 HID_NAME（如 "RDing TEMPerV1.4"），异常时退
// 回设备表的通用名。名称截断到 32 字符，避免传感器键过长。
func usbTempLabel(hidName, fallback string) string {
	name := strings.TrimSpace(hidName)
	if name == "" {
		name = fallback
	}
	// HID_NAME 可能含空格等字符，压成短横线分隔更接近现有 hwmon 标签风格
	name = strings.Join(strings.Fields(name), "-")
	if len(name) > 32 {
		name = name[:32]
	}
	return name
}

// noteUSBTempError 记录/清除最近一次 USB 温度计读取错误，错误变化时打一条
// 日志（走注入的 m.logf，与其余错误日志同源），避免每分钟采样刷屏。
// 调用方需持有 m.mu（m.logf 不取锁，重入安全）。
func (m *Manager) noteUSBTempError(err error) {
	message := ""
	if err != nil {
		message = err.Error()
	}
	if message == m.usbLastError {
		return
	}
	m.usbLastError = message
	if err != nil {
		m.logf("usb temperature sensor read failed: %v (logging once until the error changes)", err)
	}
}

// USBTemperatureLoop 后台读取循环：锁外执行阻塞 IO，读到的值在锁内写进
// 缓存；extraTemperatures 只采信 usbTempStaleAfter 内的缓存。随 ctx 取消
// 退出。logger 当前仅用于满足启动签名（错误日志走 noteUSBTempError）。
func (m *Manager) USBTemperatureLoop(ctx context.Context, _ *log.Logger) {
	for ctx.Err() == nil {
		readings := m.usbTemperatureReadings()
		now := time.Now()
		m.mu.Lock()
		m.usbTempReadings = readings
		m.usbTempAt = now
		m.mu.Unlock()
		if !serialSleep(ctx, usbTempInterval) {
			return
		}
	}
}

// ProbeUSBTemperatureSensors 启动时探测 USB 温度传感器并把发现的设备记入
// 日志，方便用户确认接线与权限；无设备时静默。结果同时入缓存，首屏
// /api/status 即可用（后台循环的第一轮要等首个间隔后才有意义）。
func (m *Manager) ProbeUSBTemperatureSensors(logger *log.Logger) {
	if logger == nil {
		logger = log.Default()
	}
	readings := m.usbTemperatureReadings()
	now := time.Now()
	m.mu.Lock()
	m.usbTempReadings = readings
	m.usbTempAt = now
	m.mu.Unlock()
	for _, reading := range readings {
		logger.Printf("usb temperature sensor detected: %s %.1f°C", reading.Key, reading.Celsius)
	}
}
