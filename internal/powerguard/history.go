package powerguard

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	// 纯 Go 的 SQLite 驱动：无 CGO，交叉编译不受影响；WAL 模式保证断电后
	// 数据文件要么是旧状态要么是新状态。
	_ "modernc.org/sqlite"
)

// 历史采样参数：60 秒一个点，单文件 history.db。满配设备每分钟约 0.7KB
// （主表 45B + 风扇 165B + 盘位 180B + 传感器 315B），按默认保留 30 天约
// 30–45MB，默认大小上限 64MB 是两倍余量；超限后按天删最旧数据。
// 保留期（retention_days，默认 30、下限 1、不设实际上限）由用户配置；磁盘
// 体积由大小上限兜底（超限按天删最旧），所以不限期是安全的。仅保留一个
// 防溢出护栏：time.Duration 按纳秒计，天数超过约 292 年会溢出为负数、把
// 清理 cutoff 推到未来导致清空全部数据，故荒谬大值钳到 36500（100 年）。
// 存储层实际多留 2 天缓冲（historyRetentionBufferDays）：日期/大小清理以
// 天粒度从最旧侧删除时，用户可见的展示窗口（保留天数）前缘永远不会缺数据。
const (
	historyInterval             = time.Minute
	historyDefaultRetentionDays = 30 // 保留期默认值；展示窗口上限 = 保留天数×24
	historyMinRetentionDays     = 1
	historyMaxRetentionDays     = 36500 // 防溢出护栏（100 年），不是产品意义上的上限
	// 存储比展示窗口多留的缓冲天数（历史设计为 32 天存储 = 30 天展示 + 2 天）
	historyRetentionBufferDays = 2
	historyPruneEvery          = time.Hour
	historyMaxPoints           = 480
	historyFileVersion         = 1 // /api/history 响应结构版本
	historyMinMaxSizeMB        = 8
	historyMaxMaxSizeMB        = 1024
	historySizePruneBytes      = 24 * 60 * 60 // 大小超限时每次删除的最旧数据跨度（秒）
	historySizePruneRounds     = 64           // 单次清理最多删除的天数，防止死循环
	archiveFlushMinRows        = 200000       // 归档冲刷体量阈值：满配 ~210B/行 ≈ 42MB，攒够才写 HDD（跨月也会提前冲）
)

type HistoryConfig struct {
	Enabled        bool   `json:"enabled"`
	MaxSizeMB      int64  `json:"max_size_mb"`
	RetentionDays  int    `json:"retention_days"`  // 历史保留天数（展示窗口），默认 30
	ArchiveEnabled bool   `json:"archive_enabled"` // 长期记录：清理前先把旧数据按月归档到用户目录
	ArchiveDir     string `json:"archive_dir"`     // 长期记录保存位置（绝对路径，保存时验证可写）
}

func DefaultHistoryConfig() HistoryConfig {
	return HistoryConfig{Enabled: true, MaxSizeMB: 64, RetentionDays: historyDefaultRetentionDays}
}

// ClampHistoryMaxSize 把大小上限限制在合理区间，配置文件里的非法值静默归位。
func ClampHistoryMaxSize(maxSizeMB int64) int64 {
	if maxSizeMB < historyMinMaxSizeMB {
		return historyMinMaxSizeMB
	}
	if maxSizeMB > historyMaxMaxSizeMB {
		return historyMaxMaxSizeMB
	}
	return maxSizeMB
}

// ClampHistoryRetentionDays 把历史保留天数限制在合理区间，配置文件里的非法值
// 静默归位（0 由 normalizeConfig 先归为默认值，这里不会遇到 0）。
func ClampHistoryRetentionDays(days int) int {
	if days < historyMinRetentionDays {
		return historyMinRetentionDays
	}
	if days > historyMaxRetentionDays {
		return historyMaxRetentionDays
	}
	return days
}

type HistoryFanSample struct {
	ID         string `json:"id"`
	RPM        int64  `json:"rpm"`
	PWMPercent int    `json:"pwm_percent"`
}

type HistoryDiskSample struct {
	ID           string  `json:"id"`
	TemperatureC float64 `json:"c"`
}

type HistorySensorSample struct {
	Group string  `json:"group"` // cpu | gpu | nic | other
	Key   string  `json:"key"`
	C     float64 `json:"c"`
}

type HistorySample struct {
	TS      int64                 `json:"ts"` // unix 秒
	CPUC    float64               `json:"cpu_c,omitempty"`
	HDDC    float64               `json:"hdd_c,omitempty"`
	NVMeC   float64               `json:"nvme_c,omitempty"`
	Fans    []HistoryFanSample    `json:"fans,omitempty"`
	Disks   []HistoryDiskSample   `json:"disks,omitempty"`   // 单盘温度（仅在线且有读数的槽位）
	Sensors []HistorySensorSample `json:"sensors,omitempty"` // CPU 逐传感器 / 网卡 / 其它
}

type historyFile struct {
	Version         int               `json:"version"`
	IntervalSeconds int               `json:"interval_seconds"`
	Samples         []HistorySample   `json:"samples"`
	DefaultGroups   map[string]string `json:"default_groups,omitempty"` // 键→默认父类（当前规则），前端据此展示默认归属
}

// HistoryStore 把采样写入 SQLite（WAL 模式），读写并发安全。
// 查询按需走索引取范围切片，不用常驻整段历史在内存里。
type HistoryStore struct {
	mu                sync.Mutex
	db                *sql.DB
	path              string
	retentionDays     int    // 用户配置的保留天数（不含缓冲），0 值由构造函数归为默认
	archiveEnabled    bool   // 长期记录：清理前先把旧数据归档到用户目录
	archiveDir        string // 长期记录保存位置（绝对路径，保存配置时已验证可写）
	archiveFailStreak int    // 长期记录连续失败轮数；失败期间暂停删除保数据（见 archiveForDelete）
	archiveWatermark  int64  // 归档水位：之前的采样都已归档+删除；水位到当期 cutoff 之间是 SSD 上的缓冲
	logger            *log.Logger
	lastPrune         time.Time
}

