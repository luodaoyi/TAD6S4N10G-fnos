# TAD6S4N —— Debian 原生 tank（Go 单二进制）

作者原版是给**飞牛 fnOS 的浏览器插件（.fpk + Web UI）**。本目录把同一套硬件能力搬到**原生 Debian**，但**不改作者一行 Go 源码**——作者后端复用，前端（tank）用 **Go 原生重写**成单静态二进制。

## 为什么用 Go / 单二进制
- 作者明确要求：**禁止 Python 做 TUI，用 Go/Rust 原生，交付单文件可执行**，不依赖本机 Python/pip/解释器。
- 所以 `tank` 用 Go 写（stdlib + 终端 raw/ANSI），`CGO_ENABLED=0` 静态链接，目标机不需要 Go、Python 或编译器；若安装目录没有预置二进制，则需要 `curl` 自动下载。
- 作者后端 `tad-module` 同样预编译成静态单二进制，直接运行。

## 原理
```
作者 Go 后端 tad-module  ──unix socket──→  /api/status
        （复用，不改源码）                  ↓
tank（Go 单二进制）读取 /api/status，渲染终端面板
```
- 后端把 CPU/coretemp、RAPL 功耗、风扇、硬盘槽位、GPIO 全部聚合进 `/api/status`。
- `tank` 只做"读接口 + 画面板"，不碰底层 CPU/风扇/功耗写入。
- 后端必须以 root 运行：官方 `tad-module serve()` 启动时强制检查 root，并负责 `/sys`、`/dev`、SMART、RAPL、hwmon/PWM、GPIO 以及服务停止时的状态恢复。监控配置关闭写入功能，不等于后端可以降权运行。

## 目录结构（debian-tui/）
```
debian-tui/
├── single-binary/           ← 交付说明和安装脚本；二进制由 CI/Release 或 .fpk 提供
│   ├── tank.service        systemd 配置（启动 tad-module）
│   ├── install-tui-lanrenbao.sh   一键安装（本地二进制或 curl 下载）
│   └── README.txt          组成 + 安装 + 使用
├── tank.go                 tank 的 Go 源码
├── lanrenbao/ui/           浏览器版前端静态资源（可选，供 Web UI）
└── tui-readme.md           本说明
```

## 安装（目标 x86_64 Linux）
```bash
cd .../debian-tui/single-binary
sudo ./install-tui-lanrenbao.sh
```
脚本自动：复制 `tad-module` 到 `/usr/local/libexec/tank/`、写 `/etc/tank/config.json`（监控模式 enabled=false，不主动应用功耗/风扇/GPIO）、复制 `tank` 到 `/usr/local/bin/tank`、落地并启动以 root 运行的 `tank.service`。后端必须使用 root，这是官方后端 `serve()` 的启动要求；安装脚本本身也必须由 root 执行。

本地预置 `tank` 和 `tad-module` 时无需下载；缺少二进制时，安装脚本会使用 `curl` 拉取后端 `.fpk`，而 `tank` 需要通过 `TANK_RELEASE_TANK` 指定下载地址。无论哪种方式，目标机都不需要 Go、Python 或编译器；后端仍必须以 root 运行。

## 使用
```bash
tank              # 读取 root 后端提供的实时状态；普通用户需属于 www-data 组
sudo tank         # root 管理员可直接运行
tank --once       # 打印一次面板（同输入2）
systemctl status tank
journalctl -u tank.service -f
```

## tank 面板内容
- 数据头：CPU / Fan:PWM / Core-Package 温度
- 前置 3.5" 硬盘仓（6 格，█有盘 □无盘，定宽温度）
- 内置 M.2 NVMe（2×2，同格式）
- 明细表（槽位/设备/状态/温度/容量型号）
- 硬盘温度、SMART 健康与休眠状态：由作者后端统一调用 smartctl 并缓存，tank 只读取 `/api/status`，不直接访问硬盘。

## 注意
- **风扇**：作者 README 要求第三方 `fnos-it87-kmod`（内核自带 it87 不识别本板）。当前交付**不含**风扇控制；未装驱动前 tank 显示 `Fan: N/A` 属正常。
- **监控模式**：后端 `enabled=false` 启动，默认不主动修改功耗/风扇/GPIO；后端仍必须以 root 常驻，因为官方 `serve()` 无条件要求 root。
- **架构**：目前是 x86_64（amd64）静态二进制；arm/arm64 需另编对应架构。
