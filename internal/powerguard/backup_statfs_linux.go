//go:build linux

package powerguard

import "syscall"

// filesystemUsage 返回该路径所在文件系统的总容量与可用空间。
func filesystemUsage(path string) (total, free uint64, err error) {
	var stat syscall.Statfs_t
	if statErr := syscall.Statfs(path, &stat); statErr != nil {
		return 0, 0, statErr
	}
	blockSize := uint64(stat.Bsize)
	return stat.Blocks * blockSize, stat.Bavail * blockSize, nil
}
