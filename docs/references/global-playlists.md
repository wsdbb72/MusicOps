# 主流歌单读取：能力、授权与参考记录

[返回首页](../../README.md) · [操作指南](../用户操作说明.md)

本文记录 MusicOps 0.4 系列的歌单元数据读取边界。平台规则、公开页面与账号权限可能变化，以下以 **2026-10-15** 核对的文档和当前适配器为依据。MusicOps 不通过这些接口下载平台音频；歌单目标随后进入独立的资源搜索、身份核对和文件验证流程。

## “支持”具体表示什么

| 层次 | 说明 |
| --- | --- |
| 链接识别 | 能从受支持的完整链接、分享文本或选定平台后的 ID 建立来源定位 |
| 读取可见条目 | 获得当时页面或接口返回的元数据，可能只是预览 |
| 清单完整 | 总数、分页、实际条目对账通过；不是仅因为返回 HTTP 200 |
| 资料完整 | 歌名、艺人、专辑与时长等足够可靠；与条目数量完整分开判断 |
| 可下载或可入库 | 必须另有可用音源、权限与完整音频校验；歌单可读并不等于音源可取 |

需要授权、限流、网页访问拦截、页面变化和部分数据都会保留状态。没有可靠总数或续页未完成时，不宣称读到了完整歌单。用户可以明确选择可见曲目，也可以导入更完整的文件。

## Spotify

**可选授权**：Spotify OAuth Access Token 原文（可带 `Bearer`），或：

```json
{"access_token":"在本机填写有效Token","market":"US"}
```

