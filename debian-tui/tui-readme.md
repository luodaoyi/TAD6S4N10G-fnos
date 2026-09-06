# TAD6S4N —— Debian 原生 tank（Go 单二进制）

作者原版是给**飞牛 fnOS 的浏览器插件（.fpk + Web UI）**。本目录把同一套硬件能力搬到**原生 Debian**，但**不改作者一行 Go 源码**——作者后端复用，前端（tank）用 **Go 原生重写**成单静态二进制。

## 为什么用 Go / 单二进制
- 作者明确要求：**禁止 Python 做 TUI，用 Go/Rust 原生，交付单文件可执行**，不依赖本机 Python/pip/解释器。
- 所以 `tank` 用 Go 写（stdlib + 终端 raw/ANSI），`CGO_ENABLED=0` 静态链接，**目标机免 Go、免 Python、免 curl、免编译**。
- 作者后端 `tad-module` 同样预编译成静态单二进制，直接运行。

## 原理
```
作者 Go 后端 tad-module  ──unix socket──→  /api/status
        （复用，不改源码）                  ↓
tank（Go 单二进制）读取 /api/status，渲染终端面板
```
- 后端把 CPU/coretemp、RAPL 功耗、风扇、硬盘槽位、GPIO 全部聚合进 `/api/status`。
- tank 只做"读接口 + 画面板"，不碰底层 CPU/风扇/功耗写入。

## 目录结构（debian-tui/）
```
debian-tui/
├── go编译单文件/           ← 交付给用户的 4 个文件（免编译）+ README.txt
│   ├── tank                Go 静态单二进制（TUI，V260905-2）
│   ├── tad-module          Go 静态单二进制（作者后端）
│   ├── tank.service        systemd 配置（启动 tad-module）
│   ├── install-tui-lanrenbao.sh   一键安装（拷贝+写配置+起服务）
│   └── README.txt          组成 + 安装 + 使用
├── tank.go                 tank 的 Go 源码（V260905-2）
├── lanrenbao/ui/           浏览器版前端静态资源（可选，供 Web UI）
├── tankfan                 Python 版风扇工具（本次不做；它87驱动另需第三方模块）
└── tui-readme.md           本说明
```

## 安装（目标 x86_64 Linux）
```bash
cd .../debian-tui/go编译单文件
sudo ./install-tui-lanrenbao.sh
```
脚本自动：复制 `tad-module` 到 `/usr/local/libexec/tank/`、写 `/etc/tank/config.json`（监控模式 enabled=false，不改功耗/风扇/GPIO）、复制 `tank` 到 `/usr/local/bin/tank`、落地并启动 `tank.service`。

无需 Go/Python/curl/编译器，root 即可。

## 使用
```bash
tank              # 欢迎屏：回车=实时刷新(3秒)；输入2=打印一次快照(适合手机小窗)
tank --once       # 打印一次面板（同输入2）
systemctl status tank
journalctl -u tank.service -f
```

## tank 面板内容
- 数据头：CPU / Fan:PWM / Core-Package 温度
- 前置 3.5" 硬盘仓（6 格，█有盘 □无盘，定宽温度）
- 内置 M.2 NVMe（2×2，同格式）
- 明细表（槽位/设备/状态/温度/容量型号）
- 硬盘温度：tank 直接调 `smartctl`（不带 `-n standby`）读取，避开作者 smartctl 解析在 7.5 版的 bug；目标机建议装 `smartmontools`。

## 注意
- **风扇**：作者 README 要求第三方 `fnos-it87-kmod`（内核自带 it87 不识别本板）。当前交付**不含**风扇控制；未装驱动前 tank 显示 `Fan: N/A` 属正常。
- **监控模式**：后端 `enabled=false` 启动，只读展示，不修改功耗/风扇/GPIO。
- **架构**：目前是 x86_64（amd64）静态二进制；arm/arm64 需另编对应架构。
