# 国内歌单直连适配依据

核实日期：2026-10-15。适配器只获取歌单和歌曲元数据，不调用音频、播放授权或账户写入接口。

## 来源与实现边界

- [Playlist Out](https://github.com/LengxiQwQ/playlistout) 使用 [MIT](https://github.com/LengxiQwQ/playlistout/blob/main/LICENSE)。核对了其 QQ、汽水、酷我客户端和酷狗公开分享页字段。
- [GoMusic](https://github.com/Bistutu/GoMusic) 的 [LICENSE](https://github.com/Bistutu/GoMusic/blob/main/LICENSE) 为 MIT。
- [PlaylistAggregatorLite](https://github.com/BabaBoyGegeBoy/PlaylistAggregatorLite) 声明 AGPL-3.0，并依赖同许可的 [music-lib](https://github.com/guohuiyuan/music-lib)。本项目仅核对公开接口地址和字段事实，未引入其依赖或复制实现。
- 本模块独立实现分页、规范化和数量对账；没有调用以上项目的公共代理服务，没有执行网页中的 JavaScript。

## 官方接口与分页

| 平台 | HTTPS 接口 | 分页和完整性依据 |
|---|---|---|
| QQ 音乐 | `c.y.qq.com/qzone/fcg-bin/fcg_ucc_getcdinfo_byids_cp.fcg`；备用 `u.y.qq.com/cgi-bin/musicu.fcg` 的 `music.srfDissInfo.DissInfo/CgiGetDiss` | `song_begin` 绝对偏移、`song_num`；与 `songnum` 等总数字段对账 |
| 汽水音乐 | `beta-luna.douyin.com/luna/playlist/detail` | POST 歌单 ID、`cursor`、`count`；`next_cursor`、`has_more` 和 `playlist.count_tracks` |
| 酷我音乐 | `m.kuwo.cn/newh5app/wapi/api/www/playlist/playListInfo` | `pid`、从 1 开始的 `pn`、`rn`；`data.total` 与 `musicList` |
| 咪咕音乐 | `app.c.nf.migu.cn/MIGUM3.0/resource/playlist/song/v2.0` | `playlistId`、`pageNo`、`pageSize`；`data.totalCount` 与 `songList` |
| 酷狗音乐 | `pubsongscdn.kugou.com/v2/get_other_list_file`，`mobiles.kugou.com/v5/special/info_v2`；专题接口与官方分享页作为补充 | 匿名公开合集 `global_collection_id/page/pagesize/count`；H5 可提取公开合集 ID；仅有预览时保留部分结果 |

具体参考文件：[QQ 客户端](https://github.com/LengxiQwQ/playlistout/blob/main/worker/src/providers/qqmusic/client.ts)、[汽水客户端](https://github.com/LengxiQwQ/playlistout/blob/main/cli/qishui/qishui_playlist_export.py)、[酷我客户端](https://github.com/LengxiQwQ/playlistout/blob/main/worker/src/providers/kuwo/client.ts)、[酷狗导出脚本](https://github.com/LengxiQwQ/playlistout/blob/main/cli/kugou/kugou_playlist_export.py)、[咪咕公开接口字段](https://github.com/guohuiyuan/music-lib/blob/main/migu/playlist.go)。

## 本次匿名真实验证

这些是公开样例的即时结果，不代表所有地区、登录状态或私密歌单都可读取。

| 平台与公开样例 | 本次结果 |
|---|---|
| [QQ 歌单 9044196528](https://y.qq.com/n/ryqq/playlist/9044196528) | 声明 637 首，读取 637 首；其中 7 首保留元数据缺项标记 |
| [汽水歌单 7649633511712636971](https://music.douyin.com/qishui/share/playlist?playlist_id=7649633511712636971) | 声明 313 首，游标分页读取 313 首 |
| [酷我歌单 1082685104](https://www.kuwo.cn/playlist_detail/1082685104) | 从酷我公开首页取得样例；声明 484 首，分页读取 484 首 |
| [咪咕歌单 182310879](https://music.migu.cn/v3/music/playlist/182310879) | 声明 13 首，读取 13 首；1 首保留缺项标记 |
| [酷狗公开合集](https://www.kugou.com/yy/special/single/collection_3_569038616_3_0.html) | 匿名官方合集接口声明 2 首、读取 2 首；1 首保留缺项标记 |
| [酷狗公开合集 62 首](https://www.kugou.com/yy/special/single/collection_3_1504683618_2_0.html) | 匿名官方合集接口声明 62 首、读取 62 首 |

酷狗旧专题 HTTPS 接口在本次网络中连接失败，移动端备用页返回重定向；程序保留明确的访问失败状态，不降级到明文 HTTP。公开合集优先使用官方匿名 H5 元数据请求，仅使用公开客户端协议签名材料，`uid=0`、账户 Token 为空；这不代表用户私钥或登录凭据。有关协议参考的版权和完整许可保存在 [PlaylistOut-LICENSE.txt](PlaylistOut-LICENSE.txt)。没有内置账户扫码登录或真实用户 Token，不尝试读取私密歌单。公开分享页存在合集 ID 时，会尝试该公开合集；上游不允许读取时保留实际预览并标记部分结果，或由用户导入合法导出文件。

## 完整性与凭据

只有声明总数与返回条目数完全一致，且没有页重复、游标停滞、歌单变化或缺失平台 ID，才确认清单完整。缺少专辑、艺人或时长的曲目仍保留，标记 `metadata_complete=false`；清单完整与元数据齐全是两个维度。

请求限制为 HTTPS 官方域名白名单、每次最多 20 MB、每单最多 50,000 首以及有限分页。每页都有可取消检查点。网络失败、限流、登录要求和结构改变会返回明确状态；已经读取的条目不会丢弃或伪造补齐。

Cookie 仅从本机凭据库读取；同一引用可绑定本平台明确的接口域名列表，每次请求再按 Cookie 的 domain、hostOnly、path 和过期时间筛选。Cookie 不发送给第三方解析服务，不跨域自动跟随重定向，也不写入公开报告。
