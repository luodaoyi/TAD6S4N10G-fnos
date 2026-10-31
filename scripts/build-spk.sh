#!/usr/bin/env bash
# 打群晖 DSM 7 SPK 包。
#
# 用法:
#   scripts/build-spk.sh                 # 版本取 manifest（正式包）
#   TAD_SPK_VERSION=0.0.1 scripts/build-spk.sh   # 测试包（约定 0.0.1）
#
# 与 fpk 的差异：SPK 由纯 tar 组装（INFO + package.tgz + scripts/ + conf/ +
# 图标），不依赖 fnpack。本机无 Go 时自动回落到 docker golang:1.25 构建
# （与 scripts/build.sh 同一套编译参数）。产物：仓库根的
# tad-module-v<版本>.spk 与 .sha256。
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${TAD_SPK_VERSION:-$(sed -n 's/^version=//p' "${PROJECT_ROOT}/manifest" | head -1 | tr -d '"')}"
STAGE="${PROJECT_ROOT}/.cache/spk-stage"
SPK_NAME="tad-module-v${VERSION}.spk"
# MSYS 下用 //c/... 形式挂载（带单斜杠会被路径改写破坏）
DOCKER_SRC="//${PROJECT_ROOT#/}"

# 测试包（0.0.1）给静态资源加时间戳，强制浏览器拉新（与 build.sh 同规则）
CACHE_VERSION="${VERSION}"
if [[ "${VERSION}" =~ ^0\.0\.[0-9]+$ ]]; then
  CACHE_VERSION="${VERSION}-$(date -u +%Y%m%d%H%M%S)"
fi

mkdir -p "${PROJECT_ROOT}/.cache/go-build" "${PROJECT_ROOT}/.cache/go-tmp" "${STAGE}"
cd "${PROJECT_ROOT}"

# 探测顺序:PATH 中的 go → 本机常见安装路径(/c/Users/<user>/sdk/go*,与
# scripts/build.sh 的本地构建环境一致)→ docker 回落
if ! command -v go >/dev/null 2>&1; then
  for candidate in /c/Users/*/sdk/go*/bin; do
    [ -x "${candidate}/go" ] && PATH="${candidate}:${PATH}" && break
  done
  export PATH
fi

if command -v go >/dev/null 2>&1; then
  export GOCACHE="${PROJECT_ROOT}/.cache/go-build" GOTMPDIR="${PROJECT_ROOT}/.cache/go-tmp"
  GO() { go "$@"; }
else
  echo "==> 本机无 go，使用 docker golang:1.25 构建"
  GO() {
    MSYS_NO_PATHCONV=1 docker run --rm \
      -v "${DOCKER_SRC}:/src" -w /src \
      -e GOCACHE=/src/.cache/go-build -e GOTMPDIR=/src/.cache/go-tmp \
      -e GOMODCACHE=/src/.cache/gomod \
      golang:1.25 go "$@"
  }
fi

echo "==> 编译后端（版本 ${VERSION}）"
mkdir -p "${PROJECT_ROOT}/app/bin"
# 编译目标固定为 DSM x86_64:本地 go 分支若沿用宿主目标(macOS/Windows/ARM)
# 会产出无法在目标机运行的包,且仍能通过执行位检查——末尾必须复核 ELF64、小端与 x86_64 架构
if command -v go >/dev/null 2>&1; then
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  GOCACHE="${PROJECT_ROOT}/.cache/go-build" GOTMPDIR="${PROJECT_ROOT}/.cache/go-tmp" \
  go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o app/bin/tad-module ./backend/powerguard
else
  MSYS_NO_PATHCONV=1 docker run --rm \
    -v "${DOCKER_SRC}:/src" -w /src \
    -e GOOS=linux -e GOARCH=amd64 -e CGO_ENABLED=0 \
    -e GOCACHE=/src/.cache/go-build -e GOTMPDIR=/src/.cache/go-tmp \
    -e GOMODCACHE=/src/.cache/gomod \
    golang:1.25 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o app/bin/tad-module ./backend/powerguard
fi
ELF_IDENT="$(od -An -tx1 -N6 "${PROJECT_ROOT}/app/bin/tad-module" | tr -d ' \n')"
ELF_MACHINE="$(od -An -tx1 -j18 -N2 "${PROJECT_ROOT}/app/bin/tad-module" | tr -d ' \n')"
if [[ "${ELF_IDENT}" != "7f454c460201" || "${ELF_MACHINE}" != "3e00" ]]; then
  echo "产物不是 x86_64 小端 ELF64（编译目标错误?），拒绝出包" >&2
  exit 1
fi

