package powerguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	StorageEmpty   = "empty"
	StoragePresent = "present"
	StorageUsed    = "used"
	StorageWarning = "warning"
	StorageUnknown = "unknown"

	StorageActivityBusy     = "busy"
	StorageActivityWorking  = "working"
	StorageActivityIdle     = "idle"
	StorageActivitySleeping = "sleeping"
	StorageActivityUnknown  = "unknown"

	storageIdleUtilMax    = 0.0
	storageWorkingUtilMax = 70.0
)

type StorageSlot struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	Slot         int      `json:"slot"`
	State        string   `json:"state"`
	Activity     string   `json:"activity,omitempty"`
	Utilization  *float64 `json:"utilization_percent,omitempty"`
	BusPath      string   `json:"bus_path"`
	Device       string   `json:"device,omitempty"`
	Model        string   `json:"model,omitempty"`
	Serial       string   `json:"serial,omitempty"`
	SizeBytes    int64    `json:"size_bytes,omitempty"`
	Purpose      string   `json:"purpose,omitempty"`
	Health       string   `json:"health,omitempty"`
	TemperatureC float64  `json:"temperature_c,omitempty"`
	Warning      string   `json:"warning,omitempty"`
	SMARTError   string   `json:"-"`
}

type StorageStatus struct {
	Slots     []StorageSlot `json:"slots"`
	UpdatedAt time.Time     `json:"updated_at,omitempty"`
	LastError string        `json:"last_error,omitempty"`
}

type storageSlotSpec struct {
	ID      string
	Kind    string
	Slot    int
	BusPath string
	Prefix  bool
}

// TAD6S4N10G 的仓位模板：绝对 PCI 地址只在出厂总线拓扑下成立（例如拔掉
// 与盘位共用同一 PCIe 上游交换芯片的网卡后，内核重新编号会使这些路径失效）。
// 真正的解析优先走 discoverSlotBusPaths（SATA 按端口拓扑推导，M.2 按根端口
// 走线表锚定）；此表仅提供读不到 sysfs 时的兜底路径与稳定的仓位 ID/数量。
var storageSlotSpecs = []storageSlotSpec{
	{ID: "front-1", Kind: "front", Slot: 1, BusPath: "/dev/disk/by-path/pci-0000:02:00.0-ata-1"},
	{ID: "front-2", Kind: "front", Slot: 2, BusPath: "/dev/disk/by-path/pci-0000:02:00.0-ata-2"},
	{ID: "front-3", Kind: "front", Slot: 3, BusPath: "/dev/disk/by-path/pci-0000:02:00.0-ata-3"},
	{ID: "front-4", Kind: "front", Slot: 4, BusPath: "/dev/disk/by-path/pci-0000:02:00.0-ata-4"},
	{ID: "front-5", Kind: "front", Slot: 5, BusPath: "/dev/disk/by-path/pci-0000:02:00.0-ata-5"},
	{ID: "front-6", Kind: "front", Slot: 6, BusPath: "/dev/disk/by-path/pci-0000:02:00.0-ata-6"},
	{ID: "m2-1", Kind: "m2", Slot: 1, BusPath: "/dev/disk/by-path/pci-0000:04:00.0-nvme-", Prefix: true},
	{ID: "m2-2", Kind: "m2", Slot: 2, BusPath: "/dev/disk/by-path/pci-0000:05:00.0-nvme-", Prefix: true},
	{ID: "m2-3", Kind: "m2", Slot: 3, BusPath: "/dev/disk/by-path/pci-0000:06:00.0-nvme-", Prefix: true},
	{ID: "m2-4", Kind: "m2", Slot: 4, BusPath: "/dev/disk/by-path/pci-0000:07:00.0-nvme-", Prefix: true},
}

