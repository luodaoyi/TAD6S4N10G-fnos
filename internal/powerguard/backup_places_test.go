package powerguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 位置识别（我的文件 / 团队文件 / 外接存储 / 远程挂载）决定了界面默认能选哪些目录，
// 这里用一份仿真出来的飞牛文件系统布局把分类规则钉死。

const backupPlacesFakeMounts = `/dev/sdb2 / ext4 rw,relatime,errors=remount-ro 0 0
/dev/sdb1 /boot/efi vfat rw,relatime 0 0
/dev/mapper/trim_x-0 /vol1 btrfs rw,relatime,ssd 0 0
/dev/sdc1 /media/usb0 ext4 rw,relatime 0 0
1000-1-2056118d:cloud-storage/v1/dav /vol02/1000-1-2056118d fuse.rclone rw,nosuid,nodev 0 0
`

func backupPlacesFakeManager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	write := func(path, content string) {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir := func(path string) {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(path)), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	write("proc/mounts", backupPlacesFakeMounts)
	// 可移动介质标记。isRemovableDevice 走 /sys/class/block/<name>（真实 sysfs
	// 里是符号链接，这里用普通目录模拟——EvalSymlinks 对普通目录原样返回）。
	// 真实拓扑里分区要经符号链接上溯一层才到整盘，那部分由
	// TestIsRemovableDeviceUsesSysfsSymlinkAndUsbPath 覆盖；这里直接给整盘和
	// 分区条目各写一份 removable，让分类测试只钉住「识别结果」。
	write("sys/class/block/sdc/removable", "1\n")
	write("sys/class/block/sdc1/removable", "1\n")
	write("sys/class/block/sdd/removable", "1\n")
	write("sys/class/block/sdd1/removable", "1\n")
	write("sys/class/block/sdb/removable", "0\n")
	write("sys/class/block/sda/removable", "0\n")
	// /sys/block 一份同样保留，覆盖「只有 /sys/block」的回退路径。
	write("sys/block/sdc/removable", "1\n")
	write("sys/block/sdb/removable", "0\n")
	write("sys/block/sda/removable", "0\n")

	mkdir("vol1/1000/照片")
	mkdir("vol1/1000/.@#local")
	mkdir("vol1/电影")
	mkdir("vol1/@appdata/fntemplog")
	mkdir("vol1/@appshare")
	mkdir("vol1/docker")
	mkdir("vol1/thumb")
	mkdir("vol1/.cache.trim-sysrestore")
	mkdir("vol02/1000-1-2056118d/来自：分享")
	mkdir("media/usb0/DCIM")

	return &Manager{Root: root}
}

func TestBackupPlacesBuildsFnosCategories(t *testing.T) {
	manager := backupPlacesFakeManager(t)
	places := manager.BackupPlaces()

	byKind := map[string][]BackupPlace{}
	for _, place := range places {
		byKind[place.Kind] = append(byKind[place.Kind], place)
	}

	personal := byKind[BackupPlacePersonal]
	if len(personal) != 1 || personal[0].Path != "/vol1/1000" || personal[0].Name != "我的文件" {
		t.Fatalf("我的文件识别不正确: %+v", personal)
	}

	team := byKind[BackupPlaceTeam]
	if len(team) != 1 || team[0].Path != "/vol1/电影" {
		t.Fatalf("团队文件应只包含 /vol1/电影（系统目录必须过滤）: %+v", team)
	}

	external := byKind[BackupPlaceExternal]
	if len(external) != 1 || external[0].Path != "/media/usb0" || !external[0].Removable {
		t.Fatalf("外接存储识别不正确: %+v", external)
	}

	remote := byKind[BackupPlaceRemote]
	if len(remote) != 1 || remote[0].Path != "/vol02/1000-1-2056118d" {
		t.Fatalf("远程挂载识别不正确: %+v", remote)
	}

	// 卷根 /vol1 本身、以及存放远程挂载的 /vol02 都不能被当成内部存储卷。
	for _, place := range places {
		switch place.Path {
		case "/vol1", "/vol02", "/vol1/@appdata", "/vol1/@appshare", "/vol1/docker", "/vol1/thumb", "/vol1/.cache.trim-sysrestore":
			t.Fatalf("不该出现在位置列表里: %+v", place)
		}
	}
}

