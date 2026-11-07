package app

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/sqlitemigrate"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

const page = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>MusicOps</title><link rel="stylesheet" href="/app.css"></head>
<body>
<header><div><p class="mark">MusicOps</p><h1>任务</h1></div><p class="quiet">此进程不启动 Python。种子字节不是专辑下载，也不是入库。</p></header>
<nav id="nav" aria-label="功能">
<button type="button" data-view="tasks" aria-current="page">任务</button>
<button type="button" data-view="queue">队列</button>
<button type="button" data-view="review">核对</button>
<button type="button" data-view="sites">站点</button>
<button type="button" data-view="library">库</button>
<button type="button" data-view="log">日志</button>
</nav>
<main>
<p id="status" role="status">正在读取</p>
<div id="list"></div>
<section id="note" hidden></section>
<section id="detail" hidden>
<button type="button" id="back">返回列表</button>
<h2 id="detail-title"></h2>
<dl id="detail-fields"></dl>
</section>
</main>
<script src="/app.js"></script>
</body></html>`

// Handler is the local application API. Request bodies are not written to the log.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /{$}", a.home)
	mux.HandleFunc("GET /app.css", a.asset("text/css; charset=utf-8", appCSS))
	mux.HandleFunc("GET /app.js", a.asset("text/javascript; charset=utf-8", appJS))
	mux.HandleFunc("GET /v1/jobs", a.jobs)
	mux.HandleFunc("POST /v1/archive/import", a.importArchive)
	mux.HandleFunc("POST /v1/archive/project", a.projectArchive)
	mux.HandleFunc("POST /v1/archive/verify", a.verifyArchive)
	mux.HandleFunc("POST /v1/archive/rollback", a.rollbackArchive)
	mux.HandleFunc("GET /v1/archive/record", a.archiveRecord)
	mux.HandleFunc("POST /v1/search", a.startSearch)
	mux.HandleFunc("POST /v1/tokens/recheck", a.recheckToken)
	mux.HandleFunc("GET /v1/jobs/{id}", a.job)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; connect-src 'self'; img-src 'none'; frame-ancestors 'none'; base-uri 'none'")
		if r.URL.Path != "/healthz" && !a.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="MusicOps", charset="UTF-8"`)
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "code": "AUTH_REQUIRED"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *App) authorized(r *http.Request) bool {
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Basic ") {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, "Basic "))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(raw, []byte("musicops:"+a.password)) == 1
}

func (a *App) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "engine": "pebble", "application": "musicops", "python": false, "data_schema": dataengine.SchemaVersion})
}

func (a *App) home(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, page)
}

func (a *App) asset(contentType, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}
}

func (a *App) jobs(w http.ResponseWriter, r *http.Request) {
	var after []byte
	if raw := r.URL.Query().Get("after"); raw != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || len(decoded) > 4096 || !strings.HasPrefix(string(decoded), "workflow-root/") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
			return
		}
		after = decoded
	}
	page, err := a.flows.List(after, 100)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "JOBS_UNAVAILABLE"})
		return
	}
	jobs := make([]jobView, 0, len(page.Jobs))
	for _, job := range page.Jobs {
		jobs = append(jobs, publicJob(job))
	}
	next := ""
	if len(page.Next) > 0 {
		next = base64.RawURLEncoding.EncodeToString(page.Next)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "jobs": jobs, "next": next, "downloaded": false, "production_verified": false, "python": false,
	})
}

func (a *App) importArchive(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source string `json:"source"`
	}
	if err := decode(r, &body); err != nil || a.allowSource(body.Source) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	report, err := sqlitemigrate.Import(a.engine, body.Source)
	writeMigration(w, report, err)
}

func (a *App) projectArchive(w http.ResponseWriter, r *http.Request) {
	if err := decode(r, &struct{}{}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	report, err := sqlitemigrate.Project(a.engine, 0)
	if err != nil {
		writeMigration(w, sqlitemigrate.Report{}, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "report": report, "live_profile": false})
}

func (a *App) verifyArchive(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source string `json:"source"`
	}
	if err := decode(r, &body); err != nil || a.allowSource(body.Source) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	report, err := sqlitemigrate.Verify(a.engine, body.Source)
	writeMigration(w, report, err)
}

