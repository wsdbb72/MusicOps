# MusicOps 全量 Go 迁移 — Muse Wonder 终审档案

> **状态：NOT YET REVIEWED（尚未评审）**。本文档只准备评审材料，不代替评审、不伪造评审结论。
> 终审与复审通过前，不创建 Release、不打 tag、不推进 `latest`（`MIGRATION_PLAN.md` §1）。
>
> 候选提交：`refactor/go-desktop` 分支的 Phase 5 实现提交（SHA 待推送后填入，见“候选提交”一节）。
> 评审对象：`refactor/go-desktop` 分支在该提交时的全部内容，对照 `main`（`e453b0ca`，v0.4.32）。

## 候选提交

- 分支：`refactor/go-desktop`
- Phase 5 实现提交 SHA：`c3bd468cd058aa751620568ceaca623edf9a549e`（“MusicOps full-Go migration Phase 5: packaging and switch-over (release-ready)”）
- Windows gate 修复提交 SHA：`0e30643016cd2190cea87f0e7592d4af1483bbbb`
- dataengine 测试竞态修复提交 SHA：`5f819b9dde58ddbdf921d51db3fa518595e27294`（最后一次代码改动；其后的提交仅改文档）
- **候选代码提交（供评审）：`5f819b9` 的树**。档案更新提交（本文档的最后一次修改）只改本文档，不改代码；评审对象为该提交的树，即 `5f819b9` 的代码 + 最新档案。
- CI 验证：Native core 运行 **42581790198**（HEAD `5f819b9`，含上述全部提交）：completed success，四任务全绿（macos-15、windows-latest、race、ubuntu-latest），四份任务日志逐行核查零 FAIL（Windows 日志正向确认 `ok internal/dataengine 7.324s` 与 `ok internal/releasegate 6.818s`）。注意：前两轮运行曾各藏一个 Windows 失败（42579135564 的 releasegate 3 项、42580776325 的 dataengine 1 项），任务结论均为 success——pwsh 假绿再次证实，均已由 `0e30643`、`5f819b9` 修复，见“本次发现并修复的问题”第 4、5 项。
- 基线：Phase 4 完成态 `5f0b2fc`（Native core 运行 42572612091 四任务全绿、四份日志逐行零 FAIL）

## 评审范围：每阶段摘要

### 阶段 0–1 — 盘点、地基（已完成，2026-11-09）

计划文档、分支 `refactor/go-desktop`、CI 触发分支接线、任务控制 HTTP 接入。远程 HEAD `1f0aecc`，Native core 全绿。

### 阶段 2 — 完整操作界面（已完成，2026-11-09）

搜索提交、核对确认（`workflow.Repository.Review`，accept/retry/skip）、凭据与下载配置、批量操作、统一反馈通道、任务重试语义。发布校验永久失败的任务可由 failed 任务重试出 publishing 阶段（阶段 1 的遗留缺口已闭合）。

### 阶段 3 — 执行器（已完成，2026-11-09）

真实下载执行器（qBittorrent，add-stopped/归属校验/归档/入库）、token 扣费确认路径（`SpendSource` + 文件源，线上格式仍阻塞）、流媒体回退（四平台如实 `no_result` + 歌单导入）、转码（PCM WAV→FLAC）、目录接线（真实 MusicBrainz）、媒体端点（NCM/FLAC 标签/LRC/去重/艺人文件夹）。合并 E2E 40/40。合并后 CI 失败（macOS/Windows 路径问题）已修复，运行 42561417140 四任务全绿。

### 阶段 4 — 桌面壳与平台能力（已完成，2026-11-09）

Wails v2.17.1 桌面壳（窗口反向代理同一 WebUI）、Go 监管、Vault 三平台、自动更新、配置迁移、CLI。运行 42572612091 四任务全绿且日志零 FAIL。已知缺口见“真实剩余缺口”。

### 阶段 5 — 打包与切换（本档案的评审重点，2026-11-09）

