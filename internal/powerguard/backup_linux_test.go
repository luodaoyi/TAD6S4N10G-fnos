//go:build linux

package powerguard

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 归档模式依赖系统 rsync，只能在 Linux 上做真实端到端验证。

func backupWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func backupArchiveJob() GPIOBackupJob {
	return GPIOBackupJob{
		ID: "arc", Name: "归档测试", Mode: BackupModeArchive, Keep: 2,
		Source: BackupLocation{Kind: "path", Path: "/src"},
		Target: BackupLocation{Kind: "path", Path: "/dst"},
	}
}

// 「按日期转移归档」同样在目标下堆时间戳快照，保留份数必须对它也生效，
// 否则快照一份都不会被清掉，NAS 空间一直涨。
func TestRunBackupJobMoveArchiveAlsoPrunesSnapshots(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync 未安装，跳过转移归档端到端测试")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root}
	job := backupArchiveJob()
	job.Mode = BackupModeMoveArchive

	for index := 0; index < 3; index++ {
		// 移动类会清空源，每一轮都要重新放文件进去。
		backupWrite(t, filepath.Join(root, "src", "a.txt"), strings.Repeat("y", index+1))
		if _, err := manager.RunBackupJob(context.Background(), job, false); err != nil {
			t.Fatalf("第 %d 次转移归档失败: %v", index, err)
		}
	}
	if snapshots := manager.snapshotEntries("/dst"); len(snapshots) != 2 {
		t.Fatalf("保留 2 份时应当只剩 2 份快照，实际 %d：%v", len(snapshots), snapshots)
	}
	if _, err := os.Stat(filepath.Join(root, "src", "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("转移归档应当清空源文件，实际 err=%v", err)
	}
}

// 试跑通过真实运行路径走一遍（rsync --dry-run），不该留下「上次备份」状态。
func TestRunBackupJobNowDryRunDoesNotUpdateLastResult(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync 未安装，跳过试跑端到端测试")
	}
	root := t.TempDir()
	backupWrite(t, filepath.Join(root, "src", "a.txt"), "alpha")
	if err := os.MkdirAll(filepath.Join(root, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root}
	job := backupArchiveJob()

	if _, err := manager.RunBackupJobNow(context.Background(), job, true); err != nil {
		t.Fatalf("试跑失败: %v", err)
	}
	if manager.backupRun.running {
		t.Fatal("试跑结束应当复位运行标记")
	}
	if manager.backupRun.last != nil {
		t.Fatalf("试跑不该写「上次备份」: %+v", manager.backupRun.last)
	}
	if manager.backupRun.lastError != "" {
		t.Fatalf("试跑不该写 lastError，实际 %q", manager.backupRun.lastError)
	}
	if snapshots := manager.snapshotEntries("/dst"); len(snapshots) != 0 {
		t.Fatalf("试跑不该真的建快照目录: %v", snapshots)
	}
}

func TestRunBackupJobArchiveUsesHardlinksAndKeepsCount(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync 未安装，跳过归档模式端到端测试")
	}
	root := t.TempDir()
	backupWrite(t, filepath.Join(root, "src", "a.txt"), "alpha-0")
	backupWrite(t, filepath.Join(root, "src", "sub", "b.txt"), "beta")
	if err := os.MkdirAll(filepath.Join(root, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root}
	job := backupArchiveJob()

	var snapshots []string
	for index := 0; index < 3; index++ {
		// 每次让 a.txt 的长度也变化：rsync 的快速校验按「大小 + 修改时间」判断
		// 是否需要传输，同一秒内写入且长度相同的内容会被视为未变化而跳过。
		backupWrite(t, filepath.Join(root, "src", "a.txt"), strings.Repeat("x", index+1))
		result, err := manager.RunBackupJob(context.Background(), job, false)
		if err != nil {
			t.Fatalf("第 %d 次归档失败: %v", index, err)
		}
		if result.Files == 0 {
			t.Fatalf("第 %d 次归档没有复制任何文件", index)
		}
		snapshots = manager.snapshotEntries("/dst")
	}
	// keep=2：跑三次之后只应留下最近两份快照（这一项同时守住「清理误用快照
	// 目录而非目标根」的回归——传错目录时这里会剩 3 份）。
	if len(snapshots) != 2 {
		t.Fatalf("keep=2 应保留 2 份快照，实际 %d 份: %v", len(snapshots), snapshots)
	}

	// 最后两份快照之间，内容未变的 sub/b.txt 应是硬链接（同 inode）。
	first := snapshots[len(snapshots)-2]
	second := snapshots[len(snapshots)-1]
	unchangedA, err := os.Stat(filepath.Join(root, first, "sub", "b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	unchangedB, err := os.Stat(filepath.Join(root, second, "sub", "b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(unchangedA, unchangedB) {
		t.Fatal("内容未变化的文件没有复用硬链接，硬链接增量失效")
	}
	// 内容变化的 a.txt 必须是重新复制的独立文件。
	changedA, err := os.Stat(filepath.Join(root, first, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	changedB, err := os.Stat(filepath.Join(root, second, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(changedA, changedB) {
		t.Fatal("内容已变化的文件被错误地硬链接复用")
	}
}

// 标识文件必须写进源介质、并且不被复制到备份里；换了一块内容不同的介质时
// 必须拒绝执行，这正是「防止认错盘」的核心行为。
func TestRunBackupJobMarkerGuardsWrongMedium(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync 未安装，跳过标识文件端到端测试")
	}
	root := t.TempDir()
	backupWrite(t, filepath.Join(root, "src", "a.txt"), "alpha")
	if err := os.MkdirAll(filepath.Join(root, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root}
	job := GPIOBackupJob{
		ID: "marker", Name: "标识", Mode: BackupModeCopy,
		Source: BackupLocation{Kind: "path", Path: "/src"},
		Target: BackupLocation{Kind: "path", Path: "/dst"},
	}

	marker, written, err := manager.WriteBackupMarker(job)
	if err != nil {
		t.Fatalf("写入标识文件失败: %v", err)
	}
	if written != "/src/"+BackupMetaDirName+"/"+backupMarkerFileName {
		t.Fatalf("标识文件路径不正确: %q", written)
	}
	job.Marker = marker
	// 写新位置的同时要把旧版写在源根的 .tad-backup-source 清掉，别让两处标记并存。
	backupWrite(t, filepath.Join(root, "src", backupMarkerLegacyName), "old-marker")
	if _, _, err := manager.WriteBackupMarker(job); err != nil {
		t.Fatalf("重复写入标识文件失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "src", backupMarkerLegacyName)); !os.IsNotExist(err) {
		t.Fatal("旧版标识文件应当被清理")
	}

	result, err := manager.RunBackupJob(context.Background(), job, false)
	if err != nil {
		t.Fatalf("带标识的备份应能跑通: %v", err)
	}
	if result.Files == 0 {
		t.Fatal("备份没有复制任何文件")
	}
	// 元数据目录（以及旧版两个标记名）都不该被复制进备份。
	for _, name := range []string{BackupMetaDirName, backupMarkerLegacyName, BackupAutoMarkerLegacyDirName} {
		if _, err := os.Stat(filepath.Join(root, "dst", name)); err == nil {
			t.Fatalf("元数据 %s 不该被复制进备份", name)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "dst", "a.txt")); err != nil {
		t.Fatalf("正常文件应被复制: %v", err)
	}

	// 换了一块盘：源目录里没有标识文件（或内容不同）时必须拒绝。
	wrong := t.TempDir()
	backupWrite(t, filepath.Join(wrong, "src", "b.txt"), "beta")
	if err := os.MkdirAll(filepath.Join(wrong, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	wrongManager := &Manager{Root: wrong}
	if _, err := wrongManager.RunBackupJob(context.Background(), job, false); err == nil {
		t.Fatal("缺少标识文件的介质应当被拒绝")
	}
	backupWrite(t, filepath.Join(wrong, "src", BackupMetaDirName, backupMarkerFileName), "someone-else-disk")
	if _, err := wrongManager.RunBackupJob(context.Background(), job, false); err == nil {
		t.Fatal("标识内容不一致的介质应当被拒绝")
	}

	// 旧版位置（源根的 .tad-backup-source）继续认，升级过来的盘不用重新标。
	legacy := t.TempDir()
	backupWrite(t, filepath.Join(legacy, "src", "c.txt"), "gamma")
	if err := os.MkdirAll(filepath.Join(legacy, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	backupWrite(t, filepath.Join(legacy, "src", backupMarkerLegacyName), marker)
	legacyManager := &Manager{Root: legacy}
	if _, err := legacyManager.RunBackupJob(context.Background(), job, false); err != nil {
		t.Fatalf("旧版位置的标识文件应当继续通过校验: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, "dst", backupMarkerLegacyName)); err == nil {
		t.Fatal("旧版标识文件同样不该被复制进备份")
	}
}

// 两块打标卷 + 「打标卷 → NAS」规则的公共环境：两块可移动卷、一个已存在的
// NAS 目标目录、配置落盘（打标记录要进 GPIO.AutoVolumes）。
func autoBackupTwoVolumeManager(t *testing.T) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	write := func(path, content string) {
		backupWrite(t, filepath.Join(root, filepath.FromSlash(path)), content)
	}
	mkdir := func(path string) {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(path)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("proc/mounts", `/dev/sdb2 / ext4 rw,relatime 0 0
/dev/mapper/trim_x-0 /vol1 btrfs rw,relatime 0 0
/dev/sdc1 /media/usb0 ext4 rw,relatime 0 0
/dev/sdd1 /media/usb1 ext4 rw,relatime 0 0
`)
	// 两块「USB 盘」。真实 sysfs 里 /sys/class/block/<dev> 是符号链接，测试里用
	// 普通目录模拟（EvalSymlinks 对普通目录原样返回）；分区上溯整盘的拓扑由
	// TestIsRemovableDeviceUsesSysfsSymlinkAndUsbPath 覆盖，这里直接给分区条目
	// 写 removable。
	write("sys/class/block/sdc/removable", "1\n")
	write("sys/class/block/sdc1/removable", "1\n")
	write("sys/class/block/sdd/removable", "1\n")
	write("sys/class/block/sdd1/removable", "1\n")
	mkdir("media/usb0")
	mkdir("media/usb1")
	mkdir("vol1/1000/导入")

	manager := &Manager{Root: root, ConfigPath: filepath.Join(root, "config.json")}
	if err := manager.saveConfigLocked(DefaultConfig(Profile{})); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.MarkAutoBackupVolume("/media/usb0", "盘A"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.MarkAutoBackupVolume("/media/usb1", "盘B"); err != nil {
		t.Fatal(err)
	}
	return manager, root
}

// CodeRabbit 第二轮：「打标卷 → NAS」的多块卷共用同一个 NAS 目标（rule.Fixed）。
// 逐卷清理会按「目标根下全部快照」计数，卷 B 的清理把卷 A 刚生成的快照删掉——
// move_archive 下源文件已经删了，数据就从卡上和 NAS 上同时消失。
// 修法：单卷执行不清理，全部跑完后统一清理一次，保留份数不小于本次新生成的
// 快照数；sync 模式多卷同时在线时正式运行直接拒绝。
func TestRunAutoBackupJobMultiVolumeSharedTargetKeepsSnapshots(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync 未安装，跳过自动备份端到端测试")
	}
	manager, root := autoBackupTwoVolumeManager(t)

	rule := GPIOAutoBackup{
		ID: "import", Name: "导入", Direction: BackupDirectionFromVolume,
		Fixed: BackupLocation{Kind: "path", Path: "/vol1/1000/导入"},
		Mode:  BackupModeMoveArchive, Keep: 1,
	}
	backupWrite(t, filepath.Join(root, "media", "usb0", "a.txt"), "A")
	backupWrite(t, filepath.Join(root, "media", "usb1", "b.txt"), "B")

	result, err := manager.RunAutoBackupJob(context.Background(), rule, false)
	if err != nil {
		t.Fatalf("双卷导入失败: %v", err)
	}
	target := "/vol1/1000/导入"
	// 修前的行为：卷 B 的清理把卷 A 刚生成的快照删掉，只剩 1 份——而卷 A 的
	// 源文件已被删，数据从卡上和 NAS 上同时消失。
	if snapshots := manager.snapshotEntries(target); len(snapshots) != 2 {
		t.Fatalf("两块卷共用目标时，保留份数应至少覆盖本次的 2 份快照，实际 %d: %v", len(snapshots), snapshots)
	}
	if result.Pruned != 0 {
		t.Fatalf("本次新生成 2 份快照、Keep=1，统一清理不应删掉它们: pruned=%d", result.Pruned)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		sub := "media/usb0/"
		if name == "b.txt" {
			sub = "media/usb1/"
		}
		if _, err := os.Stat(filepath.Join(root, sub, name)); !os.IsNotExist(err) {
			t.Fatalf("转移归档应清空 %s: %v", name, err)
		}
	}

	// 第二轮：目标根下已有 2 份旧快照，本轮又生成 2 份 → keep = max(1, 2) = 2，
	// 最终应只剩本轮最新的 2 份。
	backupWrite(t, filepath.Join(root, "media", "usb0", "a2.txt"), "A2")
	backupWrite(t, filepath.Join(root, "media", "usb1", "b2.txt"), "B2")
	second, err := manager.RunAutoBackupJob(context.Background(), rule, false)
	if err != nil {
		t.Fatalf("第二轮双卷导入失败: %v", err)
	}
	if snapshots := manager.snapshotEntries(target); len(snapshots) != 2 {
		t.Fatalf("第二轮后应只剩 2 份快照，实际 %d: %v", len(snapshots), snapshots)
	}
	if second.Pruned != 2 {
		t.Fatalf("第二轮应清理掉 2 份旧快照，实际 pruned=%d", second.Pruned)
	}
}

// sync 模式的 --delete 会把前一块卷刚同步进 NAS 的文件删掉：多块打标卷同时
// 在线时正式运行必须直接拒绝，且一个卷都不能动。
func TestRunAutoBackupJobRejectsSyncWithMultipleVolumes(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync 未安装，跳过自动备份端到端测试")
	}
	manager, root := autoBackupTwoVolumeManager(t)

	backupWrite(t, filepath.Join(root, "media", "usb0", "a.txt"), "A")
	backupWrite(t, filepath.Join(root, "media", "usb1", "b.txt"), "B")
	rule := GPIOAutoBackup{
		ID: "synconly", Name: "同步导入", Direction: BackupDirectionFromVolume,
		Fixed: BackupLocation{Kind: "path", Path: "/vol1/1000/导入"},
		Mode:  BackupModeSync,
	}
	if _, err := manager.RunAutoBackupJob(context.Background(), rule, false); err == nil ||
		!strings.Contains(err.Error(), "同步模式") {
		t.Fatalf("多卷 + 同步模式应被拒绝并说明原因: %v", err)
	}
	// 拒绝发生在任何卷执行之前：卷上的文件必须原样保留。
	for _, name := range []string{"media/usb0/a.txt", "media/usb1/b.txt"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(name))); err != nil {
			t.Fatalf("拒绝执行时不应动卷上的文件 %s: %v", name, err)
		}
	}
}
