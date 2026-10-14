//go:build linux

package powerguard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// sdcard_linux.go：在飞牛（Linux）上重新识别 USB 读卡器。
//
// 做法与作者原版 RR 插件 tad6s4n10g 的 sdcMain() 一致：把读卡器 USB 设备的
// authorized 置 0 再置 1，等效于「拔掉再插上」，内核会重新枚举 LUN；再对
// 相关 SCSI 控制器写一次 scan 作为兜底。区别是原版把 PCI 路径写死成 DSM 虚拟机
// 的 0000:00:0d.0/usb2/2-1/2-1.4，这里全部从 USB 设备树动态发现。
//
// 安全边界：
//   - 只处理 USB 接口类 08（大容量存储），绝不碰 SATA/NVMe 上的系统盘；
//   - 上面还挂着正在使用的分区（/proc/mounts 里出现）的设备直接跳过，
//     免得把正在读写的移动硬盘踢下线；
//   - 除了直接挂载，被 md/dm（RAID、LVM、dm-crypt）占用的块设备、以及被用作
//     交换区的分区也算「正在使用」——它们在 /proc/mounts 里显示为 /dev/md*、
//     /dev/mapper/* 或压根不出现，只按挂载点判断会把仍在服役的盘去授权，
//     轻则阵列降级、重则卷掉线。

const (
	// sdcardUSBMassStorageClass 是 bInterfaceClass 里的大容量存储类别。
	sdcardUSBMassStorageClass = "08"
	// sdcardMaxDevices 是一次刷新最多处理的设备数，防止异常情况下无限循环。
	sdcardMaxDevices = 16
)

var (
	// sdcardRebindDelay 是 authorized 置 0 与置 1 之间的间隔，给设备栈一点时间。
	sdcardRebindDelay = 1 * time.Second
	// sdcardSettleDelay 是重新授权之后等待介质被重新读出的最长时间；一旦读到就提前返回。
	// 实测空读卡器的块设备 size 为 0，插卡并识别成功后才会变成真实容量，所以不一定
	// 要等满——但慢的读卡器确实要几秒，所以上限放宽到 6 秒。
	sdcardSettleDelay = 6 * time.Second
	// sdcardPollInterval 是等待介质期间轮询块设备容量的间隔。
	sdcardPollInterval = 250 * time.Millisecond
)

type sdcardUSBDevice struct {
	name    string   // 设备名，如 2-1.4
	sysPath string   // /sys/bus/usb/devices/2-1.4
	hosts   []string // 关联的 SCSI 控制器，如 host6
	blocks  []string // 关联的块设备，如 sdb
	mounts  []string // 该设备上正在使用的挂载点描述
}

func (m *Manager) refreshSDCard(ctx context.Context) (SdcardRefreshResult, error) {
	result := SdcardRefreshResult{}
	devices, err := m.usbStorageDevices()
	if err != nil {
		return result, err
	}

	before := m.blockSnapshot()

	// 同型号多口读卡器会有共同前缀，已识别到的控制器的集合
	hosts := make(map[string]bool)
	rebound := 0
	for _, device := range devices {
		for _, host := range device.hosts {
			hosts[host] = true
		}
		if len(device.mounts) > 0 {
			result.Skipped = append(result.Skipped,
				fmt.Sprintf("%s 正在使用（%s）", device.name, strings.Join(device.mounts, "、")))
			continue
		}
		if rebindErr := m.rebindUSBDevice(ctx, device); rebindErr != nil {
			// 写 /sys 失败（例如权限不够）是失败，不能混进「跳过」里让界面显示成功。
			result.Failed = append(result.Failed, fmt.Sprintf("%s（%v）", device.name, rebindErr))
			continue
		}
		result.Devices = append(result.Devices, device.name)
		rebound++
	}

	result.ScannedHosts = m.rescanSCSIHosts(hosts)

	if rebound > 0 {
		// 等内核把介质重新读出来：认到就立刻返回，认不到才等满上限。
		result.NewDevices = m.waitForNewBlocks(ctx, before)
	}

	if len(devices) == 0 {
		if result.ScannedHosts == 0 {
			return result, fmt.Errorf("没有检测到 USB 读卡器或 USB 存储设备")
		}
		result.Summary = fmt.Sprintf("没有检测到 USB 存储设备，已重新扫描 %d 个 SCSI 控制器",
			result.ScannedHosts)
		return result, nil
	}
	if len(result.Devices) == 0 && len(result.Failed) > 0 {
		// 一个都没成功就别报喜：界面会拿 error 显示失败原因。
		return result, fmt.Errorf("刷新SD卡失败：%s", strings.Join(result.Failed, "；"))
	}
	result.Summary = sdcardSummary(result)
	return result, nil
}

