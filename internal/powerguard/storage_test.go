package powerguard

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStorageSlotTemplatesKeepTenLogicalSlots(t *testing.T) {
	if len(storageSlotSpecs) != 10 {
		t.Fatalf("got %d slots, want 10", len(storageSlotSpecs))
	}
	if storageSlotSpecs[0].ID != "front-1" || storageSlotSpecs[5].ID != "front-6" {
		t.Fatalf("unexpected front slot templates: %+v", storageSlotSpecs[:6])
	}
	if storageSlotSpecs[6].ID != "m2-1" || storageSlotSpecs[9].ID != "m2-4" {
		t.Fatalf("unexpected M.2 slot templates: %+v", storageSlotSpecs[6:])
	}
}

func tadStorageFixture(asmBDF string, nvmeBDFs []string) []storagePCIDevice {
	devices := []storagePCIDevice{
		{BDF: asmBDF, Vendor: "0x1b21", Device: "0x1166", Class: "0x010601"},
		{BDF: "0000:00:1d.0", Vendor: "0x8086", Class: "0x060400"},
		{BDF: "0000:00:1d.1", Vendor: "0x8086", Class: "0x060400"},
		{BDF: "0000:00:1d.2", Vendor: "0x8086", Class: "0x060400"},
		{BDF: "0000:00:1d.3", Vendor: "0x8086", Class: "0x060400"},
	}
	for index, bdf := range nvmeBDFs {
		devices = append(devices, storagePCIDevice{
			BDF: bdf, Class: "0x010802",
			Topology: "/sys/devices/pci0000:00/0000:00:1d." + string(rune('0'+index)) + "/" + bdf,
		})
	}
	return devices
}

func TestStorageMappingSurvivesLeafBDFRenumbering(t *testing.T) {
	withMellanox := mapStorageSlotSpecs(tadStorageFixture("0000:02:00.0", []string{
		"0000:04:00.0", "0000:05:00.0", "0000:06:00.0", "0000:07:00.0",
	}))
	withoutMellanox := mapStorageSlotSpecs(tadStorageFixture("0000:01:00.0", []string{
		"0000:03:00.0", "0000:04:00.0", "0000:05:00.0", "0000:06:00.0",
	}))

	if len(withMellanox) != 10 || len(withoutMellanox) != 10 {
		t.Fatalf("slot count changed: with=%d without=%d", len(withMellanox), len(withoutMellanox))
	}
	if withMellanox[0].BusPath != "/dev/disk/by-path/pci-0000:02:00.0-ata-1" ||
		withoutMellanox[0].BusPath != "/dev/disk/by-path/pci-0000:01:00.0-ata-1" {
		t.Fatalf("ASM1166 BDF was not discovered dynamically: with=%+v without=%+v", withMellanox[0], withoutMellanox[0])
	}
	for index := 6; index < 10; index++ {
		if withMellanox[index].ID != withoutMellanox[index].ID ||
			!withMellanox[index].MappingKnown || !withoutMellanox[index].MappingKnown {
			t.Fatalf("logical M.2 mapping changed at %d: with=%+v without=%+v", index, withMellanox[index], withoutMellanox[index])
		}
	}
	if withMellanox[6].BusPath != "/dev/disk/by-path/pci-0000:04:00.0-nvme-" ||
		withoutMellanox[6].BusPath != "/dev/disk/by-path/pci-0000:03:00.0-nvme-" {
		t.Fatalf("M.2 slot 1 did not follow root port 00:1d.0: with=%+v without=%+v", withMellanox[6], withoutMellanox[6])
	}
}

func TestStorageMappingSupportsM2SATAAndPrefersNVMe(t *testing.T) {
	devices := tadStorageFixture("0000:02:00.0", []string{"0000:04:00.0", "0000:05:00.0", "0000:06:00.0"})
	devices = append(devices, storagePCIDevice{
		BDF: "0000:00:17.0", Vendor: "0x8086", Device: "0x54d3", Class: "0x010601",
	})
	specs := mapStorageSlotSpecs(devices)

	if specs[8].BusPath != "/dev/disk/by-path/pci-0000:06:00.0-nvme-" || !specs[8].Prefix {
		t.Fatalf("NVMe must take priority for M.2 3: %+v", specs[8])
	}
	if specs[9].BusPath != "/dev/disk/by-path/pci-0000:00:17.0-ata-2" || specs[9].Prefix {
		t.Fatalf("M.2 4 SATA mapping is wrong: %+v", specs[9])
	}
}

func TestStorageMappingKeepsUnknownSATAAndAuthoritativeEmptyM2(t *testing.T) {
	specs := mapStorageSlotSpecs(nil)
	if len(specs) != 10 {
		t.Fatalf("got %d slots, want 10", len(specs))
	}
	for _, spec := range specs[:6] {
		if spec.MappingKnown || spec.Warning == "" {
			t.Fatalf("undetermined SATA slot must carry a warning: %+v", spec)
		}
	}
	for _, spec := range specs[6:] {
		if !spec.MappingKnown || spec.BusPath != "" || spec.Warning != "" {
			t.Fatalf("absent M.2 endpoint must be an empty physical bay: %+v", spec)
		}
	}
}

