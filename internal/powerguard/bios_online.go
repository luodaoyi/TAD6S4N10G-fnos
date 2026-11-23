package powerguard

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// BIOS 在线升级：从 ky.mi-d.cn 拉取 zip.txt 索引（由 BIOS-FID-Tool 生成），
// 解析各机型固件的 $FID 版本信息，与当前 flash 的 $FID 比对决定是否需要升级。
// 安全约束（2026-10-23 用户确认）：
//   - 机型判定只认压缩包名里的 -N100-/-N150-/-N305- 段，必须与本机 CPU 型号完全一致；
//     N150 与 N100/N305 互刷会砖，N100 与 N305 互刷虽不变砖但有问题，一律禁止跨机型。
//   - $FID 主板 ID 编的是版本号（TADSNx10 的 x 对应主版本），不能用来区分机型。
//   - 新旧判定用 $FID 构建日期；更新日志只显示日期与压缩包文件名日期匹配的段落。

const biosIndexMaxBytes = 4 << 20

var (
	biosIndexDateRe  = regexp.MustCompile(`(?m)^\s*(\d{4}_\d{2}_\d{2})\s*$`)
	biosZipModelRe   = regexp.MustCompile(`-(N100|N150|N305)-`)
	biosZipDateRe    = regexp.MustCompile(`(\d{4}_\d{2}_\d{2})`)
	errBIOSNoCheck   = errors.New("请先执行「检查更新」")
	errBIOSNoUpdate  = errors.New("当前没有可升级的 BIOS 版本")
	errBIOSModelConf = errors.New("固件机型与本机 CPU 型号不一致，已拦截（跨机型刷写可能变砖）")
)

// biosFIDReadHook 测试注入的当前 flash 读取实现；nil 时走 flashrom 真实读取。
var biosFIDReadHook func(ctx context.Context, m *Manager) ([]byte, error)

type biosFID struct {
	BoardID   string
	Version   string
	BuildDate time.Time
	BuildText string
	// degraded: 版本来自 dmidecode 兜底(AMI 主版本),不是 flashrom 的精确
	// $FID——群晖 DSM 自带 libpci 过旧,厂商 flashrom 跑不起来
	degraded bool
}

type biosIndexRecord struct {
	ZipName   string
	File      string
	BoardID   string
	Version   string
	BuildDate time.Time
	BuildText string
	Size      int64
	SHA256    string
}

type biosChangelog struct {
	Date string
	Text string
}

// BIOSOnlineStatus 是在线检查的结果，全部可 JSON 序列化给前端。
type BIOSOnlineStatus struct {
	CheckedAt        time.Time `json:"checked_at"`
	Model            string    `json:"model"`
	CurrentBoardID   string    `json:"current_board_id"`
	CurrentVersion   string    `json:"current_version"`
	CurrentBuildDate string    `json:"current_build_date"`
	CurrentError     string    `json:"current_error,omitempty"`
	// CurrentVersionNote 非空表示当前版本来自降级读取(如 DSM 上的 dmidecode),
	// 只到 AMI 主版本精度,前端随版本号一并展示
	CurrentVersionNote string `json:"current_version_note,omitempty"`
	// Checking: 检查进行中。各阶段完成即发布快照进缓存,前端轮询渐进渲染。
	Checking           bool   `json:"checking,omitempty"`
	CandidateZip       string `json:"candidate_zip"`
	CandidateVersion   string `json:"candidate_version"`
	CandidateBuildDate string `json:"candidate_build_date"`
	DownloadURL        string `json:"download_url"`
	UpdateAvailable    bool   `json:"update_available"`
	Changelog          string `json:"changelog"`
	Error              string `json:"error,omitempty"`
}

// biosOnlineCache 缓存最近一次检查选中的候选记录，刷写时凭它下载。
type biosOnlineCache struct {
	status    BIOSOnlineStatus
	candidate biosIndexRecord
	hasCandi  bool
}

func biosIndexURL() string {
	return "https://ky.mi-d.cn/ky/TANK%E8%AE%A2%E5%88%B6%E4%B8%BB%E6%9D%BF%E7%9B%B8%E5%85%B3/TAD6S4N10G(N00_N305)/%E4%B8%BB%E6%9D%BFBIOS/zip.txt"
}

