# 文本编码与历史标题更正

## 编码规则

- 文本和中文文件名使用 UTF-8；保留已有 PowerShell 中文脚本的 UTF-8 BOM。
  二进制文件不转码，Git 的八进制路径转义不作为重命名依据。
- 发现错码先核对原始字节与上下文。有正常历史文案时原样恢复，并在 PR
  说明来源 SHA 和位置；只能按上下文重写时应明确说明。
- 编码检查只报告位置和原因，不自动改写内容，也不读取符号链接指向的仓库外文件。
  潜在错码仍须人工核对，不能据此批量替换正常中文或合法 Unicode。

```sh
PYTHONDONTWRITEBYTECODE=1 python3 scripts/test-encoding.py
python3 scripts/check-encoding.py
```

CI 检查追踪文本、文件名及 PR/push 新引入的提交消息；标签和首次推送检查
当前提交。可通过 `--commit-range <完整base-SHA>..<完整head-SHA>` 指定范围。
检测包含正常中文和标点包围的可逆转码片段。

故意包含损坏 Unicode 的文本测试夹具，可在对应行注明
`encoding-check: allow -- 具体测试原因`。这不豁免无效 UTF-8 字节、文件名
或提交消息，也不用于隐藏产品文案问题。

## 历史标题更正对照

[`e26bbedddaa92d9ecc9690fb03bc41b0577f9f3b`](https://github.com/luodaoyi/TAD6S4N10G-fnos/commit/e26bbedddaa92d9ecc9690fb03bc41b0577f9f3b)
的提交消息第 1 行发生了可逆转码损坏。正常标题对应
[PR #33 原题](https://github.com/luodaoyi/TAD6S4N10G-fnos/pull/33)，保留原来的 PR 后缀：

> feat: 图形化备份、三曲线风扇模式、移动设备面板与传感器默认名 (#33)

原题和完整字节转码复现共同支持此对照，不是不可逆损坏后的猜测重写。
这里记录正常标题，**旧提交对象和已发布标签没有被修改**。
