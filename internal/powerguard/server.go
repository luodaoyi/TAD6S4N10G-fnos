package powerguard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Server struct {
	Manager     *Manager
	Socket      string
	WebRoot     string
	BasePath    string
	SocketGroup string
	Logger      *log.Logger
	History     *HistoryStore
	LogPath     string // 守护进程文本日志路径；空 = stderr 模式，日志端点降级
}

const configRequestMaxBytes = 3 << 20

func (s *Server) ListenAndServe() error {
	if s.Manager == nil {
		return errors.New("manager is required")
	}
	if s.Socket == "" {
		return errors.New("socket path is required")
	}
	if s.BasePath == "" {
		s.BasePath = "/app/tad-module"
	}
	if s.Logger == nil {
		s.Logger = log.Default()
	}
	if s.SocketGroup == "" {
		s.SocketGroup = "www-data"
	}
	if err := os.MkdirAll(filepath.Dir(s.Socket), 0o755); err != nil {
		return err
	}
	if info, err := os.Lstat(s.Socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("refusing to remove non-socket path %s", s.Socket)
		}
		if err := os.Remove(s.Socket); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", s.Socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(s.Socket)
	group, err := user.LookupGroup(s.SocketGroup)
	if err != nil {
		return fmt.Errorf("lookup socket group %s: %w", s.SocketGroup, err)
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return fmt.Errorf("parse socket group %s gid %q: %w", s.SocketGroup, group.Gid, err)
	}
	if err := os.Chown(s.Socket, -1, gid); err != nil {
		return fmt.Errorf("set socket group %s: %w", s.SocketGroup, err)
	}
	if err := os.Chmod(s.Socket, 0o660); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/history", s.handleHistory)
	mux.HandleFunc("/api/history/clear", s.handleHistoryClear)
	mux.HandleFunc("/api/history/archive", s.handleHistoryArchive)
	mux.HandleFunc("/api/history/export/sql", s.handleHistoryExportSQL)
	mux.HandleFunc("/api/history/export/csv", s.handleHistoryExportCSV)
	mux.HandleFunc("/api/debug/report", s.handleDebugReport)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/config/global", s.handleGlobalConfig)
	mux.HandleFunc("/api/config/fan", s.handleFanConfig)
	mux.HandleFunc("/api/config/gpio", s.handleGPIOConfig)
	mux.HandleFunc("/api/config/history", s.handleHistoryConfig)
	mux.HandleFunc("/api/config/sensor-names", s.handleSensorNamesConfig)
	mux.HandleFunc("/api/config/serial-sensor", s.handleSerialSensorConfig)
	mux.HandleFunc("/api/config/ui-prefs", s.handleUIPrefsConfig)
	mux.HandleFunc("/api/config/log", s.handleLogConfig)
	mux.HandleFunc("/api/log/clear", s.handleLogClear)
	mux.HandleFunc("/api/log/export", s.handleLogExport)
	mux.HandleFunc("/api/fans/debug", s.handleFansDebug)
	mux.HandleFunc("/api/fans/debug/takeover", s.handleFansDebugTakeover)
	mux.HandleFunc("/api/fans/debug/pwm", s.handleFansDebugPWM)
	mux.HandleFunc("/api/fans/debug/auto", s.handleFansDebugAuto)
	mux.HandleFunc("/api/fans/debug/auto/stop", s.handleFansDebugAutoStop)
	mux.HandleFunc("/api/fans/debug/calibrate", s.handleFansDebugCalibrate)
	mux.HandleFunc("/api/apply", s.handleApply)
	mux.HandleFunc("/api/restore", s.handleRestore)
	mux.Handle("/", http.FileServer(http.Dir(s.WebRoot)))

	handler := s.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasPrefix(path, s.BasePath) {
			path = strings.TrimPrefix(path, s.BasePath)
			if path == "" {
				path = "/"
			}
			r.URL.Path = path
		}
		mux.ServeHTTP(w, r)
	}))
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	return server.Serve(listener)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, s.Manager.Status())
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.History == nil {
		writeJSON(w, http.StatusOK, historyFile{Version: historyFileVersion})
		return
	}
	rangeHours := parseFloatQuery(r, "range", 24)
	samples, interval, err := s.History.Aggregated(r.Context(), rangeHours, historyMaxPoints, time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取历史数据失败: "+err.Error())
		return
	}
	defaultGroups := ReclassifySensorGroups(samples)
	ApplySensorGroupOverrides(samples, s.Manager.SensorGroupOverrides())
	writeJSON(w, http.StatusOK, historyFile{Version: historyFileVersion, IntervalSeconds: interval, Samples: samples, DefaultGroups: defaultGroups})
}

