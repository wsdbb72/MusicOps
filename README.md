# MusicOps 0.3.0

中文 Windows 音乐库管理工具，围绕歌单入库、资料补全和音质升级组织操作。独立后台保存任务进度，关闭窗口后可以继续下载和整理。

## 安装与启动

下载 [Windows EXE 安装包](https://github.com/wsdbb72/MusicOps/releases/download/v0.3.0/MusicOps-0.3.0-Setup.exe)，安装后打开 MusicOps。也可以解压 Release 中的 Windows ZIP，直接运行 MusicOps.exe；无需安装 Python。

首次启动会打开可跳过的设置向导。选择音乐库，然后填写已开通的站点或服务。没有账号的服务保持关闭即可；之后通过“连接与设置”重新打开向导。旧凭据留空会保留，保存后无需重新填写。

## 三个工作流

- **歌单入库**：解析网易云歌单，选择曲目，检查音乐库，按专辑去重；搜索 OpenCD、DicMusic、Orpheus、Redacted，交给 qBittorrent 下载。已启用站点均没有匹配资源时，回退内置网易云下载器或接入客户端下载目录；NCM 在工作副本中解密。整理标签、封面、同名同步歌词后，完整解码验证并入库。
- **资料补全**：检查歌曲、艺人、专辑艺人、封面和同名 .lrc，修正完整多碟专辑的碟号及总碟数。可靠证据自动补齐，缺失或冲突的身份资料在任务中心核对。近期暂无资料的结果会缓存；可以点击“重新查询暂无资料”。
- **音质升级**：先比较站点公布的格式、位深、采样率和有效码率，再验证实际下载音频。新版曲目、完整时长、音质和发行身份通过后，旧库副本移到音乐库外的恢复目录，只保留新版供音乐服务器读取。做种源保持原样。

OpenCD 使用登录 Cookie 直接获取种子；Gazelle 站点解析实际 FL 按钮，受每批 FL 预算限制。请求前保存意图，请求后核对免费状态与种子身份。免费状态未知、余额不明或请求超时都不会自动重复消费令牌或启动收费下载。Redacted 使用相同 Gazelle 适配层，未配置账号时不访问。

歌词接口和网易云官方歌曲 ID 可提供同步 .lrc。封面来自已有专辑资源、确定的网易云专辑 ID 或 MusicBrainz 发行 ID；无法确定时允许用户选择已核对封面或明确跳过。歌曲不可完整下载、只有试听或缺少权限时会保留具体状态。

MusicBrainz、Discogs 提供结构化资料查询；Discogs Token 可在向导中填写。AllMusic、Rate Your Music、Album of the Year、VGMdb 提供官网查证入口。相同标题不代表相同录音或发行，无损文件压缩后码率更高也不代表音质更高。

## 连接与凭据

PT 站点导入 Cookie-Editor 导出的 JSON 文件或粘贴 JSON；qBittorrent 填 WebUI 地址、用户名和密码。歌词鉴权填写服务提供的完整 Authorization 原文。可选的 AI 服务填写地址、模型和 API Key；AI 仅帮助解释问题，不决定删除或发行身份。Navidrome 填服务地址及有扫描权限的账号；Roon 在设置 → 存储中监听同一个音乐库目录。

凭据通过当前 Windows 账户的 DPAPI 加密，并绑定到对应域名。仓库、发行包和可导出配置不包含密码、Cookie、个人数据库或私有服务地址。更新和卸载保留私人 profile。

下载及整理均位于监听目录外。整张新发行在同卷工作区完成验证后一次移入音乐库；旧版替换保留日志和恢复副本。Roon 通过监听目录自动发现新文件；Navidrome 可在发布后触发扫描。连接成功或扫描请求成功不能代替客户端曲目可见性验收。

完整音频验证需要 FFmpeg 和 ffprobe；公开发行包不携带这两个工具，可在向导中指定已安装程序。已有本机工具在更新时保留。

## 开发

需要 Python 3.12 或以上：

```powershell
py -3.12 -m venv .venv
.\.venv\Scripts\python.exe -m pip install -r requirements.lock
.\.venv\Scripts\python.exe -m pip install -e .
.\.venv\Scripts\musicops-gui.exe --base .\profile
.\.venv\Scripts\python.exe -m pytest -q
pwsh -File packaging/build_windows.ps1 -Python .\.venv\Scripts\python.exe
```

测试使用临时目录、合成音频和虚拟服务；GUI 中不提供演示档案入口。测试保留旧界面快照仅用于历史回归，不进入运行包。

Inno Setup 7 安装脚本为 packaging/MusicOps.iss，仅打包明确列出的运行文件。构建不包含 profile、音乐、种子缓存或凭据。媒体工具如需自行分发，应满足对应许可与源码义务。

详见 [用户操作说明](docs/用户操作说明.md) 与 [恢复说明](docs/运维与恢复说明.md)。源码采用 [MIT 许可证](LICENSE)。
