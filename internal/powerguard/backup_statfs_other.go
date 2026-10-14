//go:build !linux

package powerguard

import "errors"

// filesystemUsage 只在目标运行平台（linux/amd64）上有意义；其余平台返回错误，
// 界面据此隐藏容量信息而不是显示错误的 0。
func filesystemUsage(path string) (total, free uint64, err error) {
	return 0, 0, errors.New("filesystem usage is only available on linux")
}
