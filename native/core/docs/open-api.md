# 开放接口（供其他软件调用）

核心进程默认只监听 `127.0.0.1`，外部软件在本机用 HTTP 调用即可。要让局域网内的其他机器调用，启动时把 `--listen` 改成 `0.0.0.0:8080`，并设置 `MUSICOPS_API_KEY`（≥16 字符）。**没有 API Key 时只接受 Basic 认证，等同于把页面密码交给调用方。**

- 机器可读契约：`GET /v1/openapi.json`
- 能力自述：`GET /v1/open/info`（版本、数据 schema、端点清单、qBittorrent 是否已配置；不含任何凭据）
- 认证：`Authorization: Basic base64("musicops:<MUSICOPS_APP_PASSWORD>")` 或 `X-API-Key: <MUSICOPS_API_KEY>`
- 页面自己用的 `/v1/*` 不在承诺范围内；本文只描述 `/v1/open/*`。

## 1. 端到端流程

```
解析歌单 → 逐首/逐专建检索任务 → 处理待核对 → 开始下载 → 入库 → 校验
```

### 1.1 解析歌单（只读名单，不下载）

```bash
curl -s -H "X-API-Key: $KEY" -H 'Content-Type: application/json' \
  -d '{"platform":"netease","playlist_id":"123456789","save":true}' \
  http://127.0.0.1:8080/v1/open/playlists/parse
```

返回 `name`、`track_count`、`tracks[]`（每首含 `artist`/`title`/`album`）。`downloaded` 与 `imported` 恒为 `false`——解析不碰音频。

### 1.2 建检索任务

```bash
curl -s -H "X-API-Key: $KEY" -H 'Content-Type: application/json' \
  -d '{
    "job_id":"ext-20261112-001",
    "scene":"playlist",
    "query":"Artist Album",
    "expected_title":"Album",
    "expected_artist":"Artist",
    "enabled_sites":["dicmusic","opencd"],
    "identity":{"evidence_id":"ext-ev-1","kind":"catalog","source":"musicbrainz",
                "subject_id":"<MBID>","tracklist_verified":true,"identity_confirmed":true}
  }' http://127.0.0.1:8080/v1/open/search
```

- 不带 `identity_confirmed`（或为 `false`）时，任务停在 `needs_review` / `review_kind=metadata`：**在拿到身份证据前不会去读站点**。这是有意为之，不是故障。
- `job_id` 缺省会自动生成；重复提交同一个 `job_id` 是重放，不会建第二个任务。
- `enabled_sites` 只接受 `dicmusic` / `orpheus` / `redacted` / `opencd`。

### 1.3 轮询任务状态

```bash
curl -s -H "X-API-Key: $KEY" http://127.0.0.1:8080/v1/open/jobs/ext-20261112-001
```

关键字段：`stage`、`review_kind`、`error_class`、`message`、`paused`、`cancel_requested`。检索的产物是同一种任务：`search:<root>`，用同一个接口查。

`stage=needs_review` 时按 `review_kind` 处理：

| review_kind | 含义 | 可用动作 |
| --- | --- | --- |
| `metadata` | 身份未确认 | 补证据重试不可用，只能 `skip` |
| `search` | 有候选，等选一个 | `accept`（带 `site`+`candidate_id`）/`retry`/`skip` |
| `candidate` | 种子已保存，未下载 | `accept` 或直接调下载接口 |
| `token` | 令牌请求未对账，种子已保存 | 可直接调下载接口（不会二次扣费） |
| `error` | 执行器异常 | `retry`/`skip` |

先读 `GET /v1/open/jobs/{id}/review` 拿到候选清单，再：

```bash
curl -s -H "X-API-Key: $KEY" -H 'Content-Type: application/json' \
  -d '{"action":"accept","site":"dicmusic","candidate_id":"dicmusic:12345"}' \
  http://127.0.0.1:8080/v1/open/jobs/search:ext-20261112-001/review
```

### 1.4 开始下载

```bash
curl -s -X POST -H "X-API-Key: $KEY" -H 'Content-Type: application/json' -d '{}' \
  http://127.0.0.1:8080/v1/open/jobs/process:search:ext-20261112-001/download
```

`{}` 表示使用已保存的 qBittorrent 配置。返回 `download_task_id`。之后可以 `pause` / `resume` / `recheck` / `delete`（`delete` 只从客户端移除种子，**保留已下载文件**）：

```bash
curl -s -H "X-API-Key: $KEY" -H 'Content-Type: application/json' \
  -d '{"action":"pause"}' \
  http://127.0.0.1:8080/v1/open/jobs/<id>/download/control
```

### 1.5 入库与校验

下载完成后入库由工作流自己推进；外部软件自己抓到的文件走这两步：

```bash
# 1) 上传（multipart/form-data，字段名 file）
curl -s -H "X-API-Key: $KEY" -F file=@./track.flac http://127.0.0.1:8080/v1/audio/incoming

# 2) 带元数据入库
curl -s -H "X-API-Key: $KEY" -H 'Content-Type: application/json' \
  -d '{"source":"incoming/track.flac","artist":"Artist","album":"Album","title":"Title","track":1}' \
  http://127.0.0.1:8080/v1/open/library/import
```

`import` 只接受本机状态目录或音乐库内的路径，拒绝符号链接；返回 `request_id`、`path`、`sha256`，之后 `GET /v1/library` 能看到这条记录。

### 1.6 读写标签

```bash
curl -s -H "X-API-Key: $KEY" 'http://127.0.0.1:8080/v1/open/library/tags?path=incoming/track.flac'
curl -s -H "X-API-Key: $KEY" -H 'Content-Type: application/json' \
  -d '{"path":"incoming/track.flac","tags":{"genre":"Ambient","date":"2019-01-02"}}' \
  http://127.0.0.1:8080/v1/open/library/tags
```

字段白名单：`title/artist/album/albumartist/tracknumber/discnumber/date/genre/composer/lyrics`。只支持 FLAC；写入前后都有校验，失败自动恢复原文件。

## 2. 错误码

| code | 含义 | 处理 |
| --- | --- | --- |
| `AUTH_REQUIRED` | 没有凭据或凭据错误 | 补 `X-API-Key` 或 Basic |
| `INVALID_REQUEST` | 字段不合法；可能带 `field` | 按 `field` 修正 |
| `WORKFLOW_REJECTED` | 当前阶段不能执行该动作 | 先查任务状态 |
| `NOT_FOUND` | 没有这个任务/文件 | — |
| `TOKEN_NOT_SETTLED` | 令牌状态不允许下载 | 去令牌页核对，或等 30 分钟到期 |
| `NO_TORRENT_RECEIPT` | 这条任务没有保存的种子 | 重新检索 |
| `CATALOG_UNAVAILABLE` | 目录查询失败 | 稍后重试 |
| `PLAYLIST_UNAVAILABLE` | 歌单平台不可用 | 检查平台与名单 ID |

## 3. 行为约定

- 所有接口都是**显式动作**：解析不下载，检索不下载，下载不扣第二次令牌，入库不删源文件。
- 同一资源最多一次 `usetoken`；令牌待核对（`REQUEST_UNKNOWN`/`EXPIRED`）仍可下载，因为下载只读已保存的种子。
- 未确认身份不会擅自读站点；凭据类的错误不会把密码写进响应或日志。
- `POST` 请求体上限 1 MiB；路径参数只接受库内路径，拒绝 `..` 与符号链接。

## 4. 变更策略

`/v1/open/*` 的字段名与错误码只增不改；删除或改名会先在新版本里保留一段时间的旧别名。页面用的 `/v1/*` 不在此承诺内。