func TestBackupPlacesOrderFollowsFnosSidebar(t *testing.T) {
	manager := backupPlacesFakeManager(t)
	places := manager.BackupPlaces()
	if len(places) < 4 {
		t.Fatalf("位置数量不足: %+v", places)
	}
	order := []string{places[0].Kind, places[1].Kind, places[2].Kind, places[3].Kind}
	want := []string{BackupPlacePersonal, BackupPlaceTeam, BackupPlaceExternal, BackupPlaceRemote}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("位置顺序应为 %v，实际 %v", want, order)
		}
	}
}

func TestPlacePathOwnerPicksDeepestPlace(t *testing.T) {
	places := []BackupPlace{
		{ID: "personal:/vol1/1000", Path: "/vol1/1000", Kind: BackupPlacePersonal},
		{ID: "team:/vol1/photo", Path: "/vol1/photo", Kind: BackupPlaceTeam},
	}
	cases := []struct {
		path string
		want string
	}{
		{"/vol1/1000", "personal:/vol1/1000"},
		{"/vol1/1000/照片/2024", "personal:/vol1/1000"},
		{"/vol1/photo", "team:/vol1/photo"},
		{"/vol1", ""},
		{"/etc", ""},
		{"/vol1/10000", ""}, // 前缀相同但不是同一层，不能误判
	}
	for _, test := range cases {
		if got := placePathOwner(places, test.path); got != test.want {
			t.Fatalf("placePathOwner(%q) = %q，期望 %q", test.path, got, test.want)
		}
	}
}

