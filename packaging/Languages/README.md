# Windows 安装向导的简体中文翻译

`ChineseSimplified.isl` 来自 [kira-96 / Inno-Setup-Chinese-Simplified-Translation](https://github.com/kira-96/Inno-Setup-Chinese-Simplified-Translation)，维护者为 Zhenghan Yang（Kira）。

固定使用提交 [`1ff90acc4ed4aee82b1cda43253243deee3daed4`](https://github.com/kira-96/Inno-Setup-Chinese-Simplified-Translation/blob/1ff90acc4ed4aee82b1cda43253243deee3daed4/ChineseSimplified.isl) 中的 Inno Setup 6.5.0+ 翻译。本机原有、已验收的翻译与该版本逐行相同；仅将 CRLF 换行统一为 LF，未修改翻译内容和原作者文件头。

LF 文本的 SHA-256：`bf0751fa176569c6faa2f6e17ed2734617bef325d5cc06eae030fdd0258ee778`。

该项目采用 MIT 许可，Copyright (c) 2019 - 2020 kirakira。完整授权原文保存在本目录的 [LICENSE](LICENSE)，与上述固定提交一致。更新翻译时必须同时核对固定来源、许可和原生安装回归。

Inno Setup 的编译器安装包不一定包含这份用户贡献翻译。`MusicOps.iss` 因此直接使用仓库内的语言文件，无需修改编译器安装目录，也不在构建时下载可变的翻译文件。

`collect_licenses.py` 会将本目录的 LICENSE 和来源说明复制到运行包的 `licenses/Inno-Setup-Translation/`，使 Windows 安装包、便携版及源码发行包均保留完整授权。Mac 运行包也保留该说明；Mac 安装向导使用系统自身的本地化，不使用此翻译。
