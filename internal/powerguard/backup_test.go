package powerguard

import (
	"os"
	"path/filepath"
	"testing"
)

func backupTestConfig(t *testing.T) GPIOConfig {
	t.Helper()
	config := DefaultConfig(profiles[1]).GPIO
	config.Backups = []GPIOBackupJob{{
		ID:     "usb-photos",
		Name:   "U 盘照片备份",
		Source: BackupLocation{Kind: "volume", Label: "PHOTOS", Sub: "DCIM"},
		Target: BackupLocation{Kind: "path", Path: "/vol1/backup/photos"},
		Mode:   BackupModeArchive,
		Keep:   5,
	}}
	return config
}

func TestGPIOBackupActionBinds(t *testing.T) {
	config := backupTestConfig(t)
	config.Buttons[0].Actions.Short = GPIOActionBackupPrefix + "usb-photos"
	if err := validateGPIOConfig(config); err != nil {
		t.Fatalf("绑定备份任务的按键配置被拒绝: %v", err)
	}
	job := gpioBackupForAction(config, GPIOActionBackupPrefix+"usb-photos")
	if job == nil || job.Name != "U 盘照片备份" {
		t.Fatalf("未能按动作取回备份任务: %+v", job)
	}
	if display := gpioActionDisplay(config, config.Buttons[0].Actions.Short); display != "备份：U 盘照片备份" {
		t.Fatalf("动作显示名不正确: %q", display)
	}
}

func TestGPIOBackupActionRejectsMissingJob(t *testing.T) {
	config := backupTestConfig(t)
	config.Buttons[0].Actions.Short = GPIOActionBackupPrefix + "not-exists"
	if err := validateGPIOConfig(config); err == nil {
		t.Fatal("引用不存在的备份任务应当被拒绝")
	}
}

func TestValidateGPIOBackupJobRules(t *testing.T) {
	cases := []struct {
		name string
		job  GPIOBackupJob
	}{
		{"名称缺失", GPIOBackupJob{ID: "ok", Mode: BackupModeCopy, Source: BackupLocation{Kind: "path", Path: "/a"}, Target: BackupLocation{Kind: "path", Path: "/b"}}},
		{"模式非法", GPIOBackupJob{ID: "ok", Name: "n", Mode: "move", Source: BackupLocation{Kind: "path", Path: "/a"}, Target: BackupLocation{Kind: "path", Path: "/b"}}},
		{"源缺失", GPIOBackupJob{ID: "ok", Name: "n", Mode: BackupModeCopy, Target: BackupLocation{Kind: "path", Path: "/b"}}},
		{"源非绝对路径", GPIOBackupJob{ID: "ok", Name: "n", Mode: BackupModeCopy, Source: BackupLocation{Kind: "path", Path: "rel/path"}, Target: BackupLocation{Kind: "path", Path: "/b"}}},
		{"源含..", GPIOBackupJob{ID: "ok", Name: "n", Mode: BackupModeCopy, Source: BackupLocation{Kind: "path", Path: "/a/../b"}, Target: BackupLocation{Kind: "path", Path: "/b"}}},
		{"卷标识缺失", GPIOBackupJob{ID: "ok", Name: "n", Mode: BackupModeCopy, Source: BackupLocation{Kind: "volume"}, Target: BackupLocation{Kind: "path", Path: "/b"}}},
		{"卷子目录为绝对路径", GPIOBackupJob{ID: "ok", Name: "n", Mode: BackupModeCopy, Source: BackupLocation{Kind: "volume", Label: "x", Sub: "/sub"}, Target: BackupLocation{Kind: "path", Path: "/b"}}},
	}
	for _, test := range cases {
		if err := validateGPIOBackupJob(test.job); err == nil {
			t.Fatalf("%s：应当被拒绝", test.name)
		}
	}
	valid := GPIOBackupJob{
		ID: "job-1", Name: "有效任务", Mode: BackupModeArchive, Keep: 3,
		Source: BackupLocation{Kind: "volume", UUID: "2335-7ACB", Sub: "photos"},
		Target: BackupLocation{Kind: "path", Path: "/vol1/backup"},
	}
	if err := validateGPIOBackupJob(valid); err != nil {
		t.Fatalf("合法任务被拒绝: %v", err)
	}
}

