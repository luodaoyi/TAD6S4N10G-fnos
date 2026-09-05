#!/bin/sh
# TAD 内置 IT87 DKMS 辅助：安装/升级/启动时确保模块可用；停止/卸载不 rmmod。
# 可被同目录生命周期脚本 source。

export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:${PATH:-}"

IT87_DKMS_NAME="it87"
IT87_DKMS_VER="v1.0-48-g40bec4b"
IT87_SRC_DIR="/usr/src/${IT87_DKMS_NAME}-${IT87_DKMS_VER}"
IT87_EXT_KVER_FILE="/etc/it87-kmod-kver"
IT87_OUR_KVER_FILE="/etc/tad-module-it87-kver"
IT87_OUR_MARKER="/etc/tad-module-it87-managed"
IT87_OUR_MODCONF="/etc/modprobe.d/tad-module-it87.conf"
IT87_OUR_LOADCONF="/etc/modules-load.d/tad-module-it87.conf"
IT87_EXT_MODCONF="/etc/modprobe.d/it87.conf"
IT87_EXT_LOADCONF="/etc/modules-load.d/it87.conf"
IT87_FALLBACK_ID="0x8613"

it87_app_dest() {
  if [ -n "${TRIM_APPDEST:-}" ]; then
    printf '%s\n' "${TRIM_APPDEST}"
    return 0
  fi
  printf '%s\n' "${APP_DEST:-/var/apps/tad-module/target}"
}

it87_pkg_var() {
  if [ -n "${TRIM_PKGVAR:-}" ]; then
    printf '%s\n' "${TRIM_PKGVAR}"
    return 0
  fi
  printf '%s\n' "${PKG_VAR:-/var/apps/tad-module/var}"
}

it87_log() {
  _it87_log="$(it87_pkg_var)/tad-module.log"
  mkdir -p "$(it87_pkg_var)" 2>/dev/null || true
  printf '%s it87-dkms: %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*" >>"${_it87_log}" 2>/dev/null || true
}

it87_module_loaded() {
  lsmod 2>/dev/null | grep -q "^${IT87_DKMS_NAME} "
}

it87_external_managed() {
  # 对方独立 FPK 的内核版本记录 / conf：只要存在即视为外部优先（不依赖无 OUR_MARKER）
  if [ -f "${IT87_EXT_KVER_FILE}" ]; then
    return 0
  fi
  # 对方写入的 modules-load / modprobe（非我们的 tad-module-* 文件名）
  if [ -f "${IT87_EXT_LOADCONF}" ]; then
    return 0
  fi
  if [ -f "${IT87_EXT_MODCONF}" ]; then
    return 0
  fi
  # DKMS 已注册但不是我们托管的（仍用 marker 区分自建残留）
  if command -v dkms >/dev/null 2>&1; then
    if dkms status -m "${IT87_DKMS_NAME}" -v "${IT87_DKMS_VER}" >/dev/null 2>&1 \
      && [ ! -f "${IT87_OUR_MARKER}" ]; then
      return 0
    fi
  fi
  return 1
}

it87_should_skip_build() {
  if it87_external_managed; then
    it87_log "检测到独立 it87-kmod / 外部 DKMS 管理，跳过自建 DKMS"
    return 0
  fi
  # 模块已加载且非我们托管：不双开 DKMS
  if it87_module_loaded && [ ! -f "${IT87_OUR_MARKER}" ]; then
    it87_log "it87 模块已加载且非本插件托管，跳过自建 DKMS"
    return 0
  fi
  return 1
}

it87_bundled_src() {
  printf '%s/it87-src\n' "$(it87_app_dest)"
}