func (a *App) rollbackArchive(w http.ResponseWriter, r *http.Request) {
	if err := decode(r, &struct{}{}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	deleted, err := sqlitemigrate.Rollback(a.engine)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "ARCHIVE_CONFLICT"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": deleted})
}

func (a *App) archiveRecord(w http.ResponseWriter, r *http.Request) {
	record, found, err := sqlitemigrate.Get(a.engine, r.URL.Query().Get("table"), r.URL.Query().Get("pk"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "found": found, "record": record})
}

func (a *App) startSearch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		JobID              string   `json:"job_id"`
		BatchID            string   `json:"batch_id"`
		Scene              string   `json:"scene"`
		Query              string   `json:"query"`
		ExpectedTitle      string   `json:"expected_title"`
		SourceTrackIDs     []string `json:"source_track_ids"`
		EnabledSites       []string `json:"enabled_sites"`
		StreamingEnabled   bool     `json:"streaming_enabled"`
		Synthetic          bool     `json:"synthetic"`
		BaseURL            string   `json:"base_url"`
		Cookie             string   `json:"cookie"`
		MatchedCount       uint64   `json:"matched_count"`
		FreeConfirmed      bool     `json:"free_confirmed"`
		TokenCost          int      `json:"token_cost"`
		BalanceKnown       bool     `json:"balance_known"`
		Balance            int      `json:"balance"`
		Budget             int      `json:"budget"`
		SafetyMargin       int      `json:"safety_margin"`
		DicmusicAuthKey    string   `json:"dicmusic_authkey"`
		OrpheusTorrentPass string   `json:"orpheus_torrent_pass"`
		Identity           Identity `json:"identity"`
	}
	if err := decode(r, &body); err != nil || strings.ContainsAny(body.Cookie+body.DicmusicAuthKey+body.OrpheusTorrentPass, "\r\n") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	if body.Identity.IdentityConfirmed && (body.Identity.Kind == "ai" || body.Identity.Kind == "synthetic_fixture" || !body.Identity.TracklistVerified) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "IDENTITY_EVIDENCE_REJECTED"})
		return
	}
	// Caller flags are accepted so an older client is not rejected.
	// They are not stored. A page and the account document decide the download.
	body.Balance = 0
	body.BalanceKnown = false
	body.FreeConfirmed = false
	body.TokenCost = 0
	plan := SearchPlan{
		RootJobID: body.JobID, Query: body.Query, ExpectedTitle: body.ExpectedTitle, EnabledSites: body.EnabledSites,
		StreamingEnabled: body.StreamingEnabled, Synthetic: body.Synthetic, BaseURL: body.BaseURL, Identity: body.Identity,
		Budget: body.Budget, SafetyMargin: body.SafetyMargin,
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	matched := body.MatchedCount
	if body.Scene == "playlist" && matched == 0 {
		matched = uint64(len(body.SourceTrackIDs))
	}
	a.remember(body.JobID, body.Cookie)
	a.rememberDownload(body.JobID, "dicmusic", body.DicmusicAuthKey)
	a.rememberDownload(body.JobID, "orpheus", body.OrpheusTorrentPass)
	_, err = a.flows.Start("start-"+body.JobID, workflow.Start{
		JobID: body.JobID, BatchID: body.BatchID, Scene: body.Scene, Stage: "identifying", Label: body.ExpectedTitle,
		SourceTrackIDs: body.SourceTrackIDs, MatchedCount: matched, Payload: raw, Evidence: []workflow.EvidenceInput{plan.evidence()},
		NowNS: time.Now().UnixNano(),
	})
	if err != nil {
		a.forget(body.JobID)
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "WORKFLOW_REJECTED"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "job_id": body.JobID, "downloaded": false, "production_verified": false, "python": false})
}

func (a *App) job(w http.ResponseWriter, r *http.Request) {
	inspection, err := a.flows.Inspect(r.PathValue("id"))
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, workflow.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]any{"ok": false, "code": "NOT_FOUND"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "inspection": scrubInspection(inspection), "downloaded": false, "production_verified": false})
}

func (a *App) allowSource(path string) error {
	if path == "" || !strings.EqualFold(filepath.Ext(path), ".sqlite") {
		return dataengine.ErrInvalidRequest
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return dataengine.ErrInvalidRequest
	}
	for _, root := range []string{a.Dir, a.Backup} {
		rel, err := filepath.Rel(root, abs)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return dataengine.ErrInvalidRequest
		}
	}
	return nil
}

func writeMigration(w http.ResponseWriter, report sqlitemigrate.Report, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "report": report})
		return
	}
	code, status := "DATA_OPERATION_FAILED", http.StatusInternalServerError
	switch {
	case errors.Is(err, dataengine.ErrInvalidRequest):
		code, status = "INVALID_REQUEST", http.StatusBadRequest
	case errors.Is(err, sqlitemigrate.ErrSchema):
		code, status = "SCHEMA_REJECTED", http.StatusBadRequest
	case errors.Is(err, sqlitemigrate.ErrConflict):
		code, status = "ARCHIVE_CONFLICT", http.StatusConflict
	case errors.Is(err, sqlitemigrate.ErrSourceChanged):
		code, status = "SOURCE_CHANGED", http.StatusConflict
	}
	writeJSON(w, status, map[string]any{"ok": false, "code": code})
}

func decode(r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