// waitForNewBlocks 轮询块设备容量，看介质有没有被重新读出来；认到立刻返回。
func (m *Manager) waitForNewBlocks(ctx context.Context, before map[string]int64) []string {
	deadline := time.Now().Add(sdcardSettleDelay)
	for {
		if changed := diffBlockSizes(before, m.blockSnapshot()); len(changed) > 0 {
			return changed
		}
		if time.Now().After(deadline) {
			return nil
		}
		if !waitWithContext(ctx, sdcardPollInterval) {
			return nil
		}
	}
}

func sdcardSummary(result SdcardRefreshResult) string {
	var builder strings.Builder
	if len(result.Devices) == 0 {
		builder.WriteString("没有需要重新识别的 USB 读卡器")
	} else {
		fmt.Fprintf(&builder, "已重新识别 %d 个 USB 读卡器（%s）",
			len(result.Devices), strings.Join(result.Devices, "、"))
	}
	if len(result.Skipped) > 0 {
		fmt.Fprintf(&builder, "；跳过 %d 个（%s）", len(result.Skipped), strings.Join(result.Skipped, "；"))
	}
	if len(result.Failed) > 0 {
		fmt.Fprintf(&builder, "；%d 个失败（%s）", len(result.Failed), strings.Join(result.Failed, "；"))
	}
	switch {
	case len(result.NewDevices) > 0:
		fmt.Fprintf(&builder, "；识别到设备：%s", strings.Join(result.NewDevices, "、"))
	case len(result.Devices) > 0:
		builder.WriteString("；没有读到存储介质，如果卡没插好请插好后重试")
	}
	return builder.String()
}

// usbStorageDevices 从 USB 设备树里找出所有大容量存储设备（读卡器、U 盘、
// 移动硬盘都算），并带上它关联的 SCSI 控制器、块设备与正在使用的挂载点。
func (m *Manager) usbStorageDevices() ([]sdcardUSBDevice, error) {
	base := m.rooted("/sys/bus/usb/devices")
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("当前系统没有 USB 设备树（%s）", base)
		}
		return nil, fmt.Errorf("无法读取 USB 设备列表：%v", err)
	}
	seen := make(map[string]bool)
	devices := make([]sdcardUSBDevice, 0, 4)
	for _, entry := range entries {
		interfaceDir := filepath.Join(base, entry.Name())
		class, err := readTrim(filepath.Join(interfaceDir, "bInterfaceClass"))
		if err != nil || class != sdcardUSBMassStorageClass {
			continue
		}
		deviceDir, name := usbDeviceFromInterface(interfaceDir, entry.Name())
		if name == "" || seen[name] {
			continue
		}
		if _, err := os.Stat(filepath.Join(deviceDir, "authorized")); err != nil {
			// 没有 authorized 就没法重新识别，交给 SCSI 重扫兜底。
			continue
		}
		if len(devices) >= sdcardMaxDevices {
			break
		}
		seen[name] = true
		device := sdcardUSBDevice{name: name, sysPath: deviceDir}
		device.hosts, device.blocks = m.sdcardBlockDevices(deviceDir)
		device.mounts = m.sdcardActiveMounts(device.blocks)
		devices = append(devices, device)
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].name < devices[j].name })
	return devices, nil
}

// usbDeviceFromInterface 把接口条目（2-1.4:1.0）还原成它所属的 USB 设备
// （2-1.4）：sysfs 里 bInterfaceClass 只出现在接口层，而 authorized 在设备层。
// 注意 /sys/bus/usb/devices/ 下这些条目都是符号链接，接口的父目录是 devices
// 本身而不是设备，所以设备路径必须靠「去掉冒号后缀再拼回同一目录」得到。
func usbDeviceFromInterface(interfaceDir, entryName string) (string, string) {
	if index := strings.Index(entryName, ":"); index > 0 {
		name := entryName[:index]
		return filepath.Join(filepath.Dir(interfaceDir), name), name
	}
	return interfaceDir, entryName
}