it87_check_toolchain() {
  _krel="$(uname -r)"
  _missing=""
  for _c in dkms gcc make; do
    command -v "${_c}" >/dev/null 2>&1 || _missing="${_missing} ${_c}"
  done
  [ -e "/lib/modules/${_krel}/build" ] || _missing="${_missing} linux-headers-${_krel}"

  if [ -n "${_missing}" ]; then
    it87_log "缺少构建依赖:${_missing}，尝试 apt 安装"
    if command -v apt-get >/dev/null 2>&1; then
      timeout 180 apt-get update >>"$(it87_pkg_var)/tad-module.log" 2>&1 || true
      timeout 180 apt-get install -y dkms gcc make "linux-headers-${_krel}" >>"$(it87_pkg_var)/tad-module.log" 2>&1 || true
    fi
    _missing=""
    for _c in dkms gcc make; do
      command -v "${_c}" >/dev/null 2>&1 || _missing="${_missing} ${_c}"
    done
    [ -e "/lib/modules/${_krel}/build" ] || _missing="${_missing} linux-headers-${_krel}"
  fi

  if [ -n "${_missing}" ]; then
    _msg="IT87 DKMS 失败：缺少构建工具/内核头文件(${_missing})。请在飞牛终端执行: apt-get update && apt-get install -y dkms gcc make linux-headers-\$(uname -r) 后重启本模块或重新安装。"
    it87_log "${_msg}"
    printf '%s\n' "${_msg}" >"${TRIM_TEMP_LOGFILE:-/dev/stderr}"
    return 1
  fi
  it87_log "构建工具链就绪 (krel=${_krel})"
  return 0
}

it87_probe_sio_devid() {
  _cc="$(command -v gcc || command -v cc || true)"
  [ -z "${_cc}" ] && return 1
  _src="$(mktemp /tmp/tad_sio_probe.XXXXXX.c)"
  _bin="$(mktemp /tmp/tad_sio_probe.XXXXXX)"
  cat >"${_src}" <<'CEOF'
#include <stdio.h>
#include <sys/io.h>
static int read_id(unsigned int p){
    outb(0x87,p); outb(0x01,p); outb(0x55,p); outb(0x55,p);
    outb(0x20,p); unsigned char lo=inb(p+1);
    outb(0x21,p); unsigned char hi=inb(p+1);
    outb(0x02,p); outb(0x02,p+1);
    return (hi<<8)|lo;
}
int main(){
    unsigned int ports[]={0x2e,0x4e};
    int ids[]={0x8603,0x8606,0x8607,0x8613,0x8620,0x8622,0x8623,0x8625,
               0x8628,0x8655,0x8665,0x8686,0x8790,0x8791,0x8792,0x8795,0x8796,0x8797};
    int n=(int)(sizeof(ids)/sizeof(ids[0]));
    for(int k=0;k<2;k++){
        if(ioperm(ports[k],2,1)!=0) continue;
        int id=read_id(ports[k]);
        ioperm(ports[k],2,0);
        for(int i=0;i<n;i++){ if(ids[i]==id){ printf("0x%04x\n",id); return 0; } }
    }
    return 1;
}
CEOF
  _rc=1
  if "${_cc}" -O2 "${_src}" -o "${_bin}" 2>/dev/null; then
    "${_bin}"; _rc=$?
  fi
  rm -f "${_src}" "${_bin}"
  return ${_rc}
}