// handleHistoryExportSQL 下载 history.db 的一致性快照（VACUUM INTO，含 WAL 数据）。
func (s *Server) handleHistoryExportSQL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if !isAdmin(r) {
		writeError(w, http.StatusForbidden, "仅管理员可以导出历史数据")
		return
	}
	if s.History == nil {
		writeError(w, http.StatusServiceUnavailable, "历史数据存储不可用")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="tad-module-history.db"`)
	if err := s.History.ExportSQLite(r.Context(), w); err != nil {
		s.Logger.Printf("history sql export failed: %v", err)
	}
}

// handleHistoryExportCSV 把全部历史采样导出为宽表 CSV（UTF-8 带 BOM）。
func (s *Server) handleHistoryExportCSV(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if !isAdmin(r) {
		writeError(w, http.StatusForbidden, "仅管理员可以导出历史数据")
		return
	}
	if s.History == nil {
		writeError(w, http.StatusServiceUnavailable, "历史数据存储不可用")
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="tad-module-history.csv"`)
	if err := s.History.WriteCSV(r.Context(), w); err != nil {
		s.Logger.Printf("history csv export failed: %v", err)
	}
}

func parseIntQuery(r *http.Request, name string, fallback int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func parseFloatQuery(r *http.Request, name string, fallback float64) float64 {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}
	return value
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !isAdmin(r) {
		writeError(w, http.StatusForbidden, "仅管理员可以修改功耗、风扇与按键配置")
		return
	}
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, configRequestMaxBytes))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "配置格式错误: "+err.Error())
		return
	}
	if err := s.Manager.SaveAndApply(cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Manager.Status())
}

func (s *Server) handleGlobalConfig(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigRequest(w, r) {
		return
	}
	var cfg GlobalConfig
	if err := decodeConfigRequest(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "配置格式错误: "+err.Error())
		return
	}
	if err := s.Manager.SaveGlobalConfig(cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Manager.Status())
}

func (s *Server) handleFanConfig(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigRequest(w, r) {
		return
	}
	var cfg FanConfig
	if err := decodeConfigRequest(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "配置格式错误: "+err.Error())
		return
	}
	if err := s.Manager.SaveFanConfig(cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Manager.Status())
}

// handleUIPrefsConfig 保存前端界面偏好（当前：历史温度范围档位）；
// 读取随 /api/status 的 config.ui_prefs 一起下发，无需单独 GET。
func (s *Server) handleUIPrefsConfig(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigRequest(w, r) {
		return
	}
	var payload struct {
		HistoryRangeHours float64             `json:"history_range_hours"`
		FanDebugVisible   bool                `json:"fan_debug_visible"`
		HistorySeries     map[string]bool     `json:"history_series"`
		HistoryChildren   map[string][]string `json:"history_children"`
	}
	if err := decodeConfigRequest(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "配置格式错误: "+err.Error())
		return
	}
	prefs := UIPrefsConfig{
		HistoryRangeHours: payload.HistoryRangeHours,
		FanDebugVisible:   payload.FanDebugVisible,
		HistorySeries:     payload.HistorySeries,
		HistoryChildren:   payload.HistoryChildren,
	}
	if err := s.Manager.SaveUIPrefs(prefs); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Manager.Status())
}

func (s *Server) handleSensorNamesConfig(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigRequest(w, r) {
		return
	}
	var payload struct {
		Names  map[string]string `json:"names"`
		Groups map[string]string `json:"groups"`
	}
	if err := decodeConfigRequest(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "配置格式错误: "+err.Error())
		return
	}
	if payload.Names == nil {
		payload.Names = map[string]string{}
	}
	if payload.Groups == nil {
		payload.Groups = map[string]string{}
	}
	if err := s.Manager.SaveSensorSettings(payload.Names, payload.Groups); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Manager.Status())
}

// handleSerialSensorConfig 保存整组 USB 串口温度传感器配置（多设备数组）；
// 保存即踢监督者按新配置增删读取器。各设备连接状态与最近读数随
// /api/status 的 serial 字段下发，无需单独 GET。
func (s *Server) handleSerialSensorConfig(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigRequest(w, r) {
		return
	}
	var payload struct {
		Configs []SerialSensorConfig `json:"configs"`
	}
	if err := decodeConfigRequest(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "配置格式错误: "+err.Error())
		return
	}
	if payload.Configs == nil {
		payload.Configs = []SerialSensorConfig{}
	}
	if err := s.Manager.SaveSerialSensorConfigs(payload.Configs); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Manager.Status())
}