echo "==> 生成图标"
if [[ ! -f "${PROJECT_ROOT}/app/ui/images/icon_256.png" ]]; then
  GO run ./tools/icon-gen -out app/ui/images
fi
# DSM 桌面图标多尺寸（app/config 的 {0} 占位符按尺寸取 tad-module-<n>.png）
# 注意用相对路径：docker 分支挂载点在 /src，宿主绝对路径会写进容器内部丢失
GO run ./tools/icon-scale -in app/ui/images/icon_256.png -out .cache/spk-stage/payload-app-images -base tad-module

echo "==> 组装 package.tgz（套件载荷）"
rm -rf "${STAGE}/payload" "${STAGE}/root"
mkdir -p "${STAGE}/payload/bin" "${STAGE}/root"
cp "${PROJECT_ROOT}/app/bin/tad-module" "${STAGE}/payload/bin/tad-module"
chmod 755 "${STAGE}/payload/bin/tad-module"
# 静态资源打时间戳进包，不改动工作区文件（build.sh 是改完再还原，这里直接改暂存副本）
mkdir -p "${STAGE}/payload/ui"
cp -r "${PROJECT_ROOT}/app/ui/static" "${STAGE}/payload/ui/static"
sed -i "s/styles\.css?v=[^\"]*/styles.css?v=${CACHE_VERSION}/g; s/app\.js?v=[^\"]*/app\.js?v=${CACHE_VERSION}/g" \
  "${STAGE}/payload/ui/static/index.html"
