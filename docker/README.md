# TAD6S4N 模块 · Docker 版

把 [TAD6S4N10G-fnos](https://github.com/luodaoyi/TAD6S4N10G-fnos) 的后端与 Web 界面
打包成 Docker 镜像，运行在 TrueNAS SCALE 或任意支持 Docker 的 Linux 宿主。未修改任何
Go 源码。

## 能力

- ✅ 温度、硬盘仓位/SMART/繁忙度、功耗墙、历史曲线、GPIO 按键、蜂鸣器/LED
- ⚠️ 风扇需宿主编译加载 it87 内核模块；备份仅限挂进容器的路径
- ❌ 开关机计划、固件刷写、压力测试、CRPS

## 快速开始

```bash
cd docker
docker compose up -d --build
docker logs tad-module    # 首次生成的密码
```

访问 `http://<主机IP>:8123/`，用 `admin` + 密码登录。

网络受限时，改 compose 里 `build.args.GOPROXY` 为可达的模块源（不需要可删除该行）。

## 风扇（it87）

内核自带 it87 不识别本板 IT8613E，需用仓库内补丁源码针对宿主内核头文件编译后加载：

```bash
docker compose --profile build run --rm it87-build   # 内核头文件目录见 compose 注释
insmod app/it87-src/it87.ko ignore_resource_conflict=1 force_id=0x8613
```

`.ko` 放持久目录并加开机加载命令；内核升级后重编一次。

## 配置

- 持久化 `/data`：`config.json`、`history.db`、日志、认证
- 环境变量：`TAD_USERNAME`、`TAD_PASSWORD`、`TAD_HTTP_ADDR`

## 已知限制

ZFS 用途列不识别；开关机计划的关机只关容器；固件/压测/CRPS 需宿主级或专用硬件；仅支持
N100/N150/N305。