func TestBackupPathGuardRejectsDangerousTargets(t *testing.T) {
	cases := []struct {
		name string
		src  string
		dst  string
	}{
		{"目标等于源", "/vol1/photos", "/vol1/photos"},
		{"目标在源内", "/vol1/photos", "/vol1/photos/sub"},
		{"源在目标内", "/vol1/photos/sub", "/vol1/photos"},
		{"目标是系统根", "/vol1/photos", "/"},
		{"目标是卷根", "/vol1/photos", "/vol1"},
		{"目标是系统目录", "/vol1/photos", "/usr"},
		{"目标是整个我的文件", "/vol1/photos", "/vol1/1000"},
		{"源是系统根", "/", "/vol1/backup"},
	}
	for _, test := range cases {
		if err := backupPathGuard(test.src, test.dst); err == nil {
			t.Fatalf("%s：应当被拒绝", test.name)
		}
	}
	if err := backupPathGuard("/vol1/photos", "/vol1/backup/photos"); err != nil {
		t.Fatalf("正常路径被拒绝: %v", err)
	}
	if err := backupPathGuard("/vol1/1000/photos", "/vol1/backup"); err != nil {
		t.Fatalf("以「我的文件」下的目录作为源应当被允许: %v", err)
	}
	// 整块盘/整个 U 盘一起备份是常见需求，源允许是卷根。
	if err := backupPathGuard("/vol1", "/vol2/backup"); err != nil {
		t.Fatalf("以整个卷作为源应当被允许: %v", err)
	}
}

func TestParseRsyncStatsReadsCounts(t *testing.T) {
	output := `
Number of files: 1,234 (reg: 1,000, dir: 234)
Number of regular files transferred: 56
Total file size: 5,678,901 bytes
Total transferred file size: 123,456 bytes
Number of deleted files: 7
`
	files, bytes, deleted := parseRsyncStats(output)
	if files != 56 || bytes != 123456 || deleted != 7 {
		t.Fatalf("统计解析错误: files=%d bytes=%d deleted=%d", files, bytes, deleted)
	}
}

