package crpsump

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------- 状态快照缓存 (提前读取: 打开页面即有数据) ----------------
//
// bitbang 读一轮完整状态需数百毫秒, iframe 首次打开时前端第一个请求等待明显。
// 后台协程周期刷新快照, /api/status 直接回缓存 (数据最多旧一个刷新周期);
// 快照未就绪 (刚启动/连续失败) 时回退实时读取, 保持旧行为。
// 刷新与请求共用总线锁, 串行化无竞争; 缓存也降低了多客户端轮询的总线压力。

const (
	StatusRefreshInterval = 3 * time.Second  // 快照后台刷新周期
	StatusSnapshotMaxAge  = 10 * time.Second // 快照最大可信时长, 超出回退实时读
)

var (
	statusMu   sync.Mutex
	statusSnap map[string]any
	statusTs   time.Time
	// fanSnapRefresh: 风扇设置后的延后快照刷新进行中标记 (防连点堆积 goroutine)
	fanSnapRefresh atomic.Bool
)

// refreshStatusSnapshot 立即读一轮并更新快照 (风扇切换后调用, 消除缓存滞后跳动)
func refreshStatusSnapshot(bus *Bus) {
	if snap := readAll(bus); snap != nil && snap["error"] == nil {
		statusMu.Lock()
		statusSnap = snap
		statusTs = time.Now()
		statusMu.Unlock()
	}
}

// StartStatusRefresher 启动状态快照后台刷新循环 (两个入口各调一次)
func StartStatusRefresher(bus *Bus) {
	go func() {
		for {
			refreshStatusSnapshot(bus)
			time.Sleep(StatusRefreshInterval)
		}
	}()
}

// cachedStatus 优先回状态快照; 快照缺失/过期时回退实时读取
func cachedStatus(bus *Bus) map[string]any {
	statusMu.Lock()
	snap, age := statusSnap, time.Since(statusTs)
	statusMu.Unlock()
	if snap != nil && age <= StatusSnapshotMaxAge {
		return snap
	}
	return readAll(bus) // 回退: 快照未就绪/过期时实时读
}

// ---------------- 参数读取 ----------------

// emptyStatus 生成全空的 /api/status JSON (err 为错误码)
func emptyStatus(err string) map[string]any {
	return map[string]any{
		"ok": false, "error": err,
		"vin": nil, "vout": nil, "iout": nil, "pin": nil, "pout": nil,
		"iin": nil, "iin_calc": nil, "pf": nil,
		"efficiency": nil, "temp1": nil, "temp2": nil, "fan": nil,
		"status_raw": nil, "bus": "gpio", "addr": "0x5b",
		"ts":          time.Now().Unix(),
		"app_version": AppVersion,
		"mfr_id":      "", "mfr_model": "", "mfr_rev": "",
	}
}

// readAll 实时读一轮电源全部参数并组装 /api/status JSON
func readAll(b *Bus) map[string]any {
	if b == nil {
		return emptyStatus("no-gpio")
	}
	if !b.isConfigured() {
		// 尚未在界面设置 PSCL/PSDA: 不读取电源参数
		return emptyStatus("gpio-not-configured")
	}
	if !b.isOK() {
		return emptyStatus("no-gpio")
	}
	sw := b.readWord(Cmd["STATUS_WORD"])
	var statusRaw any
	var okFlag any
	if sw != nil {
		statusRaw = fmt.Sprintf("%04X", *sw)
		okFlag = *sw == 0x0000
	}
	vin := linear11Ptr(b.readWord(Cmd["READ_VIN"]))
	iin := linear11Ptr(b.readWord(Cmd["READ_IIN"]))
	vout := readVoutReal(b) // 用 VOUT_MODE 指数解码
	iout := linear11Ptr(b.readWord(Cmd["READ_IOUT"]))
	pin := linear11Ptr(b.readWord(Cmd["READ_PIN"]))
	pout := linear11Ptr(b.readWord(Cmd["READ_POUT"]))
	t1 := linear11Ptr(b.readWord(Cmd["READ_T1"]))
	t2 := linear11Ptr(b.readWord(Cmd["READ_T2"]))
	fan := linear11Ptr(b.readWord(Cmd["READ_FAN1"]))

	var eff any
	if pin != nil && pout != nil && *pin > 0 {
		eff = round1(*pout / *pin * 100)
	}
	var calcVout any
	if pout != nil && iout != nil && *iout > 0 {
		calcVout = round2(*pout / *iout)
	}
	var iinCalc any
	if pin != nil && vin != nil && *vin > 0 {
		iinCalc = round2(*pin / *vin)
	}
	// 功率因数: PF = 有功功率 PIN / (VIN × IIN)。交流输入下 PF<1(含无功/谐波分量)
	var pf any
	if pin != nil && vin != nil && iin != nil && *vin > 0 && *iin > 0 {
		pf = round3(*pin / (*vin * *iin))
	}

	if energyStats != nil && pin != nil && *pin >= 0 {
		energyStats.UpdatePin(*pin) // 刷新实时功率显示 (积分由后台采样器负责)
	}

	mfrID, mfrModel := getMfr(b)
	var voutFinal any = fval(vout)
	if voutFinal == nil {
		voutFinal = calcVout
	}
	return map[string]any{
		"ok": okFlag, "status_raw": statusRaw,
		"vin": fval(vin), "vout": voutFinal, "vout_calc": calcVout,
		"iout": fval(iout), "iin": fval(iin), "iin_calc": iinCalc,
		"pin": fval(pin), "pout": fval(pout),
		"efficiency": eff, "pf": pf,
		"temp1": fval(t1), "temp2": fval(t2), "fan": fval(fan),
		"mfr_id": mfrID, "mfr_model": mfrModel,
		"fan_mode":    b.fanModeNow(),
		"bus":         "gpio",
		"addr":        fmt.Sprintf("0x%02X", b.addr),
		"ts":          time.Now().Unix(),
		"app_version": AppVersion,
	}
}

