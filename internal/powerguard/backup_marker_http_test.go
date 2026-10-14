package powerguard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// HTTP 层的回归：标识文件接口的鉴权、入参校验与返回结构。
// 直接调 handler，不依赖 unix socket（Windows 上 localhost 监听不可用）。

func backupMarkerRequest(t *testing.T, job any) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{"job": job})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/backup/marker", bytes.NewReader(body))
	request.Header.Set("X-Trim-Isadmin", "true")
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestHandleBackupMarkerWritesAndReturnsContent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	server := &Server{Manager: &Manager{Root: root}}
	job := GPIOBackupJob{
		ID: "m1", Name: "标识", Mode: BackupModeCopy,
		Source: BackupLocation{Kind: "path", Path: "/src"},
		Target: BackupLocation{Kind: "path", Path: "/dst"},
	}

	recorder := httptest.NewRecorder()
	server.handleBackupMarker(recorder, backupMarkerRequest(t, job))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 %d，正文 %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Marker string `json:"marker"`
		Path   string `json:"path"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(response.Marker, "tad-") {
		t.Fatalf("应自动生成标识内容，实际 %q", response.Marker)
	}
	if response.Path != "/src/"+BackupMetaDirName+"/"+backupMarkerFileName {
		t.Fatalf("返回的写入路径不正确: %q", response.Path)
	}
	data, err := os.ReadFile(filepath.Join(root, "src", BackupMetaDirName, backupMarkerFileName))
	if err != nil {
		t.Fatalf("标识文件没写进源目录: %v", err)
	}
	if strings.TrimSpace(string(data)) != response.Marker {
		t.Fatalf("文件内容 %q 与返回的标识 %q 不一致", strings.TrimSpace(string(data)), response.Marker)
	}
}

func TestHandleBackupMarkerRejectsBadRequests(t *testing.T) {
	server := &Server{Manager: &Manager{Root: t.TempDir()}}

	// 缺 job
	recorder := httptest.NewRecorder()
	server.handleBackupMarker(recorder, backupMarkerRequest(t, nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("缺少 job 应返回 400，实际 %d", recorder.Code)
	}

	// 源不存在
	recorder = httptest.NewRecorder()
	server.handleBackupMarker(recorder, backupMarkerRequest(t, GPIOBackupJob{
		ID: "m2", Name: "x", Mode: BackupModeCopy,
		Source: BackupLocation{Kind: "path", Path: "/not-there"},
		Target: BackupLocation{Kind: "path", Path: "/dst"},
	}))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("源不存在应返回 400，实际 %d", recorder.Code)
	}

	// 非管理员
	body, _ := json.Marshal(map[string]any{"job": GPIOBackupJob{ID: "m3", Name: "x", Mode: BackupModeCopy}})
	request := httptest.NewRequest(http.MethodPost, "/api/backup/marker", bytes.NewReader(body))
	recorder = httptest.NewRecorder()
	server.handleBackupMarker(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("非管理员应返回 403，实际 %d", recorder.Code)
	}

	// GET 不允许
	recorder = httptest.NewRecorder()
	get := httptest.NewRequest(http.MethodGet, "/api/backup/marker", nil)
	get.Header.Set("X-Trim-Isadmin", "true")
	server.handleBackupMarker(recorder, get)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应返回 405，实际 %d", recorder.Code)
	}
}

func TestHandleBackupBrowseCarriesPlacesAndScope(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"proc", "vol1/1000/照片", "vol1/电影"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "mounts"),
		[]byte("/dev/mapper/t-0 /vol1 btrfs rw 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := &Server{Manager: &Manager{Root: root}}

	// 不带 path：位置总览
	request := httptest.NewRequest(http.MethodGet, "/api/backup/browse", nil)
	request.Header.Set("X-Trim-Isadmin", "true")
	recorder := httptest.NewRecorder()
	server.handleBackupBrowse(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 %d，正文 %s", recorder.Code, recorder.Body.String())
	}
	var overview BackupBrowseResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}
	if overview.Mode != "places" || overview.Scope != BackupScopePlaces {
		t.Fatalf("应为默认 places 总览: %+v", overview)
	}
	if len(overview.Places) != 2 {
		t.Fatalf("应识别出 2 个位置，实际 %+v", overview.Places)
	}

	// 进入位置内部
	request = httptest.NewRequest(http.MethodGet, "/api/backup/browse?path=%2Fvol1%2F1000", nil)
	request.Header.Set("X-Trim-Isadmin", "true")
	recorder = httptest.NewRecorder()
	server.handleBackupBrowse(recorder, request)
	var inside BackupBrowseResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &inside); err != nil {
		t.Fatal(err)
	}
	if inside.Mode != "browse" || inside.Inside != "personal:/vol1/1000" || inside.Parent != "" {
		t.Fatalf("位置内部浏览结果不正确: %+v", inside)
	}

	// 高级模式
	request = httptest.NewRequest(http.MethodGet, "/api/backup/browse?path=%2F&scope=all", nil)
	request.Header.Set("X-Trim-Isadmin", "true")
	recorder = httptest.NewRecorder()
	server.handleBackupBrowse(recorder, request)
	var advanced BackupBrowseResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &advanced); err != nil {
		t.Fatal(err)
	}
	if advanced.Scope != BackupScopeAll || advanced.Mode != "browse" {
		t.Fatalf("高级模式结果不正确: %+v", advanced)
	}
}
