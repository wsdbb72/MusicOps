# MusicOps 全量 Go 迁移计划

> 分支：`refactor/go-desktop`（从 `refactor/go-core` @ `453804ed` 切出）。
> 依据：`native/core/ARCHITECTURE.md`（迁移契约）、`AGENTS.md`、三路盘点（Python 桌面端、Go 核心、打包/CI/桌面壳）。
> 铁律（用户最初的 Cursor 提示词）：全 Go、桌面/Web 双端形态、工作流两端真实可用；不许跳过、占位、TODO、模拟替代真实业务；不回归原有功能；不要未实现就标完成；报告只写真实通过数据与真实剩余缺口。

## 1. 目标与既定方向

- 一个 Go 后端、一套业务状态与任务执行路径；桌面和服务器共用 Web 操作界面；Windows 与 macOS 桌面使用同一套页面和同一套业务 API（`ARCHITECTURE.md:3,68`）。
- 正式切换前，Windows、macOS ARM64、Linux amd64 容器都要在各自平台上走完 `desktop-release` 原生验收，**并且安装包里不能再依赖 Python 运行时**（`ARCHITECTURE.md:76`）。
- 全部必需接口和数据核验通过后，切换唯一后端，删除旧 SQL 业务与重复入口；迁移失败保持旧档案和正在运行的正式版（`ARCHITECTURE.md:76` 之后）。
- 三端验收通过后，发布前还要 Muse Wonder（`muse-wonder-1.0`）审核最终候选提交并复审；复审通过前不创建 Release、不打 tag、不推进 `latest`（`ARCHITECTURE.md:70`）。

## 2. 盘点结论摘要

### 2.1 Python 桌面端（`src/` 125 个文件，约 19,185 行；测试 85 个文件、942 个测试）

- GUI：`gui/workbench.py` 1137 行，6 页工作台（歌单入库/资料补全/音质升级/任务中心/歌手·专辑搜索/去重合并）+ 7 页连接向导；另有人工核对、去重、曲库、合辑、扫码登录、更新控制器。
- 业务入口：`app/service.py` 1076 行（唯一入口，读命令直执行、写命令入队）；`app/pipeline.py` 1436 行，场景 playlist/complete/upgrade/fill/expand，阶段 `_snapshotting→_identifying→_searching→_requesting→_downloading→_fallback→_metadata→_enrich→_validating→_publishing→_retiring→_notifying`。
- Worker：独立 Python 进程，`multiprocessing` Listener + HMAC 签名 + `ipc_requests` 幂等/防重放 + 写命令互斥锁 + Windows PowerShell 监督（5 秒巡检）。
- Providers/Clients：四站（DicMusic/open.cd/Orpheus/RED，Gazelle/Nexus）、MusicBrainz/Discogs、网易云、国内外歌单、AI、歌词；qBittorrent 全套、go-music-dl + FlareSolverr 托管、Vault 三平台凭据库（DPAPI/Keychain/AES-GCM）、Plex/Navidrome/Roon。
- 媒体：NCM 解密、mutagen 标签、OpenCC、歌词边车/内嵌；`library/transactions.py` 644 行事务引擎。
- 打包：PyInstaller 单文件夹包，双 EXE 共享依赖；`MusicOps.iss` 把 `_internal/*`（冻结 CPython 运行时，含 Python DLL）+ `tools/services/*` 打进安装器——这就是 0.4.32 安装程序仍解压 Python DLL 的来源。`prepare_engines.py` pin go-music-dl 与 FlareSolverr 源码并捆 Chrome for Testing 155。

### 2.2 Go 核心现状（`native/core/`，22 个包，`go test ./...` 全过，181 个顶层测试）

已真实落地：Pebble 任务/工作流原子账本、四站只读检索分类、令牌预留记账、SQLite 只读迁移、本地音频导入入库、只读为主的单页 WebUI（任务/队列/核对/站点/库/日志 6 视图）、5 个已注册执行器（identify_release、snapshot_album、search_release、process_release、publish_album）。

