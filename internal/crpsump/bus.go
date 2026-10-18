// psu-monitor-go: CRPS 电源 PMBus 监控服务 (主板 GPIO bitbang I2C) —— Go 版
//
// MIT License
//
// Copyright (c) 2026 psu-monitor contributors
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.
//
// 注意：风扇控制为实验性功能。错误的风扇占空比可能影响电源散热。请谨慎调节，
// 并确保电源温度在安全范围内。
//
// 功能与 Python 版 (psu-monitor-fpk/app-src/bin/psu-monitor, v1.0.43) 完全对齐：
//   - 通过主板 F_PANEL GPIO 用软件 bitbang I2C 读取光宝 PS-2551-5L 电源 PMBus
//       SCL = Pin19 (0xA01/bit2),  SDA = Pin20 (0xA05/bit0)
//       方向控制: SCL(Pin19) SuperIO LDN7 0xC9/bit2, SDA(Pin20) 0xCD/bit0 (0=输入/1=输出)
//   - 读取参数: VIN / VOUT / IOUT / PIN / POUT / 温度 / 风扇 / 状态字 / 厂商信息
//   - 控制风扇: FAN_COMMAND_1 (0x3B) 占空比写入 (带 CRC-8 PEC)
//   - 耗电量统计: 后台每 15s 采样 READ_PIN 矩形积分, 按总量/今日/本月/按日累计,
//       持久化 energy.json (默认与 --config 同目录, 可用 --energy 指定), 重启不丢
//   - HTTP API:
//       GET  /api/status           -> 电源参数 JSON
//       GET  /api/bus /api/log /api/config /api/energy
//       POST /api/fan /api/config /api/detect /api/energy (耗电统计清零)
//
// 用法: psu-monitor-go serve --addr 127.0.0.1 --port 3878 --web-root ui/static
//       psu-monitor-go serve --socket /path/psu-monitor.sock --web-root ui/static --config cfg.json --pid pid

// Package crpsump 实现 CRPS 电源 PMBus 监控: GPIO bitbang I2C 总线、PMBus 读写、
// 风扇控制、耗电统计与 HTTP API; serve 入口见 backend/crpsump。
package crpsump

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// ---------------- 常量 ----------------

const (
	DefaultAddr   = 0x5b // 光宝 PS-2551-5L PMBus 从机地址
	LogPath       = "/tmp/psu-monitor.log"
	LogMaxSize    = 1 << 20                // 日志单文件上限 1MB, 超出轮转
	Delay         = 100 * time.Microsecond // bitbang 半周期
	AppVersion    = "1.0.49"
	MfrCacheTTL   = 60 // 秒
	DefaultSCL    = "Pin19"
	DefaultSDA    = "Pin20"
	GatewayPrefix = "/app/psu-monitor"
	ReadableTTL   = 2 // 秒: /api/config 的 readable 缓存, 避免轮询持续压总线
)

// ---- GPIO 引脚表 (Pin19~22, 全部实测可双向) ----
// 每针: Port=数据口, Bit=数据位, DirReg=方向寄存器(LDN7配置区), DirBit=方向位; 方向位 1=输出, 0=输入
type GPPin struct {
	Port   int
	Bit    int
	DirReg int
	DirBit int
}

var GPPins = map[string]GPPin{
	"Pin19": {Port: 0xA01, Bit: 2, DirReg: 0xC9, DirBit: 2}, // GPIO105
	"Pin20": {Port: 0xA05, Bit: 0, DirReg: 0xCD, DirBit: 0}, // GPIO106
	"Pin21": {Port: 0xA01, Bit: 1, DirReg: 0xC9, DirBit: 1}, // GPIO107
	"Pin22": {Port: 0xA05, Bit: 1, DirReg: 0xCD, DirBit: 1}, // GPIO108
}

var UIPins = []string{"Pin19", "Pin20", "Pin21", "Pin22"} // 界面可选 GPIO 针脚