func (s *Server) handleHistoryConfig(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigRequest(w, r) {
		return
	}
	var cfg HistoryConfig
	if err := decodeConfigRequest(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "配置格式错误: "+err.Error())
		return
	}
	if err := s.Manager.SaveHistoryConfig(cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 保存成功后立即同步保留期与长期记录设置并清理一次（缩短保留期当场生效），
	// 失败只记日志：配置本身已经落盘，采样循环的下一次 PruneIfNeeded 也会跟上。
	if s.History != nil {
		if err := s.History.SyncSettings(s.Manager.HistorySettings()); err != nil {
			s.Logger.Printf("history settings sync failed: %v", err)
		}
	}
	writeJSON(w, http.StatusOK, s.Manager.Status())
}

// handleHistoryArchive 手动补一次长期记录冲刷（清空数据库前前端会先调用，
// 让未冲刷的缓冲先落到归档盘，之后无论确认还是取消都不丢该归档的数据）。
// 长期记录未开启时是空操作，同样返回 ok。
func (s *Server) handleHistoryArchive(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigRequest(w, r) {
		return
	}
	if s.History == nil {
		writeError(w, http.StatusServiceUnavailable, "历史数据存储不可用")
		return
	}
	flushed, err := s.History.FlushArchive(time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "长期记录冲刷失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "flushed": flushed})
}

// handleLogConfig 保存运行日志的大小设置（与历史数据库大小上限解耦）。
func (s *Server) handleLogConfig(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigRequest(w, r) {
		return
	}
	var cfg LogConfig
	if err := decodeConfigRequest(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "配置格式错误: "+err.Error())
		return
	}
	if err := s.Manager.SaveLogConfig(cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Manager.Status())
}

// handleLogClear 清空运行日志：截断主文件并删除上一代备份。显式破坏操作，
// 由前端二次确认；未写入文件（stderr 模式）时是空操作。截断后留一条说明行。
func (s *Server) handleLogClear(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigRequest(w, r) {
		return
	}
	if s.LogPath == "" {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if err := os.Truncate(s.LogPath, 0); err != nil && !errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, "清空日志失败: "+err.Error())
		return
	}
	for _, suffix := range []string{".1", ".1.tmp"} {
		if err := os.Remove(s.LogPath + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusInternalServerError, "删除日志备份失败: "+err.Error())
			return
		}
	}
	s.Logger.Printf("log cleared by user")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleLogExport 下载运行日志（上一代备份在前、当前日志在后，时间顺序天然
// 衔接）。流式输出，不做整体加载；未写入文件时返回 404。运行日志可能含
// 设备路径等环境细节，与清空同级别仅限管理员。
func (s *Server) handleLogExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if !isAdmin(r) {
		writeError(w, http.StatusForbidden, "仅管理员可以下载运行日志")
		return
	}
	if s.LogPath == "" {
		writeError(w, http.StatusNotFound, "日志未写入文件")
		return
	}
	if _, err := os.Stat(s.LogPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, "暂无日志文件")
			return
		}
		writeError(w, http.StatusInternalServerError, "读取日志失败: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="tad-module-log.txt"`)
	w.WriteHeader(http.StatusOK)
	for _, path := range []string{s.LogPath + ".1", s.LogPath} {
		file, err := os.Open(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return // 头已发出，中途失败只能断流
		}
		_, err = io.Copy(w, file)
		file.Close()
		if err != nil {
			return
		}
	}
}

// ---- 风扇调试控制（调试页勾选后展示;全部管理员鉴权） ----

// handleFansDebug 返回调试状态与全部已发现风扇（含 0 转通道）。
func (s *Server) handleFansDebug(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if !isAdmin(r) {
		writeError(w, http.StatusForbidden, "仅管理员可以使用风扇调试")
		return
	}
	writeJSON(w, http.StatusOK, s.Manager.FanDebugState())
}

// handleFansDebugCalibrate 主动标定单个风扇的满转基准：全速运转至读数稳态
// （最多约 9 秒），学习特性表最高档并更新基准，随后恢复标定前的控制状态。
// 同步返回（前端按钮期间禁用行内控件）。
func (s *Server) handleFansDebugCalibrate(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigRequest(w, r) {
		return
	}
	if s.Manager == nil {
		writeError(w, http.StatusServiceUnavailable, "风扇控制不可用")
		return
	}
	var payload struct {
		ID string `json:"id"`
	}
	if err := decodeConfigRequest(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "配置格式错误: "+err.Error())
		return
	}
	if payload.ID == "" {
		writeError(w, http.StatusBadRequest, "缺少风扇 ID")
		return
	}
	base, err := s.Manager.CalibrateFanRPM(payload.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"ok": 1, "base": base})
}