func TestPruneBackupSnapshotsKeepsNewest(t *testing.T) {
	root := t.TempDir()
	target := "/backup"
	directory := filepath.Join(root, "backup")
	for _, name := range []string{
		"2026-10-10_010101", "2026-10-11_010101", "2026-10-12_010101", "user-notes",
	} {
		if err := os.MkdirAll(filepath.Join(directory, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 目标根上的元数据目录同样不能被当成旧快照清掉。
	if err := os.MkdirAll(filepath.Join(directory, BackupMetaDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root}
	pruned, err := manager.pruneBackupSnapshots(target, 1)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 2 {
		t.Fatalf("应当清理 2 份旧快照，实际 %d", pruned)
	}
	remaining := manager.snapshotEntries(target)
	if len(remaining) != 1 || filepath.Base(remaining[0]) != "2026-10-12_010101" {
		t.Fatalf("保留的快照不正确: %v", remaining)
	}
	if _, err := os.Stat(filepath.Join(directory, "user-notes")); err != nil {
		t.Fatal("非快照目录不能被删除")
	}
	if _, err := os.Stat(filepath.Join(directory, BackupMetaDirName)); err != nil {
		t.Fatal("元数据目录不能被删除")
	}
}

func TestPruneBackupSnapshotsUnlimitedKeepsAll(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "backup")
	if err := os.MkdirAll(filepath.Join(directory, "2026-10-10_010101"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root}
	pruned, err := manager.pruneBackupSnapshots("/backup", 0)
	if err != nil || pruned != 0 {
		t.Fatalf("保留份数为 0 时不应清理: pruned=%d err=%v", pruned, err)
	}
}

// 两种「按日期归档」都会在目标下生成时间戳快照，保留份数必须都算数：
// 只认 archive 的话，转移归档的快照会一直堆下去。
func TestBackupModeUsesSnapshots(t *testing.T) {
	cases := map[string]bool{
		BackupModeArchive:       true,
		BackupModeMoveArchive:   true,
		BackupModeCopy:          false,
		BackupModeSync:          false,
		BackupModeMoveOverwrite: false,
	}
	for mode, want := range cases {
		if got := backupModeUsesSnapshots(mode); got != want {
			t.Fatalf("%s：期望 %v，实际 %v", mode, want, got)
		}
	}
}

// 试跑没有真的写过任何文件，因此不该覆盖「上次备份」结果，也不该播放蜂鸣回执。
func TestFinishBackupRunDryRunKeepsLastResult(t *testing.T) {
	manager := &Manager{}
	previous := BackupResult{JobName: "昨晚的备份", FinishedAt: "2026-10-14T03:00:00+08:00"}
	manager.backupRun.last = &previous
	manager.backupRun.running = true

	dry := BackupResult{JobName: "试跑"}
	manager.finishBackupRun("试跑", &dry, nil, nil, true)

	if manager.backupRun.running {
		t.Fatal("试跑结束应当复位运行标记")
	}
	if manager.backupRun.last == nil || manager.backupRun.last.JobName != "昨晚的备份" {
		t.Fatalf("试跑不该顶掉上次结果: %+v", manager.backupRun.last)
	}
	if manager.backupRun.lastError != "" {
		t.Fatalf("试跑不该写 lastError，实际 %q", manager.backupRun.lastError)
	}

	// 正式跑一次：这次才该更新「上次备份」。
	manager.backupRun.running = true
	real := BackupResult{JobName: "正式任务"}
	manager.finishBackupRun("正式任务", &real, nil, nil, false)
	if manager.backupRun.last == nil || manager.backupRun.last.JobName != "正式任务" {
		t.Fatalf("正式运行应当更新上次结果: %+v", manager.backupRun.last)
	}
	if manager.backupRun.running {
		t.Fatal("正式运行结束同样要复位运行标记")
	}
}

func TestNextBackupSnapshotAvoidsCollision(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{Root: root}
	first, previous, err := manager.nextBackupSnapshot("/backup")
	if err != nil {
		t.Fatal(err)
	}
	if previous != "" {
		t.Fatalf("首次快照不应有 link-dest 目标: %q", previous)
	}
	if !backupSnapshotPat.MatchString(filepath.Base(first)) {
		t.Fatalf("快照目录名不符合时间戳规范: %q", first)
	}
	if err := os.MkdirAll(root+filepath.FromSlash(first), 0o755); err != nil {
		t.Fatal(err)
	}
	second, previous, err := manager.nextBackupSnapshot("/backup")
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("同名快照已被占用，应当自动换名")
	}
	if previous != first {
		t.Fatalf("上一份快照应为 %q，实际 %q", first, previous)
	}
}

func TestCheckBackupMarker(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "usb")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkBackupMarker(source, ""); err != nil {
		t.Fatalf("未配置标识时不应校验: %v", err)
	}
	if err := checkBackupMarker(source, "photo-disk"); err == nil {
		t.Fatal("缺少标识文件时应报错")
	}
	if err := os.MkdirAll(filepath.Join(source, BackupMetaDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, BackupMetaDirName, backupMarkerFileName), []byte("photo-disk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkBackupMarker(source, "photo-disk"); err != nil {
		t.Fatalf("标识内容一致时应当通过: %v", err)
	}
	if err := checkBackupMarker(source, "other"); err == nil {
		t.Fatal("标识内容不一致时应报错")
	}
	// 旧版写在源根的 .tad-backup-source 继续认（升级上来的盘不用重新标）。
	legacy := t.TempDir()
	if err := os.WriteFile(filepath.Join(legacy, backupMarkerLegacyName), []byte("photo-disk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkBackupMarker(legacy, "photo-disk"); err != nil {
		t.Fatalf("旧版位置的标识文件应当继续通过校验: %v", err)
	}
	if err := checkBackupMarker(legacy, "other"); err == nil {
		t.Fatal("旧版位置内容不一致时同样应报错")
	}
}

func TestBackupLocationSummary(t *testing.T) {
	volume := BackupLocation{Kind: "volume", Label: "PHOTOS", Sub: "DCIM"}
	if got := volume.summary(); got != "卷 PHOTOS/DCIM" {
		t.Fatalf("卷摘要不正确: %q", got)
	}
	path := BackupLocation{Kind: "path", Path: "/vol1/backup"}
	if got := path.summary(); got != "/vol1/backup" {
		t.Fatalf("路径摘要不正确: %q", got)
	}
}
