package crpsump

import (
	"os"
	"sync"
)

// ---------------- 端口 I/O (/dev/port 单字节, 进程级句柄常开) ----------------

var (
	portMu   sync.Mutex
	portFile *os.File
)

// portHandle 懒加载 /dev/port 句柄; 打开成功则进程级复用, 失败每次重试
func portHandle() (*os.File, error) {
	portMu.Lock()
	defer portMu.Unlock()
	if portFile != nil {
		return portFile, nil
	}
	f, err := os.OpenFile("/dev/port", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	portFile = f
	return f, nil
}

func inb(port int) byte {
	f, err := portHandle()
	if err != nil {
		f, err = os.OpenFile("/dev/port", os.O_RDWR, 0)
		if err != nil {
			return 0xFF
		}
		defer f.Close()
	}
	buf := []byte{0}
	if _, err := f.ReadAt(buf, int64(port)); err != nil {
		return 0xFF
	}
	return buf[0]
}

func outb(port int, v byte) {
	f, err := portHandle()
	if err != nil {
		f, err = os.OpenFile("/dev/port", os.O_RDWR, 0)
		if err != nil {
			return
		}
		defer f.Close()
	}
	f.WriteAt([]byte{v & 0xFF}, int64(port))
}

// ---------------- SuperIO LDN7 配置访问 ----------------

func sioEnter() { outb(0x2E, 0x87); outb(0x2E, 0x01); outb(0x2E, 0x55); outb(0x2E, 0x55) }
func sioExit()  { outb(0x2E, 0xAA) }
func sioRead(reg byte) byte {
	outb(0x2E, reg)
	return inb(0x2F)
}
func sioWrite(reg, v byte) {
	outb(0x2E, reg)
	outb(0x2F, v&0xFF)
}
