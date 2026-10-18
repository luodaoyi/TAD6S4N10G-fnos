package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"fnos-powerguard/internal/crpsump"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	fs := flag.NewFlagSet("psu-monitor-go", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1", "")
	port := fs.Int("port", 3878, "")
	sockPath := fs.String("socket", "", "")
	webRoot := fs.String("web-root", "ui/static", "")
	pidFile := fs.String("pid", "", "")
	_ = fs.String("log", "", "") // 兼容 Python 参数, 日志固定写 LogPath
	cfgPath := fs.String("config", "", "")
	energyPath := fs.String("energy", "", "") // 耗电统计文件, 默认与 --config 同目录 energy.json
	_ = fs.Parse(args)

	if *pidFile != "" {
		_ = os.WriteFile(*pidFile, []byte(strconv.Itoa(os.Getpid())), 0644)
	}

	// 读取 GPIO 引脚配置 (scl_pin / sda_pin), 缺省用默认
	sclPin, sdaPin := crpsump.DefaultSCL, crpsump.DefaultSDA
	configured := false
	if *cfgPath != "" {
		if data, err := os.ReadFile(*cfgPath); err == nil {
			var cfg struct {
				Scl string `json:"scl_pin"`
				Sda string `json:"sda_pin"`
			}
			if json.Unmarshal(data, &cfg) == nil {
				if cfg.Scl != "" {
					sclPin = cfg.Scl
				}
				if cfg.Sda != "" {
					sdaPin = cfg.Sda
				}
				configured = true // 有配置文件 = 已在界面设置过 GPIO
			}
		} else {
			crpsump.Logf("警告: 读取 GPIO 配置失败 (%s): %v", *cfgPath, err)
		}
	}

	bus := crpsump.NewBus(sclPin, sdaPin, configured)
	srv := &crpsump.AppServer{Bus: bus, WebRoot: *webRoot, ConfigPath: *cfgPath}

	// 耗电统计文件路径: --energy > 与 config 同目录 > 当前目录
	ep := *energyPath
	if ep == "" && *cfgPath != "" {
		ep = filepath.Join(filepath.Dir(*cfgPath), "energy.json")
	}
	if ep == "" {
		ep = "energy.json"
	}
	energy := crpsump.StartEnergy(bus, ep)
	crpsump.StartStatusRefresher(bus) // 状态快照后台刷新: 页面首开即有数据

	// 启动自检: 仅在 GPIO 已配置后才读取电源
	crpsump.LogStartup(bus, sclPin, sdaPin)

	var ln net.Listener
	var err error
	if *sockPath != "" {
		// 飞牛网关模式: 监听 Unix domain socket, 由 fnOS 网关反代。
		// 权限 0750(owner+group 可连): 不开放到 0777, 避免任意本地进程绕过网关直连控制风扇。
		// 独立部署时依赖 fnOS 网关鉴权; 若网关以不同用户/组运行导致连不上, 按需放宽。
		_ = os.Remove(*sockPath)
		if d := filepath.Dir(*sockPath); d != "" {
			_ = os.MkdirAll(d, 0755)
		}
		ln, err = net.Listen("unix", *sockPath)
		if err == nil {
			_ = os.Chmod(*sockPath, 0750)
			crpsump.Logf("listening on unix socket %s (0750)", *sockPath)
		}
	} else {
		ln, err = net.Listen("tcp", fmt.Sprintf("%s:%d", *addr, *port))
	}
	if err != nil {
		crpsump.Logf("监听失败: %v", err)
		os.Exit(1)
	}

	hs := &http.Server{Handler: srv}
	go func() {
		if err := hs.Serve(ln); err != nil && err != http.ErrServerClosed {
			crpsump.Logf("服务异常: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	energy.Save() // 退出前把耗电统计落盘
	_ = hs.Close()
}
