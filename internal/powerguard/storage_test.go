package powerguard

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStorageSlotMappingMatchesTAD6S4N10G(t *testing.T) {
	if len(storageSlotSpecs) != 10 {
		t.Fatalf("got %d slots, want 10", len(storageSlotSpecs))
	}
	if storageSlotSpecs[0].ID != "front-1" || storageSlotSpecs[0].BusPath != "/dev/disk/by-path/pci-0000:02:00.0-ata-1" {
		t.Fatalf("unexpected front slot 1 mapping: %+v", storageSlotSpecs[0])
	}
	if storageSlotSpecs[5].ID != "front-6" || storageSlotSpecs[5].BusPath != "/dev/disk/by-path/pci-0000:02:00.0-ata-6" {
		t.Fatalf("unexpected front slot 6 mapping: %+v", storageSlotSpecs[5])
	}
	if storageSlotSpecs[6].ID != "m2-1" || storageSlotSpecs[6].BusPath != "/dev/disk/by-path/pci-0000:04:00.0-nvme-" {
		t.Fatalf("unexpected M.2 slot 1 mapping: %+v", storageSlotSpecs[6])
	}
	if storageSlotSpecs[9].ID != "m2-4" || storageSlotSpecs[9].BusPath != "/dev/disk/by-path/pci-0000:07:00.0-nvme-" {
		t.Fatalf("unexpected M.2 slot 4 mapping: %+v", storageSlotSpecs[9])
	}
}

func TestParseLSBLKFindsRAIDAndSystemUsage(t *testing.T) {
	data := []byte(`{
  "blockdevices": [
    {"name":"sda","kname":"sda","type":"disk","size":1000000,"model":"Disk A","serial":"A1","fstype":null,"mountpoints":[null],"children":[
      {"name":"sda1","kname":"sda1","type":"part","size":900000,"model":null,"serial":null,"fstype":"linux_raid_member","mountpoints":[null],"children":[
        {"name":"md0","kname":"md0","type":"raid5","size":800000,"model":null,"serial":null,"fstype":"ext4","mountpoints":["/vol1"]}
      ]}
    ]},
    {"name":"nvme1n1","kname":"nvme1n1","type":"disk","size":2000000,"model":"NVMe","serial":"N1","fstype":null,"mountpoints":[null],"children":[
      {"name":"nvme1n1p2","kname":"nvme1n1p2","type":"part","size":1900000,"model":null,"serial":null,"fstype":"ext4","mountpoints":["/"]}
    ]}
  ]
}`)
	blocks, err := parseLSBLK(data)
	if err != nil {
		t.Fatal(err)
	}
	front := blocks["sda"]
	if !front.Used || front.SizeBytes != 1000000 || !containsString(front.Purposes, "md0") {
		t.Fatalf("unexpected RAID disk info: %+v", front)
	}
	system := blocks["nvme1n1"]
	if !system.Used || !containsString(system.Purposes, "系统盘") {
		t.Fatalf("unexpected system disk info: %+v", system)
	}
}

