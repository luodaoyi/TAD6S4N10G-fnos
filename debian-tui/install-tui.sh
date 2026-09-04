#!/usr/bin/env bash
# tank - one-shot installer for the Debian-native TAD6S4N TUI.
#
# Builds the author's Go backend (unchanged), installs it as a systemd
# service providing /api/status on a Unix socket, and installs the `tank`
# terminal front-end. The backend starts in MONITORING-ONLY mode: power/fan
# limits are NOT changed until you enable them from the panel (F1/F2) or the
# web UI.
#
# Usage:  sudo ./install-tui.sh
# Env:    TANK_LIB=/usr/local/libexec  TANK_ETC=/etc/tank  TANK_VAR=/var/lib/tank
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LIB="${TANK_LIB:-/usr/local/libexec/tank}"
ETC="${TANK_ETC:-/etc/tank}"
VAR="${TANK_VAR:-/var/lib/tank}"
RUN=/run/tank
LOG=/var/log/tank
BIN="$LIB/tad-module"
UI="$LIB/ui"
SOCKET="$RUN/tad-module.sock"

log() { printf '\033[1;34m[+] %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m[!] %s\033[0m\n' "$*"; }
die() { printf '\033[1;31m[!] %s\033[0m\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "请用 root 运行: sudo $0"

for cmd in go curl python3; do
  command -v "$cmd" >/dev/null 2>&1 || warn "缺少 '$cmd'，请先安装（apt install golang curl python3）"
done

log "构建作者 Go 后端（不修改源码）"
mkdir -p "$LIB" "$ETC" "$VAR" "$RUN" "$LOG"
(cd "$REPO" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags "-s -w" -o "$BIN" ./backend/powerguard)
chmod 755 "$BIN"

log "复制前端静态资源（可选，供浏览器版使用）"
if [ -d "$REPO/app/ui/static" ]; then
  mkdir -p "$UI"
  cp -a "$REPO/app/ui/static/" "$UI/"
else
  mkdir -p "$UI"
  echo "TAD6S4N - 无浏览器界面，可用 tank 终端面板" > "$UI/index.html"
fi

log "写入后端配置（监控模式，不修改功耗/风扇）"
cat > "$ETC/config.json" <<'JSON'
{
  "enabled": false,
  "pl1_w": 15,
  "pl2_w": 15,
  "reapply_seconds": 30,
  "fan": {
    "enabled": false,
    "cpu_fan_ids": [],
    "hdd_fan_ids": [],
    "nvme_fan_ids": [],
    "min_pwm_percent": 60,
    "emergency_temp_c": 85,
    "poll_seconds": 2,
    "curve": [{"temp_c":40,"pwm_percent":60},{"temp_c":55,"pwm_percent":70},{"temp_c":70,"pwm_percent":85},{"temp_c":80,"pwm_percent":100}],
    "hdd_curve": [{"temp_c":25,"pwm_percent":60},{"temp_c":35,"pwm_percent":85},{"temp_c":50,"pwm_percent":100}],
    "nvme_curve": [{"temp_c":25,"pwm_percent":60},{"temp_c":35,"pwm_percent":85},{"temp_c":50,"pwm_percent":100}],
    "hdd_slot_ids": ["front-1","front-2","front-3","front-4","front-5","front-6"],
    "nvme_slot_ids": ["m2-1","m2-2","m2-3","m2-4"]
  },
  "gpio": {
    "version": 1,
    "enabled": false,
    "buttons": [
      {"id":"copy","actions":{"short":"none","hold_3s":"none","hold_9s":"none","hold_15s":"none"}},
      {"id":"network","actions":{"short":"none","hold_3s":"none","hold_9s":"none","hold_15s":"none"}},
      {"id":"rear_reset","actions":{"short":"none","hold_3s":"none","hold_9s":"none","hold_15s":"none"}}
    ]
  }
}
JSON
chmod 600 "$ETC/config.json"

log "安装 tank TUI 前端"
install -m 0755 "$REPO/debian-tui/tank" /usr/local/bin/tank

log "安装 systemd 服务"
install -m 0644 "$REPO/debian-tui/tank.service" /etc/systemd/system/tank.service
systemctl daemon-reload
systemctl enable --now tank.service

sleep 1
if systemctl is-active --quiet tank.service; then
  log "tank.service 已运行"
  warn "如需风扇控制，请加载 it87 驱动：modprobe it87（或写入 /etc/modules-load.d/tank.conf）"
else
  warn "tank.service 未启动，请查看: journalctl -u tank.service -e"
fi

cat <<'DONE'

完成。使用方法：
  systemctl status tank          查看后台服务
  tank                            打开 TUI 面板（需要窗口 >= 85x24）
  tank --once                     输出一次文字快照
  journalctl -u tank.service -f   查看后台日志
DONE