Token 只发送至 `api.spotify.com`。2026 年官方读取接口为 `/v1/playlists/{id}/items`，当前仅允许读取该用户拥有或作为协作者的歌单，其他列表可能返回 403；仅凭“歌单公开”无法推断官方 API 一定授权。[官方 Get Playlist Items](https://developer.spotify.com/documentation/web-api/reference/get-playlists-items)、[Get Playlist](https://developer.spotify.com/documentation/web-api/reference/get-playlist)。

适配器按接口分页与总数核对。未填写 Token 时尝试公开网页预览，该路径始终保留“可能不完整”状态；预览缺少的专辑等信息不填造。向导不自动办理 OAuth 登录，也不自动刷新 Token，过期后需重新配置或改用导出文件。

本轮有模拟接口分页、权限和部分响应测试；本地网络读取曾超时，不能据此宣称 Spotify 所有真实歌单已端到端验收。

## Apple Music

当前读取 **共享 Catalog 歌单**，优先保留完整链接中的地区和歌单名称路径，例如 `/us/playlist/名称/pl.…`。请直接粘贴原分享链接；仅填裸 `pl.…` ID 可能缺少准确地区或网页路径。

不填授权时，尝试公开页面内可核实的结构化总数与歌单曲目列表。若网页只返回预览、发生不受支持的地区跳转或数量不符，保留不完整 / 访问失败状态。

**可选授权**：Developer Token / JWT 原文（可带 `Bearer`），或：

```json
{"developer_token":"在本机填写有效DeveloperToken"}
```

Token 只发送至 `api.music.apple.com`。Catalog 请求使用 `storefront` 地区与歌单 ID，并沿曲目关系的 `next` 分页。当前没有读取私人资料库歌单或获取 Music User Token 的登录流程。[官方 Get a Catalog Playlist](https://developer.apple.com/documentation/applemusicapi/get-a-catalog-playlist)、[Apple Music API](https://developer.apple.com/documentation/applemusicapi)。

本轮开发已通过实际适配器读取公开 Today’s Hits 歌单：来源声明 **50 首**，实际返回 **50 首**，`complete=true`，资料缺失 **0**，无完整性警告。这验证了该次公开网页路径；不代替已打包应用、Developer Token、所有地区或私人歌单的现场验收。

## YouTube / YouTube Music

输入含 `list=` 的播放列表链接。未填授权时，尝试公开初始数据与只读续页；仅在存在可核实的总数、分页上下文完整且续页耗尽时，才可能报告清单完整。缺少上下文、续页中断、已删除或地区不可见条目都可能造成部分结果。

**可选授权**：API Key 原文、`{"api_key":"在本机填写Key"}`，或 OAuth `{"access_token":"在本机填写Token"}`。凭据只发送至 `www.googleapis.com`，不转发到公开页面。官方路径使用 `playlistItems.list` 读取条目，并用 `videos.list` 补充可用视频的时长。[PlaylistItems: list](https://developers.google.com/youtube/v3/docs/playlistItems/list)、[Videos: list](https://developers.google.com/youtube/v3/docs/videos/list)。

YouTube 的视频标题与频道名不是可靠的歌曲 / 歌手 / 专辑字段。普通视频不把频道自动当作音乐艺人；YouTube Music 若有明确艺人、专辑关系则可保留，否则等待用户核对。没有专辑的演出、混剪或原声不能伪装成某个正式发行。

本轮模拟测试覆盖官方分页、公开视频续页与缺失资料；未宣称当前网络环境已完成 YouTube 全链路真实验收。

## bilibili

链接定位支持收藏夹与已适配的合集列表，实际可见性仍受公开状态和平台访问策略影响。可选 Cookie-Editor JSON 数组或 `{"cookies":[…]}`，仅按域名白名单发送到 `api.bilibili.com`。

普通视频的 UP 主不自动映射成音乐艺人，视频标题也可能只是节目或分 P 名称。缺少可信专辑或演唱信息时保持缺失，后续应核对音乐身份，或导入已有可靠标签的曲目表。不要把一个视频合集当作已确认的音乐发行。

本轮公开 API 曾返回 412 访问拦截，因此不会把合成测试描述成真实账号全部通过；用户可在能访问原列表的客户端导出文件，再导入 MusicOps。

## 国内平台与现场样本

网易云沿用清单、分批详情与完整性对账。QQ、汽水、酷狗、酷我、咪咕具有独立的只读分页适配，授权仅在需要时填写。

本轮公开样本对 QQ **637 / 637**、汽水 **313 / 313**、酷我 **484 / 484**、咪咕 **13 / 13**，以及两个酷狗样本 **2 / 2**、**62 / 62** 个条目完成数量对账。样本数量用于说明当次验证范围，不保证任意歌单或未来接口持续可用。酷狗分享页仍可能仅给预览，其他公开 Collection / 普通歌单的完整度应以该次分页与总数核对为准；未确认完整的清单保留部分状态。

这些公开读取路径不是平台对第三方稳定性、永久可用性或音频下载权限的承诺。遇到结构变化或访问限制时，保留错误与导出文件兜底。

## 文件导入与身份隔离

所有平台都可走同一文件导入入口：

- **JSON**：推荐 Playlist Out Canonical JSON，或受支持的歌曲对象数组与常见平台结构。
- **CSV / TSV**：按明确中英文列名读取，不按第几列猜歌名和歌手；支持常见 Exportify 与 `TITLE / ARTIST / ALBUM` 表格。
- **TXT**：普通文本需明确 `title-artist` 或 `artist-title` 顺序。已识别的 Playlist Out 信头有固定顺序；其他不明格式返回待选择，不静默交换歌名和艺人。
- **M3U / M3U8**：读取 `EXTINF` 元数据，不打开音频条目。
- **XLSX**：标准库只读 OOXML，支持普通曲目表与 `Tracks / Playlist Info`；拒绝宏、公式、外部工作表引用和过量 ZIP 展开。

单文件 20 MB、最多 50,000 首；优先 UTF-8 / BOM，GB18030 回退会明确标记。没有专辑、艺人或时长时保持为空，不为通过校验伪造值。原歌曲 ID 缺失时只生成稳定的本地目标标识，`source_song_id` 仍为空，不生成平台歌曲 ID。

每条目标都有来源命名空间，例如 `qqmusic:42`、`netease:42` 不相同。跨平台回退网易云时，原平台 ID 不作网易云下载参数；必须查到唯一且歌名、艺人、专辑、时长均匹配的曲目，才产生明确的网易云引用。无唯一匹配或资料缺失时等待核对。

## 参考项目与许可证

参考格式或公开行为不代表复用其完整项目，也不等于沿用其网络请求、授权或部署方案。

| 参考 | 核对的材料与用途 | 许可证与使用范围 |
| --- | --- | --- |
| [Playlist Out](https://github.com/LengxiQwQ/playlistout) | [Canonical JSON](https://github.com/LengxiQwQ/playlistout/blob/main/docs/JSON-SCHEMA.md)、[导出格式规范](https://github.com/LengxiQwQ/playlistout/blob/main/docs/EXPORT-FORMATS.md)、[原始导出器](https://github.com/LengxiQwQ/playlistout/blob/main/cli/core/exporter.py)；对齐 JSON、CSV、XLSX、TXT、M3U8 交换格式 | [MIT](https://github.com/LengxiQwQ/playlistout/blob/main/LICENSE)；保留[上游许可](PlaylistOut-LICENSE.txt)，MusicOps 文件导入器独立实现 |
| [PlaylistAggregatorLite](https://github.com/BabaBoyGegeBoy/PlaylistAggregatorLite) | [原始界面](https://github.com/BabaBoyGegeBoy/PlaylistAggregatorLite/blob/main/static/src/App.vue) 的文本顺序、对象数组与文件交互格式 | [AGPL-3.0](https://github.com/BabaBoyGegeBoy/PlaylistAggregatorLite/blob/main/LICENSE)；仅参考输出格式，不复制其源码 |
| [GoMusic](https://github.com/Bistutu/GoMusic) | [网易云读取逻辑](https://github.com/Bistutu/GoMusic/blob/main/logic/neteasy.go) 与歌单文本迁移的思路 | [MIT](https://github.com/Bistutu/GoMusic/blob/main/LICENSE)；独立实现当前适配行为 |
| [XueshiQiao 网易云导出示例](https://gist.github.com/XueshiQiao/858974dbf86bc91030fbf49f9c6daf43) | 原始 gist 的“歌名 - 歌手”文本结构 | 页面未见明确许可，不复制脚本；不在用户浏览器执行该脚本 |
| [Spotifylist-to-gkl](https://github.com/zhangolve/Spotifylist-to-gkl) | [原始转换脚本](https://github.com/zhangolve/Spotifylist-to-gkl/blob/master/Spotifylist-to-gkl.py) 所读取的 `Track Name / Artist Name` 等 CSV 列 | 仓库未见明确许可，不复制代码；兼容列名独立实现 |
| [superjodash YouTube 示例](https://gist.github.com/superjodash/6f82f50418576dd982b48ae33cf962f2) | 原始 gist 的 `TITLE / ARTIST / ALBUM` 表格输出 | 页面未见明确许可，不复制代码；不在用户浏览器执行该脚本 |

上表是实现来源与使用范围记录；各项目的具体许可文本以对应仓库为准。MusicOps 对平台元数据的读取、免费资源请求、下载器管理与音乐文件事务分别实现和验证。
