#!/usr/bin/env bash
# tank - one-shot installer (PREBUILT variant).
#
# Ships STATICALLY-LINKED, prebuilt binaries: the backend `./lanrenbao/tad-module`
# and the Go-native TUI `./tank`. The target machine does NOT need Go, python3 or
# curl — no compilation, no interpreter runtime. Requires root only.
#
# Usage:  sudo ./install-tui-lanrenbao.sh
# Env:    TANK_LIB=/usr/local/libexec  TANK_ETC=/etc/tank  TANK_VAR=/var/lib/tank
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HERE="$SCRIPT_DIR"                        # prebuilt artifacts live next to this script
LIB="${TANK_LIB:-/usr/local/libexec/tank}"
ETC="${TANK_ETC:-/etc/tank}"
VAR="${TANK_VAR:-/var/lib/tank}"
RUN=/run/tank
LOG=/var/log/tank
BIN="$LIB/tad-module"
UI="$LIB/ui"

log() { printf '\033[1;34m[+] %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m[!] %s\033[0m\n' "$*"; }
die() { printf '\033[1;31m[!] %s\033[0m\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "请用 root 运行: sudo $0"

[ -f "$HERE/tad-module" ] || die "缺少预编译后端: $HERE/tad-module（请先在别的机器编译或从 Release 获取）"

log "创建目录"
mkdir -p "$LIB" "$ETC" "$VAR" "$RUN" "$LOG"

log "复制预编译后端 + 前端静态资源（无需 Go）"
install -m 0755 "$HERE/tad-module" "$BIN"
if [ -d "$HERE/ui" ]; then
  mkdir -p "$UI"
  cp -a "$HERE/ui/." "$UI/"
else
  mkdir -p "$UI"
  printf 'TAD6S4N - 无浏览器界面，可用 tank 终端面板\n' > "$UI/index.html"
fi

log "写入后端配置（监控模式，不修改功耗/风扇/GPIO）"
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
[ -f "$SCRIPT_DIR/tank" ] && install -m 0755 "$SCRIPT_DIR/tank" /usr/local/bin/tank \
  || warn "未找到 $SCRIPT_DIR/tank，跳过（请从 debian-tui/tank 复制）"

log "安装 systemd 服务"
[ -f "$SCRIPT_DIR/tank.service" ] && install -m 0644 "$SCRIPT_DIR/tank.service" /etc/systemd/system/tank.service \
  || die "未找到 $SCRIPT_DIR/tank.service"
systemctl daemon-reload
systemctl enable --now tank.service

sleep 1
if systemctl is-active --quiet tank.service; then
  log "tank.service 已运行"
  warn "如需风扇控制，请加载第三方 it87 驱动（内核自带 it87 不识别本板）：见 tui-readme.md"
else
  warn "tank.service 未启动，请查看: journalctl -u tank.service -e"
fi

cat <<'DONE'

完成。使用：
  systemctl status tank
  tank                打开 TUI 面板
  tank --once         输出一次文字快照
  journalctl -u tank.service -f
DONE
