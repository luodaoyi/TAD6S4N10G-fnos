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

	storageWorkingUtilMax    = 70.0
	storageWorkingDisplayMin = 0.5
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
	ID             string
	Kind           string
	Slot           int
	BusPath        string
	Prefix         bool
	MappingKnown   bool
	MissingUnknown bool
	Warning        string
}

var storageSlotSpecs = []storageSlotSpec{
	{ID: "front-1", Kind: "front", Slot: 1},
	{ID: "front-2", Kind: "front", Slot: 2},
	{ID: "front-3", Kind: "front", Slot: 3},
	{ID: "front-4", Kind: "front", Slot: 4},
	{ID: "front-5", Kind: "front", Slot: 5},
	{ID: "front-6", Kind: "front", Slot: 6},
	{ID: "m2-1", Kind: "m2", Slot: 1},
	{ID: "m2-2", Kind: "m2", Slot: 2},
	{ID: "m2-3", Kind: "m2", Slot: 3},
	{ID: "m2-4", Kind: "m2", Slot: 4},
}

type storagePCIDevice struct {
	BDF      string
	Vendor   string
	Device   string
	Class    string
	Topology string
}

var pciBDFPattern = regexp.MustCompile(`^[[:xdigit:]]{4}:[[:xdigit:]]{2}:[[:xdigit:]]{2}\.[0-7]$`)

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
	IOTicks   uint64
	InFlight  uint64
	SampledAt time.Time
	Util      float64
	HasUtil   bool
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
	runtimeSpecs, mappingErr := m.discoverStorageSlotSpecs()
	if mappingErr != nil {
		status.LastError = combineError(status.LastError, mappingErr)
	}
	for _, spec := range runtimeSpecs {
		slot := StorageSlot{
			ID: spec.ID, Kind: spec.Kind, Slot: spec.Slot,
			State: StorageEmpty, BusPath: spec.BusPath,
		}
		if !spec.MappingKnown {
			slot.State = StorageUnknown
			slot.Warning = spec.Warning
			status.LastError = combineError(status.LastError, fmt.Errorf("%s: %s", spec.ID, spec.Warning))
			status.Slots = append(status.Slots, slot)
			continue
		}
		if spec.BusPath == "" {
			status.Slots = append(status.Slots, slot)
			continue
		}
		device, kname, err := m.resolveStorageDevice(spec)
		if errors.Is(err, fs.ErrNotExist) {
			if spec.MissingUnknown {
				slot.State = StorageUnknown
				slot.Warning = spec.Warning
				status.LastError = combineError(status.LastError, fmt.Errorf("%s: %s", spec.ID, spec.Warning))
			}
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

func (m *Manager) discoverStorageSlotSpecs() ([]storageSlotSpec, error) {
	devices, err := m.readStoragePCIDevices()
	if err != nil {
		specs := cloneStorageSlotSpecs()
		for index := range specs {
			specs[index].Warning = "无法读取 PCI 拓扑，不能确定物理仓位"
		}
		return specs, fmt.Errorf("读取 PCI 拓扑: %w", err)
	}
	return mapStorageSlotSpecs(devices), nil
}

func cloneStorageSlotSpecs() []storageSlotSpec {
	return append([]storageSlotSpec(nil), storageSlotSpecs...)
}

func mapStorageSlotSpecs(devices []storagePCIDevice) []storageSlotSpec {
	specs := cloneStorageSlotSpecs()

	// The board routes M.2 1..4 through the stable 00:1d.0..3 root ports.
	// Leaf BDFs are assigned after PCI enumeration and can move with the mlx5 card.
	var asmControllers []storagePCIDevice
	var intelSATA *storagePCIDevice
	nvmeBySlot := make(map[int][]storagePCIDevice)
	for index := range devices {
		device := &devices[index]
		if pciID(device.Vendor) == "1b21" && pciID(device.Device) == "1166" && pciClass(device.Class) == "0106" {
			asmControllers = append(asmControllers, *device)
		}
		if pciID(device.Vendor) == "8086" && pciClass(device.Class) == "0106" && strings.HasSuffix(strings.ToLower(device.BDF), ":00:17.0") {
			intelSATA = device
		}
		if pciClass(device.Class) == "0108" {
			if slot, ok := tadM2SlotFromTopology(device.Topology); ok {
				nvmeBySlot[slot] = append(nvmeBySlot[slot], *device)
			}
		}
	}

	for index := 0; index < 6; index++ {
		spec := &specs[index]
		switch len(asmControllers) {
		case 1:
			spec.MappingKnown = true
			spec.BusPath = fmt.Sprintf("/dev/disk/by-path/pci-%s-ata-%d", strings.ToLower(asmControllers[0].BDF), spec.Slot)
		case 0:
			spec.Warning = "未识别到 ASM1166 SATA 控制器，不能确定前置仓位"
		default:
			spec.Warning = "检测到多个 ASM1166 SATA 控制器，不能确定前置仓位"
		}
	}

	for slot := 1; slot <= 4; slot++ {
		spec := &specs[5+slot]
		controllers := nvmeBySlot[slot]
		switch len(controllers) {
		case 1:
			spec.MappingKnown = true
			spec.MissingUnknown = true
			spec.Prefix = true
			spec.BusPath = "/dev/disk/by-path/pci-" + strings.ToLower(controllers[0].BDF) + "-nvme-"
			spec.Warning = "已识别 NVMe 控制器，但没有找到对应块设备路径"
		case 0:
			if slot >= 3 && intelSATA != nil {
				spec.MappingKnown = true
				spec.BusPath = fmt.Sprintf("/dev/disk/by-path/pci-%s-ata-%d", strings.ToLower(intelSATA.BDF), slot-2)
			} else {
				// Empty M.2 root ports may not be enumerated by firmware. The fixed
				// board wiring still makes an absent endpoint an authoritative empty bay.
				spec.MappingKnown = true
			}
		default:
			spec.Warning = fmt.Sprintf("M.2 %d 根端口下检测到多个 NVMe 控制器，不能确定仓位", slot)
		}
	}
	return specs
}

func (m *Manager) readStoragePCIDevices() ([]storagePCIDevice, error) {
	base := m.rooted("/sys/bus/pci/devices")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	devices := make([]storagePCIDevice, 0, len(entries))
	for _, entry := range entries {
		bdf := strings.ToLower(entry.Name())
		if !pciBDFPattern.MatchString(bdf) {
			continue
		}
		path := filepath.Join(base, entry.Name())
		vendor, _ := readTrim(filepath.Join(path, "vendor"))
		deviceID, _ := readTrim(filepath.Join(path, "device"))
		class, _ := readTrim(filepath.Join(path, "class"))
		topology, resolveErr := filepath.EvalSymlinks(path)
		if resolveErr != nil {
			topology = path
		}
		devices = append(devices, storagePCIDevice{
			BDF: bdf, Vendor: vendor, Device: deviceID, Class: class,
			Topology: filepath.ToSlash(topology),
		})
	}
	return devices, nil
}

func pciID(value string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(value)), "0x")
}

