// The open API is the stable surface other software uses: parse a playlist,
// search, review, download, import. The /v1/* handlers are the UI's own shape
// and may change with the page; the /v1/open/* aliases and the document below
// are the contract. Both hit the same handlers, so there is no second
// implementation to drift.
package app

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/version"
)

// openAPIDocument is served at GET /v1/openapi.json. It is hand-written rather
// than generated: the surface is small and stable, and a generator would lock
// the internals into the public contract.
const openAPIDocument = `{
  "openapi": "3.1.0",
  "info": {
    "title": "MusicOps 开放接口",
    "version": "1",
    "description": "供其他软件调用的稳定接口：解析歌单、检索、核对、下载、入库。与页面共用同一套任务与工作流；本文件描述的是 /v1/open/* 别名，页面用的 /v1/* 不在此承诺之内。默认只监听 127.0.0.1；跨机调用需自行把监听地址改到局域网，并设置 MUSICOPS_API_KEY。"
  },
  "servers": [{ "url": "http://127.0.0.1:8080", "description": "本机核心" }],
  "security": [{ "basicAuth": [] }, { "apiKey": [] }],
  "components": {
    "securitySchemes": {
      "basicAuth": { "type": "http", "scheme": "basic", "description": "用户名固定 musicops，密码是启动时的 MUSICOPS_APP_PASSWORD" },
      "apiKey": { "type": "apiKey", "in": "header", "name": "X-API-Key", "description": "仅当宿主设置了 MUSICOPS_API_KEY 时有效；权限与密码相同" }
    },
    "schemas": {
      "Error": {
        "type": "object",
        "properties": {
          "ok": { "type": "boolean", "const": false },
          "code": { "type": "string", "description": "稳定的错误码，例如 INVALID_REQUEST、WORKFLOW_REJECTED、CATALOG_UNAVAILABLE" },
          "error": { "type": "string" },
          "field": { "type": "string", "description": "请求字段有问题时给出字段名" }
        },
        "required": ["ok", "code"]
      },
      "TrackRequest": {
        "type": "object",
        "properties": {
          "artist": { "type": "string" },
          "album": { "type": "string" },
          "title": { "type": "string" },
          "query": { "type": "string", "description": "检索关键词；缺省时由 artist/album/title 拼出" },
          "track": { "type": "integer" },
          "duration": { "type": "number", "description": "秒；用于目录核对，缺省则不核对时长" }
        },
        "required": ["title"]
      },
      "SearchRequest": {
        "type": "object",
        "properties": {
          "job_id": { "type": "string", "description": "缺省则生成；重复提交同一个 id 会重放同一个任务" },
          "batch_id": { "type": "string", "description": "缺省等于 job_id" },
          "scene": { "type": "string", "enum": ["playlist", "upgrade", "fill", "expand"], "default": "playlist" },
          "query": { "type": "string" },
          "expected_title": { "type": "string" },
          "expected_artist": { "type": "string" },
          "source_track_ids": { "type": "array", "items": { "type": "string" } },
          "enabled_sites": { "type": "array", "items": { "type": "string" }, "description": "dicmusic / orpheus / redacted / opencd（三个令牌站共用同一套账本）" },
          "identity": {
            "type": "object",
            "description": "调用方自带的身份证据。没有证据时传 identity_confirmed=false，任务会停在待核对（metadata），不会擅自检索。",
            "properties": {
              "evidence_id": { "type": "string" },
              "kind": { "type": "string", "enum": ["catalog", "api", "website"] },
              "source": { "type": "string" },
              "subject_id": { "type": "string", "description": "如 MusicBrainz Release ID" },
              "tracklist_verified": { "type": "boolean" },
              "identity_confirmed": { "type": "boolean" },
              "payload": { "type": "string" }
            }
          },
          "cookie": { "type": "string", "description": "本次请求使用的站点 Cookie；不传则用保存的 Cookie" }
        },
        "required": ["query", "expected_title"]
      },
      "JobStatus": {
        "type": "object",
        "properties": {
          "ok": { "type": "boolean" },
          "job": {
            "type": "object",
            "properties": {
              "job_id": { "type": "string" },
              "stage": { "type": "string", "description": "queued / searching / downloading / validating / publishing / needs_review / completed / failed / cancelled" },
              "review_kind": { "type": "string", "description": "needs_review 时说明该看什么：metadata / search / candidate / token / error" },
              "error_class": { "type": "string" },
              "message": { "type": "string" },
              "paused": { "type": "boolean" },
              "cancel_requested": { "type": "boolean" }
            }
          }
        }
      },
      "ReviewAction": {
        "type": "object",
        "properties": {
          "action": { "type": "string", "enum": ["accept", "retry", "skip"] },
          "site": { "type": "string" },
          "candidate_id": { "type": "string" }
        },
        "required": ["action"]
      }
    }
  },
  "paths": {
    "/v1/open/info": {
      "get": {
        "summary": "能力自述",
        "description": "版本、数据 schema、开放端点清单、qBittorrent 是否已配置。不含任何凭据。",
        "responses": { "200": { "description": "能力清单" } }
      }
    },
    "/v1/open/playlists/parse": {
      "post": {
        "summary": "解析歌单（只读名单，不下载）",
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "schema": {
                "type": "object",
                "properties": {
                  "platform": { "type": "string", "description": "netease / qqmusic / deezer / qobuz / applemusic / tidal" },
                  "playlist_id": { "type": "string" },
                  "save": { "type": "boolean", "description": "同时把名单存进歌单库" },
                  "subscribe": { "type": "boolean", "description": "保存并订阅后续变更" }
                },
                "required": ["platform", "playlist_id"]
              }
            }
          }
        },
        "responses": {
          "200": { "description": "曲目名单；downloaded 与 imported 恒为 false" },
          "502": { "description": "平台不可用", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/Error" } } } }
        }
      }
    },
    "/v1/open/search": {
      "post": {
        "summary": "创建检索任务",
        "description": "任务先在 identify 阶段核对身份：调用方给出已确认的身份证据才会进入四站检索；否则停在待核对（metadata），由调用方补证据或跳过。",
        "requestBody": { "required": true, "content": { "application/json": { "schema": { "$ref": "#/components/schemas/SearchRequest" } } } },
        "responses": {
          "202": { "description": "已创建；返回 job_id" },
          "400": { "description": "字段问题（返回 field）" },
          "422": { "description": "场景不支持" }
        }
      }
    },
    "/v1/open/jobs/{id}": {
      "get": {
        "summary": "任务状态",
        "parameters": [{ "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "状态", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/JobStatus" } } } }, "404": { "description": "没有这个任务" } }
      }
    },
    "/v1/open/jobs/{id}/review": {
      "post": {
        "summary": "处理待核对",
        "requestBody": { "required": true, "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ReviewAction" } } } },
        "responses": { "200": { "description": "已处理" }, "409": { "description": "该任务当前不能执行这个动作" } }
      }
    },
    "/v1/open/jobs/{id}/download": {
      "post": {
        "summary": "开始下载（交给 qBittorrent）",
        "description": "body 传空对象即使用已保存的 qBittorrent 配置；也可临时传 base_url/username/password。令牌待核对（token）任务只要种子已保存同样可以下载。",
        "requestBody": { "required": false, "content": { "application/json": { "schema": { "type": "object", "properties": { "base_url": { "type": "string" }, "username": { "type": "string" }, "password": { "type": "string" }, "save_path": { "type": "string" }, "category": { "type": "string" }, "tags": { "type": "string" } } } } } },
        "responses": {
          "202": { "description": "已排队；返回 download_task_id" },
          "409": { "description": "NO_TORRENT_RECEIPT 或 TOKEN_NOT_SETTLED" }
        }
      }
    },
    "/v1/open/jobs/{id}/download/control": {
      "post": {
        "summary": "控制下载中的种子",
        "requestBody": { "required": true, "content": { "application/json": { "schema": { "type": "object", "properties": { "action": { "type": "string", "enum": ["pause", "resume", "recheck", "delete"] } }, "required": ["action"] } } } },
        "responses": { "200": { "description": "已执行" }, "409": { "description": "不在下载阶段" }, "502": { "description": "qBittorrent 未接受" } }
      }
    },
    "/v1/open/catalog/identify": {
      "post": {
        "summary": "目录识别（MusicBrainz）",
        "description": "离线匹配发行与曲目，返回 confirmed / conflict / unresolved。confirmed 仅表示与 MusicBrainz 曲目表离线匹配通过。",
        "responses": { "200": { "description": "识别结果" }, "502": { "description": "目录不可用" } }
      }
    },
    "/v1/open/library/import": {
      "post": {
        "summary": "把服务器上已有的音频文件入库",
        "description": "文件必须在本机状态目录或音乐库里（incoming/ 最合适）。先 POST /v1/audio/incoming 上传，再用本接口带元数据入库；也可直接对库内文件调用。",
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "schema": {
                "type": "object",
                "properties": {
                  "source": { "type": "string", "description": "incoming/xxx.flac 这样的相对路径" },
                  "artist": { "type": "string" },
                  "album": { "type": "string" },
                  "title": { "type": "string" },
                  "track": { "type": "integer" },
                  "root_id": { "type": "string", "description": "缺省按艺人+专辑派生" },
                  "origin": { "type": "string", "enum": ["local_import", "manual"], "default": "local_import" }
                },
                "required": ["source", "artist", "album", "title", "track"]
              }
            }
          }
        },
        "responses": { "200": { "description": "已入库；返回 request_id / path / sha256" }, "409": { "description": "目标位置已有同名文件" }, "422": { "description": "音频被拒绝" } }
      }
    },
    "/v1/open/library/tags": {
      "get": {
        "summary": "读取标签",
        "parameters": [{ "name": "path", "in": "query", "required": true, "schema": { "type": "string" } }],
        "responses": { "200": { "description": "标签" } }
      },
      "post": {
        "summary": "写标签（FLAC）",
        "requestBody": { "required": true, "content": { "application/json": { "schema": { "type": "object", "properties": { "path": { "type": "string" }, "tags": { "type": "object" } }, "required": ["path", "tags"] } } } },
        "responses": { "200": { "description": "已写入并回读" }, "422": { "description": "写入或回读失败，已恢复" } }
      }
    }
  }
}
`

