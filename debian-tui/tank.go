// tank - TAD6S4N hardware monitor TUI (native Go, single static binary).
//
// Reads the module's /api/status over its Unix socket and renders a terminal
// panel. HDD temperature is queried directly from smartctl WITHOUT the "-n
// standby" flag (the author's SMART parser is currently broken for smartctl
// 7.5's object-form "power_mode"), so sleeping drives get a real reading.
//
// Go native (stdlib only), no python/curses. Build:
//   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o tank .
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const defSocket = "/run/tank/tad-module.sock"

var (
	socket     = getenv("TANK_SOCKET", defSocket)
	refreshSec = atoi(getenv("TANK_REFRESH", "3"), 3)
	hddCache   sync.Map
)

// ---- /api/status subset ----------------------------------------------------

type status struct {
	DeviceName string        `json:"device_name"`
	OSName     string        `json:"os_name"`
	OSVersion  string        `json:"os_version"`
	CPUModel   string        `json:"cpu_model"`
	CPU        cpuTemp       `json:"cpu_temperature"`
	Packages   []pkgInfo     `json:"packages"`
	Fan        fanControl    `json:"fan_control"`
	Storage    storageStatus `json:"storage"`
	LastError  string        `json:"last_error"`
}
type cpuTemp struct {
	CoreMaxC    float64 `json:"core_max_c"`
	PackageMaxC float64 `json:"package_max_c"`
}
type pkgInfo struct {
	PL1W int64 `json:"pl1_w"`
	PL2W int64 `json:"pl2_w"`
}
type fanControl struct {
	DriverDetected bool     `json:"driver_detected"`
	Active         bool     `json:"active"`
	Temperature    float64  `json:"temperature_c"`
	HDDTemperature float64  `json:"hdd_temperature_c"`
	NVMeTemperature float64 `json:"nvme_temperature_c"`
	Fans           []fanDev `json:"fans"`
}
type fanDev struct {
	ID         string `json:"id"`
	RPM        int64  `json:"rpm"`
	PWMPercent int    `json:"pwm_percent"`
	Mode       int64  `json:"mode"`
}
type storageStatus struct {
	Slots     []slot `json:"slots"`
	LastError string `json:"last_error"`
}
type slot struct {
	ID          string  `json:"id"`
	Kind        string  `json:"kind"`
	Slot        int     `json:"slot"`
	State       string  `json:"state"`
	Device      string  `json:"device"`
	Model       string  `json:"model"`
	SizeBytes   int64   `json:"size_bytes"`
	Temperature float64 `json:"temperature_c"`
	Warning     string  `json:"warning"`
}

// ---- helpers ---------------------------------------------------------------

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
func atoi(s string, def int) int {
	v, err := strconv.Atoi(s)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

var httpClient = &http.Client{
	Timeout: 3 * time.Second,
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	},
}

func fetchStatus() (*status, error) {
	resp, err := httpClient.Get("http://localhost/api/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var st status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		io.Copy(io.Discard, resp.Body)
		return nil, err
	}
	return &st, nil
}

func readHDDTemp(dev string) (float64, bool) {
	if v, ok := hddCache.Load("hdd:" + dev); ok {
		if t, ok := v.(float64); ok && t > 0 {
			return t, true
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "smartctl", "-j", "-A", dev).Output()
	if err != nil {
		return 0, false
	}
	var doc struct {
		Temperature struct {
			Current float64 `json:"current"`
		} `json:"temperature"`
	}
	if json.Unmarshal(out, &doc) != nil || doc.Temperature.Current <= 0 {
		return 0, false
	}
	hddCache.Store("hdd:"+dev, doc.Temperature.Current)
	return doc.Temperature.Current, true
}

// ---- terminal io (stdlib, linux) -------------------------------------------

func ioctl(fd, req uintptr, arg uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg)
	if errno != 0 {
		return errno
	}
	return nil
}

type termios struct {
	Iflag  uint32
	Oflag  uint32
	Cflag  uint32
	Lflag  uint32
	Line   uint8
	Cc     [19]uint8
	Ispeed uint32
	Ospeed uint32
}

func makeRaw() (func(), error) {
	fd := uintptr(os.Stdin.Fd())
	var t termios
	if err := ioctl(fd, syscall.TCGETS, uintptr(unsafe.Pointer(&t))); err != nil {
		return nil, err
	}
	old := t
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB
	t.Cflag |= syscall.CS8
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	if err := ioctl(fd, syscall.TCSETS, uintptr(unsafe.Pointer(&t))); err != nil {
		return nil, err
	}
	restore := func() { _ = ioctl(fd, syscall.TCSETS, uintptr(unsafe.Pointer(&old))) }
	return restore, nil
}

