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

// Package crpsump 实现 CRPS 电源 PMBus 监控: GPIO bitbang I2C 总线、PMBus 读写、
// 风扇控制、耗电统计与 HTTP API; 由主应用 (backend/powerguard) 挂载为 /api/crps/*。
package crpsump

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------- 常量 ----------------

const (
	DefaultAddr   = 0x5b                   // 光宝 PS-2551-5L PMBus 从机地址
	LogMaxSize    = 1 << 20                // 日志单文件上限 1MB, 超出轮转
	Delay         = 100 * time.Microsecond // bitbang 半周期
	AppVersion    = "1.0.49"
	MfrCacheTTL   = 60 // 秒
	DefaultSCL    = "Pin19"
	DefaultSDA    = "Pin20"
	GatewayPrefix = "/app/psu-monitor"
	ReadableTTL   = 2 // 秒: /api/config 的 readable 缓存, 避免轮询持续压总线
)

// LogPath 运行日志路径 (var 以便宿主按自身日志策略覆盖)
var LogPath = "/tmp/psu-monitor.log"

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
	pio        portIO // 端口实现 (默认真实硬件, 单测可 mock)
	sclOut     bool   // 方向缓存
	sdaOut     bool
	driven     map[string]bool // 实际驱动为输出的针脚名 (stateMu 保护): 切针时逐一释放, 释放失败不丢失追踪
	ioErr      error           // 当前事务首个端口写失败 (mu 保护); 事务末尾检查, 失败则整个事务作废
	mu         sync.Mutex      // 总线 I/O 与引脚配置
	stateMu    sync.Mutex      // configured / fanMode / readable 缓存
	ok         bool
	configured bool // 是否已在界面设置好 PSCL/PSDA
	fanMode    string
	lastOK     bool // readable() TTL 缓存
	lastTs     int64
}

// NewBus 创建 GPIO bitbang PMBus 总线: 校验引脚名, configured 时初始化方向寄存器。
// 原始引脚名非法或两针相同时按未配置处理 (configured=false): 垃圾 crps.json 不得
// 导致启动时向(回退后的)引脚写 GPIO 方向。
func NewBus(scl, sda string, configured bool) *Bus {
	configuredPins := configured
	// 校验引脚名: 非法名会令 GPPins 返回零值 GPPin{0,0,0,0}, 导致向 port 0x000(DMA 区) 写入
	if _, ok := GPPins[scl]; !ok {
		configuredPins = false
		Logf("警告: 非法 SCL 引脚 '%s', 回退默认 %s", scl, DefaultSCL)
		scl = DefaultSCL
	}
	if _, ok := GPPins[sda]; !ok {
		configuredPins = false
		Logf("警告: 非法 SDA 引脚 '%s', 回退默认 %s", sda, DefaultSDA)
		sda = DefaultSDA
	}
	if scl == sda {
		configuredPins = false
		Logf("警告: SCL/SDA 不能同一根针, SDA 回退默认 %s", DefaultSDA)
		sda = DefaultSDA
	}
	b := &Bus{
		addr:       DefaultAddr,
		sclName:    scl,
		sdaName:    sda,
		sclCfg:     GPPins[scl],
		sdaCfg:     GPPins[sda],
		pio:        defaultPortIO,
		driven:     map[string]bool{},
		configured: configuredPins,
		fanMode:    "auto",
	}
	// /dev/port 不可达 (无 root/非 Linux/容器限制): ok=false, 接口降级 no-gpio,
	// 不再做无谓的方向写入与后续大量失败的系统调用
	if !portAvailable() {
		Logf("警告: 无法打开 /dev/port, CRPS GPIO 不可用, 接口降级 no-gpio")
		return b
	}
	b.ok = true
	// 初始化: SCL=输出, SDA=输入(释放)。
	// 仅在已配置时写方向寄存器: 未配置的机器(可能无 CRPS 硬件)启动不得触碰 GPIO 方向,
	// 首次界面保存/探测成功(reconfigure)时才写。方向写失败同样降级 no-gpio。
	if configuredPins {
		if err := b.initDirs(); err != nil {
			Logf("警告: GPIO 方向初始化失败: %v, 接口降级 no-gpio", err)
			b.ok = false
		}
	}
	return b
}