func NewHistoryStore(path string) (*HistoryStore, error) {
	if path == "" {
		return nil, errors.New("history path is required")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open history db: %w", err)
	}
	// WAL：写只追加日志页，读不阻塞写；synchronous=NORMAL 在 WAL 下够安全。
	// wal_autocheckpoint 保持默认（1000 页）即可：WAL 平时由读连接自动合并，
	// 大小超限清理后另有显式 wal_checkpoint(TRUNCATE) 截断，无需更激进。
	// cache_size 上限 8MB：按天查询顺序扫表，页面缓存命中率足够，同时约束
	// 守护进程常驻内存（modernc 驱动默认只有 2MB，30 天数据量下偏小）。
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=3000",
		"PRAGMA foreign_keys=ON",
		"PRAGMA cache_size=-8192",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("history pragma: %w", err)
		}
	}
	schema := `
CREATE TABLE IF NOT EXISTS history (
	ts     INTEGER PRIMARY KEY,
	cpu_c  REAL NOT NULL DEFAULT 0,
	hdd_c  REAL NOT NULL DEFAULT 0,
	nvme_c REAL NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS history_fans (
	ts          INTEGER NOT NULL REFERENCES history(ts) ON DELETE CASCADE ON UPDATE CASCADE,
	fan_id      TEXT NOT NULL,
	rpm         INTEGER NOT NULL DEFAULT 0,
	pwm_percent INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (ts, fan_id)
);
CREATE TABLE IF NOT EXISTS history_slots (
	ts           INTEGER NOT NULL REFERENCES history(ts) ON DELETE CASCADE ON UPDATE CASCADE,
	slot_id      TEXT NOT NULL,
	temperature_c REAL NOT NULL DEFAULT 0,
	PRIMARY KEY (ts, slot_id)
);
CREATE TABLE IF NOT EXISTS history_sensors (
	ts      INTEGER NOT NULL REFERENCES history(ts) ON DELETE CASCADE ON UPDATE CASCADE,
	grp     TEXT NOT NULL,
	key     TEXT NOT NULL,
	c       REAL NOT NULL DEFAULT 0,
	PRIMARY KEY (ts, grp, key)
);`
	// 索引现状说明：四张表的查询都是 `WHERE ts >= ? ORDER BY ts`——主表 ts 是
	// INTEGER PRIMARY KEY（rowid 聚簇），三张子表的复合主键均以 ts 开头，
	// 范围扫描天然走索引，无需额外建索引（额外索引只会拖慢写入）。
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("history schema: %w", err)
	}
	// 不在构造时 Prune：此刻 retentionDays 还是默认值（30 天、归档关），
	// 用户配置的更长保留期/长期记录要等 main 构造后的 SyncSettings 才生效——
	// 先删会把超期数据按错误窗口清掉。首次清理由 SyncSettings（启动即调）
	// 或采样循环的 PruneIfNeeded 承担，两者都先应用用户设置再删。
	return &HistoryStore{db: db, path: path, retentionDays: historyDefaultRetentionDays, lastPrune: time.Time{}}, nil
}

func (s *HistoryStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// ExportSQLite 把数据库的一致性快照写入 w：VACUUM INTO 临时文件再读回，
// 避开 WAL 未合并导致直接复制丢数据的问题。
func (s *HistoryStore) ExportSQLite(ctx context.Context, w io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tmp, err := os.CreateTemp("", "tad-module-history-*.db")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	tmp.Close()
	os.Remove(tmpName) // VACUUM INTO 要求目标文件不存在
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, tmpName); err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	defer os.Remove(tmpName)
	file, err := os.Open(tmpName)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(w, file)
	return err
}

// WriteCSV 把全部历史导出为宽表 CSV：时间与聚合温度为固定列，风扇/单盘/
// 传感器按出现的 ID 动态成列（缺失留空）。首行带 BOM，方便 Excel 识别 UTF-8。
//
// 流式实现（内存常量，1GB 库实测堆峰值从 ~2GB 降到 ~10MB）：列头用三个
// DISTINCT 预扫（走主键索引，JOIN history 排除孤儿子行，与旧行为一致）；
// 数据行用四张表各自的 ts 有序游标做归并连接——history 主表驱动，每个时刻
// 只在内存里保留当前 ts 的子行（满配 ~23 行），单次扫表无需整体加载。
func (s *HistoryStore) WriteCSV(ctx context.Context, w io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeCSVLocked(ctx, w, 0, math.MaxInt64)
}