func biosZipURL(zipName string) string {
	return "https://ky.mi-d.cn/ky/TANK%E8%AE%A2%E5%88%B6%E4%B8%BB%E6%9D%BF%E7%9B%B8%E5%85%B3/TAD6S4N10G(N00_N305)/%E4%B8%BB%E6%9D%BFBIOS/" + zipName
}

// parseBIOSFID 在 BIOS 镜像中定位 $FID 标识并解析主板 ID、版本号与构建日期。
// 布局：$FID + 3 字节 + 8 字节主板 ID(NUL 结尾) + 16 字节 + 4 组"dd\0"版本 + 6 字节日期(年 u16le/月/日/时/分)。
func parseBIOSFID(data []byte) (biosFID, bool) {
	var fid biosFID
	for i := 0; i+64 < len(data); i++ {
		if data[i] != '$' || data[i+1] != 'F' || data[i+2] != 'I' || data[i+3] != 'D' {
			continue
		}
		boardID := strings.TrimRight(string(data[i+7:i+15]), "\x00")
		for p := i + 16; p < i+80 && p+18 < len(data); p++ {
			parts := make([]string, 0, 4)
			ok := true
			for g := 0; g < 4; g++ {
				b0, b1, b2 := data[p+g*3], data[p+g*3+1], data[p+g*3+2]
				if b0 >= '0' && b0 <= '9' && b1 >= '0' && b1 <= '9' && b2 == 0 {
					parts = append(parts, string([]byte{b0, b1}))
				} else {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			dp := p + 12
			year := int(data[dp]) | int(data[dp+1])<<8
			month, day := int(data[dp+2]), int(data[dp+3])
			hour, minute := int(data[dp+4]), int(data[dp+5])
			if year < 2020 || year > 2035 || month < 1 || month > 12 || day < 1 || day > 31 {
				continue
			}
			fid.BoardID = boardID
			fid.Version = strings.Join(parts, ".")
			fid.BuildDate = time.Date(year, time.Month(month), day, hour, minute, 0, 0, time.UTC)
			fid.BuildText = fmt.Sprintf("%04d-%02d-%02d %02d:%02d", year, month, day, hour, minute)
			return fid, true
		}
	}
	return fid, false
}

// parseBIOSIndex 解析 zip.txt：含 [upgrade] 段的块解析为固件记录，其余含日期行的块解析为更新日志。
func parseBIOSIndex(text string) ([]biosIndexRecord, []biosChangelog) {
	// Windows 工具写出的 zip.txt 带 UTF-8 BOM，不剥掉会混进第一条更新日志的标题
	text = strings.TrimPrefix(text, "\uFEFF")
	var records []biosIndexRecord
	var changelogs []biosChangelog
	for _, block := range splitBIOSBlocks(text) {
		if strings.Contains(block, "[upgrade]") {
			if record, ok := parseBIOSUpgradeBlock(block); ok {
				records = append(records, record)
			}
			continue
		}
		if changelog, ok := parseBIOSChangelogBlock(block); ok {
			changelogs = append(changelogs, changelog)
		}
	}
	return records, changelogs
}

// splitBIOSBlocks 按纯等号行（分隔线）切块。
func splitBIOSBlocks(text string) []string {
	var blocks []string
	var current strings.Builder
	flush := func() {
		if trimmed := strings.TrimSpace(current.String()); trimmed != "" {
			blocks = append(blocks, trimmed)
		}
		current.Reset()
	}
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) >= 10 && strings.Trim(trimmed, "=") == "" {
			flush()
			continue
		}
		current.WriteString(line)
		current.WriteString("\n")
	}
	flush()
	return blocks
}