var Cmd = map[string]byte{
	"CAPABILITY":    0x19,
	"VOUT_MODE":     0x20,
	"STATUS_WORD":   0x79,
	"READ_VIN":      0x88,
	"READ_IIN":      0x89,
	"READ_VOUT":     0x8b,
	"READ_IOUT":     0x8c,
	"READ_T1":       0x8d,
	"READ_T2":       0x8e,
	"READ_FAN1":     0x90,
	"READ_FAN2":     0x91,
	"READ_POUT":     0x96,
	"READ_PIN":      0x97,
	"FAN_COMMAND_1": 0x3b,
	"MFR_ID":        0x99,
	"MFR_MODEL":     0x9a,
	"MFR_REVISION":  0x9b,
}

// ---------------- PMBus bitbang 总线 ----------------
//
// 并发模型:
//   - mu        : 保护总线 I/O 时序(readWord/write*/busReset/bit 操作) 及引脚配置字段
//                 (sclName/sdaName/sclCfg/sdaCfg/ok)。任何访问底层 GPIO 的操作必须先持有 mu,
//                 保证切换引脚瞬间不会有正在进行的传输半途换针脚。
//   - stateMu   : 保护非 I/O 状态字段 (configured / fanMode / readable 缓存)。
//   - 方向缓存   : sclOut/sdaOut 记录 SIO 方向寄存器缓存, 只在方向真正变化时写 SIO,
//                 避免每次位操作都完整走一遍 enter/exit (项目惯例要求的性能优化)。

type Bus struct {
	addr       byte
	sclName    string
	sdaName    string
	sclCfg     GPPin
	sdaCfg     GPPin
	sclOut     bool // 方向缓存
	sdaOut     bool
	mu         sync.Mutex // 总线 I/O 与引脚配置
	stateMu    sync.Mutex // configured / fanMode / readable 缓存
	ok         bool
	configured bool // 是否已在界面设置好 PSCL/PSDA
	fanMode    string
	lastOK     bool // readable() TTL 缓存
	lastTs     int64
}

func NewBus(scl, sda string, configured bool) *Bus {
	// 校验引脚名: 非法名会令 GPPins 返回零值 GPPin{0,0,0,0}, 导致向 port 0x000(DMA 区) 写入
	if _, ok := GPPins[scl]; !ok {
		Logf("警告: 非法 SCL 引脚 '%s', 回退默认 %s", scl, DefaultSCL)
		scl = DefaultSCL
	}
	if _, ok := GPPins[sda]; !ok {
		Logf("警告: 非法 SDA 引脚 '%s', 回退默认 %s", sda, DefaultSDA)
		sda = DefaultSDA
	}
	if scl == sda {
		Logf("警告: SCL/SDA 不能同一根针, SDA 回退默认 %s", DefaultSDA)
		sda = DefaultSDA
	}
	b := &Bus{
		addr:       DefaultAddr,
		sclName:    scl,
		sdaName:    sda,
		sclCfg:     GPPins[scl],
		sdaCfg:     GPPins[sda],
		configured: configured,
		fanMode:    "auto",
		ok:         true,
	}
	// 初始化: SCL=输出, SDA=输入(释放)
	b.initDirs()
	return b
}

// setPinDir 无条件写 SIO 方向寄存器 (被带缓存的 ensure* 调用)
func (b *Bus) setPinDir(cfg GPPin, out bool) {
	sioEnter()
	sioWrite(0x07, 0x07) // 选 LDN7 (GPIO)
	cur := sioRead(byte(cfg.DirReg))
	if out {
		cur |= 1 << uint(cfg.DirBit)
	} else {
		cur &^= 1 << uint(cfg.DirBit)
	}
	sioWrite(byte(cfg.DirReg), cur)
	sioExit()
}

// ensure* 只在方向缓存与实际不符时才写 SIO (方向寄存器缓存)
func (b *Bus) ensureSclOut() {
	if b.sclOut {
		return
	}
	b.setPinDir(b.sclCfg, true)
	b.sclOut = true
}
func (b *Bus) ensureSdaOut() {
	if b.sdaOut {
		return
	}
	b.setPinDir(b.sdaCfg, true)
	b.sdaOut = true
}
func (b *Bus) ensureSdaIn() {
	if !b.sdaOut {
		return
	}
	b.setPinDir(b.sdaCfg, false)
	b.sdaOut = false
}

func (b *Bus) initDirs() {
	b.setPinDir(b.sclCfg, true)
	b.sclOut = true
	b.setPinDir(b.sdaCfg, false)
	b.sdaOut = false
}

// ---- 状态字段访问 (stateMu) ----