- **Windows**：`packaging/MusicOps-go.iss`（Go 包体：`MusicOps.exe` + `musicops-core.exe` + `tools/` + 文档；无 `MusicOps-worker.exe`、无 `_internal`；版本只接受 `/DAppVersion=`，默认 `0.0.0-dev`；升级检测兼容 Go 与 legacy Python 两种布局，profile/tools 保留；运行中三可执行文件任一被占即拒更新）+ `packaging/build_windows_go.ps1`（引擎、goversioninfo PE 版本资源、CGO_ENABLED=0 双二进制、`collect_go_licenses.py`、manifest、Go 冒烟、releasegate、ISCC 编译、安装器验收）+ `native/core/internal/smoke/installer_windows_test.go`（6 项命名检查 + JSON 报告）。
- **macOS**：`packaging/build_macos_go.sh`（darwin/arm64，核心 CGO_ENABLED=0、壳 CGO_ENABLED=1，ad-hoc 签名 + `--verify --deep --strict`、审计、签名前的 manifest、Go 冒烟、ditto ZIP、hdiutil DMG 挂载验收、`macos_pkg_go.sh` PKG、SHA-256/512 sidecar、build.json、releasegate）+ `packaging/macos/` 模板（Info.plist、Distribution.xml、components.plist、Welcome/ReadMe、preinstall）+ `native/core/internal/smoke/installer_darwin_test.go`（11 项检查：安装、payload digest、root 属主、签名、receipt 版本、profile/Vault 保留、运行核心拒重装、重装清旧文件、已安装 `.app` 冒烟）。
- **Docker**：`docker/Dockerfile` 重写（golang:1.26.3-bookworm 构建阶段 + debian:bookworm-slim 运行阶段，无 Python 运行时；HEALTHCHECK 走 `musicops healthcheck`；ENTRYPOINT `tini -s -- /opt/musicops/entrypoint.sh`）+ `docker/entrypoint.sh`（`/config/web-password` 生成/复用 0600、16 字符下限守卫、vault key/HOME、PATH、exec 单核心）+ `native/core/cmd/docker-smoke/main.go`（容器端到端：提包验 Go、healthy 等待、401/WebUI/healthz、uid 1000、amd64、无 VNC、ffmpeg、干净停机、重启密码保留）+ `docker/compose.yaml` healthcheck + `.dockerignore` 放行 `native/` 与根 `LICENSE`。
- **共享**：`native/core/internal/smoke/`（黑盒冒烟：含桌面壳的发版包体 23 项，无桌面壳的开发包体 20 项；含 `.app` 布局映射：`.app` 根下的二进制走 `Contents/MacOS`、工具走 `Contents/Resources/tools`、manifest 走 `Contents/Resources/manifest.json`；假 `.app` 端到端已在 Linux 上验证通过）+ `native/core/internal/releasegate/`（真实门禁：无 Python、Go 构建 + 版本、manifest、引擎、安装器、smoke 报告版本一致、build.json 版本）+ `musicops healthcheck` 子命令 + 桌面壳 `--version` + `packaging/collect_go_licenses.py`（Go 模块许可证收集；Python 依赖许可证不再进 Go 包）。
- **测试更新**：`tests/test_windows_installer.py`（Go 包体 + `MusicOps-go.iss`，6 个场景名不变）+ `tests/test_release_versions.py`（版本同步改读源码文件、不再依赖已安装的 Python 包；新增 Go 版 `.iss` 命令行版本测试；PE 资源测试改测 `MusicOps.exe` + `musicops-core.exe`）。

## 测试证据（本地，真实命令与结果）

- `go vet ./...`：通过。
- `go test ./... -count=1`：32 个包 ok、4 个无测试文件、零 FAIL（本地，`TMPDIR=$HOME/.cache/go-tmp GOTMPDIR=$HOME/.cache/go-tmp`）。
- `-race` 相关包（app/workflow/taskrepo/downloadpolicy/smoke/releasegate/desktop/cmd/musicops/cmd/docker-smoke）：9 个包全部 ok（app 74.4s、workflow 19.4s、taskrepo 41.5s、downloadpolicy 1.0s、smoke 10.1s、releasegate 19.4s、desktop 23.9s、cmd/musicops 27.9s、docker-smoke 1.0s），零 FAIL。
- `pytest tests/test_native_data_transport.py`：8 passed（`MUSICOPS_TEST_DATA_BINARY` 指向新构建的 `musicops-data`）。
- `pytest tests/test_release_versions.py tests/test_windows_installer.py`：2 passed、8 skipped（Windows-only，在 Linux 上正确跳过）。
- 交叉构建/检查：`GOOS=windows go vet ./internal/smoke/` 通过；`GOOS=windows CGO_ENABLED=0` 构建核心 + 桌面壳通过（含 goversioninfo PE 版本资源，UTF-16LE 内含 `9.9.9-test`）；`GOOS=darwin go vet`（core/smoke/releasegate/docker-smoke）通过；`GOOS=darwin CGO_ENABLED=0` 构建核心通过；`GOOS=linux go build ./...` 通过。
- 冒烟：开发包体 20/20 通过（0.99s）；假 `.app`（Linux 核心按 `.app` 布局摆放 + manifest）20 项通过（仅 engines_present 跳过，因假包无引擎）。
- `collect_go_licenses.py`：89 个模块有许可证文件、97 个许可证文件、0 个 `.py` 文件、无 Python 依赖许可证；144 个仅存在于模块图的模块跳过（无源码下载、未链接进二进制）；3 个有源码无许可证文件的模块列为警告（`go-errors/errors`、`kr/pretty`、`kr/text`，均为间接依赖）。
- `docker-smoke` 单元测试：通过（含本次修复的 `healthzOK` 缺失 `python` 字段误接受问题——见“本次发现并修复的问题”）。
- macOS 脚本：`bash -n` 通过；Info.plist/Distribution.xml/components.plist 模板解析通过；`GOOS=darwin go test -c` 编译 smoke 与 releasegate 测试二进制通过。

