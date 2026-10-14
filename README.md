# MusicOps 0.2.0 稳定联网版

中文 Windows 桌面音乐库管理工具。使用独立后台任务、SQLite 状态和可复核的执行计划，连接音乐目录、网易云歌单、音乐资料网站、歌词服务、PT 站点和 qBittorrent。

在线工作台支持真实服务预检、歌单详情、音乐目录的只读标签索引、资源检索和歌曲／专辑身份候选。MusicBrainz、Discogs 提供两条结构化 API 查询适配器；Discogs 需要使用者自行配置 Token。AllMusic、Rate Your Music、Album of the Year、VGMdb 提供四个官网人工查证入口。服务限流、鉴权或网络异常会保留相应失败状态。名称、艺人、发行版本和时长仍需交叉核对；网站候选不会直接改写文件或代替完整音频验证。

在线模式是新档案的默认工作方式。离线演示使用合成音频，供开发和流程验证。真实下载、免费令牌操作、音乐文件发布／隔离以及 Roon 客户端验收应在完成相应预检和确认具体计划后执行；当前在线界面只开放已实现的服务能力。

## 安装与启动

需要 Windows 和 Python 3.12 或以上版本。

```powershell
py -3.12 -m venv .venv
.\.venv\Scripts\python.exe -m pip install -r requirements.lock
.\.venv\Scripts\python.exe -m pip install -e .
.\.venv\Scripts\musicops-gui.exe
```

也可以通过 `--base` 指定独立的本地运行档案。首次启动后，在设置中选择自己的音乐目录和服务地址，再导入或填写对应凭据。应用不会自动下载曲目或修改音乐文件。

设置页支持填写自己的服务地址、音乐目录和媒体工具路径，并管理 PT／网易云 Cookie、qBittorrent 用户名和密码、AI 密钥、歌词鉴权及 Discogs Token。公开示例不会包含使用者的秘密值。

PT Cookie 使用站点导入按钮；可选的网易云登录态使用“自定义凭据”保存 Cookie-Editor JSON，再在网易云来源设置中填写对应引用。公开歌单元数据通常可以直接查询。

```powershell
.\.venv\Scripts\musicops-gui.exe --base .\profile
```

凭据由当前 Windows 账户的 DPAPI 加密，并与目标域名绑定。公开仓库不包含凭据、Cookie、个人歌单快照、运行数据库或私有服务地址。运行档案不能直接复制给另一台机器或另一位 Windows 用户作为共享凭据。

## 开发与验证

```powershell
.\.venv\Scripts\python.exe -m pytest -q
```

测试默认使用临时目录、虚拟服务和合成媒体。在线音乐目录索引读取文件属性和有限标签，不计算完整音频哈希或执行整库解码；这类结果保持未完成完整验证的状态。

构建 Windows 文件夹包：

```powershell
pwsh -File packaging/build_windows.ps1 -Python .\.venv\Scripts\python.exe
```

FFmpeg／ffprobe 可由使用者自行安装并在设置中指定。该仓库不分发媒体工具二进制；自行分发含第三方工具的包时，应满足对应许可证和源码义务。

基本联网和标签索引不需要 FFmpeg。完整解码与合成媒体测试需要使用者提供工具路径。Roon 预检可核验 Core 连接与历史日志；当前曲目浏览保持 `AUTH_REQUIRED`，没有完成曲目可见性验收。应用没有注册浏览扩展，不要求用户启用不存在的扩展；连接成功或日志记录不能证明曲目已被收录或播放。

更多说明见 [操作说明](docs/用户操作说明.md)、[运行档案与恢复](docs/运维与恢复说明.md) 和 [开发说明](docs/开发与验证.md)。源码采用 [MIT 许可证](LICENSE)。
