package powerguard

// 归档冲刷性能探针：默认跳过（避免拖慢 CI），TAD_ARCHIVE_BENCH=1 时运行。
// 目的：量化 CSV.gz 归档（含同月二次归并路径）在满配 30 天数据上的耗时、
// 内存增量与产物体积。压缩只发生在归档冲刷时刻（每约 6 天到一个月一次），
// 每分钟采样路径零新增开销；低端 ARM（A53 级）按实测量级再慢 3~5× 估算。

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestArchiveFlushPerformanceProbe(t *testing.T) {
	if os.Getenv("TAD_ARCHIVE_BENCH") == "" {
		t.Skip("set TAD_ARCHIVE_BENCH=1 to run the archive flush performance probe")
	}
	store := newTestStore(t)
	defer store.Close()
	store.archiveEnabled = true
	store.archiveDir = t.TempDir()

	// 满配 30 天：4 风扇 + 6 盘位 + 12 传感器，ID 逐个区分（同 ID 会被主键
	// 去重，量不出满配行数），温度缓变+读数抖动拟真
	const samples = 43200
	base := time.Now().Add(-30 * 24 * time.Hour).Truncate(time.Minute)
	rng := rand.New(rand.NewSource(1))
	value := func(v float64) float64 { return v + rng.Float64()*0.4 - 0.2 }
	start := time.Now()
	for i := 0; i < samples; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		sample := HistorySample{TS: ts.Unix(), CPUC: value(52), HDDC: value(38), NVMeC: value(45)}
		for f := 0; f < 4; f++ {
			sample.Fans = append(sample.Fans, HistoryFanSample{ID: fmt.Sprintf("it8613:fan%d", f), RPM: int64(1150 + rng.Intn(60)), PWMPercent: 45})
		}
		for d := 0; d < 6; d++ {
			sample.Disks = append(sample.Disks, HistoryDiskSample{ID: fmt.Sprintf("front-%d", d), TemperatureC: value(36)})
		}
		for s := 0; s < 12; s++ {
			sample.Sensors = append(sample.Sensors, HistorySensorSample{Group: "cpu", Key: fmt.Sprintf("Core %d", s), C: value(50)})
		}
		if err := store.Append(sample); err != nil {
			t.Fatal(err)
		}
	}
	buildElapsed := time.Since(start)

	lo, hi := base.Unix(), base.Add(30*24*time.Hour).Unix()
	gcAndRead := func() runtime.MemStats {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m
	}
	before := gcAndRead()
	flushStart := time.Now()
	if err := store.archiveBetween(lo, hi); err != nil {
		t.Fatal(err)
	}
	firstFlush := time.Since(flushStart)
	firstMem := gcAndRead()

	// 同月二次归并（跨月/大小清理留下的第二批）：走"读旧+归并重写"路径，
	// 最后一小时作为重叠批次，内存峰值与耗时都以这条路径为准。
	mergeStart := time.Now()
	if err := store.archiveBetween(hi-3600, hi); err != nil {
		t.Fatal(err)
	}
	mergeFlush := time.Since(mergeStart)
	mergeMem := gcAndRead()

	entries, err := filepath.Glob(filepath.Join(store.archiveDir, "tad-history-*.csv.gz"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("expect month files, got %v (err=%v)", entries, err)
	}
	var archiveBytes int64
	for _, entry := range entries {
		info, err := os.Stat(entry)
		if err != nil {
			t.Fatal(err)
		}
		archiveBytes += info.Size()
	}

	t.Logf("samples=%d rows≈%d build=%s db=%dMB",
		samples, samples*23, buildElapsed.Round(time.Millisecond), store.dbSizeBytes()>>20)
	mib := func(delta uint64) float64 { return float64(delta) / (1 << 20) }
	t.Logf("first flush(流式写)=%s heapΔ=%.0fMB | merge flush(读旧归并)=%s heapΔ=%.0fMB allocΔ=%.0fMB",
		firstFlush.Round(time.Millisecond), mib(firstMem.HeapAlloc-before.HeapAlloc),
		mergeFlush.Round(time.Millisecond),
		float64(int64(mergeMem.HeapAlloc)-int64(firstMem.HeapAlloc))/(1<<20),
		mib(mergeMem.TotalAlloc-firstMem.TotalAlloc))
	t.Logf("archive files=%d total=%dB（跨自然月各归各的文件）", len(entries), archiveBytes)
}