// handleFansDebugTakeover 接管/释放单个风扇：接管后脱离一切曲线控制。
func (s *Server) handleFansDebugTakeover(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigRequest(w, r) {
		return
	}
	var payload struct {
		ID    string `json:"id"`
		Taken bool   `json:"taken"`
	}
	if err := decodeConfigRequest(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "配置格式错误: "+err.Error())
		return
	}
	if err := s.Manager.SetFanDebugTakeover(payload.ID, payload.Taken); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Manager.FanDebugState())
}

// handleFansDebugPWM 设定单个被接管风扇的调试转速（百分比）。
func (s *Server) handleFansDebugPWM(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigRequest(w, r) {
		return
	}
	var payload struct {
		ID    string `json:"id"`
		Value int    `json:"value"`
		Unit  string `json:"unit"`
	}
	if err := decodeConfigRequest(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "配置格式错误: "+err.Error())
		return
	}
	if err := s.Manager.SetFanDebugValue(payload.ID, payload.Value, payload.Unit); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Manager.FanDebugState())
}

// handleFansDebugAuto 按风扇启动自动递增:fans 列表逐个开启,每台风扇以各自的
// 手动转速为基准,按各自 step/interval/unit 递增到单位上限自动完成。
func (s *Server) handleFansDebugAuto(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigRequest(w, r) {
		return
	}
	var payload struct {
		Fans []struct {
			ID       string `json:"id"`
			Step     int    `json:"step"`
			Interval int    `json:"interval"`
			Unit     string `json:"unit"`
		} `json:"fans"`
	}
	if err := decodeConfigRequest(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "配置格式错误: "+err.Error())
		return
	}
	for _, fan := range payload.Fans {
		if err := s.Manager.SetFanDebugAuto(fan.ID, true, fan.Step, fan.Interval, fan.Unit); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, s.Manager.FanDebugState())
}

// handleFansDebugAutoStop 停止单个风扇的自动递增,保持当前转速,参数保留。
func (s *Server) handleFansDebugAutoStop(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigRequest(w, r) {
		return
	}
	var payload struct {
		ID string `json:"id"`
	}
	if err := decodeConfigRequest(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "配置格式错误: "+err.Error())
		return
	}
	if err := s.Manager.SetFanDebugAuto(payload.ID, false, 0, 0, ""); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Manager.FanDebugState())
}

// handleHistoryClear 清空历史数据库：四张表全删 + VACUUM 回收空间，
// schema 与配置保留，响应 {"ok": true}。
func (s *Server) handleHistoryClear(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigRequest(w, r) {
		return
	}
	if s.History == nil {
		writeError(w, http.StatusServiceUnavailable, "历史数据存储不可用")
		return
	}
	if err := s.History.Clear(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "清空历史数据失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleGPIOConfig(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigRequest(w, r) {
		return
	}
	var cfg GPIOConfig
	if err := decodeConfigRequest(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "配置格式错误: "+err.Error())
		return
	}
	if err := s.Manager.SaveGPIOConfig(cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Manager.Status())
}

func (s *Server) authorizeConfigRequest(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return false
	}
	if !isAdmin(r) {
		writeError(w, http.StatusForbidden, "仅管理员可以修改模块配置")
		return false
	}
	return true
}

func decodeConfigRequest(r *http.Request, target any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, configRequestMaxBytes))
	dec.DisallowUnknownFields()
	return dec.Decode(target)
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !isAdmin(r) {
		writeError(w, http.StatusForbidden, "仅管理员可以应用功耗、风扇与按键配置")
		return
	}
	if err := s.Manager.ApplyCurrent(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Manager.Status())
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !isAdmin(r) {
		writeError(w, http.StatusForbidden, "仅管理员可以恢复原始功耗与风扇配置")
		return
	}
	if err := s.Manager.DisableAndRestore(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Manager.Status())
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self' https://api.github.com; style-src 'self'; script-src 'self'; img-src 'self' data:; frame-ancestors 'self'")
		next.ServeHTTP(w, r)
	})
}

func isAdmin(r *http.Request) bool {
	value := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Trim-Isadmin")))
	return value == "true" || value == "1" || value == "yes"
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
