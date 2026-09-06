#!/usr/bin/env bash
# tank - one-shot installer (PREBUILT variant).
#
# Ships the backend `tad-module` (author's official v0.10.15 static binary, from
# the .fpk) and the Go-native TUI `tank`. Binaries are NOT vendored in git; the
# official tad-module is fetched from the author's Release / .fpk and tank is a
# CI build. Place them next to this script (./tank, ./tad-module) or provide
# URLs below to download. Target machine needs only root (plus curl if downloading).
#
# The backend runs read-only, as a dedicated unprivileged user (NOT root).
# Usage:  sudo ./install-tui-lanrenbao.sh
# Env:    TANK_LIB / TANK_ETC / TANK_VAR   TANK_RELEASE_TANK / TANK_RELEASE_BACKEND
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HERE="$SCRIPT_DIR"
LIB="${TANK_LIB:-/usr/local/libexec/tank}"
ETC="${TANK_ETC:-/etc/tank}"
VAR="${TANK_VAR:-/var/lib/tank}"
RUN=/run/tank
LOG=/var/log/tank
BIN="$LIB/tad-module"
UI="$LIB/ui"
SRV_USER=tank

# Optional: download the binaries if not present locally.
#   TANK_RELEASE_TANK    URL to a tank binary (e.g. a CI Release asset)
#   TANK_RELEASE_BACKEND URL to the author's tad-module binary (or .fpk to extract from)
TANK_RELEASE_TANK="${TANK_RELEASE_TANK:-}"
TANK_RELEASE_BACKEND="${TANK_RELEASE_BACKEND:-https://github.com/luodaoyi/TAD6S4N10G-fnos/releases/download/v0.10.15/tad-module.fpk}"

log() { printf '\033[1;34m[+] %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m[!] %s\033[0m\n' "$*"; }
die() { printf '\033[1;31m[!] %s\033[0m\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "请用 root 运行: sudo $0"

# --- obtain backend binary --------------------------------------------------
if [ ! -f "$HERE/tad-module" ]; then
  if command -v curl >/dev/null 2>&1 && [ -n "$TANK_RELEASE_BACKEND" ]; then
    log "下载作者后端 tad-module（官方 Release）"
    tmp="$(mktemp -d)"
    curl -fL --silent --show-error -o "$tmp/backend.fpk" "$TANK_RELEASE_BACKEND"
    tar -xzf "$tmp/backend.fpk" -C "$tmp" app.tgz 2>/dev/null || true
    tar -xzf "$tmp/app.tgz" -C "$tmp" 2>/dev/null || true
    [ -f "$tmp/app/bin/tad-module" ] && cp "$tmp/app/bin/tad-module" "$HERE/tad-module" || die "无法从 .fpk 提取 tad-module"
    chmod 0755 "$HERE/tad-module"
    rm -rf "$tmp"
  else
    die "缺少后端 $HERE/tad-module：请从作者 Release 下载并放到本目录，或设置 TANK_RELEASE_BACKEND"
  fi
fi
[ -f "$HERE/tad-module" ] || die "缺少预编译后端: $HERE/tad-module"

# --- obtain tank binary -----------------------------------------------------
if [ ! -f "$HERE/tank" ]; then
  if command -v curl >/dev/null 2>&1 && [ -n "$TANK_RELEASE_TANK" ]; then
    log "下载 tank（CI 产物）"
    curl -fL --silent --show-error -o "$HERE/tank" "$TANK_RELEASE_TANK"
    chmod 0755 "$HERE/tank"
  else
    die "缺少 $HERE/tank：请设置 TANK_RELEASE_TANK 指向已编译的 tank 二进制"
  fi
fi

# --- dedicated unprivileged user -------------------------------------------
if ! id "$SRV_USER" >/dev/null 2>&1; then
  log "创建专用只读用户 $SRV_USER"
  useradd --system --no-create-home --shell /usr/sbin/nologin "$SRV_USER" 2>/dev/null || true
fi

# Let the invoking (SSH) user read the module socket (owned root:www-data), so
# `tank` runs without sudo.
if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != root ]; then
  log "把启动用户 $SUDO_USER 加入 www-data（读取 /run/tank/tad-module.sock）"
  usermod -a -G www-data "$SUDO_USER" 2>/dev/null || warn "无法把 $SUDO_USER 加入 www-data，请手动 usermod -a -G www-data $SUDO_USER"
fi

log "创建目录"
mkdir -p "$LIB" "$ETC" "$VAR" "$RUN" "$LOG"

log "复制预编译后端 + 前端静态资源"
install -m 0755 "$HERE/tad-module" "$BIN"
if [ -d "$HERE/ui" ]; then
  mkdir -p "$UI"
  cp -a "$HERE/ui/." "$UI/"
else
  mkdir -p "$UI"
  printf 'TAD6S4N - 无浏览器界面，可用 tank 终端面板\n' > "$UI/index.html"
fi

log "写入后端配置（首次；升级/重装不覆盖已有配置）"
if [ ! -f "$ETC/config.json" ]; then
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
  chmod 0640 "$ETC/config.json"
  chown "$SRV_USER:$SRV_USER" "$ETC/config.json"
else
  log "保留已有 $ETC/config.json（不覆盖）"
fi

log "安装 tank TUI 前端"
[ -f "$HERE/tank" ] || die "缺少 $HERE/tank：请设置 TANK_RELEASE_TANK 或手动放入 tank 二进制"
install -m 0755 "$HERE/tank" /usr/local/bin/tank

log "安装 systemd 服务（专用只读用户，非 root）"
[ -f "$HERE/tank.service" ] && install -m 0644 "$HERE/tank.service" /etc/systemd/system/tank.service \
  || die "未找到 tank.service"
systemctl daemon-reload
systemctl enable --now tank.service

sleep 1
if systemctl is-active --quiet tank.service; then
  log "tank.service 已运行（用户: $SRV_USER）"
  warn "如需风扇控制，请加载第三方 it87 驱动（内核自带 it87 不识别本板）"
else
  warn "tank.service 未启动，请查看: journalctl -u tank.service -e"
fi

cat <<'DONE'

完成。使用：
  systemctl status tank
  tank                打开 TUI 面板（如权限不足，把当前用户加入 www-data 组，或 sudo tank）
  tank --once         输出一次文字快照
  journalctl -u tank.service -f

说明：后端以只读用户 tank 常驻（非 root）；监控模式只读，不改功耗/风扇/GPIO。
DONE
