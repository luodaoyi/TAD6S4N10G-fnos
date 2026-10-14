//go:build linux

package powerguard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 端到端（隔离 sys-root，不碰真实硬件）：读卡器要被重新识别，正在使用的
// 移动硬盘要被跳过，SCSI 重扫只落在真实存在的控制器上。
func TestRefreshSDCardRebindsReaderAndSkipsMountedDevice(t *testing.T) {
	fastSDCardWaits(t)

	root := t.TempDir()
	// 读卡器 2-1.4：没有卡在里面，authorized 初始为 0，应当被重新识别。
	sdcardMkdir(t, root, "sys/bus/usb/devices/2-1.4:1.0")
	sdcardWrite(t, root, "sys/bus/usb/devices/2-1.4:1.0/bInterfaceClass", "08\n")
	sdcardWrite(t, root, "sys/bus/usb/devices/2-1.4/authorized", "0\n")
	sdcardMkdir(t, root, "sys/bus/usb/devices/2-1.4/2-1.4:1.0/host6/target6:0:0/6:0:0:0/block/sdb")
	sdcardMkdir(t, root, "sys/class/scsi_host/host6")
	sdcardWrite(t, root, "sys/class/scsi_host/host6/scan", "")

	// 移动硬盘 2-1.3：上面挂着正在使用的分区，绝不能动它。
	sdcardMkdir(t, root, "sys/bus/usb/devices/2-1.3:1.0")
	sdcardWrite(t, root, "sys/bus/usb/devices/2-1.3:1.0/bInterfaceClass", "08\n")
	sdcardWrite(t, root, "sys/bus/usb/devices/2-1.3/authorized", "0\n")
	sdcardMkdir(t, root, "sys/bus/usb/devices/2-1.3/2-1.3:1.0/host7/target7:0:0/7:0:0:0/block/sdc")
	sdcardMkdir(t, root, "sys/class/scsi_host/host7")
	sdcardWrite(t, root, "sys/class/scsi_host/host7/scan", "")

	// 一个非存储类设备（键盘）不该被当成读卡器。
	sdcardMkdir(t, root, "sys/bus/usb/devices/1-2:1.0")
	sdcardWrite(t, root, "sys/bus/usb/devices/1-2:1.0/bInterfaceClass", "03\n")
	sdcardWrite(t, root, "sys/bus/usb/devices/1-2/authorized", "1\n")

	sdcardWrite(t, root, "proc/mounts", "/dev/sdc1 /mnt/usbdisk ext4 rw,relatime 0 0\n")
	sdcardWrite(t, root, "proc/partitions", "major minor  #blocks  name\n\n"+
		"   8        0  500000000 sda\n"+
		"   8        1  499000000 sda1\n"+
		"   8       16   30000000 sdc\n"+
		"   8       17   29900000 sdc1\n")
	// 设备名与容量快照：读卡器里没卡，所以 sdb 的容量是 0。
	sdcardWrite(t, root, "sys/block/sda/size", "500000000\n")
	sdcardWrite(t, root, "sys/block/sdb/size", "0\n")
	sdcardWrite(t, root, "sys/block/sdc/size", "30000000\n")

	manager := &Manager{Root: root}
	result, err := manager.RunRefreshSDCard(context.Background())
	if err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	if len(result.Devices) != 1 || result.Devices[0] != "2-1.4" {
		t.Fatalf("只该重新识别读卡器 2-1.4，实际 %+v", result.Devices)
	}
	if got := sdcardRead(t, root, "sys/bus/usb/devices/2-1.4/authorized"); got != "1" {
		t.Fatalf("读卡器应被重新授权为 1，实际 %q", got)
	}
	if got := sdcardRead(t, root, "sys/bus/usb/devices/2-1.3/authorized"); got != "0" {
		t.Fatalf("正在使用的移动硬盘不该被碰，实际 %q", got)
	}
	if got := sdcardRead(t, root, "sys/bus/usb/devices/1-2/authorized"); got != "1" {
		t.Fatalf("非存储类设备不该被碰，实际 %q", got)
	}
	if len(result.Skipped) != 1 || !strings.Contains(result.Skipped[0], "2-1.3") {
		t.Fatalf("应记录跳过正在使用的设备，实际 %+v", result.Skipped)
	}
	if result.ScannedHosts != 2 {
		t.Fatalf("应重扫 host6 与 host7，实际 %d", result.ScannedHosts)
	}
	if result.Summary == "" {
		t.Fatal("应给出可读摘要")
	}
	if result.NewDevices != nil {
		t.Fatalf("分区表没变化时不该报告新设备，实际 %+v", result.NewDevices)
	}
	if running, _, lastErr := manager.SdcardRefreshStatus(); running || lastErr != "" {
		t.Fatalf("刷新结束后状态应回到空闲，实际 running=%v err=%q", running, lastErr)
	}
}

