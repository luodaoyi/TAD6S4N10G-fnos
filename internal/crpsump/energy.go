package crpsump

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"
)

// ---------------- 耗电量统计 ----------------
//
// 模型: 后台每 15s 采样一次 READ_PIN, 用矩形积分 (功率×时长) 累计耗电量,
// 按 "总量 / 今日 / 本月 / 按日明细" 四个维度累计, 周期落盘 energy.json, 重启不丢。
// 单次采样间隔超过 EnergyMaxGap (关机/挂起/长时间读不到) 不计入, 避免把中断时间算成耗电;
// 服务重启后若距上次落盘 <= EnergyMaxGap, 按断电前功率补计这段时间 (机器确实在耗电)。

const (
	EnergySampleInterval = 15 * time.Second // PIN 采样周期
	EnergySaveInterval   = 5 * time.Minute  // 落盘周期
	EnergyMaxGap         = 10 * time.Minute // 采样间隔上限: 超出视为中断, 不计入
	EnergyDaysKeep       = 400              // 按日明细保留天数 (~13 个月)
)

type EnergyStats struct {
	mu        sync.Mutex
	path      string
	totalWh   float64
	sessionWh float64            // 本次运行累计
	firstTs   int64              // 统计起始时间
	updatedTs int64              // 最后一次累计时间
	lastPin   float64            // 最近一次有效采样功率 (W)
	hasPin    bool               // 是否已有有效功率采样
	days      map[string]float64 // "2006-01-02" -> Wh
	months    map[string]float64 // "2006-01"    -> Wh
	dirty     bool
	// resetPending: Reset 后首个 Sample 只重建时间基准不积分——采样器的 dt 跨越
	// 清零时刻, 直接积分会把清零前的时段补记进新统计
	resetPending bool
}

var energyStats *EnergyStats // 全局单例, main 里初始化; readAll 里刷新 lastPin

type energyFile struct {
	TotalWh   float64            `json:"total_wh"`
	FirstTs   int64              `json:"first_ts"`
	UpdatedTs int64              `json:"updated_ts"`
	LastPinW  float64            `json:"last_pin_w"`
	HasPin    bool               `json:"has_pin"`
	Days      map[string]float64 `json:"days"`
	Months    map[string]float64 `json:"months"`
}

// NewEnergy 创建耗电统计实例并载入既有落盘数据
func NewEnergy(path string) *EnergyStats {
	e := &EnergyStats{
		path:   path,
		days:   map[string]float64{},
		months: map[string]float64{},
	}
	e.load()
	return e
}

// load 从磁盘恢复统计; 文件缺失/损坏忽略, 短暂中断期按断电前功率补计
func (e *EnergyStats) load() {
	data, err := os.ReadFile(e.path)
	if err != nil {
		return
	}
	var f energyFile
	if json.Unmarshal(data, &f) != nil || f.Days == nil || f.Months == nil {
		Logf("警告: 耗电统计文件损坏, 忽略并重新累计: %s", e.path)
		return
	}
	e.totalWh = f.TotalWh
	e.firstTs = f.FirstTs
	e.updatedTs = f.UpdatedTs
	e.lastPin = f.LastPinW
	e.hasPin = f.HasPin
	e.days = f.Days
	e.months = f.Months
	if e.days == nil {
		e.days = map[string]float64{}
	}
	if e.months == nil {
		e.months = map[string]float64{}
	}
	// 中断补计: 距上次落盘 <= EnergyMaxGap 视为运行中断(如升级重启), 按断电前功率补上;
	// 超过上限视为整机关机过, 不补。
	if e.hasPin && e.updatedTs > 0 {
		gap := time.Since(time.Unix(e.updatedTs, 0))
		if gap > 0 && gap <= EnergyMaxGap {
			e.addWh(e.lastPin*gap.Hours(), time.Now())
			Logf("耗电统计: 补计中断期间 %.1f 分钟 ≈ %.4f kWh", gap.Minutes(), e.lastPin*gap.Hours()/1000)
		}
	}
	Logf("耗电统计已载入: 累计 %.3f kWh, 按日 %d 天 (%s)", e.totalWh/1000, len(e.days), e.path)
}

// addWh 把 wh 累计进总量/当日/当月 (调用方须持 mu)
func (e *EnergyStats) addWh(wh float64, now time.Time) {
	if wh <= 0 {
		return
	}
	e.totalWh += wh
	e.sessionWh += wh
	if e.firstTs == 0 {
		e.firstTs = now.Unix()
	}
	e.updatedTs = now.Unix()
	e.days[now.Format("2006-01-02")] += wh
	e.months[now.Format("2006-01")] += wh
	for len(e.days) > EnergyDaysKeep { // 按日明细超限: 删最旧
		oldest := ""
		for k := range e.days {
			if oldest == "" || k < oldest {
				oldest = k
			}
		}
		delete(e.days, oldest)
	}
	e.dirty = true
}