func termSize() (int, int) {
	type winsize struct {
		Row, Col, Xpixel, Ypixel uint16
	}
	var ws winsize
	if ioctl(uintptr(os.Stdout.Fd()), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))) != nil {
		return 80, 24
	}
	if ws.Col == 0 {
		ws.Col = 80
	}
	if ws.Row == 0 {
		ws.Row = 24
	}
	return int(ws.Col), int(ws.Row)
}

// ---- rendering -------------------------------------------------------------

const (
	rst = "\033[0m"; bold = "\033[1m"; dim = "\033[2m"
	red = "\033[31m"; green = "\033[32m"; yellow = "\033[33m"
	cyan = "\033[36m"; white = "\033[97m"
	hide = "\033[?25l"; show = "\033[?25h"; home = "\033[H"; clr = "\033[2J"
)

func tempStr(c float64) string {
	if c <= 0 {
		return "--"
	}
	return fmt.Sprintf("%04.1f°C", c)
}

// isWide reports whether a rune occupies 2 terminal columns (East Asian Wide
// / Fullwidth). Block & geometric chars (█ □ ● ─ │ etc) are NOT wide, so the
// bay borders align on terminals that render them as a single column.
func isWide(r rune) bool {
	return (r >= 0x1100 && r <= 0x115F) || r == 0x2329 || r == 0x232A ||
		(r >= 0x2E80 && r <= 0xA4CF && r != 0x303F) ||
		(r >= 0xAC00 && r <= 0xD7A3) ||
		(r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0xFE30 && r <= 0xFE4F) ||
		(r >= 0xFF00 && r <= 0xFF60) ||
		(r >= 0xFFE0 && r <= 0xFFE6) ||
		(r >= 0x20000 && r <= 0x2FFFD) ||
		(r >= 0x30000 && r <= 0x3FFFD)
}

