# 第三方依赖与媒体工具

Python 依赖及版本见 `requirements.lock`。分发运行包时应保留实际依赖的许可证原文；`collect_licenses.py` 用于收集已安装依赖的许可文件。

PySide6／Qt 使用其开源许可选项，运行时采用动态库。PyInstaller 的 bootloader 适用其对应许可与例外。mutagen、Pillow、cryptography、HTTP 客户端、Python 运行时及其间接依赖各自保留其许可证。

本仓库不分发 FFmpeg／ffprobe 二进制。应用通过独立子进程调用用户指定的工具。具体 FFmpeg 构建可能采用 LGPL 或 GPL 等许可；分发者应根据实际编译选项保留许可证，并满足完整对应源码、构建脚本等义务。参见 [FFmpeg 官方许可说明](https://ffmpeg.org/legal.html)。

远程网站、音乐资料与歌词接口通过独立适配器接入；本仓库不包含其服务器源码、账户凭据或完整资料数据库。使用远程服务时应遵守各服务的访问规则与内容许可。