func TestBackupBrowseScopesControlReach(t *testing.T) {
	manager := backupPlacesFakeManager(t)

	// 默认（places）范围：空路径返回位置总览。
	overview, err := manager.BrowseBackupPath("", "")
	if err != nil {
		t.Fatal(err)
	}
	if overview.Mode != "places" || overview.Path != "" {
		t.Fatalf("空路径应返回位置总览: %+v", overview)
	}
	if len(overview.Places) == 0 {
		t.Fatal("位置总览应带上位置列表")
	}

	// 系统目录在默认范围里进不去，一律退回位置总览，避免小白误入。
	for _, path := range []string{"/", "/etc", "/vol1", "/vol02"} {
		result, err := manager.BrowseBackupPath(path, "")
		if err != nil {
			t.Fatalf("%s 应安全退回总览而不是报错: %v", path, err)
		}
		if result.Mode != "places" {
			t.Fatalf("%s 不该在默认范围里被浏览", path)
		}
	}

	// 位置内部可以正常下钻，并且到位置根目录时 parent 为空（＝回到位置总览）。
	root, err := manager.BrowseBackupPath("/vol1/1000", "")
	if err != nil {
		t.Fatal(err)
	}
	if root.Mode != "browse" || root.Path != "/vol1/1000" {
		t.Fatalf("我的文件根目录应可浏览: %+v", root)
	}
	if root.Parent != "" {
		t.Fatalf("位置根目录的上一级应为位置总览，实际 %q", root.Parent)
	}
	if root.Inside != "personal:/vol1/1000" {
		t.Fatalf("inside 标识不正确: %q", root.Inside)
	}

	child, err := manager.BrowseBackupPath("/vol1/1000/照片", "")
	if err != nil {
		t.Fatal(err)
	}
	if child.Parent != "/vol1/1000" {
		t.Fatalf("子目录的上一级应是位置根目录，实际 %q", child.Parent)
	}

	// 位置内部隐藏飞牛自己的内部标记目录；普通子目录不被打「位置」角标。
	for _, entry := range root.Entries {
		if entry.Name == ".@#local" {
			t.Fatal("位置内部不应显示 .@# 开头的飞牛内部目录")
		}
		if entry.IsPlace {
			t.Fatalf("%s 不是位置根目录，不该有位置角标", entry.Name)
		}
	}

	// 高级范围（scope=all）放开整个文件系统。
	all, err := manager.BrowseBackupPath("/", BackupScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	if all.Mode != "browse" || all.Scope != BackupScopeAll || all.Path != "/" {
		t.Fatalf("高级模式应能浏览根目录: %+v", all)
	}
	// 伪文件系统即便在高级模式里也要隐藏，避免点进去卡死。
	for _, entry := range all.Entries {
		if entry.Name == "proc" || entry.Name == "sys" || entry.Name == "dev" {
			t.Fatalf("高级模式仍应隐藏内核伪文件系统: %s", entry.Name)
		}
	}

	// 高级模式走到 /vol1 时，其下的 uid 目录应被标记为「位置」。
	volumeRoot, err := manager.BrowseBackupPath("/vol1", BackupScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	marked := false
	for _, entry := range volumeRoot.Entries {
		if entry.Name == "1000" && entry.IsPlace {
			marked = true
		}
	}
	if !marked {
		t.Fatal("/vol1 下的 1000 应被标记为位置根目录")
	}
}

func TestIsBackupSystemDirName(t *testing.T) {
	system := []string{"@appdata", "@appshare", "docker", "thumb", "lost+found", "#recycle", ".cache", "mediasrv.transcode"}
	for _, name := range system {
		if !isBackupSystemDirName(name) {
			t.Fatalf("%q 应被当成系统目录", name)
		}
	}
	for _, name := range []string{"电影", "备份", "photo", "backup-2024"} {
		if isBackupSystemDirName(name) {
			t.Fatalf("%q 是普通目录，不该被过滤", name)
		}
	}
}

func TestWriteBackupMarkerCreatesFileOnSource(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "usb"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root}
	job := GPIOBackupJob{
		ID: "marker", Name: "标识测试", Mode: BackupModeCopy,
		Source: BackupLocation{Kind: "path", Path: "/usb"},
		Target: BackupLocation{Kind: "path", Path: "/backup"},
	}

	// 内容留空：应自动生成并写进源介质。
	marker, written, err := manager.WriteBackupMarker(job)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(marker, "tad-") {
		t.Fatalf("应自动生成标识内容，实际 %q", marker)
	}
	if written != "/usb/"+BackupMetaDirName+"/"+backupMarkerFileName {
		t.Fatalf("写入路径不正确: %q", written)
	}
	// 元数据统一放在 .tad-backup/ 里，源根不该出现裸的标识文件。
	if _, err := os.Stat(filepath.Join(root, "usb", backupMarkerLegacyName)); !os.IsNotExist(err) {
		t.Fatalf("源根不该有裸标识文件: %v", err)
	}
	if info, err := os.Stat(filepath.Join(root, "usb", BackupMetaDirName)); err != nil || !info.IsDir() {
		t.Fatalf("应建出统一元数据目录: %v %+v", err, info)
	}
	if err := checkBackupMarker(filepath.Join(root, "usb"), marker); err != nil {
		t.Fatalf("写入后校验应通过: %v", err)
	}
	if err := checkBackupMarker(filepath.Join(root, "usb"), marker+"x"); err == nil {
		t.Fatal("标识内容不一致时应拒绝执行")
	}

	// 用户已经给了内容：沿用用户的内容，不重新生成。
	job.Marker = "photo-disk"
	again, _, err := manager.WriteBackupMarker(job)
	if err != nil {
		t.Fatal(err)
	}
	if again != "photo-disk" {
		t.Fatalf("应沿用用户填写的标识内容，实际 %q", again)
	}

	// 源不存在时应给出可理解的错误，而不是写坏文件。
	if _, _, err := manager.WriteBackupMarker(GPIOBackupJob{
		Source: BackupLocation{Kind: "path", Path: "/not-there"},
	}); err == nil {
		t.Fatal("源不存在时应报错")
	}
}

func TestIsBackupRemoteFSType(t *testing.T) {
	remote := []string{"fuse.rclone", "fuse.sshfs", "fuseblk", "nfs", "nfs4", "cifs", "davfs"}
	for _, fsType := range remote {
		if !isBackupRemoteFSType(fsType) {
			t.Fatalf("%q 应识别为远程挂载", fsType)
		}
	}
	for _, fsType := range []string{"btrfs", "ext4", "xfs", "vfat", "tmpfs"} {
		if isBackupRemoteFSType(fsType) {
			t.Fatalf("%q 不是远程挂载", fsType)
		}
	}
}

// CodeRabbit 第二轮：部分 USB 硬盘盒把 removable 报成 0，只能靠 sysfs 符号链接
// 解析出的总线路径（含 /usb）认出来；分区要用 sysfs 归属整盘，不能去末尾数字猜
// （nvme0n1p1 去掉数字得到 nvme0n1p，mmcblk0p1 得到 mmcblk0p，都不对）。
func TestIsRemovableDeviceUsesSysfsSymlinkAndUsbPath(t *testing.T) {
	root := t.TempDir()
	write := func(path, content string) {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(linkPath, target string) {
		full := filepath.Join(root, filepath.FromSlash(linkPath))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.FromSlash(target), full); err != nil {
			t.Skipf("无法创建符号链接，跳过： %v", err)
		}
	}

	// USB 硬盘盒：removable=0，但 sysfs 路径带 /usb。
	usbDisk := "devices/pci0000:00/0000:00:14.0/usb1/1-2/1-2:1.0/host6/target6:0:0/6:0:0:0/block/sdc"
	write("sys/"+usbDisk+"/removable", "0\n")
	write("sys/"+usbDisk+"/sdc1/partition", "1\n")
	link("sys/class/block/sdc", "../../"+usbDisk)
	link("sys/class/block/sdc1", "../../"+usbDisk+"/sdc1")

	// 内置 NVMe：removable=0、路径无 /usb。
	nvme := "devices/pci0000:00/0000:00:01.0/nvme/nvme0/nvme0n1"
	write("sys/"+nvme+"/removable", "0\n")
	write("sys/"+nvme+"/nvme0n1p1/partition", "1\n")
	link("sys/class/block/nvme0n1", "../../"+nvme)
	link("sys/class/block/nvme0n1p1", "../../"+nvme+"/nvme0n1p1")

	manager := &Manager{Root: root}
	if !manager.isRemovableDevice("/dev/sdc1") {
		t.Fatal("USB 硬盘盒分区（removable=0、总线路径含 /usb）应被识别为可移动")
	}
	if !manager.isRemovableDevice("/dev/sdc") {
		t.Fatal("USB 硬盘盒整盘应被识别为可移动")
	}
	if manager.isRemovableDevice("/dev/nvme0n1p1") {
		t.Fatal("内置 NVMe 分区不应被识别为可移动")
	}
	if manager.isRemovableDevice("/dev/nvme0n1") {
		t.Fatal("内置 NVMe 整盘不应被识别为可移动")
	}

	// 老内核 / 测试假根里只有 /sys/block 的回退路径：整盘名直接命中。
	legacy := t.TempDir()
	legacyFull := filepath.Join(legacy, "sys", "block", "sdd")
	if err := os.MkdirAll(legacyFull, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyFull, "removable"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacyManager := &Manager{Root: legacy}
	if !legacyManager.isRemovableDevice("/dev/sdd") {
		t.Fatal("只有 /sys/block 时整盘也应能识别为可移动")
	}
	if legacyManager.isRemovableDevice("/dev/sde") {
		t.Fatal("sysfs 里不存在的设备不应被识别为可移动")
	}
}