// setPinDir 写 SIO 方向寄存器 (被带缓存的 ensure* 调用), name 为针脚名 (driven 追踪用)。
// 流程为读-改-写: 选 LDN7 -> 读方向寄存器当前值 -> 改目标位 -> 写回。
// 索引写入与数据读取任一失败都向上传递且禁止写回: 读失败时拿到的字节不可信
// (可能是旧索引残留值), 写回会把同组其他针脚的方向一起改错。读失败也不更新
// driven 追踪与方向缓存, 下次操作会重试。整段 sioEnter..sioExit 在 PortIOLocker
// 下进行, 与宿主 GPIO 访问串行化; sioExit 用 defer 保证中途失败也退出配置模式。
// 调用方须持 b.mu (NewBus 构造期除外); driven 由 stateMu 保护, 本函数自行加锁。
func (b *Bus) setPinDir(name string, cfg GPPin, out bool) (err error) {
	PortIOLocker.Lock()
	defer PortIOLocker.Unlock()
	if err = sioEnter(b.pio); err != nil {
		return err
	}
	defer func() { // 保留首个写错误; 无错时采用 sioExit 的错误
		if exitErr := sioExit(b.pio); err == nil {
			err = exitErr
		}
	}()
	if err = sioWrite(b.pio, 0x07, 0x07); err != nil { // 选 LDN7 (GPIO)
		return err
	}
	cur, err := sioRead(b.pio, byte(cfg.DirReg))
	if err != nil { // 读失败: 禁止写回方向寄存器, 禁止更新缓存/追踪
		return err
	}
	if out {
		cur |= 1 << uint(cfg.DirBit)
	} else {
		cur &^= 1 << uint(cfg.DirBit)
	}
	if err = sioWrite(b.pio, byte(cfg.DirReg), cur); err != nil {
		return err
	}
	// 方向写回成功才更新追踪: out=true 记录为已驱动, 释放(out=false)后移除
	b.stateMu.Lock()
	if out {
		b.driven[name] = true
	} else {
		delete(b.driven, name)
	}
	b.stateMu.Unlock()
	return nil
}

// ensure* 只在方向缓存与实际不符时才写 SIO (方向寄存器缓存); 写失败不更新缓存,
// 返回错误由调用方 (bit 操作/事务) 记录上报, 下次操作会重试写方向
func (b *Bus) ensureSclOut() error {
	if b.sclOut {
		return nil
	}
	if err := b.setPinDir(b.sclName, b.sclCfg, true); err != nil {
		return err
	}
	b.sclOut = true
	return nil
}

// ensureSdaOut 确保 SDA 引脚为输出方向 (方向缓存命中时不写 SIO)
func (b *Bus) ensureSdaOut() error {
	if b.sdaOut {
		return nil
	}
	if err := b.setPinDir(b.sdaName, b.sdaCfg, true); err != nil {
		return err
	}
	b.sdaOut = true
	return nil
}

// ensureSdaIn 确保 SDA 引脚为输入方向 (方向缓存命中时不写 SIO)
func (b *Bus) ensureSdaIn() error {
	if !b.sdaOut {
		return nil
	}
	if err := b.setPinDir(b.sdaName, b.sdaCfg, false); err != nil {
		return err
	}
	b.sdaOut = false
	return nil
}

