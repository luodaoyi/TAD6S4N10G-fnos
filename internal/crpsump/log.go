package crpsump

import (
	"fmt"
	"os"
	"sync"
)

// ---------------- 日志 (常开句柄 + O_NOFOLLOW + 按大小轮转) ----------------

var (
	logMu   sync.Mutex
	logFile *os.File
)

// ensureLogFile 懒打开日志文件并进程级复用 (调用方须持 logMu)
func ensureLogFile() *os.File {
	if logFile != nil {
		return logFile
	}
	// openNoFollow (Linux): 拒绝把日志写入符号链接目标(防多用户环境下被劫持)
	f, err := os.OpenFile(LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY|openNoFollow, 0600)
	if err != nil {
		return nil
	}
	logFile = f
	return f
}

// Logf 输出一行日志到 stdout 并写入固定日志文件 LogPath, 供包内与 backend 入口共用
func Logf(format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	fmt.Println(line)
	logMu.Lock()
	defer logMu.Unlock()
	f := ensureLogFile()
	if f == nil {
		return
	}
	if st, err := f.Stat(); err == nil && st.Size() > LogMaxSize {
		f.Close()
		os.Rename(LogPath, LogPath+".1") // 轮转: 当前日志重命名为 .1
		logFile = nil
		f = ensureLogFile()
		if f == nil {
			return
		}
	}
	f.WriteString(line + "\n")
}
