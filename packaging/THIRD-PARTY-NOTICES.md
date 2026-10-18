# 第三方依赖与媒体工具

Python 依赖及版本见 `requirements.lock`。分发运行包时应保留实际依赖的许可证原文；`collect_licenses.py` 用于收集已安装依赖的许可文件。

PySide6／Qt 使用其开源许可选项，运行时采用动态库。PyInstaller 的 bootloader 适用其对应许可与例外。mutagen、Pillow、cryptography、HTTP 客户端、Python 运行时及其间接依赖各自保留其许可证。

本仓库不分发 FFmpeg／ffprobe 二进制。应用通过独立子进程调用用户指定的工具。具体 FFmpeg 构建可能采用 LGPL 或 GPL 等许可；分发者应根据实际编译选项保留许可证，并满足完整对应源码、构建脚本等义务。参见 [FFmpeg 官方许可说明](https://ffmpeg.org/legal.html)。

远程网站、音乐资料与歌词接口通过独立适配器接入；本仓库不包含其服务器源码、账户凭据或完整资料数据库。使用远程服务时应遵守各服务的访问规则与内容许可。
## Playlist Out

Kugou public-playlist request formatting/signature material is adapted from
LengxiQwQ/playlistout, Copyright (c) 2026 LengxiQwQ, under the MIT License.
Its complete notice is included in `licenses/PlaylistOut/LICENSE.txt` in the
application distribution and `docs/references/PlaylistOut-LICENSE.txt` in source.

## Inno Setup 简体中文翻译

Windows 安装向导使用 [kira-96/Inno-Setup-Chinese-Simplified-Translation](https://github.com/kira-96/Inno-Setup-Chinese-Simplified-Translation) 的简体中文翻译，固定提交 `1ff90acc4ed4aee82b1cda43253243deee3daed4`，适用 Inno Setup 6.5.0+。
Copyright (c) 2019 - 2020 kirakira，采用 MIT 许可。完整授权及来源说明保存在运行包的 `licenses/Inno-Setup-Translation/` 和源码的 `packaging/Languages/`。

## 内置独立服务（0.4.5 起）

- [go-music-dl v1.1.3](https://github.com/guohuiyuan/go-music-dl/tree/b4aa4c2e00032ff5f636310834168e85bebca010)，AGPL-3.0，作为独立进程通过本机 HTTP 接口调用。MusicOps 适配补丁仅禁止引擎明文持久化平台 Cookie，并增加本机请求鉴权。
- [FlareSolverr v3.5.2](https://github.com/FlareSolverr/FlareSolverr/tree/93125d6ea6956906c8dfa29e169b2dc67d993dc3)，MIT；独立运行时包括其 Python 依赖及对应许可证。
- Chrome for Testing / ChromeDriver 固定为 155.0.8059.84，各系统下载包均校验源码中固定的 SHA-256，并保留浏览器随附的许可说明。

每个完整二进制包的 `tools/services/licenses/engine-sources.zip`（macOS 位于应用的 `Contents/Resources/` 下）包含上述两个服务的对应源码、适配补丁、CLI 实际使用的 Go 模块源码及构建脚本。原始提交和浏览器摘要记录在 `tools/services/engines.json`。可使用 MusicOps 源码包中的 `packaging/prepare_engines.py` 在对应系统原生重建；Go 1.26.3、Python 3.12+，首次构建需联网获取固定依赖。MusicOps 源码本身不包含账户数据或第三方二进制。
