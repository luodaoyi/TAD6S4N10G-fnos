//go:build !linux

package powerguard

import "fmt"

// storage_notify_other.go：非 Linux 平台没有 /sys/block 与 PC Speaker，
// 监视一拍就直接报告不可用（界面上会显示原因，不影响其它功能）。
func (m *Manager) storageDeviceSnapshot() (storageDeviceSnapshot, error) {
	return storageDeviceSnapshot{}, fmt.Errorf("存储设备插入提示需要在 linux 上运行")
}
