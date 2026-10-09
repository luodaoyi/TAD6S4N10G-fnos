//go:build linux

package powerguard

// 串口的 linux 实现：openSerialPortOS 打开 /dev/tty* 并按波特率配成 raw 模式。
// 只在 linux 构建参与编译；windows（开发期测试）走 usb_serial_nolinux.go 桩。

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// openSerialPortOS 打开真实串口：O_NOCTTY 防止串口变成控制终端，raw 模式与
// 波特率由 configureSerial 完成。无权限（非 root、不在 dialout 组）时在这里
// 失败，由读取器按重连节奏记日志。
func openSerialPortOS(path string, baud int) (serialPort, error) {
	file, err := os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := configureSerial(file, baud); err != nil {
		_ = file.Close()
		return nil, err
	}
	return unixSerialPort{file: file}, nil
}

// unixSerialPort 用 os.File 实现串口读写；usb-serial 驱动支持 poll，因此
// SetReadDeadline 有效（与 usb_temper.go 的 hidrawFile 同一机制）。
type unixSerialPort struct {
	file *os.File
}

func (p unixSerialPort) Read(b []byte) (int, error)        { return p.file.Read(b) }
func (p unixSerialPort) SetReadDeadline(t time.Time) error { return p.file.SetReadDeadline(t) }
func (p unixSerialPort) Close() error                      { return p.file.Close() }

// serialBaudConstants 波特率 → termios 波特常量（linux 的波特率编在 Cflag
// 的 CBAUD 位域里）。白名单与 NormalizeSerialSensorConfig 共用 serialBaudRates。
var serialBaudConstants = map[int]uint32{
	2400:   unix.B2400,
	4800:   unix.B4800,
	9600:   unix.B9600,
	19200:  unix.B19200,
	38400:  unix.B38400,
	57600:  unix.B57600,
	115200: unix.B115200,
}

// configureSerial 把串口配成 8N1 raw 模式 + 指定波特率：传感器固件自发文本
// 行，禁用回显/规范模式/流控后按字节直达。VMIN=0 + VTIME=0.5s 是 deadline
// 不可用内核上的兜底；正常路径的读超时走 SetReadDeadline。
func configureSerial(file *os.File, baud int) error {
	constant, ok := serialBaudConstants[baud]
	if !ok {
		return fmt.Errorf("unsupported baud rate %d", baud)
	}
	termios, err := unix.IoctlGetTermios(int(file.Fd()), unix.TCGETS)
	if err != nil {
		return fmt.Errorf("tcgets: %w", err)
	}
	termios.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON | unix.IXOFF
	termios.Oflag &^= unix.OPOST
	termios.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	termios.Cflag &^= unix.CSIZE | unix.PARENB | unix.CSTOPB | unix.CBAUD
	termios.Cflag |= unix.CS8 | unix.CREAD | unix.CLOCAL | constant
	termios.Cc[unix.VMIN] = 0
	termios.Cc[unix.VTIME] = 5
	if err := unix.IoctlSetTermios(int(file.Fd()), unix.TCSETS, termios); err != nil {
		return fmt.Errorf("tcsets: %w", err)
	}
	// 清 DTR/RTS：open() 默认会把两条线拉起，Arduino 类板子把 DTR 接到
	// RESET——间歇轮询每次开口都复位一次传感器就读不到数了。清掉后开口
	// 不再触发对端复位（需要复位的烧录器自己会拉，不受影响）。
	// best-effort：个别 usb-serial 驱动/内核组合不支持这两个 ioctl（真机
	// CH340 出过 tiocmset 返回 EFAULT），丢掉的只是防复位优化，串口读数
	// 不受影响——静默放行，不能让整个传感器因此打不开。
	if bits, err := unix.IoctlGetInt(int(file.Fd()), unix.TIOCMGET); err == nil {
		bits &^= unix.TIOCM_DTR | unix.TIOCM_RTS
		_ = unix.IoctlSetInt(int(file.Fd()), unix.TIOCMSET, bits)
	}
	return nil
}
