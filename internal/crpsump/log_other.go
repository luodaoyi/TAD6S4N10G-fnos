//go:build !linux

package crpsump

// 非 Linux 平台无 O_NOFOLLOW (及 /dev/port 依赖), 恒为 0: 保证包可编译用于
// 宿主应用 (tad-module) 的跨平台构建与本地测试, 运行时 GPIO/日志功能自然降级。
const openNoFollow = 0