// drivenPins 快照当前已驱动为输出的针脚名 (stateMu 保护), 排序保证遍历顺序稳定
func (b *Bus) drivenPins() []string {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	names := make([]string, 0, len(b.driven))
	for n := range b.driven {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// isDriven 查询某针脚是否在已驱动追踪中 (stateMu 保护)
func (b *Bus) isDriven(name string) bool {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	return b.driven[name]
}

// initDirs 初始化引脚方向: SCL=输出, SDA=输入(释放); 任一步失败返回错误。
// SDA 设置失败时安全回滚: 把已设为输出的 SCL 释放回输入, 不遗留半初始化输出脚;
// 回滚也失败时 SCL 仍留在 driven 追踪中 (setPinDir 失败不动追踪), 后续
// switchPin 会重试释放, 不会丢失清理状态。
func (b *Bus) initDirs() error {
	if err := b.setPinDir(b.sclName, b.sclCfg, true); err != nil {
		return err
	}
	b.sclOut = true
	if err := b.setPinDir(b.sdaName, b.sdaCfg, false); err != nil {
		if rbErr := b.setPinDir(b.sclName, b.sclCfg, false); rbErr != nil {
			Logf("警告: initDirs 回滚 SCL 失败: %v (原错误: %v), 针脚保持追踪待重试释放", rbErr, err)
		} else {
			b.sclOut = false
		}
		return err
	}
	b.sdaOut = false
	return nil
}

// ---- 状态字段访问 (stateMu) ----

// isConfigured 读 configured 状态 (stateMu 保护)
func (b *Bus) isConfigured() bool { b.stateMu.Lock(); defer b.stateMu.Unlock(); return b.configured }

// setConfigured 写 configured 状态 (stateMu 保护)
func (b *Bus) setConfigured(v bool) {
	b.stateMu.Lock()
	b.configured = v
	b.stateMu.Unlock()
}

// fanModeNow 读当前风扇模式 (stateMu 保护)
func (b *Bus) fanModeNow() string { b.stateMu.Lock(); defer b.stateMu.Unlock(); return b.fanMode }

// setFanMode 写风扇模式 (stateMu 保护)
func (b *Bus) setFanMode(v string) {
	b.stateMu.Lock()
	b.fanMode = v
	b.stateMu.Unlock()
}

// isOK 读总线硬件可用状态 (持 mu)
func (b *Bus) isOK() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.ok }

// pinNames 加锁读当前引脚名 (配合 switchPin 在锁内改写)
func (b *Bus) pinNames() (string, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sclName, b.sdaName
}

// switchPin 切换 SCL/SDA 引脚并初始化方向, 不改变 configured 状态。
// 引脚配置字段在持锁状态下改写, 避免与进行中的 readWord 位操作产生数据竞争。
// 释放旧引脚失败时如实报失败: 不覆盖 sclName/sdaName (旧配置原样保留), 旧脚
// 仍留在 driven 追踪中, 下次切换会重试释放——绝不报成功却遗留输出脚。
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
	// 释放不再使用的旧引脚为输入, 避免 F_PANEL 针脚残留推挽输出与外部线路冲突。
	// 只释放本总线实际驱动过的针 (driven 追踪): 从未驱动过的引脚不触碰任何 GPIO。
	// 任一释放失败都中止切换并上报, 避免覆盖引脚字段后把旧脚遗忘在输出状态。
	for _, old := range b.drivenPins() {
		if old != scl && old != sda {
			if err := b.setPinDir(old, GPPins[old], false); err != nil {
				return false, "释放旧引脚 " + old + " 失败: " + err.Error()
			}
		}
	}
	b.sclName = scl
	b.sclCfg = sc
	b.sdaName = sda
	b.sdaCfg = sd
	if !portAvailable() { // 硬件不可达: 如实报失败, 不得标记可用后又持续 no-gpio
		b.ok = false
		return false, "无法打开 /dev/port, GPIO 不可用"
	}
	if err := b.initDirs(); err != nil { // 方向写失败: 如实报失败, 不假装就绪
		b.ok = false
		return false, "GPIO 方向写入失败: " + err.Error()
	}
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
	if !ok {
		return ok, msg
	}
	if err := b.busReset(); err != nil { // 复位电源从机, 清掉之前探测可能留下的错误状态
		return false, "GPIO 写入失败(总线复位): " + err.Error()
	}
	b.setConfigured(true) // 界面设置成功 => GPIO 就绪
	return true, "ok"
}

