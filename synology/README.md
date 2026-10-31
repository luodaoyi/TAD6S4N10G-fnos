# TAD6S4N 模块 · 群晖 DSM 移植版

把主线（fnOS fpk 插件）移植到 **群晖 DSM 7.x（x86_64）** 的分支成果。典型场景：
同一块 TAD6S4N10G 主板通过 RR 等引导加载器运行 DSM。后端二进制与主线完全同源，
只把「宿主集成」换成了 DSM 的套件形态。

## 与 fnOS 主线的差异

| 方面 | fnOS 主线 | DSM 移植版 |
| --- | --- | --- |
| 打包 | `manifest` + fnpack → `.fpk` | `synology/` + `scripts/build-spk.sh` → `.spk` |
| 界面入口 | fnOS 应用中心，宿主网关反代到 Unix socket | **DSM 桌面嵌入**：DSM nginx 反代 `/webman/3rdparty/tad-module/` → 本机回环 `127.0.0.1:8123`，主菜单图标直达 |
| 管理员鉴权 | fnOS 网关注入 `X-Trim-Isadmin` | **后端校验 DSM 会话及 administrators 组**（注册表优先、CGI 兜底）；nginx 仅携带共享密钥，认证后由后端注入 `X-Trim-Isadmin` |
| root 权限 | fnOS 模块即 root | **DSM 7 禁止第三方套件以 root 运行**：服务经 [SimplePermissionManager](https://github.com/XPEnology-Community/SimplePermissionManager)（SPM）的 `spm-exec` 提权（见下） |
| it87 风扇驱动 | 无独立 FPK 时经 DKMS 自动编译 | DSM 无 DKMS/gcc/内核头：只探测/加载系统已有的 it87 模块（引导加载器 extra 驱动等），缺驱动时风扇页给 DSM 专属提示 |
| 平台探测 | `/etc/issue`、`/etc/fnos-release` | 新增 `DetectPlatform`：识别 `fnos` / `dsm` / `generic`，随 `/api/status` 的 `platform` 字段下发 |

fnOS 主线行为不受影响：TCP 监听、Basic 认证都是可选 flag，不传即维持原样；
Unix socket 的组权限默认仍是 `www-data`（Debian TUI 依赖不变）。

## 构建

```bash
TAD_SPK_VERSION=0.0.1 scripts/build-spk.sh   # 测试包（约定 0.0.1，资源带时间戳）
scripts/build-spk.sh                          # 正式包（版本取 manifest）
```

打包在 GNU/Linux 工具环境下验证，需 GNU `sed`、`md5sum`、`sha256sum`；
macOS 宿主请先提供这些 GNU 工具，或在 Linux 环境中打包。

本机无 Go 时自动用 docker `golang:1.25` 编译（与 `scripts/build.sh` 同一套参数，
`CGO_ENABLED=0` 静态二进制）。产物在仓库根：`tad-module-v<版本>.spk` + `.sha256`。

## 安装（DSM 7）

**前置：必须先装 [SimplePermissionManager](https://github.com/XPEnology-Community/SimplePermissionManager)。**
DSM 7 起第三方套件服务不允许以 root 运行，而本套件的功耗限制、风扇 PWM、
GPIO 按键、SMART 读取都是 root 硬件通路。SPM 提供 setuid 入口 `spm-exec`，
按「调用方 UID 是否被启用」放权——源码级确认：在 SPM 界面 Package 标签勾选
启用本套件即可，**不需要逐脚本 GPG 签名**（签名链只是免手动勾选的自动批准通道，
见文末「签名自动批准」）。

安装顺序：

1. 套件中心 → 常规 → 信任级别改为 **任何发行者**（本包未签名）。
2. 安装 **SimplePermissionManager**，打开其界面点 **Active** 激活。
3. 套件中心 → 手动安装 → 选择本套件 `.spk`。
4. 打开 SPM → **Package 标签 → 勾选启用 tad-module**。
5. 套件中心启动「TAD6S4N 模块」；DSM 主面板出现图标（默认回环监听，局域网不能直连 8123）。
6. 浏览器打开 DSM 桌面 → 主菜单「TAD6S4N 模块」（或直接访问
   `http://<NAS>:5000/webman/3rdparty/tad-module/`），**鉴权就是 DSM 登录，无需额外密码**。
   实际路径按安装卷不同而不同：程序与配置在 `<安装卷>/@appstore/tad-module/`，
   运行数据在 `<安装卷>/@appdata/tad-module/`；`/var/packages/tad-module/` 下的
   `etc/` 是不被使用的空壳目录（DSM 卷安装布局），以 `ps -p $(awk '{print $1}'
   <安装卷>/@appdata/tad-module/tad-module.pid)` 里的实际参数为准。

**排障**：启动失败且日志（`/var/packages/tad-module/var/tad-module.log` 或
套件信息页）提示需要 root → 检查 SPM 已激活、且 Package 标签已勾选
tad-module；换端口/参数见下节「运行参数」。

## 安装向导（root 提示 + 端口）

安装时会弹出两步向导（`WIZARD_UIFILES/install_uifile`，DSM 7 原生支持安装时
手动设置）：

1. **⚠️本软件需要root授权**——功耗/风扇/GPIO/SMART 都是硬件 root 通路，
   DSM 7 下须经 SPM 提权；这一步就是前置条件的最后提醒，装完再想起 SPM
   是启动失败的最常见原因。
2. **访问方式与端口**——可选「桌面嵌入模式（推荐，仅回环 + DSM 登录鉴权）」
   或「独立端口模式（0.0.0.0 对外监听，自动生成 Basic 凭据）」，并填写端口号
   （默认 8123）。选值在 postinst 落盘到 `etc/tad-module.cfg` 的 `HTTP_ADDR`；
   升级时向导不再出现，已有 cfg 一律保留。端口被占用时启动会被预检拦下并在
   `var/startup-debug.log` 给出处置提示（改 cfg 端口后重启套件即可）。

## root 提权的实现方式

- `conf/privilege` 声明 `run-as: package`——DSM 以套件专属用户运行服务脚本；
- `scripts/start-stop-status` 是薄壳：已是 root（DSM 6/调试）直接执行；
  否则 `exec spm-exec sh real-start-stop-status "$@"`，由 SPM 按 UID 提权；
- **实测 DSM 7（RR）上安装期脚本（postinst 等）也以套件用户运行**：postinst
  初始化目录、凭据和参数文件；其中需要 root 的硬件命令失败时降级安装。
  配置生成和功耗应用由**首次 root 启动**的守护进程完成。DSM 首装的自动
  风扇绑定在套件用户下会失败，首次启动不会补绑；请在风扇页手动配置并
  开启曲线。`preuninst` 的
  停服兜底同样走薄壳（经 SPM 提权）；
- 没装 SPM 时启动会失败并打印可操作的分步指引（fail-closed，不会静默降权
  跑一个半残的守护进程）。

安全说明（上游 review 后加固）：默认只监听 `127.0.0.1`，管理面不对局域网
裸露。DSM 的 `/webman/3rdparty/` 路径**本身没有登录校验**（第三方套件的 CGI
历来要自己做），因此鉴权不依赖该前缀，而是三层防线：

1. **DSM 会话 + 管理员组**：后端优先读取 root-only 会话注册表
   `/usr/syno/etc/private/session/current.users`，未匹配时再本地执行 DSM 自带的
   `authenticate.cgi`（转发完整会话 Cookie）兜底；两种路径均须确认用户属于
   `administrators` 组才注入管理员身份，认证失败返回 401。
   会话判定缓存 **30 秒**以支撑页面轮询；在此窗口内注销（删除注册表记录）、
   有界会话到期或移出管理员组，原授权最多仍可使用 30 秒，窗口结束后的请求
   才重新校验。不会因持续请求延长缓存期限。注册表字段语义来自 DSM 7.3.2
   实测，缺失、畸形或未知取值不作为授权依据；CGI 兜底也失败则拒绝；
2. **反代共享密钥**：nginx 与守护进程之间用 root-only 随机 token
   （`etc/proxy-token`）标识来源，无密钥的直连请求（含本机进程）一律 403；
   客户端自带的 `X-Trim-Isadmin` 头在 TCP 路径上先被剥除，管理员身份只能由
   后端校验后注入；
3. **同源防护**：非幂等请求校验 Origin/Referer（null/畸形/非 http(s)/端口
   不符一律拒绝）。

卸载时会清理写入 DSM nginx 的反代配置（仅删带本套件标记的文件，`nginx -t`
校验通过才 reload），不留残留入口。

若想绕开 DSM 直接访问（如给手机浏览器用）：把
`<安装卷>/@appstore/tad-module/etc/tad-module.cfg` 的 `HTTP_ADDR` 改成
`0.0.0.0:8123` 后重启套件——启动脚本检测到非回环监听会**自动生成 Basic
凭据**（`etc/web-auth.json` + `initial-password.txt`），没有凭据文件时守护
进程拒绝非回环启动（fail-closed）。跨站写请求有 Origin/Referer 同源防护。
**注意：此模式未启用 TLS，Basic 凭据以明文传输，仅建议受信内网使用；更稳妥
的方式是保持默认回环监听、经 HTTPS 的 DSM 入口访问。**
DSM 防火墙开启时，此模式需放行 TCP 8123。

## 运行参数

`<安装卷>/@appstore/tad-module/etc/tad-module.cfg`（升级不覆盖，改后重启套件；
真机教训：DSM/RR 上 `/usr/local` 不保证可写，运行参数一律放套件 etc/ 内）：

```sh
HTTP_ADDR="127.0.0.1:8123"  # 默认仅本机；改为 0.0.0.0:8123 启用 Basic 直连
IT87_FORCE_ID=""      # 仅当系统自带 it87 模块时生效，如 force_id="IT8620E"
```

## it87 风扇驱动（DSM 特有）

DSM 没有 DKMS 工具链，本套件不会编译内核模块。驱动来源（任一）：

- 引导加载器的驱动扩展：RR 用户在 `user_config.yml`/extra 驱动里加入 it87
  （必要时带 `force_id`，与 fnOS 版 `third_party/it87` 适配的机型一致）；
- 系统镜像已内置 it87。

`start-stop-status` 启动时若未见 it87（模块或 hwmon 节点），会按
`IT87_FORCE_ID` 尝试 `modprobe it87`，失败不阻断启动；风扇页随后给出 DSM
专属提示。其余温控行为（三曲线、紧急满速、关停档）与 fnOS 主线一致。

## 套件目录布局

实际布局（卷安装；`/var/packages/tad-module` 部分是空壳，以 ps 里的实际路径为准）：

```
<安装卷>/@appstore/tad-module/      # 套件载荷
├── bin/tad-module                  # Go 后端（与主线同源，root 经 SPM 运行）
├── ui/static/                      # Web 界面 + ui/tad-module 主面板入口
├── etc/                            # config.json、web-auth.json、initial-password.txt、tad-module.cfg
└── scripts/                        # start-stop-status 薄壳 / real-start-stop-status / 安装器脚本

<安装卷>/@appdata/tad-module/       # 运行数据（升级保留）
├── original-state.json、history.db、tad-module.log、tad-module.pid
```

卸载时 `preuninst` 先优雅停服务（守护进程 SIGTERM 退出前恢复功耗/风扇原始
状态），再兜底 `release-fans`，与主线「卸载交还风扇控制权」语义一致。

## 签名自动批准（可选，未接入）

SPM 支持逐脚本 GPG 签名自动批准（免手动勾选）：开发者中间公钥需送
XPEnology-Community 根密钥签发，脚本旁放 `.sig` JSON（sha256 + 签名 + 公钥链），
且 SPM 端开启 TrustSignature。当前版本走「手动勾选启用」路径已够用；若后续
要进 RR 生态的自动分发再申请签名链。

## 已验证环境

- **群晖 DSM 7.3.2**（build 86009-3，RR 引导，TAD6S4N10G / N100，SA6400 平台 v1000，内核 5.10.55+）
  ——安装、启动、桌面内嵌窗口、硬盘面板（6 仓+温度）、风扇通道、网卡固件/PSID、BIOS 渐进式在线检查均实测通过（SPK 本体端到端验收）。

## 已知限制 / TODO

- 未签名包：DSM 7 安装时会提示发行者不受信任，需调信任级别（见上）。
- **必须先安装并激活 SPM、并在 Package 标签启用本套件，服务才能启动**；
  开机自启同样经 SPM 放权（`permissions.json` 持久），SPM 被停用时本套件
  无法重启服务。
- 桌面嵌入依赖启动脚本写 nginx conf.d 并 reload：`/usr/local` 不可写或
  nginx reload 失败时自动降级（日志有提示），此时可用 cfg 里改非回环 +
  Basic 认证的独立端口方式访问。
- DSM 6 未验证：目标只有 DSM 7.x（第三方 root 限制正是 7.0 引入，DSM 6 走
  薄壳的 root 直跑分支理论上可用）。
- DSM 桌面入口使用同源反代；修改回环 `HTTP_ADDR` 端口后重启套件，启动脚本
  会同步更新 nginx upstream，无需修改桌面资源。
- Release CI 已同时构建 FPK、SPK 和各自 SHA-256 校验文件；标签发布还附带
  Debian TUI `tank-linux-amd64`。从对应版本的 GitHub Release 下载：fnOS 在
  应用中心手动安装/升级 `.fpk`，DSM 在套件中心手动安装/升级 `.spk`（升级
  保留配置和运行数据）；下载后可用 `sha256sum -c <包名>.sha256` 验证。
  标签 `v<版本>` 必须与 `manifest` 版本一致；旧版本资产不覆盖。
- CRPS、固件刷写、按键映射等板块依赖硬件通路（`/dev/port`、PMBus），
  在 RR 的 DSM 上可用性取决于引导配置，与 fnOS 版相同的降级逻辑照常生效。
