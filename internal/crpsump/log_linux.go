//go:build linux

package crpsump

import "syscall"

// openNoFollow: 打开日志文件时拒绝跟随符号链接 (防多用户环境日志劫持), 仅 Linux 支持
const openNoFollow = syscall.O_NOFOLLOW