func TestRefreshSDCardWithoutUSBStorage(t *testing.T) {
	fastSDCardWaits(t)

	root := t.TempDir()
	sdcardMkdir(t, root, "sys/bus/usb/devices")
	// 没有 USB 存储设备时退回扫 USB 控制器；SATA 控制器（ahci）必须不被惊动，
	// 那是系统盘所在的地方。
	sdcardMkdir(t, root, "sys/class/scsi_host/host0")
	sdcardWrite(t, root, "sys/class/scsi_host/host0/proc_name", "usb-storage\n")
	sdcardWrite(t, root, "sys/class/scsi_host/host0/scan", "")
	sdcardMkdir(t, root, "sys/class/scsi_host/host1")
	sdcardWrite(t, root, "sys/class/scsi_host/host1/proc_name", "ahci\n")
	sdcardWrite(t, root, "sys/class/scsi_host/host1/scan", "untouched")
	sdcardWrite(t, root, "proc/partitions", "major minor  #blocks  name\n\n")
	sdcardWrite(t, root, "proc/mounts", "")

	manager := &Manager{Root: root}
	result, err := manager.RunRefreshSDCard(context.Background())
	if err != nil {
		t.Fatalf("应该退化到只重扫控制器: %v", err)
	}
	if result.ScannedHosts != 1 || !strings.Contains(result.Summary, "没有检测到 USB 存储设备") {
		t.Fatalf("结果不正确: %+v", result)
	}
	if got := sdcardRead(t, root, "sys/class/scsi_host/host1/scan"); got != "untouched" {
		t.Fatalf("SATA 控制器不该被重扫，实际内容 %q", got)
	}
	if got := sdcardRead(t, root, "sys/class/scsi_host/host0/scan"); got != "- - -" {
		t.Fatalf("USB 控制器应被写入扫描指令，实际内容 %q", got)
	}
}

func TestRefreshSDCardFailsWithoutUSBTree(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{Root: root}
	if _, err := manager.RunRefreshSDCard(context.Background()); err == nil {
		t.Fatal("没有 USB 设备树时应报错，而不是假装成功")
	}
}

// 插好卡之后容量从 0 变成真实值，应当被识别出来并写进摘要。
func TestRefreshSDCardReportsNewlyReadMedium(t *testing.T) {
	fastSDCardWaits(t)

	root := t.TempDir()
	sdcardMkdir(t, root, "sys/bus/usb/devices/2-1.4:1.0")
	sdcardWrite(t, root, "sys/bus/usb/devices/2-1.4:1.0/bInterfaceClass", "08\n")
	sdcardWrite(t, root, "sys/bus/usb/devices/2-1.4/authorized", "1\n")
	sdcardMkdir(t, root, "sys/bus/usb/devices/2-1.4/2-1.4:1.0/host32")
	sdcardMkdir(t, root, "sys/class/scsi_host/host32")
	sdcardWrite(t, root, "sys/class/scsi_host/host32/proc_name", "usb-storage\n")
	sdcardWrite(t, root, "sys/class/scsi_host/host32/scan", "")
	sdcardWrite(t, root, "proc/mounts", "")
	// 空读卡器：设备节点在、容量是 0。
	sdcardWrite(t, root, "sys/block/sdc/size", "0\n")

	// 模拟内核在重新识别之后读出介质。
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(root, "sys/block/sdc/size"), []byte("250000000\n"), 0o644)
	}()

	manager := &Manager{Root: root}
	result, err := manager.RunRefreshSDCard(context.Background())
	if err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	if len(result.NewDevices) != 1 || !strings.HasPrefix(result.NewDevices[0], "sdc（") {
		t.Fatalf("应当报告读到了介质，实际 %+v", result.NewDevices)
	}
	if !strings.Contains(result.Summary, "识别到设备") {
		t.Fatalf("摘要应说明识别结果: %q", result.Summary)
	}
}