func (b *Bus) isConfigured() bool { b.stateMu.Lock(); defer b.stateMu.Unlock(); return b.configured }
func (b *Bus) setConfigured(v bool) {
	b.stateMu.Lock()
	b.configured = v
	b.stateMu.Unlock()
}
func (b *Bus) fanModeNow() string { b.stateMu.Lock(); defer b.stateMu.Unlock(); return b.fanMode }
func (b *Bus) setFanMode(v string) {
	b.stateMu.Lock()
	b.fanMode = v
	b.stateMu.Unlock()
}

func (b *Bus) isOK() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.ok }

// pinNames 加锁读当前引脚名 (配合 switchPin 在锁内改写)
func (b *Bus) pinNames() (string, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sclName, b.sdaName
}

// switchPin 切换 SCL/SDA 引脚并初始化方向, 不改变 configured 状态。
// 引脚配置字段在持锁状态下改写, 避免与进行中的 readWord 位操作产生数据竞争。
func (b *Bus) switchPin(scl, sda string) (bool, string) {
	sc, okS := GPPins[scl]
	sd, okD := GPPins[sda]
	if !okS {
		return false, "未知引脚: " + scl
	}
	if !okD {
		return false, "未知引脚: " + sda
	}
	if scl == sda {
		return false, "SCL 与 SDA 不能是同一根针"
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sclName = scl
	b.sclCfg = sc
	b.sdaName = sda
	b.sdaCfg = sd
	b.initDirs() // 重置方向缓存为新引脚
	b.ok = true
	return true, "ok"
}

// reconfigure 运行时切换 SCL/SDA 并标记为已配置(界面保存)
func (b *Bus) reconfigure(scl, sda *string) (bool, string) {
	ns, nd := b.pinNames()
	if scl != nil {
		ns = *scl
	}
	if sda != nil {
		nd = *sda
	}
	ok, msg := b.switchPin(ns, nd)
	if ok {
		b.setConfigured(true) // 界面设置成功 => GPIO 就绪
		b.busReset()          // 复位电源从机, 清掉之前探测可能留下的错误状态
	}
	return ok, msg
}

// autoDetect 自动扫描候选针脚组合, 找到电源实际接的 PSCL/PSDA (默认扫 Pin19~22)。
// 支持 ctx 取消; 中途取消会恢复探测前引脚。
func (b *Bus) autoDetect(ctx context.Context) (string, string) {
	origScl, origSda := b.pinNames()
	restore := func() {
		b.switchPin(origScl, origSda)
		b.busReset() // 探测结束复位, 让电源回到正常态
	}
	for _, scl := range UIPins {
		for _, sda := range UIPins {
			if scl == sda {
				continue
			}
			if !sleepCtx(ctx, 300*time.Millisecond) { // 等电源稳定, 不做总线复位(避免干扰读数)
				restore()
				return "", ""
			}
			if ok, _ := b.switchPin(scl, sda); !ok {
				continue
			}
			raw := b.readVinWithRetry(ctx, 3, 200*time.Millisecond)
			Logf("  [detect] try %s/SCL + %s/SDA  VIN=%sV%s", scl, sda, ptrStr(linear11Ptr(raw)), rawStr(raw))
			if raw != nil { // 读到非 None 即命中(错误组合全是 None)
				b.busReset()
				return scl, sda
			}
		}
	}
	restore() // 未找到: 恢复探测前引脚
	return "", ""
}

// readVinWithRetry 读 VIN 并带 n 次重试; 供 autoDetect / readable 复用(去重重试逻辑)
func (b *Bus) readVinWithRetry(ctx context.Context, n int, d time.Duration) *uint16 {
	var raw *uint16
	for i := 0; i < n; i++ {
		raw = b.readWord(Cmd["READ_VIN"])
		if raw != nil {
			return raw
		}
		if i < n-1 && !sleepCtx(ctx, d) {
			return nil
		}
	}
	return raw
}

// readable 实际读一次电源, 判断当前接线是否真能读到数据(而非仅 GPIO 方向 OK)。
// 带 ReadableTTL 缓存: 缓存期内不重新读, 避免 /api/config 被前端轮询时持续压总线。
func (b *Bus) readable(ctx context.Context) bool {
	if !b.isConfigured() || !b.isOK() {
		return false
	}
	b.mu.Lock()
	now := time.Now().Unix()
	if now-b.lastTs < ReadableTTL {
		v := b.lastOK
		b.mu.Unlock()
		return v
	}
	b.mu.Unlock()
	ok := b.readVinWithRetry(ctx, 3, 100*time.Millisecond) != nil
	b.mu.Lock()
	b.lastOK = ok
	b.lastTs = time.Now().Unix()
	b.mu.Unlock()
	return ok
}

// sleepCtx 可取消的睡眠; 返回 false 表示 ctx 已取消
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// ---- bit 操作 (调用方须持 mu) ----

func (b *Bus) scl(v int) {
	cur := inb(b.sclCfg.Port)
	if v != 0 {
		cur |= 1 << uint(b.sclCfg.Bit)
	} else {
		cur &^= 1 << uint(b.sclCfg.Bit)
	}
	outb(b.sclCfg.Port, cur)
}

func (b *Bus) sdaWrite(v int) {
	b.ensureSdaOut()
	cur := inb(b.sdaCfg.Port)
	if v != 0 {
		cur |= 1 << uint(b.sdaCfg.Bit)
	} else {
		cur &^= 1 << uint(b.sdaCfg.Bit)
	}
	outb(b.sdaCfg.Port, cur)
}

func (b *Bus) sdaRead() int {
	b.ensureSdaIn()
	return int((inb(b.sdaCfg.Port) >> uint(b.sdaCfg.Bit)) & 1)
}

func (b *Bus) sdaRelease() { b.ensureSdaIn() }

// ---- I2C 时序 (调用方须持 mu) ----

func (b *Bus) i2cStart() {
	b.ensureSclOut()
	b.sdaWrite(1)
	b.scl(1)
	time.Sleep(Delay)
	b.sdaWrite(0)
	time.Sleep(Delay)
	b.scl(0)
	time.Sleep(Delay)
}

func (b *Bus) i2cStop() {
	b.sdaWrite(0)
	b.scl(1)
	time.Sleep(Delay)
	b.sdaRelease()
	time.Sleep(Delay)
}

// busReset SMBus 总线复位: 在当前 SCL 上发 >=9 个时钟脉冲, SDA 释放为高, 结束发 STOP。
// 持 mu 执行, 避免与并发事务的时钟交错产生总线垃圾时序。
func (b *Bus) busReset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensureSclOut()
	b.ensureSdaIn()
	for i := 0; i < 9; i++ {
		b.scl(0)
		time.Sleep(Delay)
		b.scl(1)
		time.Sleep(Delay)
	}
	b.sdaWrite(0)
	b.scl(1)
	time.Sleep(Delay)
	b.sdaRelease()
	time.Sleep(Delay)
	time.Sleep(50 * time.Millisecond)
}

