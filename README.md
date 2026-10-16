<div align="center">
  <img src="src/musicops/assets/musicops.png" width="112" alt="MusicOps 图标">
  <h1>MusicOps</h1>
  <p><strong>从主流平台歌单到自己的音乐库，让下载、整理与升级连起来。</strong></p>
  <p>中文桌面应用 · 十个平台歌单 · 资料补全 · 音质升级</p>
  <p>
    <a href="https://github.com/wsdbb72/MusicOps/releases/latest"><img src="https://img.shields.io/github/v/release/wsdbb72/MusicOps?label=%E6%9C%80%E6%96%B0%E7%89%88%E6%9C%AC&amp;color=16857d" alt="最新版本"></a>
    <a href="https://github.com/wsdbb72/MusicOps/actions/workflows/windows-tests.yml"><img src="https://github.com/wsdbb72/MusicOps/actions/workflows/windows-tests.yml/badge.svg" alt="Windows 测试"></a>
    <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue" alt="MIT 许可证"></a>
    <img src="https://img.shields.io/badge/Windows-10%20%2F%2011-0078D4" alt="Windows 10 / 11">
    <img src="https://img.shields.io/badge/macOS-15%2B-555555" alt="macOS 15 及以上">
  </p>
  <p>
    <a href="https://github.com/wsdbb72/MusicOps/releases/latest"><strong>下载桌面版本</strong></a> ·
    <a href="docs/用户操作说明.md">配置与操作指南</a> ·
    <a href="docs/运维与恢复说明.md">备份与恢复</a> ·
    <a href="https://github.com/wsdbb72/MusicOps/issues">反馈问题</a>
  </p>
</div>

![MusicOps 工作台](docs/images/workbench.png)

*界面截图使用合成音乐资料，不包含私人账号、歌单或运行档案。*

MusicOps 把日常音乐库维护收敛为三个入口：把歌单中缺少的音乐补进库、补齐已有音源的资料，以及寻找并验证更好的音源。任务在独立后台执行，进度和需要人工核对的项目集中显示。

支持网易云、QQ 音乐、汽水、酷狗、酷我、咪咕、bilibili、Apple Music、Spotify、YouTube Music 的歌单链接识别，以及 JSON / CSV / TSV / TXT / M3U8 / XLSX 文件导入。**链接能识别，不代表每个平台都能在未登录时完整读取**；具体入口、授权与完整性边界见下方支持表。

下载来自已配置的 OpenCD、DicMusic、Orpheus、Redacted；可用站点确认没有匹配资源时，歌单入库可回退网易云。完成整理和音频验证后，再把文件交给 Roon / Navidrome 读取。

## 目录

