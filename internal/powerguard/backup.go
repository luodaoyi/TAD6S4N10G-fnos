package powerguard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// backup.go 实现「按键触发备份」：源/目标/模式全部在界面上图形化选择，
// 由机箱按键（或界面上的「立即运行」）执行。
//
// 五种模式，底层统一交给系统自带的 rsync（飞牛已内置 /usr/bin/rsync）：
//
//	copy           直接复制覆盖 —— 源内容复制进目标；同名文件按「覆盖开关」决定覆盖或跳过
//	sync           同步文件     —— 目标与源完全一致，目标里多出来的文件会被删除
//	archive        按日期归档   —— 每次在目标下生成 YYYY-MM-DD_HHMMSS 快照，未变化的文件通过
//	                               --link-dest 与上一份快照做硬链接复用，只真正复制新增/变化的文件，
//	                               即「硬链接增量」，多份快照几乎不额外占空间
//	move_overwrite 移动覆盖     —— 同名文件覆盖后把源文件删掉（只删已成功复制的）；
//	                               固定覆盖，不能与 --ignore-existing 组合（那会删掉没传的文件）
//	move_archive   按日期转移归档 —— 先在目标下建日期目录，复制完成后删除源文件；
//	                               只删已成功复制的文件，目录保留
//
// 两种「移动」模式只删除被 rsync 确认传输成功的文件（--remove-source-files），
// 不会动没传完的文件，也不会删目录结构。普通备份任务与自动备份都开放，
// 但源位置受 moveSourceGuard 约束：可移动卷根目录可以整盘搬走，
// 「我的文件」「团队文件」「远程挂载」根目录与系统目录不行——一次误选就会搬空。
//
// 可移动介质：卷标（LABEL）与 UUID 作为稳定标识，运行时解析到实际挂载点，
// 因此 U 盘/外接硬盘插拔换口导致的挂载路径变化不会让任务失效。

const (
	// BackupModeCopy 直接复制覆盖：不删除目标里的多余文件。
	BackupModeCopy = "copy"
	// BackupModeSync 同步文件：目标镜像源，删除多余文件。
	BackupModeSync = "sync"
	// BackupModeArchive 按日期归档 + 硬链接增量。
	BackupModeArchive = "archive"
	// BackupModeMoveOverwrite 移动覆盖：复制后删除源文件（只删已成功复制的）。
	BackupModeMoveOverwrite = "move_overwrite"
	// BackupModeMoveArchive 按日期转移归档：复制进日期目录后删除源文件。
	BackupModeMoveArchive = "move_archive"

	backupJobMaxCount    = 32
	backupJobMaxName     = 64
	backupIDMaxLen       = 64
	backupMarkerMaxLen   = 128
	backupLocationMaxLen = 1024

	// 快照目录名，同时用于识别「哪些目录是本插件生成的快照」。
	backupSnapshotLayout = "2006-01-02_150405"
	backupSnapshotSuffix = "_%d"

	backupRsyncFallbackTimeout = 6 * time.Hour
)

// backupModeIsMove 判断是否为会删除源文件的「移动」模式。
func backupModeIsMove(mode string) bool {
	return mode == BackupModeMoveOverwrite || mode == BackupModeMoveArchive
}

var (
	backupJobIDPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	backupSnapshotPat   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}_\d{6}(_\d+)?$`)
	backupVolumeRootPat = regexp.MustCompile(`^/vol[0-9]*$`)
	// 个人空间根目录 /vol1/1000：同步模式会把目标里多余的文件删掉，
	// 直接指到整个「我的文件」等于清空个人空间，必须要求再往下选一层。
	backupPersonalRootPat = regexp.MustCompile(`^/vol[0-9]+/[0-9]+$`)
	backupRsyncOnce       struct {
		path string
		err  error
		done bool
	}
)

// backupForbiddenTargets 明确禁止把备份目标指向系统关键目录：sync 模式会删除
// 目标中多余的文件，指到这些位置等于删系统。要求用户至少选到二级目录。
var backupForbiddenTargets = map[string]bool{
	"/": true, "/boot": true, "/boot/efi": true, "/dev": true, "/proc": true,
	"/sys": true, "/run": true, "/bin": true, "/sbin": true, "/lib": true,
	"/lib32": true, "/lib64": true, "/libx32": true, "/usr": true, "/etc": true,
	"/var": true, "/opt": true, "/tmp": true, "/root": true, "/home": true,
	"/mnt": true, "/media": true, "/run/media": true, "/srv": true, "/boot/firmware": true,
}

// BackupLocation 描述备份的源或目标。两种形态：
//
//	kind=path   直接用绝对路径（普通目录/文件）
//	kind=volume 用卷标或 UUID 标识可移动介质（插入顺序/接口变化不影响），
//	            可再用 sub 指定卷内的子目录
type BackupLocation struct {
	Kind  string `json:"kind"`
	Path  string `json:"path,omitempty"`
	UUID  string `json:"uuid,omitempty"`
	Label string `json:"label,omitempty"`
	Sub   string `json:"sub,omitempty"`
}

// GPIOBackupJob 一个图形化配置的备份任务，可被按键动作 backup:<id> 引用。
type GPIOBackupJob struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Source    BackupLocation `json:"source"`
	Target    BackupLocation `json:"target"`
	Mode      string         `json:"mode"`
	Overwrite bool           `json:"overwrite"`
	Keep      int            `json:"keep,omitempty"`
	Marker    string         `json:"marker,omitempty"`
	// ExtraSources 额外的源：自动备份的「打标卷 → NAS」方向在一块盘上标了多个
	// 子目录时，一次 rsync 传多个源——归档模式下它们进同一份快照，移动模式下
	// 也只删这些源里已成功复制的文件。普通备份任务不用它。
	ExtraSources []BackupLocation `json:"extra_sources,omitempty"`
	// KeepSourceNames 目录源不加尾斜杠：rsync 会把目录本身复制进目标
	// （NAS 上是 目标/DCIM/…），而不是把内容铺进目标根。
	KeepSourceNames bool `json:"keep_source_names,omitempty"`
}

// BackupVolume 供界面选择的可挂载卷（含可移动介质）。
type BackupVolume struct {
	Device     string `json:"device"`
	MountPoint string `json:"mount_point"`
	Label      string `json:"label,omitempty"`
	UUID       string `json:"uuid,omitempty"`
	FSType     string `json:"fstype,omitempty"`
	Removable  bool   `json:"removable"`
	ReadOnly   bool   `json:"read_only"`
	TotalBytes uint64 `json:"total_bytes,omitempty"`
	FreeBytes  uint64 `json:"free_bytes,omitempty"`
}

// BackupBrowseEntry 目录浏览结果里的一项。
type BackupBrowseEntry struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	IsDir      bool   `json:"is_dir"`
	Size       int64  `json:"size,omitempty"`
	ModifiedAt string `json:"modified_at,omitempty"`
	IsVolume   bool   `json:"is_volume,omitempty"`
	IsPlace    bool   `json:"is_place,omitempty"`
	ReadOnly   bool   `json:"read_only,omitempty"`
	Link       bool   `json:"link,omitempty"`
}

// BackupBrowseResult 一次目录浏览的完整响应。
type BackupBrowseResult struct {
	// Mode=places 表示当前是「位置」总览（还没进入具体目录）；mode=browse 为目录内容。
	Mode string `json:"mode"`
	// Scope=places 只允许在位置范围内选择；scope=all 为高级模式（整个文件系统）。
	Scope string `json:"scope"`
	// Path 为空表示位置总览。
	Path string `json:"path"`
	// Parent 为空表示「上一级」应回到位置总览。
	Parent string `json:"parent,omitempty"`
	// Inside 是当前路径所属的位置 ID，界面据此提示「外接存储会按 UUID 记录」等信息。
	Inside  string              `json:"inside,omitempty"`
	Places  []BackupPlace       `json:"places"`
	Entries []BackupBrowseEntry `json:"entries"`
	Volumes []BackupVolume      `json:"volumes"`
}

// BackupPlace 一个「位置」入口，与飞牛文件管理器左侧的分类一一对应：
// 我的文件（internal 卷里的用户个人空间）、团队文件（共享目录）、
// 外接存储（U 盘/移动硬盘）、远程挂载（云盘与网络位置）。
type BackupPlace struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Subtitle   string `json:"subtitle,omitempty"`
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	Removable  bool   `json:"removable,omitempty"`
	UUID       string `json:"uuid,omitempty"`
	Label      string `json:"label,omitempty"`
	ReadOnly   bool   `json:"read_only,omitempty"`
	TotalBytes uint64 `json:"total_bytes,omitempty"`
	FreeBytes  uint64 `json:"free_bytes,omitempty"`
}

// BackupResult 一次备份执行的结果。
type BackupResult struct {
	JobID      string `json:"job_id"`
	JobName    string `json:"job_name"`
	Mode       string `json:"mode"`
	Source     string `json:"source"`
	Target     string `json:"target"`
	Snapshot   string `json:"snapshot,omitempty"`
	Files      int64  `json:"files"`
	Bytes      int64  `json:"bytes"`
	Deleted    int64  `json:"deleted,omitempty"`
	Pruned     int    `json:"pruned,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	DryRun     bool   `json:"dry_run,omitempty"`
	Warning    string `json:"warning,omitempty"`
	Output     string `json:"output,omitempty"`
}

