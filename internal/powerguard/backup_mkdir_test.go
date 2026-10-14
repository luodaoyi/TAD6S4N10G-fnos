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

// 「新建文件夹」的回归：按钮只在可选位置（或高级模式）内生效，名字必须干净。
// 这些约束决定了界面上的这个按钮最多能被滥用到什么程度。

func backupMkdirRequest(t *testing.T, payload map[string]any) *http.Request {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/backup/mkdir", bytes.NewReader(body))
	request.Header.Set("X-Trim-Isadmin", "true")
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestCreateBackupDirectoryInsidePlace(t *testing.T) {
	manager := backupPlacesFakeManager(t)

	result, err := manager.CreateBackupDirectory("/vol1/1000", "  备份  ", "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != "/vol1/1000/备份" || result.Name != "备份" {
		t.Fatalf("名称应去掉首尾空格并回传完整路径: %+v", result)
	}
	info, err := os.Stat(filepath.Join(manager.Root, "vol1", "1000", "备份"))
	if err != nil || !info.IsDir() {
		t.Fatalf("目录没有真正创建: %v", err)
	}

	// 位置内部的下级目录、外接存储卷根都可以新建。
	if _, err := manager.CreateBackupDirectory("/vol1/1000/照片", "2026", ""); err != nil {
		t.Fatalf("位置内部下级目录应可新建: %v", err)
	}
	if _, err := manager.CreateBackupDirectory("/media/usb0/DCIM", "100CANON", ""); err != nil {
		t.Fatalf("外接存储内部应可新建: %v", err)
	}
	if _, err := manager.CreateBackupDirectory("/vol02/1000-1-2056118d", "来自备份", ""); err != nil {
		t.Fatalf("远程挂载位置应可新建: %v", err)
	}
}

func TestCreateBackupDirectoryScopeGuard(t *testing.T) {
	manager := backupPlacesFakeManager(t)

	// 不在任何位置里的路径：默认范围一律拒绝（与浏览的退回逻辑不同，这里必须报错，
	// 否则用户会以为建成功了却找不到）。
	for _, path := range []string{"/etc", "/vol1", "/vol02", "/", "/vol1/docker"} {
		if _, err := manager.CreateBackupDirectory(path, "备份", ""); err == nil {
			t.Fatalf("%s 不该允许新建目录", path)
		}
	}

	// 高级模式放开到整个文件系统（卷根是备份目录的常见落点）。
	if _, err := manager.CreateBackupDirectory("/vol1", "备份", BackupScopeAll); err != nil {
		t.Fatalf("高级模式应允许在卷根下新建: %v", err)
	}
	if _, err := manager.CreateBackupDirectory("/vol1/docker", "备份", BackupScopeAll); err != nil {
		t.Fatalf("高级模式下即使名字像系统目录（路径里出现 docker）也应允许: %v", err)
	}
	// 但文件系统根目录与伪文件系统永远拒绝。
	for _, path := range []string{"/", "/proc", "/sys", "/dev"} {
		if _, err := manager.CreateBackupDirectory(path, "备份", BackupScopeAll); err == nil {
			t.Fatalf("%s 即使高级模式也不该允许新建目录", path)
		}
	}

	// 目标不是目录（是个文件）时报错，不做「把文件顶掉」这种事。
	if err := os.WriteFile(filepath.Join(manager.Root, "vol1", "1000", "note.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CreateBackupDirectory("/vol1/1000/note.txt", "备份", ""); err == nil {
		t.Fatal("在文件下面新建目录应报错")
	}
}

func TestCreateBackupDirectoryNameValidation(t *testing.T) {
	manager := backupPlacesFakeManager(t)

	rejected := []struct {
		name string
		why  string
	}{
		{"", "空名字"},
		{"   ", "全是空格"},
		{"a/b", "含斜杠"},
		{`a\b`, "含反斜杠"},
		{".", "单点"},
		{"..", "双点"},
		{"@appdata", "系统隐藏前缀"},
		{"#recycle", "系统回收站"},
		{".@#local", "飞牛内部标记"},
		{"docker", "系统目录名"},
		{"thumb", "缩略图目录"},
		{strings.Repeat("字", 200), "名字过长"},
		{"bad\x00name", "含控制字符"},
	}
	for _, test := range rejected {
		if _, err := manager.CreateBackupDirectory("/vol1/1000", test.name, ""); err == nil {
			t.Fatalf("%q（%s）应被拒绝", test.name, test.why)
		}
	}

	// 正常名字（含用户自己的隐藏目录）应当放行。
	for _, name := range []string{"备份", "backup-2026", "照片 备份", ".git", "我的 备份(1)"} {
		if _, err := manager.CreateBackupDirectory("/vol1/1000", name, ""); err != nil {
			t.Fatalf("%q 应被允许: %v", name, err)
		}
	}
}

func TestCreateBackupDirectoryRejectsExisting(t *testing.T) {
	manager := backupPlacesFakeManager(t)

	if _, err := manager.CreateBackupDirectory("/vol1/1000", "备份", ""); err != nil {
		t.Fatal(err)
	}
	// 同名目录
	if _, err := manager.CreateBackupDirectory("/vol1/1000", "备份", ""); err == nil {
		t.Fatal("同名目录已存在时应报错")
	}
	// 同名文件也要挡住（否则会覆盖用户数据）
	if err := os.WriteFile(filepath.Join(manager.Root, "vol1", "1000", "别动"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CreateBackupDirectory("/vol1/1000", "别动", ""); err == nil {
		t.Fatal("同名文件已存在时应报错")
	}
}

func TestCreateBackupDirectoryRejectsReadOnlyVolume(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sys", "block", "sdc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "media", "usb0"), 0o755); err != nil {
		t.Fatal(err)
	}
	mounts := "/dev/mapper/t-0 /vol1 btrfs rw 0 0\n/dev/sdc1 /media/usb0 ext4 ro 0 0\n"
	if err := os.WriteFile(filepath.Join(root, "proc", "mounts"), []byte(mounts), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sys", "block", "sdc", "removable"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root}

	if _, err := manager.CreateBackupDirectory("/media/usb0", "备份", ""); err == nil {
		t.Fatal("只读卷上不该允许新建目录")
	} else if !strings.Contains(err.Error(), "只读") {
		t.Fatalf("应明确提示只读，实际 %v", err)
	}
}

func TestHandleBackupMkdirCreatesAndGuards(t *testing.T) {
	manager := backupPlacesFakeManager(t)
	server := &Server{Manager: manager}

	recorder := httptest.NewRecorder()
	server.handleBackupMkdir(recorder, backupMkdirRequest(t, map[string]any{
		"path": "/vol1/1000", "name": "备份", "scope": "places",
	}))
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 %d，正文 %s", recorder.Code, recorder.Body.String())
	}
	var result BackupMkdirResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Path != "/vol1/1000/备份" {
		t.Fatalf("返回路径不正确: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(manager.Root, "vol1", "1000", "备份")); err != nil {
		t.Fatalf("目录没有创建: %v", err)
	}

	// 越界路径 → 400，且带可读原因
	recorder = httptest.NewRecorder()
	server.handleBackupMkdir(recorder, backupMkdirRequest(t, map[string]any{
		"path": "/etc", "name": "备份", "scope": "places",
	}))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("越界路径应返回 400，实际 %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "error") {
		t.Fatalf("错误响应应带 error 字段: %s", recorder.Body.String())
	}

	// 未知字段（DisallowUnknownFields）→ 400
	recorder = httptest.NewRecorder()
	server.handleBackupMkdir(recorder, backupMkdirRequest(t, map[string]any{
		"path": "/vol1/1000", "name": "x", "surprise": true,
	}))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("未知字段应返回 400，实际 %d", recorder.Code)
	}

	// 非管理员 → 403
	body, _ := json.Marshal(map[string]any{"path": "/vol1/1000", "name": "x"})
	request := httptest.NewRequest(http.MethodPost, "/api/backup/mkdir", bytes.NewReader(body))
	recorder = httptest.NewRecorder()
	server.handleBackupMkdir(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("非管理员应返回 403，实际 %d", recorder.Code)
	}

	// GET → 405
	recorder = httptest.NewRecorder()
	get := httptest.NewRequest(http.MethodGet, "/api/backup/mkdir", nil)
	get.Header.Set("X-Trim-Isadmin", "true")
	server.handleBackupMkdir(recorder, get)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应返回 405，实际 %d", recorder.Code)
	}
}
