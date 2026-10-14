//go:build linux

package powerguard

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// writeFakeBlock 在假根下造一个块设备：真实目录放在 sys/devices/<devicePath>/block/<name>，
// 再按内核的样子在 sys/block/<name> 放一个**相对**符号链接指过去（相对链接才能在
// 假根里被 EvalSymlinks 正确解析）。
func writeFakeBlock(t *testing.T, root, name, devicePath string, sectors int64, removable string) {
	t.Helper()
	target := filepath.Join(root, "sys", "devices", filepath.FromSlash(devicePath), "block", name)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("建假块设备失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "size"), []byte(strconv.FormatInt(sectors, 10)+"\n"), 0o644); err != nil {
		t.Fatalf("写容量失败: %v", err)
	}
	if removable != "" {
		if err := os.WriteFile(filepath.Join(target, "removable"), []byte(removable+"\n"), 0o644); err != nil {
			t.Fatalf("写 removable 失败: %v", err)
		}
	}
	linkDir := filepath.Join(root, "sys", "block")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatalf("建 sys/block 失败: %v", err)
	}
	relative, err := filepath.Rel(linkDir, target)
	if err != nil {
		t.Fatalf("算相对路径失败: %v", err)
	}
	if err := os.Symlink(relative, filepath.Join(linkDir, name)); err != nil {
		t.Fatalf("建符号链接失败: %v", err)
	}
}

func TestStorageDeviceSnapshotClassifiesDevices(t *testing.T) {
	root := t.TempDir()
	// 系统盘：SATA，容量 500 GiB。
	writeFakeBlock(t, root, "sda", "pci0000:00/ata1/host0/target0:0:0/0:0:0:0", 1048576000, "0")
	// USB 读卡器认到卡：巴士路径里有 /usb，removable 报 0 也要算移动存储。
	writeFakeBlock(t, root, "sdb",
		"pci0000:00/0000:00:14.0/usb2/2-1/2-1.4/2-1.4:1.0/host6/target6:0:0/6:0:0:0", 30000000, "0")
	// 空读卡器：块设备在、容量 0 → 不算插入。
	writeFakeBlock(t, root, "sdc",
		"pci0000:00/0000:00:14.0/usb2/2-1/2-1.5/2-1.5:1.0/host7/target7:0:0/7:0:0:0", 0, "1")
	// 内置托架上 removable 标志为 1 的老式盘位：按 removable 也算移动存储。
	writeFakeBlock(t, root, "sdd", "pci0000:00/ata2/host1/target1:0:0/1:0:0:0", 2000000, "1")
	// 虚拟设备：不该出现在快照里。
	writeFakeBlock(t, root, "loop0", "virtual/block", 100, "")
	writeFakeBlock(t, root, "md0", "virtual/block", 200, "")
	writeFakeBlock(t, root, "dm-0", "virtual/block", 300, "")

	manager := &Manager{Root: root}
	snapshot, err := manager.storageDeviceSnapshot()
	if err != nil {
		t.Fatalf("采样失败: %v", err)
	}
	if len(snapshot.internal) != 1 || snapshot.internal["sda"] != 1048576000 {
		t.Fatalf("内置存储识别不对: %+v", snapshot.internal)
	}
	if len(snapshot.removable) != 2 || snapshot.removable["sdb"] != 30000000 || snapshot.removable["sdd"] != 2000000 {
		t.Fatalf("移动存储识别不对: %+v", snapshot.removable)
	}
	for _, virtual := range []string{"loop0", "md0", "dm-0", "sdc"} {
		if _, ok := snapshot.internal[virtual]; ok {
			t.Fatalf("虚拟设备/空设备 %s 不该被监视", virtual)
		}
		if _, ok := snapshot.removable[virtual]; ok {
			t.Fatalf("虚拟设备/空设备 %s 不该被监视", virtual)
		}
	}
}

func TestStorageDeviceSnapshotReportsMissingSysBlock(t *testing.T) {
	manager := &Manager{Root: t.TempDir()}
	if _, err := manager.storageDeviceSnapshot(); err == nil {
		t.Fatal("没有 /sys/block 时应当报错，让监视层保留旧基线")
	}
}

func TestStorageBlockIsVirtual(t *testing.T) {
	for _, name := range []string{"loop0", "zram0", "dm-2", "md127", "sr0", "nbd0", "rbd1", "fd0", "ram3"} {
		if !storageBlockIsVirtual(name) {
			t.Fatalf("%s 应当被当成虚拟设备", name)
		}
	}
	for _, name := range []string{"sda", "sdb", "nvme0n1", "mmcblk0", "sdz"} {
		if storageBlockIsVirtual(name) {
			t.Fatalf("%s 是真实设备，不该被过滤", name)
		}
	}
}
