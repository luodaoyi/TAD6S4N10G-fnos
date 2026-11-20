#!/bin/sh
# tad-module Docker entrypoint.
#
# 职责：
#   1. 首次启动生成默认配置（功耗/风扇默认关闭，按键映射/蜂鸣/LED 默认开启）。
#   2. 生成 HTTP Basic 认证凭据（后端在非回环监听时强制要求，fail-closed）。
#   3. 以 root 启动后端（上游 serve() 的硬性要求）。
#
# 环境变量：
#   TAD_DATA_DIR     持久化目录（默认 /data）
#   TAD_HTTP_ADDR    监听地址（默认 0.0.0.0:8123）
#   TAD_USERNAME     Basic 认证用户名（默认 admin）
#   TAD_PASSWORD     Basic 认证密码（不设置则首次启动自动生成并打印）
set -eu

DATA_DIR="${TAD_DATA_DIR:-/data}"
HTTP_ADDR="${TAD_HTTP_ADDR:-0.0.0.0:8123}"
CONFIG="${DATA_DIR}/config.json"
STATE="${DATA_DIR}/original-state.json"
AUTH="${DATA_DIR}/web-auth.json"
LOG="${DATA_DIR}/tad-module.log"

mkdir -p "$DATA_DIR"

if [ ! -f "$CONFIG" ]; then
  cp /app/config.default.json "$CONFIG"
  chmod 600 "$CONFIG"
  echo "tad-module: created default config at $CONFIG"
fi

if [ ! -f "$AUTH" ]; then
  USERNAME="${TAD_USERNAME:-admin}"
  PASSWORD="${TAD_PASSWORD:-}"
  if [ -z "$PASSWORD" ]; then
    PASSWORD="$(tr -dc A-Za-z0-9 </dev/urandom | head -c 24)"
    echo "tad-module: generated initial HTTP Basic password: $PASSWORD"
    echo "tad-module: (stored in $AUTH; delete that file and restart to regenerate)"
  fi
  printf '{"username":"%s","password":"%s"}\n' "$USERNAME" "$PASSWORD" > "$AUTH"
  chmod 600 "$AUTH"
fi

exec /app/tad-module serve \
  --config "$CONFIG" \
  --state "$STATE" \
  --web-root /app/ui/static \
  --http "$HTTP_ADDR" \
  --auth-file "$AUTH" \
  --log "$LOG"