// Sample 记录一次功率采样: 用距上次采样的时长做矩形积分
func (e *EnergyStats) Sample(pin float64, dt time.Duration) {
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastPin = pin
	e.hasPin = true
	if e.resetPending { // 清零后首个采样: dt 跨越清零时刻, 只建基准不积分
		e.resetPending = false
		e.updatedTs = now.Unix()
		e.dirty = true
		return
	}
	if dt > 0 && dt <= EnergyMaxGap {
		e.addWh(pin*dt.Hours(), now)
	} else {
		e.updatedTs = now.Unix() // 间隔异常: 只刷新时间基准, 不计电量
	}
}

// UpdatePin 只刷新最近功率显示 (readAll 每次成功读 PIN 时调用, 不积分)
func (e *EnergyStats) UpdatePin(pin float64) {
	e.mu.Lock()
	e.lastPin = pin
	e.hasPin = true
	e.mu.Unlock()
}

// Snapshot 生成 /api/energy JSON
func (e *EnergyStats) Snapshot() map[string]any {
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	dayKeys := make([]string, 0, len(e.days))
	for k := range e.days {
		dayKeys = append(dayKeys, k)
	}
	sort.Strings(dayKeys)
	if len(dayKeys) > 30 {
		dayKeys = dayKeys[len(dayKeys)-30:]
	}
	days := make([]map[string]any, 0, len(dayKeys))
	for _, k := range dayKeys {
		days = append(days, map[string]any{"date": k, "wh": round3(e.days[k])})
	}
	var pin any
	if e.hasPin {
		pin = round1(e.lastPin)
	}
	return map[string]any{
		"ok":          true,
		"total_wh":    round3(e.totalWh),
		"today_wh":    round3(e.days[now.Format("2006-01-02")]),
		"month_wh":    round3(e.months[now.Format("2006-01")]),
		"session_wh":  round3(e.sessionWh),
		"pin_w":       pin,
		"days":        days,
		"first_ts":    e.firstTs,
		"updated_ts":  e.updatedTs,
		"ts":          now.Unix(),
		"app_version": AppVersion,
	}
}

// Reset 统计清零 (总量/本次运行/按日/按月全部归零) 并立即落盘;
// 落盘失败返回错误 (内存已清零, 但调用方须告知用户持久化未成功, 避免重启后旧统计复活)
func (e *EnergyStats) Reset() error {
	e.mu.Lock()
	e.totalWh, e.sessionWh = 0, 0
	e.firstTs, e.updatedTs = 0, 0
	e.lastPin, e.hasPin = 0, false
	e.days = map[string]float64{}
	e.months = map[string]float64{}
	e.dirty = true
	e.resetPending = true
	e.mu.Unlock()
	if err := e.save(); err != nil {
		return err
	}
	Logf("耗电统计已清零")
	return nil
}

// Save 落盘 (忽略错误: 退出回调与后台周期保存用)
func (e *EnergyStats) Save() {
	_ = e.save()
}

// save 落盘: 临时文件 + rename 原子替换, 防写一半损坏; 返回错误供 Reset 上报
func (e *EnergyStats) save() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.dirty {
		return nil
	}
	f := energyFile{
		TotalWh: e.totalWh, FirstTs: e.firstTs, UpdatedTs: e.updatedTs,
		LastPinW: e.lastPin, HasPin: e.hasPin,
		Days: e.days, Months: e.months,
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := e.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		Logf("耗电统计保存失败: %v", err)
		return err
	}
	if err := os.Rename(tmp, e.path); err != nil {
		Logf("耗电统计保存失败: %v", err)
		return err
	}
	e.dirty = false
	return nil
}

// energySampler 后台采样循环: 每 EnergySampleInterval 读一次 PIN 并积分, 周期落盘。
// 与前端轮询共用总线 (mutex 串行化), 每周期只多 1 次 readWord, 开销可忽略。
func energySampler(bus *Bus) {
	last := time.Now()
	save := time.NewTicker(EnergySaveInterval)
	defer save.Stop()
	Logf("耗电统计采样已启动: 每 %s 采样 READ_PIN, 每 %s 落盘", EnergySampleInterval, EnergySaveInterval)
	for {
		select {
		case <-save.C:
			energyStats.Save()
		case <-time.After(EnergySampleInterval):
		}
		now := time.Now()
		dt := now.Sub(last)
		last = now
		if !bus.isConfigured() || !bus.isOK() {
			continue
		}
		if pin := linear11Ptr(bus.readWord(Cmd["READ_PIN"])); pin != nil && *pin >= 0 && *pin < 10000 {
			energyStats.Sample(*pin, dt)
		}
	}
}

// StartEnergy 初始化全局耗电统计并启动后台采样循环 (backend 入口调用),
// 返回实例供退出前落盘。
func StartEnergy(bus *Bus, path string) *EnergyStats {
	energyStats = NewEnergy(path)
	go energySampler(bus)
	return energyStats
}