// ---------------- HTTP 服务 ----------------

// AdminCheck 写操作管理员鉴权钩子, 与宿主应用同一套校验 (宿主挂载前注入其 isAdmin,
// 校验 X-Trim-Isadmin 头)。未注入时 (独立运行) 默认放行, 保持旧行为。
var AdminCheck = func(r *http.Request) bool { return true }

// jsonBodyMaxBytes POST JSON body 上限 (对齐宿主配置接口的 LimitReader 做法)
const jsonBodyMaxBytes = 1 << 20

type AppServer struct {
	Bus        *Bus
	WebRoot    string
	ConfigPath string
	// DetectFn 可注入的自动探测函数 (单测失败路径用, 绕过真实 bitbang 扫描);
	// nil 时使用 Bus.autoDetect。第三个返回值是探测收尾清理错误 (针脚释放失败)。
	DetectFn func(ctx context.Context) (string, string, error)
}

var mimeTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".js":   "application/javascript; charset=utf-8",
	".png":  "image/png",
	".svg":  "image/svg+xml",
}

// stripGateway 剥掉网关前缀 (独立 serve 经网关反代时路径还原)
func (s *AppServer) stripGateway(path string) string {
	if strings.HasPrefix(path, GatewayPrefix) {
		path = path[len(GatewayPrefix):]
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
	}
	if path == "" {
		return "/"
	}
	return path
}

