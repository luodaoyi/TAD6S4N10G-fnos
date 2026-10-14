//go:build linux

package powerguard

import (
	"fmt"
	"path/filepath"
	"strings"
)

// storage_notify_linux.go：在飞牛（Linux）上枚举块设备并区分「移动存储 / 内置存储」。
//
// 判据只看内核已经认到的块设备与容量，不去探 USB 设备树：容量为 0 的空读卡器
// 不算「插入了存储」，等真正读到介质的那一拍才提示——用户插卡本来想要的就是这个。

// storageBlockVirtualPrefixes 是内核提供的「非物理盘」名字前缀：loop（镜像挂载）、
// ram/zram（内存盘）、dm-（LVM / 加密层）、md（软 RAID 阵列）、sr（光驱）、
// nbd/rbd（网络块设备）、fd（软驱）。它们的出现不该被当成「插了一块盘」。
var storageBlockVirtualPrefixes = []string{
	"loop", "ram", "zram", "dm-", "md", "sr", "nbd", "rbd", "fd",
}

func (m *Manager) storageDeviceSnapshot() (storageDeviceSnapshot, error) {
	snapshot := storageDeviceSnapshot{
		removable: make(map[string]int64),
		internal:  make(map[string]int64),
	}
	base := m.rooted("/sys/block")
	matches, _ := filepath.Glob(filepath.Join(base, "*"))
	if len(matches) == 0 {
		return snapshot, fmt.Errorf("没有找到块设备（%s）", base)
	}
	for _, match := range matches {
		name := filepath.Base(match)
		if storageBlockIsVirtual(name) {
			continue
		}
		sectors, err := readInt(filepath.Join(match, "size"))
		if err != nil || sectors <= 0 {
			// 空读卡器的块设备容量是 0，还不算有存储介质。
			continue
		}
		if m.storageBlockIsRemovable(match) {
			snapshot.removable[name] = sectors
		} else {
			snapshot.internal[name] = sectors
		}
	}
	return snapshot, nil
}

func storageBlockIsVirtual(name string) bool {
	for _, prefix := range storageBlockVirtualPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// storageBlockIsRemovable 判断块设备算不算「移动存储」：内核 removable 标志为 1
// （读卡器与多数 U 盘），或者它挂在 USB 总线下面——很多 USB 硬盘盒把 removable
// 报成 0，只能靠总线路径认。
//
// 注意 sysfs 里 /sys/block/<dev> 是符号链接；这里的符号链接目标是相对路径，
// 所以在 --sys-root 假根下也能解析成功。
func (m *Manager) storageBlockIsRemovable(blockDir string) bool {
	if flag, err := readTrim(filepath.Join(blockDir, "removable")); err == nil && flag == "1" {
		return true
	}
	resolved, err := filepath.EvalSymlinks(blockDir)
	if err != nil {
		return false
	}
	return strings.Contains(filepath.ToSlash(resolved), "/usb")
}
