package crpsump

import (
	"os"
	"sync"
)

// ---------------- 端口 I/O (/dev/port 单字节, 进程级句柄常开) ----------------

// portIO 抽象 /dev/port 单字节读写, 便于单测 mock (默认 realPortIO 走真实硬件)
type portIO interface {
	inb(port int) byte
	outb(port int, v byte)
}

type realPortIO struct{}

// inb 真实硬件端口读 (转发到包级 inb)
func (realPortIO) inb(port int) byte    { return inb(port) }

// outb 真实硬件端口写 (转发到包级 outb)
func (realPortIO) outb(port int, v byte) { outb(port, v) }

// defaultPortIO 为 NewBus 创建的总线默认端口实现 (单测可替换为 mock)
var defaultPortIO portIO = realPortIO{}

// portAvailable 探测 /dev/port 是否可打开 (单测可替换)
var portAvailable = func() bool {
	_, err := portHandle()
	return err == nil
}

// PortIOLocker 串行化 SuperIO 配置空间 (0x2E/0x2F) 与 GPIO 数据口的读-改-写序列。
// 宿主应用 (powerguard) 在挂载本板块前注入其与按键轮询/LED 反馈共用的锁,
// 防止 bitbang 与宿主 GPIO 访问并发交错; 未注入时 (独立运行) 用包内私有锁。
// 锁约定为叶子锁: 持锁期间不得再获取其他锁 (Bus.mu 等在拿本锁之前持有)。
var PortIOLocker sync.Locker = &sync.Mutex{}

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

// inb 从 /dev/port 读一个字节; 常驻句柄失效时退化为临时打开 (失败返回 0xFF)
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

// outb 向 /dev/port 写一个字节; 常驻句柄失效时退化为临时打开
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

// ---------------- SuperIO LDN7 配置访问 (调用方须持 PortIOLocker) ----------------

// sioEnter 进入 SuperIO 配置模式
func sioEnter(p portIO) { p.outb(0x2E, 0x87); p.outb(0x2E, 0x01); p.outb(0x2E, 0x55); p.outb(0x2E, 0x55) }

// sioExit 退出 SuperIO 配置模式
func sioExit(p portIO)  { p.outb(0x2E, 0xAA) }

// sioRead 读 SuperIO 配置寄存器
func sioRead(p portIO, reg byte) byte {
	p.outb(0x2E, reg)
	return p.inb(0x2F)
}

// sioWrite 写 SuperIO 配置寄存器
func sioWrite(p portIO, reg, v byte) {
	p.outb(0x2E, reg)
	p.outb(0x2F, v&0xFF)
}