// openAPI serves the machine-readable contract. An invalid document is a
// server bug: it answers 500 instead of handing a client broken JSON.
func (a *App) openAPI(w http.ResponseWriter, _ *http.Request) {
	if !json.Valid([]byte(openAPIDocument)) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "OPENAPI_INVALID"})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, openAPIDocument)
}

// openInfo is GET /v1/open/info: what this instance can do, without secrets.
func (a *App) openInfo(w http.ResponseWriter, _ *http.Request) {
	qbConfigured := false
	if settings, err := a.loadQBittorrentSettings(); err == nil && strings.TrimSpace(settings.BaseURL) != "" {
		qbConfigured = true
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "application": "musicops", "version": version.Version,
		"data_schema": dataengine.SchemaVersion,
		"auth":        []string{"basic", "api_key"},
		"qbittorrent_configured": qbConfigured,
		"token_sites":            []string{"dicmusic", "orpheus", "redacted"},
		"endpoints": []string{
			"GET /v1/open/info",
			"POST /v1/open/playlists/parse",
			"POST /v1/open/search",
			"GET /v1/open/jobs/{id}",
			"POST /v1/open/jobs/{id}/review",
			"POST /v1/open/jobs/{id}/download",
			"POST /v1/open/jobs/{id}/download/control",
			"POST /v1/open/catalog/identify",
			"POST /v1/open/library/import",
			"GET /v1/open/library/tags",
			"POST /v1/open/library/tags",
		},
		"note": "GET /v1/openapi.json 是机器可读的同一份契约。检索任务默认停在待核对身份，不会在没有证据时去读站点。",
	})
}