// autoDetect 自动扫描候选针脚组合, 找到电源实际接的 PSCL/PSDA (默认扫 Pin19~22)。
// 支持 ctx 取消; 中途取消会恢复探测前引脚。
// 第三个返回值是收尾清理错误: 释放扫描期间驱动过的针脚失败时如实返回,
// 调用方必须上报 (针脚可能仍为输出); driven 追踪保留, 下次探测/切换会重试释放。
func (b *Bus) autoDetect(ctx context.Context) (string, string, error) {
	origScl, origSda := b.pinNames()
	// restore 收尾分两种: 已配置 -> switchPin 恢复原引脚(其内部持 b.mu 释放扫描
	// 期间驱动过的其它针脚)+ busReset; 未配置 -> 只把扫描期间驱动过的针脚全部
	// 释放回输入, 不驱动任何引脚——switchPin/busReset 都会把原 SCL 设为输出,
	// 未配置机器不该因此残留输出脚 (探测取消/无匹配时尤其如此)。释放走
	// setPinDir, 须持 b.mu 调用 (锁约定); 释放失败保留 driven 追踪并聚合上报,
	// 下次重试。未配置分支同时把引脚字段还原为 orig (不写方向), 否则残留的
	// 扫描组合会让 reconfigure 只传一侧引脚时拼出错误搭档。
	restore := func() error {
		if !b.isConfigured() {
			b.mu.Lock()
			defer b.mu.Unlock()
			var failed []string
			for _, name := range b.drivenPins() {
				if err := b.setPinDir(name, GPPins[name], false); err != nil {
					failed = append(failed, name)
				}
			}
			b.sclName = origScl
			b.sclCfg = GPPins[origScl]
			b.sdaName = origSda
			b.sdaCfg = GPPins[origSda]
			if len(failed) > 0 {
				return fmt.Errorf("针脚 %s 未能释放回输入", strings.Join(failed, ", "))
			}
			return nil
		}
		if ok, msg := b.switchPin(origScl, origSda); !ok {
			return fmt.Errorf("恢复原引脚失败: %s", msg)
		}
		if err := b.busReset(); err != nil {
			// 复位末尾 sdaRelease 的端口写失败会让 SDA 滞留输出, 必须如实上报
			return fmt.Errorf("总线复位失败: %w", err)
		}
		return nil
	}
	for _, scl := range UIPins {
		for _, sda := range UIPins {
			if scl == sda {
				continue
			}
			if !sleepCtx(ctx, 300*time.Millisecond) { // 等电源稳定, 不做总线复位(避免干扰读数)
				return "", "", restore()
			}
			if ok, _ := b.switchPin(scl, sda); !ok {
				continue
			}
			raw := b.readVinWithRetry(ctx, 3, 200*time.Millisecond)
			Logf("  [detect] try %s/SCL + %s/SDA  VIN=%sV%s", scl, sda, ptrStr(linear11Ptr(raw)), rawStr(raw))
			if raw != nil { // 读到非 None 即命中(错误组合全是 None)
				b.busReset()
				return scl, sda, nil
			}
		}
	}
	return "", "", restore() // 未找到: 恢复探测前引脚
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
//
// 每处数据口的读-改-写都在 PortIOLocker 临界区内完成, 与宿主的按键轮询/LED 反馈
// (同处 0xA0x GPIO 块) 串行化; 锁粒度为单次 RMW, bitbang 位间可被宿主轮询插入,
// 不影响 I2C 时序正确性 (Delay 为软件定时)。
// 端口写失败不逐位返回 (避免时序函数签名爆炸), 统一记入 ioErr, 由事务层
// (readWord/readBlock/writeWordCmd/busReset) 末尾检查并使整个事务失败。

// setIOErr 记录本次事务首个端口写失败 (调用方须持 mu); nil 入参为空操作
func (b *Bus) setIOErr(err error) {
	if err != nil && b.ioErr == nil {
		b.ioErr = err
	}
}

// takeIOErr 取出并清零事务错误 (调用方须持 mu)
func (b *Bus) takeIOErr() error {
	err := b.ioErr
	b.ioErr = nil
	return err
}

// scl 写 SCL 电平 (v!=0 为高, 0 为低)。数据口读失败时中止: 读-改-写回会把
// 不可信字节写回数据口, 破坏同组其他 GPIO 的电平
func (b *Bus) scl(v int) {
	PortIOLocker.Lock()
	cur, err := b.pio.inb(b.sclCfg.Port)
	if err != nil {
		b.setIOErr(err)
		PortIOLocker.Unlock()
		return
	}
	if v != 0 {
		cur |= 1 << uint(b.sclCfg.Bit)
	} else {
		cur &^= 1 << uint(b.sclCfg.Bit)
	}
	b.setIOErr(b.pio.outb(b.sclCfg.Port, cur))
	PortIOLocker.Unlock()
}

// sdaWrite 写 SDA 电平 (自动确保 SDA 为输出方向); 方向或数据口读/写失败记入 ioErr
func (b *Bus) sdaWrite(v int) {
	if err := b.ensureSdaOut(); err != nil {
		b.setIOErr(err)
		return
	}
	PortIOLocker.Lock()
	cur, err := b.pio.inb(b.sdaCfg.Port)
	if err != nil {
		b.setIOErr(err)
		PortIOLocker.Unlock()
		return
	}
	if v != 0 {
		cur |= 1 << uint(b.sdaCfg.Bit)
	} else {
		cur &^= 1 << uint(b.sdaCfg.Bit)
	}
	b.setIOErr(b.pio.outb(b.sdaCfg.Port, cur))
	PortIOLocker.Unlock()
}

// sdaRead 读 SDA 电平 (自动确保 SDA 为输入方向); 方向或数据口读失败记入 ioErr
// 并按高电平返回 (失败读到的电平不可信, 按释放态处理)
func (b *Bus) sdaRead() int {
	if err := b.ensureSdaIn(); err != nil {
		b.setIOErr(err)
		return 1
	}
	PortIOLocker.Lock()
	v, err := b.pio.inb(b.sdaCfg.Port)
	PortIOLocker.Unlock()
	if err != nil {
		b.setIOErr(err)
		return 1
	}
	return int((v >> uint(b.sdaCfg.Bit)) & 1)
}

// sdaRelease 释放 SDA (切为输入, 由外部上拉拉高)
func (b *Bus) sdaRelease() { b.setIOErr(b.ensureSdaIn()) }

// ---- I2C 时序 (调用方须持 mu) ----

// i2cStart 产生 I2C START 时序
func (b *Bus) i2cStart() {
	b.setIOErr(b.ensureSclOut())
	b.sdaWrite(1)
	b.scl(1)
	time.Sleep(Delay)
	b.sdaWrite(0)
	time.Sleep(Delay)
	b.scl(0)
	time.Sleep(Delay)
}

// i2cStop 产生 I2C STOP 时序
func (b *Bus) i2cStop() {
	b.sdaWrite(0)
	b.scl(1)
	time.Sleep(Delay)
	b.sdaRelease()
	time.Sleep(Delay)
}

// busReset SMBus 总线复位: 在当前 SCL 上发 >=9 个时钟脉冲, SDA 释放为高, 结束发 STOP。
// 持 mu 执行, 避免与并发事务的时钟交错产生总线垃圾时序。返回端口写错误 (nil 为成功)。
func (b *Bus) busReset() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ioErr = nil
	b.setIOErr(b.ensureSclOut())
	b.setIOErr(b.ensureSdaIn())
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
	return b.takeIOErr()
}