func (b *Bus) i2cWriteByte(v byte) bool {
	for i := 7; i >= 0; i-- {
		b.sdaWrite(int((v >> uint(i)) & 1))
		time.Sleep(Delay)
		b.scl(1)
		time.Sleep(Delay)
		b.scl(0)
		time.Sleep(Delay)
	}
	b.sdaRelease()
	time.Sleep(Delay)
	b.scl(1)
	time.Sleep(Delay)
	ack := 1 - b.sdaRead()
	b.scl(0)
	time.Sleep(Delay)
	return ack == 1
}

func (b *Bus) i2cReadByte(ack bool) int {
	val := 0
	for i := 7; i >= 0; i-- {
		b.scl(1)
		time.Sleep(Delay)
		val = (val << 1) | b.sdaRead()
		b.scl(0)
		time.Sleep(Delay)
	}
	if ack {
		b.sdaWrite(0)
	} else {
		b.sdaRelease()
	}
	time.Sleep(Delay)
	b.scl(1)
	time.Sleep(Delay)
	b.scl(0)
	time.Sleep(Delay)
	b.sdaRelease()
	return val
}

// ---- 事务 (自带加锁) ----

// readWord 读一个 word (从设备先发低字节), 返回 nil 表示失败
func (b *Bus) readWord(cmd byte) *uint16 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.i2cStart()
	if !b.i2cWriteByte(b.addr << 1) {
		b.i2cStop()
		return nil
	}
	if !b.i2cWriteByte(cmd) {
		b.i2cStop()
		return nil
	}
	b.i2cStart()
	if !b.i2cWriteByte((b.addr << 1) | 1) {
		b.i2cStop()
		return nil
	}
	lsb := b.i2cReadByte(true)
	msb := b.i2cReadByte(false)
	b.i2cStop()
	v := uint16(msb<<8 | lsb)
	return &v
}