// sdcardBlockDevices 找出该 USB 设备下的块设备与 SCSI 控制器名。
// 典型路径：/sys/bus/usb/devices/2-1.4/2-1.4:1.0/host6/target6:0:0/6:0:0:0/block/sdb
// 控制器优先用 host* 目录来找：读卡器里没插卡时块设备路径不存在，host 仍然在。
func (m *Manager) sdcardBlockDevices(deviceDir string) (hosts []string, blocks []string) {
	seenHost := make(map[string]bool)
	hostMatches, _ := filepath.Glob(filepath.Join(deviceDir, "*", "host*"))
	for _, match := range hostMatches {
		if name := filepath.Base(match); name != "" && !seenHost[name] {
			seenHost[name] = true
			hosts = append(hosts, name)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(deviceDir, "*", "*", "*", "*", "block", "*"))
	sort.Strings(matches)
	for _, match := range matches {
		if block := filepath.Base(match); block != "" {
			blocks = append(blocks, block)
		}
		relative, err := filepath.Rel(deviceDir, match)
		if err != nil {
			continue
		}
		for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
			if strings.HasPrefix(part, "host") && !seenHost[part] {
				seenHost[part] = true
				hosts = append(hosts, part)
			}
		}
	}
	sort.Strings(hosts)
	sort.Strings(blocks)
	return hosts, blocks
}

// sdcardActiveMounts 返回该 USB 设备上正在被使用的证据（有则不动它）。
//
// 三类都算「正在使用」：
//  1. /proc/mounts 上直接挂着它；
//  2. 被 md（RAID 成员）、dm（LVM PV、dm-crypt）等上层设备占用 —— 看
//     /sys/block/<blk>/holders/* 与各分区 /sys/block/<blk>/<blk>N/holders/*；
//  3. 分区被用作交换区 —— 看 /proc/swaps。
//
// 后两类在挂载表里要么是 /dev/md*、/dev/mapper/*，要么根本查不到，只按第 1 类
// 判断就会去授权一块还在服役的盘。
func (m *Manager) sdcardActiveMounts(blocks []string) []string {
	if len(blocks) == 0 {
		return nil
	}
	found := make([]string, 0, 4)
	found = append(found, m.sdcardMountedPoints(blocks)...)
	found = append(found, m.sdcardBlockHolders(blocks)...)
	found = append(found, m.sdcardSwapDevices(blocks)...)
	sort.Strings(found)
	return found
}

// sdcardMountedPoints 返回直接挂在 /proc/mounts 上的设备/分区。
func (m *Manager) sdcardMountedPoints(blocks []string) []string {
	data, err := os.ReadFile(m.rooted("/proc/mounts"))
	if err != nil {
		return nil
	}
	var found []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "/dev/") {
			continue
		}
		name := strings.TrimPrefix(fields[0], "/dev/")
		if sdcardBlockMatches(name, blocks) {
			found = append(found, fmt.Sprintf("%s 挂载在 %s", fields[0], fields[1]))
		}
	}
	return found
}

// sdcardBlockHolders 返回占用这些块设备（或其分区）的上层设备，如 md0、dm-0。
// 只读 /sys，不碰任何设备。
func (m *Manager) sdcardBlockHolders(blocks []string) []string {
	var found []string
	seen := make(map[string]bool)
	for _, block := range blocks {
		if block == "" {
			continue
		}
		patterns := []string{
			m.rooted(filepath.Join("/sys/block", block, "holders", "*")),
			// 分区层的 holders：sdb1/holders/md0 这种。
			m.rooted(filepath.Join("/sys/block", block, block+"*", "holders", "*")),
		}
		for _, pattern := range patterns {
			matches, _ := filepath.Glob(pattern)
			for _, match := range matches {
				holder := filepath.Base(match)
				if holder == "" || holder == "." || holder == "/" {
					continue
				}
				key := block + "|" + holder
				if seen[key] {
					continue
				}
				seen[key] = true
				found = append(found, fmt.Sprintf("%s 被 %s 占用", block, holder))
			}
		}
	}
	return found
}

// sdcardSwapDevices 返回被当作交换区使用的分区。空交换文件（无 /proc/swaps
// 条目）与表头行都会被自然跳过。
func (m *Manager) sdcardSwapDevices(blocks []string) []string {
	data, err := os.ReadFile(m.rooted("/proc/swaps"))
	if err != nil {
		return nil
	}
	var found []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "/dev/") {
			continue
		}
		name := strings.TrimPrefix(fields[0], "/dev/")
		if sdcardBlockMatches(name, blocks) {
			found = append(found, fmt.Sprintf("%s 用作交换区", fields[0]))
		}
	}
	return found
}

