TAD6S4N —— Debian 原生 tank 面板
====================================

用于 TAD6S4N10G 主板，运行在 普通 Debian（x86_64）上。前端 tank 用 Go 单二进制，
后端复用作者原版（不改源码）。

【文件】 本目录为交付/说明；二进制不在 git 树里（由 CI/Release 提供）
  tank.service              systemd 配置：以专用只读用户 tank 启动后端（非 root）
  install-tui-lanrenbao.sh  安装脚本：获取二进制 + 写监控配置 + 起服务
  README.txt                本说明

【二进制从哪来】
  tad-module  作者官方后端：从官方 Release/.fpk 提取（脚本自动下载
              https://github.com/luodaoyi/TAD6S4N10G-fnos/releases/download/v0.10.15/tad-module.fpk）
  tank        本仓库 CI 编译（.github/workflows/tank-build.yml），首次 Release 挂出前，
              默认 TANK_RELEASE_TANK 为空——请先手动从 CI artifact 或本地编译得到 tank，
              放到本目录 ./tank；或设置 TANK_RELEASE_TANK=<tank 二进制 URL>。
  二者均可用环境变量 TANK_RELEASE_TANK / TANK_RELEASE_BACKEND 指定下载地址，
  或手动从 Release/本地得到后与脚本放在同目录（./tank、./tad-module）。

【安装】
  sudo ./install-tui-lanrenbao.sh
  脚本：取二进制 → 建只读用户 tank → 写 /etc/tank/config.json（仅首次，不覆盖）
        → 装 tank → 起 tank.service（User=tank，最小权限）

【使用】
  tank              交互面板（回车=实时刷新3s；输入2=打印一次快照）
  tank --once       打印一次面板
  systemctl status tank
  journalctl -u tank.service -f

【说明】
  - 后端监控模式（enabled=false）只读，不改功耗/风扇/GPIO；后端以非 root 常驻。
  - 硬盘温度由 tank 调 smartctl -n standby 读取（不唤醒待机盘）。
  - 风扇（it87）需第三方 fnos-it87-kmod；未装前显示 N/A。

【版本】  后端 tad-module v0.10.15（官方） | 前端 tank V260906-03
