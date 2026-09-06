TAD6S4N —— Debian 原生 tank 面板（4 文件免编译版）
====================================================

本目录 4 个文件，拿到任何 x86_64（amd64）Linux 上即可安装使用：
无需 Go、无需 python3、无需 curl、无需联网编译。root 执行即可。

【组成】
  tank                      Go 静态单二进制：终端监控面板（V260905-2）
                            - 回车 = 实时刷新面板（3 秒刷新，需较宽的 SSH 窗口）
                            - 输入 2 = 打印一次面板快照（适合手机小窗口）
                            - 读 /api/status 展示 CPU/核心温度/功耗/风扇/硬盘仓/明细表
  tad-module                作者 Go 后端单二进制（v0.10.15，从官方 .fpk 提取，
                            免自己编译）：提供 /api/status（监听 Unix socket）
  tank.service              systemd 配置：把 tad-module 跑成常驻服务
  install-tui-lanrenbao.sh  一键安装脚本：拷贝 2 个二进制 + 写配置 + 起服务

【安装】
  sudo ./install-tui-lanrenbao.sh

脚本自动：
  1. 复制 tad-module 到 /usr/local/libexec/tank/
  2. 写 /etc/tank/config.json（监控模式：enabled=false，不改功耗/风扇/GPIO）
  3. 复制 tank 到 /usr/local/bin/tank
  4. 落地 tank.service 并 systemctl enable --now 启动

【使用】
  tank                  # 交互监控面板（回车进入；输入 2 打印一次）
  tank --once           # 打印一次面板（等价于面板里输入 2）
  systemctl status tank
  journalctl -u tank.service -f

【说明】
  - 后端是"作者原版，未改动"，直接复用其 /api/status 接口；本目录只新增
    前端 tank（Go 单二进制）与安装脚本，未修改作者任何源码。
  - 后端默认监控模式启动（enabled=false）：只读展示，不改功耗/风扇/GPIO。
  - 硬盘温度由 tank 调 smartctl 直接读取（不带 -n standby，避开作者 smartctl
    解析在 smartctl 7.5 下 power_mode 为对象的 bug）；目标机最好有
    smartmontools 的 smartctl，否则 SATA 温度显示 00.0°C（代表休眠/未读到）。
  - 风扇控制（it87）当前未包含：内核自带 it87 不识别本板，需另装第三方
    fnos-it87-kmod（见仓库 tui-readme.md）。未装前风扇显示 N/A 属正常。

【版本】
  后端 tad-module : v0.10.15（作者官方 Release）
  前端 tank       : V260905-2