# DSM 桌面应用注册（app/config 框架应用型 + 多尺寸图标；spksrc/SPM 同机制）
mkdir -p "${STAGE}/payload/app/images"
cp "${PROJECT_ROOT}/synology/app/config" "${STAGE}/payload/app/config"
cp "${PROJECT_ROOT}/synology/app/tadmodule.js" "${STAGE}/payload/app/tadmodule.js"
cp "${STAGE}"/payload-app-images/tad-module-*.png "${STAGE}/payload/app/images/"
sed -i 's/\r$//' "${STAGE}/payload/app/config"
# 桌面框架经 /webman/3rdparty/<pkg>/tadmodule.js 与 images/ 加载应用资源,
# 该前缀被我们的反代接管,故同一份资源必须同时在 ui/static(后端可吐出)
cp "${STAGE}/payload/app/tadmodule.js" "${STAGE}/payload/ui/static/tadmodule.js"
mkdir -p "${STAGE}/payload/ui/static/images"
cp "${STAGE}"/payload/app/images/*.png "${STAGE}/payload/ui/static/images/"
sed -i 's/\r$//' "${STAGE}/payload/app/config"
(cd "${STAGE}/payload" && tar czf "${STAGE}/root/package.tgz" bin ui app)
# MSYS/drvfs 上 chmod 可能静默失效（bin/tad-module 曾打出 644 无执行位，
# DSM 解包后 start-stop-status 起不来）——列表里没有 x 位就用 docker 重打
if ! tar tzvf "${STAGE}/root/package.tgz" | grep -E '^-r.x' | grep -q ' bin/tad-module$'; then
  echo "==> 宿主 tar 丢了执行位（MSYS 常见），改用 docker 重打 package.tgz"
  rm -f "${STAGE}/root/package.tgz"
  MSYS_NO_PATHCONV=1 docker run --rm -v "${DOCKER_SRC}:/src" \
    -w /src/.cache/spk-stage/payload golang:1.25 tar czf ../root/package.tgz bin ui app || true
fi
# docker 回退后仍无执行位（Docker 不在线等），用 python tarfile 兜底：
# 权限按规则显式设置——目录与 bin/ 下可执行 0755,其余 0644
if ! tar tzvf "${STAGE}/root/package.tgz" 2>/dev/null | grep -E '^-r.x' | grep -q ' bin/tad-module$'; then
  echo "==> docker 不可用,改用 python tarfile 重打 package.tgz（显式权限）"
  rm -f "${STAGE}/root/package.tgz"
  python - "${STAGE}/payload" "${STAGE}/root/package.tgz" <<'PYEOF'
import os, stat, sys, tarfile
payload, out = sys.argv[1], sys.argv[2]
with tarfile.open(out, "w:gz") as tf:
    for top in ("bin", "ui", "app"):
        base = os.path.join(payload, top)
        for root, dirs, files in os.walk(base):
            for name in sorted(dirs):
                p = os.path.join(root, name)
                ti = tf.gettarinfo(p, arcname=os.path.relpath(p, payload))
                ti.mode = 0o755
                tf.addfile(ti)
            for name in sorted(files):
                p = os.path.join(root, name)
                # Windows python 的 relpath/walk 产生反斜杠,归一后再匹配前缀
                rel = os.path.relpath(p, payload).replace(os.sep, "/")
                ti = tf.gettarinfo(p, arcname=rel)
                ti.mode = 0o755 if rel.startswith("bin/") else 0o644
                ti.uid = ti.gid = 0
                with open(p, "rb") as f:
                    tf.addfile(ti, f)
print("python tarfile 打包完成")
PYEOF
fi
tar tzvf "${STAGE}/root/package.tgz" | grep -E '^-r.x' | grep -q ' bin/tad-module$' || {
  echo "package.tgz 的 bin/tad-module 缺执行位" >&2
  exit 1
}

echo "==> 组装 SPK 根（INFO/scripts/conf/图标）"
mkdir -p "${STAGE}/root"
# INFO 里 version 带引号,替换时必须带上引号（只匹配裸值会静默漏替换,真机装出来就是 @VERSION@）
sed "s/^version=\"@VERSION@\"\$/version=\"${VERSION}\"/" "${PROJECT_ROOT}/synology/INFO" > "${STAGE}/root/INFO"
grep -q "^version=\"${VERSION}\"\$" "${STAGE}/root/INFO" || {
  echo "INFO 版本号替换失败" >&2
  exit 1
}
# DSM 安装时校验 INFO.checksum(= package.tgz 的 md5,spksrc 标准),缺失会被
# 真机以「无法正确安装此套件」拒绝——必须在 package.tgz 定稿后追加
CHECKSUM_MD5="$(md5sum "${STAGE}/root/package.tgz" | cut -d' ' -f1)"
grep -v '^checksum=' "${STAGE}/root/INFO" > "${STAGE}/root/INFO.tmp"
mv "${STAGE}/root/INFO.tmp" "${STAGE}/root/INFO"
echo "checksum=\"${CHECKSUM_MD5}\"" >> "${STAGE}/root/INFO"
cp -r "${PROJECT_ROOT}/synology/scripts" "${STAGE}/root/scripts"
cp -r "${PROJECT_ROOT}/synology/conf" "${STAGE}/root/conf"
cp -r "${PROJECT_ROOT}/synology/WIZARD_UIFILES" "${STAGE}/root/WIZARD_UIFILES"
# 归一 LF：Windows 工作区可能带入 CRLF，DSM 的 sh 解析不了 \r
find "${STAGE}/root/scripts" "${STAGE}/root/conf" "${STAGE}/root/WIZARD_UIFILES" -type f -exec sed -i 's/\r$//' {} +
chmod 755 "${STAGE}/root/scripts"/*
# 安装向导 JSON 必须合法（DSM 按 JSON 渲染向导页，畸形会装不出来/空白）；
# 显式 utf-8：Windows python 默认 cp936 读 ⚠️/中文会直接 UnicodeDecodeError
for wf in "${STAGE}"/root/WIZARD_UIFILES/*uifile; do
  python -c "import json,sys; json.load(open(sys.argv[1], encoding='utf-8'))" "${wf}" || {
    echo "安装向导 JSON 非法: ${wf}" >&2
    exit 1
  }
done
cp "${PROJECT_ROOT}/app/ui/images/icon_64.png" "${STAGE}/root/PACKAGE_ICON.PNG"
cp "${PROJECT_ROOT}/app/ui/images/icon_256.png" "${STAGE}/root/PACKAGE_ICON_256.PNG"

echo "==> 打 SPK"
tar cf "${PROJECT_ROOT}/${SPK_NAME}" -C "${STAGE}/root" \
  INFO package.tgz scripts conf WIZARD_UIFILES PACKAGE_ICON.PNG PACKAGE_ICON_256.PNG
# 生命周期脚本必须带执行位（同 MSYS 失效问题）
for s in preinst postinst preuninst postuninst preupgrade postupgrade start-stop-status real-start-stop-status; do
  tar tvf "${PROJECT_ROOT}/${SPK_NAME}" | grep -E "^-r.x" | grep -q " scripts/${s}\$" || {
    echo "SPK 内 scripts/${s} 缺执行位" >&2
    exit 1
  }
done
tar tf "${PROJECT_ROOT}/${SPK_NAME}" | grep -q "^WIZARD_UIFILES/install_uifile$" || {
  echo "SPK 内缺 WIZARD_UIFILES/install_uifile（安装向导）" >&2
  exit 1
}
(cd "${PROJECT_ROOT}" && sha256sum "${SPK_NAME}" > "${SPK_NAME}.sha256")

echo "==> 完成: ${PROJECT_ROOT}/${SPK_NAME}"
tar tf "${PROJECT_ROOT}/${SPK_NAME}"