it87_register_and_build() {
  _bundled="$(it87_bundled_src)"
  if [ ! -d "${_bundled}" ] || [ ! -f "${_bundled}/dkms.conf" ]; then
    _msg="IT87 DKMS 失败：未找到内置源码 ${_bundled}（FPK 是否包含 app/it87-src？）"
    it87_log "${_msg}"
    printf '%s\n' "${_msg}" >"${TRIM_TEMP_LOGFILE:-/dev/stderr}"
    return 1
  fi

  it87_check_toolchain || return 1

  it87_log "复制 DKMS 源码到 ${IT87_SRC_DIR}"
  rm -rf "${IT87_SRC_DIR}"
  cp -r "${_bundled}" "${IT87_SRC_DIR}"
  chmod -R u+rwX,go+rX "${IT87_SRC_DIR}"

  if ! dkms status -m "${IT87_DKMS_NAME}" -v "${IT87_DKMS_VER}" >/dev/null 2>&1; then
    it87_log "dkms add ${IT87_DKMS_NAME} ${IT87_DKMS_VER}"
    dkms add -m "${IT87_DKMS_NAME}" -v "${IT87_DKMS_VER}" >>"$(it87_pkg_var)/tad-module.log" 2>&1 || true
  fi

  it87_log "dkms install ${IT87_DKMS_NAME} ${IT87_DKMS_VER} --force"
  if ! dkms install -m "${IT87_DKMS_NAME}" -v "${IT87_DKMS_VER}" --force >>"$(it87_pkg_var)/tad-module.log" 2>&1; then
    _msg="IT87 DKMS 编译/安装失败，请查看 $(it87_pkg_var)/tad-module.log；常见原因是缺少 linux-headers-\$(uname -r) 或 gcc。"
    it87_log "${_msg}"
    printf '%s\n' "${_msg}" >"${TRIM_TEMP_LOGFILE:-/dev/stderr}"
    return 1
  fi

  echo "$(uname -r)" >"${IT87_OUR_KVER_FILE}"
  echo "tad-module" >"${IT87_OUR_MARKER}"
  return 0
}

it87_configure_modprobe() {
  # 仅在我们托管时写入可识别的 conf；不 rmmod 已加载模块来重试（KISS）
  if it87_module_loaded; then
    if [ ! -f "${IT87_OUR_MODCONF}" ]; then
      echo "options it87 ignore_resource_conflict=1" >"${IT87_OUR_MODCONF}"
    fi
    echo "it87" >"${IT87_OUR_LOADCONF}"
    it87_log "模块已加载，写入 modules-load 配置"
    return 0
  fi

  modprobe "${IT87_DKMS_NAME}" ignore_resource_conflict=1 >>"$(it87_pkg_var)/tad-module.log" 2>&1 || true
  if it87_module_loaded; then
    echo "options it87 ignore_resource_conflict=1" >"${IT87_OUR_MODCONF}"
    echo "it87" >"${IT87_OUR_LOADCONF}"
    it87_log "驱动自动探测成功"
    return 0
  fi

  _force_id="$(it87_probe_sio_devid 2>/dev/null || true)"
  if [ -n "${_force_id}" ]; then
    it87_log "探测到芯片 ID=${_force_id}，写入 force_id"
    echo "options it87 ignore_resource_conflict=1 force_id=${_force_id}" >"${IT87_OUR_MODCONF}"
    modprobe "${IT87_DKMS_NAME}" ignore_resource_conflict=1 "force_id=${_force_id}" >>"$(it87_pkg_var)/tad-module.log" 2>&1 || true
  else
    it87_log "未能探测 Super I/O，回退 force_id=${IT87_FALLBACK_ID}"
    echo "options it87 ignore_resource_conflict=1 force_id=${IT87_FALLBACK_ID}" >"${IT87_OUR_MODCONF}"
    modprobe "${IT87_DKMS_NAME}" ignore_resource_conflict=1 "force_id=${IT87_FALLBACK_ID}" >>"$(it87_pkg_var)/tad-module.log" 2>&1 || true
  fi
  echo "it87" >"${IT87_OUR_LOADCONF}"
  return 0
}

