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
<html lang="zh-CN"><head><meta charset="utf-8"><title>MusicOps</title>
<style>body{font-family:sans-serif;max-width:42rem;margin:2rem auto;line-height:1.5}code{background:#f3f3f3;padding:0 .2rem}</style>
</head><body>
<h1>MusicOps</h1>
<p>这是 Go 应用进程。它使用 Pebble 保存状态，并在本进程内调度只读检索和 SQLite 档案导入。此进程不启动 Python。</p>
<p>检索命中后，只有调用方已经确认免费，或令牌账本预留成立，才会保存种子字节。保存种子不是专辑下载，也不是入库。流媒体获取和音乐库发布还没有执行器。</p>
<p>健康检查：<a href="/healthz"><code>/healthz</code></a></p>
</body></html>`

// Handler is the local application API. Request bodies are not written to the log.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /{$}", a.home)
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
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; img-src 'none'; frame-ancestors 'none'; base-uri 'none'")
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
	// balance and balance_known are accepted so an older client is not rejected.
	// They are not stored and cannot authorize a token request.
	body.Balance = 0
	body.BalanceKnown = false
	plan := SearchPlan{
		RootJobID: body.JobID, Query: body.Query, ExpectedTitle: body.ExpectedTitle, EnabledSites: body.EnabledSites,
		StreamingEnabled: body.StreamingEnabled, Synthetic: body.Synthetic, BaseURL: body.BaseURL, Identity: body.Identity,
		FreeConfirmed: body.FreeConfirmed, TokenCost: body.TokenCost, Budget: body.Budget, SafetyMargin: body.SafetyMargin,
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "inspection": inspection, "downloaded": false, "production_verified": false})
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