缺口（按类）：
- **没有执行器**：`prepare_batch`、`repair_album`、真实下载（qBittorrent）、真实扣费、用户库发布/做种迁移、流媒体下载、转码（`transcode` 队列容量 1 但无任何命令映射）、批量暂停/通用重试、浏览器求解器、桌面壳/worker/打包。
- **只实现未接线**：`audiopublish`（完整实现，零调用方）、`downloadpolicy`（失败分类/重试档/流媒体顺序，零调用方）、`releaseidentity`（离线身份解析，零调用方）；`reviewai` 的 `Advise` 完整但生产进程 `reviewModel` 恒为 nil，AI 建议永不触发；`mediatx` 只挂在 RPC 侧，`cmd/musicops` 侧不可用。
- **HTTP 层缺失**：任务暂停/继续/取消/重试、核对确认、凭据设置、下载/流媒体/转码操作、歌单/专辑管理、批量操作。仓储层 `Control` 存在但 HTTP 不可达。
- **只有 fixture/测试协议**：令牌扣费只认 `musicops.fixture.v1`（无经核对的线上逐笔扣费格式）；合成检索/种子/文件发布；`production_verified` 全仓恒假。
- **文档-only**：Muse Wonder 终审、三端正式发布门（仅 `releasegate` 的 workflow 文本测试）。

### 2.3 打包/CI/桌面壳

- 发布链里没有任何 `go build`/`native/core`/`cmd/musicops` 引用；Go 只在 `setup-go`（为捆绑引擎准备）和 Docker 构建阶段出现。Docker 镜像仍是 `python:3.12-slim`，入口启动 Python worker + Python Web 服务。
- `native-core.yml` 只在 `refactor/go-core` 上触发；`windows-tests.yml` 只在 main、`codex/**`、全部 PR 上触发。**新分支 `refactor/go-desktop` 推代码时两套 CI 都不会跑**——阶段 1 必须先接上。
- **桌面壳方案在仓库里未决定**：全仓 grep `webview|wails|tray|systray|桌面壳` 只有一处命中，还是 `ARCHITECTURE.md:87` 的否定句（“这个入口还不是桌面壳”）；`webbrowser` 零使用；`QWebEngine` 零命中。既定方向只到“同一套 WebUI + 同一套 API、不依赖 Python”。`cmd/musicops` 目前只做到监听 `127.0.0.1:0` 并把 URL 打印到 stdout，仓库里不存在启动器。

## 3. 分阶段计划

### 阶段 0 — 盘点与分支（已完成）

- 目标：三路盘点 + 新分支 `refactor/go-desktop`（本地工作区已干净同步到 `453804ed`）。
- 验收：本计划文档在分支内；分支已推远程。

### 阶段 1 — 地基（进行中）

- 1.1 本计划文档进分支。
- 1.2 `native-core.yml` 增加 `refactor/go-desktop` 触发分支，让 Go CI 在本分支上跑起来。
- 1.3 修正已知错误：`internal/app/app.go:79-80` 的注释称 publish_album 未注册，与 map 内实际注册（`workflow.CommandPublish: a.publish`）矛盾，改写注释。
- 1.4 把仓储层已有的任务控制接通到 HTTP：`POST /v1/jobs/{id}/control`（pause/resume/cancel，经 `workflow.Repository.Control`），详情页加暂停/继续/取消按钮（取消需确认框），并加 HTTP 级测试。
- 验收标准：`go vet ./...` 过；`go test ./... -count=1` 22 个包全过；相关包 `-race` 过；`pytest tests/test_native_data_transport.py` 8 passed；新分支上有对应提交；CI 在新分支上跑起来。
- **阶段结束时安装包里还剩的 Python：全部**（PySide6 GUI + worker + PyInstaller + Inno/macOS/Docker 均未动）。