// writeCSVLocked 是 WriteCSV 的范围版（调用方须已持锁）：只导出 [lo, hi) 的
// 采样，列头发现限定在同一范围内——归档单个月份时，只有该月出现过的 ID 才
// 成列。lo=0、hi=math.MaxInt64 即全库，行为与 WriteCSV 一致。
func (s *HistoryStore) writeCSVLocked(ctx context.Context, w io.Writer, lo, hi int64) error {
	// 列发现：DISTINCT 走复合主键索引，只读 ID 列不取数据；JOIN history
	// 保证"出现过在主表里的 ID"才成列（孤儿子行不成列，旧行为同样不可见）。
	fans, err := s.distinctStrings(ctx, `SELECT DISTINCT f.fan_id FROM history_fans f JOIN history h ON h.ts = f.ts WHERE f.ts >= ? AND f.ts < ? ORDER BY f.fan_id`, lo, hi)
	if err != nil {
		return err
	}
	disks, err := s.distinctStrings(ctx, `SELECT DISTINCT d.slot_id FROM history_slots d JOIN history h ON h.ts = d.ts WHERE d.ts >= ? AND d.ts < ? ORDER BY d.slot_id`, lo, hi)
	if err != nil {
		return err
	}
	type csvSensorPair struct{ group, key string }
	var sensors []csvSensorPair
	sensorRows, err := s.db.QueryContext(ctx, `SELECT DISTINCT s.grp, s.key FROM history_sensors s JOIN history h ON h.ts = s.ts WHERE s.ts >= ? AND s.ts < ? ORDER BY s.grp, s.key`, lo, hi)
	if err != nil {
		return err
	}
	for sensorRows.Next() {
		var pair csvSensorPair
		if err := sensorRows.Scan(&pair.group, &pair.key); err != nil {
			sensorRows.Close()
			return err
		}
		sensors = append(sensors, pair)
	}
	if err := sensorRows.Err(); err != nil {
		sensorRows.Close()
		return err
	}
	sensorRows.Close()

	buf := bufio.NewWriterSize(w, 64<<10)
	if _, err := buf.WriteString("\xef\xbb\xbf"); err != nil {
		return err
	}
	csvWriter := csv.NewWriter(buf)
	header := make([]string, 0, 5+len(fans)*2+len(disks)+len(sensors))
	header = append(header, "ts", "time", "cpu_c", "hdd_c", "nvme_c")
	fanRPMIndex := make(map[string]int, len(fans))
	for _, id := range fans {
		fanRPMIndex[id] = len(header)
		header = append(header, "fan_"+id+"_rpm", "fan_"+id+"_pwm")
	}
	diskIndex := make(map[string]int, len(disks))
	for _, id := range disks {
		diskIndex[id] = len(header)
		header = append(header, "disk_"+id+"_c")
	}
	sensorIndex := make(map[string]int, len(sensors))
	for _, pair := range sensors {
		sensorIndex[pair.group+"\x00"+pair.key] = len(header)
		header = append(header, "sensor_"+pair.group+"_"+pair.key+"_c")
	}
	if err := csvWriter.Write(header); err != nil {
		return err
	}
	formatC := func(value float64) string {
		if value <= 0 {
			return ""
		}
		return strconv.FormatFloat(value, 'f', 1, 64)
	}

	// 四个 ts 有序游标。子表游标落后于主表时（孤儿子行，历史库理论上不该有，
	// 但老库的 PRAGMA foreign_keys 是连接级的、无法完全排除）直接跳过。
	mainRows, err := s.db.QueryContext(ctx, `SELECT ts, cpu_c, hdd_c, nvme_c FROM history WHERE ts >= ? AND ts < ? ORDER BY ts`, lo, hi)
	if err != nil {
		return err
	}
	defer mainRows.Close()
	fanRows, err := s.db.QueryContext(ctx, `SELECT ts, fan_id, rpm, pwm_percent FROM history_fans WHERE ts >= ? AND ts < ? ORDER BY ts, fan_id`, lo, hi)
	if err != nil {
		return err
	}
	defer fanRows.Close()
	slotRows, err := s.db.QueryContext(ctx, `SELECT ts, slot_id, temperature_c FROM history_slots WHERE ts >= ? AND ts < ? ORDER BY ts, slot_id`, lo, hi)
	if err != nil {
		return err
	}
	defer slotRows.Close()
	sensorDataRows, err := s.db.QueryContext(ctx, `SELECT ts, grp, key, c FROM history_sensors WHERE ts >= ? AND ts < ? ORDER BY ts, grp, key`, lo, hi)
	if err != nil {
		return err
	}
	defer sensorDataRows.Close()

	var (
		mainTS                              int64
		mainCPU, mainHDD, mainNVMe          float64
		fanTS, fanRPM                       int64
		fanID                               string
		fanPWM                              int
		slotTS                              int64
		slotID                              string
		slotC                               float64
		sensorTS                            int64
		sensorGroup, sensorKey              string
		sensorC                             float64
		mainOK, fanOK, slotOK, sensorDataOK bool
	)
	advance := func(rows *sql.Rows, dest ...any) (bool, error) {
		if !rows.Next() {
			return false, rows.Err()
		}
		if err := rows.Scan(dest...); err != nil {
			return false, err
		}
		return true, nil
	}
	if mainOK, err = advance(mainRows, &mainTS, &mainCPU, &mainHDD, &mainNVMe); err != nil {
		return err
	}
	if fanOK, err = advance(fanRows, &fanTS, &fanID, &fanRPM, &fanPWM); err != nil {
		return err
	}
	if slotOK, err = advance(slotRows, &slotTS, &slotID, &slotC); err != nil {
		return err
	}
	if sensorDataOK, err = advance(sensorDataRows, &sensorTS, &sensorGroup, &sensorKey, &sensorC); err != nil {
		return err
	}

	record := make([]string, len(header))
	for rowCount := 0; mainOK; rowCount++ {
		if rowCount%1000 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		for fanOK && fanTS < mainTS {
			if fanOK, err = advance(fanRows, &fanTS, &fanID, &fanRPM, &fanPWM); err != nil {
				return err
			}
		}
		for slotOK && slotTS < mainTS {
			if slotOK, err = advance(slotRows, &slotTS, &slotID, &slotC); err != nil {
				return err
			}
		}
		for sensorDataOK && sensorTS < mainTS {
			if sensorDataOK, err = advance(sensorDataRows, &sensorTS, &sensorGroup, &sensorKey, &sensorC); err != nil {
				return err
			}
		}
		for i := range record {
			record[i] = ""
		}
		record[0] = strconv.FormatInt(mainTS, 10)
		record[1] = time.Unix(mainTS, 0).Format("2006-01-02 15:04:05")
		record[2] = formatC(mainCPU)
		record[3] = formatC(mainHDD)
		record[4] = formatC(mainNVMe)
		for fanOK && fanTS == mainTS {
			if idx, ok := fanRPMIndex[fanID]; ok {
				record[idx] = strconv.FormatInt(fanRPM, 10)
				record[idx+1] = strconv.Itoa(fanPWM)
			}
			if fanOK, err = advance(fanRows, &fanTS, &fanID, &fanRPM, &fanPWM); err != nil {
				return err
			}
		}
		for slotOK && slotTS == mainTS {
			if idx, ok := diskIndex[slotID]; ok {
				record[idx] = formatC(slotC)
			}
			if slotOK, err = advance(slotRows, &slotTS, &slotID, &slotC); err != nil {
				return err
			}
		}
		for sensorDataOK && sensorTS == mainTS {
			if idx, ok := sensorIndex[sensorGroup+"\x00"+sensorKey]; ok {
				record[idx] = formatC(sensorC)
			}
			if sensorDataOK, err = advance(sensorDataRows, &sensorTS, &sensorGroup, &sensorKey, &sensorC); err != nil {
				return err
			}
		}
		if err := csvWriter.Write(record); err != nil {
			return err
		}
		if mainOK, err = advance(mainRows, &mainTS, &mainCPU, &mainHDD, &mainNVMe); err != nil {
			return err
		}
	}
	if err := mainRows.Err(); err != nil {
		return err
	}
	csvWriter.Flush()
	if err := csvWriter.Error(); err != nil {
		return err
	}
	return buf.Flush()
}

