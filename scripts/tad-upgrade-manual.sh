#!/bin/sh
# tad-module 手动升级（保留全部配置与历史数据）
#
# 为什么需要这个脚本：
#   这台飞牛的「升级安装」流程（app-center 的 update/task）对本模块是空转的——
#   任务返回成功、服务也重启了，但 /usr/local/apps 里的程序文件一个都没被替换
#   （已用文件时间戳、平台记录的 manifest、以及线上页面加载的 ?v= 三方面核实）。
#   而「卸载后全新安装」（install/task）是通的——但那样会清空 @appconf 里的配置。
#
#   本脚本按平台本该做的方式就地替换程序文件：
#     · 动：/usr/local/apps/@appcenter/tad-module/（bin、ui、config、it87-src）
#           /var/apps/tad-module/（manifest、图标、cmd、wizard、config）
#     · 不动：@appconf/tad-module/config.json（按键映射、备份任务、自动备份规则）
#             @appdata/tad-module/（历史曲线数据）
#
# 用法（必须给出安装包路径）：
#   sudo sh tad-upgrade-manual.sh /路径/to/tad-module.fpk
#
# 不设默认值：原来写死的 /vol1/1000/222/tad-module.fpk 只是作者本机的路径，
# 换台机器就不存在，还会让人以为不带参数也能跑。
set -eu