func TestStorageMappingPreservesSparseM2Slots(t *testing.T) {
	devices := []storagePCIDevice{
		{BDF: "0000:02:00.0", Vendor: "0x1b21", Device: "0x1166", Class: "0x010601"},
		{BDF: "0000:06:00.0", Class: "0x010802", Topology: "/sys/devices/pci0000:00/0000:00:1d.2/0000:06:00.0"},
	}
	specs := mapStorageSlotSpecs(devices)
	if specs[8].BusPath != "/dev/disk/by-path/pci-0000:06:00.0-nvme-" || !specs[8].MappingKnown {
		t.Fatalf("physical M.2 3 was not preserved: %+v", specs[8])
	}
	for _, index := range []int{6, 7, 9} {
		if !specs[index].MappingKnown || specs[index].BusPath != "" {
			t.Fatalf("sparse empty M.2 slot was compacted or unresolved: %+v", specs[index])
		}
	}
}

func TestStorageMappingIgnoresUnknownNVMeRootPort(t *testing.T) {
	devices := []storagePCIDevice{
		{BDF: "0000:02:00.0", Vendor: "0x1b21", Device: "0x1166", Class: "0x010601"},
		{BDF: "0000:08:00.0", Class: "0x010802", Topology: "/sys/devices/pci0000:00/0000:00:01.0/0000:08:00.0"},
	}
	specs := mapStorageSlotSpecs(devices)
	for _, spec := range specs[6:] {
		if !spec.MappingKnown || spec.BusPath != "" {
			t.Fatalf("unknown-root NVMe must not appear in an M.2 bay: %+v", spec)
		}
	}
}

func TestCollectStorageDoesNotReportUnknownTopologyAsEmpty(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dev", "disk", "by-path"), 0o755); err != nil {
		t.Fatal(err)
	}
	status := (&Manager{Root: root}).collectStorage(false)
	if len(status.Slots) != 10 {
		t.Fatalf("got %d slots, want 10", len(status.Slots))
	}
	for _, slot := range status.Slots {
		if slot.State != StorageUnknown || slot.Warning == "" {
			t.Fatalf("unknown topology was reported as a physical state: %+v", slot)
		}
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
	return manager.readBlockActivity(kname)
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
	if _, util := manager.readBlockActivity("sda"); util != nil {
		t.Fatalf("first sample should not have utilization, got %v", *util)
	}
	sample, ok := loadBlockIO("sda")
	if !ok {
		t.Fatal("missing first sample")
	}
	sample.SampledAt = sample.SampledAt.Add(-time.Second)
	storeBlockIO("sda", sample)
	writeBlockStat(t, statPath, "2 0 8 12 4 0 9 14 0 830 0\n")
	got, util := manager.readBlockActivity("sda")
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
	manager.readBlockActivity("sdb")
	sample, _ := loadBlockIO("sdb")
	sample.SampledAt = sample.SampledAt.Add(-time.Second)
	storeBlockIO("sdb", sample)
	writeBlockStat(t, statPath, "10 0 8 2 3 0 9 4 0 170 0\n")
	got, util := manager.readBlockActivity("sdb")
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

func TestActivityFromUtilizationBoundaries(t *testing.T) {
	cases := []struct {
		util float64
		want string
		note string
	}{
		{0.4, StorageActivityIdle, "低于 0.5 经前端取整显示 0%，归空闲"},
		{0.5, StorageActivityWorking, "0.5 恰好进入工作带"},
		{0.51, StorageActivityWorking, "0.51 工作带"},
		{70, StorageActivityWorking, "70 仍属工作带"},
		{70.01, StorageActivityBusy, "刚超过 70 判繁忙"},
		{100, StorageActivityBusy, "100 繁忙"},
	}
	for _, item := range cases {
		if got := activityFromUtilization(item.util); got != item.want {
			t.Fatalf("activityFromUtilization(%.2f) = %q, want %q（%s）", item.util, got, item.want, item.note)
		}
	}
}

func TestReadBlockActivitySubHalfPercentIsIdle(t *testing.T) {
	resetBlockIOStates()
	root := t.TempDir()
	statPath := filepath.Join(root, "sys", "class", "block", "sdd", "stat")
	if err := os.MkdirAll(filepath.Dir(statPath), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root}
	sampleUtil(t, manager, statPath, "sdd", "1 0 8 2 3 0 9 4 0 100 0\n", 0)
	got, util := sampleUtil(t, manager, statPath, "sdd", "1 0 8 2 3 0 9 4 0 104 0\n", time.Second)
	if got != StorageActivityIdle || util == nil || *util != 0 {
		t.Fatalf("0.4%% util: got %q util %v, want idle with util forced to 0", got, util)
	}
}
