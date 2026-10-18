# 流媒体回退参考与接口边界

本轮阅读和核查以下项目的 README、可用接口或下载流程。MusicOps 的适配、任务状态、校验和界面由本工程独立实现，不捆绑这些第三方项目的代码或可执行文件。

| 项目 | 本轮参考范围 |
| --- | --- |
| [go-music-dl](https://github.com/guohuiyuan/go-music-dl) | 主要接入对象：Web `GET /search?q=...&sources=...&type=song` 的 `li.song-card` 数据字段，`GET /download` 返回音频；默认网页基路径 `/music`。源码定位：`internal/web/music.go`、`templates/partials/song_list.html`、`internal/appshell/server.go`。AGPL-3.0 项目独立部署，MusicOps 经 HTTP 接入。 |
| [qqmusicdownloader](https://github.com/yuqie6/qqmusicdownloader) / [qq_music_download](https://github.com/sky-tool/qq_music_download) | QQ 登录 Cookie、音质与单曲 / 歌单下载交互；没有直接嵌入其凭据或调用未核实接口。 |
| [kuwo-cli](https://github.com/landv/kuwo-cli) / [Kuwo-Music-Downloader-V2.2](https://github.com/iTCraftStudio/Kuwo-Music-Downloader-V2.2) | 酷我搜索和下载流程；MusicOps 实际经统一 Web 服务处理。 |
| [KugouDownloaderNew](https://github.com/CreateDownloader/KugouDownloaderNew) | 酷狗工具说明与接入方式；未将 Windows 专用第三方程序冒充 Mac 功能。 |
| [apple-music-downloader](https://github.com/zhaarey/apple-music-downloader) | ALAC / AAC / Atmos、订阅凭据及独立 wrapper 服务等前提。此版不内置其解密服务，不宣称无配置即可下载 Apple Music 全曲。 |
| [qishui-decrypt-tool](https://github.com/520Qiuyu/qishui-decrypt-tool) | 汽水格式和完整音源条件；实际下载交由配置的服务，MusicOps 完整解码后验收。 |

GitHub Topics 是项目发现页面，不能作为具体下载协议。Spotify / YouTube 原生专有音源须由独立兼容适配器提供；此版可将它们的歌单需求匹配到其他已验证平台的完整音源，但不会称为 Spotify / YouTube 原生下载。

## 自动验收

使用模拟 HTTP 服务返回与所读接口一致的页面、JSON、合成 FLAC，验证跨来源身份核对、拒绝错艺人 / 试听、音频全解码和已完成曲目的恢复缓存。没有为验收消耗真实 FL、发起真实音乐下载或改动个人音乐库。账号权限、服务部署和私人站点均不能由模拟结果推断为已现场通过。
