TAD6S4N —— Debian 原生 tank 面板
====================================

用于 TAD6S4N10G 主板，运行在 普通 Debian（x86_64）上。前端 tank 用 Go 单二进制，
后端复用作者原版（不改源码）。

【文件】 本目录为交付/说明；二进制不在 git 树里（由 CI/Release 提供）
  tank.service              systemd 配置：以 root 启动官方后端（硬性要求）
  install-tui-lanrenbao.sh  安装脚本：获取二进制 + 写监控配置 + 起服务
  README.txt                本说明

【二进制从哪来】
  tad-module  作者官方后端：从官方 Release/.fpk 提取（脚本自动下载
              https://github.com/luodaoyi/TAD6S4N10G-fnos/releases/download/v0.10.19/tad-module.fpk）
  tank        本仓库 CI 编译（.github/workflows/tank-build.yml），首次 Release 挂出前，
              默认 TANK_RELEASE_TANK 为空——请先手动从 CI artifact 或本地编译得到 tank，
              放到本目录 ./tank；或设置 TANK_RELEASE_TANK=<tank 二进制 URL>。
  二者均可用环境变量 TANK_RELEASE_TANK / TANK_RELEASE_BACKEND 指定下载地址，
  或手动从 Release/本地得到后与脚本放在同目录（./tank、./tad-module）。

【安装】
  sudo ./install-tui-lanrenbao.sh
  脚本：取二进制 → 写 /etc/tank/config.json（仅首次，不覆盖）
        → 装 tank → 以 root 起 tank.service
  官方 tad-module 的 serve() 启动时强制要求 root；这是后端硬性要求，
  不是监控模式可以关闭的选项。后端负责 /sys、/dev、SMART、RAPL、
  hwmon/PWM、GPIO 访问及服务停止时的状态恢复。

【使用】
  tank              交互面板（回车=实时刷新3s；输入2=打印一次快照）
  tank --once       打印一次面板
  systemctl status tank
  journalctl -u tank.service -f

【说明】
  - 后端监控模式（enabled=false）默认不主动改功耗/风扇/GPIO；后端必须以 root 常驻。
  - root 是官方 tad-module 的 serve() 启动要求，不是可选配置；即使仅展示
    /api/status，也不能用普通用户启动后端。
  - 硬盘温度、SMART 健康与休眠状态由作者后端统一调用 smartctl 并缓存，
    tank 只读取 /api/status，不直接访问硬盘。
  - 风扇（it87）需第三方 fnos-it87-kmod；未装前显示 N/A。

【版本】  后端 tad-module v0.10.19（官方） | 前端 tank V260913-10
