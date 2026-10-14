//go:build linux

package powerguard

import "syscall"

// 打标写盘失败时用来把底层 errno 翻译成人话（见 autoBackupWriteError）。
// 这两个常量只在 Linux 上有定义，所以单独放平台文件；其它平台用不会命中的兜底值。
const (
	errnoReadOnlyFS = syscall.EROFS
	errnoNoSpace    = syscall.ENOSPC
)