// 写 /sys 失败（例如权限不足）必须如实报错，不能混成「跳过」而让界面显示成功。
func TestRefreshSDCardFailsWhenSysfsNotWritable(t *testing.T) {
	fastSDCardWaits(t)

	root := t.TempDir()
	sdcardMkdir(t, root, "sys/bus/usb/devices/2-1.4:1.0")
	sdcardWrite(t, root, "sys/bus/usb/devices/2-1.4:1.0/bInterfaceClass", "08\n")
	// 用目录占住 authorized 的位置：写入必然失败。
	sdcardMkdir(t, root, "sys/bus/usb/devices/2-1.4/authorized")
	sdcardMkdir(t, root, "sys/class/scsi_host/host32")
	sdcardWrite(t, root, "sys/class/scsi_host/host32/proc_name", "usb-storage\n")
	sdcardWrite(t, root, "proc/mounts", "")

	manager := &Manager{Root: root}
	result, err := manager.RunRefreshSDCard(context.Background())
	if err == nil {
		t.Fatalf("所有设备都失败时必须报错，实际返回成功: %+v", result)
	}
	if len(result.Failed) != 1 {
		t.Fatalf("应记录失败的设备，实际 %+v", result.Failed)
	}
	if len(result.Devices) != 0 {
		t.Fatalf("没有设备成功时不该出现在 Devices 里: %+v", result.Devices)
	}
}

// 只有 SATA 控制器时不能假装刷新成功：那是系统盘的地方，什么也没做就该如实说。
func TestRefreshSDCardFailsWithOnlySATAHosts(t *testing.T) {
	root := t.TempDir()
	sdcardMkdir(t, root, "sys/bus/usb/devices")
	sdcardMkdir(t, root, "sys/class/scsi_host/host0")
	sdcardWrite(t, root, "sys/class/scsi_host/host0/proc_name", "ahci\n")
	sdcardWrite(t, root, "sys/class/scsi_host/host0/scan", "")
	sdcardWrite(t, root, "proc/partitions", "major minor  #blocks  name\n\n")
	sdcardWrite(t, root, "proc/mounts", "")

	manager := &Manager{Root: root}
	if _, err := manager.RunRefreshSDCard(context.Background()); err == nil {
		t.Fatal("没有 USB 存储设备也没有 USB 控制器时应报错")
	}
	if got := sdcardRead(t, root, "sys/class/scsi_host/host0/scan"); got != "" {
		t.Fatalf("SATA 控制器不该被触碰，实际内容 %q", got)
	}
}

// 按键走的是后台入口：以前 StartRefreshSDCard 先占了位，再去调会自己占位的
// RunRefreshSDCard，被自己设的标记拒掉——网络键短按永远只响一声失败长鸣。
func TestStartRefreshSDCardRunsToCompletion(t *testing.T) {
	fastSDCardWaits(t)

	root := t.TempDir()
	sdcardMkdir(t, root, "sys/bus/usb/devices/2-1.4:1.0")
	sdcardWrite(t, root, "sys/bus/usb/devices/2-1.4:1.0/bInterfaceClass", "08\n")
	sdcardWrite(t, root, "sys/bus/usb/devices/2-1.4/authorized", "0\n")
	sdcardMkdir(t, root, "sys/bus/usb/devices/2-1.4/2-1.4:1.0/host6")
	sdcardMkdir(t, root, "sys/class/scsi_host/host6")
	sdcardWrite(t, root, "sys/class/scsi_host/host6/scan", "")
	sdcardWrite(t, root, "proc/mounts", "")

	manager := &Manager{Root: root}
	if err := manager.StartRefreshSDCard(); err != nil {
		t.Fatalf("空闲时后台刷新应当被接受: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if running, _, _ := manager.SdcardRefreshStatus(); !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("后台刷新一直没有结束")
		}
		time.Sleep(2 * time.Millisecond)
	}
	running, summary, lastErr := manager.SdcardRefreshStatus()
	if running {
		t.Fatal("结束后不该还标记着正在运行")
	}
	if lastErr != "" {
		t.Fatalf("后台刷新不该失败，实际 %q", lastErr)
	}
	if summary == "" {
		t.Fatal("成功后应当写进摘要")
	}
	if got := sdcardRead(t, root, "sys/bus/usb/devices/2-1.4/authorized"); got != "1" {
		t.Fatalf("读卡器应被重新授权，实际 %q", got)
	}
}

