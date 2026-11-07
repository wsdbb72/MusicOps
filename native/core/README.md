# MusicOps Go 核心（开发中）

当前正式版仍为0.4.31；本目录没有接管真实档案。完整迁移边界见[架构契约](ARCHITECTURE.md)。

Go1.26.3与Pebble v2.1.7，存储及数据服务可用CGO_ENABLED=0构建，无须用户安装数据库服务。

已实现：

- 原生格式2、自有目录与独占锁保护；128MiB有界条件批写、同步WAL、一致性快照、最多200项/约4MiB游标分页、独立checkpoint。
- 任务参数/结果按内容压缩，最大原始64MiB；结果按执行尝试保留。列表只读小摘要，计数固定读取32个键；相同内容共用一份blob。
- 领取、Begin、暂停/继续/取消/重试、进度、完成、中断与历史结果查询。关键请求的回执与业务变化一起提交，进度使用单调sequence去重，避免轮询日志膨胀。重试使用新的fence，旧回调不能覆盖。
- 四个独立队列默认identify4/publish1/transcode1/aux2，空闲槽持续补充；安全检查点处理控制请求，退出期限到达不会偷走尚在运行的任务。
- 阶段完成与后继任务创建同一批原子提交，支持扇出、父子查询、计数聚合、回执重查及并发完成竞争；没有“先完成识别，再碰运气排入搜索”的两个写入窗口。
- 工作流账本把当前阶段、领域证据、任务完成和后继作业放进同一次条件批写。歌单来源曲目数与本地文件数分开。AI 或别名证据不能单独确认身份。搜索后继要先经过四站分类：精确命中才进入获取。限流和网络失败先按 10 分钟、30 分钟、60 分钟重试；歌单或补曲在三次之后才排队流媒体，其它场景进入待核对。离线发行目录把同一发行的曲目合成一次观察。DicMusic 与 Orpheus 的免费令牌只在条件批写里预留，未知余额不会被编造成令牌。流媒体顺序先是 Tidal 和 Apple Music 的无损档，再是 QQ 音乐和网易云；没有音源也没有资料时才排歌单文件下载器。合成文件会复制进隔离库并在同一目录改名，回执不写成已下载或生产已验证。Go 应用注册了识别交接、四站只读检索，以及受授权约束的种子获取。没有免费确认或令牌预留时不会请求种子。音乐文件、做种和用户库发布仍然没有执行器。
- 正式发布仍只走 desktop-release：Windows、macOS ARM64 和 Linux amd64 容器都通过后才公开，`latest` 在那之后推进。本目录的开发检查不会触发它。
- 认证本机RPC仅暴露明确的业务方法，旧原型的raw apply/get/scan接口及check-only启动入口已经移除。RPC16请求槽、96MiB单体限制、128MiB聚合输入预算；输入预算不等于堆内存上限。
- 旧数据进程仍有父进程管道、随机端口、环境密钥，退出时排空已接收请求。Python 客户端只保留给 0.4.31 对照，不翻译 SQL，也不回退第二数据库。新应用的验收是 `cmd/musicops` 自己的进程测试。

`cmd/musicops` 是不依赖 Python 的应用进程。它启动调度器、只读四站检索、SQLite v1/v2 原始导入和领域投影。种子获取只在免费确认或令牌预留成立后保存元信息，不把结果写成已入库。`cmd/musicops-data` 仍是旧 Python 客户端使用的存储进程，不是新的应用入口。0.4.31 的 Python 程序还在树里，供当前正式版运行；新二进制不启动它。

```sh
cd native/core
go test ./... -count=1
go vet ./...
CGO_ENABLED=0 go build -trimpath -o musicops ./cmd/musicops
MUSICOPS_APP_PASSWORD='choose-a-long-password' ./musicops --state-dir /path/to/empty-native-dir --backup-dir /path/to/empty-backup-dir --listen 127.0.0.1:8741
```

标准输出一行 JSON，含 `python: false` 和实际监听地址。开发目录不能指向正在运行的正式 profile 或音乐库。导入接口只接受调用方给出的 `.sqlite` 副本，不写回源文件。

同机一次合成测量：200 行里含一条 256KiB 载荷，SQLite 顺序读取约 1.6ms，导入 Pebble 并逐条核对约 70ms，该次导入分配约 7MB。这不是跨平台加速比，也不是正式档案的耗时。

参考：[Pebble官方实现](https://github.com/cockroachdb/pebble)、[同步批写契约](https://pkg.go.dev/github.com/cockroachdb/pebble/v2#Batch.Commit)。