var pciAddrPattern = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-9a-f]$`)

// bayRootPorts 是实测的 M.2 仓位走线：仓 ↔ PCH 根端口是主板布线，总线重
// 编号、换盘、空仓都不变，是唯一稳定的仓位锚点（2026-09 丝印实测：2 号仓
// 的盘挂 1d.0、3 号仓挂 1d.2、4 号仓挂 1d.3、1 号仓空。注意 1、2 号仓与
// 端口顺序交错，与整机 addon 的 dts 定义不同；空仓的根端口不被固件枚举，
// 也无法靠扫描建表，只能固化实测值）。
var bayRootPorts = map[string]string{
	"m2-1": "0000:00:1d.1",
	"m2-2": "0000:00:1d.0",
	"m2-3": "0000:00:1d.2",
	"m2-4": "0000:00:1d.3",
}

// discoverSlotBusPaths 从 sysfs 相对拓扑推导当前各仓位的 by-path 前缀。
// SATA：取注册 ATA 端口数最多的控制器（TAD6S4N10G 为六口），按端口号升序
// 对应 front-1..N，端口注册与是否插盘无关；M.2：按 bayRootPorts 走线表把
// NVMe 控制器锚回对应仓。读不到 sysfs 时返回空表，调用方沿用模板固定路径。
func (m *Manager) discoverSlotBusPaths() map[string]string {
	overrides := make(map[string]string)
	if controller, ports := bestSATAController(m.discoverSATAControllers()); controller != "" {
		if count := min(len(ports), 6); count != 0 {
			for index, port := range ports[:count] {
				overrides[fmt.Sprintf("front-%d", index+1)] = fmt.Sprintf("/dev/disk/by-path/pci-%s-ata-%d", controller, port)
			}
		}
	}
	if endpoints := m.discoverNVMeEndpoints(); len(endpoints) != 0 {
		for id, path := range assignNVMeBays(endpoints) {
			overrides[id] = path
		}
	}
	return overrides
}

func (m *Manager) discoverSATAControllers() map[int]string {
	dir := m.rooted("/sys/class/ata_port")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	ports := make(map[int]string)
	for _, entry := range entries {
		port, parseErr := strconv.Atoi(strings.TrimPrefix(entry.Name(), "ata"))
		if parseErr != nil || port <= 0 {
			continue
		}
		resolved, linkErr := filepath.EvalSymlinks(filepath.Join(dir, entry.Name()))
		if linkErr != nil {
			continue
		}
		addr := lastPCIComponent(resolved)
		if addr != "" {
			ports[port] = addr
		}
	}
	return ports
}

func bestSATAController(ports map[int]string) (string, []int) {
	counts := make(map[string]int)
	portNumbers := make(map[string][]int)
	for port, addr := range ports {
		counts[addr]++
		portNumbers[addr] = append(portNumbers[addr], port)
	}
	best := ""
	bestCount := 0
	for addr, count := range counts {
		if count > bestCount || (count == bestCount && best != "" && addr < best) {
			best = addr
			bestCount = count
		}
	}
	if best == "" {
		return "", nil
	}
	numbers := portNumbers[best]
	sort.Ints(numbers)
	return best, numbers
}

type nvmeEndpoint struct {
	RootPort string
	BusPath  string
}

// discoverNVMeEndpoints 枚举在位的 NVMe 控制器端点：控制器自身的 PCI 地址
// （BusPath 用）与其直接上游根端口（仓位锚点用）。空仓控制器不出现在
// /sys/class/nvme，空仓根端口也不被固件枚举，所以端点列表只包含插盘的仓。
func (m *Manager) discoverNVMeEndpoints() []nvmeEndpoint {
	dir := m.rooted("/sys/class/nvme")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	endpoints := make([]nvmeEndpoint, 0, len(entries))
	for _, entry := range entries {
		resolved, linkErr := filepath.EvalSymlinks(filepath.Join(dir, entry.Name()))
		if linkErr != nil {
			continue
		}
		addr := lastPCIComponent(resolved)
		if addr == "" {
			continue
		}
		endpoints = append(endpoints, nvmeEndpoint{
			RootPort: parentPCIComponent(resolved),
			BusPath:  fmt.Sprintf("/dev/disk/by-path/pci-%s-nvme-", addr),
		})
	}
	return endpoints
}

// assignNVMeBays 按走线表把 NVMe 端点锚回 M.2 仓：根端口对得上即认定该盘
// 在该仓。表内端口没有端点的仓输出必然不存在的占位路径——既判定为空仓，
// 也兜住模板固定路径在总线重编号后撞上别的盘、把空仓显示成幻影盘的风险。
// 挂在未知根端口上的盘（如 PCIe 转接卡）不属于任何 M.2 仓，不参与显示。
func assignNVMeBays(endpoints []nvmeEndpoint) map[string]string {
	assigned := make(map[string]string, len(bayRootPorts))
	for _, spec := range storageSlotSpecs {
		if spec.Kind != "m2" {
			continue
		}
		root := bayRootPorts[spec.ID]
		if root == "" {
			continue
		}
		assigned[spec.ID] = fmt.Sprintf("/dev/disk/by-path/pci-%s-empty-nvme-", root)
		for _, endpoint := range endpoints {
			if endpoint.RootPort == root {
				assigned[spec.ID] = endpoint.BusPath
				break
			}
		}
	}
	return assigned
}

func lastPCIComponent(path string) string {
	last := ""
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if pciAddrPattern.MatchString(part) {
			last = part
		}
	}
	return last
}

// parentPCIComponent 取路径中最后一个 PCI 地址的直接上游 PCI 地址；对
// 直挂根端口的设备即主板走线根端口。
func parentPCIComponent(path string) string {
	prev := ""
	last := ""
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if pciAddrPattern.MatchString(part) {
			prev = last
			last = part
		}
	}
	return prev
}

type blockInfo struct {
	KName     string
	SizeBytes int64
	Model     string
	Serial    string
	Used      bool
	Purposes  []string
}

type lsblkDocument struct {
	BlockDevices []lsblkNode `json:"blockdevices"`
}

type lsblkNode struct {
	Name        string      `json:"name"`
	KName       string      `json:"kname"`
	Type        string      `json:"type"`
	Size        json.Number `json:"size"`
	Model       string      `json:"model"`
	Serial      string      `json:"serial"`
	FSType      string      `json:"fstype"`
	MountPoints []string    `json:"mountpoints"`
	Children    []lsblkNode `json:"children"`
}

type smartResult struct {
	Passed         *bool
	TemperatureC   float64
	Critical       int64
	PercentageUsed int64
	PowerMode      string
}

type blockIOSample struct {
	Reads      uint64
	Writes     uint64
	ReadTicks  uint64
	WriteTicks uint64
	IOTicks    uint64
	InFlight   uint64
	StallCount int
	SampledAt  time.Time
	Util       float64
	HasUtil    bool
}

var blockIOStates sync.Map

func (m *Manager) RefreshStorage(forceSMART bool) StorageStatus {
	m.storageScanMu.Lock()
	defer m.storageScanMu.Unlock()
	status := m.collectStorage(forceSMART)
	m.storageMu.Lock()
	m.storageStatus = status
	m.storageMu.Unlock()
	return cloneStorageStatus(status)
}

func (m *Manager) StorageStatus() StorageStatus {
	m.RefreshStorageActivity()
	m.storageMu.RLock()
	defer m.storageMu.RUnlock()
	status := cloneStorageStatus(m.storageStatus)
	if len(status.Slots) != 0 {
		return status
	}
	status.LastError = "硬盘仓位尚未完成首次刷新"
	for _, spec := range storageSlotSpecs {
		status.Slots = append(status.Slots, StorageSlot{
			ID: spec.ID, Kind: spec.Kind, Slot: spec.Slot,
			State: StorageUnknown, BusPath: spec.BusPath,
		})
	}
	return status
}

func (m *Manager) RefreshStorageActivity() {
	m.storageMu.Lock()
	defer m.storageMu.Unlock()
	for index := range m.storageStatus.Slots {
		slot := &m.storageStatus.Slots[index]
		if slot.Device == "" || slot.State == StorageEmpty || slot.Activity == StorageActivitySleeping {
			continue
		}
		slot.Activity, slot.Utilization = m.readBlockActivity(filepath.Base(slot.Device), slot.Kind)
	}
}

func cloneStorageStatus(status StorageStatus) StorageStatus {
	status.Slots = append([]StorageSlot(nil), status.Slots...)
	return status
}

func (m *Manager) collectStorage(forceSMART bool) StorageStatus {
	status := StorageStatus{UpdatedAt: time.Now()}
	byPathDir := m.rooted("/dev/disk/by-path")
	if _, err := os.Stat(byPathDir); err != nil {
		status.LastError = fmt.Sprintf("无法读取硬盘总线路径: %v", err)
		for _, spec := range storageSlotSpecs {
			status.Slots = append(status.Slots, StorageSlot{
				ID: spec.ID, Kind: spec.Kind, Slot: spec.Slot,
				State: StorageUnknown, BusPath: spec.BusPath,
			})
		}
		return status
	}

	blocks, topologyErr := m.readBlockTopology()
	if topologyErr != nil {
		status.LastError = combineError(status.LastError, topologyErr)
	}
	overrides := m.discoverSlotBusPaths()
	for _, spec := range storageSlotSpecs {
		if base, ok := overrides[spec.ID]; ok {
			spec.BusPath = base
		}
		slot := StorageSlot{
			ID: spec.ID, Kind: spec.Kind, Slot: spec.Slot,
			State: StorageEmpty, BusPath: spec.BusPath,
		}
		device, kname, err := m.resolveStorageDevice(spec)
		if errors.Is(err, fs.ErrNotExist) {
			status.Slots = append(status.Slots, slot)
			continue
		}
		if err != nil {
			slot.State = StorageUnknown
			slot.Warning = err.Error()
			status.LastError = combineError(status.LastError, fmt.Errorf("%s: %w", spec.ID, err))
			status.Slots = append(status.Slots, slot)
			continue
		}
		slot.State = StoragePresent
		slot.Device = device
		info, ok := blocks[kname]
		if !ok {
			info = m.readBlockInfoFromSysfs(kname)
		}
		slot.Model = strings.TrimSpace(info.Model)
		slot.Serial = strings.TrimSpace(info.Serial)
		slot.SizeBytes = info.SizeBytes
		if info.Used {
			slot.State = StorageUsed
		}
		slot.Purpose = strings.Join(info.Purposes, "、")
		slot.Activity, slot.Utilization = m.readBlockActivity(kname, spec.Kind)
		if warning := m.mdWarning(info.Purposes); warning != "" {
			slot.State = StorageWarning
			slot.Warning = warning
		}
		status.Slots = append(status.Slots, slot)
	}

	smartctl, err := findSmartctl()
	if err != nil {
		status.LastError = combineError(status.LastError, err)
		return status
	}
	for index := range status.Slots {
		slot := &status.Slots[index]
		if slot.Device == "" || slot.State == StorageUnknown {
			continue
		}
		result, err := readSMART(smartctl, slot.Device, slot.Kind == "front", forceSMART)
		if err != nil {
			slot.Health = "未读取"
			slot.SMARTError = err.Error()
			continue
		}
		slot.TemperatureC = result.TemperatureC
		if powerModeIsSleeping(result.PowerMode) {
			slot.Activity = StorageActivitySleeping
			slot.Utilization = nil
		}
		slot.Health = "正常"
		var warnings []string
		if result.Passed != nil && !*result.Passed {
			warnings = append(warnings, "SMART 健康检查未通过")
		}
		if result.Critical != 0 {
			warnings = append(warnings, fmt.Sprintf("NVMe critical_warning=%d", result.Critical))
		}
		if result.PercentageUsed >= 100 {
			warnings = append(warnings, fmt.Sprintf("NVMe 寿命使用率 %d%%", result.PercentageUsed))
		}
		if result.Passed == nil && result.Critical == 0 {
			slot.Health = "未读取"
		}
		if len(warnings) != 0 {
			slot.State = StorageWarning
			slot.Health = "告警"
			slot.Warning = strings.Trim(strings.Join([]string{slot.Warning, strings.Join(warnings, "；")}, "；"), "；")
		}
	}
	return status
}

func (m *Manager) resolveStorageDevice(spec storageSlotSpec) (string, string, error) {
	actual := m.rooted(spec.BusPath)
	if spec.Prefix {
		matches, _ := filepath.Glob(actual + "*")
		actual = ""
		for _, match := range matches {
			if strings.Contains(filepath.Base(match), "-part") {
				continue
			}
			actual = match
			break
		}
		if actual == "" {
			return "", "", fs.ErrNotExist
		}
	}
	if _, err := os.Lstat(actual); err != nil {
		return "", "", err
	}
	resolved, err := filepath.EvalSymlinks(actual)
	if err != nil {
		return "", "", fmt.Errorf("解析 %s: %w", spec.BusPath, err)
	}
	kname := filepath.Base(resolved)
	if kname == "" || kname == "." || kname == string(filepath.Separator) {
		return "", "", fmt.Errorf("%s 没有有效块设备", spec.BusPath)
	}
	return "/dev/" + kname, kname, nil
}

func (m *Manager) readBlockTopology() (map[string]blockInfo, error) {
	if m.Root != "" && m.Root != "/" {
		return map[string]blockInfo{}, nil
	}
	path, err := exec.LookPath("lsblk")
	if err != nil {
		return nil, errors.New("未找到 lsblk，无法判断硬盘是否已使用")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "-J", "-b", "-o", "NAME,KNAME,TYPE,SIZE,MODEL,SERIAL,FSTYPE,MOUNTPOINTS").Output()
	if err != nil {
		return nil, fmt.Errorf("执行 lsblk: %w", err)
	}
	return parseLSBLK(output)
}

func parseLSBLK(data []byte) (map[string]blockInfo, error) {
	var document lsblkDocument
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("解析 lsblk JSON: %w", err)
	}
	result := make(map[string]blockInfo)
	for _, node := range document.BlockDevices {
		if node.KName == "" {
			node.KName = node.Name
		}
		if node.Type != "disk" {
			continue
		}
		info := blockInfo{KName: node.KName, Model: node.Model, Serial: node.Serial}
		info.SizeBytes, _ = strconv.ParseInt(string(node.Size), 10, 64)
		purposes := make(map[string]bool)
		inspectLSBLKNode(node, &info.Used, purposes)
		for purpose := range purposes {
			info.Purposes = append(info.Purposes, purpose)
		}
		sort.Strings(info.Purposes)
		result[node.KName] = info
	}
	return result, nil
}

func inspectLSBLKNode(node lsblkNode, used *bool, purposes map[string]bool) {
	for _, mount := range node.MountPoints {
		mount = strings.TrimSpace(mount)
		if mount == "" {
			continue
		}
		*used = true
		if mount == "/" {
			purposes["系统盘"] = true
		} else {
			purposes["挂载 "+mount] = true
		}
	}
	if node.FSType != "" {
		*used = true
		if node.FSType == "linux_raid_member" {
			purposes["RAID 成员"] = true
		}
	}
	if strings.HasPrefix(node.Type, "raid") {
		*used = true
		name := node.KName
		if name == "" {
			name = node.Name
		}
		purposes[name] = true
	}
	for _, child := range node.Children {
		inspectLSBLKNode(child, used, purposes)
	}
}

func (m *Manager) readBlockInfoFromSysfs(kname string) blockInfo {
	info := blockInfo{KName: kname}
	base := filepath.Join(m.rooted("/sys/class/block"), kname)
	info.Model, _ = readTrim(filepath.Join(base, "device", "model"))
	info.Serial, _ = readTrim(filepath.Join(base, "device", "serial"))
	if sectors, err := readInt(filepath.Join(base, "size")); err == nil {
		info.SizeBytes = sectors * 512
	}
	return info
}

func activityFromUtilization(util float64) string {
	if util > storageWorkingUtilMax {
		return StorageActivityBusy
	}
	if util > storageIdleUtilMax {
		return StorageActivityWorking
	}
	return StorageActivityIdle
}

func storageUtilization(previous, sample blockIOSample) *float64 {
	elapsedMS := sample.SampledAt.Sub(previous.SampledAt).Milliseconds()
	if elapsedMS <= 0 || sample.IOTicks < previous.IOTicks {
		return nil
	}
	util := float64(sample.IOTicks-previous.IOTicks) / float64(elapsedMS) * 100
	if util < 0 {
		util = 0
	}
	if util > 100 {
		util = 100
	}
	return &util
}

func loadBlockIO(kname string) (blockIOSample, bool) {
	value, ok := blockIOStates.Load(kname)
	if !ok {
		return blockIOSample{}, false
	}
	sample, ok := value.(blockIOSample)
	return sample, ok
}

func storeBlockIO(kname string, sample blockIOSample) {
	blockIOStates.Store(kname, sample)
}

func utilizationPointer(sample blockIOSample) *float64 {
	if !sample.HasUtil {
		return nil
	}
	value := sample.Util
	return &value
}

func (m *Manager) readBlockActivity(kname, kind string) (string, *float64) {
	data, err := os.ReadFile(filepath.Join(m.rooted("/sys/class/block"), kname, "stat"))
	if err != nil {
		return StorageActivityUnknown, nil
	}
	fields := strings.Fields(string(data))
	if len(fields) < 10 {
		return StorageActivityUnknown, nil
	}
	reads, readErr := strconv.ParseUint(fields[0], 10, 64)
	writes, writeErr := strconv.ParseUint(fields[4], 10, 64)
	readTicks, readTickErr := strconv.ParseUint(fields[3], 10, 64)
	writeTicks, writeTickErr := strconv.ParseUint(fields[7], 10, 64)
	inFlight, flightErr := strconv.ParseUint(fields[8], 10, 64)
	ioTicks, ioTickErr := strconv.ParseUint(fields[9], 10, 64)
	if readErr != nil || writeErr != nil || readTickErr != nil || writeTickErr != nil || flightErr != nil || ioTickErr != nil {
		return StorageActivityUnknown, nil
	}
	sample := blockIOSample{
		Reads: reads, Writes: writes, ReadTicks: readTicks, WriteTicks: writeTicks,
		IOTicks: ioTicks, InFlight: inFlight, SampledAt: time.Now(),
	}
	previous, hadPrevious := loadBlockIO(kname)
	_ = kind
	if !hadPrevious {
		storeBlockIO(kname, sample)
		return StorageActivityIdle, nil
	}
	if util := storageUtilization(previous, sample); util != nil {
		sample.Util = *util
		sample.HasUtil = true
	} else {
		sample.Util = previous.Util
		sample.HasUtil = previous.HasUtil
	}
	storeBlockIO(kname, sample)
	if !sample.HasUtil {
		return StorageActivityIdle, nil
	}
	state := activityFromUtilization(sample.Util)
	out := utilizationPointer(sample)
	if state == StorageActivityIdle {
		zero := 0.0
		return state, &zero
	}
	return state, out
}

func powerModeIsSleeping(mode string) bool {
	mode = strings.ToLower(strings.TrimSpace(mode))
	return strings.Contains(mode, "standby") || strings.Contains(mode, "sleep")
}

func (m *Manager) mdWarning(purposes []string) string {
	for _, purpose := range purposes {
		if !strings.HasPrefix(purpose, "md") {
			continue
		}
		degraded, err := readInt(filepath.Join(m.rooted("/sys/class/block"), purpose, "md", "degraded"))
		if err == nil && degraded > 0 {
			return fmt.Sprintf("%s 有 %d 个降级成员", purpose, degraded)
		}
	}
	return ""
}

func findSmartctl() (string, error) {
	for _, candidate := range []string{"/usr/sbin/smartctl", "/usr/bin/smartctl"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	path, err := exec.LookPath("smartctl")
	if err != nil {
		return "", errors.New("未找到 smartctl，仓位可显示但无法读取 SMART")
	}
	return path, nil
}

func readSMART(smartctl, device string, standbyAware, force bool) (smartResult, error) {
	args := []string{"-j"}
	if standbyAware && !force {
		args = append(args, "-n", "standby")
	}
	args = append(args, "-H", "-A", device)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	output, commandErr := exec.CommandContext(ctx, smartctl, args...).CombinedOutput()
	result, parseErr := parseSMART(output)
	if parseErr == nil {
		return result, nil
	}
	if commandErr != nil {
		return smartResult{}, fmt.Errorf("smartctl %s: %w", device, commandErr)
	}
	return smartResult{}, parseErr
}

func parseSMART(data []byte) (smartResult, error) {
	var document struct {
		SmartStatus struct {
			Passed *bool `json:"passed"`
		} `json:"smart_status"`
		Temperature struct {
			Current float64 `json:"current"`
		} `json:"temperature"`
		NVMe struct {
			CriticalWarning int64   `json:"critical_warning"`
			Temperature     float64 `json:"temperature"`
			PercentageUsed  int64   `json:"percentage_used"`
		} `json:"nvme_smart_health_information_log"`
		PowerMode string `json:"power_mode"`
		Smartctl  struct {
			ExitStatus int64 `json:"exit_status"`
			Messages   []struct {
				String string `json:"string"`
			} `json:"messages"`
		} `json:"smartctl"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return smartResult{}, fmt.Errorf("解析 SMART JSON: %w", err)
	}
	temperature := document.Temperature.Current
	if temperature == 0 {
		temperature = document.NVMe.Temperature
	}
	// smartctl -n standby reports sleeping drives via smartctl.messages
	// ("Device is in STANDBY mode, exit(2)") rather than a power_mode field.
	powerMode := strings.TrimSpace(document.PowerMode)
	if powerMode == "" {
		for _, msg := range document.Smartctl.Messages {
			lower := strings.ToLower(msg.String)
			switch {
			case strings.Contains(lower, "standby"):
				powerMode = "standby"
			case strings.Contains(lower, "sleep"):
				powerMode = "sleep"
			}
			if powerMode != "" {
				break
			}
		}
	}
	return smartResult{
		Passed: document.SmartStatus.Passed, TemperatureC: temperature,
		Critical: document.NVMe.CriticalWarning, PercentageUsed: document.NVMe.PercentageUsed,
		PowerMode: powerMode,
	}, nil
}