// send 统一写响应: 字符串/字节直出, 其他按 JSON 编码
func (s *AppServer) send(w http.ResponseWriter, code int, body any, ctype string) {
	var data []byte
	switch v := body.(type) {
	case string:
		data = []byte(v)
	case []byte:
		data = v
	default:
		data, _ = json.Marshal(body)
		if ctype == "" {
			ctype = "application/json"
		}
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(code)
	w.Write(data)
}

// serveStatic 提供 WebRoot 内静态文件 (防目录穿越)
func (s *AppServer) serveStatic(w http.ResponseWriter, path string) {
	if path == "/" || path == "" {
		path = "/index.html"
	}
	root, _ := filepath.Abs(s.WebRoot)
	full, err := filepath.Abs(filepath.Join(s.WebRoot, strings.TrimPrefix(path, "/")))
	if err != nil || !strings.HasPrefix(full, root+string(filepath.Separator)) {
		s.send(w, 404, "Not Found", "text/plain; charset=utf-8")
		return
	}
	if info, err := os.Stat(full); err != nil || info.IsDir() {
		s.send(w, 404, "Not Found", "text/plain; charset=utf-8")
		return
	}
	data, err := os.ReadFile(full)
	if err != nil {
		s.send(w, 404, "Not Found", "text/plain; charset=utf-8")
		return
	}
	ctype := mimeTypes[strings.ToLower(filepath.Ext(full))]
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	s.send(w, 200, data, ctype)
}

// doGet 分发 GET 请求到各 API 端点 (非 /api/ 路径回落静态文件)
func (s *AppServer) doGet(w http.ResponseWriter, r *http.Request, p string) {
	ctx := r.Context()
	switch p {
	case "/api/status":
		s.send(w, 200, cachedStatus(s.Bus), "application/json")
	case "/api/bus":
		addr := "0x5b"
		if s.Bus != nil {
			addr = fmt.Sprintf("0x%02X", s.Bus.addr)
		}
		s.send(w, 200, map[string]any{"bus": "gpio", "addr": addr}, "application/json")
	case "/api/energy":
		if energyStats != nil {
			s.send(w, 200, energyStats.Snapshot(), "application/json")
		} else {
			s.send(w, 200, map[string]any{"ok": false, "error": "no-energy"}, "application/json")
		}
	case "/api/log":
		// 下载运行日志 (可能含硬件信息, 与写操作同级要求管理员)。
		// O_NOFOLLOW 拒绝符号链接 (/tmp 多用户可写, 防 root 被劫持读任意文件);
		// 完整错误只记服务端日志, 不向客户端回显原始错误文本 (防泄露服务器路径)。
		if !AdminCheck(r) {
			s.send(w, http.StatusForbidden, map[string]any{"error": "仅管理员可以下载日志"}, "application/json")
			return
		}
		f, err := os.OpenFile(LogPath, os.O_RDONLY|openNoFollow, 0)
		if err != nil {
			s.send(w, 200, "日志文件不存在(尚无输出)", "text/plain; charset=utf-8")
			return
		}
		defer f.Close()
		if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
			s.send(w, 200, "日志文件不存在(尚无输出)", "text/plain; charset=utf-8")
			return
		}
		body, err := io.ReadAll(f)
		if err != nil {
			Logf("读取运行日志失败: %v", err)
			s.send(w, 500, map[string]any{"error": "read-failed"}, "application/json")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="psu-monitor.log"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(200)
		w.Write(body)
	case "/api/config":
		if s.Bus != nil {
			scl, sda := s.Bus.pinNames()
			// readable 带 TTL 缓存, 前端轮询 config 不会持续压总线
			s.send(w, 200, map[string]any{
				"ok":          true,
				"scl_pin":     scl,
				"sda_pin":     sda,
				"pins":        UIPins,
				"connected":   s.Bus.isOK(),        // 仅 GPIO 方向是否 OK
				"readable":    s.Bus.readable(ctx), // 真实能否读到电源数据 (TTL 缓存)
				"configured":  s.Bus.isConfigured(),
				"app_version": AppVersion,
			}, "application/json")
		} else {
			s.send(w, 200, map[string]any{"ok": false, "error": "no-gpio"}, "application/json")
		}
	default:
		if strings.HasPrefix(p, "/api/") {
			s.send(w, 404, map[string]any{"error": "not found"}, "application/json")
		} else {
			s.serveStatic(w, p)
		}
	}
}

// readJSONBody 读并解析 POST JSON body: 超过 jsonBodyMaxBytes 返回 *http.MaxBytesError,
// 非法 JSON 返回解析错误。调用方必须拒绝出错请求, 不得以默认值继续执行写操作
// (静默截断可能把超限/残缺 body 解析成"合法"默认值而误写硬件)。
func readJSONBody(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, jsonBodyMaxBytes))
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m == nil { // JSON "null": Unmarshal 不报错但置 nil, 按非法 body 拒绝 (否则调用方空响应 200)
		return nil, errors.New("body 必须是 JSON 对象")
	}
	return m, nil
}

// jsonBodyOrReject 读取并校验 POST JSON body; 非法 JSON 回 400 / 超限回 413 并返回 nil
// (调用方须立即 return 停止处理)
func (s *AppServer) jsonBodyOrReject(w http.ResponseWriter, r *http.Request) map[string]any {
	m, err := readJSONBody(w, r)
	if err == nil {
		return m
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		s.send(w, http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "error": "请求体超过 1MB 上限"}, "application/json")
	} else {
		s.send(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "非法 JSON 请求体"}, "application/json")
	}
	return nil
}

