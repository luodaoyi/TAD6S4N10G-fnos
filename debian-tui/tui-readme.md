# TAD6S4N —— Debian 原生 TUI（tank）

作者原版是给**飞牛 fnOS 的浏览器插件（`.fpk` + Web UI）**。本目录把同一套硬件能力搬到**原生 Debian**，但**不改作者一行 Go 源码**——只是在它的 Go 后台服务外面套一个**终端面板壳子**。

```
浏览器版 (fnOS)                    Debian 版 (本目录)
─────────────────                 ─────────────────────
app/ui/static  → 浏览器渲染         debian-tui/tank → curses 终端渲染
        ↑                                ↑
        │ /api/status (unix socket)       │ curl --unix-socket /api/status
作者 Go 服务（完全复用，不修改）  ← 作者 Go 服务（完全复用，不修改）
```

## 文件说明

| 文件 | 作用 |
|---|---|
| `tank` | 终端面板前端（Python + curses）。只读 `/api/status` 并画面板：窗口检测、仓格、明细表、温度/功耗，`q`/`r`/`F1`/`F2` 按键。`tank --once` 输出纯文本快照。 |
| `tank.service` | systemd 单元，把作者的 Go 二进制跑起来，提供 `/api/status` 这个 unix-socket 接口。 |
| `install.sh` | 一键构建 + 安装：编译作者 Go 后端、写监控模式配置、装 `tank`、落地并启动 systemd 服务。 |
| `tui-readme.md` | 本说明。 |

> 作者 Go 源码、`internal/powerguard`、`cmd/*`、`app/*`、`manifest` 均未改动；本目录只是**新增**。

## 原理（为什么不用改底层）

作者的 Go 服务把全部硬件识别聚合进一个接口：

```
GET /api/status    (server.go)
├─ cpu_temperature   coretemp 取 Core N 最大值（作者逻辑）
├─ packages          Intel RAPL 功耗限制（作者逻辑）
├─ fan_control       it87 风扇 PWM/RPM/模式（作者逻辑）
├─ storage.slots     front-1..6 / m2-1..4 槽位、温度、SMART、休眠、繁忙度
├─ gpio              按键 /dev/port（作者逻辑）
└─ config            当前配置
```

所以 TUI **不需要**做 PCI 拓扑（`00:1d.x`）、ASM1166、`lsblk`、`smartctl`、it87 枚举、RAPL 读取——这些作者都做好了。TUI 只把 JSON 画成方框。写操作（改风扇）也走作者接口：`POST /api/config/fan`（需 `X-Trim-Isadmin: true` 头，`tank` 已带）。

## 安装

```bash
cd TAD6S4N10G-fnos/debian-tui
sudo ./install.sh
```

依赖：`go`（构建后端）、`curl`、`python3`（Debian 自带）。

`install.sh` 做的事：
1. `go build ./backend/powerguard`（依赖 `go.mod` 无外部包，可离线）。
2. 复制 `app/ui/static` 为浏览器版静态资源（可选，TUI 不需要）。
3. 写 `/etc/tank/config.json`（**监控模式**：`enabled=false`，不改功耗/风扇/GPIO）。
4. 安装 `tank` 到 `/usr/local/bin/tank`。
5. 落地 `tank.service` 并 `systemctl enable --now`。

## 使用

```bash
tank               # 交互面板（固定尺寸、居中；普通窗口可直接显示）
tank --once        # 输出一次纯文本快照（无需大窗口）
systemctl status tank
journalctl -u tank.service -f
```

按键：`q` 退出，`r` 立即刷新，`F1` 风扇自动曲线，`F2` 风扇关闭/BIOS 手动。

## 排版与对齐

- **固定尺寸面板**：面板宽度恒定，不随终端变大而拉伸；窗口检测仅用于判断能否完整显示。
- **显示宽度对齐**：内部按「终端列宽」对齐（中文/宽字符按 2 列），所以中英混排、卡格、表格均不错位。
- **仓位格子**：只显示「槽位号 + 占位符 + 定宽温度」，状态词放在下方明细表。
  - `█` = 有机（使用/未用/休眠/告警）；`□` = 空仓；两符号等宽。
  - 空仓温度统一 `00.0°C`（定宽），占位符与温度恒对齐。
- **明细表**：保留 `使用 / 未用 / 空置`（作者逻辑：未用=有盘未挂载，使用=在用/挂载/RAID 成员，空置=无盘）。
- **状态警告**：面板右下角提示异常；`上/下方向键` 未来可做滚动（当前固定显示）。

## 数据说明

- **SATA 温度**：作者用 `smartctl -n standby` 读普通硬盘，**休眠盘读不到** → `--`。TUI 额外读内核 `drivetemp` hwmon 补上（实测 44/45/48°C，连休眠盘也有）。这部分只读、不改作者源码。
- **M.2 温度**：作者 SMART 直接读到，正常显示。
- **功耗**：RAPL `PL1/PL2`，来自作者 `packages[]`。
- **风扇**：见下文。

## 风扇（重要）

作者 README 明确要求风扇需安装**外部驱动** `fnos-it87-kmod`：

> https://github.com/IamAyang233/fnos-it87-kmod（作者 README 链接）

- 它是**第三方**项目（针对飞牛 fnOS 内核的 DKMS 现场编译模块，不是作者写的，也不在作者仓库里）。
- 本仓库**不含**任何风扇驱动源码或 `.ko`。
- 在这台 `i3-N305`/Debian 上实测：内核自带 `it87.ko` 与 `nct6775.ko` 均 `modprobe` 报 **`No such device`** → 识别不到这块白牌主板的 ITE 风扇芯片。
- 因此未装该驱动前，TUI 正常显示 `Fan: N/A（it87 未加载）`，这是**预期行为**，不是 bug。
- 若要让风扇可控：需要为**当前 Debian/PVE 内核**编译 `fnos-it87-kmod`（其源码在第三方仓库，GPL-2.0），要求 gcc/dkms/make + 内核头文件。这是**驱动层**工作，与 TUI 无关。

## 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `TANK_SOCKET` | `/run/tank/tad-module.sock` | Go 服务的 unix socket |
| `TANK_MIN_W` | `85` | 开启检测时的最小终端宽度 |
| `TANK_MIN_H` | `24` | 开启检测时的最小终端高度 |
| `TANK_REFRESH` | `2` | 自动刷新秒数 |
| `TANK_CHECK_SIZE` | （关） | 设为 `1` 恢复「窗口太小」警告框。默认关闭，避免普通窗口被拦 |

## 关键入口（`/api/status` 主要字段）

- `cpu_model` / `profile.display`
- `cpu_temperature.{core_max_c, package_max_c}`
- `packages[].{pl1_w, pl2_w}`
- `fan_control.{driver_detected, available, active, fans[].{rpm,pwm_percent,mode}}`
- `storage.slots[].{id, kind, slot, state, activity, device, model, size_bytes, temperature_c}`
- `gpio.buttons[]`
- `config` / `last_error`

## 与 PVE/Debian 的关系

已在这台 `i3-N305`/Debian 13 机上验证：`coretemp`、`intel-rapl`、ASM1166 前置仓、`00:1d.0` 下的 NVMe、`/dev/port`、`lsblk`、`smartctl`、`drivetemp` 均可用；`it87` 需安装第三方模块后风扇才会显示。
