//go:build !linux

package powerguard

// 非 linux 构建没有串口实现：发行版只出 linux/amd64，windows 构建仅用于
// 开发期跑测试。打开恒报错，读取器按重连节奏安静重试；单测用假设备注入
// openSerialPort，不受此桩影响。

import "errors"

func openSerialPortOS(path string, baud int) (serialPort, error) {
	return nil, errors.New("serial temperature sensors are only supported on linux")
}