// 只按 /proc/mounts 判断「正在使用」会把仍在服役的盘去授权：RAID 成员、LVM PV、
// dm-crypt 在挂载表里是 md/dm 的名字，swap 分区压根不出现。这些设备一律跳过。
func TestRefreshSDCardSkipsRAIDMemberAndSwap(t *testing.T) {
	fastSDCardWaits(t)

	root := t.TempDir()
	// 读卡器 2-1.4（sdb）：干净，应当被重新识别。
	sdcardWrite(t, root, "sys/bus/usb/devices/2-1.4:1.0/bInterfaceClass", "08\n")
	sdcardWrite(t, root, "sys/bus/usb/devices/2-1.4/authorized", "0\n")
	sdcardMkdir(t, root, "sys/bus/usb/devices/2-1.4/2-1.4:1.0/host6")
	sdcardMkdir(t, root, "sys/class/scsi_host/host6")
	sdcardWrite(t, root, "sys/class/scsi_host/host6/scan", "")

	// RAID 成员 2-1.3（sdc）：分区被 md0 占着，挂载表里查不到它。
	sdcardWrite(t, root, "sys/bus/usb/devices/2-1.3:1.0/bInterfaceClass", "08\n")
	sdcardWrite(t, root, "sys/bus/usb/devices/2-1.3/authorized", "1\n")
	sdcardMkdir(t, root, "sys/bus/usb/devices/2-1.3/2-1.3:1.0/host7/target7:0:0/7:0:0:0/block/sdc")
	sdcardMkdir(t, root, "sys/class/scsi_host/host7")
	sdcardWrite(t, root, "sys/class/scsi_host/host7/scan", "")
	sdcardMkdir(t, root, "sys/block/sdc/sdc1/holders/md0")

	// 交换分区 2-1.5（sdd）：出现在 /proc/swaps 里。
	sdcardWrite(t, root, "sys/bus/usb/devices/2-1.5:1.0/bInterfaceClass", "08\n")
	sdcardWrite(t, root, "sys/bus/usb/devices/2-1.5/authorized", "1\n")
	sdcardMkdir(t, root, "sys/bus/usb/devices/2-1.5/2-1.5:1.0/host8/target8:0:0/8:0:0:0/block/sdd")
	sdcardMkdir(t, root, "sys/class/scsi_host/host8")
	sdcardWrite(t, root, "sys/class/scsi_host/host8/scan", "")

	sdcardWrite(t, root, "proc/mounts", "")
	sdcardWrite(t, root, "proc/swaps", "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n"+
		"/dev/sdd2                               partition\t8000508\t\t0\t\t-2\n")

	manager := &Manager{Root: root}
	result, err := manager.RunRefreshSDCard(context.Background())
	if err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	if len(result.Devices) != 1 || result.Devices[0] != "2-1.4" {
		t.Fatalf("只该重新识别干净的读卡器，实际 %+v", result.Devices)
	}
	if got := sdcardRead(t, root, "sys/bus/usb/devices/2-1.3/authorized"); got != "1" {
		t.Fatalf("RAID 成员不该被去授权，实际 %q", got)
	}
	if got := sdcardRead(t, root, "sys/bus/usb/devices/2-1.5/authorized"); got != "1" {
		t.Fatalf("交换分区所在盘不该被去授权，实际 %q", got)
	}
	if len(result.Skipped) != 2 {
		t.Fatalf("两块盘都该被跳过，实际 %+v", result.Skipped)
	}
	joined := strings.Join(result.Skipped, " | ")
	if !strings.Contains(joined, "md0") || !strings.Contains(joined, "交换区") {
		t.Fatalf("跳过原因应写清是阵列还是交换区，实际 %q", joined)
	}
	if result.ScannedHosts != 3 {
		t.Fatalf("三个控制器都该被重扫，实际 %d", result.ScannedHosts)
	}
}