- [先下载并运行](#先下载并运行)
- [歌单平台与文件格式](#歌单平台与文件格式)
- [第一次使用：七步配置](#第一次使用七步配置)
- [三个工作流怎么用](#三个工作流怎么用)
- [手动搜索四站专辑](#手动搜索四站专辑)
- [任务中心与常见问题](#任务中心与常见问题)
- [资料识别与音质判断](#资料识别与音质判断)
- [目录、更新与隐私](#目录更新与隐私)
- [开发、验证与贡献](#开发验证与贡献)

## 先下载并运行

在 [最新 Release](https://github.com/wsdbb72/MusicOps/releases/latest) 的 **Assets** 中选择。以实际上传的资产和构建结果为准：

| 文件 | 用途 | 使用方式 |
| --- | --- | --- |
| `MusicOps-版本号-Setup.exe` | 推荐，常规安装 | 运行安装包，按提示安装，再打开 MusicOps |
| `MusicOps-版本号-Windows.zip` | 绿色文件夹版 | 完整解压到可写目录，双击 `MusicOps.exe` |
| `MusicOps-版本号-macOS-arm64.pkg` | Apple Silicon Mac，推荐 | 双击运行安装向导，安装到“应用程序” |
| `MusicOps-版本号-macOS-x86_64.pkg` | Intel Mac，推荐 | 双击运行安装向导，安装到“应用程序” |
| `MusicOps-版本号-macOS-芯片.zip` | Mac 手动安装版 | 解压，将 `MusicOps.app` 拖入“应用程序” |
| `MusicOps-版本号-source.zip` | 开发用源码 | 普通使用无需下载 |
| `SHA256SUMS.txt` | 下载完整性核对 | 对照同一 Release 中对应文件的 SHA256 |

Windows 运行环境为 **Windows 10 / 11 64 位**；macOS 构建目标为 **macOS 15 及以上**，分 Apple Silicon `arm64` 与 Intel `x86_64`，不承诺兼容旧系统。桌面发行包无需安装 Python。Windows 绿色版需要保留整个文件夹，尤其是 `_internal`，不能只取出一个 EXE。

macOS 的 `.pkg` 是原生安装向导，安装到 `/Applications/MusicOps.app`，无需手动拖放。升级前在“任务中心”点击 **退出应用及后台**；运行中的任务会阻止退出和安装替换。旧版没有此按钮时，确认任务已停止后，在“活动监视器”退出 MusicOps 和 MusicOps-worker 再安装。个人资料和钥匙串凭据保留；安装器不会以管理员身份启动应用。

Mac 安装包和 ZIP 附带对应 `.sha256`、`.sha512` 和 `-build.json`，由 GitHub Actions 在对应架构机器构建并实际安装、重复安装及运行验收。当前 `.app` 仅有 ad-hoc 完整性签名，`.pkg` 未使用 Developer ID Installer 签名，也没有 Apple 公证，Gatekeeper 可能拦截。确认下载来源与校验值后，可按系统提示在 **系统设置 → 隐私与安全** 中选择“仍要打开”；不要关闭系统安全功能。[Apple 官方说明](https://support.apple.com/en-us/102445)。CI 验收不代表所有私人音乐服务均已现场实测。

完整音频验证需要 **FFmpeg 和 ffprobe**。公开包不附带这两个程序，可从 [FFmpeg 官方下载页](https://ffmpeg.org/download.html) 获取适合自己系统的构建，解压后在向导中分别选择 `ffmpeg`、`ffprobe` 可执行程序（Windows 对应 `.exe`）。已有工具可以继续使用。

首次打开会进入设置向导；没有的账号或服务可以跳过，以后点击 **连接与设置** 补充。

<details>
<summary>想校验下载的安装包？</summary>

在下载目录打开 PowerShell，运行：

```powershell
Get-FileHash -LiteralPath .\MusicOps-版本号-Setup.exe -Algorithm SHA256
```

将结果与同一 Release 的 `SHA256SUMS.txt` 对照。把命令中的“版本号”替换为实际文件名。

</details>

## 歌单平台与文件格式

### 在线解析支持表

下表描述当前读取能力。只有来源给出的总数、实际条目与完整分页相互吻合时，才将清单标为完整；预览、受限、失效条目或缺失资料会保留明确提示。

| 平台 | 可以输入 | 不填授权时 | 可选授权 / 完整读取条件 |
| --- | --- | --- | --- |
| 网易云音乐 | 歌单链接、短链、歌单 ID | 读取公开清单，分批获取详情并对账 | 需要账号访问时导入网易云 Cookie JSON |
| QQ 音乐 | 歌单链接、分享短链、选定平台后的 ID | 公开歌单分页读取 | 可选匹配官方接口域名的 Cookie JSON |
| 汽水音乐 | 歌单链接、分享文案、选定平台后的 ID | 公开歌单分页读取 | 可选对应接口 Cookie JSON |
| 酷狗音乐 | 歌单 / 分享链接、选定平台后的 ID | 尝试公开歌单、分享页与可用分页 | 部分个人云歌单或 H5 仅有预览；需要授权或改用导出文件 |
| 酷我音乐 | 歌单链接、选定平台后的 ID | 公开歌单分页读取 | 可选对应接口 Cookie JSON |
| 咪咕音乐 | 歌单链接、分享链接、选定平台后的 ID | 公开歌单分页读取 | 可选对应接口 Cookie JSON |
| bilibili | 收藏夹 / 合集等受支持的列表链接 | 读取可见视频元数据；可能受平台访问限制 | 可选 Cookie JSON；UP 主、视频标题不自动当作音乐艺人和曲名 |
| Apple Music | 带地区与 `pl.…` 的共享歌单链接 | 尝试公开网页中可核实的曲目与总数 | Developer Token 可走官方 Catalog 分页；私人资料库歌单不在当前范围 |
| Spotify | 公开歌单链接、选定平台后的 ID | 公开预览，始终按可能不完整处理 | Access Token；当前官方 Items API 限本人拥有 / 协作歌单 |
| YouTube / YouTube Music | 含 `list=` 的歌单链接 | 尝试公开页面及可验证的续页，不能保证完整 | Data API Key 或 OAuth Access Token 可用官方分页；视频可能没有音乐专辑资料 |

在线平台能力与**现场验证**分开记录：本轮开发对 QQ、汽水、酷狗、酷我、咪咕的公开样本完成了条目数对账，Apple Music 的一个公开歌单通过实际适配器读取 **50 / 50 首、资料缺失 0**；Spotify / YouTube、平台授权接口及受访问限制的列表，不据合成测试宣称全部账号实测通过。更详细的 [国际平台边界、官方 API 与参考项目](docs/references/global-playlists.md) 可供排查和开发时查阅。

### 文件导入：所有平台的共同兜底

没有可用授权、网页受限或只返回预览时，可以在原平台或兼容导出工具中导出歌单，再点击 MusicOps 的 **导入文件**。这一步读取歌单元数据，不读取文件里列出的音频，也不运行导出脚本。

| 格式 | 支持的结构 | 注意 |
| --- | --- | --- |
| JSON | Playlist Out Canonical JSON、歌曲对象数组、常见网易云 / QQ / Spotify 导出结构 | 推荐；尽量保留平台、原歌曲 ID、艺人、专辑与时长 |
| CSV / TSV | 带列名表格；中英文歌名、歌手、专辑、时长等常见列 | 按列名读取，不按列位置猜测；兼容常见 Exportify / YouTube 表格列 |
| TXT | 每行“歌名 - 歌手”或“歌手 - 歌名”，可有第三段专辑 | 导入时明确选择顺序；歧义多时改用 JSON / CSV |
| M3U / M3U8 | 带 `#EXTINF` 的扩展列表 | 只用条目元数据，不访问后面的音源路径或 URL |
| XLSX | 第一张曲目表或 `Tracks`；可读取 `Playlist Info` | 只读普通表格，不执行公式、宏或外部数据连接；带公式时另存为纯值 CSV |

单文件最多 **20 MB、50,000 首**；文本支持 UTF-8 / BOM，必要时明确提示按 GB18030 解码。缺少专辑、艺人或时长会保持缺失并等待核对，不生成“未知专辑”等假资料。文件中的清单数量与上游完整歌单是否一致，仍取决于导出文件本身；部分导出标记和数量不符会保留。

## 第一次使用：七步配置

**最小配置：音乐库 + FFmpeg / ffprobe。** 资料补全可以先用；需要从站点下载时，再添加至少一个站点的 Cookie 和 qBittorrent。歌词、Discogs、AI、Navidrome 都是可选项。

### 1. 选择音乐库

选择存放**整理完成的音乐**的目录。Roon / Navidrome 应读取这个目录；MusicOps 自动管理下载和整理工作区。

使用 Roon 时，在 **Roon → 设置 → 存储** 中添加相同音乐库目录。不要把下载、备份、隔离目录一起设成监听目录，否则未整理或旧版文件也可能被读入。

### 2. 导入资源站 Cookie

| 站点 | 站点地址 | MusicOps 的处理方式 |
| --- | --- | --- |
| OpenCD | `www.open.cd` | 使用登录 Cookie 搜索并直接获取种子 |
| DicMusic | `dicmusic.com` | 搜索，按实际免费状态或 FL 按钮处理 |
| Orpheus | `orpheus.network` | 同上，使用 Gazelle 适配层 |
| Redacted / RED | `redacted.sh` | 复用 Gazelle 适配层；没有账号可关闭 |

每个有账号的站点分别操作：

1. 在浏览器中登录该站点，确认当前页面能正常访问。
2. 使用 Cookie-Editor 导出当前站点的 **JSON**。
3. 在向导中找到相同站点，点击 **导入 JSON 文件**，或粘贴完整 JSON。
4. 勾选 **参与检索**，保存设置。

JSON 应为 Cookie 对象组成的数组；无需手工挑选或重写字段。登录名和密码、浏览器地址、`Cookie: a=b` 文本都不是这里需要的 JSON。Cookie 过期后重新导出并覆盖即可。

已保存的凭据以点号 / 星号及保存状态提示表示，**不会回显真实 Cookie 或密钥**。保留掩码不改动即可继续使用原值；输入或导入新内容后才替换。详情见 [凭据填写说明](docs/用户操作说明.md#凭据已保存时如何修改)。

### 3. 连接 qBittorrent，设置 FL

在 qBittorrent 的 **选项 → WebUI** 中启用 Web 用户界面，填写：

| 输入项 | 填什么 |
| --- | --- |
| WebUI 地址 | MusicOps 能访问的完整地址，例如 `http://127.0.0.1:8080` |
| 用户名 | WebUI 的登录用户名 |
| 密码 | WebUI 的登录密码，与站点 Cookie 无关 |

先在浏览器打开该地址并尝试登录，便于确认地址、端口和账号。远程连接使用 HTTPS；本机连接可以使用上面的回环地址。

**“最多使用 FL 免费下载”控制 DicMusic / Orpheus / RED 本批任务最多发起几次 FL 使用：**

- 填 `0`：不消耗 FL 次数，只采用已经免费的可用资源。
- 填 `X`：本批最多使用 `X` 次，达到上限后相应任务等待处理。
- 勾选 **全部**：次数输入框变灰，本批忽略填写的次数上限。

“全部”仍受站点实际余额、权限与免费状态核验约束。这三个站点只走已经免费或 FL 生效的下载路径；请求结果未知时保留待核对状态，避免重复使用 FL。OpenCD 使用直接获取种子的流程，不消耗这里的 FL 额度。

MusicOps 为自己的下载设置分类、任务标记和保存目录，不接管无关下载或更改全局下载偏好。**做种文件保持原样**，整理在工作副本中进行。

### 4. 设置网易云回退和媒体工具

歌单入库时，已启用的站点**均完成检索且没有匹配资源**，才进入网易云回退。站点登录失败、限流、响应不完整或网络失败会先等待处理，不会当作“没有资源”。

| 配置 | 何时需要 |
| --- | --- |
| 启用网易云回退 | 希望找不到站点资源时继续尝试网易云 |
| 网易云登录态 | 可选，导入网易云当前站点的 Cookie-Editor JSON |
| API 服务 | 可选，填写自己的兼容服务地址；需要提供 `/song/url` 格式的响应 |
| 客户端下载目录 | 可选，接入已有网易云客户端下载的音频或 NCM |
| `ffmpeg` / `ffprobe` 程序 | 分别选择对应 EXE，用于完整解码验证与媒体信息检查 |

不填 API 时会尝试网易云公开可用的完整音源。使用自己的 API 服务时，相关账号权限由该服务提供；MusicOps 不把网易云 Cookie 自动发往自定义 API。无法完整获取或只有试听的曲目会保留原因。

接入客户端下载目录时，程序按歌曲、艺人、专辑资料匹配文件。匹配的 NCM 会复制到工作区后解密，原文件保留；多个版本无法唯一确定时需要先核对。

### 5. 歌单平台授权（可跳过）

这一页配置的是**读取歌单的登录信息**，与前面的 PT 站点下载 Cookie、qBittorrent 密码各自独立。网易云登录态仍在“网易云回退与媒体工具”页填写；本页可选其余九个平台。

| 平台 | 怎么填写 |
| --- | --- |
| QQ、汽水、酷狗、酷我、咪咕 | 对应平台 Cookie-Editor JSON，只有匹配读取接口域名的 Cookie 才会使用 |
| bilibili | Cookie-Editor JSON 数组，或含 `cookies` 数组的 JSON |
| Spotify | Access Token 原文，或 `{"access_token":"…","market":"US"}` |
| Apple Music | Developer Token / JWT 原文，或 `{"developer_token":"…"}` |
| YouTube | API Key 原文或 `{"api_key":"…"}`；OAuth 可填 `{"access_token":"…"}` |

没有授权时保持空白，先尝试公开读取或文件导入。向导不会代你完成平台 OAuth 授权或自动刷新过期 Token；需要时自行获取有效凭据并重新填写。Spotify、Apple、YouTube 的平台 Token 只发送给对应官方 API，不发送到公开预览网页。

### 6. 设置歌词与资料服务

| 服务 | 需要填写 | 不配置时 |
| --- | --- | --- |
| 歌词接口 | 服务地址 + 服务提供的完整 `Authorization` 原文 | 可继续使用已有歌词及可确定歌曲 ID 对应的网易云歌词 |
| Discogs | 个人 Token | 跳过需要该 Token 的结构化查询 |
| AI | 兼容 Responses 的服务地址、模型名称、API Key，并勾选启用 | 所有核心工作流仍可使用 |

歌词鉴权填服务给出的原文，**不要自行添加 `Bearer`**；如果服务提供的原文本身带有此前缀，则完整保留。无需填写内部的 `vault://…` 凭据引用。

匹配并校验通过的同步歌词保存为音频同名 `.lrc`，例如 `01 - Song.flac` 对应 `01 - Song.lrc`。确实暂无歌词的曲目可以继续处理；鉴权失败会单独提示，便于修正。AI 只辅助解释问题，不负责确认发行身份或决定删除音源。

### 7. 连接 Navidrome（可跳过）

填写 Navidrome 服务地址、用户名和密码，并勾选启用。该账号需要有扫描权限，服务器需能读取所选音乐库中的同一批文件。部署在另一台机器或容器内时，要确认目录映射实际指向这些文件。

MusicOps 入库完成后请求 Navidrome 扫描；Roon 通过自己的存储监听发现文件。文件入库、扫描请求成功与客户端最终显示属于不同步骤，可以在相应客户端搜索和播放确认。

## 三个工作流怎么用

### 歌单入库：把缺少的音乐带进库

```text
平台歌单 / 导出文件 → 选择曲目 → 检查现有音乐库 → 按专辑创建任务
          → 已配置站点检索 → qBittorrent 下载
             └─ 全部无匹配资源 → 网易云回退 / 接入客户端下载
          → 标签、封面与 .lrc → 完整音频验证 → 入库 → 音乐服务器读取
```

1. 打开 **歌单入库**，选择“自动识别平台”并粘贴歌单链接 / 分享文本，点击 **解析歌单**；也可点击 **导入文件**。输入纯 ID 时先选择对应平台。
2. 检查解析结果，勾选需要的曲目，也可使用 **全选 / 取消**。
3. 点击 **开始入库**，到 **任务中心** 查看扫描、建任务及后续专辑处理进度。
4. 遇到 **需要核对**，选中任务并点击 **核对 / 重试选中任务**，根据提示处理。

已在库中且身份、专辑及已知时长匹配的曲目会跳过。同一专辑的需求合并处理；站点下载通常按整张发行获取，不保证只下载所选单曲。解析清单不完整时，界面会保留警告，只处理你明确选择的可见曲目。

不同平台的歌曲 ID 相互隔离，`spotify:123` 不会被当成 `netease:123`。跨平台回退网易云时，必须找到**唯一且歌名、艺人、专辑、完整时长均匹配**的曲目；缺少其中任一项或存在多个候选时等待核对，不直接套用原平台的数字 ID 下载。

### 资料补全：维护已有音乐

打开 **资料补全**，点击 **检查并自动补全**。程序检查歌曲、艺人、专辑、专辑艺人、封面、同步歌词，以及多碟曲序资料；可靠资料会自动补齐，不确定的项目等待核对。

近期没有查到资料的结果会缓存，避免反复请求。若刚修改服务配置或确认上游已有新资料，可点 **重新查询暂无资料**。

### 音质升级：新版验证通过，再移出旧版

打开 **音质升级**，选择是否优先无损 24 bit，再点击 **检查并自动升级**。

程序先根据候选媒体信息筛选，下载后检查**实际编码、位深、采样率、声道、曲目与完整时长**。新版完整解码、资料和身份检查通过并成功入库后，才把旧版移到音乐库外的恢复目录，监听目录中保留新版。

缺少同一发行的 ID 时，需要确认录音、混音和版本是否一致。不同声道、无法确定的版本或没有实际音质提升的候选不会自动替换。恢复副本保留，下载与做种源不改动。

## 手动搜索四站专辑

打开 **专辑搜索**，输入歌手 / 专辑名称，选择一种方式：

| 方式 | 适合什么情况 | 行为 |
| --- | --- | --- |
| 原样搜索 | 已知道站点常用关键词，或想宽泛查看结果 | 按输入的搜索文本检索已启用站点 |
| 识别专辑后精准搜索 | 希望先把艺人与专辑名称核对清楚 | 查询 MusicBrainz / Discogs，选择确认的发行，再用标准艺人 + 专辑名检索四站并过滤不匹配名称 |

多个发行版本先由你选择，不默认把重制、现场、豪华版当成同一版。结果列表展示来源站点、音质信息与做种数量，每个站点保留独立搜索状态。列表展示接口当前返回页，更多资源可打开站点详情继续查看。此页面负责**搜索与查看详情**，不会因点击搜索就自动添加下载任务。

<details>
<summary>查看专辑搜索界面</summary>

![手动专辑搜索](docs/images/album-search.png)

</details>

## 任务中心与常见问题

任务中心显示批次准备和专辑处理状态，支持查看详情、暂停、继续、取消、核对及恢复原版。**关闭窗口后，后台仍可继续工作**；下次打开同一运行档案可以继续查看。

<details>
<summary>查看任务中心界面</summary>

![任务中心](docs/images/tasks.png)

</details>

| 看到的状态 / 现象 | 代表什么 | 建议操作 |
| --- | --- | --- |
| 批次准备 / 扫描音乐库 | 正在读取现有库并合并需求，显示已检查数量和当前阶段 | 查看进度；避免反复点击开始；需要时可取消 |
| 等待处理 | 已进入后台队列 | 检查是否暂停，必要时点“继续” |
| 下载中 | 等待 qBittorrent 完成整张发行 | 在 qBittorrent 检查连接、磁盘和下载进度 |
| 需要核对资料、曲序或版本 | 证据不足以自动确定 | 选中任务，点“核对 / 重试选中任务” |
| 歌词服务未通过 | 可能是地址、鉴权或网络问题 | 修正连接设置后重试，或明确跳过本次歌词服务 |
| FL 结果未知 / 请求超时 | 无法确认请求是否已经生效 | 先在站点核对；不要反复点击以免重复使用 FL |
| 部分站点未完成检索 | 尚不能确定没有资源 | 更新该站 Cookie 或处理限流；不用的站点可关闭 |
| 旧版准备任务已停止，请重新开始本批次 | 旧版曾排队的请求保留为核对记录，避免升级后重复执行 | 返回对应工作流，检查选择与设置后重新开始 |
| 旧版专辑队列已暂停 | 旧版扫描生成的专辑没有可核对的批次记录，升级后等待人工确认 | 核对后继续对应专辑；重复点击产生的旧队列可返回工作流重新开始 |
| 没有新专辑任务 | 可能全部已入库、资料近期检查过，或批次创建失败 | 查看本次准备任务结果和具体错误；不要删除数据库重试 |
| 新版没有实际音质提升 | 实际媒体检查不满足升级条件 | 当前音源保留，无需处理 |
| 已入库，但客户端没有显示 | 音乐服务器尚未发现或无法读取文件 | 核对监听目录、权限、目录映射和扫描状态 |

遇到界面长时间停在一个提示时，先查看任务详情和后台连接状态。暂停、取消在安全检查点生效，正在进行的单个网络或文件步骤可能需要先结束。**取消不删除已完成的发布，也不停止做种。** 不要通过删除 `profile`、手工改数据库或再次批量启动来解除等待。

需要回退已完成的替换时，选中任务点击 **恢复选中任务的原版**。恢复前会重新检查文件，后来被其他软件改动的内容会阻止覆盖。详见 [备份与恢复指南](docs/运维与恢复说明.md)。

## 资料识别与音质判断

| 来源 | 当前用途 |
| --- | --- |
| MusicBrainz | 结构化歌曲、发行与标识资料查询 |
| Discogs | 使用个人 Token 的结构化查询 |
| AllMusic、Rate Your Music、Album of the Year、VGMdb | 在核对界面提供官网检索入口，供人工查证 |
| 网易云 | 歌单、明确歌曲 / 专辑 ID 对应的资料与可用歌词等 |

封面优先使用已有可靠资源或确定专辑身份对应的图片；仍无法确定时，可选择已核对的本地封面，或明确跳过。网站搜索命中和同名专辑本身不能证明发行相同，四个官网查证入口也不等于已经实现自动抓取与写入。

音质升级比较媒体参数，同时检查身份和完整性。**无损压缩码率更高、文件更大，不能单独证明音质更好；24 bit 标记也不能证明母带或听感一定更好。** 程序不会用这些信号代替录音和发行核对。

## 目录、更新与隐私

应用文件与私人运行档案分开管理。典型 Windows 安装目录如下；媒体工具可以放在其他位置，再在向导中指定。

```text
MusicOps/
├─ MusicOps.exe               # 桌面应用
├─ MusicOps-worker.exe        # 独立后台
├─ _internal/                 # 运行依赖，必须保留
├─ licenses/                  # 依赖许可证
├─ LICENSE
├─ THIRD-PARTY-NOTICES.md
├─ 启动说明.txt
└─ profile/                   # 私人配置、状态、凭据及恢复数据
```

| 目录 / 数据 | 作用 | 更新时怎么处理 |
| --- | --- | --- |
| `profile/state` | SQLite 中的配置、任务和操作记录 | 保留 |
| `profile/vault` | 系统账户保护的凭据与连接信息 | 保留，不公开上传 |
| `profile/source` | 下载源与接入文件 | 可能关联做种，不盲目清空 |
| `profile/work`、库外同卷工作区 | 整理和验证副本 | 有任务运行时保留 |
| `profile/backup`、`profile/quarantine` | 原文件与恢复内容 | 确认不再需要恢复后再清理 |
| 所选音乐库 | 供 Roon / Navidrome 读取的成品 | 不放运行档案或下载临时文件 |

安装版快捷方式指向该安装目录的 `profile`。绿色版没有指定档案时会使用程序选择的现有档案或本地应用数据目录；需要明确使用随包档案时，可从 PowerShell 运行 `.\MusicOps.exe --base .\profile`。同一个档案的 GUI 与后台必须一致。

Windows 使用当前账户 DPAPI 保护秘密值；macOS 将主密钥保存在登录钥匙串，本地秘密文件使用 AES-GCM 加密，没有明文存储回退。macOS 默认档案为 `~/Library/Application Support/MusicOps/profile`，不放在 `MusicOps.app` 内部。

更新前保留完整私人档案与原版本，在“任务中心”选择“退出应用及后台”；正在执行的任务需等待完成，或取消后再退出。关闭窗口不代表后台已经退出。Windows 安装器保留私人 `profile`，卸载也不会主动删除它；macOS 安装器整体替换应用，保留用户目录中的档案，卸载时将“应用程序”中的 MusicOps 移到废纸篓即可。更换系统账户或机器时，原凭据可能无法解密，应重新填写。

凭据与目标域名绑定；服务主机名改变后，原凭据不会自动转发到新域名，需要重新配置。公开仓库和发行包不携带私人 Cookie、密钥、数据库或音乐。提交反馈时请只提供**脱敏的错误文本、版本和复现步骤**，不要附整个 `profile`、原始鉴权文件或含下载密钥的链接。

## 开发、验证与贡献

源码开发需要 Python **3.12+**。Windows 在仓库根目录运行：

```powershell
py -3.12 -m venv .venv
.\.venv\Scripts\python.exe -m pip install -r requirements.lock
.\.venv\Scripts\python.exe -m pip install --no-deps -e .
.\.venv\Scripts\musicops-gui.exe --base .\profile
```

运行测试与构建文件夹包：

```powershell
.\.venv\Scripts\python.exe -m pytest -q
pwsh -File packaging/build_windows.ps1 -Python .\.venv\Scripts\python.exe
```

构建产物位于 `dist/MusicOps-Windows`；安装器脚本为 [`packaging/MusicOps.iss`](packaging/MusicOps.iss)，使用 Inno Setup 7 编译。公开发布前还需收集许可证、核对打包清单、运行 EXE 验证并检查敏感信息。FFmpeg 的本地测试安装与其二进制再分发是两件事，自行加入发行包时需履行对应许可。

CI 使用锁定依赖、临时目录、合成音频和模拟服务验证行为。macOS 运行包由 GitHub Actions 在对应架构机器构建，构建与发布结果以具体 Release 和 Actions 为准。测试通过说明覆盖场景通过，不代表所有私人站点账号、真实资源和音乐服务器均已现场验收；未配置的站点不会被当作已验证连接。

欢迎通过 [Issue](https://github.com/wsdbb72/MusicOps/issues) 反馈可复现问题，或提交聚焦一个问题的 Pull Request。请写明版本、操作入口、预期与实际结果，附脱敏提示；提交代码时补上与行为相关的回归验证。适配器变更应覆盖鉴权失败、限流、超时、部分响应、取消与身份冲突。

更多说明：[用户操作](docs/用户操作说明.md) · [运行档案与恢复](docs/运维与恢复说明.md) · [开发与验证](docs/开发与验证.md) · [国际平台与参考项目](docs/references/global-playlists.md)。

感谢 [Playlist Out](https://github.com/LengxiQwQ/playlistout)、[PlaylistAggregatorLite](https://github.com/BabaBoyGegeBoy/PlaylistAggregatorLite)、[GoMusic](https://github.com/Bistutu/GoMusic)、[Spotifylist-to-gkl](https://github.com/zhangolve/Spotifylist-to-gkl)、[XueshiQiao 的网易云导出示例](https://gist.github.com/XueshiQiao/858974dbf86bc91030fbf49f9c6daf43) 与 [superjodash 的 YouTube 表格示例](https://gist.github.com/superjodash/6f82f50418576dd982b48ae33cf962f2) 提供的协议、格式与思路参考。具体参考范围与许可见 [参考记录](docs/references/global-playlists.md#参考项目与许可证)。

源码使用 [MIT 许可证](LICENSE)。第三方依赖见 [许可说明](packaging/THIRD-PARTY-NOTICES.md)。
