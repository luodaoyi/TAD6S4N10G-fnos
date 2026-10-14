//go:build !linux

package powerguard

import "syscall"

// 非 Linux 平台（开发机的 Windows 单测）没有 EROFS / ENOSPC 这两个 errno：
// 用 0 代替，errors.Is 永不命中，只会走 fs.ErrPermission 那条分支。
const (
	errnoReadOnlyFS = syscall.Errno(0)
	errnoNoSpace    = syscall.Errno(0)
)
