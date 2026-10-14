package powerguard

// backup_auto.go：「自动备份」——按键触发后自动扫描**被打标**的可移动卷并执行备份。
//
// 为什么需要它：普通备份任务把目标写死成某一块盘（卷标/UUID），插上另一块盘就不认。
// 打标之后，同一条规则可以服务任意多块盘——给每块 U 盘 / 移动硬盘**打标**，按键一按就把
// 当前插着的所有打标盘挨个处理一遍。
//
// 打标写在卷内的统一元数据目录 .tad-backup/ 里（mark.json，与源介质标识共用这一个
// 目录），包含：
//
//	alias  别名，列表里显示成友好名字（如「相机SD卡」）
//	subs   已打标的卷内子目录，空 = 整块卷
//
// 子目录打标：给「DCIM」「VIDEO」各打一次标，这块盘就只操作这两个子目录的内容
// （每个子目录作为独立的 rsync 源、保留目录名，落到 NAS 上就是 目标/DCIM/… 与
// 目标/VIDEO/…）。选中卷根打标 = 整卷参与。
//
// 两个方向：
//
//	to_volume   NAS 目录 → 打标卷：把 NAS 上选定的目录备份进打标盘。
//	            移动类模式（复制完删源）需要规则里显式确认（AllowMoveToVolume）：
//	            源是 NAS 上的原始资料，删掉不可逆，而这条规则又可能被一次按键
//	            作用到多块盘上——所以「每次只允许一块打标卷在线」也在这里强制。
//	from_volume 打标卷 → NAS 目录：把打标盘里的资料导入 NAS；配合
//	              move_archive（按日期转移归档）可在导入后清空 U 盘
//
// 打标只表示「这块盘（或盘里的这几个子目录）参与自动备份」，不做内容校验；打标、
// 子目录打标、取消打标都在界面上完成。设备拔掉后 NAS 侧仍保留记录，列表里能看到
// 「离线」的盘并支持离线取消打标——记录会在设备下次上线时自动清除。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// GPIOActionAutoBackupPrefix 自动备份的按键动作前缀：auto_backup:<规则ID>。
	GPIOActionAutoBackupPrefix = "auto_backup:"

	// BackupAutoMarkerDirName 打标目录：与源介质标识共用同一个元数据目录
	// （BackupMetaDirName = .tad-backup），卷根只多这一个隐藏目录。
	BackupAutoMarkerDirName = BackupMetaDirName
	// BackupAutoMarkerFileName 打标目录内的标记文件（JSON：别名 / 打标时间 / 已打标子目录）。
	BackupAutoMarkerFileName = "mark.json"
	// BackupAutoMarkerLegacyDirName 旧版打标目录（v0.10.30–v0.10.34）：里面放 mark.json。
	// 读到它按已打标处理，下次写入时收拾干净。
	BackupAutoMarkerLegacyDirName = ".tad-auto-backup"
	// BackupAutoMarkerLegacyName 更旧的打标文件（v0.10.29 及以前）：与打标目录同名、
	// 但写在卷根的普通文件。读到它按「整块卷已打标」处理。
	BackupAutoMarkerLegacyName = ".tad-auto-backup"

	// BackupDirectionToVolume NAS 目录 → 打标卷。移动类模式要求规则里显式确认
	// （GPIOAutoBackup.AllowMoveToVolume），并且运行时只允许一块打标卷在线。
	BackupDirectionToVolume = "to_volume"
	// BackupDirectionFromVolume 打标卷 → NAS 目录：移动类模式的常规用法（导入后清空卡）。
	BackupDirectionFromVolume = "from_volume"

	autoBackupMaxCount = 32
	// autoBackupSubMaxCount 单块打标卷最多标记多少个子目录。
	autoBackupSubMaxCount = 32
	// autoBackupVolumeMaxCount NAS 侧最多保留多少条打标卷记录。
	autoBackupVolumeMaxCount = 64
	// autoBackupAliasMaxLen 打标卷别名的最大字符数。
	autoBackupAliasMaxLen = 32
)

// validateGPIOAutoVolumeRecords 校验 NAS 侧保存的打标卷记录。
func validateGPIOAutoVolumeRecords(records []AutoBackupVolumeRecord) error {
	if len(records) > autoBackupVolumeMaxCount {
		return fmt.Errorf("打标卷记录不能超过 %d 条", autoBackupVolumeMaxCount)
	}
	identities := make(map[string]bool, len(records))
	for _, record := range records {
		identity := strings.TrimSpace(record.Identity)
		if identity == "" {
			return errors.New("打标卷记录的设备标识不能为空")
		}
		if identities[identity] {
			return fmt.Errorf("打标卷记录 %q 重复", identity)
		}
		identities[identity] = true
		if utf8.RuneCountInString(record.Alias) > autoBackupAliasMaxLen {
			return fmt.Errorf("打标卷记录 %q 的别名不能超过 %d 个字符", identity, autoBackupAliasMaxLen)
		}
		if len(record.Subs) > autoBackupSubMaxCount {
			return fmt.Errorf("打标卷记录 %q 的子目录不能超过 %d 个", identity, autoBackupSubMaxCount)
		}
		for _, sub := range record.Subs {
			if err := validateAutoBackupSub(sub); err != nil {
				return fmt.Errorf("打标卷记录 %q 的子目录无效：%v", identity, err)
			}
		}
	}
	return nil
}

// GPIOAutoBackup 一条自动备份规则，可被按键动作 auto_backup:<id> 引用。
// Fixed 是 NAS 侧的固定位置：to_volume 时是源，from_volume 时是目标。
type GPIOAutoBackup struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Direction string         `json:"direction"`
	Fixed     BackupLocation `json:"fixed"`
	// VolumeSub 打标卷内的子目录，空 = 卷根。to_volume 时是目标子目录，
	// from_volume 时是源子目录。
	VolumeSub string `json:"volume_sub,omitempty"`
	Mode      string `json:"mode"`
	Overwrite bool   `json:"overwrite"`
	Keep      int    `json:"keep,omitempty"`
	// AllowMoveToVolume 显式确认「NAS → 打标卷」方向使用移动类模式：这个组合会删除
	// NAS 上的源文件，而规则又可能被一次按键作用到多块打标盘上，风险必须由用户在
	// 保存规则时逐条确认（按键触发时没有界面可以弹窗）。未勾选时后端直接拒绝。
	AllowMoveToVolume bool `json:"allow_move_to_volume,omitempty"`
}

