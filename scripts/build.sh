#!/usr/bin/env bash
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="$(sed -n 's/^version=//p' "${PROJECT_ROOT}/manifest" | head -1 | tr -d '"')"
FNPACK="${FNPACK:-fnpack}"

# 打包时把 index.html 的静态资源版本号对齐 manifest，避免浏览器沿用旧缓存的 CSS/JS
INDEX_HTML="${PROJECT_ROOT}/app/ui/static/index.html"
INDEX_HTML_BACKUP="$(mktemp)"
restore_index_html() {
  if [[ -n "${INDEX_HTML_BACKUP:-}" && -f "${INDEX_HTML_BACKUP}" ]]; then
    mv -f "${INDEX_HTML_BACKUP}" "${INDEX_HTML}"
  fi
}
trap restore_index_html EXIT
cp "${INDEX_HTML}" "${INDEX_HTML_BACKUP}"
# 测试包（0.0.1）每次构建附加时间戳：同一版本号反复出包时强制浏览器拉新，
# 正式版保持干净的 ?v=<manifest 版本>
CACHE_VERSION="${VERSION}"
if [[ "${VERSION}" =~ ^0\.0\.[0-9]+$ ]]; then
  CACHE_VERSION="${VERSION}-$(date -u +%Y%m%d%H%M%S)"
fi
sed -i "s/styles\.css?v=[^\"]*/styles.css?v=${CACHE_VERSION}/g; s/app\.js?v=[^\"]*/app.js?v=${CACHE_VERSION}/g" "${INDEX_HTML}"

mkdir -p "${PROJECT_ROOT}/app/bin" "${PROJECT_ROOT}/app/ui/images" "${PROJECT_ROOT}/.cache/go-build" "${PROJECT_ROOT}/.cache/go-tmp"

cd "${PROJECT_ROOT}"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
GOCACHE="${PROJECT_ROOT}/.cache/go-build" GOTMPDIR="${PROJECT_ROOT}/.cache/go-tmp" \
go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o app/bin/tad-module ./backend/powerguard

GOCACHE="${PROJECT_ROOT}/.cache/go-build" GOTMPDIR="${PROJECT_ROOT}/.cache/go-tmp" \
go run ./tools/icon-gen -out app/ui/images
cp app/ui/images/icon_64.png ICON.PNG
cp app/ui/images/icon_256.png ICON_256.PNG

if [[ "${1:-}" != "--skip-package" ]]; then
  PACK_OUTPUT="$("${FNPACK}" build 2>&1)"
  printf '%s\n' "${PACK_OUTPUT}"
  if grep -q "Packing failed" <<<"${PACK_OUTPUT}" || ! compgen -G "${PROJECT_ROOT}/*.fpk" >/dev/null; then
    echo "fnpack build failed" >&2
    exit 1
  fi
fi