// BackupStatus 备份运行状态，随 /api/backup/status 下发。
type BackupStatus struct {
	Running   bool          `json:"running"`
	JobID     string        `json:"job_id,omitempty"`
	JobName   string        `json:"job_name,omitempty"`
	StartedAt string        `json:"started_at,omitempty"`
	Last      *BackupResult `json:"last,omitempty"`
	// Auto 是最近一次自动备份（多块打标卷）的汇总；普通任务运行后被清空。
	Auto      *AutoBackupResult `json:"auto,omitempty"`
	LastError string            `json:"last_error,omitempty"`
}

// ---------------------------------------------------------------------------
// 配置校验
// ---------------------------------------------------------------------------

func validateGPIOBackups(jobs []GPIOBackupJob) error {
	if len(jobs) > backupJobMaxCount {
		return fmt.Errorf("备份任务不能超过 %d 个", backupJobMaxCount)
	}
	ids := make(map[string]bool, len(jobs))
	names := make(map[string]bool, len(jobs))
	for _, job := range jobs {
		if err := validateGPIOBackupJob(job); err != nil {
			return err
		}
		if ids[job.ID] {
			return fmt.Errorf("备份任务 ID %q 重复", job.ID)
		}
		key := strings.ToLower(strings.TrimSpace(job.Name))
		if names[key] {
			return fmt.Errorf("备份任务名称 %q 重复", strings.TrimSpace(job.Name))
		}
		ids[job.ID] = true
		names[key] = true
	}
	return nil
}

func validateGPIOBackupJob(job GPIOBackupJob) error {
	if !backupJobIDPattern.MatchString(job.ID) {
		return fmt.Errorf("备份任务 ID %q 只能使用 1-%d 位字母、数字、点、下划线或连字符", job.ID, backupIDMaxLen)
	}
	name := strings.TrimSpace(job.Name)
	if name == "" || utf8.RuneCountInString(name) > backupJobMaxName {
		return fmt.Errorf("备份任务 %q 的名称长度必须为 1-%d 个字符", job.ID, backupJobMaxName)
	}
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return fmt.Errorf("备份任务 %q 的名称含有控制字符", job.ID)
	}
	switch job.Mode {
	case BackupModeCopy, BackupModeSync, BackupModeArchive:
	case BackupModeMoveOverwrite, BackupModeMoveArchive:
		// 移动类模式会删源，普通备份任务也开放（与自动备份一致），
		// 但执行前有 moveSourceGuard 挡住「一次误选就搬空」的位置。
	default:
		return fmt.Errorf("备份任务 %q 的复制模式无效", job.ID)
	}
	if err := validateBackupLocation(job.ID, "源", job.Source); err != nil {
		return err
	}
	if err := validateBackupLocation(job.ID, "目标", job.Target); err != nil {
		return err
	}
	if len(job.ExtraSources) > autoBackupSubMaxCount {
		return fmt.Errorf("备份任务 %q 的附加源过多（最多 %d 个）", job.ID, autoBackupSubMaxCount)
	}
	for _, location := range job.ExtraSources {
		if err := validateBackupLocation(job.ID, "附加源", location); err != nil {
			return err
		}
	}
	if job.Keep < 0 || job.Keep > 1000 {
		return fmt.Errorf("备份任务 %q 的归档保留份数必须在 0-1000 之间（0 表示不限）", job.ID)
	}
	if utf8.RuneCountInString(job.Marker) > backupMarkerMaxLen {
		return fmt.Errorf("备份任务 %q 的标识内容不能超过 %d 个字符", job.ID, backupMarkerMaxLen)
	}
	if strings.ContainsRune(job.Marker, '\x00') {
		return fmt.Errorf("备份任务 %q 的标识内容不能包含 NUL 字符", job.ID)
	}
	return nil
}

func validateBackupLocation(jobID, role string, loc BackupLocation) error {
	switch loc.Kind {
	case "", "path":
		path := strings.TrimSpace(loc.Path)
		if path == "" {
			return fmt.Errorf("备份任务 %q 必须选择%s", jobID, role)
		}
		if len(path) > backupLocationMaxLen {
			return fmt.Errorf("备份任务 %q 的%s路径过长", jobID, role)
		}
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("备份任务 %q 的%s必须是绝对路径", jobID, role)
		}
		if strings.ContainsRune(path, '\x00') {
			return fmt.Errorf("备份任务 %q 的%s路径含有非法字符", jobID, role)
		}
		if strings.Contains(path, "/../") || strings.HasSuffix(path, "/..") {
			return fmt.Errorf("备份任务 %q 的%s路径不能包含 ..", jobID, role)
		}
	case "volume":
		uuid := strings.TrimSpace(loc.UUID)
		label := strings.TrimSpace(loc.Label)
		if uuid == "" && label == "" {
			return fmt.Errorf("备份任务 %q 的%s必须指定卷标或 UUID", jobID, role)
		}
		if len(uuid) > 128 || len(label) > 128 {
			return fmt.Errorf("备份任务 %q 的%s卷标识过长", jobID, role)
		}
		sub := strings.TrimSpace(loc.Sub)
		if len(sub) > backupLocationMaxLen {
			return fmt.Errorf("备份任务 %q 的%s子目录过长", jobID, role)
		}
		if strings.HasPrefix(sub, "/") {
			return fmt.Errorf("备份任务 %q 的%s子目录必须是相对路径", jobID, role)
		}
		if strings.Contains(sub, "..") {
			return fmt.Errorf("备份任务 %q 的%s子目录不能包含 ..", jobID, role)
		}
	default:
		return fmt.Errorf("备份任务 %q 的%s类型无效", jobID, role)
	}
	return nil
}

func (loc BackupLocation) summary() string {
	if loc.Kind == "volume" {
		name := strings.TrimSpace(loc.Label)
		if name == "" {
			name = strings.TrimSpace(loc.UUID)
		}
		if loc.Sub != "" {
			name += "/" + loc.Sub
		}
		return "卷 " + name
	}
	return loc.Path
}

// ---------------------------------------------------------------------------
// action 绑定
// ---------------------------------------------------------------------------

const GPIOActionBackupPrefix = "backup:"

func gpioBackupIDFromAction(action string) (string, bool) {
	if !strings.HasPrefix(action, GPIOActionBackupPrefix) {
		return "", false
	}
	return strings.TrimPrefix(action, GPIOActionBackupPrefix), true
}