// readBlock block read (厂商信息), 返回 nil 表示失败
func (b *Bus) readBlock(cmd byte) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.i2cStart()
	if !b.i2cWriteByte(b.addr << 1) {
		b.i2cStop()
		return nil
	}
	if !b.i2cWriteByte(cmd) {
		b.i2cStop()
		return nil
	}
	b.i2cStart()
	if !b.i2cWriteByte((b.addr << 1) | 1) {
		b.i2cStop()
		return nil
	}
	n := b.i2cReadByte(true) // 长度字节 (SMBus block read: 长度须 ACK)
	if n > 32 {
		b.i2cStop()
		return nil
	}
	data := make([]byte, 0, n)
	for i := 0; i < n; i++ {
		data = append(data, byte(b.i2cReadByte(i < n-1)))
	}
	b.i2cStop()
	return data
}

// readMfr 读 MFR 字符串。GPIO bitbang 慢时序下 block read 尾部偶有噪声字节。
// 待真机确认: 尾部噪声很可能是固件在 block read 末尾仍期待 PEC 字节所致(写路径启用了 PEC),
// 读路径标准 SMBus block read 无 PEC。防御策略: 重试 3 次取"多数票"——两次结果完全一致才
// 采纳(噪声随机不会重复, 真值稳定), 全不一致时退回最长可打印串。
// 注: 不能只取"最长"——混入噪声的串恰比干净串长, 会反向污染。
func (b *Bus) readMfr(cmd byte) string {
	seen := make(map[string]int, 3)
	best := ""
	for i := 0; i < 3; i++ {
		time.Sleep(200 * time.Millisecond)
		d := b.readBlock(cmd)
		if d == nil || len(d) == 0 {
			continue
		}
		var sb strings.Builder
		for _, c := range d {
			if c >= 32 && c < 127 {
				sb.WriteByte(c)
			}
		}
		s := strings.Trim(strings.Trim(sb.String(), "#"), " ")
		if s == "" {
			continue
		}
		seen[s]++
		if seen[s] >= 2 { // 两次读数一致: 认定真值, 提前返回
			return s
		}
		if len(s) > len(best) {
			best = s
		}
	}
	return best
}

// writeFanRaw 写 FAN_COMMAND_1 原始值 (带 PEC)。固件启用 PEC, 须附 CRC-8。失败重试 3 次
func (b *Bus) writeFanRaw(lsb, msb byte) bool {
	return b.writeWordCmd(Cmd["FAN_COMMAND_1"], lsb, msb)
}

