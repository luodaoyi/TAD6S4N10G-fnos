package powerguard

// 守护进程日志的大小清理：与历史数据库共用用户设定的"数据库大小上限"
// （max_size_mb），超限即清理，不做按天轮转——日志量正常时一年也到不了
// 上限，只有持续错误刷屏才需要兜底，大小触发足够。
//
// 清理方式是 copytruncate：先把当前内容另存 <log>.1（覆盖上一代，保留一份
// 排障现场——触发清理的往往正是那段错误日志），再就地截断原文件。打开中的
// O_APPEND 文件描述符在截断后继续从文件头追加，无需换 fd，天然并发安全；
// 复制与截断之间新写入的几条日志会被截掉，属可接受的日志级误差。
// 总磁盘占用 ≤ 2×上限（主文件 + 上一代）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"
)

const (
	logRotateCheckEvery = 10 * time.Minute // 低频检查：错误刷屏的最坏增速下也远跑不赢
	logRotateSuffix     = ".1"
	// 写入路径兜底的检查频率：每 N 次写入 stat 一次。定时轮转才是常规清理
	// （带备份）；写入兜底只防"错误刷屏在两次定时检查之间跑赢上限"的极端
	// 情形，就地截断不备份。
	logSizeCapCheckEvery = 64
)

// SizeCappedLogWriter 包住日志文件的写入端：每 logSizeCapCheckEvery 次写入
// 检查一次文件大小，超限就地截断。O_APPEND fd 在截断后继续从文件头追加，
// 与定时轮转（先备份再截断）并发安全。
type SizeCappedLogWriter struct {
	file  *os.File
	limit func() int64
	pends int
}

// NewSizeCappedLogWriter 构造写入兜底；limit 为 nil 或返回 <=0 表示不限制。
func NewSizeCappedLogWriter(file *os.File, limit func() int64) *SizeCappedLogWriter {
	return &SizeCappedLogWriter{file: file, limit: limit}
}

func (w *SizeCappedLogWriter) Write(p []byte) (int, error) {
	n, err := w.file.Write(p)
	w.pends++
	if w.pends < logSizeCapCheckEvery {
		return n, err
	}
	w.pends = 0
	if w.limit == nil {
		return n, err
	}
	limit := w.limit()
	if limit <= 0 {
		return n, err
	}
	if info, statErr := w.file.Stat(); statErr == nil && info.Size() > limit {
		_ = w.file.Truncate(0)
	}
	return n, err
}

// RotateLogIfNeeded 检查日志文件是否超过 limitBytes，超限则把当前内容备份到
// <logPath>.1（覆盖旧备份）并就地截断；返回是否发生了清理。limitBytes<=0 或
// 文件未打开时不做任何事；备份失败时保持原文件不动（宁可超限也不丢日志）。
func RotateLogIfNeeded(logPath string, file *os.File, limitBytes int64) (bool, error) {
	if limitBytes <= 0 || file == nil || logPath == "" {
		return false, nil
	}
	info, err := os.Stat(logPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat log: %w", err)
	}
	if info.Size() <= limitBytes {
		return false, nil
	}
	if err := copyFileAtomic(logPath, logPath+logRotateSuffix); err != nil {
		return false, fmt.Errorf("backup log: %w", err)
	}
	if err := file.Truncate(0); err != nil {
		return false, fmt.Errorf("truncate log: %w", err)
	}
	return true, nil
}

// copyFileAtomic 把 src 复制到 dst：先写同目录临时文件再 rename 替换，
// 备份中途断电不会留下半个 .1。
func copyFileAtomic(src, dst string) error {
	tmp := dst + ".tmp"
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// LogRotateLoop 挂在守护进程主 ctx 上周期检查日志大小；limitBytes 每次现取
// （用户改大小上限后最迟 10 分钟生效）。发生清理时在新文件头记一条说明，
// 方便从日志本身看出发生过截断。
func LogRotateLoop(ctx context.Context, logger *log.Logger, logPath string, file *os.File, limitBytes func() int64) {
	if file == nil || logPath == "" {
		return
	}
	ticker := time.NewTicker(logRotateCheckEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		limit := limitBytes()
		rotated, err := RotateLogIfNeeded(logPath, file, limit)
		if err != nil {
			logger.Printf("log rotation check failed: %v", err)
			continue
		}
		if rotated {
			logger.Printf("log exceeded %d MB, previous content saved to %s%s", limit>>20, filepath.Base(logPath), logRotateSuffix)
		}
	}
}