### 阶段 2 — 完整操作界面（契约硬门槛）

- 搜索提交表单（现在只有 API，没有页面入口）；核对确认（需先设计确认/驳回语义：确认候选、接受元数据修正、还是跳过——仓储层目前没有 review-decide 函数，不能凭空编一个）；凭据与下载配置页（Vault 引用、站点账号、qBittorrent、流媒体开关）；批量操作（批量暂停/重试/取消）；确认对话框与一致的成功/失败/待核对反馈；任务重试语义（workflow 层目前只有 pause/resume/cancel，retry 只在 taskrepo 层）。
- 用已有领域测试向量验证端到端行为；验收要在浏览器里走通全部操作；未接入能力标明未接入，不虚构数据。
- 验收标准：契约 `ARCHITECTURE.md:68` 的页面覆盖（任务、队列、核对、站点与下载配置、音乐库、日志）全部真实可用；`go test` + 浏览器走查通过。
- **阶段结束时安装包里还剩的 Python：全部**（界面逻辑已 Go 化，但桌面壳/打包仍 Python）。

### 阶段 3 — 执行器（从“检索到已入库”之间的全部执行侧链路）

- 真实下载：Go qBittorrent 客户端（先停着加种、免费证据匹配、标签归一化、所有权校验、完成归档、活动迁移），接 `process_release` 后的 `downloading` 边。
- 真实扣费确认：需先拿到经核对的线上逐笔扣费格式（目前只有 `musicops.fixture.v1`）——见阻塞项。
- 流媒体执行：按 `downloadpolicy` 顺序接通 Tidal/Apple Music/QQ/网易云/歌单平台文件下载器（现在只有顺序策略，未接线）。
- 转码：给 `transcode` 队列接命令与 handler（WAV→FLAC 等，现在是 `media/validation.py` 的 `prepare_media_copy`）。
- 用户库发布/做种迁移：把 `publish_album` 从“校验本地导入记录”扩展到真实发布与做种迁移；接通 `audiopublish`（现在零调用方）。
- 目录与歌单：MusicBrainz/Discogs、网易云只读/下载/匹配、国内外歌单解析与订阅、歌单文件导入。
- AI 核对：接通 `reviewai`（现在 `reviewModel` 恒为 nil）——见阻塞项（需真实模型服务）。
- 媒体处理：NCM 解密、mutagen 标签读写、OpenCC、歌词边车/内嵌、封面校验。
- 曲库维护：去重合并（PCM 指纹）、合辑、艺人文件夹整理、歌单订阅定时检查。
- 验收标准：每个执行器在受控环境真实跑通（合成基址 + 真实凭证时分别验证，明确区分）；`go test` 全过。
- **阶段结束时安装包里还剩的 Python：桌面壳、worker、Vault、自动更新、打包脚本**（执行器已 Go 化）。

### 阶段 4 — 桌面壳与平台能力（壳方案已定：Windows/macOS Go 原生 GUI，Docker 同界面 WebUI，见 §4）

- 按选定的壳方案实现桌面入口：进程拉起/停止、打开界面、窗口/托盘/自启、崩溃恢复、与 WebUI 同一套 API。
- Go worker 监督/生命周期（替代 `worker/supervision.py` 的 PowerShell 巡检与 `lifecycle.py`）；Vault 三平台凭据库（DPAPI/Keychain/AES-GCM，`vault://` 引用）；自动更新（GitHub Releases 检查 + SHA-256 验签 + 安装器交接）；profile/配置迁移（`startup.py` 选址、`config/schema.py` 缺省）；CLI（`cli.py` 的命令与 `--wait`）。
- 验收标准：桌面端在 Windows/macOS 上真实启动、能走完一次完整业务、退出与升级不丢数据；与 Python 版同等行为的回归测试通过。
- **阶段结束时安装包里还剩的 Python：打包脚本本身（构建时用 Python 编排），安装包内容已无 Python 运行时**（壳、worker、业务全是 Go）。