// distinctStrings 跑一列 DISTINCT 查询并按序返回（列头发现与归档月份发现用）。
func (s *HistoryStore) distinctStrings(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

// Append 追加一个采样点（ts 为主键，重复写入即覆盖）。
// 写入路径说明：database/sql 会对 *sql.DB 的每条固定 SQL 缓存预编译语句，
// 每分钟一个事务、每事务十来条 Exec 的量级下，手动管理 Tx 级预编译语句
// 只会增加代码复杂度，收益可以忽略——维持现状。
func (s *HistoryStore) Append(sample HistorySample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO history (ts, cpu_c, hdd_c, nvme_c) VALUES (?, ?, ?, ?)
ON CONFLICT(ts) DO UPDATE SET cpu_c=excluded.cpu_c, hdd_c=excluded.hdd_c, nvme_c=excluded.nvme_c`,
		sample.TS, sample.CPUC, sample.HDDC, sample.NVMeC); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM history_fans WHERE ts = ?`, sample.TS); err != nil {
		return err
	}
	for _, fan := range sample.Fans {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO history_fans (ts, fan_id, rpm, pwm_percent) VALUES (?, ?, ?, ?)`,
			sample.TS, fan.ID, fan.RPM, fan.PWMPercent); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM history_slots WHERE ts = ?`, sample.TS); err != nil {
		return err
	}
	for _, disk := range sample.Disks {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO history_slots (ts, slot_id, temperature_c) VALUES (?, ?, ?)`,
			sample.TS, disk.ID, disk.TemperatureC); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM history_sensors WHERE ts = ?`, sample.TS); err != nil {
		return err
	}
	for _, sensor := range sample.Sensors {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO history_sensors (ts, grp, key, c) VALUES (?, ?, ?, ?)`,
			sample.TS, sensor.Group, sensor.Key, sensor.C); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// SetLogger 注入守护进程日志器（长期记录失败/回退需要让用户看到原因）。
func (s *HistoryStore) SetLogger(logger *log.Logger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logger = logger
}

// logf 在注入了日志器时记一条日志；未注入（单测）静默。
func (s *HistoryStore) logf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Printf(format, args...)
	}
}

// SyncSettings 同步用户可调的清理配置（保留天数、长期记录）并立即按新保留
// 期清理一次：保存配置后马上调用，缩短保留期能当场生效。用户主动保存说明
// 可能刚修好了长期记录目录，连续失败计数一并归零。
func (s *HistoryStore) SyncSettings(settings HistoryConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applySettingsLocked(settings)
	s.archiveFailStreak = 0
	s.archiveWatermark = 0 // 用户主动保存（可能刚修好目录）：立即安排一次冲刷
	return s.pruneLocked(time.Now())
}

func (s *HistoryStore) applySettingsLocked(settings HistoryConfig) {
	s.retentionDays = ClampHistoryRetentionDays(settings.RetentionDays)
	s.archiveEnabled = settings.ArchiveEnabled
	s.archiveDir = strings.TrimSpace(settings.ArchiveDir)
}

// PruneIfNeeded 挂在采样循环上的低频清理：日期轮转距上次超过 1 小时才真正
// 执行 DELETE；大小上限每次都检查（仅两次 stat），超限才进入删除与 VACUUM。
// 历史记录停用后清理照跑，让旧数据按期收敛。每次调用都同步保留天数与长期
// 记录设置，保证配置变更最迟在下个采样点生效。
func (s *HistoryStore) PruneIfNeeded(now time.Time, settings HistoryConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applySettingsLocked(settings)
	if s.lastPrune.IsZero() || time.Since(s.lastPrune) >= historyPruneEvery {
		if err := s.pruneLocked(now); err != nil {
			return err
		}
	}
	if settings.MaxSizeMB <= 0 {
		return nil
	}
	return s.enforceSizeLocked(now, settings.MaxSizeMB<<20)
}

// Prune 删除保留期之外的采样（history_fans 级联删除）。
func (s *HistoryStore) Prune(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pruneLocked(now)
}

func (s *HistoryStore) pruneLocked(now time.Time) error {
	// 长期记录开启时，删除与归档合并为一次"冲刷"：过期数据留在主库（SSD）
	// 当缓冲，攒到跨自然月或体量达到阈值才一次性写入归档盘（HDD 平时可持续
	// 休眠，一年只有十几次大块顺序写），写完再删并把水位推进到 cutoff。
	// 冲刷失败：水位不动、本轮不删（保数据，SSD 继续缓冲），下小时重试。
	// 不设"连续失败后回退直接删除"——长期记录是用户明确要保的数据，归档
	// 失败宁可让库增长（大小上限另有约束），由日志暴露等待用户修复目录。
	if s.archiveEnabled && s.archiveDir != "" {
		return s.flushArchiveLocked(now)
	}
	cutoff := now.Add(-time.Duration(s.retentionDays+historyRetentionBufferDays) * 24 * time.Hour).Unix()
	if _, err := s.db.Exec(`DELETE FROM history WHERE ts < ?`, cutoff); err != nil {
		return err
	}
	s.lastPrune = now
	return nil
}

func (s *HistoryStore) flushArchiveLocked(now time.Time) error {
	// 无论本轮是"缓冲未满不动归档盘"还是冲刷失败，都算完成了一次轮转检查：
	// 不更新的话每个采样点（1 分钟）都会重进冲刷判定/重试，错误日志跟着刷屏；
	// 每小时一次的重试节奏足够，快速恢复靠用户保存配置（SyncSettings 立即冲刷）
	s.lastPrune = now
	cutoff := now.Add(-time.Duration(s.retentionDays+historyRetentionBufferDays) * 24 * time.Hour).Unix()
	if !s.archiveFlushDue(now, cutoff) {
		// 缓冲未满且没跨月：不动归档盘，过期数据继续留在 SSD
		return nil
	}
	return s.deleteExpiredLocked(s.archiveWatermark, cutoff)
}

// archiveFlushDue 冲刷条件：水位未建立（首轮/重启后），自上次冲刷后过期的
// 数据跨了自然月（cutoff 的月份离开水位月份 = 上一个整月已完整过期），或未
// 冲刷的过期数据体量达到阈值。跨月比较用 cutoff 而非 now：保留期极长时水位
// 和 cutoff 都在很远的历史里，跟 now 比会每小时误判"该冲刷"。
func (s *HistoryStore) archiveFlushDue(now time.Time, cutoff int64) bool {
	if s.archiveWatermark == 0 {
		return true
	}
	if cutoff <= s.archiveWatermark {
		return false // 保留期极长：还没有任何数据过期
	}
	if monthKey(s.archiveWatermark) != monthKey(cutoff) {
		return true
	}
	var pending int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM history WHERE ts >= ? AND ts < ?`, s.archiveWatermark, cutoff).Scan(&pending); err != nil {
		return true // 查询失败按"需要冲刷"处理，让具体错误在归档步骤暴露
	}
	return pending >= archiveFlushMinRows
}

// deleteExpiredLocked 归档并删除 [lo, hi) 的采样（lo=水位）；长期记录开启时
// 先冲刷归档（失败则中止，调用方不得继续删）。删除严格限定在已归档区间内：
// 水位之下不该有数据（采样永远写"当前时刻"），万一因时钟回跳等异常出现，
// 留在原地也绝不无声丢弃。成功后水位推进到 hi。
func (s *HistoryStore) deleteExpiredLocked(lo, hi int64) error {
	if s.archiveEnabled && s.archiveDir != "" {
		if err := s.archiveForDelete(lo, hi); err != nil {
			return err
		}
	}
	if _, err := s.db.Exec(`DELETE FROM history WHERE ts >= ? AND ts < ?`, lo, hi); err != nil {
		return err
	}
	s.archiveWatermark = hi
	return nil
}

// archiveForDelete 长期记录的归档步骤：成功返回 nil，失败返回 error 且调用方
// 不得删除该批数据。长期记录开启时归档失败**永远保数据不删**——保存位置坏掉
// 由运行日志暴露、磁盘风险由大小上限约束，不做静默丢弃；用户保存配置
// （SyncSettings）即视为可能已修复，失败计数归零重试。
func (s *HistoryStore) archiveForDelete(lo, hi int64) error {
	if err := s.archiveBetween(lo, hi); err != nil {
		s.archiveFailStreak++
		s.logf("long-term archive failed (%d consecutive rounds), pruning paused until the archive dir recovers: %v", s.archiveFailStreak, err)
		return err
	}
	if s.archiveFailStreak > 0 {
		s.logf("long-term archive recovered")
	}
	s.archiveFailStreak = 0
	return nil
}