// i2cWriteByte 写一个字节并读 ACK, 返回是否被从机应答
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

// i2cReadByte 读一个字节; ack=true 应答继续读, false 不应答结束
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

// readWord 读一个 word (从设备先发低字节), 返回 nil 表示失败 (含端口写失败)
func (b *Bus) readWord(cmd byte) *uint16 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ioErr = nil
	b.i2cStart()
	if !b.i2cWriteByte(b.addr << 1) {
		b.i2cStop()
		b.takeIOErr()
		return nil
	}
	if !b.i2cWriteByte(cmd) {
		b.i2cStop()
		b.takeIOErr()
		return nil
	}
	b.i2cStart()
	if !b.i2cWriteByte((b.addr << 1) | 1) {
		b.i2cStop()
		b.takeIOErr()
		return nil
	}
	lsb := b.i2cReadByte(true)
	msb := b.i2cReadByte(false)
	b.i2cStop()
	if b.takeIOErr() != nil {
		return nil
	}
	v := uint16(msb<<8 | lsb)
	return &v
}

// readBlock block read (厂商信息), 返回 nil 表示失败 (含端口写失败)
func (b *Bus) readBlock(cmd byte) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ioErr = nil
	b.i2cStart()
	if !b.i2cWriteByte(b.addr << 1) {
		b.i2cStop()
		b.takeIOErr()
		return nil
	}
	if !b.i2cWriteByte(cmd) {
		b.i2cStop()
		b.takeIOErr()
		return nil
	}
	b.i2cStart()
	if !b.i2cWriteByte((b.addr << 1) | 1) {
		b.i2cStop()
		b.takeIOErr()
		return nil
	}
	n := b.i2cReadByte(true) // 长度字节 (SMBus block read: 长度须 ACK)
	if n > 32 {
		b.i2cStop()
		b.takeIOErr()
		return nil
	}
	data := make([]byte, 0, n)
	for i := 0; i < n; i++ {
		data = append(data, byte(b.i2cReadByte(i < n-1)))
	}
	b.i2cStop()
	if b.takeIOErr() != nil {
		return nil
	}
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
		b.ioErr = nil
		ok := true
		b.i2cStart()
		if !b.i2cWriteByte(b.addr << 1) {
			b.i2cStop()
			b.takeIOErr()
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
		if b.takeIOErr() != nil { // 端口写失败: 本次作废, 重试
			ok = false
		}
		b.mu.Unlock()
		if ok {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// ---- 风扇 ----

// writeFanPct 按百分比写风扇占空比 (钳制到 10~100)
func (b *Bus) writeFanPct(pct int) bool {
	if pct < 10 { // 手动指定占比下限 10% (光宝固件 10% 以下实测无效)
		pct = 10
	}
	if pct > 100 {
		pct = 100
	}
	return b.writeFanRaw(byte(pct&0xFF), byte((pct>>8)&0xFF))
}

// fanRpm 读风扇转速 (RPM), 失败返回 nil
func (b *Bus) fanRpm() *float64 {
	raw := b.readWord(Cmd["READ_FAN1"])
	return linear11Ptr(raw)
}

// readTemp1 读进风口温度 T1 (°C), 失败返回 nil
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
	// 光宝固件: FAN_COMMAND_1=10% 即释放手动、回自动温控的触发值。手动 10% 与"自动"
	// 写的是同一值, 模式须如实记为 auto, 否则 fan_mode 与电源真实状态不符。
	if mode == "manual" && pct <= 10 {
		mode = "auto"
	}
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

// linear11Ptr linear11 的指针版: nil 入 nil 出, 便于读数失败透传
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
	if mode == nil || (*mode&0x80) != 0 { // bit7=0 才表示 Linear 模式
		return nil
	}
	return decodeVout(mode, b.readWord(Cmd["READ_VOUT"]))
}

// decodeVout VOUT_MODE Linear 模式指数解码 (纯函数, 便于单测)
func decodeVout(mode, raw *uint16) *float64 {
	if mode == nil || raw == nil || (*mode&0x80) != 0 {
		return nil
	}
	exp := int(*mode & 0x1F)
	if exp >= 16 {
		exp -= 32
	}
	v := round2(float64(*raw) * math.Pow(2, float64(exp)))
	return &v
}

// ---------------- 四舍五入 helpers (对齐 Python round 显示精度) ----------------

// round1 保留 1 位小数
func round1(x float64) float64 { return math.Round(x*10) / 10 }

// round2 保留 2 位小数
func round2(x float64) float64 { return math.Round(x*100) / 100 }

// round3 保留 3 位小数
func round3(x float64) float64 { return math.Round(x*1000) / 1000 }

// fval *float64 转 any (nil 透传), 供 JSON 输出
func fval(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// ptrStr 格式化浮点指针为日志文本 (nil 显示 "None")
func ptrStr(p *float64) string {
	if p == nil {
		return "None"
	}
	return fmt.Sprintf("%g", *p)
}

// rawStr 格式化原始 word 为日志后缀 (nil 返回空串)
func rawStr(raw *uint16) string {
	if raw == nil {
		return ""
	}
	return fmt.Sprintf(" (raw=0x%04X)", *raw)
}

// ---------------- 全局状态 ----------------

var (
	mfrMu     sync.Mutex
	mfrID     string
	mfrModel  string
	mfrTs     int64
	mfrFailTs int64 // 读取失败负缓存时间戳: 失败时每次 readAll 重试会阻塞 ~1.2s+ 并霸占 mfrMu
)

// MfrFailTTL 失败负缓存时长 (秒): 远短于成功缓存, 电源接回后很快恢复真实读取
const MfrFailTTL = 10

// getMfr 读厂商 ID/型号 (带成功缓存与失败负缓存, 防轮询阻塞总线)
func getMfr(b *Bus) (string, string) {
	now := time.Now().Unix()
	mfrMu.Lock()
	defer mfrMu.Unlock()
	if mfrTs > 0 && now-mfrTs < MfrCacheTTL {
		return mfrID, mfrModel
	}
	if mfrFailTs > 0 && now-mfrFailTs < MfrFailTTL { // 失败负缓存期内直接返回旧值
		return mfrID, mfrModel
	}
	id := b.readMfr(Cmd["MFR_ID"])
	model := b.readMfr(Cmd["MFR_MODEL"])
	if id == "" && model == "" {
		mfrFailTs = now
		return mfrID, mfrModel
	}
	mfrFailTs = 0
	if id != "" {
		mfrID = id
	}
	if model != "" {
		mfrModel = model
	}
	mfrTs = now
	return mfrID, mfrModel
}