func dispWidth(s string) int {
	n := 0
	for _, r := range s {
		if isWide(r) {
			n += 2
		} else {
			n++
		}
	}
	return n
}
func pad(s string, w int) string {
	gap := w - dispWidth(s)
	if gap < 0 {
		// trim by display width
		out := []rune{}
		n := 0
		for _, r := range s {
			cw := 2
			if !isWide(r) {
				cw = 1
			}
			if n+cw > w {
				break
			}
			out = append(out, r)
			n += cw
		}
		return string(out)
	}
	return s + strings.Repeat(" ", gap)
}
func padTo(s string, w int) string {
	return pad(s, w)
}
func humanSize(b int64) string {
	if b <= 0 {
		return "-"
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	v := float64(b)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", int64(b))
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
func slotLabel(s slot) string {
	if s.Kind == "m2" {
		return fmt.Sprintf("[M.2 %d]", s.Slot)
	}
	return fmt.Sprintf("前置 %d", s.Slot)
}
func fanPL(st *status) string {
	var fan string
	if st.Fan.DriverDetected && len(st.Fan.Fans) > 0 {
		best := st.Fan.Fans[0]
		for _, f := range st.Fan.Fans {
			if f.RPM > best.RPM {
				best = f
			}
		}
		stA := ""
		if st.Fan.Active {
			stA = " *曲线"
		}
		fan = fmt.Sprintf("Fan: %dRPM(%d%%)%s", best.RPM, best.PWMPercent, stA)
	} else {
		fan = "Fan: N/A"
	}
	pl1, pl2 := int64(0), int64(0)
	for _, p := range st.Packages {
		if p.PL1W > pl1 {
			pl1 = p.PL1W
		}
		if p.PL2W > pl2 {
			pl2 = p.PL2W
		}
	}
	pl := "PL N/A"
	if pl1 > 0 || pl2 > 0 {
		pl = fmt.Sprintf("PL %02d/%02dW", pl1, pl2)
	}
	return fmt.Sprintf("%s | %s", fan, pl)
}

func glyph(state string) string {
	switch state {
	case "used", "present", "sleeping", "warning":
		return "█"
	}
	return "□"
}
func stateWord(state string) string {
	switch state {
	case "used":
		return "使用"
	case "present":
		return "未用"
	case "sleeping":
		return "休眠"
	case "warning":
		return "告警"
	default:
		return "空置"
	}
}

type boxCell struct{ lines []string }

// drawBox builds a bordered grid as lines. rows[r][c] is a cell; each cell has
// content lines. Item width (in terminal cells) is fixed so borders align.
func drawBox(rows [][]boxCell, width int) []string {
	if len(rows) == 0 || len(rows[0]) == 0 {
		return nil
	}
	ncols := len(rows[0])
	var out []string
	top := "┌"
	for c := 0; c < ncols; c++ {
		if c > 0 {
			top += "┬"
		}
		top += strings.Repeat("─", width)
	}
	top += "┐"
	out = append(out, top)

	for r, row := range rows {
		maxln := 0
		for _, cl := range row {
			if len(cl.lines) > maxln {
				maxln = len(cl.lines)
			}
		}
		for li := 0; li < maxln; li++ {
			s := ""
			for _, cl := range row {
				t := ""
				if li < len(cl.lines) {
					t = cl.lines[li]
				}
				s += "│" + padTo(t, width)
			}
			s += "│"
			out = append(out, s)
		}
		if r < len(rows)-1 {
			d := "├"
			for c := 0; c < ncols; c++ {
				if c > 0 {
					d += "┼"
				}
				d += strings.Repeat("─", width)
			}
			d += "┤"
			out = append(out, d)
		}
	}
	bottom := "└"
	for c := 0; c < ncols; c++ {
		if c > 0 {
			bottom += "┴"
		}
		bottom += strings.Repeat("─", width)
	}
	bottom += "┘"
	out = append(out, bottom)
	return out
}
// panelLines builds the full monitor panel as plain text lines (last line is
// the status hint). Used by both the live renderer and the one-shot print.
func panelLines(st *status) []string {
	lines := []string{}
	add := func(s string) { lines = append(lines, s) }

	// ── 数据头（拆分，改善对齐）──
	add("CPU: " + st.CPUModel)
	add(fanPL(st))
	add(fmt.Sprintf("Core: %s   Package: %s", tempStr(st.CPU.CoreMaxC), tempStr(st.CPU.PackageMaxC)))
	add("")

	// front bays + m2
	front, m2 := []slot{}, []slot{}
	for _, s := range st.Storage.Slots {
		if s.Kind == "front" {
			front = append(front, s)
		} else if s.Kind == "m2" {
			m2 = append(m2, s)
		}
	}
	add("[ 前置 3.5\" 硬盘槽位 ]")
	// enable HDD temps then build 6 cells (display 6..1 left-to-right)
	frontCells := []boxCell{}
	for i := len(front) - 1; i >= 0; i-- {
		s := front[i]
		if s.Device != "" {
			if t, ok := readHDDTemp(s.Device); ok {
				s.Temperature = t
			}
		}
		temp := tempStr(s.Temperature)
		if s.Temperature <= 0 {
			temp = "00.0°C" // 空仓/休眠：仍用定宽温度占位，保证对齐
		}
		frontCells = append(frontCells, boxCell{lines: []string{
			fmt.Sprintf("%d%s", s.Slot, glyph(s.State)),
			temp,
		}})
	}
	if len(frontCells) > 0 {
		for _, ln := range drawBox([][]boxCell{frontCells}, 6) {
			add(ln)
		}
	}
	add("")
	add("[ 内置 M.2 NVMe 槽位 ]")
	bySlot := map[int]slot{}
	for _, s := range m2 {
		s.Temperature = s.Temperature // keep API temp for M.2
		bySlot[s.Slot] = s
	}
	cell := func(num int) boxCell {
		s, ok := bySlot[num]
		st := "empty"
		if ok {
			st = s.State
		}
		temp := "00.0°C"
		if ok && s.Temperature > 0 {
			temp = tempStr(s.Temperature)
		}
		line1 := fmt.Sprintf("%d%s", num, glyph(st))
		return boxCell{lines: []string{line1, temp}}
	}
	m2rows := [][]boxCell{{cell(4), cell(2)}, {cell(3), cell(1)}}
	for _, ln := range drawBox(m2rows, 6) {
		add(ln)
	}
	add("")

	// detail table
	add(pad("槽位", 9) + pad("设备", 10) + pad("状态", 6) + pad("温度", 8) + "容量 / 型号")
	for _, s := range st.Storage.Slots {
		if s.Kind == "front" && s.Device != "" {
			if t, ok := readHDDTemp(s.Device); ok {
				s.Temperature = t
			}
		}
		model := fmt.Sprintf("%s - %s", humanSize(s.SizeBytes), s.Model)
		line := (pad(slotLabel(s), 9) + pad(trimBase(s.Device), 10) + pad(stateWord(s.State), 6) +
			pad(tempStr(s.Temperature), 8) + model)
		add(line)
	}

	// last line: status hint
	lines = append(lines, "Ctrl+C 退出监控  |  "+fmt.Sprintf("%d 秒刷新", refreshSec))
	return lines
}

func render(st *status, fetchErr string, w, h int) {
	if st == nil {
		fmt.Print(home + clr + red + "取数失败: " + fetchErr + rst + "\r\n")
		return
	}
	// ── 方案0：整屏一屏显示，不滚动/不翻页，超出即截断 ──
	fmt.Print(home + clr)
	max := h - 1
	if max < 1 {
		max = 1
	}
	lines := panelLines(st)
	for i, ln := range lines {
		if i >= max {
			break
		}
		if i == len(lines)-1 {
			fmt.Print(dim + ln + rst + "\r\n")
		} else {
			fmt.Print(ln + "\r\n")
		}
	}
}

func trimBase(dev string) string {
	if dev == "" {
		return "-"
	}
	return strings.TrimPrefix(dev, "/dev/")
}

// ---- main loop -------------------------------------------------------------

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--once" || os.Args[1] == "txt") {
		once()
		return
	}
	restore, err := makeRaw()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tank: raw mode:", err)
		os.Exit(1)
	}
	defer restore()
	fmt.Print(hide)
	defer fmt.Print(show)

	key := make(chan string, 32)
	go func() {
		b := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(b)
			if err != nil || n == 0 {
				return
			}
			switch b[0] {
			case 0x03:
				key <- "ctrl-c"
			case '\n', '\r':
				key <- "enter"
			case 'q':
				key <- "q"
			case 'r':
				key <- "r"
			case '2':
				key <- "2"
			}
		}
	}()

	// ── 欢迎屏：先提示，回车后进监控面板 ──
	welcome := []string{
		"请调大SSH窗口显示更完整本脚本3秒刷新一次，按下 Ctrl+C 退出监控，",
		"调整风扇转速请SSH界面输入 tankfan",
		"槽位号后的 █ 代表有盘，",
		"空心 □ 代表无盘，",
		"有盘但温度 00.0°C 就是休眠了",
		"",
		"直接按下回车进入实时刷新监控面板（需要大ssh窗口）",
		"输入 2 直接打印面板信息（适合手机ssh小窗口）",
		"当前版本V260905-2",
		"请输入数字",
	}
	fmt.Print(home + clr)
	for _, l := range welcome {
		fmt.Print(dim + l + rst + "\r\n")
	}
	for {
		select {
		case k := <-key:
			if k == "ctrl-c" || k == "q" {
				fmt.Print(show)
				return
			}
			if k == "enter" {
				goto monitor
			}
			if k == "2" {
				restore()
				fmt.Print(show)
				once()
				return
			}
		case <-time.After(200 * time.Millisecond):
		}
	}

monitor:
	var st *status
	var fetchErr string
	last := time.Time{}
	for {
		w, h := termSize()
		if st == nil || time.Since(last) >= time.Duration(refreshSec)*time.Second {
			if s, err := fetchStatus(); err != nil {
				fetchErr = err.Error()
			} else {
				st = s
				fetchErr = ""
			}
			last = time.Now()
		}
		render(st, fetchErr, w, h)

		select {
		case k := <-key:
			switch k {
			case "q", "ctrl-c":
				fmt.Print(show)
				return
			case "r":
				if s, err := fetchStatus(); err == nil {
					st = s
					fetchErr = ""
				} else {
					fetchErr = err.Error()
				}
			}
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func once() {
	st, err := fetchStatus()
	if err != nil {
		fmt.Println("tank: 无法连接", socket, "—", err)
		fmt.Println("提示: 先执行 systemctl start tank.service")
		os.Exit(1)
	}
	// 打印一帧完整的监控面板（与实时面板版式一致，不刷新），末尾弱化提示行
	lines := panelLines(st)
	for i, ln := range lines {
		if i == len(lines)-1 {
			fmt.Println(dim + ln + rst)
		} else {
			fmt.Println(ln)
		}
	}
}