if [ $# -lt 1 ]; then
  echo "用法： sudo sh $0 /路径/to/tad-module.fpk" >&2
  exit 1
fi
PKG=$1
APPCENTER=/usr/local/apps/@appcenter/tad-module
APPMETA=/var/apps/tad-module
BAK=
STAGE=
METABAK=
SVC_WAS_RUNNING=0
SVC_STOPPED=0

# check_archive_members：解包前先看成员名单，拒绝绝对路径与带 .. 的成员。
# tar 以 root 解包，一旦包里混进这类成员就会写到 STAGE 之外（某些 tar / busybox
# 变体并不会替你挡掉）。
check_archive_members() {
  archive=$1
  bad=$(tar -tf "$archive" 2>/dev/null | grep -E '(^/|(^|/)\.\.(/|$))' || true)
  if [ -n "$bad" ]; then
    echo "安装包内有不安全的成员路径（绝对路径或 ..），已中止：" >&2
    printf '%s\n' "$bad" | head -5 >&2
    exit 1
  fi
}

# svc_running：服务此刻是否在运行。套接字是主判据——服务启动时创建、退出时
# 删除，而且 $APPCENTER 就是它实际所在的目录（/var/apps/.../target 指过来）；
# 进程名作辅助判据，防套接字被误删后漏判。
svc_running() {
  if [ -e "$APPCENTER/tad-module.sock" ]; then
    return 0
  fi
  pgrep -f "tad-module serve" >/dev/null 2>&1
}

# cleanup：无论怎么退出都要清掉临时暂存目录；只有确实把「原本在运行」的服务
# 停下来了（SVC_STOPPED=1）又没走到最后一步，才自动回滚，免得升级失败后服务
# 停着、程序目录换了一半，用户还不知道怎么退回去。配置（@appconf）与历史数据
# （@appdata）全程没动过，回滚只涉及程序文件。
#
# trap 之所以挂在第 2 步之前：STAGE 是在第 1 步建的，早退（用法不对、包不存在、
# 本机没装模块）也得把它清掉。但服务相关的动作必须由 SVC_STOPPED 门控——那些
# 早退根本没停过服务，去调 cmd/main 只会平白多一次无意义的重启；服务原本就停着
# 的（SVC_WAS_RUNNING=0）也不能被失败的升级顺手拉起来。
cleanup() {
  status=$?
  if [ -n "$STAGE" ] && [ -d "$STAGE" ]; then
    rm -rf "$STAGE"
  fi
  if [ "$status" = "0" ]; then
    # 成功收尾：平台记录的备份不再需要（程序目录的备份保留，供手动回滚）。
    if [ -n "$METABAK" ] && [ -d "$METABAK" ]; then
      rm -rf "$METABAK"
    fi
    exit "$status"
  fi
  if [ "$SVC_STOPPED" != "1" ]; then
    exit "$status"
  fi
  if [ -n "$BAK" ] && [ -d "$BAK" ]; then
    echo "升级失败（退出码 $status），正在回滚程序目录到 $BAK …" >&2
    restored=1
    rm -rf "$APPCENTER" || restored=0
    mv "$BAK" "$APPCENTER" || restored=0
    # 平台记录（cmd/wizard 等）在第 5 步也换过，必须一并还原，否则 cmd/main
    # 已经不在，下面的 restart 根本跑不起来。
    if [ -n "$METABAK" ] && [ -d "$METABAK" ]; then
      rm -rf "$APPMETA/cmd" "$APPMETA/wizard" 2>/dev/null || true
      cp -a "$METABAK/cmd" "$APPMETA/cmd" || restored=0
      cp -a "$METABAK/wizard" "$APPMETA/wizard" || restored=0
      cp -a "$METABAK/manifest" "$METABAK/ICON.PNG" "$METABAK/ICON_256.PNG" "$APPMETA/" || restored=0
    fi
    if [ "$restored" != "1" ]; then
      echo "回滚失败！程序目录备份在 $BAK，平台记录备份在 $METABAK，请手动恢复：" >&2
      echo "  sudo sh -c 'rm -rf $APPCENTER && mv $BAK $APPCENTER && cp -a $METABAK/cmd $METABAK/wizard $APPMETA/'" >&2
    elif [ "$SVC_WAS_RUNNING" = "1" ]; then
      sh "$APPMETA/cmd/main" restart >/dev/null 2>&1 || true
      echo "已回滚，配置与历史数据未受影响。" >&2
    else
      echo "已回滚，配置与历史数据未受影响（服务原本就是停止状态，未拉起）。" >&2
    fi
  elif [ "$SVC_WAS_RUNNING" = "1" ]; then
    # 已停服务但还没动过程序目录：把原本在运行的服务起回来。
    sh "$APPMETA/cmd/main" start >/dev/null 2>&1 || true
  fi
  exit "$status"
}
trap cleanup EXIT

if [ "$(id -u)" != "0" ]; then
  echo "请用 sudo 运行： sudo sh $0" >&2
  exit 1
fi
if [ ! -f "$PKG" ]; then
  echo "找不到安装包：$PKG" >&2
  exit 1
fi
if [ ! -d "$APPCENTER" ] || [ ! -d "$APPMETA" ]; then
  echo "本机没有安装 tad-module，已中止（请走应用中心正常安装）" >&2
  exit 1
fi

echo "== 1/6 解包并校验 =="
# 固定路径 + rm -rf 在 /tmp 下有被抢符号链接的风险，用 mktemp 拿一个唯一目录，
# 退出时由 cleanup 负责清掉。
STAGE=$(mktemp -d /tmp/tad-upgrade.XXXXXX)
check_archive_members "$PKG"
tar -xf "$PKG" -C "$STAGE"
APP=$(sed -n 's/^appname[[:space:]]*=[[:space:]]*//p' "$STAGE/manifest" | head -1)
VER=$(sed -n 's/^version[[:space:]]*=[[:space:]]*//p' "$STAGE/manifest" | head -1)
if [ "$APP" != "tad-module" ]; then
  echo "包内 appname=$APP，不是 tad-module，已中止" >&2
  exit 1
fi
if [ ! -f "$STAGE/app.tgz" ]; then
  echo "包内缺少 app.tgz，已中止" >&2
  exit 1
fi
echo "    安装包：$PKG"
echo "    包版本：$VER"

echo "== 2/6 停止服务 =="
if svc_running; then
  SVC_WAS_RUNNING=1
fi
if ! sh "$APPMETA/cmd/main" stop; then
  echo "停止服务失败，已中止（程序文件未做任何改动）" >&2
  exit 1
fi
sleep 1
rm -f "$APPCENTER/tad-module.sock"
if svc_running; then
  echo "stop 已执行，但服务仍在运行，已中止升级（程序文件未做任何改动）；请手动确认 tad-module 进程后再试" >&2
  exit 1
fi
SVC_STOPPED=1

echo "== 3/6 备份现有程序目录（可回滚）=="
# 先复制成 .partial，成功后再改名：cp 半途失败时留下的残缺目录不能让 cleanup
# 误当成可用备份拿去回滚。
PARTIAL="$APPCENTER.bak-$(date +%Y%m%d%H%M%S).partial"
cp -a "$APPCENTER" "$PARTIAL"
BAK="${PARTIAL%.partial}"
mv "$PARTIAL" "$BAK"
echo "    $BAK"

echo "== 4/6 展开新版程序文件 =="
check_archive_members "$STAGE/app.tgz"
tar -xzf "$STAGE/app.tgz" -C "$APPCENTER"
chmod +x "$APPCENTER/bin/tad-module" 2>/dev/null || true

echo "== 5/6 更新平台记录（manifest / 图标 / 生命周期脚本）=="
# 先把将被替换的 cmd/wizard 备份下来：下面的 rm -rf 之后一旦复制失败，cleanup
# 才有东西可还原——否则 $APPMETA/cmd/main 已经不在，回滚分支的 restart 根本
# 跑不起来（平台记录备份只保留到本次升级结束）。
# 备份先写进临时目录，cmd/wizard 完整复制成功后才把 METABAK 指过去：cp 半途
# 失败时 cleanup 不能拿着残缺的备份去回滚——回滚分支会先删掉完好的 cmd/wizard
# 再用残缺副本顶上，服务就再也起不来了。
METABAK_TMP="$APPMETA.bak-meta-$(date +%Y%m%d%H%M%S)"
mkdir -p "$METABAK_TMP"
if ! cp -a "$APPMETA/cmd" "$APPMETA/wizard" "$METABAK_TMP/"; then
  rm -rf "$METABAK_TMP"
  echo "备份 cmd/wizard 失败，已中止升级（回滚程序目录后服务会拉回原版）" >&2
  exit 1
fi
if [ -d "$METABAK_TMP/cmd" ] && [ -f "$METABAK_TMP/cmd/main" ] && [ -d "$METABAK_TMP/wizard" ]; then
  # manifest 与图标同样是回滚要原样盖回去的平台记录：复制失败若被忽略，
  # 回滚就会拿残缺备份替换原件，还照样报「已回滚」。这里就地校验，
  # 缺任何一个都中止（此刻还没动过 $APPMETA 里的任何文件）。
  if ! cp -a "$APPMETA/manifest" "$APPMETA/ICON.PNG" "$APPMETA/ICON_256.PNG" "$METABAK_TMP/"; then
    rm -rf "$METABAK_TMP"
    echo "备份平台记录（manifest / 图标）失败，已中止升级（程序目录未替换，服务将原样拉回）" >&2
    exit 1
  fi
  if [ ! -f "$METABAK_TMP/manifest" ] || [ ! -f "$METABAK_TMP/ICON.PNG" ] || [ ! -f "$METABAK_TMP/ICON_256.PNG" ]; then
    rm -rf "$METABAK_TMP"
    echo "备份平台记录（manifest / 图标）失败，已中止升级（程序目录未替换，服务将原样拉回）" >&2
    exit 1
  fi
  METABAK=$METABAK_TMP
else
  rm -rf "$METABAK_TMP"
  echo "备份 cmd/wizard 失败，已中止升级（回滚程序目录后服务会拉回原版）" >&2
  exit 1
fi
cp -a "$STAGE/manifest" "$STAGE/ICON.PNG" "$STAGE/ICON_256.PNG" "$APPMETA/"
rm -rf "$APPMETA/cmd" "$APPMETA/wizard"
cp -a "$STAGE/cmd" "$APPMETA/cmd"
cp -a "$STAGE/wizard" "$APPMETA/wizard"
cp -a "$STAGE/config/." "$APPMETA/config/" 2>/dev/null || true
chmod +x "$APPMETA"/cmd/* 2>/dev/null || true

echo "== 6/6 启动服务 =="
sh "$APPMETA/cmd/main" start
sleep 2
echo
echo "现装版本：$("$APPCENTER/bin/tad-module" version)"
echo "配置（未改动）：$APPMETA/etc/config.json（平台的 @appconf/tad-module/config.json）"
echo "历史数据（未改动）：$APPMETA/var/（平台的 @appdata/tad-module/）"
echo
echo "本次的程序目录备份留在：$BAK"
echo "如需手动回滚： sudo sh -c 'rm -rf $APPCENTER && mv $BAK $APPCENTER && sh $APPMETA/cmd/main restart'"
echo
echo "完成。请回飞牛桌面刷新模块页面（Windows 上 Ctrl+F5；手机 App 里退出该页面重进），"
echo "然后核对按钮控制页的按键映射、备份任务、自动备份规则是否都还在。"