// archiveBetween 把 [lo, hi) 的采样按自然月（本地时区）归档到用户目录：
// 一个月一个压缩 CSV 文件（tad-history-202609.csv.gz），文件名天然唯一且可按
// 年月识别；文件数量不限制。CSV 格式与 /api/history/export.csv 的宽表完全
// 一致（Excel 可直接打开）；gzip 压缩后满配约 1.4MB/月，对比 SQLite 月文件
// 的 ~50MB 缩减约 35 倍——宽表同一列的值逐行重复、格式一致，gzip 的窗口
// 跨行匹配效率极高，文本化即拿到列压缩的大部分收益。
func (s *HistoryStore) archiveBetween(lo, hi int64) error {
	if err := os.MkdirAll(s.archiveDir, 0o755); err != nil {
		return fmt.Errorf("create archive dir: %w", err)
	}
	months, err := s.distinctStrings(context.Background(),
		`SELECT DISTINCT strftime('%Y%m', ts, 'unixepoch', 'localtime') FROM history WHERE ts >= ? AND ts < ? ORDER BY 1`, lo, hi)
	if err != nil {
		return err
	}
	for _, month := range months {
		mLo, mHi, err := monthBounds(month)
		if err != nil {
			return err
		}
		if mLo < lo {
			mLo = lo
		}
		if mHi > hi {
			mHi = hi
		}
		if mLo >= mHi {
			continue
		}
		path := filepath.Join(s.archiveDir, fmt.Sprintf("tad-history-%s.csv.gz", month))
		if err := s.archiveMonthRange(path, mLo, mHi); err != nil {
			return fmt.Errorf("archive %s: %w", month, err)
		}
	}
	return nil
}

// monthKey 把 unix 秒转成本地 YYYYMM（冲刷的跨月条件用）。
func monthKey(ts int64) string {
	return time.Unix(ts, 0).Format("200601")
}

// monthBounds 把 YYYYMM 解析成本地日历月的 [起, 止) unix 秒区间。
func monthBounds(month string) (int64, int64, error) {
	if len(month) != 6 {
		return 0, 0, fmt.Errorf("unexpected month %q", month)
	}
	year, err := strconv.Atoi(month[:4])
	if err != nil {
		return 0, 0, fmt.Errorf("unexpected month %q: %w", month, err)
	}
	mon, err := strconv.Atoi(month[4:6])
	if err != nil || mon < 1 || mon > 12 {
		return 0, 0, fmt.Errorf("unexpected month %q", month)
	}
	lo := time.Date(year, time.Month(mon), 1, 0, 0, 0, 0, time.Local)
	return lo.Unix(), lo.AddDate(0, 1, 0).Unix(), nil
}