// sdcardBlockMatches 判断某个 /dev 下的名字（sdb1）是否属于这些块设备（sdb）。
func sdcardBlockMatches(deviceName string, blocks []string) bool {
	for _, block := range blocks {
		if block == "" {
			continue
		}
		if deviceName == block {
			return true
		}
		if strings.HasPrefix(deviceName, block) && sdcardAllDigits(deviceName[len(block):]) {
			return true
		}
	}
	return false
}

func sdcardAllDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// rebindUSBDevice 把设备去授权再授权：内核会当成一次热插拔，重新读取介质。
func (m *Manager) rebindUSBDevice(ctx context.Context, device sdcardUSBDevice) error {
	authorized := filepath.Join(device.sysPath, "authorized")
	if err := writeAndVerify(authorized, 0); err != nil {
		return fmt.Errorf("关闭设备失败：%v", err)
	}
	if !waitWithContext(ctx, sdcardRebindDelay) {
		// 上下文取消也要把设备放回去，不能让它停在关闭状态。
		_ = writeAndVerify(authorized, 1)
		return ctx.Err()
	}
	if err := writeAndVerify(authorized, 1); err != nil {
		return fmt.Errorf("重新启用设备失败：%v", err)
	}
	return nil
}

// rescanSCSIHosts 让 SCSI 控制器重新扫描。优先只扫与读卡器相关的控制器；
// 定位不到时退回扫描 USB 相关的控制器（proc_name 带 usb），绝不惊动 SATA/NVMe
// ——那上面是系统盘与数据盘，重扫没有意义。
func (m *Manager) rescanSCSIHosts(preferred map[string]bool) int {
	names := make([]string, 0, len(preferred))
	for name := range preferred {
		names = append(names, name)
	}
	if len(names) == 0 {
		names = m.usbSCSIHosts()
	}
	sort.Strings(names)
	scanned := 0
	for _, name := range names {
		path := m.rooted(filepath.Join("/sys/class/scsi_host", name, "scan"))
		if _, err := os.Stat(path); err != nil {
			continue
		}
		// 标准的「全通道全目标全 LUN」写法；个别控制器不支持就跳过。
		if err := os.WriteFile(path, []byte("- - -"), 0o600); err != nil {
			continue
		}
		scanned++
	}
	return scanned
}

// usbSCSIHosts 列出 USB 存储控制器（usb-storage / uas）。
func (m *Manager) usbSCSIHosts() []string {
	matches, _ := filepath.Glob(m.rooted("/sys/class/scsi_host/host*"))
	var names []string
	for _, match := range matches {
		procName, err := readTrim(filepath.Join(match, "proc_name"))
		if err != nil || !strings.Contains(strings.ToLower(procName), "usb") {
			continue
		}
		names = append(names, filepath.Base(match))
	}
	sort.Strings(names)
	return names
}

// blockSnapshot 记录当前块设备名与容量（以 512 字节扇区计）。
//
// 判断「卡有没有被重新读出来」看容量：实测空读卡器的块设备 size 是 0（设备节点
// /dev/sdc 都在，但不在 /proc/partitions 里），插好卡并识别成功后 size 才变成
// 真实容量。只看 /proc/partitions 是不够的——换一张容量不同的卡时设备名不变，
// 只有容量会变。
func (m *Manager) blockSnapshot() map[string]int64 {
	result := make(map[string]int64)
	matches, _ := filepath.Glob(m.rooted("/sys/block/*"))
	for _, match := range matches {
		name := filepath.Base(match)
		sectors, err := readInt(filepath.Join(match, "size"))
		if err != nil {
			continue
		}
		result[name] = sectors
	}
	return result
}

// diffBlockSizes 返回新出现或容量发生变化的块设备；容量为 0 的（空读卡器）不算。
func diffBlockSizes(before, after map[string]int64) []string {
	changed := make([]string, 0, 2)
	for name, size := range after {
		if size <= 0 {
			continue
		}
		if old, existed := before[name]; !existed || old != size {
			changed = append(changed, fmt.Sprintf("%s（%s）", name, formatBlockCapacity(size)))
		}
	}
	sort.Strings(changed)
	return changed
}

// formatBlockCapacity 已移到跨平台的 sdcard.go（存储插入提示也要用它）。

// waitWithContext 睡一会儿，但能被取消；返回 false 表示等待被打断。
func waitWithContext(ctx context.Context, duration time.Duration) bool {
	if ctx == nil {
		time.Sleep(duration)
		return true
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