### 阶段 5 — 打包与切换

- Go 构建的安装器：Windows Inno（只打 Go 二进制 + 引擎，不含 `_internal`）、macOS PKG/DMG/ZIP（Go `.app`）、Linux Docker 镜像（Go 二进制入口，替换 `docker/entrypoint.py` 的 Python 启动）。
- `desktop-release.yml` 改走 Go 产物；冒烟测试（引擎/启动/并行/监督/安装器）改跑 Go 包；`releasegate` 从文本测试升级为真实门禁检查。
- 按契约顺序正式切换唯一后端、删除旧 SQL 业务与重复入口；Muse Wonder 终审与复审；发版（版本号/tag/Release 按仓库既定流程，不在本分支做）。
- 验收标准：三端 `desktop-release` 原生验收全过；安装包内零 Python 运行时（解包验证）；旧档案迁移核验通过。
- **阶段结束时安装包里还剩的 Python：无**（Python 旧代码按契约删除；构建编排脚本视情况保留或 Go 化）。

## 4. 阻塞项（需要用户或外部输入，勿用模拟假装完成）

1. **桌面壳方案（已决定，2026-11-09，用户拍板）**：Windows 与 macOS 用 Go 原生 GUI；Docker 用与 Go GUI 相同的 WebUI（同一套界面、同一套操作，与 AGENTS.md 要求的 Docker 界面与桌面操作同步一致，不另搞一套库工作流实现）。原先的选项留作记录：
   - A. 系统浏览器打开本地 WebUI（Go 进程 + 浏览器）：零新增依赖，复用已有基本认证与响应式页面，与 Docker 用法一致；无原生窗口/托盘体验，需另写启动器负责拉起/停止进程与打开 URL。
   - B. 内嵌 webview 壳（系统 WebView + Go 后端）：有真实桌面窗口与应用图标，复用同一套 WebUI/API；需引入 webview 绑定并处理 Windows（WebView2 运行时依赖）与 macOS（WKWebView）两套原生依赖及打包。
   - C. Go 原生 GUI 框架（Wails/Fyne/Gio）：单 Go 技术栈；Wails 本质仍是 webview 壳，Fyne/Gio 则要重写一套与 WebUI 平行的原生界面，违反“同一套页面”的既定方向，工作量与分叉风险最大。**已选此项（桌面端），但按用户决定：原生 GUI 与 Docker WebUI 共用同一套界面设计，不做两套分叉实现。**
   - D. 保留 Qt 壳、只把后端换成 Go：现有工作台/安装器/更新链路可复用；但 Qt 仍需 Python 运行时，与“安装包里不能再依赖 Python 运行时”的硬门槛直接冲突，只能作过渡、不能作终态。
2. **真实 AI 模型服务**：`reviewai` 需要可用的模型服务/凭证，现在没有；`reviewModel` 恒为 nil。
3. **线上逐笔扣费格式**：没有经核对的真实格式，`musicops.fixture.v1` 只是测试协议；真实扣费确认做不了。
4. **真实四站凭证与 qBittorrent**：真实下载/扣费/流媒体的端到端验证需要真实凭证与可接的 qBittorrent，现在没有；未尝试、未伪造。
5. **Muse Wonder 终审**：只是文档里的门，无代码、无 CI 步骤；发布前需真实走终审与复审。

## 5. 工作规则

- 每做完一个可验证的单元就跑测试（`go test ./...`、`go vet ./...`、相关 `-race`、pytest 传输测试），过了就推到 `refactor/go-desktop` 分支上；不攒到最后一次推。
- 不新建 PR、不合并、不改版本号、不打 tag、不碰 main。
- 做到哪一步就如实汇报哪一步；确实做不了的列为阻塞项和原因，不用模拟或文案假装完成。