// AutoBackupVolume 一块当前插着的可移动卷（无论打没打标）。
type AutoBackupVolume struct {
	Device     string `json:"device"`
	MountPoint string `json:"mount_point"`
	Label      string `json:"label,omitempty"`
	UUID       string `json:"uuid,omitempty"`
	ReadOnly   bool   `json:"read_only"`
	// Alias 打标时设置的别名；Subs 已打标的卷内子目录（空 = 整块卷）；
	// Marked 是否已打标；Legacy 是否为旧版卷根标识文件（需在下次写入时升级）。
	Alias    string   `json:"alias,omitempty"`
	Subs     []string `json:"subs,omitempty"`
	Marked   bool     `json:"marked"`
	Legacy   bool     `json:"legacy,omitempty"`
	MarkedAt string   `json:"marked_at,omitempty"`
}

// identity 卷的稳定标识：换接口、换挂载路径后仍能认出同一块盘。
// 优先级 UUID > 卷标 > 设备名。
func (volume AutoBackupVolume) identity() string {
	for _, candidate := range []string{volume.UUID, volume.Label, volume.Device} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func (volume AutoBackupVolume) displayName() string {
	name := strings.TrimSpace(volume.Alias)
	if name == "" {
		name = strings.TrimSpace(volume.Label)
	}
	if name == "" {
		name = strings.TrimSpace(volume.UUID)
	}
	if name == "" {
		name = volume.Device
	}
	return fmt.Sprintf("%s（%s）", name, volume.MountPoint)
}

// AutoBackupMark 打标标记的内容（卷内 .tad-backup/mark.json）。
type AutoBackupMark struct {
	Alias    string   `json:"alias,omitempty"`
	MarkedAt string   `json:"marked_at,omitempty"`
	Volume   string   `json:"volume,omitempty"`
	Subs     []string `json:"subs,omitempty"` // 已打标的卷内子目录（相对卷根）；空 = 整块卷
}

// AutoBackupVolumeRecord NAS 侧保存的打标记录：设备拔掉之后列表里仍能看到它，
// 并支持「离线取消打标」（下次上线自动清除）。标记本身始终以卷内的
// .tad-backup/mark.json 为准，这里只是它的影子。
type AutoBackupVolumeRecord struct {
	Identity     string   `json:"identity"`
	Alias        string   `json:"alias,omitempty"`
	Subs         []string `json:"subs,omitempty"`
	MarkedAt     string   `json:"marked_at,omitempty"`
	LastDevice   string   `json:"last_device,omitempty"`
	LastLabel    string   `json:"last_label,omitempty"`
	LastMount    string   `json:"last_mount,omitempty"`
	PendingClear bool     `json:"pending_clear,omitempty"` // 离线时取消打标：等设备下次上线再删标记
}

func (record AutoBackupVolumeRecord) displayName() string {
	name := strings.TrimSpace(record.Alias)
	if name == "" {
		name = strings.TrimSpace(record.LastLabel)
	}
	if name == "" {
		name = strings.TrimSpace(record.Identity)
	}
	return name
}

// AutoBackupDevice 打标列表里的一行：在线（当前插着，可能是还没打标的可移动卷）
// 或离线（打过标但当前不在）。
type AutoBackupDevice struct {
	Identity     string   `json:"identity"`
	Alias        string   `json:"alias,omitempty"`
	Subs         []string `json:"subs,omitempty"`
	Marked       bool     `json:"marked"`
	Online       bool     `json:"online"`
	PendingClear bool     `json:"pending_clear,omitempty"`
	Legacy       bool     `json:"legacy,omitempty"`
	ReadOnly     bool     `json:"read_only,omitempty"`
	Removable    bool     `json:"removable"`
	Device       string   `json:"device,omitempty"`
	MountPoint   string   `json:"mount_point,omitempty"`
	Label        string   `json:"label,omitempty"`
	UUID         string   `json:"uuid,omitempty"`
	MarkedAt     string   `json:"marked_at,omitempty"`
}

// AutoBackupResult 一次自动备份的汇总：多块打标卷逐个执行后的结果集合。
type AutoBackupResult struct {
	RuleID     string         `json:"rule_id"`
	RuleName   string         `json:"rule_name"`
	Direction  string         `json:"direction"`
	DryRun     bool           `json:"dry_run,omitempty"`
	Volumes    int            `json:"volumes"`
	Results    []BackupResult `json:"results,omitempty"`
	Files      int64          `json:"files"`
	Bytes      int64          `json:"bytes"`
	Pruned     int            `json:"pruned,omitempty"` // 多卷共用 NAS 目标时统一清理掉的快照数
	StartedAt  string         `json:"started_at,omitempty"`
	FinishedAt string         `json:"finished_at,omitempty"`
}

// ---------------------------------------------------------------------------
// 校验
// ---------------------------------------------------------------------------

func validateGPIOAutoBackups(rules []GPIOAutoBackup) error {
	if len(rules) > autoBackupMaxCount {
		return fmt.Errorf("自动备份规则不能超过 %d 条", autoBackupMaxCount)
	}
	ids := make(map[string]bool, len(rules))
	names := make(map[string]bool, len(rules))
	for _, rule := range rules {
		if err := validateGPIOAutoBackup(rule); err != nil {
			return err
		}
		if ids[rule.ID] {
			return fmt.Errorf("自动备份规则 ID %q 重复", rule.ID)
		}
		key := strings.ToLower(strings.TrimSpace(rule.Name))
		if names[key] {
			return fmt.Errorf("自动备份规则名称 %q 重复", strings.TrimSpace(rule.Name))
		}
		ids[rule.ID] = true
		names[key] = true
	}
	return nil
}

func validateGPIOAutoBackup(rule GPIOAutoBackup) error {
	if !backupJobIDPattern.MatchString(rule.ID) {
		return fmt.Errorf("自动备份规则 ID %q 只能使用 1-%d 位字母、数字、点、下划线或连字符", rule.ID, backupIDMaxLen)
	}
	name := strings.TrimSpace(rule.Name)
	if name == "" || utf8.RuneCountInString(name) > backupJobMaxName {
		return fmt.Errorf("自动备份规则 %q 的名称长度必须为 1-%d 个字符", rule.ID, backupJobMaxName)
	}
	switch rule.Direction {
	case BackupDirectionToVolume, BackupDirectionFromVolume:
	default:
		return fmt.Errorf("自动备份规则 %q 的方向无效", rule.ID)
	}
	switch rule.Mode {
	case BackupModeCopy, BackupModeSync, BackupModeArchive:
	case BackupModeMoveOverwrite, BackupModeMoveArchive:
		// 移动类模式会删除源文件。「打标卷 → NAS」的源在可移动介质上，删源正是
		// 「导入后清空卡」，直接放行；「NAS → 打标卷」的源是 NAS 上的原始资料，
		// 必须由用户在规则里显式确认（前端会弹确认并记下这个勾）才接受。
		if rule.Direction != BackupDirectionFromVolume && !rule.AllowMoveToVolume {
			return fmt.Errorf("自动备份规则 %q 使用移动类模式时会删除 NAS 上的源文件，请在规则里勾选确认后再保存", rule.ID)
		}
	default:
		return fmt.Errorf("自动备份规则 %q 的复制模式无效", rule.ID)
	}
	role := "NAS 侧位置"
	if rule.Direction == BackupDirectionFromVolume {
		role = "NAS 侧目标"
	}
	if err := validateBackupLocation(rule.ID, role, rule.Fixed); err != nil {
		return err
	}
	sub := strings.TrimSpace(rule.VolumeSub)
	if len(sub) > backupLocationMaxLen {
		return fmt.Errorf("自动备份规则 %q 的卷内子目录过长", rule.ID)
	}
	if strings.HasPrefix(sub, "/") {
		return fmt.Errorf("自动备份规则 %q 的卷内子目录必须是相对路径", rule.ID)
	}
	if strings.Contains(sub, "..") {
		return fmt.Errorf("自动备份规则 %q 的卷内子目录不能包含 ..", rule.ID)
	}
	if rule.Keep < 0 || rule.Keep > 1000 {
		return fmt.Errorf("自动备份规则 %q 的归档保留份数必须在 0-1000 之间（0 表示不限）", rule.ID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// action 绑定
// ---------------------------------------------------------------------------

func gpioAutoBackupIDFromAction(action string) (string, bool) {
	if !strings.HasPrefix(action, GPIOActionAutoBackupPrefix) {
		return "", false
	}
	return strings.TrimPrefix(action, GPIOActionAutoBackupPrefix), true
}

func gpioAutoBackupForAction(config GPIOConfig, action string) *GPIOAutoBackup {
	id, ok := gpioAutoBackupIDFromAction(action)
	if !ok {
		return nil
	}
	for _, rule := range config.Autos {
		if rule.ID == id {
			copy := rule
			return &copy
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 打标（卷内 .tad-backup/mark.json）
// ---------------------------------------------------------------------------

// autoBackupMarkerDir 打标目录在卷内的位置（与源标识共用 .tad-backup）。
func autoBackupMarkerDir(mountPoint string) string {
	return filepath.Join(mountPoint, BackupAutoMarkerDirName)
}

// autoBackupMarkerFile 标记文件在卷内的位置。
func autoBackupMarkerFile(mountPoint string) string {
	return filepath.Join(autoBackupMarkerDir(mountPoint), BackupAutoMarkerFileName)
}

// autoBackupLegacyMarkerDir 旧版打标目录（v0.10.30–v0.10.34）的位置。
func autoBackupLegacyMarkerDir(mountPoint string) string {
	return filepath.Join(mountPoint, BackupAutoMarkerLegacyDirName)
}

// autoBackupLegacyMarkerFile 旧版打标目录里的标记文件的位置。
func autoBackupLegacyMarkerFile(mountPoint string) string {
	return filepath.Join(autoBackupLegacyMarkerDir(mountPoint), BackupAutoMarkerFileName)
}

// autoBackupLegacyMarkerName 更旧的打标文件（写在卷根的普通文件）的位置。
func autoBackupLegacyMarkerName(mountPoint string) string {
	return filepath.Join(mountPoint, BackupAutoMarkerLegacyName)
}

// validateAutoBackupSub 校验「卷内子目录」：必须是相对卷根的路径。
func validateAutoBackupSub(sub string) error {
	if strings.TrimSpace(sub) == "" {
		return errors.New("子目录不能为空")
	}
	if utf8.RuneCountInString(sub) > backupLocationMaxLen {
		return fmt.Errorf("子目录路径过长（最多 %d 个字符）", backupLocationMaxLen)
	}
	slash := filepath.ToSlash(sub)
	if strings.HasPrefix(slash, "/") {
		return errors.New("必须是相对卷根的路径")
	}
	if strings.Contains(slash, "..") {
		return errors.New("不能包含 ..")
	}
	return nil
}

// normalizeAutoBackupSubs 清理子目录列表：统一斜杠、去空白、去重、排序。
func normalizeAutoBackupSubs(subs []string) []string {
	out := make([]string, 0, len(subs))
	seen := make(map[string]bool, len(subs))
	for _, sub := range subs {
		slash := filepath.ToSlash(strings.Trim(strings.TrimSpace(sub), "/"))
		if slash == "" || seen[slash] {
			continue
		}
		if err := validateAutoBackupSub(slash); err != nil {
			continue
		}
		seen[slash] = true
		out = append(out, slash)
	}
	sort.Strings(out)
	return out
}

// readAutoBackupMark 读取卷上的打标标记，返回标记内容、是否已打标、是否为旧位置
// （旧位置会在下次写入时收拾干净）。标记文件读不出来（损坏/无权限）时按「已打标、
// 整块卷」处理，避免盘插着却被静默忽略。
func (m *Manager) readAutoBackupMark(volume AutoBackupVolume) (AutoBackupMark, bool, bool) {
	// 1) 当前位置：卷内 .tad-backup/mark.json
	if data, err := os.ReadFile(m.rooted(autoBackupMarkerFile(volume.MountPoint))); err == nil {
		var mark AutoBackupMark
		if json.Unmarshal(data, &mark) != nil {
			return AutoBackupMark{}, true, false
		}
		mark.Subs = normalizeAutoBackupSubs(mark.Subs)
		return mark, true, false
	}
	// 2) 旧版打标目录 .tad-auto-backup/mark.json（v0.10.30–v0.10.34）
	if data, err := os.ReadFile(m.rooted(autoBackupLegacyMarkerFile(volume.MountPoint))); err == nil {
		var mark AutoBackupMark
		if json.Unmarshal(data, &mark) != nil {
			return AutoBackupMark{}, true, true
		}
		mark.Subs = normalizeAutoBackupSubs(mark.Subs)
		return mark, true, true
	}
	// 3) 更旧版：与打标目录同名的卷根普通文件，只能按「整块卷已打标」理解。
	if info, err := os.Stat(m.rooted(autoBackupLegacyMarkerName(volume.MountPoint))); err == nil && !info.IsDir() {
		return AutoBackupMark{}, true, true
	}
	return AutoBackupMark{}, false, false
}

// removeAutoBackupLegacyMarks 收拾旧版的两处打标位置。写成 best-effort：清不掉最多
// 多留一个旧文件，不影响新版打标本身；目录只删自己的 mark.json，空掉后再试探着删目录
// （里面有别的东西时 os.Remove 会失败，正好保住用户的文件）。
func (m *Manager) removeAutoBackupLegacyMarks(mountPoint string) {
	legacyMark := m.rooted(autoBackupLegacyMarkerFile(mountPoint))
	if _, err := os.Stat(legacyMark); err == nil {
		if err := os.Remove(legacyMark); err != nil && !os.IsNotExist(err) {
			m.logfUnlocked("auto backup: 清理旧版打标文件失败 %s: %v", legacyMark, err)
		}
	}
	legacyDir := m.rooted(autoBackupLegacyMarkerDir(mountPoint))
	if info, err := os.Stat(legacyDir); err == nil && info.IsDir() {
		if err := os.Remove(legacyDir); err != nil {
			m.logfUnlocked("auto backup: 旧版打标目录未能删除（可能非空）%s: %v", legacyDir, err)
		}
	}
	legacyName := m.rooted(autoBackupLegacyMarkerName(mountPoint))
	if info, err := os.Stat(legacyName); err == nil && !info.IsDir() {
		if err := os.Remove(legacyName); err != nil && !os.IsNotExist(err) {
			m.logfUnlocked("auto backup: 清理更旧版打标文件失败 %s: %v", legacyName, err)
		}
	}
}

// autoBackupWriteError 把写盘的底层错误翻译成用户看得懂的话：打标失败最常见的
// 原因就是卷只读、没权限、空间不足，原样抛 errno 用户看不懂。
func autoBackupWriteError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrPermission):
		return errors.New("没有写入权限")
	case errors.Is(err, errnoReadOnlyFS):
		return errors.New("该卷以只读方式挂载（read-only）")
	case errors.Is(err, errnoNoSpace):
		return errors.New("该卷空间不足")
	default:
		return err
	}
}

// listAutoBackupVolumes 列出当前插着的可移动卷，并读入它们的打标状态。
func (m *Manager) listAutoBackupVolumes() []AutoBackupVolume {
	var volumes []AutoBackupVolume
	for _, volume := range m.ListBackupVolumes() {
		if !volume.Removable {
			continue
		}
		entry := AutoBackupVolume{
			Device: volume.Device, MountPoint: volume.MountPoint,
			Label: volume.Label, UUID: volume.UUID, ReadOnly: volume.ReadOnly,
		}
		if mark, marked, legacy := m.readAutoBackupMark(entry); marked {
			entry.Marked, entry.Legacy = true, legacy
			entry.Alias, entry.Subs, entry.MarkedAt = mark.Alias, mark.Subs, mark.MarkedAt
		}
		volumes = append(volumes, entry)
	}
	sort.SliceStable(volumes, func(i, j int) bool { return volumes[i].MountPoint < volumes[j].MountPoint })
	return volumes
}

// markedAutoBackupVolumes 当前插着、且已打标的可移动卷（执行自动备份时用）。
func (m *Manager) markedAutoBackupVolumes() []AutoBackupVolume {
	var marked []AutoBackupVolume
	for _, volume := range m.listAutoBackupVolumes() {
		if volume.Marked {
			marked = append(marked, volume)
		}
	}
	return marked
}

// autoBackupVolumeForPath 从用户选的目录定位它所在的可移动卷。
func (m *Manager) autoBackupVolumeForPath(path string) (AutoBackupVolume, error) {
	clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	if clean == "" || !strings.HasPrefix(clean, "/") {
		return AutoBackupVolume{}, errors.New("请先选择要打标的目录")
	}
	var best AutoBackupVolume
	found := false
	bestLen := -1
	for _, volume := range m.listAutoBackupVolumes() {
		mount := filepath.ToSlash(filepath.Clean(volume.MountPoint))
		if clean != mount && !strings.HasPrefix(clean, mount+"/") {
			continue
		}
		if len(mount) > bestLen {
			best, bestLen, found = volume, len(mount), true
		}
	}
	if !found {
		return AutoBackupVolume{}, errors.New("所选目录不在可移动存储上：自动备份只支持给 U 盘 / 移动硬盘打标")
	}
	return best, nil
}

// autoBackupSubRelative 把用户选的目录换算成「相对卷根的子目录」；选的正是卷根时
// 返回空串。只做路径合法性判断，**不要求目录当前存在**。
//
// 取消打标必须用它：用户把 DCIM 删掉或改名之后，界面上那个「×」不能一直报
// 「卷内目录不存在」，否则标记永远取消不掉，每次自动备份还会因为附加源不存在而失败。
func (m *Manager) autoBackupSubRelative(volume AutoBackupVolume, path string) (string, error) {
	clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	mount := filepath.ToSlash(filepath.Clean(volume.MountPoint))
	if clean == mount {
		return "", nil
	}
	if !strings.HasPrefix(clean, mount+"/") {
		return "", fmt.Errorf("所选目录不在这块卷上：%s", volume.displayName())
	}
	rel := strings.TrimPrefix(clean, mount+"/")
	if err := validateAutoBackupSub(rel); err != nil {
		return "", fmt.Errorf("卷内子目录无效：%v", err)
	}
	return rel, nil
}

// autoBackupSubForPath 打标时用：在 autoBackupSubRelative 之上再要求目录真的存在，
// 免得把「已经不存在的目录」写进标记里。
func (m *Manager) autoBackupSubForPath(volume AutoBackupVolume, path string) (string, error) {
	rel, err := m.autoBackupSubRelative(volume, path)
	if err != nil || rel == "" {
		return rel, err
	}
	mount := filepath.ToSlash(filepath.Clean(volume.MountPoint))
	info, statErr := os.Stat(m.rooted(filepath.Join(mount, rel)))
	if statErr != nil || !info.IsDir() {
		return "", fmt.Errorf("卷内目录不存在：%s", rel)
	}
	return rel, nil
}

// writeAutoBackupMark 把标记写进卷内打标目录。旧版的两处位置（.tad-auto-backup 目录
// 与卷根同名文件）会被顺手清掉，不让同一块盘上并存两份标记。
func (m *Manager) writeAutoBackupMark(volume AutoBackupVolume, mark AutoBackupMark) error {
	if volume.ReadOnly {
		return fmt.Errorf("%s 是只读挂载（read-only），无法打标", volume.displayName())
	}
	if err := os.MkdirAll(m.rooted(autoBackupMarkerDir(volume.MountPoint)), 0o755); err != nil {
		return fmt.Errorf("创建打标目录失败：%v", autoBackupWriteError(err))
	}
	mark.Subs = normalizeAutoBackupSubs(mark.Subs)
	mark.Volume = volume.displayName()
	data, err := json.MarshalIndent(mark, "", "  ")
	if err != nil {
		return fmt.Errorf("生成打标内容失败：%v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(m.rooted(autoBackupMarkerFile(volume.MountPoint)), data, 0o644); err != nil {
		return fmt.Errorf("写入打标文件失败：%v", autoBackupWriteError(err))
	}
	m.removeAutoBackupLegacyMarks(volume.MountPoint)
	return nil
}

// normalizeAutoBackupAlias 校验并清理别名。
func normalizeAutoBackupAlias(alias string) (string, error) {
	trimmed := strings.TrimSpace(alias)
	if utf8.RuneCountInString(trimmed) > autoBackupAliasMaxLen {
		return "", fmt.Errorf("别名最多 %d 个字符", autoBackupAliasMaxLen)
	}
	if strings.ContainsAny(trimmed, "\n\r\t") {
		return "", errors.New("别名不能包含换行或制表符")
	}
	return trimmed, nil
}

// MarkAutoBackupVolume 整卷打标：清空子目录列表（= 整块卷参与），可同时设置别名。
func (m *Manager) MarkAutoBackupVolume(path, alias string) (AutoBackupVolume, error) {
	volume, err := m.autoBackupVolumeForPath(path)
	if err != nil {
		return volume, err
	}
	cleanAlias, err := normalizeAutoBackupAlias(alias)
	if err != nil {
		return volume, err
	}
	mark := AutoBackupMark{Alias: cleanAlias, MarkedAt: time.Now().Format(time.RFC3339)}
	if err := m.writeAutoBackupMark(volume, mark); err != nil {
		return volume, err
	}
	m.rememberAutoBackupVolume(volume, mark)
	volume.Marked, volume.Legacy = true, false
	volume.Alias, volume.Subs, volume.MarkedAt = cleanAlias, nil, mark.MarkedAt
	return volume, nil
}

// AddAutoBackupSub 给卷内某个子目录打标：它成为这块盘参与备份的操作范围，
// 可以打多个（逐个累积）。选中卷根时提示改用整卷打标。
func (m *Manager) AddAutoBackupSub(path string) (AutoBackupVolume, string, error) {
	volume, err := m.autoBackupVolumeForPath(path)
	if err != nil {
		return volume, "", err
	}
	sub, err := m.autoBackupSubForPath(volume, path)
	if err != nil {
		return volume, "", err
	}
	if sub == "" {
		return volume, "", errors.New("请选择卷内的子目录；要对整块卷打标请用「给整块卷打标」")
	}
	mark, marked, _ := m.readAutoBackupMark(volume)
	if !marked {
		mark = AutoBackupMark{}
	}
	if !gpioContainsString(mark.Subs, sub) && len(mark.Subs) >= autoBackupSubMaxCount {
		return volume, sub, fmt.Errorf("一块盘最多打标 %d 个子目录", autoBackupSubMaxCount)
	}
	mark.Subs = normalizeAutoBackupSubs(append(mark.Subs, sub))
	mark.MarkedAt = time.Now().Format(time.RFC3339)
	if err := m.writeAutoBackupMark(volume, mark); err != nil {
		return volume, sub, err
	}
	m.rememberAutoBackupVolume(volume, mark)
	return volume, sub, nil
}

// RemoveAutoBackupSub 取消某个子目录的打标；全部子目录都取消后回到「整块卷」。
// 只按路径算相对子目录，不要求目录还在——目录被删掉之后也得能取消。
func (m *Manager) RemoveAutoBackupSub(path string) (AutoBackupVolume, string, error) {
	volume, err := m.autoBackupVolumeForPath(path)
	if err != nil {
		return volume, "", err
	}
	sub, err := m.autoBackupSubRelative(volume, path)
	if err != nil {
		return volume, "", err
	}
	if sub == "" {
		return volume, "", errors.New("请选择要取消打标的卷内子目录")
	}
	mark, marked, _ := m.readAutoBackupMark(volume)
	if !marked {
		return volume, sub, fmt.Errorf("%s 还没有打标", volume.displayName())
	}
	kept := make([]string, 0, len(mark.Subs))
	for _, item := range mark.Subs {
		if item != sub {
			kept = append(kept, item)
		}
	}
	mark.Subs = kept
	mark.MarkedAt = time.Now().Format(time.RFC3339)
	if err := m.writeAutoBackupMark(volume, mark); err != nil {
		return volume, sub, err
	}
	m.rememberAutoBackupVolume(volume, mark)
	return volume, sub, nil
}

// SetAutoBackupAlias 给一块已打标的卷改别名（在线的按路径定位）。
func (m *Manager) SetAutoBackupAlias(path, alias string) (AutoBackupVolume, error) {
	volume, err := m.autoBackupVolumeForPath(path)
	if err != nil {
		return volume, err
	}
	cleanAlias, err := normalizeAutoBackupAlias(alias)
	if err != nil {
		return volume, err
	}
	mark, marked, _ := m.readAutoBackupMark(volume)
	if !marked {
		return volume, fmt.Errorf("%s 还没有打标，无法设置别名", volume.displayName())
	}
	mark.Alias = cleanAlias
	mark.MarkedAt = time.Now().Format(time.RFC3339)
	if err := m.writeAutoBackupMark(volume, mark); err != nil {
		return volume, err
	}
	m.rememberAutoBackupVolume(volume, mark)
	volume.Alias = cleanAlias
	return volume, nil
}

// clearAutoBackupVolumeMark 删掉卷上的打标标记（新版 mark.json + 旧版两处位置）并移除
// NAS 侧记录。只删自己的 mark.json，不碰 .tad-backup 目录里的其它文件（源介质标识共用
// 这个目录），空掉后试探着删目录。
func (m *Manager) clearAutoBackupVolumeMark(volume AutoBackupVolume) error {
	mark := m.rooted(autoBackupMarkerFile(volume.MountPoint))
	if err := os.Remove(mark); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除打标文件失败：%v", autoBackupWriteError(err))
	}
	m.removeAutoBackupLegacyMarks(volume.MountPoint)
	// 目录里只剩空壳时顺手删掉；还留着源介质标识就原样保住。
	if err := os.Remove(m.rooted(autoBackupMarkerDir(volume.MountPoint))); err != nil && !os.IsNotExist(err) {
		m.logfUnlocked("auto backup: 打标目录未删除（可能非空）%s: %v", m.rooted(autoBackupMarkerDir(volume.MountPoint)), err)
	}
	// 记录写不进去不该让取消打标失败：卷上的标记已经删掉了，记录只是它的影子。
	if err := m.forgetAutoBackupRecord(volume.identity()); err != nil {
		m.logfUnlocked("auto backup: 移除打标记录失败 %s: %v", volume.identity(), err)
	}
	return nil
}

// ClearAutoBackupMark 取消打标（按目录定位所在的可移动卷）。
func (m *Manager) ClearAutoBackupMark(path string) (AutoBackupVolume, error) {
	volume, err := m.autoBackupVolumeForPath(path)
	if err != nil {
		return volume, err
	}
	if err := m.clearAutoBackupVolumeMark(volume); err != nil {
		return volume, err
	}
	volume.Marked = false
	return volume, nil
}

// ForgetAutoBackupVolume 离线取消打标：设备没插着，先在记录上挂起，等它下次
// 上线时自动删掉标记；设备当前在线则直接清除。
func (m *Manager) ForgetAutoBackupVolume(identity string) error {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return errors.New("缺少设备标识")
	}
	for _, volume := range m.listAutoBackupVolumes() {
		if volume.identity() == identity {
			return m.clearAutoBackupVolumeMark(volume)
		}
	}
	return m.mutateAutoBackupRecords(func(records []AutoBackupVolumeRecord) ([]AutoBackupVolumeRecord, bool) {
		for index := range records {
			if records[index].Identity != identity {
				continue
			}
			if records[index].PendingClear {
				return records, false
			}
			records[index].PendingClear = true
			return records, true
		}
		return records, false
	})
}

// CancelPendingAutoBackupClear 撤销「离线取消打标」的预约（用户改主意了）。
func (m *Manager) CancelPendingAutoBackupClear(identity string) error {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return errors.New("缺少设备标识")
	}
	return m.mutateAutoBackupRecords(func(records []AutoBackupVolumeRecord) ([]AutoBackupVolumeRecord, bool) {
		for index := range records {
			if records[index].Identity != identity {
				continue
			}
			if !records[index].PendingClear {
				return records, false
			}
			records[index].PendingClear = false
			return records, true
		}
		return records, false
	})
}

// ProcessAutoBackupClears 执行挂起的「离线取消打标」：设备重新插上后删掉标记并
// 移除记录。返回本次清理的设备数。
func (m *Manager) ProcessAutoBackupClears() (int, error) {
	records, err := m.autoBackupRecords()
	if err != nil {
		return 0, err
	}
	pending := make(map[string]bool)
	for _, record := range records {
		if record.PendingClear {
			pending[record.Identity] = true
		}
	}
	if len(pending) == 0 {
		return 0, nil
	}
	cleared := 0
	var failures []string
	for _, volume := range m.listAutoBackupVolumes() {
		if !pending[volume.identity()] {
			continue
		}
		if err := m.clearAutoBackupVolumeMark(volume); err != nil {
			failures = append(failures, err.Error())
			continue
		}
		cleared++
	}
	if len(failures) > 0 {
		return cleared, errors.New(strings.Join(failures, "；"))
	}
	return cleared, nil
}

// autoBackupRecords 读取 NAS 侧保存的打标记录。
func (m *Manager) autoBackupRecords() ([]AutoBackupVolumeRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.loadConfigLocked()
	if err != nil {
		return nil, err
	}
	return append([]AutoBackupVolumeRecord(nil), cfg.GPIO.AutoVolumes...), nil
}

// mutateAutoBackupRecords 在 m.mu 保护下改写打标记录并落盘。
func (m *Manager) mutateAutoBackupRecords(fn func(records []AutoBackupVolumeRecord) ([]AutoBackupVolumeRecord, bool)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.loadConfigLocked()
	if err != nil {
		m.lastError = err.Error()
		return err
	}
	next, changed := fn(append([]AutoBackupVolumeRecord(nil), cfg.GPIO.AutoVolumes...))
	if !changed {
		return nil
	}
	cfg.GPIO.AutoVolumes = next
	if err := validateGPIOConfig(cfg.GPIO); err != nil {
		m.lastError = err.Error()
		return err
	}
	if err := m.saveConfigLocked(cfg); err != nil {
		m.lastError = err.Error()
		return err
	}
	m.lastError = ""
	return nil
}

// rememberAutoBackupVolume 把一块卷的打标状态记进 NAS 侧记录（离线列表用）。
// 记录只是卷上标记的影子：写不进记录不应该让打标本身失败，所以只记日志。
func (m *Manager) rememberAutoBackupVolume(volume AutoBackupVolume, mark AutoBackupMark) {
	if err := m.saveAutoBackupRecord(volume, mark); err != nil {
		m.logfUnlocked("auto backup: 保存打标记录失败 %s: %v", volume.MountPoint, err)
	}
}

// logfUnlocked 在 m.mu 之外记一条运行日志（logf 要求调用方持锁）。
func (m *Manager) logfUnlocked(format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logf(format, args...)
}

func (m *Manager) saveAutoBackupRecord(volume AutoBackupVolume, mark AutoBackupMark) error {
	identity := volume.identity()
	if identity == "" {
		return errors.New("无法识别该设备（缺少 UUID、卷标与设备名）")
	}
	return m.mutateAutoBackupRecords(func(records []AutoBackupVolumeRecord) ([]AutoBackupVolumeRecord, bool) {
		next := AutoBackupVolumeRecord{
			Identity: identity, Alias: strings.TrimSpace(mark.Alias),
			Subs: normalizeAutoBackupSubs(mark.Subs), MarkedAt: mark.MarkedAt,
			LastDevice: volume.Device, LastLabel: volume.Label, LastMount: volume.MountPoint,
		}
		for index := range records {
			if records[index].Identity == identity {
				records[index] = next
				return records, true
			}
		}
		return append(records, next), true
	})
}

// forgetAutoBackupRecord 移除一块卷的记录。
func (m *Manager) forgetAutoBackupRecord(identity string) error {
	return m.mutateAutoBackupRecords(func(records []AutoBackupVolumeRecord) ([]AutoBackupVolumeRecord, bool) {
		for index := range records {
			if records[index].Identity != identity {
				continue
			}
			return append(records[:index:index], records[index+1:]...), true
		}
		return records, false
	})
}

// AutoBackupDevices 打标列表：当前插着的存储设备（可打标 / 不可打标）+ 已打标
// 但当前离线的设备记录。
func (m *Manager) AutoBackupDevices() []AutoBackupDevice {
	records, _ := m.autoBackupRecords()
	devices := make([]AutoBackupDevice, 0, len(records)+4)
	seen := make(map[string]bool)
	for _, volume := range m.ListBackupVolumes() {
		if !backupDeviceFSType(volume.FSType) {
			continue
		}
		entry := AutoBackupVolume{
			Device: volume.Device, MountPoint: volume.MountPoint,
			Label: volume.Label, UUID: volume.UUID, ReadOnly: volume.ReadOnly,
		}
		mark, marked, legacy := m.readAutoBackupMark(entry)
		identity := entry.identity()
		if identity != "" {
			seen[identity] = true
		}
		devices = append(devices, AutoBackupDevice{
			Identity: identity, Alias: mark.Alias, Subs: mark.Subs,
			Marked: marked, Online: true, Legacy: legacy,
			ReadOnly: volume.ReadOnly, Removable: volume.Removable,
			Device: volume.Device, MountPoint: volume.MountPoint,
			Label: volume.Label, UUID: volume.UUID, MarkedAt: mark.MarkedAt,
		})
	}
	for _, record := range records {
		if seen[record.Identity] {
			continue
		}
		devices = append(devices, AutoBackupDevice{
			Identity: record.Identity, Alias: record.Alias, Subs: record.Subs,
			Marked: true, Online: false, PendingClear: record.PendingClear,
			Removable: true, Device: record.LastDevice, Label: record.LastLabel,
			MountPoint: record.LastMount, MarkedAt: record.MarkedAt,
		})
	}
	return devices
}

// backupDeviceFSType 判断一个卷算不算「存储设备」：网络/云盘挂载与虚拟文件系统
// 不是设备，打标列表里不展示。
func backupDeviceFSType(fsType string) bool {
	fsType = strings.ToLower(strings.TrimSpace(fsType))
	if fsType == "" {
		return false
	}
	for _, prefix := range []string{"fuse", "cifs", "smb", "nfs", "davfs", "overlay", "squashfs", "tmpfs", "devtmpfs", "proc", "sysfs", "ramfs"} {
		if strings.HasPrefix(fsType, prefix) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 执行
// ---------------------------------------------------------------------------

// autoBackupJobForVolume 把规则落到一块具体的打标卷上，生成与普通备份任务
// 同构的执行单元，复用同一套 rsync 通道与安全闸。
func autoBackupJobForVolume(rule GPIOAutoBackup, volume AutoBackupVolume) (GPIOBackupJob, error) {
	mount := filepath.ToSlash(filepath.Clean(volume.MountPoint))
	volumeLocation := func(sub string) BackupLocation {
		if strings.TrimSpace(sub) == "" {
			return BackupLocation{Kind: "path", Path: mount}
		}
		return BackupLocation{Kind: "path", Path: filepath.ToSlash(filepath.Join(mount, filepath.Clean("/"+sub)))}
	}
	job := GPIOBackupJob{
		ID: rule.ID, Name: fmt.Sprintf("%s（%s）", rule.Name, volume.displayName()),
		Mode: rule.Mode, Overwrite: rule.Overwrite, Keep: rule.Keep,
	}
	switch rule.Direction {
	case BackupDirectionToVolume:
		// 卷侧是目标：只能有一个（一个源没法同时写进多个子目录）。规则里填的
		// 子目录优先，没填就写到卷根。同步到卷根 = 清空整块盘，与内置卷根同一条红线。
		if rule.Mode == BackupModeSync && strings.TrimSpace(rule.VolumeSub) == "" {
			return GPIOBackupJob{}, fmt.Errorf("同步模式不能以整块打标卷的根目录为目标（会清空整块盘），请在卷内指定一个子目录")
		}
		job.Source, job.Target = rule.Fixed, volumeLocation(rule.VolumeSub)
	case BackupDirectionFromVolume:
		// 卷侧是源。老配置里手填的卷内子目录优先，并且保持老语义：不加尾斜杠，
		// 把该子目录里的内容铺进 NAS 目标——升级前后用同一条规则结果必须一样。
		if legacy := strings.TrimSpace(rule.VolumeSub); legacy != "" {
			job.Source, job.Target = volumeLocation(legacy), rule.Fixed
			return job, nil
		}
		// 打标时标记的子目录（可以多个）：每个子目录一个源、保留目录名
		// （NAS 上是 目标/DCIM/…），一次 rsync 传完——归档模式下它们进同一份
		// 快照，移动模式下也只删这些子目录里已成功复制的文件。
		subs := normalizeAutoBackupSubs(volume.Subs)
		if len(subs) == 0 {
			// 整块卷：保持「把卷里的内容铺进 NAS 目标」的老语义。
			job.Source, job.Target = volumeLocation(""), rule.Fixed
			return job, nil
		}
		job.Source, job.Target = volumeLocation(subs[0]), rule.Fixed
		job.KeepSourceNames = true
		for _, sub := range subs[1:] {
			job.ExtraSources = append(job.ExtraSources, volumeLocation(sub))
		}
	default:
		return GPIOBackupJob{}, fmt.Errorf("自动备份方向无效：%s", rule.Direction)
	}
	return job, nil
}

// RunAutoBackupJob 同步执行一条自动备份规则：扫描打标卷，逐个（串行）执行。
// 一块盘失败不影响其它盘，最后把所有失败一起报出来。
func (m *Manager) RunAutoBackupJob(ctx context.Context, rule GPIOAutoBackup, dryRun bool) (AutoBackupResult, error) {
	result := AutoBackupResult{
		RuleID: rule.ID, RuleName: rule.Name, Direction: rule.Direction, DryRun: dryRun,
		StartedAt: time.Now().Format(time.RFC3339),
	}
	if err := validateGPIOAutoBackup(rule); err != nil {
		return result, err
	}
	volumes := m.markedAutoBackupVolumes()
	if len(volumes) == 0 {
		result.FinishedAt = time.Now().Format(time.RFC3339)
		return result, fmt.Errorf("没有找到打标的可移动卷：请先在自动备份卡片里给 U 盘 / 移动硬盘打标（在卷内写入 %s/%s）",
			BackupAutoMarkerDirName, BackupAutoMarkerFileName)
	}
	result.Volumes = len(volumes)
	// 「NAS → 打标卷」+ 移动类模式会删掉 NAS 上的源：这条规则必须只作用于一块盘。
	// 同时插着多块打标卷时没法知道「搬到哪一块」，而且第一块盘把源搬空之后，后面的
	// 盘只会「成功」地传 0 个文件——静默地把冗余变成单点，比直接报错危险得多。
	// 试跑不删任何东西，允许列出计划，方便用户看清会发生什么。
	if !dryRun && rule.Direction == BackupDirectionToVolume && backupModeIsMove(rule.Mode) && len(volumes) > 1 {
		return result, fmt.Errorf("规则「%s」用的是移动类模式（会删除 NAS 上的源文件），而当前插着 %d 块打标卷：这条规则一次只能作用于一块盘，请先拔掉其它打标卷再运行", rule.Name, len(volumes))
	}
	// 「打标卷 → NAS」时所有卷共用 rule.Fixed 这个 NAS 目标。多块卷同时在线：
	//   · sync 模式：第二块卷的 --delete 会把第一块卷刚同步进 NAS 的文件删掉，
	//     正式运行直接拒绝；
	//   · 归档类模式：逐卷清理快照会按「目标根下全部快照」计数，卷 B 的清理把
	//     卷 A 刚生成的快照删掉——move_archive 下源文件已经删了，数据从卡上和
	//     NAS 上同时消失。改成单卷不清理、全部跑完后统一清理一次，保留份数
	//     不小于本次新生成的快照数。单块卷的行为不变。
	sharedTarget := rule.Direction == BackupDirectionFromVolume && len(volumes) > 1
	if !dryRun && sharedTarget && rule.Mode == BackupModeSync {
		return result, fmt.Errorf("规则「%s」用的是同步模式（会删除目标里多出来的文件），而当前插着 %d 块打标卷：它们共用同一个 NAS 目标，后一块卷会把前一块刚同步的文件删掉。请一次只插一块打标卷，或改用归档类模式", rule.Name, len(volumes))
	}
	deferPrune := sharedTarget && backupModeUsesSnapshots(rule.Mode)
	sharedTargetRoot := ""
	if deferPrune {
		if root, rerr := m.resolveBackupLocation(rule.Fixed); rerr == nil {
			sharedTargetRoot = root
		} else {
			// 目标解析不了时 RunBackupJob 会报同样的错，这里不再拦，
			// 让逐卷的错误信息说话。
			deferPrune = false
		}
	}
	newSnapshots := make([]string, 0, len(volumes))
	var failures []string
	for _, volume := range volumes {
		if ctx != nil && ctx.Err() != nil {
			failures = append(failures, "已取消")
			break
		}
		job, err := autoBackupJobForVolume(rule, volume)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s：%v", volume.displayName(), err))
			continue
		}
		// 往卷里备份时自动创建卷内目标目录：子目录还没建就报错太不友好，
		// 而且试跑也需要目录存在。NAS 侧目标仍要求已存在（在 RunBackupJob 里），
		// 避免手滑写错路径时在 NAS 上静默建出一堆目录。
		if rule.Direction == BackupDirectionToVolume {
			if target, rerr := m.resolveBackupLocation(job.Target); rerr == nil {
				if merr := os.MkdirAll(m.rooted(target), 0o755); merr != nil {
					failures = append(failures, fmt.Sprintf("%s：创建卷内目标目录失败：%v", volume.displayName(), merr))
					continue
				}
			}
		}
		run, err := m.runBackupJobWithOptions(ctx, job, dryRun, backupRunOptions{deferPruneTo: deferPrune})
		result.Results = append(result.Results, run)
		result.Files += run.Files
		result.Bytes += run.Bytes
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s：%v", volume.displayName(), err))
		} else if deferPrune && run.Snapshot != "" {
			newSnapshots = append(newSnapshots, run.Snapshot)
		}
	}
	// 多块卷共用 NAS 目标时的统一清理：保留份数不小于本次新生成的快照数，
	// 否则 Keep 比卷数小时会把本次刚生成的快照也清掉。试跑不删任何东西。
	if deferPrune && !dryRun && sharedTargetRoot != "" && rule.Keep > 0 {
		keep := rule.Keep
		if len(newSnapshots) > keep {
			keep = len(newSnapshots)
		}
		pruned, perr := m.pruneBackupSnapshots(sharedTargetRoot, keep)
		result.Pruned += pruned
		if perr != nil {
			failures = append(failures, "清理旧快照失败："+perr.Error())
		}
	}
	result.FinishedAt = time.Now().Format(time.RFC3339)
	if len(failures) > 0 {
		return result, fmt.Errorf("%d/%d 块打标卷处理失败：%s", len(failures), len(volumes), strings.Join(failures, "；"))
	}
	return result, nil
}