func TestSDCardHelpers(t *testing.T) {
	if deviceDir, name := usbDeviceFromInterface("/sys/bus/usb/devices/2-1.4:1.0", "2-1.4:1.0"); name != "2-1.4" ||
		deviceDir != "/sys/bus/usb/devices/2-1.4" {
		t.Fatalf("接口应还原到设备，实际 %q / %q", deviceDir, name)
	}
	// 极少见但要对：接口名里没有冒号时按目录本身处理。
	if _, name := usbDeviceFromInterface("/sys/bus/usb/devices/2-1.4", "2-1.4"); name != "2-1.4" {
		t.Fatalf("无冒号时应原样返回，实际 %q", name)
	}

	matches := []struct {
		device string
		blocks []string
		want   bool
	}{
		{"sdb", []string{"sdb"}, true},
		{"sdb1", []string{"sdb"}, true},
		{"sdc1", []string{"sdb"}, false},
		{"sdaa", []string{"sda"}, false}, // 前缀巧合但不是同一块盘
		{"mmcblk0p1", []string{"mmcblk0"}, false},
		{"nvme0n1p2", []string{"nvme0n1"}, false},
	}
	for _, test := range matches {
		if got := sdcardBlockMatches(test.device, test.blocks); got != test.want {
			t.Fatalf("%s 是否属于 %v：期望 %v，实际 %v", test.device, test.blocks, test.want, got)
		}
	}

	added := diffBlockSizes(
		map[string]int64{"sda": 100, "sdc": 0},
		map[string]int64{"sda": 100, "sdc": 0},
	)
	if len(added) != 0 {
		t.Fatalf("容量没变化时不该报告: %+v", added)
	}
	// 空读卡器（size=0）不算认出介质；插好卡后容量变正数才算。
	added = diffBlockSizes(
		map[string]int64{"sdc": 0},
		map[string]int64{"sdc": 250000000},
	)
	if len(added) != 1 || !strings.HasPrefix(added[0], "sdc（") {
		t.Fatalf("插好卡后应报告容量变化，实际 %+v", added)
	}
	// 换一张不同容量的卡：设备名没变，只有容量变，也要报告出来。
	added = diffBlockSizes(
		map[string]int64{"sdc": 125000000},
		map[string]int64{"sdc": 250000000},
	)
	if len(added) != 1 {
		t.Fatalf("换了不同容量的卡应当被识别，实际 %+v", added)
	}
	if got := formatBlockCapacity(250000000); got != "119.2 GiB" {
		t.Fatalf("容量格式化不正确: %q", got)
	}
	if got := formatBlockCapacity(2048); got != "1 MiB" {
		t.Fatalf("小容量格式化不正确: %q", got)
	}
}

// 界面上那个「刷新 SD 卡」按钮走的就是这个接口：应返回 200 与可读摘要。
func TestHandleRefreshSDCardReturnsSummary(t *testing.T) {
	fastSDCardWaits(t)

	root := t.TempDir()
	sdcardMkdir(t, root, "sys/bus/usb/devices/2-1.4:1.0")
	sdcardWrite(t, root, "sys/bus/usb/devices/2-1.4:1.0/bInterfaceClass", "08\n")
	sdcardWrite(t, root, "sys/bus/usb/devices/2-1.4/authorized", "1\n")
	sdcardMkdir(t, root, "sys/bus/usb/devices/2-1.4/2-1.4:1.0/host6")
	sdcardMkdir(t, root, "sys/class/scsi_host/host6")
	sdcardWrite(t, root, "sys/class/scsi_host/host6/scan", "")
	sdcardWrite(t, root, "proc/partitions", "major minor  #blocks  name\n\n")
	sdcardWrite(t, root, "proc/mounts", "")

	server := &Server{Manager: &Manager{Root: root}}
	request := httptest.NewRequest(http.MethodPost, "/api/gpio/refresh-sdcard", nil)
	request.Header.Set("X-Trim-Isadmin", "true")
	recorder := httptest.NewRecorder()
	server.handleRefreshSDCard(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	var result SdcardRefreshResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Summary == "" || len(result.Devices) != 1 || result.ScannedHosts != 1 {
		t.Fatalf("结果不正确: %+v", result)
	}
}

// fastSDCardWaits 把重新识别与等待介质的时间压到毫秒级，测试才能跑得快。
func fastSDCardWaits(t *testing.T) {
	t.Helper()
	rebind, settle, poll := sdcardRebindDelay, sdcardSettleDelay, sdcardPollInterval
	sdcardRebindDelay, sdcardSettleDelay, sdcardPollInterval = time.Millisecond, 300*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() {
		sdcardRebindDelay, sdcardSettleDelay, sdcardPollInterval = rebind, settle, poll
	})
}

func sdcardMkdir(t *testing.T, root, relative string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(relative)), 0o755); err != nil {
		t.Fatal(err)
	}
}

func sdcardWrite(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sdcardRead(t *testing.T, root, relative string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}