## 本次发现并修复的问题（均有测试覆盖）

1. `docker-smoke` 的 `healthzOK` 用普通 `bool` 解析 `python` 字段，缺失字段时读作 `false` 而误接受非 Go-core 的 healthz 响应。改为 `*bool`，要求字段存在且为 false；`cmd/musicops` 的 `healthcheck` 用 `map[string]any` 比较，缺失字段为 `nil`，本来就是正确的。
2. `smoke.Run` 没有 `.app` 布局映射：`build_macos_go.sh` 第 6 步按任务规定的调法（`BUNDLE=<app>`）会在 `core_present_and_executable` 等 9 项上失败（macOS 子代理探针 B1 实测证实）。已在 `smoke.go` 增加映射（`defaults` + `binRel`，`runLive` 的启动路径一并修正），单元测试 `TestAppLayoutMapping` + 假 `.app` 端到端（20 项通过）覆盖；macOS 安装器测试的显式相对路径写法不受影响。
3. Windows 构建脚本调 `collect_licenses.py`（Python 依赖许可证收集器）会把 PySide6 等许可证写进 Go 包。改为 `packaging/collect_go_licenses.py`（Go 模块许可证）；macOS 脚本同样接入。
4. releasegate 测试在 Windows 上硬编码 `Layout: "linux"`，夹具却是 `.exe` 文件名：`TestGatePassesGoodBundle` 在 `core_present_and_go_built` 上失败，篡改/删除步骤触及 manifest 未覆盖的 Linux 文件名。CI 运行 42579135564 的 Windows 任务显示 success、日志里 3 个 FAIL（pwsh 假绿再次证实）——由 `gh run view --log` 逐行核查发现。修复：测试按平台选布局（`testLayout()`）并用 `exeSuffix()` 操作真实夹具文件名；gate 本身未动（生产上 Windows 只用 windows 布局）。修复提交 `0e30643`，本地 Linux 9 项全过、`GOOS=windows go vet` 通过，CI 运行 42580116895 验证通过。
5. dataengine 重开测试 `TestStartupReopenAndConcurrentConditionalWrites` 在 Windows CI 运行 42580776325 上失败：`dirBytes` 在引擎仍打开时 walk 数据目录，Pebble 后台清理旧 OPTIONS 文件与 walk 的 stat 撞车（`OPTIONS-000003` 在 readdir 与 stat 之间消失，`profile_test.go:79`）。数据本身无损（重开读回 `profile/3/7`，201 键回放成功），且前一轮同代码通过（文档提交差量），属平台敏感竞态而非 Phase 5 回归。修复：读验证通过后先 `Close()` 再 walk（Close 后 Pebble 后台文件管理停止，目录稳定）。修复提交 `5f819b9`，本地该测试 10 次重复 + 整包全过，CI 运行 42581790198 验证通过（Windows 日志 `ok internal/dataengine 7.324s`）。

## 真实剩余缺口（评审时必须看到，不要当作已完成）

### Phase 4 遗留（Phase 5 修不了）

1. 托盘装不上：Wails v2.17.1 无公开 API 安装托盘图标；`--minimized` 只能最小化到任务栏/Dock。
2. 桌面壳未在 Windows/macOS 真机上跑过；macOS CGO 壳的真实链接与运行以 CI 为准。
3. 更新全流程未在真机上走完（Check 已对真实 GitHub API 跑通；Stage 下载与 Launch 交接未跑）。
4. `core.log` 无轮换；监管退避长时 soak 未测。
5. `control <id> retry` 在任务层被核心拒绝（400），CLI 透传 exit 7。
6. 取消只在 checkpoint 被观察（仅搜索路径修了）。
7. Windows CI 步骤结论不可靠（pwsh 假绿）：每次须直接读 Windows 任务日志 grep FAIL；工作流修复见发版清单。

### Phase 5 遗留（本地无条件验证，只能由发版工作流验证）

