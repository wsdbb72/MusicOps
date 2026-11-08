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

### 阶段 1 — 地基（已完成，2026-11-09，远程 HEAD `1f0aecc`，Native core 全绿）

- 1.1 本计划文档进分支。
- 1.2 `native-core.yml` 增加 `refactor/go-desktop` 触发分支，让 Go CI 在本分支上跑起来。
- 1.3 修正已知错误：`internal/app/app.go:79-80` 的注释称 publish_album 未注册，与 map 内实际注册（`workflow.CommandPublish: a.publish`）矛盾，改写注释。
- 1.4 把仓储层已有的任务控制接通到 HTTP：`POST /v1/jobs/{id}/control`（pause/resume/cancel，经 `workflow.Repository.Control`），详情页加暂停/继续/取消按钮（取消需确认框），并加 HTTP 级测试。
- 验收标准：`go vet ./...` 过；`go test ./... -count=1` 22 个包全过；相关包 `-race` 过；`pytest tests/test_native_data_transport.py` 8 passed；新分支上有对应提交；CI 在新分支上跑起来。
- **阶段结束时安装包里还剩的 Python：全部**（PySide6 GUI + worker + PyInstaller + Inno/macOS/Docker 均未动）。

### 阶段 2 — 完整操作界面（已完成，2026-11-09）

- 搜索提交表单：WebUI 新增搜索视图（查询/期望标题/艺人必填、5 个合法场景、每站可选站点、流媒体默认/显式选择、身份证据字段、每站 Cookie 永不保存）+ `POST /v1/search`（`StreamingEnabled` 改为 `*bool`，省略时取已存下载默认；`dicmusic_authkey`/`orpheus_torrent_pass` 仅本次使用）。
- 核对确认：`GET/POST /v1/jobs/{id}/review`（任务、核对种类、期望标题/艺人、每站 Exact 行、候选列表上限 100、允许动作与拒绝理由）+ `workflow.Repository.Review`（见下方设计）。
- 凭据与下载配置页：设置视图（下载默认保存到 Pebble `app/settings/download`、流媒体顺序只读展示、`streaming_fallback_wired: false` 如实标注未接线）+ `POST /v1/sites/preflight`（只读登录检查，Cookie 仅本次调用、不保存）。
- 批量操作：`POST /v1/jobs/batch`（pause/resume/cancel/retry，1–50 个任务、去重、逐任务结果与 code、整批 200）。
- 确认对话框与一致反馈：WebUI `setStatus(kind, text)` 单一反馈通道（ok/fail/review/info + 服务端 code）；取消用 `window.confirm`；待核对任务隐藏 pause/resume/cancel（必 409）；失败任务显示重试按钮。
- 任务重试语义：`taskrepo.ControlOwned` 接受 `retry`；`workflow.Repository.Review` 的 retry（见下方设计）。重试同时补上了阶段 1 的遗留缺口：发布校验永久失败的任务现在可通过 failed 任务重试出 publishing 阶段。
- 验收：`go vet ./...` 过；`go test ./... -count=1` 全过；app/workflow/taskrepo `-race` 过；pytest 传输 8 passed；真服务 HTTP 端到端走通（搜索→待核对→accept→获取子任务存种子→needs_review/candidate；401 搜索→retry 重检索→skip；配置保存；批量逐任务结果；预检已认证且 Cookie 未落盘；control 对已完成任务 409）。
- **阶段结束时安装包里还剩的 Python：全部**（界面逻辑已 Go 化，但桌面壳/打包仍 Python）。

#### 阶段 2 review-decide 设计（已定，2026-11-09，从真实代码出发）

**现状（以代码为准）：** `needs_review` 只由四处产生——检索执行器按 `searchdecision` 分类得到 `review_search`/`review_candidate`（`execute.go` 的 `completion`）；获取执行器在不能获取时（`acquire.go` 的 `finishAcquire`，ReviewKind 为 `candidate` 或 `token`）；执行器被确认停止后的 `Interrupt`（ReviewKind `interrupted`，任务进 `recovery_required`）；无执行器时的 `BlockUnregistered`（ReviewKind `error`）。`taskrepo` 的 `finishDecision` 只允许 running/paused 的任务完成，所以已 `blocked` 的任务**走不了** `workflow.Complete`——核对决议必须是一条新的原子转移，而不是复用执行器完成路径；`Control` 的 pause/resume/cancel 对 `blocked` 任务一律 409（`terminal()` 把 `blocked` 算终态），所以待核对任务目前除入库外没有任何出路。