// writeWordCmd 通用 SMBus Write Word (带 PEC)。失败重试 3 次
func (b *Bus) writeWordCmd(cmd, lsb, msb byte) bool {
	pec := crc8(b.addr<<1, []byte{cmd, lsb, msb})
	for t := 0; t < 3; t++ {
		b.mu.Lock()
		ok := true
		b.i2cStart()
		if !b.i2cWriteByte(b.addr << 1) {
			b.i2cStop()
			b.mu.Unlock()
			time.Sleep(50 * time.Millisecond)
			continue
		}
		for _, by := range []byte{cmd, lsb, msb, pec} {
			if !b.i2cWriteByte(by) {
				ok = false
				break
			}
		}
		b.i2cStop()
		b.mu.Unlock()
		if ok {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// ---- 风扇 ----

func (b *Bus) writeFanPct(pct int) bool {
	if pct < 10 { // 手动指定占比下限 10% (光宝固件 10% 以下实测无效)
		pct = 10
	}
	if pct > 100 {
		pct = 100
	}
	return b.writeFanRaw(byte(pct&0xFF), byte((pct>>8)&0xFF))
}

func (b *Bus) fanRpm() *float64 {
	raw := b.readWord(Cmd["READ_FAN1"])
	return linear11Ptr(raw)
}

func (b *Bus) readTemp1() *float64 {
	raw := b.readWord(Cmd["READ_T1"])
	return linear11Ptr(raw)
}

// setFan 设置风扇控制模式
//
//	manual -> FAN_COMMAND_1=pct, 手动指定占空比
//	auto   -> 恢复电源固件自动温控。实测光宝固件: FAN_COMMAND_1=10% 会触发释放控制、
//	          回到自动温控(约2700转低速档); 0 / 0xFFFF / 0x00FF 均实测无效。10% 即软切回触发值
func (b *Bus) setFan(ctx context.Context, mode string, pct int) bool {
	if mode == "manual" {
		ok := b.writeFanPct(pct)
		if ok {
			b.setFanMode("manual")
		}
		return ok
	}
	// baseline 供后台日志对比; 写入成功即返回, 不阻塞切换响应 (固件调速有迟滞)
	baseline := b.fanRpm()
	ok := b.writeFanPct(10)
	if !ok {
		return ok
	}
	b.setFanMode("auto")
	go func() {
		if !sleepCtx(context.Background(), 3*time.Second) {
			return
		}
		rpm := b.fanRpm()
		Logf("切回自动: 写 FAN_COMMAND_1=10%% -> FAN=%s (手动基线=%s)", ptrStr(rpm), ptrStr(baseline))
	}()
	return ok
}

// ---------------- 纯函数 ----------------

// crc8 SMBus CRC-8 (poly 0x07)
func crc8(addrByte byte, data []byte) byte {
	crc := byte(0)
	for _, b := range append([]byte{addrByte}, data...) {
		crc ^= b
		for i := 0; i < 8; i++ {
			if crc&0x80 != 0 {
				crc = (crc << 1) ^ 0x07
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// linear11 解码 PMBus linear11 word (5bit exp + 11bit 有符号尾数), 保留 2 位小数
func linear11(w uint16) float64 {
	exp := int((w >> 11) & 0x1F)
	if exp >= 16 {
		exp -= 32
	}
	mant := int(w & 0x7FF)
	if mant >= 1024 {
		mant -= 2048
	}
	return round2(float64(mant) * math.Pow(2, float64(exp)))
}

func linear11Ptr(w *uint16) *float64 {
	if w == nil {
		return nil
	}
	v := linear11(*w)
	return &v
}

// readVoutReal 用 VOUT_MODE(0x20) 定义的指数解码真实 VOUT (VOUT 为无符号)
func readVoutReal(b *Bus) *float64 {
	mode := b.readWord(Cmd["VOUT_MODE"])
	if mode != nil && (*mode&0x80) == 0 { // bit7=0 表示 Linear 模式
		exp := int(*mode & 0x1F)
		if exp >= 16 {
			exp -= 32
		}
		raw := b.readWord(Cmd["READ_VOUT"])
		if raw != nil {
			v := round2(float64(*raw) * math.Pow(2, float64(exp)))
			return &v
		}
	}
	return nil
}

// ---------------- 四舍五入 helpers (对齐 Python round 显示精度) ----------------

func round1(x float64) float64 { return math.Round(x*10) / 10 }
func round2(x float64) float64 { return math.Round(x*100) / 100 }
func round3(x float64) float64 { return math.Round(x*1000) / 1000 }

func fval(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
func ptrStr(p *float64) string {
	if p == nil {
		return "None"
	}
	return fmt.Sprintf("%g", *p)
}
func rawStr(raw *uint16) string {
	if raw == nil {
		return ""
	}
	return fmt.Sprintf(" (raw=0x%04X)", *raw)
}

// ---------------- 全局状态 ----------------

var (
	mfrMu    sync.Mutex
	mfrID    string
	mfrModel string
	mfrTs    int64
)

func getMfr(b *Bus) (string, string) {
	now := time.Now().Unix()
	mfrMu.Lock()
	defer mfrMu.Unlock()
	if mfrTs > 0 && now-mfrTs < MfrCacheTTL {
		return mfrID, mfrModel
	}
	id := b.readMfr(Cmd["MFR_ID"])
	model := b.readMfr(Cmd["MFR_MODEL"])
	if id != "" || model != "" {
		if id != "" {
			mfrID = id
		}
		if model != "" {
			mfrModel = model
		}
		mfrTs = now
	}
	return mfrID, mfrModel
}