func pciClass(value string) string {
	value = pciID(value)
	if len(value) < 4 {
		return value
	}
	return value[:4]
}

func tadM2RootSlot(bdf string) (int, bool) {
	bdf = strings.ToLower(strings.TrimSpace(bdf))
	for index := 0; index < 4; index++ {
		if strings.HasSuffix(bdf, fmt.Sprintf(":00:1d.%d", index)) {
			return index + 1, true
		}
	}
	return 0, false
}

func tadM2SlotFromTopology(topology string) (int, bool) {
	parts := strings.FieldsFunc(strings.ToLower(filepath.ToSlash(topology)), func(r rune) bool {
		return r == '/'
	})
	for _, part := range parts {
		if slot, ok := tadM2RootSlot(part); ok {
			return slot, true
		}
	}
	return 0, false
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
	// 低于 0.5 的繁忙度经前端 Math.round 显示为 0%，按 README 约定归为空闲。
	if util >= storageWorkingDisplayMin {
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
	inFlight, flightErr := strconv.ParseUint(fields[8], 10, 64)
	ioTicks, ioTickErr := strconv.ParseUint(fields[9], 10, 64)
	if flightErr != nil || ioTickErr != nil {
		return StorageActivityUnknown, nil
	}
	sample := blockIOSample{
		IOTicks: ioTicks, InFlight: inFlight, SampledAt: time.Now(),
	}
	previous, hadPrevious := loadBlockIO(kname)
	_ = kind
	if !hadPrevious {
		storeBlockIO(kname, sample)
		if inFlight > 0 {
			return StorageActivityBusy, nil
		}
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
