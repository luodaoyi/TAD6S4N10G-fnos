package powerguard

import (
	"os"
	"path/filepath"
	"testing"
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

func TestReadBlockActivityUsesInflightIO(t *testing.T) {
	root := t.TempDir()
	statPath := filepath.Join(root, "sys", "class", "block", "sda", "stat")
	if err := os.MkdirAll(filepath.Dir(statPath), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root}
	if err := os.WriteFile(statPath, []byte("1 0 8 2 3 0 9 4 1 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := manager.readBlockActivity("sda"); got != StorageActivityBusy {
		t.Fatalf("got %q, want busy", got)
	}
	if err := os.WriteFile(statPath, []byte("1 0 8 2 3 0 9 4 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := manager.readBlockActivity("sda"); got != StorageActivityIdle {
		t.Fatalf("got %q, want idle", got)
	}
	if err := os.WriteFile(statPath, []byte("2 0 8 2 3 0 9 4 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := manager.readBlockActivity("sda"); got != StorageActivityBusy {
		t.Fatalf("got %q after I/O counter changed, want busy", got)
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