// doPost 分发 POST 请求 (所有写接口先过管理员鉴权)
func (s *AppServer) doPost(w http.ResponseWriter, r *http.Request, p string) {
	ctx := r.Context()
	// 所有写接口 (fan/config/detect/energy reset) 要求管理员, 与宿主同一套鉴权
	if !AdminCheck(r) {
		s.send(w, http.StatusForbidden, map[string]any{"error": "仅管理员可以执行此操作"}, "application/json")
		return
	}
	switch p {
	case "/api/fan":
		if s.Bus == nil || !s.Bus.isConfigured() {
			s.send(w, 200, map[string]any{"ok": false, "error": "gpio-not-configured"}, "application/json")
			return
		}
		if !s.Bus.isOK() {
			s.send(w, 200, map[string]any{"ok": false, "error": "no-gpio"}, "application/json")
			return
		}
		body := s.jsonBodyOrReject(w, r)
		if body == nil {
			return // 非法/超限 body 已回 400/413
		}
		mode := "manual"
		if m, ok := body["mode"].(string); ok {
			mode = m
		}
		pct := 10
		if v, ok := body["pct"].(float64); ok {
			pct = int(v)
		} else if v, ok := body["percent"].(float64); ok {
			pct = int(v)
		}
		if pct < 10 { // 手动指定占比范围 10-100%
			pct = 10
		}
		if pct > 100 {
			pct = 100
		}
		ok := s.Bus.setFan(ctx, mode, pct)
		// 异步延后刷新快照: 不在响应路径同步读 (会与后台循环/连续切换抢总线锁拖慢响应),
		// 等 ~1.2s 让电源固件转速先动起来再读一轮, 前端随后立即拉取, 转速显示延迟
		// 从最坏 ~6s (快照3s+轮询3s) 压到 ~1.5s。合并标记防连点堆积多个刷新。
		if ok && fanSnapRefresh.CompareAndSwap(false, true) {
			go func() {
				time.Sleep(1200 * time.Millisecond)
				refreshStatusSnapshot(s.Bus)
				fanSnapRefresh.Store(false)
			}()
		}
		s.send(w, 200, map[string]any{"ok": ok, "mode": s.Bus.fanModeNow(), "pct": pct}, "application/json")
	case "/api/config":
		if s.Bus == nil {
			s.send(w, 200, map[string]any{"ok": false, "error": "no-gpio"}, "application/json")
			return
		}
		body := s.jsonBodyOrReject(w, r)
		if body == nil {
			return // 非法/超限 body 已回 400/413
		}
		var sclPtr, sdaPtr *string
		if v, ok := body["scl_pin"].(string); ok {
			sclPtr = &v
		}
		if v, ok := body["sda_pin"].(string); ok {
			sdaPtr = &v
		}
		ok, msg := s.Bus.reconfigure(sclPtr, sdaPtr)
		if ok && s.ConfigPath != "" {
			scl, sda := s.Bus.pinNames()
			cfg := map[string]string{"scl_pin": scl, "sda_pin": sda}
			if data, err := json.MarshalIndent(cfg, "", "  "); err == nil {
				if err := os.WriteFile(s.ConfigPath, data, 0644); err != nil {
					Logf("GPIO 配置写盘失败 (%s): %v", s.ConfigPath, err) // 完整错误只记服务端, 不回显路径
					msg = "已应用, 但配置保存失败"
				}
			}
		}
		scl, sda := s.Bus.pinNames()
		s.send(w, 200, map[string]any{
			"ok": ok, "msg": msg,
			"scl_pin": scl, "sda_pin": sda,
		}, "application/json")
	case "/api/detect":
		if s.Bus == nil {
			s.send(w, 200, map[string]any{"ok": false, "error": "no-gpio"}, "application/json")
			return
		}
		detect := s.Bus.autoDetect
		if s.DetectFn != nil {
			detect = s.DetectFn // 单测注入, 绕过真实 bitbang 扫描
		}
		scl, sda, cleanupErr := detect(ctx)
		if scl == "" {
			if cleanupErr != nil {
				// 未找到接线且收尾清理失败: 针脚可能仍为输出, 必须与
				// "未找到且已清理完成" 区分开。错误细节只记服务端日志,
				// 不下发给调用方 (内部针脚/寄存器信息无须暴露)。
				Logf("探测未命中且清理失败: %v", cleanupErr)
				s.send(w, 200, map[string]any{
					"ok": false, "found": false, "cleanup_failed": true,
					"msg": "探测未命中, 且清理 GPIO 失败, 请重试",
				}, "application/json")
				return
			}
			s.send(w, 200, map[string]any{
				"ok": false, "found": false,
				"msg": "未探测到电源接线, 请检查线缆后重试",
			}, "application/json")
			return
		}
		// 找到接线 ≠ 已应用: reconfigure (方向写+总线复位) 失败必须如实上报,
		// 不得无条件回 ok:true 让界面误以为已就绪
		ok, msg := s.Bus.reconfigure(&scl, &sda)
		if !ok {
			Logf("探测到接线 PSCL=%s PSDA=%s 但应用失败: %s", scl, sda, msg)
			s.send(w, 200, map[string]any{
				"ok": false, "found": true, "applied": false, "saved": false,
				"scl_pin": scl, "sda_pin": sda,
				"error": "apply-failed",
				"msg":   "已找到接线 PSCL=" + scl + " PSDA=" + sda + "，但应用失败: " + msg,
			}, "application/json")
			return
		}
		// 已应用 ≠ 已保存: 写盘失败要明确返回给界面 (重启后配置会丢), 不能只记日志
		saved := true
		resultMsg := "自动探测到接线 PSCL=" + scl + " PSDA=" + sda + "，已应用并保存"
		if s.ConfigPath != "" {
			cs, cd := s.Bus.pinNames()
			cfg := map[string]string{"scl_pin": cs, "sda_pin": cd}
			if data, err := json.MarshalIndent(cfg, "", "  "); err == nil {
				if err := os.WriteFile(s.ConfigPath, data, 0644); err != nil {
					Logf("已探测到, 但配置保存失败 (%s): %v", s.ConfigPath, err) // 完整错误只记服务端
					saved = false
					resultMsg = "已应用 PSCL=" + scl + " PSDA=" + sda + "，但配置保存失败，重启后会丢失"
				}
			}
		}
		s.send(w, 200, map[string]any{
			"ok": true, "found": true, "applied": true, "saved": saved,
			"scl_pin": scl, "sda_pin": sda, "msg": resultMsg,
		}, "application/json")
	case "/api/energy":
		if energyStats == nil {
			s.send(w, 200, map[string]any{"ok": false, "error": "no-energy"}, "application/json")
			return
		}
		body := s.jsonBodyOrReject(w, r)
		if body == nil {
			return // 非法/超限 body 已回 400/413
		}
		action, _ := body["action"].(string)
		if action == "reset" {
			if err := energyStats.Reset(); err != nil { // 内存已清但落盘失败: 如实报错, 避免重启后旧统计复活
				s.send(w, 500, map[string]any{"ok": false, "error": "energy-save-failed"}, "application/json")
				return
			}
			s.send(w, 200, map[string]any{"ok": true, "msg": "耗电统计已清零"}, "application/json")
		} else {
			s.send(w, 200, map[string]any{"ok": false, "error": "unknown-action"}, "application/json")
		}
	default:
		s.send(w, 404, map[string]any{"error": "not found"}, "application/json")
	}
}

