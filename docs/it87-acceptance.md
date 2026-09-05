# IT87 内置驱动验收清单

在 TAD6S4N10G + fnOS 真机上执行（CI 无法代替内核模块编译）。

## 1. 新装无独立 it87 FPK

1. 确认未安装对方 it87-kmod，且 `lsmod` 无 it87。
2. 安装本插件 FPK 并启动。
3. 期望：`lsmod | grep '^it87 '` 有输出；`/etc/tad-module-it87-managed` 存在；风扇页能读到 RPM。
4. 日志 `var/tad-module.log` 可见 DKMS add/install 成功。

## 2. 已装独立驱动：不双开 DKMS

1. 先安装 [fnos-it87-kmod](https://github.com/IamAyang233/fnos-it87-kmod)，确认 `/etc/it87-kmod-kver` 存在。
2. 再安装/启动本插件。
3. 期望：日志含「跳过自建 DKMS」；不会另起一套冲突的托管标记（或保持不写 `/etc/tad-module-it87-managed`）。

## 3. 内核升级自愈

1. 在本插件托管场景下，将 `/etc/tad-module-it87-kver` 改成旧值或删除。
2. 执行模块 stop/start 或 upgrade。
3. 期望：触发 `dkms install --force`，文件写回当前 `uname -r`。

## 4. 停止/卸载不 rmmod

1. 停止本插件：`lsmod` 仍有 it87。
2. 卸载本插件：仍不 `rmmod`；可清理本插件的 DKMS 注册与 `tad-module-it87` conf。

## 5. 许可文件

仓库含 `third_party/it87/COPYING`（GPL-2.0）与 `NOTICE`；根目录 MIT `LICENSE` 不变。
