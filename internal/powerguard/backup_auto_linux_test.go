//go:build linux

package powerguard

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// 「移动」类模式的行为只能靠真实 rsync 验证：删除时机（复制成功一个删一个）、
// 目录是否保留、失败时是否保留源文件，这些正是用户最关心的安全边界。

// 移动覆盖：目标已有同名文件时必须覆盖（强制重新传输，防止 rsync 按
// 「大小 + mtime」quick check 跳过传输却仍删源），源文件在传输成功后被删除，
// 空目录保留（盘还能继续用），目标里多出来的文件不动（移动不是同步）。
func TestRunBackupJobMoveOverwriteRemovesSourceFiles(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync 未安装，跳过移动模式端到端测试")
	}
	root := t.TempDir()
	backupWrite(t, filepath.Join(root, "src", "a.txt"), "new-a")
	backupWrite(t, filepath.Join(root, "src", "sub", "b.txt"), "beta")
	backupWrite(t, filepath.Join(root, "src", "locked.txt"), "will-stay")
	if err := os.MkdirAll(filepath.Join(root, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	backupWrite(t, filepath.Join(root, "dst", "a.txt"), "old-a")
	// dst 里多出来的文件不应被动（移动覆盖不是同步）。
	backupWrite(t, filepath.Join(root, "dst", "extra.txt"), "keep-me")

	manager := &Manager{Root: root}
	job := GPIOBackupJob{
		ID: "move", Name: "移动覆盖", Mode: BackupModeMoveOverwrite,
		Source: BackupLocation{Kind: "path", Path: "/src"},
		Target: BackupLocation{Kind: "path", Path: "/dst"},
	}
	result, err := manager.RunBackupJob(context.Background(), job, false)
	if err != nil {
		t.Fatalf("移动覆盖失败: %v", err)
	}
	if result.Files == 0 {
		t.Fatal("没有移动任何文件")
	}

	for _, name := range []string{"a.txt", "sub/b.txt"} {
		if _, err := os.Stat(filepath.Join(root, "src", filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Fatalf("%s 移动后应从源目录消失: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "src", "sub")); err != nil {
		t.Fatalf("源目录结构应保留（盘还能继续用）: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "dst", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new-a" {
		t.Fatalf("目标同名文件应被覆盖: %q", string(got))
	}
	if _, err := os.Stat(filepath.Join(root, "dst", "extra.txt")); err != nil {
		t.Fatalf("目标里多出来的文件不该被动: %v", err)
	}
}

// 按日期转移归档：目标下生成日期快照目录，源文件转移进去后删除。
func TestRunBackupJobMoveArchiveCreatesSnapshot(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync 未安装，跳过转移归档端到端测试")
	}
	root := t.TempDir()
	backupWrite(t, filepath.Join(root, "src", "a.txt"), "alpha")
	backupWrite(t, filepath.Join(root, "src", "sub", "b.txt"), "beta")
	if err := os.MkdirAll(filepath.Join(root, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root}
	job := GPIOBackupJob{
		ID: "transfer", Name: "转移", Mode: BackupModeMoveArchive,
		Source: BackupLocation{Kind: "path", Path: "/src"},
		Target: BackupLocation{Kind: "path", Path: "/dst"},
	}
	result, err := manager.RunBackupJob(context.Background(), job, false)
	if err != nil {
		t.Fatalf("按日期转移归档失败: %v", err)
	}
	if result.Snapshot == "" {
		t.Fatal("应生成快照目录")
	}
	snapshots := manager.snapshotEntries("/dst")
	if len(snapshots) != 1 {
		t.Fatalf("应只有一份快照: %v", snapshots)
	}
	for _, name := range []string{"a.txt", "sub/b.txt"} {
		if _, err := os.Stat(filepath.Join(root, snapshots[0], filepath.FromSlash(name))); err != nil {
			t.Fatalf("快照里应有 %s: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(root, "src", filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Fatalf("%s 转移后应从源目录消失: %v", name, err)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "src")); len(entries) != 1 || entries[0].Name() != "sub" {
		t.Fatalf("源目录应只留下空目录结构: %v", entries)
	}
}

// 自动备份端到端：假的可移动卷 + 打标 → 规则按方向落到卷上执行。
func TestRunAutoBackupJobFromMarkedVolume(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync 未安装，跳过自动备份端到端测试")
	}
	root := t.TempDir()
	// 假可移动卷：/dev/sdc1 挂在 /media/usb0，removable=1。
	backupWrite(t, filepath.Join(root, "proc", "mounts"),
		"/dev/sdb2 / ext4 rw 0 0\n/dev/sdc1 /media/usb0 vfat rw 0 0\n")
	backupWrite(t, filepath.Join(root, "sys", "block", "sdc", "removable"), "1\n")
	backupWrite(t, filepath.Join(root, "sys", "block", "sdb", "removable"), "0\n")
	if err := os.MkdirAll(filepath.Join(root, "media", "usb0", "DCIM"), 0o755); err != nil {
		t.Fatal(err)
	}
	backupWrite(t, filepath.Join(root, "media", "usb0", "DCIM", "p1.jpg"), "photo-1")
	backupWrite(t, filepath.Join(root, "media", "usb0", "DCIM", "p2.jpg"), "photo-2")
	// DCIM 之外的文件不该被碰：打标子目录之后只操作这些子目录里的内容。
	backupWrite(t, filepath.Join(root, "media", "usb0", "说明.txt"), "不该被搬走")
	if err := os.MkdirAll(filepath.Join(root, "vol1", "1000", "导入"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 内置盘上也放一套「打标」都不该被扫到：只有可移动卷参与自动备份
	// （旧版卷根文件、旧版打标目录、当前元数据目录三种格式都必须被忽略）。
	backupWrite(t, filepath.Join(root, "vol1", "1000", BackupAutoMarkerLegacyName), "fake")
	backupWrite(t, filepath.Join(root, "vol1", "电影", BackupAutoMarkerLegacyDirName, BackupAutoMarkerFileName), `{"alias":"假打标"}`)
	backupWrite(t, filepath.Join(root, "vol1", "音乐", BackupAutoMarkerDirName, BackupAutoMarkerFileName), `{"alias":"假打标2"}`)

	manager := &Manager{Root: root, ConfigPath: filepath.Join(root, "config.json")}
	if err := manager.saveConfigLocked(DefaultConfig(Profile{})); err != nil {
		t.Fatalf("准备配置失败: %v", err)
	}
	// 选卷内子目录打标 → 标识写进卷内专用目录，卷根不留文件。
	volume, sub, err := manager.AddAutoBackupSub("/media/usb0/DCIM")
	if err != nil {
		t.Fatalf("打标失败: %v", err)
	}
	if volume.MountPoint != "/media/usb0" || sub != "DCIM" {
		t.Fatalf("应定位到卷根并记下子目录: %+v %q", volume, sub)
	}
	if _, err := os.Stat(filepath.Join(root, "media", "usb0", BackupAutoMarkerDirName, BackupAutoMarkerFileName)); err != nil {
		t.Fatalf("标识应写在卷内专用目录: %v", err)
	}
	// 卷根只该有那一个统一元数据目录，标识文件本身不能直接躺在卷根。
	if info, err := os.Stat(filepath.Join(root, "media", "usb0", BackupAutoMarkerDirName)); err != nil || !info.IsDir() {
		t.Fatalf("卷根应只有元数据目录: %v %+v", err, info)
	}
	if _, err := os.Stat(filepath.Join(root, "media", "usb0", BackupAutoMarkerFileName)); !os.IsNotExist(err) {
		t.Fatalf("卷根不该直接出现标识文件: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "media", "usb0", BackupAutoMarkerLegacyDirName)); !os.IsNotExist(err) {
		t.Fatalf("不该再有第二个元数据目录: %v", err)
	}

	// from_volume 只填 NAS 目标：卷侧操作范围完全由打标决定。
	rule := GPIOAutoBackup{
		ID: "dump", Name: "相机导入", Direction: BackupDirectionFromVolume,
		Fixed: BackupLocation{Kind: "path", Path: "/vol1/1000/导入"},
		Mode:  BackupModeMoveArchive,
	}
	snapshotsBefore := len(manager.snapshotEntries("/vol1/1000/导入"))
	result, err := manager.RunAutoBackupJob(context.Background(), rule, false)
	if err != nil {
		t.Fatalf("自动备份失败: %v", err)
	}
	if result.Volumes != 1 || len(result.Results) != 1 {
		t.Fatalf("应处理 1 块打标卷: %+v", result)
	}
	if result.Files < 2 {
		t.Fatalf("应转移 2 个文件: %+v", result)
	}
	// 转移后：NAS 目标里多了一份日期快照，快照里保留 DCIM 目录名。
	entries, err := os.ReadDir(filepath.Join(root, "vol1", "1000", "导入"))
	if err != nil || len(entries) != snapshotsBefore+1 {
		t.Fatalf("NAS 目标应多一份快照: %v %+v", err, entries)
	}
	inSnapshot := filepath.Join(root, "vol1", "1000", "导入", entries[len(entries)-1].Name())
	if _, err := os.Stat(filepath.Join(inSnapshot, "DCIM", "p1.jpg")); err != nil {
		t.Fatalf("快照里应保留 DCIM 目录名: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "media", "usb0", "DCIM", "p1.jpg")); !os.IsNotExist(err) {
		t.Fatalf("转移后源文件应被删除: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "media", "usb0", "DCIM")); err != nil {
		t.Fatalf("源目录结构应保留: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "media", "usb0", "说明.txt")); err != nil {
		t.Fatalf("不在打标子目录里的文件不该被动: %v", err)
	}

	// 多子目录：把 VIDEO 也打上标，两个目录一次转移、各自保留目录名。
	if err := os.MkdirAll(filepath.Join(root, "media", "usb0", "VIDEO"), 0o755); err != nil {
		t.Fatal(err)
	}
	backupWrite(t, filepath.Join(root, "media", "usb0", "DCIM", "p1.jpg"), "photo-1")
	backupWrite(t, filepath.Join(root, "media", "usb0", "VIDEO", "v1.mp4"), "video-1")
	if _, _, err := manager.AddAutoBackupSub("/media/usb0/VIDEO"); err != nil {
		t.Fatal(err)
	}
	multi, err := manager.RunAutoBackupJob(context.Background(), rule, false)
	if err != nil {
		t.Fatalf("多子目录自动备份失败: %v", err)
	}
	if multi.Files < 2 {
		t.Fatalf("两个子目录的文件都该被转移: %+v", multi)
	}
	entries, err = os.ReadDir(filepath.Join(root, "vol1", "1000", "导入"))
	if err != nil {
		t.Fatal(err)
	}
	latest := filepath.Join(root, "vol1", "1000", "导入", entries[len(entries)-1].Name())
	for _, name := range []string{"DCIM/p1.jpg", "VIDEO/v1.mp4"} {
		if _, err := os.Stat(filepath.Join(latest, filepath.FromSlash(name))); err != nil {
			t.Fatalf("快照里应有 %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "media", "usb0", "说明.txt")); err != nil {
		t.Fatalf("卷根的文件始终不该被动: %v", err)
	}

	// 反方向：to_volume 把 NAS 内容备份进打标卷的子目录（规则里填的子目录说了算）。
	backupWrite(t, filepath.Join(root, "vol1", "1000", "照片", "a.jpg"), "nas-photo")
	toRule := GPIOAutoBackup{
		ID: "photos", Name: "照片上盘", Direction: BackupDirectionToVolume,
		Fixed: BackupLocation{Kind: "path", Path: "/vol1/1000/照片"},
		Mode:  BackupModeCopy, VolumeSub: "backup/照片",
	}
	toResult, err := manager.RunAutoBackupJob(context.Background(), toRule, false)
	if err != nil {
		t.Fatalf("to_volume 方向失败: %v", err)
	}
	if toResult.Files < 1 {
		t.Fatalf("应复制至少 1 个文件: %+v", toResult)
	}
	if _, err := os.Stat(filepath.Join(root, "media", "usb0", "backup", "照片", "a.jpg")); err != nil {
		t.Fatalf("文件应被复制进打标卷子目录: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "vol1", "1000", "照片", "a.jpg")); err != nil {
		t.Fatalf("to_volume 不应删除 NAS 源文件: %v", err)
	}

	// 试跑：不写任何东西（既不删源，也不新加快照）。
	before := len(manager.snapshotEntries("/vol1/1000/导入"))
	backupWrite(t, filepath.Join(root, "media", "usb0", "DCIM", "p1.jpg"), "photo-1")
	dry, err := manager.RunAutoBackupJob(context.Background(), rule, true)
	if err != nil {
		t.Fatalf("试跑失败: %v", err)
	}
	if dry.Files == 0 {
		t.Fatal("试跑应列出将转移的文件")
	}
	if _, err := os.Stat(filepath.Join(root, "media", "usb0", "DCIM", "p1.jpg")); err != nil {
		t.Fatal("试跑不应删除源文件")
	}
	if after := len(manager.snapshotEntries("/vol1/1000/导入")); after != before {
		t.Fatalf("试跑不该新加快照: %d → %d", before, after)
	}
}

// 自动备份的运行状态：running/last_auto 走同一个互斥，结束回执与普通任务一致。
func TestAutoBackupRunStateLifecycle(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync 未安装，跳过自动备份状态测试")
	}
	root := t.TempDir()
	backupWrite(t, filepath.Join(root, "proc", "mounts"), "/dev/sdc1 /media/usb0 vfat rw 0 0\n")
	backupWrite(t, filepath.Join(root, "sys", "block", "sdc", "removable"), "1\n")
	if err := os.MkdirAll(filepath.Join(root, "media", "usb0"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root}
	if _, err := manager.MarkAutoBackupVolume("/media/usb0", ""); err != nil {
		t.Fatal(err)
	}
	// NAS 目标不存在 → 执行失败，状态里要能看到原因。
	rule := GPIOAutoBackup{
		ID: "dump", Name: "导入", Direction: BackupDirectionFromVolume,
		Fixed: BackupLocation{Kind: "path", Path: "/vol1/1000/不存在"},
		Mode:  BackupModeCopy,
	}
	if _, err := manager.RunAutoBackupJobNow(context.Background(), rule, false); err == nil {
		t.Fatal("目标不存在应当失败")
	}
	status := manager.BackupStatus()
	if status.Running {
		t.Fatal("结束后不应仍是 running")
	}
	if status.LastError == "" {
		t.Fatal("失败原因应记录在状态里")
	}
	if status.Auto == nil {
		t.Fatal("自动备份的汇总结果应随状态下发")
	}
	if !strings.Contains(status.LastError, "/vol1/1000/不存在") {
		t.Fatalf("失败原因应指明是哪块盘: %q", status.LastError)
	}
}

// 打标写盘失败的 errno 要翻译成人话：只读挂载与空间不足是用户最常撞见的两种。
func TestAutoBackupWriteErrorMapsLinuxErrno(t *testing.T) {
	if err := autoBackupWriteError(syscall.EROFS); err == nil || !strings.Contains(err.Error(), "只读") {
		t.Fatalf("EROFS 应翻译成只读挂载: %v", err)
	}
	if err := autoBackupWriteError(syscall.ENOSPC); err == nil || !strings.Contains(err.Error(), "空间不足") {
		t.Fatalf("ENOSPC 应翻译成空间不足: %v", err)
	}
}

// 「NAS → 打标卷 + 移动类」：这条规则会删 NAS 上的源文件，所以
//
//	① 没勾选确认的规则直接拒绝；
//	② 同时插着多块打标卷时拒绝运行 —— 第一块盘搬空源之后，后面的盘只会
//	   「成功」地传 0 个文件，静默把冗余变成单点；
//	③ 试跑不删任何东西，放行以便看清计划；
//	④ 只剩一块打标卷时正常执行：文件落盘、NAS 上的源被删除。
func TestAutoBackupMoveToVolumeRequiresSingleVolume(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync 未安装，跳过移动类方向测试")
	}
	root := t.TempDir()
	backupWrite(t, filepath.Join(root, "proc", "mounts"),
		"/dev/sdc1 /media/usb0 vfat rw 0 0\n/dev/sdd1 /media/usb1 vfat rw 0 0\n")
	backupWrite(t, filepath.Join(root, "sys", "block", "sdc", "removable"), "1\n")
	backupWrite(t, filepath.Join(root, "sys", "block", "sdd", "removable"), "1\n")
	for _, mount := range []string{"usb0", "usb1"} {
		if err := os.MkdirAll(filepath.Join(root, "media", mount), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "vol1", "1000", "照片"), 0o755); err != nil {
		t.Fatal(err)
	}
	backupWrite(t, filepath.Join(root, "vol1", "1000", "照片", "a.jpg"), "nas-photo")
	backupWrite(t, filepath.Join(root, "vol1", "1000", "照片", "b.jpg"), "nas-photo-2")

	manager := &Manager{Root: root}
	for _, mount := range []string{"/media/usb0", "/media/usb1"} {
		if _, err := manager.MarkAutoBackupVolume(mount, ""); err != nil {
			t.Fatalf("打标失败: %v", err)
		}
	}

	rule := GPIOAutoBackup{
		ID: "move", Name: "搬到盘上", Direction: BackupDirectionToVolume,
		Fixed: BackupLocation{Kind: "path", Path: "/vol1/1000/照片"},
		Mode:  BackupModeMoveOverwrite, VolumeSub: "照片", AllowMoveToVolume: true,
	}

	// ① 没有确认标记（老前端 / 手改配置）时连校验都过不了
	unconfirmed := rule
	unconfirmed.AllowMoveToVolume = false
	if _, err := manager.RunAutoBackupJob(context.Background(), unconfirmed, false); err == nil {
		t.Fatal("未勾选确认的 to_volume 移动规则应当被拒绝")
	}

	// ② 两块打标卷同时在线 → 正式运行被拒绝，且一个文件都不动
	if _, err := manager.RunAutoBackupJob(context.Background(), rule, false); err == nil ||
		!strings.Contains(err.Error(), "只能作用于一块盘") {
		t.Fatalf("多块打标卷时应当拒绝运行: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "vol1", "1000", "照片", "a.jpg")); err != nil {
		t.Fatalf("被拒绝时绝不该动 NAS 上的源文件: %v", err)
	}

	// ③ 试跑不删东西：放行并列计划
	dry, err := manager.RunAutoBackupJob(context.Background(), rule, true)
	if err != nil || dry.Files == 0 {
		t.Fatalf("试跑应当放行并列出计划: %+v %v", dry, err)
	}
	if _, err := os.Stat(filepath.Join(root, "media", "usb0", "照片", "a.jpg")); !os.IsNotExist(err) {
		t.Fatalf("试跑不该真的搬文件: %v", err)
	}

	// ④ 拔掉一块盘（取消它的打标）→ 正常执行：文件落到唯一那块盘，NAS 源被删除
	if _, err := manager.ClearAutoBackupMark("/media/usb1"); err != nil {
		t.Fatalf("取消打标失败: %v", err)
	}
	result, err := manager.RunAutoBackupJob(context.Background(), rule, false)
	if err != nil {
		t.Fatalf("只剩一块打标卷时应能运行: %v", err)
	}
	if result.Volumes != 1 || result.Files < 2 {
		t.Fatalf("应把两个文件搬到唯一的一块盘上: %+v", result)
	}
	for _, name := range []string{"a.jpg", "b.jpg"} {
		if _, err := os.Stat(filepath.Join(root, "media", "usb0", "照片", name)); err != nil {
			t.Fatalf("文件应落到打标卷: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, "vol1", "1000", "照片", name)); !os.IsNotExist(err) {
			t.Fatalf("移动模式应当删除 NAS 上的源文件 %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "vol1", "1000", "照片")); err != nil {
		t.Fatalf("NAS 上应保留空目录结构: %v", err)
	}
}
