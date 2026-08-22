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
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	StorageEmpty   = "empty"
	StoragePresent = "present"
	StorageUsed    = "used"
	StorageWarning = "warning"
	StorageUnknown = "unknown"

	StorageActivityBusy     = "busy"
	StorageActivityIdle     = "idle"
	StorageActivitySleeping = "sleeping"
	StorageActivityUnknown  = "unknown"
)

type StorageSlot struct {
	ID           string  `json:"id"`
	Kind         string  `json:"kind"`
	Slot         int     `json:"slot"`
	State        string  `json:"state"`
	Activity     string  `json:"activity,omitempty"`
	BusPath      string  `json:"bus_path"`
	Device       string  `json:"device,omitempty"`
	Model        string  `json:"model,omitempty"`
	Serial       string  `json:"serial,omitempty"`
	SizeBytes    int64   `json:"size_bytes,omitempty"`
	Purpose      string  `json:"purpose,omitempty"`
	Health       string  `json:"health,omitempty"`
	TemperatureC float64 `json:"temperature_c,omitempty"`
	Warning      string  `json:"warning,omitempty"`
	SMARTError   string  `json:"-"`
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
	Reads    uint64
	Writes   uint64
	InFlight uint64
}

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
	status := cloneStorageStatus(m.storageStatus)
	m.storageMu.RUnlock()
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
	for _, spec := range storageSlotSpecs {
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
		slot.Activity = m.readBlockActivity(kname)
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

func (m *Manager) readBlockActivity(kname string) string {
	data, err := os.ReadFile(filepath.Join(m.rooted("/sys/class/block"), kname, "stat"))
	if err != nil {
		return StorageActivityUnknown
	}
	fields := strings.Fields(string(data))
	if len(fields) < 9 {
		return StorageActivityUnknown
	}
	reads, readErr := strconv.ParseUint(fields[0], 10, 64)
	writes, writeErr := strconv.ParseUint(fields[4], 10, 64)
	inFlight, flightErr := strconv.ParseUint(fields[8], 10, 64)
	if readErr != nil || writeErr != nil || flightErr != nil {
		return StorageActivityUnknown
	}
	sample := blockIOSample{Reads: reads, Writes: writes, InFlight: inFlight}
	if m.storageIO == nil {
		m.storageIO = make(map[string]blockIOSample)
	}
	previous, hadPrevious := m.storageIO[kname]
	m.storageIO[kname] = sample
	if sample.InFlight > 0 || (hadPrevious && (sample.Reads != previous.Reads || sample.Writes != previous.Writes)) {
		return StorageActivityBusy
	}
	return StorageActivityIdle
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