// ServeHTTP 实现 http.Handler: 剥网关前缀后按方法分发
func (s *AppServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := s.stripGateway(r.URL.Path)
	switch r.Method {
	case http.MethodGet:
		s.doGet(w, r, p)
	case http.MethodPost:
		s.doPost(w, r, p)
	default:
		s.send(w, 405, map[string]any{"error": "method not allowed"}, "application/json")
	}
}

// APIHandler 供宿主应用 (tad-module) 挂载 CRPS API 子路由:
// 宿主以 mux.Handle("/api/crps/", srv.APIHandler()) 挂载, 本 handler 把
// 融合挂载路径 /api/crps/<ep> 改写回板块原路由 /api/<ep> 后分发,
// 与独立 serve 模式行为一致。
// 融合模式下静态 UI 由宿主的 WebRoot FileServer 提供, 不经过本 handler。
func (s *AppServer) APIHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Path; strings.HasPrefix(p, "/api/crps/") {
			r.URL.Path = "/api/" + strings.TrimPrefix(p, "/api/crps/")
		}
		s.ServeHTTP(w, r)
	})
}

// ---------------- 启动自检 ----------------

// LogStartup 启动自检 (原 serve 入口启动段): GPIO 已配置时读一次 VIN 判断接线连通,
// 否则提示先在界面设置。sclPin/sdaPin 为配置文件解析出的原始引脚名 (NewBus 可能已回退纠正)。
func LogStartup(bus *Bus, sclPin, sdaPin string) {
	if bus.isConfigured() {
		var vin *uint16
		if bus.isOK() {
			vin = bus.readWord(Cmd["READ_VIN"])
		}
		scl, sda := bus.pinNames()
		Logf("GPIO bitbang PMBus: SCL=%s(0x%X/b%d) SDA=%s(0x%X/b%d) addr=0x%02X readable=%s",
			scl, GPPins[scl].Port, GPPins[scl].Bit,
			sda, GPPins[sda].Port, GPPins[sda].Bit, bus.addr,
			map[bool]string{true: "YES", false: "NO"}[vin != nil])
	} else {
		Logf("GPIO 引脚未配置: 请在界面设置 PSCL/PSDA 后开始读取。默认候选 SCL=%s SDA=%s", sclPin, sdaPin)
	}
}
