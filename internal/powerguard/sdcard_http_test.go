package powerguard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 手动刷新入口的方法与权限约束。用假的 Root（空目录）跑，保证测试不会去碰
// 真实硬件：没有 USB 设备树时会明确失败，而不是假装成功。
func TestHandleRefreshSDCardAuth(t *testing.T) {
	server := &Server{Manager: &Manager{Root: t.TempDir()}}

	request := httptest.NewRequest(http.MethodGet, "/api/gpio/refresh-sdcard", nil)
	request.Header.Set("X-Trim-Isadmin", "true")
	recorder := httptest.NewRecorder()
	server.handleRefreshSDCard(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应返回 405，实际 %d", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/gpio/refresh-sdcard", nil)
	recorder = httptest.NewRecorder()
	server.handleRefreshSDCard(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("非管理员应返回 403，实际 %d", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/gpio/refresh-sdcard", nil)
	request.Header.Set("X-Trim-Isadmin", "true")
	recorder = httptest.NewRecorder()
	server.handleRefreshSDCard(recorder, request)
	body := map[string]any{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（正文 %s）", err, recorder.Body.String())
	}
	if recorder.Code != http.StatusConflict {
		t.Fatalf("空 Root 下应当失败并返回 409，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if message, ok := body["error"].(string); !ok || message == "" {
		t.Fatalf("失败响应应带可读的 error 字段: %s", recorder.Body.String())
	}
}
