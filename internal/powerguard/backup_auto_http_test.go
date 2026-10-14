package powerguard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// 打标接口的回归：鉴权、动作分发（含兼容旧前端的 marked 字段）、返回结构。
// 直接调 handler，不依赖 unix socket（Windows 上监听不可用）。

type autoMarkerResult struct {
	Action  string             `json:"action"`
	Message string             `json:"message"`
	Devices []AutoBackupDevice `json:"devices"`
}

func autoMarkerCall(t *testing.T, server *Server, payload map[string]any) (*httptest.ResponseRecorder, autoMarkerResult) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/backup/auto/marker", bytes.NewReader(body))
	request.Header.Set("X-Trim-Isadmin", "true")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.handleBackupAutoMarker(recorder, request)
	var result autoMarkerResult
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			t.Fatalf("响应不是 JSON: %v（%s）", err, recorder.Body.String())
		}
	}
	return recorder, result
}

func TestHandleBackupAutoListsDevicesAndMarkerDir(t *testing.T) {
	manager := autoBackupFakeManager(t)
	if _, err := manager.MarkAutoBackupVolume("/media/usb0", "相机卡"); err != nil {
		t.Fatal(err)
	}
	server := &Server{Manager: manager}

	request := httptest.NewRequest(http.MethodGet, "/api/backup/auto", nil)
	request.Header.Set("X-Trim-Isadmin", "true")
	recorder := httptest.NewRecorder()
	server.handleBackupAuto(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 %d，正文 %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Devices   []AutoBackupDevice `json:"devices"`
		MarkerDir string             `json:"marker_dir"`
		Status    BackupStatus       `json:"status"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.MarkerDir != BackupAutoMarkerDirName+"/"+BackupAutoMarkerFileName {
		t.Fatalf("应告诉前端打标文件落在哪: %q", response.MarkerDir)
	}
	if len(response.Devices) != 2 {
		t.Fatalf("在线的存储设备都该列出来: %+v", response.Devices)
	}
	var marked int
	for _, device := range response.Devices {
		if device.Marked {
			marked++
		}
	}
	if marked != 1 {
		t.Fatalf("应只有一块盘已打标: %+v", response.Devices)
	}

	// 非管理员看不到；GET 之外的方法直接 405。
	recorder = httptest.NewRecorder()
	server.handleBackupAuto(recorder, httptest.NewRequest(http.MethodGet, "/api/backup/auto", nil))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("非管理员应 403，实际 %d", recorder.Code)
	}
	post := httptest.NewRequest(http.MethodPost, "/api/backup/auto", nil)
	post.Header.Set("X-Trim-Isadmin", "true")
	recorder = httptest.NewRecorder()
	server.handleBackupAuto(recorder, post)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 应 405，实际 %d", recorder.Code)
	}
}

func TestHandleBackupAutoMarkerDispatchesActions(t *testing.T) {
	manager := autoBackupFakeManager(t)
	server := &Server{Manager: manager}

	// 整卷打标（带别名）。
	recorder, result := autoMarkerCall(t, server, map[string]any{
		"action": "mark", "path": "/media/usb0/DCIM", "alias": "相机卡",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("整卷打标失败: %d %s", recorder.Code, recorder.Body.String())
	}
	if result.Action != "mark" || !strings.Contains(result.Message, "整块卷") {
		t.Fatalf("整卷打标的回执不对: %+v", result)
	}
	var marked AutoBackupDevice
	for _, device := range result.Devices {
		if device.Marked {
			marked = device
		}
	}
	if marked.MountPoint != "/media/usb0" || marked.Alias != "相机卡" {
		t.Fatalf("回执里的设备列表应是打标后的状态: %+v", result.Devices)
	}

	// 子目录打标 + 取消。
	if _, result = autoMarkerCall(t, server, map[string]any{"action": "sub_add", "path": "/media/usb0/DCIM"}); !strings.Contains(result.Message, "DCIM") {
		t.Fatalf("子目录打标的回执不对: %+v", result)
	}
	if _, result = autoMarkerCall(t, server, map[string]any{"action": "sub_remove", "path": "/media/usb0/DCIM"}); !strings.Contains(result.Message, "DCIM") {
		t.Fatalf("取消子目录打标的回执不对: %+v", result)
	}

	// 改别名。
	if _, result = autoMarkerCall(t, server, map[string]any{"action": "rename", "path": "/media/usb0", "alias": "备用盘"}); !strings.Contains(result.Message, "别名") {
		t.Fatalf("改别名的回执不对: %+v", result)
	}
	if got := manager.markedAutoBackupVolumes(); got[0].Alias != "备用盘" {
		t.Fatalf("别名没生效: %+v", got)
	}

	// 取消打标（新版 action）。
	if _, result = autoMarkerCall(t, server, map[string]any{"action": "unmark", "path": "/media/usb0"}); !strings.Contains(result.Message, "取消") {
		t.Fatalf("取消打标的回执不对: %+v", result)
	}
	if got := manager.markedAutoBackupVolumes(); len(got) != 0 {
		t.Fatalf("取消后不该再扫到卷: %+v", got)
	}

	// 兼容更早的前端：只有 marked 字段没有 action。
	if _, err := manager.MarkAutoBackupVolume("/media/usb0", ""); err != nil {
		t.Fatal(err)
	}
	recorder, result = autoMarkerCall(t, server, map[string]any{"marked": false, "path": "/media/usb0"})
	if recorder.Code != http.StatusOK || result.Action != "unmark" {
		t.Fatalf("旧前端的 marked:false 应等价于取消打标: %d %+v", recorder.Code, result)
	}

	// 打标失败要把原因原样带回给界面（这里是「不在可移动存储上」）。
	recorder, _ = autoMarkerCall(t, server, map[string]any{"action": "mark", "path": "/vol1/1000/照片"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("给内置存储打标应 400，实际 %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "可移动") {
		t.Fatalf("错误原因应说明是存储类型问题: %s", recorder.Body.String())
	}

	// 未知动作要当场拒绝。
	recorder, _ = autoMarkerCall(t, server, map[string]any{"action": "teleport"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("未知动作应 400，实际 %d", recorder.Code)
	}
}

func TestHandleBackupAutoMarkerOfflineActions(t *testing.T) {
	manager := autoBackupFakeManager(t)
	if _, err := manager.MarkAutoBackupVolume("/media/usb0", "相机卡"); err != nil {
		t.Fatal(err)
	}
	server := &Server{Manager: manager}

	// 拔盘。
	autoBackupWrite(t, manager, "proc/mounts", "/dev/mapper/trim_x-0 /vol1 btrfs rw 0 0\n")

	recorder, result := autoMarkerCall(t, server, map[string]any{"action": "offline_unmark", "identity": "/dev/sdc1"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("离线取消打标失败: %d %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(result.Message, "下次插上") {
		t.Fatalf("回执应说明是「下次上线自动清除」: %+v", result)
	}
	device, ok := autoBackupDeviceByIdentity(result.Devices, "/dev/sdc1")
	if !ok || !device.PendingClear {
		t.Fatalf("设备列表应下发「待清除」: %+v", result.Devices)
	}

	// 撤销。
	if _, result = autoMarkerCall(t, server, map[string]any{"action": "offline_keep", "identity": "/dev/sdc1"}); !strings.Contains(result.Message, "撤销") {
		t.Fatalf("撤销的回执不对: %+v", result)
	}
	if device, ok := autoBackupDeviceByIdentity(result.Devices, "/dev/sdc1"); !ok || device.PendingClear {
		t.Fatalf("撤销后不该再挂起: %+v", result.Devices)
	}

	// 缺设备标识要报错。
	recorder, _ = autoMarkerCall(t, server, map[string]any{"action": "offline_unmark"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("缺标识应 400，实际 %d", recorder.Code)
	}

	// 鉴权：非管理员 403，GET 405。
	body, _ := json.Marshal(map[string]any{"action": "unmark", "path": "/media/usb0"})
	recorder = httptest.NewRecorder()
	server.handleBackupAutoMarker(recorder, httptest.NewRequest(http.MethodPost, "/api/backup/auto/marker", bytes.NewReader(body)))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("非管理员应 403，实际 %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	server.handleBackupAutoMarker(recorder, httptest.NewRequest(http.MethodGet, "/api/backup/auto/marker", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应 405，实际 %d", recorder.Code)
	}
}

// 保存「按键控制」会整段替换 GPIO 配置：打标记录必须被显式保住，否则点一次保存
// 就会把离线列表清空。
func TestSaveGPIOConfigKeepsAutoVolumes(t *testing.T) {
	manager := autoBackupFakeManager(t)
	if _, err := manager.MarkAutoBackupVolume("/media/usb0", "相机卡"); err != nil {
		t.Fatal(err)
	}

	// 前端提交的按键控制里不含 AutoVolumes（那是打标接口的私产）。
	if err := manager.SaveGPIOConfig(DefaultGPIOConfig()); err != nil {
		t.Fatalf("保存按键控制失败: %v", err)
	}

	records, err := manager.autoBackupRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Alias != "相机卡" {
		t.Fatalf("保存按键控制不该抹掉打标记录: %+v", records)
	}
}

// 设备标识换成 UUID 之后依然要认得同一块盘（identify 的优先级链）。
func TestAutoBackupVolumeIdentityPrefersUUID(t *testing.T) {
	volume := AutoBackupVolume{Device: "/dev/sdc1", Label: "旅行盘", UUID: "1234-ABCD"}
	if volume.identity() != "1234-ABCD" {
		t.Fatalf("应优先用 UUID: %q", volume.identity())
	}
	volume.UUID = ""
	if volume.identity() != "旅行盘" {
		t.Fatalf("没有 UUID 时退回卷标: %q", volume.identity())
	}
	volume.Label = ""
	if volume.identity() != "/dev/sdc1" {
		t.Fatalf("都没有时退回设备名: %q", volume.identity())
	}
	// 打标目录与标记文件的路径拼接。
	if got := autoBackupMarkerFile("/media/usb0"); got != filepath.Join("/media/usb0", BackupAutoMarkerDirName, BackupAutoMarkerFileName) {
		t.Fatalf("标记文件路径不正确: %q", got)
	}
}