// archiveMonthRange 把 [lo, hi) 的采样写成月度归档文件（宽表 CSV + gzip，
// 复用 writeCSVLocked 的流式导出）。先写同目录临时文件再 rename：进程中断
// 最多留下一个待清理的临时文件，不会出现半截归档。
func (s *HistoryStore) archiveMonthRange(path string, lo, hi int64) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tad-archive-*.csv.gz.tmp")
	if err != nil {
		return fmt.Errorf("create temp archive: %w", err)
	}
	tmpName := tmp.Name()
	complete := false
	defer func() {
		if !complete {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	// BestCompression：归档一个月才写一次、满配整月也就 1~2 秒（低端 ARM
	// 实测量级），换约 10% 的体积缩减值得。
	gz, err := gzip.NewWriterLevel(tmp, gzip.BestCompression)
	if err != nil {
		return err
	}
	if err := s.writeArchiveMonthCSV(gz, path, lo, hi); err != nil {
		return fmt.Errorf("write archive csv: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("close gzip: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close archive file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename archive: %w", err)
	}
	complete = true
	return nil
}

// writeArchiveMonthCSV 往 gz 写 [lo, hi) 的宽表 CSV。同一自然月可能被多次
// 冲刷（首轮水位建立、跨月、大小清理提前冲刷都会在月内留下接缝），目标文件
// 已存在时读出旧内容按 ts 归并去重（同 ts 保留新值）后与新数据合并重写：
// 月文件始终保持"一个自然月一个文件、Excel 直接可开"，重跑幂等语义与旧
// SQLite 方案的 INSERT OR REPLACE 一致，不会因覆盖写丢掉先前的批次。
func (s *HistoryStore) writeArchiveMonthCSV(gz *gzip.Writer, path string, lo, hi int64) error {
	oldHeader, oldRows, err := readArchiveCSVFile(path)
	if err != nil {
		return err
	}
	if oldHeader == nil {
		return s.writeCSVLocked(context.Background(), gz, lo, hi)
	}
	// 新数据先流式生成到内存缓冲（满配一个月未压缩 ~7MB），再与旧行归并。
	var newBuf bytes.Buffer
	if err := s.writeCSVLocked(context.Background(), &newBuf, lo, hi); err != nil {
		return err
	}
	newReader := csv.NewReader(strings.NewReader(strings.TrimPrefix(newBuf.String(), "\ufeff")))
	newHeader, err := newReader.Read()
	if err != nil {
		return err
	}
	header := mergeCSVHeaders(oldHeader, newHeader)
	oldMap := mapCSVColumns(oldHeader, header)
	newMap := mapCSVColumns(newHeader, header)
	// 合并重写也要补 UTF-8 BOM（writeCSVLocked 首写会带）：列名/传感器键含
	// 中文时，无 BOM 的 CSV 会被 Excel 按本地编码误读成乱码
	if _, err := gz.Write([]byte("\ufeff")); err != nil {
		return err
	}
	w := csv.NewWriter(gz)
	if err := w.Write(header); err != nil {
		return err
	}
	writeRow := func(row []string, mapping []int) error {
		out := make([]string, len(header))
		for j, v := range row {
			if j < len(mapping) {
				if k := mapping[j]; k >= 0 {
					out[k] = v
				}
			}
		}
		return w.Write(out)
	}
	rowTS := func(row []string) (int64, error) {
		if len(row) == 0 {
			return 0, fmt.Errorf("archive row without ts column")
		}
		return strconv.ParseInt(row[0], 10, 64)
	}
	// 双流归并：旧行（内存）与新增行（流式）都按 ts 升序（水位递增保证），
	// 同 ts 保留新值。
	pending, pendingOK := make([]string, 0), false
	advance := func() error {
		row, err := newReader.Read()
		if err == io.EOF {
			pendingOK = false
			return nil
		}
		if err != nil {
			return err
		}
		pending, pendingOK = row, true
		return nil
	}
	if err := advance(); err != nil {
		return err
	}
	oldPos := 0
	for oldPos < len(oldRows) || pendingOK {
		switch {
		case oldPos >= len(oldRows):
			if err := writeRow(pending, newMap); err != nil {
				return err
			}
			if err := advance(); err != nil {
				return err
			}
		case !pendingOK:
			if err := writeRow(oldRows[oldPos], oldMap); err != nil {
				return err
			}
			oldPos++
		default:
			oldTS, err := rowTS(oldRows[oldPos])
			if err != nil {
				return err
			}
			newTS, err := rowTS(pending)
			if err != nil {
				return err
			}
			switch {
			case oldTS < newTS:
				if err := writeRow(oldRows[oldPos], oldMap); err != nil {
					return err
				}
				oldPos++
			case oldTS > newTS:
				if err := writeRow(pending, newMap); err != nil {
					return err
				}
				if err := advance(); err != nil {
					return err
				}
			default: // 同 ts：保留新值，旧行跳过（重跑幂等）
				if err := writeRow(pending, newMap); err != nil {
					return err
				}
				if err := advance(); err != nil {
					return err
				}
				oldPos++
			}
		}
	}
	w.Flush()
	return w.Error()
}

// readArchiveCSVFile 读已有月归档的表头与数据行；文件不存在返回 (nil, nil, nil)。
func readArchiveCSVFile(path string) ([]string, [][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, nil, err
	}
	defer zr.Close()
	records, err := csv.NewReader(zr).ReadAll()
	if err != nil {
		return nil, nil, err
	}
	if len(records) == 0 {
		return nil, nil, nil
	}
	header := records[0]
	header[0] = strings.TrimPrefix(header[0], "\ufeff")
	return header, records[1:], nil
}

// mergeCSVHeaders 合并两份宽表列头（并集）：固定列不会新增，旧列头原序保留，
// 新增列按所属组（fan/disk/sensor）追加到旧列头对应组区间的末尾。
func mergeCSVHeaders(oldHeader, newHeader []string) []string {
	inOld := make(map[string]bool, len(oldHeader))
	for _, h := range oldHeader {
		inOld[h] = true
	}
	headerGroup := func(h string) int {
		switch {
		case strings.HasPrefix(h, "fan_"):
			return 0
		case strings.HasPrefix(h, "disk_"):
			return 1
		case strings.HasPrefix(h, "sensor_"):
			return 2
		default:
			return -1 // 固定列（ts/time/聚合温度）
		}
	}
	var extra [3][]string
	for _, h := range newHeader {
		if !inOld[h] {
			if g := headerGroup(h); g >= 0 {
				extra[g] = append(extra[g], h)
			}
		}
	}
	merged := append([]string(nil), oldHeader...)
	for g := 0; g < 3; g++ {
		if len(extra[g]) == 0 {
			continue
		}
		pos := len(merged)
		for i, h := range merged {
			if headerGroup(h) == g {
				pos = i + 1
			}
		}
		merged = append(merged, make([]string, len(extra[g]))...)
		copy(merged[pos+len(extra[g]):], merged[pos:])
		copy(merged[pos:pos+len(extra[g])], extra[g])
	}
	return merged
}

// mapCSVColumns 给出 from 每一列在 to 布局中的目标下标（缺失为 -1），
// 供行数据按列名重排。
func mapCSVColumns(from, to []string) []int {
	index := make(map[string]int, len(to))
	for i, h := range to {
		index[h] = i
	}
	mapping := make([]int, len(from))
	for j, h := range from {
		mapping[j] = index[h]
	}
	return mapping
}

// dbSizeBytes 统计主库与 WAL 文件大小；文件不存在按 0 处理。
func (s *HistoryStore) dbSizeBytes() int64 {
	total := int64(0)
	for _, name := range []string{s.path, s.path + "-wal"} {
		if info, err := os.Stat(name); err == nil {
			total += info.Size()
		}
	}
	return total
}

// enforceSizeLocked 数据库（含 WAL）超过上限时，从最旧的一天开始逐段删除，
// 至少保留最新一个采样点。删除发生在采样间隙且只在超限时发生，VACUUM 的
// I/O 开销可以接受。
func (s *HistoryStore) enforceSizeLocked(now time.Time, limitBytes int64) error {
	deleted := false
	for round := 0; round < historySizePruneRounds; round++ {
		if s.dbSizeBytes() <= limitBytes {
			break
		}
		var oldest, newest sql.NullInt64
		if err := s.db.QueryRow(`SELECT MIN(ts), MAX(ts) FROM history`).Scan(&oldest, &newest); err != nil {
			return err
		}
		if !oldest.Valid || !newest.Valid || newest.Int64-oldest.Int64 <= historySizePruneBytes {
			break // 不足一天可删时停止，至少保留最新一天的采样
		}
		cutoff := oldest.Int64 + historySizePruneBytes
		if cutoff > newest.Int64 {
			cutoff = newest.Int64
		}
		// 大小清理走同一个"先冲刷归档再删"的出口，但不做体量/跨月缓冲：
		// 库超限时必须当场收缩（稳态下超限本身约一天一次，对 HDD 就是
		// 每天一批；归档先在 SSD 上攒着的缓冲也在这一步一并写走）。
		if err := s.deleteExpiredLocked(s.archiveWatermark, cutoff); err != nil {
			return err
		}
		deleted = true
	}
	if !deleted {
		return nil
	}
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return err
	}
	// VACUUM 只压缩主库；WAL 文件停在高位水位，必须显式截断才真正归还磁盘
	_, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// maxRangeHours 展示窗口上限 = 配置保留天数×24（不含 2 天缓冲）：用户可查询
// 的最大范围就是保留期本身，缓冲数据只用于轮转删除时兜底。
func (s *HistoryStore) maxRangeHours() float64 {
	return float64(s.retentionDays) * 24
}

// Aggregated 返回最近 rangeHours 小时的采样（支持 0.5 这样的半小时范围）；
// 行数超过 maxPoints 时按桶聚合（数值字段取峰值），第二个返回值是聚合后的
// 采样间隔秒数，前端据此识别停机断口。
func (s *HistoryStore) Aggregated(ctx context.Context, rangeHours, maxPoints float64, now time.Time) ([]HistorySample, int, error) {
	if rangeHours < 0.5 {
		rangeHours = 0.5
	}
	if maxPoints < 10 {
		maxPoints = 10
	}
	if maxPoints > 2000 {
		maxPoints = 2000
	}
	cutoff := now.Add(-time.Duration(rangeHours * float64(time.Hour))).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	// 范围钳制放在锁内：retentionDays 由 applySettingsLocked（配置保存/采样
	// 循环）持锁写入，锁外读会被 -race 报数据竞争
	if maxRange := s.maxRangeHours(); rangeHours > maxRange {
		cutoff = now.Add(-time.Duration(maxRange * float64(time.Hour))).Unix()
	}
	samples, err := s.queryRange(ctx, cutoff)
	if err != nil {
		return nil, 0, err
	}
	if len(samples) == 0 {
		return []HistorySample{}, int(historyInterval.Seconds()), nil
	}
	stride := int(math.Ceil(float64(len(samples)) / maxPoints))
	if stride <= 1 {
		return samples, int(historyInterval.Seconds()), nil
	}
	out := make([]HistorySample, 0, len(samples)/stride+1)
	for i := 0; i < len(samples); i += stride {
		out = append(out, aggregateBucket(samples[i:min(i+stride, len(samples))]))
	}
	return out, int(historyInterval.Seconds()) * stride, nil
}

// queryRange 读取 cutoff 之后的采样：主表与三张子表分别查询再按 ts 归并。
// 不做多表 JOIN——JOIN 会产生 风扇数×槽位数×传感器数 的笛卡尔积，30 天
// 数据量下行数爆炸且拖长互斥锁持有时间。
func (s *HistoryStore) queryRange(ctx context.Context, cutoff int64) ([]HistorySample, error) {
	mainRows, err := s.db.QueryContext(ctx, `SELECT ts, cpu_c, hdd_c, nvme_c FROM history WHERE ts >= ? ORDER BY ts`, cutoff)
	if err != nil {
		return nil, err
	}
	defer mainRows.Close()
	samples := make([]HistorySample, 0, 64)
	for mainRows.Next() {
		var sample HistorySample
		if err := mainRows.Scan(&sample.TS, &sample.CPUC, &sample.HDDC, &sample.NVMeC); err != nil {
			return nil, err
		}
		samples = append(samples, sample)
	}
	if err := mainRows.Err(); err != nil {
		return nil, err
	}
	// 索引在主表行收齐后建：append 扩容会使指向切片元素的指针失效
	index := make(map[int64]*HistorySample, len(samples))
	for i := range samples {
		index[samples[i].TS] = &samples[i]
	}

	fanRows, err := s.db.QueryContext(ctx, `SELECT ts, fan_id, rpm, pwm_percent FROM history_fans WHERE ts >= ? ORDER BY ts`, cutoff)
	if err != nil {
		return nil, err
	}
	defer fanRows.Close()
	for fanRows.Next() {
		var ts int64
		var fan HistoryFanSample
		if err := fanRows.Scan(&ts, &fan.ID, &fan.RPM, &fan.PWMPercent); err != nil {
			return nil, err
		}
		if sample := index[ts]; sample != nil {
			sample.Fans = append(sample.Fans, fan)
		}
	}
	if err := fanRows.Err(); err != nil {
		return nil, err
	}

	slotRows, err := s.db.QueryContext(ctx, `SELECT ts, slot_id, temperature_c FROM history_slots WHERE ts >= ? ORDER BY ts`, cutoff)
	if err != nil {
		return nil, err
	}
	defer slotRows.Close()
	for slotRows.Next() {
		var ts int64
		var disk HistoryDiskSample
		if err := slotRows.Scan(&ts, &disk.ID, &disk.TemperatureC); err != nil {
			return nil, err
		}
		if sample := index[ts]; sample != nil {
			sample.Disks = append(sample.Disks, disk)
		}
	}
	if err := slotRows.Err(); err != nil {
		return nil, err
	}

	sensorRows, err := s.db.QueryContext(ctx, `SELECT ts, grp, key, c FROM history_sensors WHERE ts >= ? ORDER BY ts`, cutoff)
	if err != nil {
		return nil, err
	}
	defer sensorRows.Close()
	for sensorRows.Next() {
		var ts int64
		var sensor HistorySensorSample
		if err := sensorRows.Scan(&ts, &sensor.Group, &sensor.Key, &sensor.C); err != nil {
			return nil, err
		}
		if sample := index[ts]; sample != nil {
			sample.Sensors = append(sample.Sensors, sensor)
		}
	}
	if err := sensorRows.Err(); err != nil {
		return nil, err
	}
	return samples, nil
}

// aggregateBucket 合并一个桶：数值字段取峰值（温度峰值更有意义），
// 风扇与单盘温度逐 ID 取峰值，时间戳取桶内最后一个点。
func aggregateBucket(bucket []HistorySample) HistorySample {
	merged := HistorySample{TS: bucket[len(bucket)-1].TS}
	fanIndex := map[string]int{}
	diskIndex := map[string]int{}
	sensorIndex := map[string]int{}
	for _, sample := range bucket {
		if sample.CPUC > merged.CPUC {
			merged.CPUC = sample.CPUC
		}
		if sample.HDDC > merged.HDDC {
			merged.HDDC = sample.HDDC
		}
		if sample.NVMeC > merged.NVMeC {
			merged.NVMeC = sample.NVMeC
		}
		for _, fan := range sample.Fans {
			idx, ok := fanIndex[fan.ID]
			if !ok {
				idx = len(merged.Fans)
				fanIndex[fan.ID] = idx
				merged.Fans = append(merged.Fans, HistoryFanSample{ID: fan.ID})
			}
			if fan.RPM > merged.Fans[idx].RPM {
				merged.Fans[idx].RPM = fan.RPM
			}
			if fan.PWMPercent > merged.Fans[idx].PWMPercent {
				merged.Fans[idx].PWMPercent = fan.PWMPercent
			}
		}
		for _, disk := range sample.Disks {
			idx, ok := diskIndex[disk.ID]
			if !ok {
				idx = len(merged.Disks)
				diskIndex[disk.ID] = idx
				merged.Disks = append(merged.Disks, HistoryDiskSample{ID: disk.ID})
			}
			if disk.TemperatureC > merged.Disks[idx].TemperatureC {
				merged.Disks[idx].TemperatureC = disk.TemperatureC
			}
		}
		for _, sensor := range sample.Sensors {
			combined := sensor.Group + "\x00" + sensor.Key
			idx, ok := sensorIndex[combined]
			if !ok {
				idx = len(merged.Sensors)
				sensorIndex[combined] = idx
				merged.Sensors = append(merged.Sensors, HistorySensorSample{Group: sensor.Group, Key: sensor.Key})
			}
			if sensor.C > merged.Sensors[idx].C {
				merged.Sensors[idx].C = sensor.C
			}
		}
	}
	return merged
}

// SampleFromStatus 从一次状态快照提取历史采样点：CPU 用展示温度，
// 硬盘/NVMe 各取在线槽位的最高温（0 表示不可用），风扇逐个记录。
func SampleFromStatus(st *Status, now time.Time) HistorySample {
	sample := HistorySample{TS: now.Unix()}
	if st == nil {
		return sample
	}
	if st.CPUTemperature.Available {
		sample.CPUC = st.CPUTemperature.DisplayC
	}
	for i := range st.Storage.Slots {
		slot := &st.Storage.Slots[i]
		if slot.TemperatureC <= 0 {
			continue // 空置、休眠未唤醒或读取失败的槽位不参与
		}
		switch slot.Kind {
		case "m2":
			if slot.TemperatureC > sample.NVMeC {
				sample.NVMeC = slot.TemperatureC
			}
		case "front":
			if slot.TemperatureC > sample.HDDC {
				sample.HDDC = slot.TemperatureC
			}
		default:
			continue
		}
		sample.Disks = append(sample.Disks, HistoryDiskSample{ID: slot.ID, TemperatureC: slot.TemperatureC})
	}
	for _, temp := range st.Temperatures {
		if temp.Celsius > 0 {
			sample.Sensors = append(sample.Sensors, HistorySensorSample{Group: "cpu", Key: temp.Label, C: temp.Celsius})
		}
	}
	for _, temp := range st.ExtraTemperatures {
		if temp.Celsius > 0 {
			sample.Sensors = append(sample.Sensors, HistorySensorSample{Group: classifySensorLabel(temp.Label), Key: temp.Label, C: temp.Celsius})
		}
	}
	for i := range st.FanControl.Fans {
		fan := &st.FanControl.Fans[i]
		if fan.RPM == 0 && fan.PWMPercent == 0 {
			continue // 未接风扇的空通道全 0，不采集（前端据此过滤幽灵风扇）
		}
		sample.Fans = append(sample.Fans, HistoryFanSample{ID: fan.ID, RPM: fan.RPM, PWMPercent: fan.PWMPercent})
	}
	return sample
}

// SampleNow 供采样循环调用：基于当前状态生成历史采样点。
func (m *Manager) SampleNow(now time.Time) HistorySample {
	status := m.Status()
	return SampleFromStatus(&status, now)
}

// classifySensorLabel 按传感器键推断父类：无冒号（coretemp 标签，如
// "Core 0"）为 cpu；其余取芯片名（剥 #N 消歧后缀）查驱动表。写入与读取
// 共用同一规则，驱动表调整（如 mlx5 归网卡）后旧数据读取时也能整体归入
// 新父类，不会出现同键曲线在两个父类间断开。
func classifySensorLabel(label string) string {
	idx := strings.Index(label, ":")
	if idx <= 0 {
		return "cpu"
	}
	chip := label[:idx]
	if hash := strings.Index(chip, "#"); hash > 0 { // nvme#2 → nvme，后缀只是消歧
		chip = chip[:hash]
	}
	switch {
	case knownNICDrivers[chip]:
		return "nic"
	case knownGPUDrivers[chip]:
		return "gpu"
	case knownUSBTempDrivers[chip]:
		// USB 温度计的 hwmon 驱动（如 out-of-tree 的 "temper"）：显式归
		// 「其它」组。结果与 default 相同，写出来是为了让 knownUSBTempDrivers
		// 表参与归类决策，将来需要单独分组时改这里即可。
		return "other"
	default:
		return "other"
	}
}

// ReclassifySensorGroups 按当前分类规则重写全部采样点的传感器父类，并返回
// 键→默认父类映射（随 /api/history 下发，前端据此展示默认归属）。
func ReclassifySensorGroups(samples []HistorySample) map[string]string {
	defaults := make(map[string]string)
	for i := range samples {
		sensors := samples[i].Sensors
		for j := range sensors {
			group := classifySensorLabel(sensors[j].Key)
			sensors[j].Group = group
			if _, ok := defaults[sensors[j].Key]; !ok {
				defaults[sensors[j].Key] = group
			}
		}
	}
	return defaults
}

// ApplySensorGroupOverrides 按用户配置改写传感器父类归属。读时应用：
// 数据库始终存默认分组（改回配置即恢复原样），查询结果整体迁移到新父类，
// 历史曲线立即跟随，无需等新采样覆盖。
func ApplySensorGroupOverrides(samples []HistorySample, overrides map[string]string) {
	if len(overrides) == 0 {
		return
	}
	for i := range samples {
		sensors := samples[i].Sensors
		for j := range sensors {
			if group, ok := overrides[sensors[j].Key]; ok {
				sensors[j].Group = group
			}
		}
	}
}

// HistoryLoop 周期采样历史数据；启动先采一个点，让图表尽快有首条数据。
func HistoryLoop(ctx context.Context, manager *Manager, logger *log.Logger, store *HistoryStore) {
	store.appendAndLog(ctx, manager, logger)
	ticker := time.NewTicker(historyInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			store.appendAndLog(ctx, manager, logger)
		}
	}
}

func (s *HistoryStore) appendAndLog(ctx context.Context, manager *Manager, logger *log.Logger) {
	select {
	case <-ctx.Done():
		return
	default:
	}
	now := time.Now()
	settings := manager.HistorySettings()
	// 停用时只停写入：已入库的数据仍可查看，并由下面的清理按保留期与
	// 大小上限继续收敛，数据库不会一直膨胀。
	if settings.Enabled {
		if err := s.Append(manager.SampleNow(now)); err != nil {
			logger.Printf("history append failed: %v", err)
		}
	}
	if err := s.PruneIfNeeded(now, settings); err != nil {
		logger.Printf("history prune failed: %v", err)
	}
}

// FlushArchive 立即补一次归档冲刷（清空数据库前由 HTTP 层调用）：把水位到
// 当前 cutoff 之间的过期缓冲一次性写入归档盘并从主库删除、推进水位。与定时
// 冲刷共用归档与失败计数（失败返回 error，缓冲原样保留在主库）。长期记录未
// 开启、保留期极长、或过期窗口里没有行（刚开启/保存过设置水位归零，数据全
// 在保留窗口内）时是空操作——返回 false 让前端如实提示"没有缓冲可归档"，
// 而不是声称已归档。返回是否真的归档并删除了数据。
func (s *HistoryStore) FlushArchive(now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.archiveEnabled || s.archiveDir == "" {
		return false, nil
	}
	cutoff := now.Add(-time.Duration(s.retentionDays+historyRetentionBufferDays) * 24 * time.Hour).Unix()
	if cutoff <= s.archiveWatermark {
		return false, nil // 保留期极长：没有任何过期缓冲可冲
	}
	var pending int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM history WHERE ts >= ? AND ts < ?`, s.archiveWatermark, cutoff).Scan(&pending); err != nil {
		return false, err
	}
	if pending == 0 {
		s.archiveWatermark = cutoff // 空窗照样消费掉，避免每小时重复扫描
		return false, nil
	}
	if err := s.archiveForDelete(s.archiveWatermark, cutoff); err != nil {
		return false, err
	}
	if _, err := s.db.Exec(`DELETE FROM history WHERE ts >= ? AND ts < ?`, s.archiveWatermark, cutoff); err != nil {
		return false, err
	}
	s.archiveWatermark = cutoff
	return true, nil
}

// Clear 清空全部历史数据并回收磁盘空间；schema 与配置（保留期、大小上限）
// 都不受影响，清空后可立即继续采样。
func (s *HistoryStore) Clear(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// history 的三张子表都有 ON DELETE CASCADE，直接 DELETE FROM history 即可
	// 级联；但逐张显式清空对意外缺外键定义的旧库更稳，顺序上先子后父。
	for _, table := range []string{"history_sensors", "history_slots", "history_fans", "history"} {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}
	// VACUUM 只压缩主库；WAL 文件停在高位水位，必须显式截断才真正归还磁盘。
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("wal checkpoint: %w", err)
	}
	return nil
}