func gpioBackupForAction(config GPIOConfig, action string) *GPIOBackupJob {
	id, ok := gpioBackupIDFromAction(action)
	if !ok {
		return nil
	}
	for _, job := range config.Backups {
		if job.ID == id {
			clone := job
			return &clone
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 卷与路径解析
// ---------------------------------------------------------------------------

// rsyncBinary 定位系统 rsync。飞牛系统盘自带 /usr/bin/rsync；找不到时给出
// 明确提示，避免用户以为是权限问题。
func (m *Manager) rsyncBinary() (string, error) {
	if backupRsyncOnce.done {
		return backupRsyncOnce.path, backupRsyncOnce.err
	}
	path, err := exec.LookPath("rsync")
	if err != nil {
		backupRsyncOnce.path, backupRsyncOnce.err = "", fmt.Errorf("系统未安装 rsync，无法执行备份（请先安装 rsync 后重试）")
	} else {
		backupRsyncOnce.path, backupRsyncOnce.err = path, nil
	}
	backupRsyncOnce.done = true
	return backupRsyncOnce.path, backupRsyncOnce.err
}

// parseMountInfo 读取挂载表，返回「块设备 → 挂载点」条目。
func (m *Manager) parseMountInfo() []mountEntry {
	data, err := os.ReadFile(m.rooted("/proc/mounts"))
	if err != nil {
		return nil
	}
	var entries []mountEntry
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || !strings.HasPrefix(fields[0], "/dev/") {
			continue
		}
		device := fields[0]
		mountPoint := strings.ReplaceAll(fields[1], `\040`, " ")
		mountPoint = strings.ReplaceAll(mountPoint, `\011`, "\t")
		mountPoint = strings.ReplaceAll(mountPoint, `\134`, `\`)
		if seen[device] {
			continue
		}
		seen[device] = true
		entries = append(entries, mountEntry{
			device:     device,
			mountPoint: mountPoint,
			fsType:     fields[2],
			options:    fields[3],
			readOnly:   hasMountOption(fields[3], "ro"),
		})
	}
	return entries
}

type mountEntry struct {
	device     string
	mountPoint string
	fsType     string
	options    string
	readOnly   bool
}

func hasMountOption(options, want string) bool {
	for _, option := range strings.Split(options, ",") {
		if option == want {
			return true
		}
	}
	return false
}

// volumeIdentifiers 从 /dev/disk/by-label 与 /dev/disk/by-uuid 反查设备的
// 卷标与 UUID。目录不存在（无 udev 符号链接）时返回空表，不报错。
func (m *Manager) volumeIdentifiers() (labels map[string]string, uuids map[string]string) {
	labels = make(map[string]string)
	uuids = make(map[string]string)
	collect := func(dir string, into map[string]string) {
		items, err := os.ReadDir(m.rooted(dir))
		if err != nil {
			return
		}
		for _, item := range items {
			target, err := os.Readlink(filepath.Join(m.rooted(dir), item.Name()))
			if err != nil {
				continue
			}
			device := filepath.Base(target)
			if device == "" {
				continue
			}
			name := item.Name()
			// udev 对不支持的字符做转义（\x20 等），做一次还原，保证卷标可读
			if unescaped, err := unescapeUdevName(name); err == nil {
				name = unescaped
			}
			if _, exists := into[device]; !exists {
				into[device] = name
			}
		}
	}
	collect("/dev/disk/by-label", labels)
	collect("/dev/disk/by-uuid", uuids)
	return labels, uuids
}

func unescapeUdevName(name string) (string, error) {
	if !strings.Contains(name, `\x`) {
		return name, nil
	}
	var builder strings.Builder
	for i := 0; i < len(name); {
		if i+3 < len(name) && name[i] == '\\' && name[i+1] == 'x' {
			value, err := strconv.ParseUint(name[i+2:i+4], 16, 8)
			if err != nil {
				return name, err
			}
			builder.WriteByte(byte(value))
			i += 4
			continue
		}
		builder.WriteByte(name[i])
		i++
	}
	return builder.String(), nil
}

// isRemovableDevice 判断块设备（整盘或分区）算不算可移动存储（USB 等）。
// 不再用「去掉末尾数字」推父设备——nvme0n1p1、mmcblk0p1 这类名字去掉数字
// 得到的不是父盘。改走 sysfs 符号链接：/sys/class/block/<name> 指向设备的
// sysfs 目录，分区嵌在整盘目录下面一层（有 partition 属性文件可辨），取上级
// 即整盘；判定规则与 storageBlockIsRemovable 完全一致：removable=1，或解析后
// 的路径含 /usb——不少 USB 硬盘盒把 removable 报成 0，只能靠总线路径认出来。
func (m *Manager) isRemovableDevice(device string) bool {
	name := filepath.Base(device)
	if name == "" {
		return false
	}
	dir, ok := m.blockDiskSysDir(name)
	if !ok {
		return false
	}
	return m.storageBlockIsRemovable(dir)
}

// blockDiskSysDir 解析块设备的 sysfs 目录并归一到整盘目录（分区上溯一层）。
// 先试 /sys/class/block/<name>（真实内核的标准入口，各种命名都能正确归属），
// 再退回 /sys/block/<name>（老内核与测试假根）；两者都没有时才按分区名推
// 整盘名兜底——真实内核不需要走到这一步。
func (m *Manager) blockDiskSysDir(name string) (string, bool) {
	for _, candidate := range []string{"/sys/class/block/" + name, "/sys/block/" + name} {
		resolved, err := filepath.EvalSymlinks(m.rooted(candidate))
		if err != nil {
			continue
		}
		resolved = filepath.ToSlash(resolved)
		if _, err := os.Stat(filepath.Join(resolved, "partition")); err == nil {
			// <name> 是分区：sysfs 里它嵌在整盘目录下面一层。
			resolved = filepath.Dir(resolved)
		}
		return resolved, true
	}
	if disk := blockPartitionDiskName(name); disk != "" && disk != name {
		if resolved, err := filepath.EvalSymlinks(m.rooted("/sys/block/" + disk)); err == nil {
			return filepath.ToSlash(resolved), true
		}
	}
	return "", false
}

// blockPartitionDiskName 从分区名推断整盘名：sdc1 → sdc、nvme0n1p1 → nvme0n1、
// mmcblk0p1 → mmcblk0。只作 /sys/class/block 缺失时的兜底，且同时认
// 「<整盘><数字>」与「<整盘>p<数字>」两种命名，避免老实现把 nvme0n1p1 猜成
// nvme0n1p 的问题。
func blockPartitionDiskName(name string) string {
	if index := strings.LastIndexByte(name, 'p'); index > 0 && index < len(name)-1 {
		if isAllDigits(name[index+1:]) && isASCIIDigit(name[index-1]) {
			return name[:index]
		}
	}
	index := len(name)
	for index > 0 && isASCIIDigit(name[index-1]) {
		index--
	}
	if index == 0 || index == len(name) {
		return ""
	}
	return name[:index]
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isASCIIDigit(s[i]) {
			return false
		}
	}
	return true
}

func isASCIIDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

// ListBackupVolumes 列出可用于备份的卷：过滤掉系统/boot 分区，保留数据卷与可移动介质。
func (m *Manager) ListBackupVolumes() []BackupVolume {
	entries := m.parseMountInfo()
	labels, uuids := m.volumeIdentifiers()
	var volumes []BackupVolume
	for _, entry := range entries {
		base := filepath.Base(entry.device)
		switch entry.mountPoint {
		case "/", "/boot", "/boot/efi", "/boot/firmware":
			continue
		}
		if strings.HasPrefix(entry.fsType, "overlay") || strings.HasPrefix(entry.fsType, "squashfs") {
			continue
		}
		volume := BackupVolume{
			Device:     entry.device,
			MountPoint: entry.mountPoint,
			Label:      labels[base],
			UUID:       uuids[base],
			FSType:     entry.fsType,
			Removable:  m.isRemovableDevice(entry.device),
			ReadOnly:   entry.readOnly,
		}
		if total, free, err := filesystemUsage(m.rooted(entry.mountPoint)); err == nil {
			volume.TotalBytes, volume.FreeBytes = total, free
		}
		volumes = append(volumes, volume)
	}
	sort.SliceStable(volumes, func(i, j int) bool {
		if volumes[i].Removable != volumes[j].Removable {
			return volumes[i].Removable
		}
		return volumes[i].MountPoint < volumes[j].MountPoint
	})
	return volumes
}

// resolveBackupLocation 把配置里的位置解析成实际绝对路径。可移动介质按
// UUID 优先、卷标次之的顺序定位，因此换接口/换插入顺序都能找到。
func (m *Manager) resolveBackupLocation(loc BackupLocation) (string, error) {
	switch loc.Kind {
	case "", "path":
		path := strings.TrimSpace(loc.Path)
		if path == "" {
			return "", errors.New("未选择路径")
		}
		return filepath.Clean(path), nil
	case "volume":
		uuid := strings.TrimSpace(loc.UUID)
		label := strings.TrimSpace(loc.Label)
		entries := m.parseMountInfo()
		labels, uuids := m.volumeIdentifiers()
		for _, entry := range entries {
			base := filepath.Base(entry.device)
			if uuid != "" {
				if uuids[base] == uuid {
					return filepath.Join(entry.mountPoint, filepath.Clean("/"+strings.TrimSpace(loc.Sub))), nil
				}
				continue
			}
			if labels[base] == label {
				return filepath.Join(entry.mountPoint, filepath.Clean("/"+strings.TrimSpace(loc.Sub))), nil
			}
		}
		name := label
		if name == "" {
			name = uuid
		}
		if uuid != "" {
			return "", fmt.Errorf("未找到 UUID 为 %s 的卷：请确认设备已插好并已挂载", name)
		}
		return "", fmt.Errorf("未找到卷标为 %q 的卷：请确认设备已插好并已挂载（也可改用 UUID 指定，更稳定）", name)
	default:
		return "", fmt.Errorf("位置类型 %q 无效", loc.Kind)
	}
}

// ---------------------------------------------------------------------------
// 位置（我的文件 / 团队文件 / 外接存储 / 远程挂载）
// ---------------------------------------------------------------------------

// 位置分类，用词与飞牛文件管理器保持一致，用户一眼能对上。
const (
	BackupPlacePersonal = "personal" // 我的文件
	BackupPlaceTeam     = "team"     // 团队文件
	BackupPlaceExternal = "external" // 外接存储
	BackupPlaceRemote   = "remote"   // 远程挂载
)

// 浏览范围：默认只让用户在「位置」里选，避免小白把备份目标指到系统目录；
// scope=all 是界面上的高级模式，放开到整个文件系统。
const (
	BackupScopePlaces = "places"
	BackupScopeAll    = "all"
)

var (
	// 内部存储卷根：飞牛的数据盘挂载在 /vol1、/vol2 …
	backupInternalVolumePattern = regexp.MustCompile(`^/vol[0-9]+$`)
	// 个人空间目录名就是 uid（/vol1/1000）。
	backupPersonalDirPattern = regexp.MustCompile(`^[0-9]+$`)
)

// backupDataFSTypes 内部存储使用的文件系统，用来把数据盘和伪文件系统区分开。
var backupDataFSTypes = map[string]bool{
	"btrfs": true, "ext2": true, "ext3": true, "ext4": true,
	"xfs": true, "f2fs": true, "jfs": true, "zfs": true,
}

// isBackupRemoteFSType 判断是否远程挂载：云盘走 rclone FUSE，另有 SMB/NFS/WebDAV。
func isBackupRemoteFSType(fsType string) bool {
	if strings.HasPrefix(fsType, "fuse.") || fsType == "fuseblk" {
		return true
	}
	switch fsType {
	case "nfs", "nfs4", "cifs", "smb3", "davfs", "webdav", "sshfs":
		return true
	}
	return false
}

// backupSystemDirNames 卷根下不面向用户的目录名（插件数据、缩略图、回收站等），
// 外加本插件自己的元数据（.tad-backup 目录与两个旧版标记名）：浏览窗口没必要把
// 备份元数据摆给用户看，也不该让人把备份目标指到里面去。
var backupSystemDirNames = map[string]bool{
	"docker": true, "thumb": true, "mediasrv.transcode": true,
	"appcenter-downloads": true, "lost+found": true, "#recycle": true,
	"#snapshot": true, "swapfile": true,
	".tad-backup": true, ".tad-auto-backup": true, ".tad-backup-source": true,
}

func isBackupSystemDirName(name string) bool {
	if name == "" || strings.HasPrefix(name, "@") ||
		strings.HasPrefix(name, ".") || strings.HasPrefix(name, "#") {
		return true
	}
	return backupSystemDirNames[name]
}

// isBackupHiddenEntryName 浏览「位置」内部时隐藏的条目名：只过滤飞牛自己的内部标记
// （@ 开头、# 开头、.@ 开头）和已知系统目录，用户的普通隐藏文件（.git 等）照常显示。
func isBackupHiddenEntryName(name string) bool {
	if name == "" || strings.HasPrefix(name, "@") ||
		strings.HasPrefix(name, "#") || strings.HasPrefix(name, ".@") {
		return true
	}
	return backupSystemDirNames[name]
}

// mountRecord 一行 /proc/mounts，不做 /dev 过滤，因此也能看到 fuse 网络挂载。
type mountRecord struct {
	source     string
	mountPoint string
	fsType     string
	options    string
	readOnly   bool
}

// parseAllMounts 读取 /proc/mounts 的全部条目，按挂载点去重。
func (m *Manager) parseAllMounts() []mountRecord {
	data, err := os.ReadFile(m.rooted("/proc/mounts"))
	if err != nil {
		return nil
	}
	var records []mountRecord
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		mountPoint := unescapeMountField(fields[1])
		if seen[mountPoint] {
			continue
		}
		seen[mountPoint] = true
		records = append(records, mountRecord{
			source:     fields[0],
			mountPoint: mountPoint,
			fsType:     fields[2],
			options:    fields[3],
			readOnly:   hasMountOption(fields[3], "ro"),
		})
	}
	return records
}

// unescapeMountField 还原 /proc/mounts 里被转义的挂载点（空格、制表符、反斜杠）。
func unescapeMountField(value string) string {
	value = strings.ReplaceAll(value, `\040`, " ")
	value = strings.ReplaceAll(value, `\011`, "\t")
	value = strings.ReplaceAll(value, `\134`, `\`)
	return value
}

// internalVolumeRoots 内部存储卷根（形如 /vol1）。先看挂载表，挂载表里找不到
// （隔离测试、容器环境）再退回扫根目录，并排除存放远程挂载的父目录。
func (m *Manager) internalVolumeRoots() []string {
	mounts := m.parseAllMounts()
	remoteParents := make(map[string]bool)
	for _, mount := range mounts {
		if isBackupRemoteFSType(mount.fsType) {
			remoteParents[filepath.ToSlash(filepath.Dir(mount.mountPoint))] = true
		}
	}
	seen := make(map[string]bool)
	var roots []string
	add := func(path string) {
		path = filepath.ToSlash(filepath.Clean(path))
		if seen[path] || remoteParents[path] || !backupInternalVolumePattern.MatchString(path) {
			return
		}
		info, err := os.Stat(m.rooted(path))
		if err != nil || !info.IsDir() {
			return
		}
		seen[path] = true
		roots = append(roots, path)
	}
	for _, mount := range mounts {
		if backupDataFSTypes[mount.fsType] {
			add(mount.mountPoint)
		}
	}
	if len(roots) == 0 {
		if entries, err := os.ReadDir(m.rooted("/")); err == nil {
			for _, entry := range entries {
				if entry.IsDir() {
					add("/" + entry.Name())
				}
			}
		}
	}
	sort.Strings(roots)
	return roots
}

// personalPlaceSubtitle 把 uid 目录翻译成可读的用户名。
func personalPlaceSubtitle(uid string) string {
	if account, err := user.LookupId(uid); err == nil && account.Username != "" {
		return account.Username
	}
	return "用户 " + uid
}

// BackupPlaces 汇总界面可选的「位置」，顺序与飞牛文件管理器一致。
func (m *Manager) BackupPlaces() []BackupPlace {
	var personal, team, external, remote []BackupPlace

	for _, root := range m.internalVolumeRoots() {
		entries, err := os.ReadDir(m.rooted(root))
		if err != nil {
			continue
		}
		var names []string
		for _, entry := range entries {
			if entry.IsDir() {
				names = append(names, entry.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			path := filepath.ToSlash(filepath.Join(root, name))
			if backupPersonalDirPattern.MatchString(name) {
				personal = append(personal, BackupPlace{
					ID:       "personal:" + path,
					Name:     "我的文件",
					Subtitle: personalPlaceSubtitle(name),
					Path:     path,
					Kind:     BackupPlacePersonal,
				})
				continue
			}
			if isBackupSystemDirName(name) {
				continue
			}
			team = append(team, BackupPlace{
				ID:   "team:" + path,
				Name: name,
				Path: path,
				Kind: BackupPlaceTeam,
			})
		}
	}

	for _, volume := range m.ListBackupVolumes() {
		if !volume.Removable {
			continue
		}
		name := strings.TrimSpace(volume.Label)
		if name == "" {
			name = "外接存储"
		}
		external = append(external, BackupPlace{
			ID:         "external:" + volume.MountPoint,
			Name:       name,
			Subtitle:   "可移动介质",
			Path:       volume.MountPoint,
			Kind:       BackupPlaceExternal,
			Removable:  true,
			UUID:       volume.UUID,
			Label:      volume.Label,
			ReadOnly:   volume.ReadOnly,
			TotalBytes: volume.TotalBytes,
			FreeBytes:  volume.FreeBytes,
		})
	}

	friendly := m.remoteMountFriendlyNames()
	aliases := m.RemoteAliases()
	for _, mount := range m.parseAllMounts() {
		if !isBackupRemoteFSType(mount.fsType) || mount.mountPoint == "/" {
			continue
		}
		remote = append(remote, BackupPlace{
			ID:       "remote:" + mount.mountPoint,
			Name:     remoteMountDisplayName(mount.mountPoint, friendly, aliases),
			Subtitle: "远程挂载",
			Path:     mount.mountPoint,
			Kind:     BackupPlaceRemote,
			ReadOnly: mount.readOnly,
		})
	}
	sort.SliceStable(remote, func(i, j int) bool { return remote[i].Path < remote[j].Path })

	places := make([]BackupPlace, 0, len(personal)+len(team)+len(external)+len(remote))
	places = append(places, personal...)
	places = append(places, team...)
	places = append(places, external...)
	places = append(places, remote...)
	return places
}

// remoteMountFriendlyNames 尽力推断云盘的显示名：遍历挂载进程的命令行，rclone
// mount 的参数形如「rclone mount 夸克网盘: /vol02/1000-1-xxx --uid 1000 ...」，
// 段名就是云盘名。服务以 root 运行才能读 /proc/<pid>/cmdline——这正是之前用
// tank 用户怎么都查不到云盘名的原因。拿不到也不影响功能：界面退回挂载目录名，
// 并允许用户手动改名（SetRemoteAlias）。
func (m *Manager) remoteMountFriendlyNames() map[string]string {
	result := make(map[string]string)
	procDir := m.rooted("/proc")
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return result
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(procDir, entry.Name(), "cmdline"))
		if err != nil || len(data) == 0 {
			continue
		}
		parts := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
		if len(parts) < 3 || !strings.Contains(filepath.Base(parts[0]), "rclone") {
			continue
		}
		for index := 1; index < len(parts)-2; index++ {
			if parts[index] != "mount" {
				continue
			}
			name := parts[index+1]
			if at := strings.Index(name, ":"); at >= 0 {
				name = name[:at]
			}
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			result[parts[index+2]] = name
			break
		}
	}
	return result
}

// RemoteAliases 返回用户手动设置的远程挂载显示名（键为挂载点）。
func (m *Manager) RemoteAliases() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.loadConfigLocked()
	if err != nil || len(cfg.RemoteAliases) == 0 {
		return nil
	}
	clone := make(map[string]string, len(cfg.RemoteAliases))
	for key, value := range cfg.RemoteAliases {
		clone[key] = value
	}
	return clone
}

// remoteMountDisplayName 决定远程挂载在界面上的名字：手动别名 > 挂载进程推断 >
// 挂载目录名。别名/推断先按完整挂载点精确匹配，再按目录名兜底一次（rclone 的
// 挂载点参数可能带尾斜杠或被规范化）。
func remoteMountDisplayName(mountPoint string, friendly, aliases map[string]string) string {
	if alias := strings.TrimSpace(aliases[mountPoint]); alias != "" {
		return alias
	}
	base := filepath.Base(mountPoint)
	normalized := strings.TrimRight(mountPoint, "/")
	for candidate, name := range friendly {
		if strings.TrimRight(candidate, "/") == normalized ||
			filepath.Base(strings.TrimRight(candidate, "/")) == base {
			return name
		}
	}
	return base
}

// SetRemoteAlias 设置 / 清除某个远程挂载点的显示名。挂载点必须是当前识别到的
// 远程挂载，防止把别名写到无从校验的路径上；name 为空表示恢复系统命名。
func (m *Manager) SetRemoteAlias(mountPoint, name string) error {
	mountPoint = filepath.ToSlash(filepath.Clean(strings.TrimSpace(mountPoint)))
	if !strings.HasPrefix(mountPoint, "/") {
		return fmt.Errorf("远程挂载路径不正确：%q", mountPoint)
	}
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > 64 {
		return errors.New("显示名最长 64 个字符")
	}
	known := false
	for _, place := range m.BackupPlaces() {
		if place.Kind == BackupPlaceRemote && place.Path == mountPoint {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("没有找到远程挂载 %s，请刷新后重试", mountPoint)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.loadConfigLocked()
	if err != nil {
		return err
	}
	if cfg.RemoteAliases == nil {
		cfg.RemoteAliases = make(map[string]string)
	}
	if name == "" {
		delete(cfg.RemoteAliases, mountPoint)
	} else {
		cfg.RemoteAliases[mountPoint] = name
	}
	return m.saveConfigLocked(cfg)
}

// placePathOwner 返回路径所属位置的 ID；位置根目录本身也算「属于」它自己。
// 路径不在任何位置里时返回空串。
func placePathOwner(places []BackupPlace, path string) string {
	target := strings.TrimRight(filepath.ToSlash(filepath.Clean(path)), "/")
	if target == "" {
		return ""
	}
	owner := ""
	bestLen := -1
	for _, place := range places {
		root := strings.TrimRight(filepath.ToSlash(filepath.Clean(place.Path)), "/")
		if root == "" {
			continue
		}
		if target != root && !strings.HasPrefix(target, root+"/") {
			continue
		}
		if len(root) > bestLen {
			owner, bestLen = place.ID, len(root)
		}
	}
	return owner
}

// isPlaceRoot 判断路径是否正好是某个位置的根本身（用于给条目打「位置」角标）。
func isPlaceRoot(places []BackupPlace, path string) bool {
	target := strings.TrimRight(filepath.ToSlash(filepath.Clean(path)), "/")
	for _, place := range places {
		if strings.TrimRight(filepath.ToSlash(filepath.Clean(place.Path)), "/") == target {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 目录浏览
// ---------------------------------------------------------------------------

// backupBrowseBlocked 浏览时隐藏的目录：内核伪文件系统，进去没有意义且可能卡住。
// 用斜杠形式比对：Windows 上 filepath.Join 会给出反斜杠，直接比对会漏掉。
var backupBrowseBlocked = map[string]bool{
	"/proc": true, "/sys": true, "/dev": true, "/run": true, "/lost+found": true,
}

// backupBrowseBlockedPath 判断某个绝对路径是否属于隐藏目录（含正斜杠归一化）。
func backupBrowseBlockedPath(path string) bool {
	return backupBrowseBlocked[filepath.ToSlash(filepath.Clean(path))]
}

// BrowseBackupPath 列出指定目录下的子目录与文件，供界面「浏览」对话框使用。
// path 为空时返回「位置」总览。scope 默认 places：只有在位置范围内的路径才可浏览，
// 其余一律退回总览；scope=all 是高级模式，可浏览整个文件系统。
func (m *Manager) BrowseBackupPath(path, scope string) (BackupBrowseResult, error) {
	if scope != BackupScopeAll {
		scope = BackupScopePlaces
	}
	places := m.BackupPlaces()
	result := BackupBrowseResult{
		Mode:    "places",
		Scope:   scope,
		Places:  places,
		Volumes: m.ListBackupVolumes(),
	}

	clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	if !strings.HasPrefix(clean, "/") {
		clean = ""
	}
	if clean == "" {
		if scope != BackupScopeAll {
			return result, nil
		}
		clean = "/"
	}
	if scope == BackupScopePlaces {
		owner := placePathOwner(places, clean)
		if owner == "" {
			// 不在任何位置里：退回位置总览，避免小白通过手改地址栏跑到系统目录。
			return result, nil
		}
		result.Inside = owner
	}

	result.Mode = "browse"
	result.Path = clean
	if clean != "/" {
		parent := filepath.ToSlash(filepath.Dir(clean))
		if backupBrowseBlockedPath(parent) {
			parent = "/"
		}
		if scope == BackupScopePlaces && placePathOwner(places, parent) == "" {
			parent = "" // 已经到位置根目录，上一级＝回到位置总览
		}
		result.Parent = parent
	}
	if backupBrowseBlockedPath(clean) {
		return result, fmt.Errorf("该目录不可浏览")
	}
	items, err := os.ReadDir(m.rooted(clean))
	if err != nil {
		if os.IsNotExist(err) {
			return result, fmt.Errorf("目录不存在或无权限访问：%s", clean)
		}
		return result, fmt.Errorf("无法读取目录 %s：%v", clean, err)
	}
	for _, item := range items {
		name := item.Name()
		// 位置范围内浏览时隐藏飞牛自己的内部目录，避免用户点进去或误选。
		if scope == BackupScopePlaces && isBackupHiddenEntryName(name) {
			continue
		}
		childPath := filepath.ToSlash(filepath.Join(clean, name))
		if backupBrowseBlockedPath(childPath) {
			continue
		}
		entry := BackupBrowseEntry{Name: name, Path: childPath}
		info, err := item.Info()
		if err == nil {
			entry.ModifiedAt = info.ModTime().Format(time.RFC3339)
		}
		if item.Type()&os.ModeSymlink != 0 {
			entry.Link = true
			resolved, err := filepath.EvalSymlinks(m.rooted(childPath))
			if err != nil {
				continue
			}
			targetInfo, err := os.Stat(resolved)
			if err != nil {
				continue
			}
			entry.IsDir = targetInfo.IsDir()
			if !entry.IsDir {
				entry.Size = targetInfo.Size()
			}
		} else {
			entry.IsDir = item.IsDir()
			if info != nil && !entry.IsDir {
				entry.Size = info.Size()
			}
		}
		for _, volume := range result.Volumes {
			if volume.MountPoint == childPath || volume.MountPoint == entry.Path {
				entry.IsVolume = true
				entry.ReadOnly = volume.ReadOnly
				break
			}
		}
		if isPlaceRoot(places, childPath) {
			entry.IsPlace = true
		}
		result.Entries = append(result.Entries, entry)
	}
	sort.SliceStable(result.Entries, func(i, j int) bool {
		if result.Entries[i].IsDir != result.Entries[j].IsDir {
			return result.Entries[i].IsDir
		}
		return strings.ToLower(result.Entries[i].Name) < strings.ToLower(result.Entries[j].Name)
	})
	return result, nil
}

// BackupMkdirResult 新建目录的结果。
type BackupMkdirResult struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// backupDirectoryNameMaxBytes 新建目录名的长度上限（按字节，够用即可）。
const backupDirectoryNameMaxBytes = 128

// validateBackupDirectoryName 校验用户输入的目录名。只允许「一个名字」，
// 不允许夹带路径分隔符，也不允许会被浏览列表隐藏的名字——否则用户建完看不见。
func validateBackupDirectoryName(name string) error {
	if name == "" {
		return errors.New("请输入文件夹名称")
	}
	if len(name) > backupDirectoryNameMaxBytes {
		return errors.New("文件夹名称过长")
	}
	if strings.ContainsAny(name, `/\`) {
		return errors.New("文件夹名称不能包含路径分隔符")
	}
	if name == "." || name == ".." {
		return errors.New("文件夹名称无效")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return errors.New("文件夹名称不能包含控制字符")
		}
	}
	if isBackupHiddenEntryName(name) {
		return errors.New("该名称会被系统隐藏，请换一个（不要用 @、# 开头或「docker」「thumb」这类系统目录名）")
	}
	return nil
}

// CreateBackupDirectory 在 path 下新建一个名为 name 的子目录（浏览对话框的「新建文件夹」）。
// scope 与浏览保持一致：默认 places 只允许在「位置」范围内创建，all 为高级模式。
// 刻意只创建一层而不做递归：用户在地址栏里手写路径时，写错不该静默建出一条目录链。
func (m *Manager) CreateBackupDirectory(path, name, scope string) (BackupMkdirResult, error) {
	var result BackupMkdirResult
	if scope != BackupScopeAll {
		scope = BackupScopePlaces
	}
	clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	if !strings.HasPrefix(clean, "/") {
		return result, errors.New("请先进入一个目录再新建文件夹")
	}
	if backupBrowseBlockedPath(clean) {
		return result, errors.New("该目录不可操作")
	}
	if clean == "/" {
		return result, errors.New("不能直接在文件系统根目录下新建文件夹，请先进到具体位置")
	}
	if scope == BackupScopePlaces && placePathOwner(m.BackupPlaces(), clean) == "" {
		return result, errors.New("该目录不在可选位置内，可先切到原生目录模式")
	}
	info, err := os.Stat(m.rooted(clean))
	if err != nil {
		return result, fmt.Errorf("目录不存在或无权限访问：%s", clean)
	}
	if !info.IsDir() {
		return result, errors.New("只能在文件夹里新建子文件夹")
	}
	for _, volume := range m.ListBackupVolumes() {
		if !volume.ReadOnly {
			continue
		}
		if clean == volume.MountPoint || strings.HasPrefix(clean, volume.MountPoint+"/") {
			return result, errors.New("该卷为只读，无法新建文件夹")
		}
	}
	name = strings.TrimSpace(name)
	if err := validateBackupDirectoryName(name); err != nil {
		return result, err
	}
	target := filepath.ToSlash(filepath.Join(clean, name))
	if _, err := os.Lstat(m.rooted(target)); err == nil {
		return result, fmt.Errorf("同名文件或文件夹已存在：%s", name)
	}
	if err := os.Mkdir(m.rooted(target), 0o755); err != nil {
		return result, fmt.Errorf("新建文件夹失败：%v", err)
	}
	result.Path = target
	result.Name = name
	return result, nil
}

// ---------------------------------------------------------------------------
// 执行
// ---------------------------------------------------------------------------

// backupPathGuard 执行前的安全闸：禁止源/目标互相包含，禁止目标指向系统目录。
func backupPathGuard(src, dst string) error {
	src = filepath.Clean(src)
	dst = filepath.Clean(dst)
	// 黑名单用斜杠形式比对：Windows 上 filepath.Clean("/") 会变成 "\"，
	// 直接比对会漏掉，跨平台测试也无法覆盖。
	srcSlash := filepath.ToSlash(src)
	dstSlash := filepath.ToSlash(dst)
	if src == dst {
		return errors.New("源与目标不能是同一个位置")
	}
	separator := string(filepath.Separator)
	if strings.HasPrefix(src+separator, dst+separator) {
		return errors.New("目标目录位于源目录内部，会导致递归复制，请另选目标")
	}
	if strings.HasPrefix(dst+separator, src+separator) {
		return errors.New("源目录位于目标目录内部，同步时会删除源数据，请另选目标")
	}
	if backupForbiddenTargets[dstSlash] || backupVolumeRootPat.MatchString(dstSlash) {
		return errors.New("目标不能是系统目录或卷根目录，请在其下另建一个备份目录")
	}
	if backupPersonalRootPat.MatchString(dstSlash) {
		return errors.New("目标不能直接是整个「我的文件」根目录，请在它下面新建一个备份目录")
	}
	// 源允许是卷根（整块盘/整个 U 盘一起备份是常见需求），但整个系统根没有意义。
	if srcSlash == "/" {
		return errors.New("请选择具体的源目录或文件，而不是整个系统根目录")
	}
	return nil
}

// moveSourceGuard 移动类模式会删除源里的文件，这里挡住「一次误选就搬空」的位置：
// 整个存储卷、整个「我的文件」/「团队文件」/远程挂载根目录、系统目录。
// 可移动卷的根目录**不在禁止之列**——整盘导入正是自动备份的主要用法。
func (m *Manager) moveSourceGuard(src string) error {
	slash := strings.TrimRight(filepath.ToSlash(filepath.Clean(src)), "/")
	if slash == "" {
		return nil
	}
	for _, volume := range m.ListBackupVolumes() {
		if !volume.Removable {
			continue
		}
		if strings.TrimRight(filepath.ToSlash(filepath.Clean(volume.MountPoint)), "/") == slash {
			return nil
		}
	}
	if backupForbiddenTargets[slash] {
		return errors.New("移动类模式会删除源里的文件，不能以系统目录为源")
	}
	if backupVolumeRootPat.MatchString(slash) {
		return errors.New("移动类模式会删除源里的文件，不能以整个存储卷为源，请在卷内选择具体目录")
	}
	if isPlaceRoot(m.BackupPlaces(), slash) {
		return errors.New("移动类模式会删除源里的文件，源不能是整个「我的文件」「团队文件」或远程挂载根目录，请在它下面选择具体目录")
	}
	return nil
}

// checkBackupMarker 校验源目录里的标识文件。标识文件用于在路径变化（如 U 盘
// 换了盘符）时确认「找对了盘」，仅在用户填写了标识内容时生效。源是单个文件时，
// 标识文件放在它所在的目录里。
func checkBackupMarker(src, marker string) error {
	if strings.TrimSpace(marker) == "" {
		return nil
	}
	dir := src
	if info, err := os.Stat(src); err == nil && !info.IsDir() {
		dir = filepath.Dir(src)
	}
	// 新版在 .tad-backup/ 目录里，旧版直接写在根上：新版优先，找不到才看旧位置。
	for _, relative := range backupMarkerCandidates(dir) {
		data, err := os.ReadFile(relative)
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(data)) != strings.TrimSpace(marker) {
			return fmt.Errorf("源目录标识文件 %s 的内容与配置不一致（期望 %q），已中止以免复制错介质",
				backupMarkerDisplayPath(dir, relative), marker)
		}
		return nil
	}
	return fmt.Errorf("源目录缺少标识文件 %s，或内容不可读：请在任务里打开「在源中创建标识文件」，写入内容为 %q 的标识（用来确认没有认错介质）",
		backupMarkerDisplayPath(dir, backupMarkerCandidates(dir)[0]), marker)
}

const (
	// BackupMetaDirName 套件写在卷内 / 源目录里的统一元数据目录：源介质标识、
	// 自动备份打标都放在它下面，不再各自往根目录丢点文件。
	BackupMetaDirName = ".tad-backup"
	// backupMarkerFileName 源介质标识文件在元数据目录里的文件名。
	backupMarkerFileName = "source"
	// backupMarkerLegacyName 旧版源介质标识文件：直接写在源根上（v0.10.34 及以前）。
	// 只做读取兼容，下次写入时清理。
	backupMarkerLegacyName = ".tad-backup-source"
)

// backupMarkerCandidates 标识文件的候选位置，按优先级排列（新版 → 旧版）。
func backupMarkerCandidates(dir string) []string {
	return []string{
		filepath.Join(dir, BackupMetaDirName, backupMarkerFileName),
		filepath.Join(dir, backupMarkerLegacyName),
	}
}

// backupMarkerDisplayPath 把绝对路径收敛成「相对源根」的展示路径，报错时才看得懂。
func backupMarkerDisplayPath(dir, path string) string {
	relative := strings.TrimPrefix(filepath.ToSlash(path), filepath.ToSlash(filepath.Clean(dir))+"/")
	if relative == "" {
		return filepath.ToSlash(path)
	}
	return relative
}

// GenerateBackupMarker 生成一个随机标识内容。用户不需要自己编，点一下
// 「在源中创建标识文件」就能把这个内容写进源介质。
func GenerateBackupMarker() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return "tad-" + hex.EncodeToString(buf[:])
	}
	return fmt.Sprintf("tad-%d", time.Now().UnixNano())
}

// WriteBackupMarker 把标识内容写进源位置里的 .tad-backup/source 文件，返回实际
// 写入的内容与文件路径；内容为空时自动生成一个（界面会把它回填到输入框）。
// 旧版写在源根的 .tad-backup-source 会被顺手清掉，不让两处标记并存。
//
// 刻意做成「用户明确操作一下才写」：如果在执行备份时自动补写，那么插错盘也会被
// 写成「对的盘」，这项校验就完全失去意义了。
func (m *Manager) WriteBackupMarker(job GPIOBackupJob) (marker string, written string, err error) {
	marker = strings.TrimSpace(job.Marker)
	if marker == "" {
		marker = GenerateBackupMarker()
	}
	src, err := m.resolveBackupLocation(job.Source)
	if err != nil {
		return "", "", fmt.Errorf("源：%v", err)
	}
	info, err := os.Stat(m.rooted(src))
	if err != nil {
		return "", "", fmt.Errorf("源不存在或不可访问：%v", err)
	}
	dir := src
	if !info.IsDir() {
		dir = filepath.Dir(src)
	}
	metaDir := m.rooted(filepath.Join(dir, BackupMetaDirName))
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		return "", "", fmt.Errorf("创建元数据目录 %s 失败：%v", BackupMetaDirName, autoBackupWriteError(err))
	}
	target := m.rooted(filepath.Join(dir, BackupMetaDirName, backupMarkerFileName))
	if err := os.WriteFile(target, []byte(marker+"\n"), 0o644); err != nil {
		return "", "", fmt.Errorf("写入标识文件失败：%v", autoBackupWriteError(err))
	}
	// 旧位置留着会变成「同一块盘两处标记」，清掉是 best-effort：删不掉不影响校验。
	legacy := m.rooted(filepath.Join(dir, backupMarkerLegacyName))
	if err := os.Remove(legacy); err != nil && !os.IsNotExist(err) {
		m.logfUnlocked("backup: 清理旧版标识文件失败 %s: %v", legacy, err)
	}
	return marker, filepath.ToSlash(filepath.Join(dir, BackupMetaDirName, backupMarkerFileName)), nil
}

// RunBackupJob 同步执行一次备份任务。
// backupRunOptions 控制单次备份执行的可选行为；零值即默认行为。
type backupRunOptions struct {
	// deferPruneTo：本次运行末尾不做快照清理，留给调用方统一清理。
	// 「打标卷 → NAS」的多块卷共用同一个 NAS 目标，逐卷清理会按「目标根下
	// 全部快照」计数，把别的卷刚生成的快照删掉——move_archive 下源文件已经
	// 删了，数据就从卡上和 NAS 上同时消失（见 backup_auto.go 的 RunAutoBackupJob）。
	deferPruneTo bool
}

// RunBackupJob 执行一条备份任务（图形化备份卡片与按键动作共用入口）。
func (m *Manager) RunBackupJob(ctx context.Context, job GPIOBackupJob, dryRun bool) (BackupResult, error) {
	return m.runBackupJobWithOptions(ctx, job, dryRun, backupRunOptions{})
}

func (m *Manager) runBackupJobWithOptions(ctx context.Context, job GPIOBackupJob, dryRun bool, opts backupRunOptions) (BackupResult, error) {
	result := BackupResult{
		JobID: job.ID, JobName: job.Name, Mode: job.Mode, DryRun: dryRun,
		StartedAt: time.Now().Format(time.RFC3339),
	}
	started := time.Now()
	rsync, err := m.rsyncBinary()
	if err != nil {
		return result, err
	}
	src, err := m.resolveBackupLocation(job.Source)
	if err != nil {
		return result, fmt.Errorf("源：%v", err)
	}
	dst, err := m.resolveBackupLocation(job.Target)
	if err != nil {
		return result, fmt.Errorf("目标：%v", err)
	}
	result.Source, result.Target = src, dst
	if err := backupPathGuard(src, dst); err != nil {
		return result, err
	}
	srcInfo, err := os.Stat(m.rooted(src))
	if err != nil {
		return result, fmt.Errorf("源不存在或不可访问：%v", err)
	}
	// 额外源（自动备份在一块盘上标了多个子目录时）：逐个校验存在性与安全闸，
	// 任何一个不合法都直接拒绝，不给「只备了一半」留空间。
	extraSources := make([]string, 0, len(job.ExtraSources))
	extraInfos := make([]os.FileInfo, 0, len(job.ExtraSources))
	for _, location := range job.ExtraSources {
		extra, err := m.resolveBackupLocation(location)
		if err != nil {
			return result, fmt.Errorf("源：%v", err)
		}
		if err := backupPathGuard(extra, dst); err != nil {
			return result, err
		}
		info, err := os.Stat(m.rooted(extra))
		if err != nil {
			return result, fmt.Errorf("源不存在或不可访问：%v", err)
		}
		extraSources, extraInfos = append(extraSources, extra), append(extraInfos, info)
	}
	if job.Mode == BackupModeMoveOverwrite || job.Mode == BackupModeMoveArchive {
		for _, source := range append([]string{src}, extraSources...) {
			if err := m.moveSourceGuard(source); err != nil {
				return result, err
			}
		}
	}
	if err := checkBackupMarker(m.rooted(src), job.Marker); err != nil {
		return result, err
	}
	dstInfo, err := os.Stat(m.rooted(dst))
	if err != nil || !dstInfo.IsDir() {
		return result, fmt.Errorf("目标目录不存在：%s（请先创建目标目录）", dst)
	}

	args := []string{"-a", "--stats", "--human-readable"}
	if dryRun {
		args = append(args, "--dry-run")
	}
	// 元数据（源标识、打标信息）只用来确认「找对了介质」，本身不是要备份的数据，
	// 都不该被复制进备份里；旧版的两处位置也一并排除，免得残留文件跟着搬过去。
	for _, name := range []string{
		BackupMetaDirName, backupMarkerLegacyName, BackupAutoMarkerLegacyDirName,
	} {
		args = append(args, "--exclude=/"+name)
	}
	targetRoot := dst // 归档模式会把 dst 改写成快照目录，清理旧快照仍要用目标根
	switch job.Mode {
	case BackupModeCopy:
		if !job.Overwrite {
			args = append(args, "--ignore-existing")
		}
	case BackupModeSync:
		args = append(args, "--delete")
	case BackupModeArchive:
		snapshot, prev, err := m.nextBackupSnapshot(targetRoot)
		if err != nil {
			return result, err
		}
		result.Snapshot = snapshot
		if prev != "" {
			args = append(args, "--link-dest="+m.rooted(prev))
		}
		dst = snapshot
	case BackupModeMoveOverwrite, BackupModeMoveArchive:
		// 移动 = 复制成功一个删一个：--remove-source-files 只删除已成功
		// 传输的文件，目录结构保留，中途出错时没传完的文件不会被删。
		// 不能与 --ignore-existing 组合（目标已有同名时文件没传也会被删），
		// 所以移动模式固定覆盖同名文件，覆盖开关在此无效。
		// --ignore-times 强制重新传输：rsync 的 quick check 按「大小 + mtime」
		// 判断文件是否已同步，同大小同时间的不同内容会被跳过——跳过的文件
		// 照样会被 --remove-source-files 删掉，目标却还是旧内容，必须杜绝。
		if job.Mode == BackupModeMoveArchive {
			snapshot, _, err := m.nextBackupSnapshot(targetRoot)
			if err != nil {
				return result, err
			}
			result.Snapshot = snapshot
			dst = snapshot
		}
		args = append(args, "--remove-source-files", "--ignore-times")
	}

	// 目录源默认加尾斜杠 = 把目录里的内容铺进目标；KeepSourceNames 时原样传，
	// rsync 会把目录本身复制进目标（NAS 上是 目标/DCIM/…）——自动备份按打标
	// 子目录导入时用这个，用户能一眼看出哪份是哪个目录。
	sourceArg := m.rooted(src)
	if srcInfo.IsDir() && !job.KeepSourceNames {
		sourceArg = strings.TrimRight(sourceArg, "/") + "/"
	}
	args = append(args, sourceArg)
	for index, extra := range extraSources {
		arg := m.rooted(extra)
		if extraInfos[index].IsDir() && !job.KeepSourceNames {
			arg = strings.TrimRight(arg, "/") + "/"
		}
		args = append(args, arg)
	}
	args = append(args, strings.TrimRight(m.rooted(dst), "/")+"/")

	if dryRun {
		args = append(args, "--out-format=%n")
	}

	timeout := backupRsyncFallbackTimeout
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(runCtx, rsync, args...)
	output := &cappedBuffer{limit: 64 << 10}
	command.Stdout = output
	command.Stderr = output
	runErr := command.Run()
	text := strings.TrimSpace(output.String())
	result.Output = text
	result.Files, result.Bytes, result.Deleted = parseRsyncStats(text)
	result.DurationMS = time.Since(started).Milliseconds()
	result.FinishedAt = time.Now().Format(time.RFC3339)

	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return result, fmt.Errorf("备份超时（超过 %s）已中止", timeout)
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			switch exitErr.ExitCode() {
			case 23, 24:
				result.Warning = "部分文件未能复制（可能被占用或权限不足），其余已同步完成"
				if job.Mode == BackupModeMoveOverwrite || job.Mode == BackupModeMoveArchive {
					result.Warning = "部分文件未能移动（可能被占用或权限不足），它们仍保留在源目录，其余已转移完成"
				}
			default:
				return result, fmt.Errorf("rsync 执行失败（退出码 %d）：%s", exitErr.ExitCode(), firstLines(text, 3))
			}
		} else {
			return result, fmt.Errorf("rsync 执行失败：%v", runErr)
		}
	}
	// 两种「按日期归档」都会在目标根下生成时间戳快照目录，保留份数对它们都要生效，
	// 否则转移归档的快照会一直堆下去。
	if backupModeUsesSnapshots(job.Mode) && job.Keep > 0 && !dryRun && result.Snapshot != "" && !opts.deferPruneTo {
		pruned, err := m.pruneBackupSnapshots(targetRoot, job.Keep)
		if err != nil {
			result.Warning = strings.TrimSpace(result.Warning + "；清理旧快照失败：" + err.Error())
		}
		result.Pruned = pruned
	}
	return result, nil
}

// backupModeUsesSnapshots 报告这种模式是否按时间戳快照目录留档（归档与转移归档
// 都是），这类模式才需要按保留份数清理旧快照。
func backupModeUsesSnapshots(mode string) bool {
	return mode == BackupModeArchive || mode == BackupModeMoveArchive
}

// nextBackupSnapshot 生成目标目录下本次快照的路径，以及可复用的上一份快照。
func (m *Manager) nextBackupSnapshot(target string) (snapshot string, previous string, err error) {
	now := time.Now()
	name := now.Format(backupSnapshotLayout)
	candidate := filepath.Join(target, name)
	for index := 2; ; index++ {
		if _, err := os.Stat(m.rooted(candidate)); os.IsNotExist(err) {
			break
		}
		candidate = filepath.Join(target, fmt.Sprintf("%s"+backupSnapshotSuffix, name, index))
		if index > 100 {
			return "", "", errors.New("无法生成快照目录名")
		}
	}
	previous = m.latestBackupSnapshot(target, candidate)
	return candidate, previous, nil
}

// snapshotEntries 列出目标目录下所有由本插件生成的快照目录（按名字即时间排序）。
func (m *Manager) snapshotEntries(target string) []string {
	items, err := os.ReadDir(m.rooted(target))
	if err != nil {
		return nil
	}
	var snapshots []string
	for _, item := range items {
		if !item.IsDir() || !backupSnapshotPat.MatchString(item.Name()) {
			continue
		}
		snapshots = append(snapshots, filepath.Join(target, item.Name()))
	}
	sort.Strings(snapshots)
	return snapshots
}

func (m *Manager) latestBackupSnapshot(target, exclude string) string {
	snapshots := m.snapshotEntries(target)
	latest := ""
	for _, snapshot := range snapshots {
		if filepath.Clean(snapshot) == filepath.Clean(exclude) {
			continue
		}
		latest = snapshot
	}
	return latest
}

// pruneBackupSnapshots 只保留最近 keep 份快照，删除更早的。只会删除符合
// 时间戳命名规则的目录，避免误删用户自己的数据。
func (m *Manager) pruneBackupSnapshots(target string, keep int) (int, error) {
	snapshots := m.snapshotEntries(target)
	if keep <= 0 || len(snapshots) <= keep {
		return 0, nil
	}
	removed := 0
	for _, snapshot := range snapshots[:len(snapshots)-keep] {
		if err := os.RemoveAll(m.rooted(snapshot)); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func parseRsyncStats(output string) (files, bytes, deleted int64) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Number of regular files transferred:"):
			files = parseCommaInt(strings.TrimSpace(strings.TrimPrefix(line, "Number of regular files transferred:")))
		case strings.HasPrefix(line, "Total transferred file size:"):
			bytes = parseCommaInt(strings.TrimSpace(strings.TrimPrefix(line, "Total transferred file size:")))
		case strings.HasPrefix(line, "Number of deleted files:"):
			deleted = parseCommaInt(strings.TrimSpace(strings.TrimPrefix(line, "Number of deleted files:")))
		}
	}
	return files, bytes, deleted
}

func parseCommaInt(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if index := strings.IndexByte(raw, ' '); index >= 0 {
		raw = raw[:index]
	}
	raw = strings.ReplaceAll(raw, ",", "")
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	return value
}

func firstLines(text string, count int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > count {
		lines = lines[:count]
	}
	return strings.Join(lines, "；")
}
