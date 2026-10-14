//go:build !linux

package powerguard

import (
	"context"
	"errors"
)

// 刷新SD卡依赖 Linux 的 sysfs（USB 设备树）与 /proc，其它平台只留一个明确报错的
// 占位实现，好让开发机上的编译与测试照常进行。
func (m *Manager) refreshSDCard(ctx context.Context) (SdcardRefreshResult, error) {
	return SdcardRefreshResult{}, errors.New("刷新SD卡仅支持 Linux（飞牛）系统")
}