func parseBIOSUpgradeBlock(block string) (biosIndexRecord, bool) {
	var record biosIndexRecord
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "压缩包:") {
			record.ZipName = strings.TrimSpace(strings.TrimPrefix(line, "压缩包:"))
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "file":
			record.File = value
		case "board_id":
			record.BoardID = value
		case "version":
			record.Version = value
		case "build_date":
			record.BuildText = value
			if parsed, err := time.ParseInLocation("2006-01-02 15:04", value, time.UTC); err == nil {
				record.BuildDate = parsed
			}
		case "size":
			record.Size, _ = strconv.ParseInt(value, 10, 64)
		case "sha256":
			record.SHA256 = strings.ToUpper(value)
		}
	}
	return record, record.File != "" && record.SHA256 != ""
}

func parseBIOSChangelogBlock(block string) (biosChangelog, bool) {
	var changelog biosChangelog
	match := biosIndexDateRe.FindStringSubmatch(block)
	if len(match) < 2 {
		return changelog, false
	}
	changelog.Date = match[1]
	// 去掉标题行与日期行，剩下的就是日志正文。
	var lines []string
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed == "BIOS说明" || trimmed == "BIOS文件解析" || trimmed == changelog.Date {
			continue
		}
		lines = append(lines, trimmed)
	}
	changelog.Text = strings.Join(lines, "\n")
	return changelog, changelog.Text != ""
}

// detectCPUModel 从 /proc/cpuinfo 识别 N100/N150/N305。
func detectCPUModel(cpuinfo string) string {
	for _, model := range []string{"N305", "N150", "N100"} {
		if strings.Contains(cpuinfo, model) {
			return model
		}
	}
	return ""
}