8. ISCC 编译 + Windows 安装器验收未在本地跑过（无 Windows/ISCC）：由 `desktop-release` 的 windows job 验证。
9. macOS PKG/DMG/安装器验收、桌面壳 CGO 构建未在本地跑过（无 Mac）：由 `macos-release` 验证。
10. Docker 镜像构建与 `docker-smoke` 端到端未在本地跑过（无 Docker）：由 `docker-release` 验证。
11. 引擎只验存在、未验实跑：`CheckEngines` 只验 `engines.json` + 两二进制可执行；旧 `smoke_engines.py` 的 9 项实跑检查（FlareSolverr 启动并连发行版 chromium 等）没有 Go 等价物——Go 核心不监管引擎，无法在冒烟里实跑它们。
12. Vault 无直接往返检查：Go 核心没有 vault 读写端点，`docker-smoke` 只能以重启后 healthy + 密码保留间接证明状态连续。
13. `MUSICOPS_WEB_PASSWORD_FILE` 覆盖被丢弃（entrypoint.sh 固定读 `/config/web-password`）；已用 12–15 字符旧密码升级的用户，容器会明文报错退出（故意的，否则核心 exit 2 且无明确原因）——发布说明应提一句。
14. `packaging/THIRD-PARTY-NOTICES.md` 仍是 Python 发行版的文本（PySide6/Qt、Python 运行时、OpenCC Python 等章节）：发版分支应为 Go 发行版修订；本分支未动它。
15. 旧 Python 源码（`src/`、旧打包/冒烟脚本、旧 Docker 入口）尚未删除：按契约顺序，删除是发版时步骤（见“发版就绪清单”），须在三端工作流于发版分支跑通后执行，不在本分支提前删。

### 长期阻塞（与 Phase 5 无关，历来如此）

16. 真实 AI 模型服务：`reviewModel` 恒为 nil。
17. 线上逐笔扣费格式：只有 `musicops.fixture.v1`，`production_verified` 恒 false。
18. 真实四站凭证与 qBittorrent：真实下载端到端未验证。
19. OpenCC：仓库无字典数据，Go 无实现，未提供转换端点。

## 发版就绪清单（届时要执行的步骤，顺序不可乱）

1. 用户批准发版（本档案的终审 + 复审通过是前置条件）。
2. 经浏览器按汇报中的精确 diff 改三个工作流：`desktop-release.yml`（Windows 走 Go 产物 + Go 冒烟 + 安装器报告校验 + pwsh 步骤结论修复 + publish job 检查更新）、`macos-release.yml`（Go 构建 + ffmpeg + 安装器验收 + build.json 合并 + `version` input）、`docker-release.yml`（`version` input + Go 冒烟 + gate + 删除 Python vault 测试步骤）。
3. 从 `refactor/go-desktop` 切 `codex/release-*` 分支。
4. 按 0.4.32 既定流程升版（4 处版本文件同步 + 发布说明重写；`THIRD-PARTY-NOTICES.md` 按缺口 14 修订）。
5. 在 `codex/release-*` 上跑 `desktop-release`：Windows/macOS/Linux 三端原生验收 + Docker 镜像构建与 `docker-smoke`；逐项核对本档案“Phase 5 遗留”第 8–10 项。
6. 三端验收通过后，按契约删除旧 Python 源码与重复入口（`src/`、旧打包/冒烟脚本、旧 Docker 入口：`docker/smoke.py`、`docker/entrypoint.py`、`docker/healthcheck.py`、`docker/requirements.txt`、`packaging/smoke_*.py`、`packaging/build_windows.ps1`、`packaging/build_macos.py`、`packaging/macos_installer.py`、`packaging/MusicOps.iss`（旧版）、`packaging/windows_version.py`（旧版）等——以发版分支上的实际引用核对为准），再跑一次全量测试。
7. Muse Wonder 终审 + 复审（本档案即终审材料；复审在删除步骤后重审候选提交）。
8. 发布 Release（工作流 publish job 建 tag + draft + 验资 + 发布；不手动建 tag/Release）。
9. 同步已验证提交到 `main`（PR 流程，与 0.4.32 同）。
10. `docker_latest` 推广 `latest`（仅在桌面 Release 成功后）。

## 评审检查单（供 Wonder 评审员逐项核对）

- [ ] 候选提交的树里，Windows/macOS/Docker 三端产物定义均不含 Python 运行时（grep `.py`/`_internal`/`MusicOps-worker` 在产物路径上）。
- [ ] 版本只从 4 处 canonical 文件来，Go 安装器不硬编码版本（`MusicOps-go.iss` 只有 `0.0.0-dev` 默认值；`build_macos_go.sh`/`build_windows_go.ps1` 只接受参数）。
- [ ] `releasegate` 对三端 bundle 的检查均真实读产物（非文本断言），smoke 报告版本与发版版本一致。
- [ ] 旧 Python 测试/脚本未被悄悄删除（删除是发版时步骤，须在验收后）。
- [ ] 本档案“真实剩余缺口”无一被写成已完成；“测试证据”每项均有可复现的命令与结果。
- [ ] 三端工作流 diff（见父代理汇报）与本分支代码一致：工作流引用的每个脚本、环境变量、报告路径在分支内真实存在。
