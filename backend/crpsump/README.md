# psu-monitor-go — CRPS 电源 PMBus 监控（Go 版）

光宝 LiteOn PS-2551-5L CRPS 550W 电源的 PMBus 监控服务，通过主板 F_PANEL GPIO
软件 bitbang I2C 读取电压/电流/功率/温度/转速/状态字，并控制风扇。
本工程是原 Python 版（psu-monitor-fpk）的 **Go 语言移植**，API 与启动参数完全兼容。

在本仓库（fnOS 电源应用）中，它是独立于主应用 `tad-module` 的板块入口：业务代码位于仓库根的
`internal/crpsump`，serve 入口在 `backend/crpsump`，与主应用相互独立、不共享配置与状态。

## 版本
- **1.0.49**：状态快照缓存——后台每 3s 刷新一轮完整状态, /api/status 直接回缓存 (≤3s 旧), 打开页面立即显示数据; 快照未就绪时回退实时读取。耗电图表 kWh 单位移至右上角, 彻底避开与刻度数字重叠。
- **1.0.48**：风扇手动占比范围收紧为 10-100%——前端滑条 min 由 0 改 10、提示文案同步；后端 /api/fan 与 writeFanPct 对 <10 的占比钳制到 10%（光宝固件 10% 以下实测无效）。
- **1.0.47**：功能精简——移除温控曲线（前端面板/导出、recordCurve 采样、GET /api/curve）；移除 GPIO 配置区两个诊断按钮及其后端（"试0x3A恢复自动" GET /api/fan-test、"写假温度测风扇" GET /api/temp-test），连带删除仅被诊断使用的 writeByte/readByteData/tempToLinear11/hexStr/pval；风扇手动/自动控制、状态读取、耗电统计、GPIO 配置/探测均不受影响。
- **1.0.46**：新增耗电量统计——后台每 15s 采样 READ_PIN 矩形积分，按总量/今日/本月/按日明细累计；持久化 energy.json（默认与 --config 同目录，可用 --energy 指定，临时文件+rename 原子写入），服务重启若距上次落盘 ≤10 分钟按断电前功率补计；新增 GET /api/energy 与 POST /api/energy（统计清零）；前端新增"耗电统计"面板（累计/今日/本月/实时功率卡片 + 近 14 日柱状图 + 统计清零）。
- **1.0.45**：并发与安全加固——总线切换/复位全程加锁消除数据竞争；/dev/port 句柄常开+方向寄存器缓存（位速率提升）；NewBus 校验非法引脚名防向 DMA 区写入；Unix socket 权限收紧 0750；日志 O_NOFOLLOW+按大小轮转；文件头补 MIT 署名；/api/config readable 加 TTL 缓存；诊断端点支持请求取消；自动探测与连通检测去重重试。
- **1.0.44**：后端移植为 Go 静态二进制（不依赖 Python 解释器）
- 默认接线：SCL=Pin19、SDA=Pin20，PMBus 地址 0x5B

## 目录
```
main.go                  后端入口（flag 解析与启动编排）
../../internal/crpsump/  业务包（log/portio/bus/energy/http 按职责拆分）
ui/static/               前端三件套（index.html / app.js / styles.css，与原版一致）
```

## 编译
在仓库根（module `fnos-powerguard`，Go 1.25+）执行：
```bash
go build ./backend/crpsump   # 本机架构
# 飞牛 NAS（x86_64）交叉编译（scripts/build.sh 已包含 crps-module 目标，产物 app/bin/crps-module）：
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-s -w" -o app/bin/crps-module ./backend/crpsump
```

## 运行
```bash
# TCP 模式（本机调试）
./crps-module serve --addr 127.0.0.1 --port 3878 --web-root ui/static

# 飞牛网关模式（Unix socket，由 fnOS 网关反代）
./crps-module serve --socket /path/psu-monitor.sock \
  --web-root ui/static --config gpio.json --pid psu-monitor.pid --log psu-monitor.log
# 耗电统计文件默认与 --config 同目录 (energy.json)，也可用 --energy /path/energy.json 指定
```
`--web-root` 默认值 `ui/static` 相对工作目录解析，即本目录下的 `ui/static`（从 `backend/crpsump` 目录启动时无需调整）。

## 部署

推荐走 fpk 应用包：仓库根执行 `scripts/build.sh` 生成 fpk（脚本会同时编译主应用
`tad-module` 与独立的 `crps-module` 二进制），在 fnOS 应用中心安装即可。

如需手动部署独立二进制到 NAS：

```bash
# 1. 交叉编译（Windows PowerShell 下同理，仅环境变量写法不同）
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-s -w" -o crps-module ./backend/crpsump

# 2. 上传到 NAS（<user>@<NAS_IP> 按实际环境替换）
scp crps-module <user>@<NAS_IP>:/tmp/

# 3. NAS 上安装到应用 bin 目录，并重启托管服务 trim_app_center（拉起有 ~1 分钟延迟）
sudo install -m755 /tmp/crps-module /usr/local/apps/@appcenter/<应用目录>/bin/crps-module
sudo systemctl restart trim_app_center

# 4. 验证（unix socket 直调 API）
curl -s --unix-socket <应用目录>/<socket>.sock http://localhost/api/status | head -c 300
```

注意：UI 静态文件由 FileServer 直接读磁盘，只改前端时覆盖 `ui/static/` 即时生效，无需重启；浏览器需 Ctrl+F5 强刷。
仅改前端可不重启；改了 Go 代码必须重启（pkill + `systemctl restart trim_app_center`）。

## API
| 方法 | 路径 | 说明 |
|---|---|---|
| GET  | /api/status  | 电源参数 JSON（VIN/VOUT/IOUT/PIN/POUT/温度/转速/状态/功率因数） |
| GET  | /api/bus     | 总线与地址 |
| GET  | /api/energy  | 耗电统计（累计/今日/本月/按日明细/实时功率） |
| POST | /api/energy  | {"action":"reset"} 耗电统计清零 |
| GET  | /api/log     | 运行日志下载 |
| GET  | /api/config  | 当前 GPIO 配置/连通性 |
| POST | /api/fan     | {mode: manual/auto, pct} 手动占比范围 10-100% |
| POST | /api/config  | {scl_pin, sda_pin} |
| POST | /api/detect  | 自动探测 PSCL/PSDA |

前端访问时网关前缀为 /app/psu-monitor（由程序自动剥离）。

## 权限
需要 root 权限访问 /dev/port（GPIO bitbang 直接 inb/outb）。
