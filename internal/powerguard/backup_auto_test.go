package powerguard

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// autoBackupFakeManager 打标场景的假宿主：一块可移动卷（/dev/sdc1 → /media/usb0）
// 加上一份真实落盘的配置——打标记录要写进 GPIO.AutoVolumes，没有 ConfigPath
// 就只测得到「卷上的标记」，测不到离线列表与离线取消打标。
func autoBackupFakeManager(t *testing.T) *Manager {
	t.Helper()
	manager := backupPlacesFakeManager(t)
	manager.ConfigPath = filepath.Join(manager.Root, "config.json")
	if err := manager.saveConfigLocked(DefaultConfig(Profile{})); err != nil {
		t.Fatalf("准备配置失败: %v", err)
	}
	for _, dir := range []string{"media/usb0/DCIM", "media/usb0/VIDEO"} {
		if err := os.MkdirAll(filepath.Join(manager.Root, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return manager
}

// autoBackupWrite 往假根里写文件（假根版本的 backupWrite 在 linux-only 文件里）。
func autoBackupWrite(t *testing.T, manager *Manager, path, content string) {
	t.Helper()
	full := filepath.Join(manager.Root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// autoBackupDevice 在打标列表里按设备标识找一行，找不到就报错。
func autoBackupDevice(t *testing.T, manager *Manager, identity string) AutoBackupDevice {
	t.Helper()
	for _, device := range manager.AutoBackupDevices() {
		if device.Identity == identity {
			return device
		}
	}
	t.Fatalf("列表里没有设备 %q: %+v", identity, manager.AutoBackupDevices())
	return AutoBackupDevice{}
}

// 自动备份规则的校验：方向与复制模式的组合是安全边界，钉死。
func TestValidateGPIOAutoBackup(t *testing.T) {
	base := GPIOAutoBackup{
		ID: "photos", Name: "照片导入", Direction: BackupDirectionFromVolume,
		Fixed: BackupLocation{Kind: "path", Path: "/vol1/1000/导入"},
		Mode:  BackupModeCopy,
	}
	if err := validateGPIOAutoBackup(base); err != nil {
		t.Fatalf("合法规则不应报错: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*GPIOAutoBackup)
	}{
		{"方向缺失", func(r *GPIOAutoBackup) { r.Direction = "" }},
		{"方向未知", func(r *GPIOAutoBackup) { r.Direction = "sideways" }},
		{"模式未知", func(r *GPIOAutoBackup) { r.Mode = "teleport" }},
		{"移动类模式用于 to_volume 需要显式确认", func(r *GPIOAutoBackup) {
			r.Direction = BackupDirectionToVolume
			r.Mode = BackupModeMoveArchive
		}},
		{"移动覆盖同样需要确认", func(r *GPIOAutoBackup) {
			r.Direction = BackupDirectionToVolume
			r.Mode = BackupModeMoveOverwrite
		}},
		{"卷内子目录不能是绝对路径", func(r *GPIOAutoBackup) { r.VolumeSub = "/abs" }},
		{"卷内子目录不能包含 ..", func(r *GPIOAutoBackup) { r.VolumeSub = "a/../b" }},
		{"保留份数越界", func(r *GPIOAutoBackup) { r.Keep = 1001 }},
		{"NAS 侧位置缺失", func(r *GPIOAutoBackup) { r.Fixed = BackupLocation{} }},
		{"名称为空", func(r *GPIOAutoBackup) { r.Name = "  " }},
	}
	for _, test := range cases {
		rule := base
		test.mutate(&rule)
		if err := validateGPIOAutoBackup(rule); err == nil {
			t.Fatalf("%s：应当被拒绝", test.name)
		}
	}

	// 移动类模式在 from_volume 方向合法；归档保留份数只对归档类有意义但两者都放行。
	for _, mode := range []string{BackupModeArchive, BackupModeMoveOverwrite, BackupModeMoveArchive} {
		rule := base
		rule.Mode = mode
		if err := validateGPIOAutoBackup(rule); err != nil {
			t.Fatalf("from_volume + %s 应当合法: %v", mode, err)
		}
	}

	// to_volume + 移动类：这个组合会删除 NAS 上的源文件，必须逐条勾选确认才放行
	// （按键触发时没有界面能弹窗，所以确认发生在保存规则的时候）。
	for _, mode := range []string{BackupModeMoveOverwrite, BackupModeMoveArchive} {
		rule := base
		rule.Direction = BackupDirectionToVolume
		rule.Mode = mode
		if err := validateGPIOAutoBackup(rule); err == nil {
			t.Fatalf("未确认的 to_volume + %s 应当被拒绝", mode)
		}
		rule.AllowMoveToVolume = true
		if err := validateGPIOAutoBackup(rule); err != nil {
			t.Fatalf("勾选确认后的 to_volume + %s 应当合法: %v", mode, err)
		}
	}
	// 确认标记不会让别的组合变非法：非移动模式多带一个字段只是冗余。
	extra := base
	extra.Direction = BackupDirectionToVolume
	extra.Mode = BackupModeSync
	extra.VolumeSub = "照片"
	extra.AllowMoveToVolume = true
	if err := validateGPIOAutoBackup(extra); err != nil {
		t.Fatalf("to_volume + 同步 + 多余的确认标记应当合法: %v", err)
	}
}

// 普通备份任务与自动备份一致，也开放移动类模式（v0.10.29 起）。
func TestValidateGPIOBackupJobAllowsMoveModes(t *testing.T) {
	job := GPIOBackupJob{
		ID: "job", Name: "普通任务", Mode: BackupModeMoveOverwrite,
		Source: BackupLocation{Kind: "path", Path: "/src"},
		Target: BackupLocation{Kind: "path", Path: "/dst"},
	}
	if err := validateGPIOBackupJob(job); err != nil {
		t.Fatalf("普通备份任务应当允许移动覆盖：%v", err)
	}
	job.Mode = BackupModeMoveArchive
	if err := validateGPIOBackupJob(job); err != nil {
		t.Fatalf("普通备份任务应当允许按日期转移归档：%v", err)
	}
}

// 移动类模式的源防误删闸：可移动卷根放行（整盘导入是正当用法），
// 整个存储卷 / 位置根目录 / 系统目录拒绝，位置内部的子目录放行。
func TestMoveSourceGuard(t *testing.T) {
	manager := backupPlacesFakeManager(t)

	for _, src := range []string{
		"/media/usb0",        // 可移动卷根
		"/vol1/1000/照片",      // 「我的文件」内部的子目录
		"/vol1/1000/照片/2026", // 更深的子目录
	} {
		if err := manager.moveSourceGuard(src); err != nil {
			t.Fatalf("源 %s 应当允许移动：%v", src, err)
		}
	}

	for _, src := range []string{
		"/vol1/1000", // 整个「我的文件」根
		"/vol1/电影",   // 整个「团队文件」根
		"/vol1",      // 整个存储卷
		"/etc",       // 系统目录
	} {
		if err := manager.moveSourceGuard(src); err == nil {
			t.Fatalf("源 %s 应当拒绝移动", src)
		}
	}
}

// 规则落到具体打标卷：方向决定源与目标，卷内子目录生效。
func TestAutoBackupJobForVolume(t *testing.T) {
	volume := AutoBackupVolume{
		Device: "/dev/sdc1", MountPoint: "/media/usb0", Label: "旅行盘",
	}
	toVolume := GPIOAutoBackup{
		ID: "photos", Name: "照片", Direction: BackupDirectionToVolume,
		Fixed: BackupLocation{Kind: "path", Path: "/vol1/1000/照片"},
		Mode:  BackupModeArchive, VolumeSub: "backup/照片", Keep: 3,
	}
	job, err := autoBackupJobForVolume(toVolume, volume)
	if err != nil {
		t.Fatal(err)
	}
	if job.Source.Path != "/vol1/1000/照片" {
		t.Fatalf("to_volume 的源应是 NAS 目录: %+v", job.Source)
	}
	if job.Target.Path != "/media/usb0/backup/照片" {
		t.Fatalf("to_volume 的目标应是卷内子目录: %+v", job.Target)
	}
	if job.Keep != 3 || job.Mode != BackupModeArchive {
		t.Fatalf("模式与保留份数应原样传递: %+v", job)
	}

	fromVolume := GPIOAutoBackup{
		ID: "dump", Name: "导入", Direction: BackupDirectionFromVolume,
		Fixed: BackupLocation{Kind: "path", Path: "/vol1/1000/导入"},
		Mode:  BackupModeMoveArchive,
	}
	job, err = autoBackupJobForVolume(fromVolume, volume)
	if err != nil {
		t.Fatal(err)
	}
	if job.Source.Path != "/media/usb0" {
		t.Fatalf("from_volume 的源应是卷根: %+v", job.Source)
	}
	if job.Target.Path != "/vol1/1000/导入" {
		t.Fatalf("from_volume 的目标应是 NAS 目录: %+v", job.Target)
	}

	// 同步模式指向整块盘的根目录等于清空整块盘，必须拒绝。
	toVolume.Mode = BackupModeSync
	toVolume.VolumeSub = ""
	if _, err := autoBackupJobForVolume(toVolume, volume); err == nil {
		t.Fatal("同步到打标卷根目录应当被拒绝")
	}

	// 打了子目录标的卷：每个子目录一个源、保留目录名（NAS 上是 目标/DCIM/…），
	// 其余的走 ExtraSources，仍然是同一次 rsync（归档同快照、移动只删这些源）。
	subVolume := volume
	subVolume.Subs = []string{"DCIM", "VIDEO"}
	job, err = autoBackupJobForVolume(fromVolume, subVolume)
	if err != nil {
		t.Fatal(err)
	}
	if job.Source.Path != "/media/usb0/DCIM" || job.Target.Path != "/vol1/1000/导入" {
		t.Fatalf("第一个子目录应是主源: %+v", job)
	}
	if !job.KeepSourceNames {
		t.Fatal("打标子目录要保留目录名，否则 NAS 上会糊成一团")
	}
	if len(job.ExtraSources) != 1 || job.ExtraSources[0].Path != "/media/usb0/VIDEO" {
		t.Fatalf("其余子目录应进额外源: %+v", job.ExtraSources)
	}

	// 老配置里手填的卷内子目录优先于打标标记，且保持老语义（不加尾斜杠 = 内容铺进目标）。
	legacyRule := fromVolume
	legacyRule.VolumeSub = "OLD"
	job, err = autoBackupJobForVolume(legacyRule, subVolume)
	if err != nil {
		t.Fatal(err)
	}
	if job.Source.Path != "/media/usb0/OLD" || len(job.ExtraSources) != 0 || job.KeepSourceNames {
		t.Fatalf("手填的卷内子目录应保持老语义: %+v", job)
	}

	// 没打子目录标 = 整块卷：保持「把卷里的内容铺进 NAS 目标」的老语义。
	plain := volume
	job, err = autoBackupJobForVolume(fromVolume, plain)
	if err != nil {
		t.Fatal(err)
	}
	if job.Source.Path != "/media/usb0" || job.KeepSourceNames || len(job.ExtraSources) != 0 {
		t.Fatalf("整块卷应保持老语义: %+v", job)
	}
}

// 打标：标识文件写进卷内的统一元数据目录（.tad-backup/mark.json），选卷内子目录也
// 只用来认定这块盘；内置盘与系统目录一律拒绝。
func TestMarkAutoBackupVolumeWritesMarkerFile(t *testing.T) {
	manager := autoBackupFakeManager(t)

	volume, err := manager.MarkAutoBackupVolume("/media/usb0/DCIM", "相机 SD 卡")
	if err != nil {
		t.Fatalf("给卷内子目录打标失败: %v", err)
	}
	if volume.MountPoint != "/media/usb0" {
		t.Fatalf("打标应定位到卷根: %+v", volume)
	}
	mountRoot := filepath.Join(manager.Root, "media", "usb0")
	markerDir := filepath.Join(mountRoot, BackupAutoMarkerDirName)
	if info, err := os.Stat(markerDir); err != nil || !info.IsDir() {
		t.Fatalf("卷根应只有一个打标目录，而不是散落的文件: %v %+v", err, info)
	}
	if _, err := os.Stat(filepath.Join(mountRoot, BackupAutoMarkerLegacyDirName)); !os.IsNotExist(err) {
		t.Fatalf("不该再出现 .tad-auto-backup 这类旧目录: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mountRoot, BackupAutoMarkerFileName)); !os.IsNotExist(err) {
		t.Fatalf("标识文件不该直接躺在卷根: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(markerDir, BackupAutoMarkerFileName))
	if err != nil {
		t.Fatalf("标识应写在 %s/%s: %v", BackupAutoMarkerDirName, BackupAutoMarkerFileName, err)
	}
	var mark AutoBackupMark
	if err := json.Unmarshal(data, &mark); err != nil {
		t.Fatalf("标识文件应是 JSON: %v（%q）", err, string(data))
	}
	if mark.Alias != "相机 SD 卡" {
		t.Fatalf("别名没写进标识: %+v", mark)
	}
	if len(mark.Subs) != 0 {
		t.Fatalf("整卷打标不该带子目录: %+v", mark.Subs)
	}
	if mark.MarkedAt == "" {
		t.Fatal("应记录打标时间")
	}

	marked := manager.markedAutoBackupVolumes()
	if len(marked) != 1 || marked[0].MountPoint != "/media/usb0" {
		t.Fatalf("打标后应能扫到该卷: %+v", marked)
	}
	if marked[0].Alias != "相机 SD 卡" || len(marked[0].Subs) != 0 || marked[0].Legacy {
		t.Fatalf("扫到的打标信息不正确: %+v", marked[0])
	}
	if !strings.Contains(marked[0].displayName(), "相机 SD 卡") {
		t.Fatalf("显示名应优先用别名: %q", marked[0].displayName())
	}

	// 内置盘 / 系统目录 / 空路径都不能打标。
	for _, path := range []string{"/vol1/1000/照片", "/etc", ""} {
		if _, err := manager.MarkAutoBackupVolume(path, ""); err == nil {
			t.Fatalf("给 %q 打标应当被拒绝", path)
		}
	}

	// 取消打标：打标目录被删掉（里面没有别的东西时目录本身也收走），扫描结果随之清空。
	if _, err := manager.ClearAutoBackupMark("/media/usb0"); err != nil {
		t.Fatalf("取消打标失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(markerDir, BackupAutoMarkerFileName)); !os.IsNotExist(err) {
		t.Fatalf("取消打标应删除打标文件: %v", err)
	}
	if _, err := os.Stat(markerDir); !os.IsNotExist(err) {
		t.Fatalf("元数据目录里没别的东西时应一并收走: %v", err)
	}
	if got := manager.markedAutoBackupVolumes(); len(got) != 0 {
		t.Fatalf("取消打标后不应再扫到卷: %+v", got)
	}
}

// 别名校验：长度上限与非法字符，没打标的盘不能设别名；改完立刻生效并落回卷里。
func TestAutoBackupAliasRules(t *testing.T) {
	manager := autoBackupFakeManager(t)

	if _, err := manager.MarkAutoBackupVolume("/media/usb0", strings.Repeat("替", autoBackupAliasMaxLen+1)); err == nil {
		t.Fatal("超长别名应当被拒绝")
	}
	if _, err := manager.MarkAutoBackupVolume("/media/usb0", "两\n行"); err == nil {
		t.Fatal("含换行的别名应当被拒绝")
	}
	if _, err := manager.SetAutoBackupAlias("/media/usb0", "还没打标"); err == nil {
		t.Fatal("没打标的盘不能设别名")
	}

	if _, err := manager.MarkAutoBackupVolume("/media/usb0", "旅行盘"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetAutoBackupAlias("/media/usb0/DCIM", "相机卡"); err != nil {
		t.Fatalf("改别名失败: %v", err)
	}
	if got := manager.markedAutoBackupVolumes(); got[0].Alias != "相机卡" {
		t.Fatalf("别名没更新: %+v", got[0])
	}
	// 只改别名不该动子目录列表。
	if _, _, err := manager.AddAutoBackupSub("/media/usb0/DCIM"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetAutoBackupAlias("/media/usb0", "备用盘"); err != nil {
		t.Fatal(err)
	}
	if got := manager.markedAutoBackupVolumes()[0]; got.Alias != "备用盘" || len(got.Subs) != 1 {
		t.Fatalf("改别名不该动子目录: %+v", got)
	}
}

// 子目录打标：可以累积多个、重复打标不重复累积、逐个取消；选中卷根要提示改用整卷打标。
func TestAutoBackupSubMarking(t *testing.T) {
	manager := autoBackupFakeManager(t)

	if _, _, err := manager.AddAutoBackupSub("/media/usb0"); err == nil {
		t.Fatal("选中卷根时应提示改用「给整块卷打标」")
	}
	if _, _, err := manager.AddAutoBackupSub("/media/usb0/不存在的目录"); err == nil {
		t.Fatal("卷内不存在的目录应当被拒绝")
	}
	if _, _, err := manager.RemoveAutoBackupSub("/media/usb0/DCIM"); err == nil {
		t.Fatal("还没打标就取消子目录应当报错")
	}

	if _, sub, err := manager.AddAutoBackupSub("/media/usb0/DCIM"); err != nil || sub != "DCIM" {
		t.Fatalf("给 DCIM 打标失败: %q %v", sub, err)
	}
	if _, sub, err := manager.AddAutoBackupSub("/media/usb0/VIDEO"); err != nil || sub != "VIDEO" {
		t.Fatalf("给 VIDEO 打标失败: %q %v", sub, err)
	}
	if _, _, err := manager.AddAutoBackupSub("/media/usb0/DCIM"); err != nil {
		t.Fatalf("重复打标同一目录不该失败: %v", err)
	}

	marked := manager.markedAutoBackupVolumes()
	if len(marked) != 1 {
		t.Fatalf("应扫到 1 块打标卷: %+v", marked)
	}
	if got := marked[0].Subs; len(got) != 2 || got[0] != "DCIM" || got[1] != "VIDEO" {
		t.Fatalf("子目录应累积并排序: %v", got)
	}

	if _, sub, err := manager.RemoveAutoBackupSub("/media/usb0/DCIM"); err != nil || sub != "DCIM" {
		t.Fatalf("取消 DCIM 打标失败: %q %v", sub, err)
	}
	if got := manager.markedAutoBackupVolumes()[0].Subs; len(got) != 1 || got[0] != "VIDEO" {
		t.Fatalf("取消后应只剩 VIDEO: %v", got)
	}
	if _, _, err := manager.RemoveAutoBackupSub("/media/usb0/VIDEO"); err != nil {
		t.Fatal(err)
	}
	// 子目录全部取消后回到「整块卷」，但标记本身还在。
	after := manager.markedAutoBackupVolumes()
	if len(after) != 1 || len(after[0].Subs) != 0 {
		t.Fatalf("子目录清空后应回到整块卷: %+v", after)
	}
}

// 目录被删掉或改名之后，「×」还得能取消打标：否则那个按钮永远报「卷内目录不存在」，
// 每次自动备份还会因为附加源不存在而失败，标记却怎么都清不掉。
func TestRemoveAutoBackupSubWorksAfterDirectoryDeleted(t *testing.T) {
	manager := autoBackupFakeManager(t)
	if _, sub, err := manager.AddAutoBackupSub("/media/usb0/DCIM"); err != nil || sub != "DCIM" {
		t.Fatalf("给 DCIM 打标失败: %q %v", sub, err)
	}
	if err := os.RemoveAll(filepath.Join(manager.Root, "media", "usb0", "DCIM")); err != nil {
		t.Fatal(err)
	}
	// 新打标依旧要求目录存在。
	if _, _, err := manager.AddAutoBackupSub("/media/usb0/DCIM"); err == nil {
		t.Fatal("目录不存在时不该还能新打标")
	}
	// 取消打标只按路径算，目录没了也要能清掉。
	if _, sub, err := manager.RemoveAutoBackupSub("/media/usb0/DCIM"); err != nil || sub != "DCIM" {
		t.Fatalf("目录删掉后仍应能取消打标: %q %v", sub, err)
	}
	if got := manager.markedAutoBackupVolumes()[0].Subs; len(got) != 0 {
		t.Fatalf("取消后不该还留着子目录: %v", got)
	}
	// 卷外路径、卷根依旧拒绝。
	if _, _, err := manager.RemoveAutoBackupSub("/vol1/1000/别的地方"); err == nil {
		t.Fatal("卷外路径应当被拒绝")
	}
	if _, _, err := manager.RemoveAutoBackupSub("/media/usb0"); err == nil {
		t.Fatal("选中卷根时应提示改用整卷操作")
	}
}

// 旧版两处打标位置都继续认：.tad-auto-backup/mark.json（v0.10.30–v0.10.34）与卷根上的
// .tad-auto-backup 普通文件（更早）；下次写入统一收敛到 .tad-backup/mark.json。
func TestAutoBackupLegacyMarkerIsUpgraded(t *testing.T) {
	manager := autoBackupFakeManager(t)
	mountRoot := filepath.Join(manager.Root, "media", "usb0")

	// 旧版打标目录：别名与子目录都要读出来，并标记成「旧位置」。
	autoBackupWrite(t, manager,
		"/media/usb0/"+BackupAutoMarkerLegacyDirName+"/"+BackupAutoMarkerFileName,
		`{"alias":"旧目录打标","subs":["DCIM"]}`)
	marked := manager.markedAutoBackupVolumes()
	if len(marked) != 1 || !marked[0].Legacy || marked[0].Alias != "旧目录打标" || len(marked[0].Subs) != 1 {
		t.Fatalf("旧版打标目录应按已打标处理: %+v", marked)
	}
	if _, err := manager.MarkAutoBackupVolume("/media/usb0", "升级后的盘"); err != nil {
		t.Fatalf("给旧版盘重新打标（升级）失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mountRoot, BackupAutoMarkerLegacyDirName)); !os.IsNotExist(err) {
		t.Fatalf("写入新位置后应清掉旧版打标目录: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mountRoot, BackupAutoMarkerDirName, BackupAutoMarkerFileName)); err != nil {
		t.Fatalf("应写进新的元数据目录: %v", err)
	}
	if got := manager.markedAutoBackupVolumes(); len(got) != 1 || got[0].Legacy {
		t.Fatalf("升级后不该再是旧版: %+v", got)
	}

	// 更旧的卷根同名普通文件：只能按「整块卷已打标」理解，写入后同样清掉。
	legacyName := filepath.Join(mountRoot, BackupAutoMarkerLegacyName)
	if err := os.RemoveAll(filepath.Join(mountRoot, BackupAutoMarkerDirName)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyName, []byte("tad-auto-backup\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	marked = manager.markedAutoBackupVolumes()
	if len(marked) != 1 || !marked[0].Legacy || len(marked[0].Subs) != 0 {
		t.Fatalf("旧版卷根文件应按整块卷已打标处理: %+v", marked)
	}
	if _, err := manager.MarkAutoBackupVolume("/media/usb0", "再升级"); err != nil {
		t.Fatalf("给旧版盘重新打标（升级）失败: %v", err)
	}
	if _, err := os.Stat(legacyName); !os.IsNotExist(err) {
		t.Fatalf("旧版卷根文件应被清理: %v", err)
	}

	// 取消打标时旧版残留也要一并清掉。
	if err := os.RemoveAll(filepath.Join(mountRoot, BackupAutoMarkerDirName)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyName, []byte("leftover\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ClearAutoBackupMark("/media/usb0"); err != nil {
		t.Fatalf("取消打标失败: %v", err)
	}
	if _, err := os.Stat(legacyName); !os.IsNotExist(err) {
		t.Fatalf("取消打标应清掉旧版残留文件: %v", err)
	}
}

// 只读卷：打标要当场报错，且错误信息要说明原因，而不是抛一个用户看不懂的 errno。
func TestMarkAutoBackupVolumeRejectsReadOnly(t *testing.T) {
	manager := autoBackupFakeManager(t)
	autoBackupWrite(t, manager, "proc/mounts",
		"/dev/mapper/trim_x-0 /vol1 btrfs rw 0 0\n/dev/sdd1 /media/ro exfat ro 0 0\n")
	autoBackupWrite(t, manager, "sys/block/sdd/removable", "1\n")
	if err := os.MkdirAll(filepath.Join(manager.Root, "media", "ro"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := manager.MarkAutoBackupVolume("/media/ro", "")
	if err == nil {
		t.Fatal("只读卷应当拒绝打标")
	}
	if !strings.Contains(err.Error(), "只读") {
		t.Fatalf("错误信息应说明是只读挂载: %v", err)
	}
}

// 写盘失败的原因要翻译成人话（权限 / 只读 / 空间不足各自一句），其它错误原样返回。
func TestAutoBackupWriteErrorTranslatesCommonFailures(t *testing.T) {
	if err := autoBackupWriteError(nil); err != nil {
		t.Fatalf("没有错误时应返回 nil: %v", err)
	}
	if err := autoBackupWriteError(fs.ErrPermission); err == nil || !strings.Contains(err.Error(), "权限") {
		t.Fatalf("权限错误应被翻译: %v", err)
	}
	other := errors.New("boom")
	if err := autoBackupWriteError(other); !errors.Is(err, other) {
		t.Fatalf("其它错误应原样返回: %v", err)
	}
}

// 打标列表：在线的存储设备都要列出来（含还没打标的与不可打标的内置盘），
// 拔掉的盘靠 NAS 侧记录继续显示成「离线」。
func TestAutoBackupDevicesListsOnlineAndOffline(t *testing.T) {
	manager := autoBackupFakeManager(t)
	if _, err := manager.MarkAutoBackupVolume("/media/usb0", "相机卡"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.AddAutoBackupSub("/media/usb0/DCIM"); err != nil {
		t.Fatal(err)
	}

	byMount := map[string]AutoBackupDevice{}
	for _, device := range manager.AutoBackupDevices() {
		byMount[device.MountPoint] = device
	}
	usb, ok := byMount["/media/usb0"]
	if !ok || !usb.Online || !usb.Marked || !usb.Removable || usb.ReadOnly {
		t.Fatalf("在线的打标卷应带完整信息: %+v", usb)
	}
	if usb.Alias != "相机卡" || len(usb.Subs) != 1 || usb.Subs[0] != "DCIM" {
		t.Fatalf("别名与子目录应随列表下发: %+v", usb)
	}
	internal, ok := byMount["/vol1"]
	if !ok || !internal.Online || internal.Marked || internal.Removable {
		t.Fatalf("内置存储也应列出来并标明不可移动: %+v", internal)
	}
	// 网络 / 云盘挂载不算存储设备。
	for _, device := range manager.AutoBackupDevices() {
		if strings.HasPrefix(device.MountPoint, "/vol02/") {
			t.Fatalf("网络挂载不该出现在打标列表里: %+v", device)
		}
	}

	// 拔盘：只剩 NAS 侧记录，列表里依然是「离线 + 已打标」。
	autoBackupWrite(t, manager, "proc/mounts", "/dev/mapper/trim_x-0 /vol1 btrfs rw 0 0\n")
	devices := manager.AutoBackupDevices()
	offline, ok := autoBackupDeviceByIdentity(devices, "/dev/sdc1")
	if !ok {
		t.Fatalf("离线设备应留在列表里: %+v", devices)
	}
	if offline.Online || !offline.Marked || offline.PendingClear {
		t.Fatalf("离线记录不正确: %+v", offline)
	}
	if offline.MountPoint != "/media/usb0" || offline.Alias != "相机卡" || len(offline.Subs) != 1 {
		t.Fatalf("离线记录应保留上次的挂载点/别名/子目录: %+v", offline)
	}
}

func autoBackupDeviceByIdentity(devices []AutoBackupDevice, identity string) (AutoBackupDevice, bool) {
	for _, device := range devices {
		if device.Identity == identity {
			return device, true
		}
	}
	return AutoBackupDevice{}, false
}

// 离线取消打标：设备不在时先在记录上挂起，等它下次上线自动清除标记；挂起还能撤销。
func TestForgetAutoBackupVolumeClearsOnNextOnline(t *testing.T) {
	manager := autoBackupFakeManager(t)
	if _, err := manager.MarkAutoBackupVolume("/media/usb0", "相机卡"); err != nil {
		t.Fatal(err)
	}
	markerDir := filepath.Join(manager.Root, "media", "usb0", BackupAutoMarkerDirName)
	if _, err := os.Stat(markerDir); err != nil {
		t.Fatalf("前置条件丢失: %v", err)
	}

	// 拔盘。
	autoBackupWrite(t, manager, "proc/mounts", "/dev/mapper/trim_x-0 /vol1 btrfs rw 0 0\n")

	if err := manager.ForgetAutoBackupVolume("/dev/sdc1"); err != nil {
		t.Fatalf("离线取消打标失败: %v", err)
	}
	if got := autoBackupDevice(t, manager, "/dev/sdc1"); !got.PendingClear {
		t.Fatalf("应记下「下次上线自动清除」: %+v", got)
	}
	if _, err := os.Stat(markerDir); err != nil {
		t.Fatalf("设备不在时不该去改卷上的标记: %v", err)
	}

	// 改主意：撤销挂起。
	if err := manager.CancelPendingAutoBackupClear("/dev/sdc1"); err != nil {
		t.Fatal(err)
	}
	if got := autoBackupDevice(t, manager, "/dev/sdc1"); got.PendingClear {
		t.Fatalf("撤销后不该再挂起: %+v", got)
	}
	if _, err := manager.ProcessAutoBackupClears(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(markerDir); err != nil {
		t.Fatalf("撤销后不该清除标记: %v", err)
	}

	// 再挂起一次，然后把盘插回来 → 自动清除标记并移除记录。
	if err := manager.ForgetAutoBackupVolume("/dev/sdc1"); err != nil {
		t.Fatal(err)
	}
	autoBackupWrite(t, manager, "proc/mounts", backupPlacesFakeMounts)
	cleared, err := manager.ProcessAutoBackupClears()
	if err != nil || cleared != 1 {
		t.Fatalf("设备重新上线时应清除 1 块盘的标记: %d %v", cleared, err)
	}
	if _, err := os.Stat(markerDir); !os.IsNotExist(err) {
		t.Fatalf("标记目录应被删除: %v", err)
	}
	// 盘又插着，列表里应恢复成「在线、未打标」——记录里的挂起状态也一并清掉。
	device, ok := autoBackupDeviceByIdentity(manager.AutoBackupDevices(), "/dev/sdc1")
	if !ok || !device.Online || device.Marked || device.PendingClear {
		t.Fatalf("清除后应恢复成在线未打标: %+v", device)
	}
	// 没有挂起的设备时是空转。
	if cleared, err := manager.ProcessAutoBackupClears(); err != nil || cleared != 0 {
		t.Fatalf("无挂起时应空转: %d %v", cleared, err)
	}
}

// 设备在线时「取消打标」＝ 当场清除，不留挂起记录。
func TestForgetAutoBackupVolumeClearsImmediatelyWhenOnline(t *testing.T) {
	manager := autoBackupFakeManager(t)
	if _, err := manager.MarkAutoBackupVolume("/media/usb0", ""); err != nil {
		t.Fatal(err)
	}
	if err := manager.ForgetAutoBackupVolume("/dev/sdc1"); err != nil {
		t.Fatalf("在线取消打标失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(manager.Root, "media", "usb0", BackupAutoMarkerDirName)); !os.IsNotExist(err) {
		t.Fatalf("在线取消应立刻删掉标记: %v", err)
	}
	if got := manager.markedAutoBackupVolumes(); len(got) != 0 {
		t.Fatalf("取消后不该再扫到卷: %+v", got)
	}
	if err := manager.ForgetAutoBackupVolume("  "); err == nil {
		t.Fatal("缺少设备标识应当报错")
	}
}

// 打标记录要真的落盘：换一个 Manager 实例（模拟重启）后离线列表仍在。
func TestAutoBackupRecordsSurviveReload(t *testing.T) {
	manager := autoBackupFakeManager(t)
	if _, err := manager.MarkAutoBackupVolume("/media/usb0", "相机卡"); err != nil {
		t.Fatal(err)
	}
	restarted := &Manager{Root: manager.Root, ConfigPath: manager.ConfigPath}
	autoBackupWrite(t, restarted, "proc/mounts", "/dev/mapper/trim_x-0 /vol1 btrfs rw 0 0\n")
	device, ok := autoBackupDeviceByIdentity(restarted.AutoBackupDevices(), "/dev/sdc1")
	if !ok || device.Online || device.Alias != "相机卡" {
		t.Fatalf("重启后离线记录应还在: %+v", device)
	}
}

// 记录写不进去（没有配置文件 / 只读）不该让打标本身失败：卷上的标记才是准的。
func TestRememberAutoBackupVolumeFailureIsNotFatal(t *testing.T) {
	manager := backupPlacesFakeManager(t) // 故意不给 ConfigPath
	if _, err := manager.MarkAutoBackupVolume("/media/usb0", "相机卡"); err != nil {
		t.Fatalf("记录写不进去不该让打标失败: %v", err)
	}
	if got := manager.markedAutoBackupVolumes(); len(got) != 1 || got[0].Alias != "相机卡" {
		t.Fatalf("卷上的标记应当生效: %+v", got)
	}
}

// 打标卷记录的校验：标识必填且不重复，别名与子目录都有限制。
func TestValidateGPIOAutoVolumeRecords(t *testing.T) {
	valid := []AutoBackupVolumeRecord{{Identity: "/dev/sdc1", Alias: "相机卡", Subs: []string{"DCIM"}}}
	if err := validateGPIOAutoVolumeRecords(valid); err != nil {
		t.Fatalf("合法记录不该报错: %v", err)
	}

	cases := []struct {
		name    string
		records []AutoBackupVolumeRecord
	}{
		{"标识为空", []AutoBackupVolumeRecord{{Identity: "  "}}},
		{"标识重复", []AutoBackupVolumeRecord{{Identity: "a"}, {Identity: "a"}}},
		{"别名过长", []AutoBackupVolumeRecord{{Identity: "a", Alias: strings.Repeat("替", autoBackupAliasMaxLen+1)}}},
		{"子目录绝对路径", []AutoBackupVolumeRecord{{Identity: "a", Subs: []string{"/abs"}}}},
		{"子目录带 ..", []AutoBackupVolumeRecord{{Identity: "a", Subs: []string{"a/../b"}}}},
		{"子目录为空串", []AutoBackupVolumeRecord{{Identity: "a", Subs: []string{" "}}}},
	}
	for _, test := range cases {
		if err := validateGPIOAutoVolumeRecords(test.records); err == nil {
			t.Fatalf("%s：应当被拒绝", test.name)
		}
	}
}

// 子目录列表的清洗：去空白、统一斜杠、去重、排序，非法项直接丢弃。
func TestNormalizeAutoBackupSubs(t *testing.T) {
	got := normalizeAutoBackupSubs([]string{" DCIM ", "VIDEO/", "DCIM", "", "/", "../x", "a//b"})
	want := []string{"DCIM", "VIDEO", "a//b"}
	if len(got) != len(want) {
		t.Fatalf("子目录清洗结果 = %v，期望 %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("子目录清洗结果 = %v，期望 %v", got, want)
		}
	}
}

// 带打标记录的 GPIO 配置要能通过校验，非法记录要被当场拒绝。
func TestGPIOAutoBackupActionBindingExtra(t *testing.T) {
	config := DefaultGPIOConfig()
	config.Autos = []GPIOAutoBackup{{
		ID: "dump", Name: "导入", Direction: BackupDirectionFromVolume,
		Fixed: BackupLocation{Kind: "path", Path: "/vol1/1000/导入"}, Mode: BackupModeCopy,
	}}
	config.Buttons[0].Actions.Short = GPIOActionAutoBackupPrefix + "dump"
	config.AutoVolumes = []AutoBackupVolumeRecord{{Identity: "/dev/sdc1", Alias: "相机卡"}}
	if err := validateGPIOConfig(config); err != nil {
		t.Fatalf("带打标记录的配置应通过校验: %v", err)
	}
	config.AutoVolumes = []AutoBackupVolumeRecord{{Identity: " "}}
	if err := validateGPIOConfig(config); err == nil {
		t.Fatal("非法打标记录应当被拒绝")
	}
}

// 按键动作 auto_backup:<id> 的绑定与校验。
func TestGPIOAutoBackupActionBinding(t *testing.T) {
	rule := GPIOAutoBackup{
		ID: "dump", Name: "导入", Direction: BackupDirectionFromVolume,
		Fixed: BackupLocation{Kind: "path", Path: "/vol1/1000/导入"},
		Mode:  BackupModeCopy,
	}
	config := DefaultGPIOConfig()
	config.Autos = []GPIOAutoBackup{rule}
	config.Buttons[0].Actions.Short = GPIOActionAutoBackupPrefix + "dump"
	if err := validateGPIOConfig(config); err != nil {
		t.Fatalf("绑定自动备份动作应通过校验: %v", err)
	}
	if got := gpioAutoBackupForAction(config, "auto_backup:dump"); got == nil || got.Name != "导入" {
		t.Fatalf("动作解析不正确: %+v", got)
	}
	if GPIOActionDisplay("auto_backup:dump") != "自动备份 dump" {
		t.Fatalf("动作显示名不正确: %q", GPIOActionDisplay("auto_backup:dump"))
	}
	if got := gpioActionDisplay(config, "auto_backup:dump"); got != "自动备份：导入" {
		t.Fatalf("带配置的显示名不正确: %q", got)
	}

	config.Buttons[0].Actions.Short = GPIOActionAutoBackupPrefix + "missing"
	if err := validateGPIOConfig(config); err == nil {
		t.Fatal("引用不存在的自动备份规则应当报错")
	}

	// 未配置任何规则时，自动备份动作不能通过校验。
	bare := DefaultGPIOConfig()
	bare.Buttons[0].Actions.Short = GPIOActionAutoBackupPrefix + "dump"
	if err := validateGPIOConfig(bare); err == nil {
		t.Fatal("没有规则时应拒绝绑定")
	}
}

// 有配置过自动备份规则的 GPIO 配置不再被视为「从未配置」，避免升级时被覆盖。
func TestGPIOConfigLooksUnconfiguredRespectsAutos(t *testing.T) {
	config := DefaultGPIOConfig()
	config.Enabled = false
	// 旧版本的默认值里没有迁移标记与提示音字段，去掉才等于「从未配置过」。
	config.Migrations = nil
	config.StorageNotify = nil
	for index := range config.Buttons {
		config.Buttons[index].Actions = GPIOActions{}
	}
	if !gpioConfigLooksUnconfigured(config) {
		t.Fatal("只有按键默认值时仍应视为未配置")
	}
	config.Autos = []GPIOAutoBackup{{
		ID: "dump", Name: "导入", Direction: BackupDirectionFromVolume,
		Fixed: BackupLocation{Kind: "path", Path: "/vol1/1000/导入"}, Mode: BackupModeCopy,
	}}
	if gpioConfigLooksUnconfigured(config) {
		t.Fatal("存在自动备份规则时应视为已配置")
	}

	// 打标卷记录同样是「用户配置过」的痕迹：升级时不能把它连同记录一起抹掉。
	config.Autos = nil
	config.AutoVolumes = []AutoBackupVolumeRecord{{Identity: "/dev/sdc1", Alias: "相机卡"}}
	if gpioConfigLooksUnconfigured(config) {
		t.Fatal("存在打标卷记录时应视为已配置")
	}
}

// 没有打标卷时必须明确报错，而不是静默「成功处理 0 块盘」。
func TestRunAutoBackupJobWithoutMarkedVolumeFails(t *testing.T) {
	manager := backupPlacesFakeManager(t)
	rule := GPIOAutoBackup{
		ID: "dump", Name: "导入", Direction: BackupDirectionFromVolume,
		Fixed: BackupLocation{Kind: "path", Path: "/vol1/1000/导入"},
		Mode:  BackupModeCopy,
	}
	result, err := manager.RunAutoBackupJob(context.Background(), rule, false)
	if err == nil {
		t.Fatal("没有打标卷应当报错")
	}
	if !strings.Contains(err.Error(), "打标") {
		t.Fatalf("错误信息应提示打标: %v", err)
	}
	if result.Volumes != 0 {
		t.Fatalf("不应有卷被处理: %+v", result)
	}
}