**决议动作（`workflow.Repository.Review`，一次原子提交内完成任务转移 + 任务转移）：**
- `accept`（仅 ReviewKind `candidate` 的**检索任务**）：应用层从已存证据（每站检索观察）与已存检索计划重建 `searchdecision.Report`，代入用户选定的站点/候选，用同一 `searchdecision.Decide` 分类，必须得到 `queue_request` 才放行；放行后搜索任务进 `completed`，新建获取子任务（payload 为补上选定站点/候选的检索计划），搜索任务进 `completed`，SearchAction 记 `queue_request`。用户是新的决策输入，分类器仍是同一台。**获取任务不能 accept**：它的候选在入队时已选定，accept 会在它下面再挂一个获取子任务；出路只有 retry 或 skip。核对页上获取任务无候选可列（其证据只有种子收据），只给 retry/skip，这是如实行为。
- 两站返回同一发行时不进核对：`selectIdentity` 会把一致的候选折成一个直接排队；只有候选身份不一致（例如发行年份不同且均非零，`reviewmatch.Same` 否决合并）时分类器才出 `selection_required`。测试里用不同 `groupYear` 构造这一情形。
- `retry`（ReviewKind `search`/`candidate`/`token` 的待核对任务，或 `failed` 任务）：复用 `taskrepo` 已有的 retry 语义（任务回 `queued`、清空属主与进度），同时把任务阶段还原为执行器重跑时应处的阶段——检索任务回 `searching`；`stage:` 任务取其后缀阶段；`process:` 子任务按父检索任务的 SearchAction 还原为 `requesting`/`fallback`；检索重试计入 `SearchRetryCount`（上限 3 次，与分类器的可重试上限一致）并清掉重试等待。`error`（无执行器，重试必定再次卡住）、`interrupted`（任务在 `recovery_required`，`taskrepo` 的 retry 明确拒绝，需先做外部影响核对——暂未接入）与 `fallback` 阶段的获取任务（重跑必定再次因“流媒体未接入”卡住）不提供重试；已取消的任务不重试（取消是终态）。
- `skip`（ReviewKind `search`/`candidate`/`token`/`error`）：任务进 `completed`，任务进 `skipped`，SearchAction 记 `skip`；这是用户的明确放弃，不是分类器的输出，代码里注明。
- 决议只接受 `needs_review`（accept/skip）与 `failed`（仅 retry）的任务；已暂停的任务重试时视为同时恢复（重试即明确要求重新执行，job 的 Paused 标志与 task 的 PauseRequested 一并清除），其余动作拒绝已暂停任务。

**实现中发现并修掉的重试 bug（2026-11-09，测试先暴露、根因已确认）：** 重试后的检索重跑会在 `Complete` 时报 `ErrStale`、被协调器中断进 `interrupted`/`recovery_required`，导致后续 skip 也 409。根因是证据 ID 只由（任务 ID，站点）决定：重试在同一任务上重跑，观测内容一变就与已存证据字节冲突（证据库对同 ID 不同字节拒绝写入）。修复：证据 ID 改为（任务 ID，fence，站点），每次尝试独立；执行器在观测缺时间戳时补 `ObservedAt`（`a.now()`），核对页按 `ObservedAt` 取每站最新一次观测（证据列表是摘要序、不是时间序）；重试保留搜索 Cookie（重跑要用同一 Cookie 再检索），skip 才删全部 secret。**accept 不删 Cookie**：获取子任务仍需会话 Cookie 读种子详情页并下载种子，删了它子任务会以“没有可用的登录状态”卡住；未被核对的 secret 只留内存、随进程重启消失，永不落盘。

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