func TestParseSMARTReportsNVMeWarningData(t *testing.T) {
	result, err := parseSMART([]byte(`{
  "smart_status":{"passed":false},
  "temperature":{"current":45},
  "nvme_smart_health_information_log":{"critical_warning":4,"temperature":45,"percentage_used":104}
}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Passed == nil || *result.Passed || result.Critical != 4 || result.PercentageUsed != 104 || result.TemperatureC != 45 {
		t.Fatalf("unexpected SMART result: %+v", result)
	}
}

func TestParseSMARTReportsStandbyPowerMode(t *testing.T) {
	result, err := parseSMART([]byte(`{"power_mode":"STANDBY"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !powerModeIsSleeping(result.PowerMode) {
		t.Fatalf("expected standby power mode, got %+v", result)
	}
}

func TestParseSMARTDetectsStandbyFromMessages(t *testing.T) {
	data := []byte(`{
  "json_format_version": [1, 0],
  "smartctl": {
    "version": [7, 3],
    "argv": ["smartctl", "-j", "-n", "standby", "-H", "-A", "/dev/sdb"],
    "messages": [
      {
        "string": "Device is in STANDBY mode, exit(2)",
        "severity": "information"
      }
    ],
    "exit_status": 2
  },
  "device": {"name": "/dev/sdb", "type": "sat", "protocol": "ATA"}
}`)
	result, err := parseSMART(data)
	if err != nil {
		t.Fatal(err)
	}
	if !powerModeIsSleeping(result.PowerMode) {
		t.Fatalf("expected sleeping power mode derived from messages, got %+v", result)
	}
	if result.Passed != nil || result.TemperatureC != 0 {
		t.Fatalf("expected no health/temperature data for sleeping drive, got %+v", result)
	}
}

func TestParseSMARTIgnoresUnrelatedMessages(t *testing.T) {
	data := []byte(`{
  "smartctl": {
    "messages": [{"string": "/dev/sdz: No such file or directory", "severity": "error"}],
    "exit_status": 2
  }
}`)
	result, err := parseSMART(data)
	if err != nil {
		t.Fatal(err)
	}
	if powerModeIsSleeping(result.PowerMode) {
		t.Fatalf("did not expect sleeping power mode for open failure, got %+v", result)
	}
}

func writeBlockStat(t *testing.T, path, line string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

func resetBlockIOStates() {
	blockIOStates = sync.Map{}
}

func sampleUtil(t *testing.T, manager *Manager, path, kname, line string, elapsed time.Duration) (string, *float64) {
	t.Helper()
	if sample, ok := loadBlockIO(kname); ok {
		sample.SampledAt = sample.SampledAt.Add(-elapsed)
		storeBlockIO(kname, sample)
	}
	writeBlockStat(t, path, line)
	return manager.readBlockActivity(kname, "front")
}

func TestReadBlockActivityUsesUtilizationBands(t *testing.T) {
	resetBlockIOStates()
	root := t.TempDir()
	statPath := filepath.Join(root, "sys", "class", "block", "sda", "stat")
	if err := os.MkdirAll(filepath.Dir(statPath), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root}

	got, util := sampleUtil(t, manager, statPath, "sda", "1 0 8 2 3 0 9 4 0 100 0\n", 0)
	if got != StorageActivityIdle || util != nil {
		t.Fatalf("first sample: got %q util %v, want idle and nil", got, util)
	}

	got, util = sampleUtil(t, manager, statPath, "sda", "1 0 8 2 3 0 9 4 0 100 0\n", time.Second)
	if got != StorageActivityIdle {
		t.Fatalf("0%% util: got %q, want idle", got)
	}
	if util == nil || *util != 0 {
		t.Fatalf("0%% util display: got %v, want 0", util)
	}

	got, util = sampleUtil(t, manager, statPath, "sda", "1 0 8 2 3 0 9 4 0 170 0\n", time.Second)
	if got != StorageActivityWorking {
		t.Fatalf("7%% util: got %q, want working", got)
	}
	if util == nil || *util < 6 || *util > 8 {
		t.Fatalf("7%% util value: got %v", util)
	}

	got, _ = sampleUtil(t, manager, statPath, "sda", "1 0 8 2 3 0 9 4 0 880 0\n", time.Second)
	if got != StorageActivityBusy {
		t.Fatalf(">70%% util: got %q, want busy", got)
	}
}

func TestReadBlockActivityUtilizationMatchesDiskstatsUtil(t *testing.T) {
	resetBlockIOStates()
	root := t.TempDir()
	statPath := filepath.Join(root, "sys", "class", "block", "sda", "stat")
	if err := os.MkdirAll(filepath.Dir(statPath), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root}
	writeBlockStat(t, statPath, "1 0 8 2 3 0 9 4 0 100 0\n")
	if _, util := manager.readBlockActivity("sda", "front"); util != nil {
		t.Fatalf("first sample should not have utilization, got %v", *util)
	}
	sample, ok := loadBlockIO("sda")
	if !ok {
		t.Fatal("missing first sample")
	}
	sample.SampledAt = sample.SampledAt.Add(-time.Second)
	storeBlockIO("sda", sample)
	writeBlockStat(t, statPath, "2 0 8 12 4 0 9 14 0 830 0\n")
	got, util := manager.readBlockActivity("sda", "front")
	if util == nil {
		t.Fatal("expected utilization")
	}
	if *util < 72 || *util > 74 {
		t.Fatalf("utilization = %.1f, want about 73", *util)
	}
	if got != StorageActivityBusy {
		t.Fatalf("73%% util: got %q, want busy", got)
	}
}

func TestReadBlockActivityWorkingWhenUtilIsNonzero(t *testing.T) {
	resetBlockIOStates()
	root := t.TempDir()
	statPath := filepath.Join(root, "sys", "class", "block", "sdb", "stat")
	if err := os.MkdirAll(filepath.Dir(statPath), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root}
	writeBlockStat(t, statPath, "10 0 8 2 3 0 9 4 0 100 0\n")
	manager.readBlockActivity("sdb", "front")
	sample, _ := loadBlockIO("sdb")
	sample.SampledAt = sample.SampledAt.Add(-time.Second)
	storeBlockIO("sdb", sample)
	writeBlockStat(t, statPath, "10 0 8 2 3 0 9 4 0 170 0\n")
	got, util := manager.readBlockActivity("sdb", "front")
	if got != StorageActivityWorking {
		t.Fatalf("util 7%% without new r/w: got %q, want working", got)
	}
	if util == nil || *util < 6 || *util > 8 {
		t.Fatalf("utilization = %v, want about 7", util)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestLastPCIComponentPicksDeepestController(t *testing.T) {
	cases := map[string]string{
		"/sys/devices/pci0000:00/0000:00:13.0/0000:02:00.0/ata1/ata_port/ata1":      "0000:02:00.0",
		"/sys/devices/pci0000:00/0000:00:1c.4/0000:01:00.0/0000:02:00.0/nvme/nvme0": "0000:02:00.0",
		"/sys/devices/platform/soc/ata_port/ata3":                                   "",
	}
	for path, want := range cases {
		if got := lastPCIComponent(path); got != want {
			t.Fatalf("lastPCIComponent(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestBestSATAControllerPrefersMostPorts(t *testing.T) {
	ports := map[int]string{
		1: "0000:02:00.0", 2: "0000:02:00.0", 3: "0000:02:00.0",
		4: "0000:02:00.0", 5: "0000:02:00.0", 6: "0000:02:00.0",
		7: "0000:05:00.0", 8: "0000:03:00.0", 9: "0000:03:00.0",
	}
	controller, numbers := bestSATAController(ports)
	if controller != "0000:02:00.0" {
		t.Fatalf("controller = %q, want 0000:02:00.0", controller)
	}
	want := []int{1, 2, 3, 4, 5, 6}
	if len(numbers) != len(want) {
		t.Fatalf("ports = %v, want %v", numbers, want)
	}
	for index := range want {
		if numbers[index] != want[index] {
			t.Fatalf("ports[%d] = %d, want %d", index, numbers[index], want[index])
		}
	}
}

func TestBestSATAControllerBreaksTiesByLowestAddress(t *testing.T) {
	controller, _ := bestSATAController(map[int]string{1: "0000:04:00.0", 2: "0000:03:00.0"})
	if controller != "0000:03:00.0" {
		t.Fatalf("controller = %q, want lowest address 0000:03:00.0", controller)
	}
}

func TestParentPCIComponentReturnsUpstreamPort(t *testing.T) {
	cases := map[string]string{
		"/sys/devices/pci0000:00/0000:00:1d.0/0000:03:00.0/nvme/nvme0":              "0000:00:1d.0",
		"/sys/devices/pci0000:00/0000:00:1c.4/0000:01:00.0/0000:02:00.0/nvme/nvme0": "0000:01:00.0",
		"/sys/devices/pci0000:00/0000:00:13.0/0000:01:00.0/ata1/ata_port/ata1":      "0000:00:13.0",
		"/sys/devices/platform/soc/nvme/nvme1":                                      "",
	}
	for path, want := range cases {
		if got := parentPCIComponent(path); got != want {
			t.Fatalf("parentPCIComponent(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestBayRootPortsSelectedBySATAControllerRootPort(t *testing.T) {
	if got := bayRootPortsForSATARootPort("0000:00:1c.0"); got["m2-1"] != "0000:00:1d.1" || got["m2-2"] != "0000:00:1d.0" {
		t.Fatalf("SATA on 1c.0 (无网卡) 应选 1/2 交错的走线表, got %v", got)
	}
	if got := bayRootPortsForSATARootPort("0000:00:1c.2"); got["m2-1"] != "0000:00:1d.0" || got["m2-2"] != "0000:00:1d.1" {
		t.Fatalf("SATA on 1c.2 (网卡在位) 应选 addon 官方位序表, got %v", got)
	}
	if got := bayRootPortsForSATARootPort(""); got["m2-1"] != "0000:00:1d.0" {
		t.Fatal("未知状态应回退 addon 官方位序表")
	}
}

func TestAssignNVMeBaysMatchesMeasuredWiringNoNIC(t *testing.T) {
	// 2026-09 实测（Mellanox 不在位，SATA 挂 1c.0）：1 号仓空（根端口
	// 1d.1 不被枚举），2/3/4 号仓分别挂 1d.0/1d.2/1d.3；控制器地址受总线
	// 重编号影响，不得参与仓位判定。
	endpoints := []nvmeEndpoint{
		{RootPort: "0000:00:1d.0", BusPath: "/dev/disk/by-path/pci-0000:03:00.0-nvme-"},
		{RootPort: "0000:00:1d.2", BusPath: "/dev/disk/by-path/pci-0000:04:00.0-nvme-"},
		{RootPort: "0000:00:1d.3", BusPath: "/dev/disk/by-path/pci-0000:05:00.0-nvme-"},
	}
	got := assignNVMeBays(endpoints, bayRootPortsSATAOn1c0)
	want := map[string]string{
		"m2-1": "/dev/disk/by-path/pci-0000:00:1d.1-empty-nvme-",
		"m2-2": endpoints[0].BusPath,
		"m2-3": endpoints[1].BusPath,
		"m2-4": endpoints[2].BusPath,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for id, path := range want {
		if got[id] != path {
			t.Fatalf("%s = %q, want %q", id, got[id], path)
		}
	}
}

func TestAssignNVMeBaysMatchesMeasuredWiringWithNIC(t *testing.T) {
	// 2026-09 实测（Mellanox 在 1c.0，SATA 挤到 1c.2）：1/2/3 号仓分别挂
	// 1d.0/1d.1/1d.2，4 号仓空（1d.3 不被枚举）。
	endpoints := []nvmeEndpoint{
		{RootPort: "0000:00:1d.0", BusPath: "/dev/disk/by-path/pci-0000:04:00.0-nvme-"},
		{RootPort: "0000:00:1d.1", BusPath: "/dev/disk/by-path/pci-0000:05:00.0-nvme-"},
		{RootPort: "0000:00:1d.2", BusPath: "/dev/disk/by-path/pci-0000:06:00.0-nvme-"},
	}
	got := assignNVMeBays(endpoints, bayRootPortsSATAOn1c2)
	want := map[string]string{
		"m2-1": endpoints[0].BusPath,
		"m2-2": endpoints[1].BusPath,
		"m2-3": endpoints[2].BusPath,
		"m2-4": "/dev/disk/by-path/pci-0000:00:1d.3-empty-nvme-",
	}
	for id, path := range want {
		if got[id] != path {
			t.Fatalf("%s = %q, want %q", id, got[id], path)
		}
	}
}

func TestAssignNVMeBaysIgnoresUnknownRootPorts(t *testing.T) {
	endpoints := []nvmeEndpoint{
		{RootPort: "0000:00:1d.0", BusPath: "/dev/disk/by-path/pci-0000:03:00.0-nvme-"},
		{RootPort: "0000:00:01.0", BusPath: "/dev/disk/by-path/pci-0000:08:00.0-nvme-"},
	}
	got := assignNVMeBays(endpoints, bayRootPortsSATAOn1c0)
	if got["m2-2"] != endpoints[0].BusPath {
		t.Fatalf("m2-2 = %q, want anchored endpoint", got["m2-2"])
	}
	for _, id := range []string{"m2-1", "m2-3", "m2-4"} {
		if !strings.HasSuffix(got[id], "-empty-nvme-") {
			t.Fatalf("%s = %q, want empty-bay placeholder", id, got[id])
		}
	}
	for _, path := range got {
		if path == endpoints[1].BusPath {
			t.Fatal("unknown-port endpoint must not be mapped into any M.2 bay")
		}
	}
}

func TestDiscoverSlotBusPathsFallsBackWithoutSysfs(t *testing.T) {
	manager := &Manager{Root: t.TempDir()}
	overrides := manager.discoverSlotBusPaths()
	if len(overrides) != 0 {
		t.Fatalf("expected empty overrides without sysfs tree, got %v", overrides)
	}
}

func TestDiscoverSlotBusPathsMapsPortsAndControllers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("模拟 sysfs 目录名含冒号，Windows 文件系统无法创建")
	}
	root := t.TempDir()
	sataDir := filepath.Join(root, "sys", "class", "ata_port")
	nvmeDir := filepath.Join(root, "sys", "class", "nvme")
	for _, dir := range []string{sataDir, nvmeDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	targets := []struct{ linkDir, linkName, target string }{
		{sataDir, "ata1", "../devices/pci0000:00/0000:00:13.0/0000:01:00.0/ata1"},
		{sataDir, "ata2", "../devices/pci0000:00/0000:00:13.0/0000:01:00.0/ata2"},
		{nvmeDir, "nvme0", "../devices/pci0000:00/0000:00:1d.0/0000:03:00.0/nvme"},
		{nvmeDir, "nvme1", "../devices/pci0000:00/0000:00:1d.3/0000:04:00.0/nvme"},
	}
	for _, item := range targets {
		if err := os.MkdirAll(filepath.Join(item.linkDir, item.target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(item.target, filepath.Join(item.linkDir, item.linkName)); err != nil {
			t.Skipf("cannot create symlinks on this platform: %v", err)
		}
	}
	manager := &Manager{Root: root}
	overrides := manager.discoverSlotBusPaths()
	if got := overrides["front-1"]; got != "/dev/disk/by-path/pci-0000:01:00.0-ata-1" {
		t.Fatalf("front-1 = %q", got)
	}
	if got := overrides["front-2"]; got != "/dev/disk/by-path/pci-0000:01:00.0-ata-2" {
		t.Fatalf("front-2 = %q", got)
	}
	if _, ok := overrides["front-3"]; ok {
		t.Fatal("front-3 should stay unresolved with only two ports registered")
	}
	// 检测不到 SATA 控制器根端口时回退官方位序表：m2-1↔1d.0、m2-4↔1d.3
	if got := overrides["m2-1"]; got != "/dev/disk/by-path/pci-0000:03:00.0-nvme-" {
		t.Fatalf("m2-1 = %q", got)
	}
	if got := overrides["m2-4"]; got != "/dev/disk/by-path/pci-0000:04:00.0-nvme-" {
		t.Fatalf("m2-4 = %q", got)
	}
	if got := overrides["m2-2"]; got != "/dev/disk/by-path/pci-0000:00:1d.1-empty-nvme-" {
		t.Fatalf("m2-2 = %q, want empty-bay placeholder", got)
	}
	if got := overrides["m2-3"]; got != "/dev/disk/by-path/pci-0000:00:1d.2-empty-nvme-" {
		t.Fatalf("m2-3 = %q, want empty-bay placeholder", got)
	}
}