it87_ensure_built_for_current_kernel() {
  _cur="$(uname -r)"
  _built=""
  [ -r "${IT87_OUR_KVER_FILE}" ] && _built="$(tr -d '[:space:]' <"${IT87_OUR_KVER_FILE}")"
  if [ "${_cur}" = "${_built}" ] && it87_module_loaded; then
    it87_log "内核未变化且模块已加载 (${_cur})"
    return 0
  fi
  if [ "${_cur}" = "${_built}" ]; then
    it87_log "内核未变化 (${_cur})，尝试 modprobe"
    modprobe "${IT87_DKMS_NAME}" 2>/dev/null || true
    return 0
  fi

  it87_log "内核变化: 上次=[${_built:-无}] 当前=[${_cur}]，重新编译"
  if [ ! -d "${IT87_SRC_DIR}" ] || ! dkms status -m "${IT87_DKMS_NAME}" -v "${IT87_DKMS_VER}" >/dev/null 2>&1; then
    it87_register_and_build || return 1
  else
    it87_check_toolchain || return 1
    if dkms install -m "${IT87_DKMS_NAME}" -v "${IT87_DKMS_VER}" --force >>"$(it87_pkg_var)/tad-module.log" 2>&1; then
      echo "${_cur}" >"${IT87_OUR_KVER_FILE}"
      it87_log "内核升级后重编译完成 (${_cur})"
    else
      it87_log "WARN: 内核升级后重编译失败，仍尝试加载现有模块"
      return 1
    fi
  fi
  return 0
}

# 主入口：install / upgrade / start
# 返回 0 表示跳过或成功；非 0 表示失败（调用方可选择非致命）
it87_ensure() {
  if it87_should_skip_build; then
    # 外部已管：尽量确保能 modprobe（不注册我们的 DKMS）
    if ! it87_module_loaded; then
      modprobe "${IT87_DKMS_NAME}" 2>/dev/null || true
    fi
    return 0
  fi

  if [ -f "${IT87_OUR_MARKER}" ]; then
    it87_ensure_built_for_current_kernel || return 1
    if ! it87_module_loaded; then
      modprobe "${IT87_DKMS_NAME}" 2>/dev/null || return 1
    fi
    it87_module_loaded || return 1
    return 0
  fi

  # 首次由本插件托管
  it87_register_and_build || return 1
  it87_configure_modprobe
  if ! it87_module_loaded; then
    it87_log "WARN: DKMS 安装完成但 modprobe 后模块仍未加载"
    return 1
  fi
  it87_log "it87 DKMS 由 tad-module 托管安装完成"
  return 0
}

# 卸载清理：不 rmmod；仅清理我们写入且可识别的文件/DKMS 注册
it87_cleanup_ours() {
  if [ ! -f "${IT87_OUR_MARKER}" ]; then
    it87_log "卸载：无本插件 IT87 托管标记，跳过 DKMS/conf 清理（不 rmmod）"
    return 0
  fi
  if it87_external_managed; then
    # 理论上有 OUR_MARKER 时 external_managed 为假；双保险
    it87_log "卸载：检测到外部管理痕迹，仅移除本插件标记文件，不 dkms remove / 不 rmmod"
    rm -f "${IT87_OUR_MARKER}" "${IT87_OUR_KVER_FILE}" "${IT87_OUR_MODCONF}" "${IT87_OUR_LOADCONF}"
    return 0
  fi

  it87_log "卸载：清理本插件 DKMS/conf/标记（明确不 rmmod it87）"
  # 模块仍在用时不 dkms remove；保留 marker/conf/SRC，便于后续卸载或启动重试清理
  if it87_module_loaded; then
    it87_log "it87 仍在加载中，跳过 dkms remove；保留 marker/conf/SRC"
    return 0
  fi
  if ! command -v dkms >/dev/null 2>&1; then
    it87_log "WARN: dkms 不可用，跳过 dkms remove；保留 marker/conf/SRC 以便后续重试"
    return 1
  fi
  if ! dkms remove -m "${IT87_DKMS_NAME}" -v "${IT87_DKMS_VER}" --all >>"$(it87_pkg_var)/tad-module.log" 2>&1; then
    it87_log "WARN: dkms remove 失败，保留 marker/conf/SRC 以便后续重试"
    return 1
  fi
  rm -f "${IT87_OUR_MODCONF}" "${IT87_OUR_LOADCONF}" "${IT87_OUR_KVER_FILE}" "${IT87_OUR_MARKER}"
  rm -rf "${IT87_SRC_DIR}"
  it87_log "卸载清理完成"
  return 0
}