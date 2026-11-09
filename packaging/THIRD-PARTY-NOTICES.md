# 第三方依赖与媒体工具

本发行版为 Go 实现：桌面壳、核心、打包与容器入口均为 Go 二进制，不含 Python 解释器，不含 MusicOps 的 Python 代码，不含 PySide6／Qt。Go 模块及版本见 `native/core/go.mod`；分发运行包时应保留实际依赖的许可证原文，`packaging/collect_go_licenses.py` 用于收集：它跑 `go list -m -json all`，把各模块的 LICENSE／LICENCE／NOTICE／COPYING 拷到包内 `licenses/<module>/`，另加 PlaylistOut 许可证与 Inno Setup 翻译声明。

桌面壳使用 [Wails v2.17.1](https://github.com/wailsapp/wails/tree/v2.17.1)，MIT 许可，窗口渲染使用各平台自带 webview（Windows WebView2、macOS WKWebView），无捆绑浏览器运行时。

源码仓库不包含 FFmpeg／ffprobe 二进制；应用通过独立子进程调用随发行包提供或用户指定的工具。具体 FFmpeg 构建可能采用 LGPL 或 GPL 等许可；分发者应根据实际编译选项保留许可证，并满足完整对应源码、构建脚本等义务。参见 [FFmpeg 官方许可说明](https://ffmpeg.org/legal.html)。

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
- [FlareSolverr v3.5.2](https://github.com/FlareSolverr/FlareSolverr/tree/93125d6ea6956906c8dfa29e169b2dc67d993dc3)，MIT；它是第三方提供的 PyInstaller 冻结二进制（非 MusicOps 代码），独立运行时包括其 Python 依赖及对应许可证。
- Chrome for Testing / ChromeDriver 固定为 155.0.8059.84，各系统下载包均校验源码中固定的 SHA-256，并保留浏览器随附的许可说明。

每个完整二进制包的 `tools/services/licenses/engine-sources.zip`（macOS 位于应用的 `Contents/Resources/` 下）包含上述两个服务的对应源码、适配补丁、CLI 实际使用的 Go 模块源码及构建脚本。原始提交和浏览器摘要记录在 `tools/services/engines.json`。可使用 MusicOps 源码包中的 `packaging/prepare_engines.py` 在对应系统原生重建；Go 1.26.3、Python 3.12+（仅构建时工具，运行时不含 Python），首次构建需联网获取固定依赖。MusicOps 源码本身不包含账户数据或第三方二进制。

## OpenCC

本 Go 发行版不含 OpenCC：仓库无字典数据，无 Go 实现，未提供转换端点。旧 Python 版的 opencc-python-reimplemented（Apache-2.0）已随 Python 应用一并移除。

## Docker 镜像

Linux Docker 镜像以 Debian bookworm-slim 为运行时底镜像，从 Debian 软件源安装 Chromium、Chromium Driver、FFmpeg、fonts-noto-cjk、openssl、tini、xvfb 等系统包，保留 `/usr/share/doc` 中的版权说明。可根据 `dpkg-query -W` 获取的精确版本从对应 Debian 软件源获取源码并重建。Docker 的引擎对应源码和 Go 模块许可另见 `/opt/musicops/engines/licenses` 与 `/opt/musicops/licenses`；本文件位于镜像内 `/opt/musicops/THIRD-PARTY-NOTICES.md`。

## 可选外部 musicdl 与 Antra 参考

[CharlesPikachu/musicdl](https://github.com/CharlesPikachu/musicdl) 的当前说明限制打包捆绑分发。本 Go 发行版不含 musicdl 适配器（旧 Python 适配器已随 Python 应用移除），不包含 musicdl 源码、二进制或依赖副本。

[Antra](https://github.com/anandprtp/Antra) 用于了解跨源匹配和完整音源核验流程；本次未复制其代码、私有在线服务或账户接口。平台与内容使用权限由用户自身账户决定。
