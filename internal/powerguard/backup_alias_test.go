package powerguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func aliasWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 服务以 root 运行后可以读 /proc/<pid>/cmdline，rclone mount 的参数里就带着
// 云盘名——这正是 tank 用户之前查不到「夸克网盘」显示名的解法。
func TestRemoteMountFriendlyNames(t *testing.T) {
	root := t.TempDir()
	aliasWrite(t, filepath.Join(root, "proc", "123", "cmdline"),
		"rclone\x00mount\x00夸克网盘:\x00/vol02/1000-1-abc\x00--uid\x001000\x00")
	aliasWrite(t, filepath.Join(root, "proc", "456", "cmdline"),
		"/bin/bash\x00-c\x00sleep 1\x00")
	aliasWrite(t, filepath.Join(root, "proc", "789", "cmdline"),
		"rclone\x00mount\x00remotename:media\x00/vol02/1000-1-def/\x00")
	aliasWrite(t, filepath.Join(root, "proc", "101", "cmdline"),
		"rclone\x00lsjson\x00remote:x\x00") // 非 mount 子命令，不该被解析

	manager := &Manager{Root: root}
	names := manager.remoteMountFriendlyNames()
	if names["/vol02/1000-1-abc"] != "夸克网盘" {
		t.Fatalf("应从挂载进程推断云盘名: %+v", names)
	}
	if names["/vol02/1000-1-def/"] != "remotename" && names["/vol02/1000-1-def"] != "remotename" {
		t.Fatalf("带路径的 remote 应只取段名: %+v", names)
	}
	if len(names) != 2 {
		t.Fatalf("非挂载进程不该被解析: %+v", names)
	}
}

func TestRemoteMountDisplayNamePriority(t *testing.T) {
	friendly := map[string]string{"/vol02/1000-1-abc": "夸克网盘"}
	aliases := map[string]string{"/vol02/1000-1-abc": "我的网盘"}
	if got := remoteMountDisplayName("/vol02/1000-1-abc", friendly, aliases); got != "我的网盘" {
		t.Fatalf("手动别名应最优先: %q", got)
	}
	if got := remoteMountDisplayName("/vol02/1000-1-abc", friendly, nil); got != "夸克网盘" {
		t.Fatalf("推断名其次: %q", got)
	}
	if got := remoteMountDisplayName("/vol02/1000-1-abc", map[string]string{"/vol02/1000-1-abc/": "夸克"}, nil); got != "夸克" {
		t.Fatalf("挂载点带尾斜杠也应能匹配: %q", got)
	}
	if got := remoteMountDisplayName("/vol02/1000-1-xyz", nil, nil); got != "1000-1-xyz" {
		t.Fatalf("什么都拿不到时应退回目录名: %q", got)
	}
}

func TestSetRemoteAlias(t *testing.T) {
	root := t.TempDir()
	aliasWrite(t, filepath.Join(root, "proc", "mounts"),
		"夸克:remote /vol02/1000-1-abc fuse.rclone rw 0 0\n")
	manager := &Manager{Root: root, ConfigPath: filepath.Join(root, "config.json")}
	// 真实部署里配置文件总是存在（服务启动即写）；测试里先落一份默认配置。
	if err := manager.saveConfigLocked(DefaultConfig(Profile{})); err != nil {
		t.Fatalf("准备配置失败: %v", err)
	}

	if err := manager.SetRemoteAlias("/vol02/1000-1-abc", "夸克网盘"); err != nil {
		t.Fatalf("设置别名失败: %v", err)
	}
	if got := manager.RemoteAliases()["/vol02/1000-1-abc"]; got != "夸克网盘" {
		t.Fatalf("别名未保存: %+v", manager.RemoteAliases())
	}

	// 落盘后重新加载也应该还在
	reloaded := &Manager{Root: root, ConfigPath: filepath.Join(root, "config.json")}
	if got := reloaded.RemoteAliases()["/vol02/1000-1-abc"]; got != "夸克网盘" {
		t.Fatalf("别名应已落盘: %+v", reloaded.RemoteAliases())
	}

	if err := manager.SetRemoteAlias("/vol02/1000-1-abc", ""); err != nil {
		t.Fatalf("清除别名失败: %v", err)
	}
	if len(manager.RemoteAliases()) != 0 {
		t.Fatalf("别名应被清除: %+v", manager.RemoteAliases())
	}

	if err := manager.SetRemoteAlias("/not/a/remote", "x"); err == nil {
		t.Fatal("未知挂载点应被拒绝")
	}
	if err := manager.SetRemoteAlias("/vol02/1000-1-abc", strings.Repeat("名", 65)); err == nil {
		t.Fatal("超长显示名应被拒绝")
	}
}
