package powerguard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 存储插入提示音的开关走真实的 /api/config/gpio 与 /api/status 通道：
// 它躺在 GPIO 配置里，但功能独立于「启用按键映射」。
func TestHandleGPIOConfigRoundTripsStorageNotify(t *testing.T) {
	manager, server := storageNotifyHTTPServer(t)

	config := storageNotifyLoadedConfig(t, manager)
	config.GPIO.StorageNotify = gpioBoolPointer(false)
	payload, err := json.Marshal(config.GPIO)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/config/gpio", strings.NewReader(string(payload)))
	request.Header.Set("X-Trim-Isadmin", "true")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.handleGPIOConfig(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("保存关闭状态失败：%d %s", recorder.Code, recorder.Body.String())
	}
	if status := manager.StorageInsertNotifyStatus(manager.storageNotifyEnabled()); status.Enabled {
		t.Fatalf("保存后应当报告已关闭: %+v", status)
	}

	// 再存回开启，状态与落盘内容都要跟着变。
	config.GPIO.StorageNotify = gpioBoolPointer(true)
	payload, _ = json.Marshal(config.GPIO)
	request = httptest.NewRequest(http.MethodPost, "/api/config/gpio", strings.NewReader(string(payload)))
	request.Header.Set("X-Trim-Isadmin", "true")
	request.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	server.handleGPIOConfig(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("保存开启状态失败：%d %s", recorder.Code, recorder.Body.String())
	}
	if status := manager.StorageInsertNotifyStatus(manager.storageNotifyEnabled()); !status.Enabled {
		t.Fatalf("保存后应当报告已开启: %+v", status)
	}

	// status 接口要把 storage_notify 段下发出去，界面才有东西可显示。
	request = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	request.Header.Set("X-Trim-Isadmin", "true")
	recorder = httptest.NewRecorder()
	server.handleStatus(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态接口失败：%d %s", recorder.Code, recorder.Body.String())
	}
	var status struct {
		StorageNotify StorageNotifyStatus `json:"storage_notify"`
		Config        Config              `json:"config"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatalf("状态响应不是合法 JSON: %v", err)
	}
	if !status.StorageNotify.Enabled {
		t.Fatalf("status 应下发 storage_notify.enabled=true: %s", recorder.Body.String())
	}
	if status.Config.GPIO.StorageNotify == nil || !*status.Config.GPIO.StorageNotify {
		t.Fatal("status 应下发 gpio.storage_notify=true，界面据此回填开关")
	}
}

// 字段名写错要报错（配置接口拒绝未知字段），别让前端静默存了个空开关。
func TestHandleGPIOConfigRejectsUnknownStorageNotifyField(t *testing.T) {
	_, server := storageNotifyHTTPServer(t)
	request := httptest.NewRequest(http.MethodPost, "/api/config/gpio", strings.NewReader(`{"version":1,"enabled":true,"storage_notify_enabled":true}`))
	request.Header.Set("X-Trim-Isadmin", "true")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.handleGPIOConfig(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("未知字段应返回 400，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
}

// storageNotifyHTTPServer 建一个可用的 Manager + Server：状态接口要先认出 CPU
// 型号才会往下走，所以假根里要补 /proc/cpuinfo。
func storageNotifyHTTPServer(t *testing.T) (*Manager, *Server) {
	t.Helper()
	root := t.TempDir()
	writeStorageNotifyFile(t, filepath.Join(root, "proc", "cpuinfo"),
		"processor\t: 0\nmodel name\t: Intel(R) N150\n")
	manager := &Manager{
		Root:       root,
		ConfigPath: filepath.Join(root, "config.json"),
		StatePath:  filepath.Join(root, "state.json"),
	}
	if err := manager.saveConfigLocked(DefaultConfig(Profile{})); err != nil {
		t.Fatalf("准备配置失败: %v", err)
	}
	return manager, &Server{Manager: manager}
}

func writeStorageNotifyFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
}

func storageNotifyLoadedConfig(t *testing.T, manager *Manager) Config {
	t.Helper()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	config, err := manager.loadConfigLocked()
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	return config
}