// biosZipModel 提取压缩包名里的机型段。
func biosZipModel(zipName string) string {
	match := biosZipModelRe.FindStringSubmatch(zipName)
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

// readCurrentBIOSFID 用 flashrom 读出当前 BIOS 区域并解析 $FID。
func (m *Manager) readCurrentBIOSFID(ctx context.Context) (biosFID, error) {
	fid, flashErr := m.readCurrentBIOSFIDViaFlashrom(ctx)
	if flashErr == nil {
		return fid, nil
	}
	// flashrom 不可用(群晖 DSM 自带 libpci 过旧,厂商 flashrom 报 LIBPCI_3.8
	// not found)时退而求其次:dmidecode 读 AMI 主版本+发布日期,精度降级
	if dmi, ok := m.readBIOSVersionViaDMI(ctx); ok {
		dmi.degraded = true
		return dmi, nil
	}
	return biosFID{}, flashErr
}

var dmiBiosDateRe = regexp.MustCompile(`(?m)^\s*Release Date:\s*(\d{2})/(\d{2})/(\d{4})`)

// readBIOSVersionViaDMI 经 dmidecode 读 BIOS 主版本(真机输出 "5.27",对应
// TAD 四段版本的前两组 05.27)与发布日期(MM/DD/YYYY)。读不到日期只给版本。
func (m *Manager) readBIOSVersionViaDMI(ctx context.Context) (biosFID, bool) {
	version, err := m.runFirmwareCommand(ctx, "", "dmidecode", []string{"-s", "bios-version"})
	version = strings.TrimSpace(version)
	if err != nil || version == "" || strings.Contains(version, "#") {
		return biosFID{}, false
	}
	fid := biosFID{Version: version}
	if out, dateErr := m.runFirmwareCommand(ctx, "", "dmidecode", []string{"-t", "bios"}); dateErr == nil {
		if match := dmiBiosDateRe.FindStringSubmatch(out); match != nil {
			month, _ := strconv.Atoi(match[1])
			day, _ := strconv.Atoi(match[2])
			year, _ := strconv.Atoi(match[3])
			if year > 2000 && month >= 1 && month <= 12 && day >= 1 && day <= 31 {
				fid.BuildDate = time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
				fid.BuildText = fmt.Sprintf("%04d-%02d-%02d", year, month, day)
			}
		}
	}
	return fid, true
}

func (m *Manager) readCurrentBIOSFIDViaFlashrom(ctx context.Context) (biosFID, error) {
	var data []byte
	if biosFIDReadHook != nil {
		var err error
		data, err = biosFIDReadHook(ctx, m)
		if err != nil {
			return biosFID{}, err
		}
	} else {
		// 系统没装 flashrom 时自动从服务器下载（读取当前版本也需要 dump BIOS 区域）。
		tool, err := m.ensureFlashrom(ctx)
		if err != nil {
			return biosFID{}, err
		}
		dir := m.firmwareDataDir()
		if dir == "" {
			return biosFID{}, errors.New("模块数据目录不可用")
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return biosFID{}, err
		}
		dump := filepath.Join(dir, "bios-current.tmp")
		defer func() { _ = os.Remove(dump) }()
		output, err := m.runFirmwareCommand(ctx, dir, tool, []string{"-p", "internal", "--ifd", "-i", "bios", "-r", dump})
		if err != nil {
			return biosFID{}, fmt.Errorf("flashrom 读取失败: %v %s", err, trimCommandOutput(output))
		}
		data, err = os.ReadFile(dump)
		if err != nil {
			return biosFID{}, fmt.Errorf("读取 BIOS 备份失败: %w", err)
		}
	}
	fid, ok := parseBIOSFID(data)
	if !ok {
		return biosFID{}, errors.New("当前 BIOS 中未找到 $FID 标识")
	}
	return fid, nil
}

func (m *Manager) cpuModel() string {
	data, err := os.ReadFile(m.rooted("/proc/cpuinfo"))
	if err != nil {
		return ""
	}
	return detectCPUModel(string(data))
}

// fetchBIOSIndex 拉取索引并对瞬时抖动重试一次:索引文件只有几 KB,厂商 CDN
// 冷路径握手慢 + NAS 本地 DNS 忽好忽坏(真机实测),单次失败立即重试比让用户
// 手动再点一次体验好得多;重试间隔 1 秒,上下文取消即时返回。
func fetchBIOSIndex(ctx context.Context) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		data, err := downloadFirmwareFile(ctx, biosIndexURL(), biosIndexMaxBytes)
		if err == nil {
			return data, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, lastErr
}

// StartBIOSOnlineCheck 后台启动一次在线检查并立即返回:POST 端点不再同步
// 等整个检查跑完(连接断开/页面刷新也不会取消检查),进度经缓存快照由
// GET 轮询渐进可见。已在跑时返回 false,不叠加并发检查。
func (m *Manager) StartBIOSOnlineCheck() bool {
	m.firmwareMu.Lock()
	if m.biosOnlineRunning {
		m.firmwareMu.Unlock()
		return false
	}
	m.biosOnlineRunning = true
	// 同步发布"检查中"快照:POST 的响应体本身就是 checking 状态,
	// 前端拿到即可进入轮询节奏,不依赖与后台 goroutine 的时序
	status := BIOSOnlineStatus{CheckedAt: time.Now(), Model: m.cpuModel(), Checking: true}
	m.biosOnline = biosOnlineCache{status: status}
	m.firmwareMu.Unlock()
	go func() {
		defer func() {
			m.firmwareMu.Lock()
			m.biosOnlineRunning = false
			m.firmwareMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		m.CheckBIOSOnline(ctx)
	}()
	return true
}

// BIOSOnline 返回缓存的在线检查结果（不触发网络/刷写工具）。
func (m *Manager) BIOSOnline() BIOSOnlineStatus {
	m.firmwareMu.Lock()
	defer m.firmwareMu.Unlock()
	return m.biosOnline.status
}

// CheckBIOSOnline 拉取 zip.txt 索引、读取当前 flash 的 $FID，判定是否有可升级版本。
func (m *Manager) CheckBIOSOnline(ctx context.Context) BIOSOnlineStatus {
	status := BIOSOnlineStatus{CheckedAt: time.Now(), Model: m.cpuModel(), Checking: true}
	cache := biosOnlineCache{status: status}
	// 每个阶段完成即发布快照:GET /api/firmware/bios-online 立即可见,
	// 前端轮询渐进渲染,不必等整个检查跑完。
	// 注意必须在这里同步 cache.status = status:cache 捕获的是创建时的
	// 结构体副本,调用方改完 status 直接 publish 才不会把旧快照发出去
	publish := func() {
		cache.status = status
		m.firmwareMu.Lock()
		m.biosOnline = cache
		m.firmwareMu.Unlock()
	}
	publish()

	// 当前版本(本地:flashrom/dmidecode)与在线索引(CDN)两路并行取,主循环
	// select 谁先回来就先应用并发布谁——DSM 上 CDN 慢(先见本机型号/当前版本),
	// fnOS 上 flashrom dump 可能更慢(先见候选版本),两种环境页面都不空等。
	type biosFIDResult struct {
		fid biosFID
		err error
	}
	type biosIndexResult struct {
		data []byte
		err  error
	}
	fidCh := make(chan biosFIDResult, 1)
	idxCh := make(chan biosIndexResult, 1)
	go func() {
		fid, err := m.readCurrentBIOSFID(ctx)
		fidCh <- biosFIDResult{fid: fid, err: err}
	}()
	go func() {
		data, err := fetchBIOSIndex(ctx)
		idxCh <- biosIndexResult{data: data, err: err}
	}()

	var fid biosFID
	var fidErr error
	haveFID := false
	applyFID := func(res biosFIDResult) {
		fid, fidErr, haveFID = res.fid, res.err, true
		if fidErr != nil {
			status.CurrentError = fidErr.Error()
		} else {
			status.CurrentBoardID = fid.BoardID
			status.CurrentVersion = fid.Version
			status.CurrentBuildDate = fid.BuildText
			if fid.degraded {
				status.CurrentVersionNote = "当前环境无法运行 flashrom，仅能读取 AMI 主版本，精确小版本不可用"
			}
		}
		publish()
	}

	// 索引结果一到就匹配候选并发布(机型匹配只依赖 Model,不必等 FID)
	var candidate biosIndexRecord
	haveCandidate := false
	var indexErr error
	applyIndex := func(res biosIndexResult) {
		if res.err != nil {
			indexErr = res.err
			return
		}
		records, changelogs := parseBIOSIndex(string(res.data))
		if len(records) == 0 {
			indexErr = errors.New("BIOS 索引中没有可用的固件记录")
			return
		}
		// 严格同机型匹配：N150 与 N100/N305 互刷会砖，N100 与 N305 互刷也有问题。
		found := false
		for _, record := range records {
			if status.Model == "" || biosZipModel(record.ZipName) != status.Model {
				continue
			}
			if !found || record.BuildDate.After(candidate.BuildDate) {
				candidate, found = record, true
			}
		}
		if !found {
			if status.Model == "" {
				indexErr = errors.New("无法识别本机 CPU 型号（需要 N100/N150/N305），已停止在线判定")
			} else {
				indexErr = fmt.Errorf("索引中没有匹配本机 %s 的固件包", status.Model)
			}
			return
		}
		status.CandidateZip = candidate.ZipName
		status.CandidateVersion = candidate.Version
		status.CandidateBuildDate = candidate.BuildText
		status.DownloadURL = biosZipURL(candidate.ZipName)
		// 更新日志只保留日期与压缩包文件名日期一致的段落。
		if dateMatch := biosZipDateRe.FindStringSubmatch(candidate.ZipName); len(dateMatch) >= 2 {
			for _, changelog := range changelogs {
				if changelog.Date == dateMatch[1] {
					status.Changelog = changelog.Text
					break
				}
			}
		}
		haveCandidate = true
		cache.candidate = candidate
		cache.hasCandi = true
		publish()
	}

	haveIndex := false
	for !haveFID || !haveIndex {
		select {
		case res := <-fidCh:
			applyFID(res)
		case res := <-idxCh:
			applyIndex(res)
			haveIndex = true
			if indexErr == nil && !haveCandidate {
				// applyIndex 内部已 publish 候选;走到这里说明无候选,
				// 把索引侧的错误状态也亮出来
				publish()
			}
		case <-ctx.Done():
			// 取消时不再等待两路收尾,直接以当前已发布内容收束
			if !haveFID {
				applyFID(biosFIDResult{err: ctx.Err()})
			}
			if !haveIndex {
				indexErr = ctx.Err()
			}
			haveFID, haveIndex = true, true
		}
	}

	status.Checking = false
	if indexErr != nil {
		status.Error = fmt.Sprintf("获取 BIOS 在线信息失败: %v", indexErr)
		publish()
		return status
	}
	candidateDate := candidate.BuildDate
	if fid.degraded {
		// DMI 只有日期精度；同一天的候选不能仅因带时分就被判为更新。
		year, month, day := candidateDate.Date()
		candidateDate = time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	}
	if !fid.BuildDate.IsZero() && candidateDate.After(fid.BuildDate) {
		status.UpdateAvailable = true
	}
	publish()
	return status
}

// StartBIOSOnlineFlash 按最近一次检查选中的候选包执行在线刷写。
func (m *Manager) StartBIOSOnlineFlash() error {
	m.firmwareMu.Lock()
	cache := m.biosOnline
	m.firmwareMu.Unlock()
	if !cache.hasCandi {
		return errBIOSNoCheck
	}
	if !cache.status.UpdateAvailable {
		return errBIOSNoUpdate
	}
	if err := m.beginFirmwareJob(firmwareJobKindBIOS); err != nil {
		return err
	}
	go m.runBIOSOnlineFlash(cache.candidate)
	return nil
}

func (m *Manager) runBIOSOnlineFlash(candidate biosIndexRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	m.firmwareSetStep("下载 BIOS 压缩包", 5)
	address := biosZipURL(candidate.ZipName)
	m.firmwareLog("下载: %s", address)
	zipData, err := downloadFirmwareFile(ctx, address, firmwareDownloadLimit)
	if err != nil {
		m.firmwareFinish(fmt.Errorf("下载 BIOS 压缩包失败: %w", err), "")
		return
	}
	m.firmwareLog("压缩包: %s（%d 字节）", candidate.ZipName, len(zipData))

	m.firmwareSetStep("解包并校验固件", 15)
	image, err := extractBIOSFromZip(zipData, candidate.File)
	if err != nil {
		m.firmwareFinish(err, "")
		return
	}
	if sum := sha256Hex(image); !strings.EqualFold(sum, candidate.SHA256) {
		m.firmwareFinish(fmt.Errorf("固件 SHA256 校验失败：期望 %s，实际 %s（可能下载损坏，已停止）", candidate.SHA256, sum), "")
		return
	}
	m.firmwareLog("固件: %s（%d 字节，SHA256 校验通过）", candidate.File, len(image))

	// 双保险：解出来的 bin 还要过一道机型校验，防止索引被改后跨机型刷写。
	if model := m.cpuModel(); model == "" || biosZipModel(candidate.ZipName) != model {
		m.firmwareFinish(fmt.Errorf("%w：本机 %s，固件包 %s", errBIOSModelConf, orDash(model), candidate.ZipName), "")
		return
	}
	if fid, ok := parseBIOSFID(image); ok {
		m.firmwareLog("目标固件 $FID: %s / %s / 构建 %s", fid.BoardID, fid.Version, fid.BuildText)
	}

	// 复用本地上传的刷写流程：备份当前 BIOS → flashrom/FPT 写入。
	m.runBIOSFlash(image)
}

// extractBIOSFromZip 从压缩包中解出指定 bin 文件。
func extractBIOSFromZip(data []byte, wantFile string) ([]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("BIOS 压缩包无法解压: %w", err)
	}
	for _, entry := range reader.File {
		if filepath.Base(entry.Name) != wantFile {
			continue
		}
		if entry.UncompressedSize64 > firmwareImageMaxBytes {
			return nil, fmt.Errorf("固件 %s 超过 64 MB 上限", wantFile)
		}
		file, err := entry.Open()
		if err != nil {
			return nil, fmt.Errorf("解压 %s 失败: %w", wantFile, err)
		}
		defer func() { _ = file.Close() }()
		image := make([]byte, entry.UncompressedSize64)
		if _, err := io.ReadFull(file, image); err != nil {
			return nil, fmt.Errorf("读取 %s 失败: %w", wantFile, err)
		}
		return image, nil
	}
	return nil, fmt.Errorf("压缩包内未找到固件 %s", wantFile)
}
