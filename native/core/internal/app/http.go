package app

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/sqlitemigrate"
	"github.com/wsdbb72/MusicOps/native/core/internal/version"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

const page = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>MusicOps</title><link rel="stylesheet" href="/app.css"></head>
<body>
<a class="skip" href="#main">跳到主要内容</a>
<div class="app">
<header class="app-bar">
<p class="mark"><img class="brand-icon" alt="" width="24" height="24" src="data:image/png;base64,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"><span class="brand-name">MusicOps</span><span class="brand-sub">本地音乐流水线</span><span class="app-version">{{version}}</span></p>
<nav id="nav" aria-label="功能">
<button type="button" data-view="tasks" aria-current="page">任务</button>
<button type="button" data-view="queue">队列</button>
<button type="button" data-view="review">核对</button>
<button type="button" data-view="search">搜索</button>
<button type="button" data-view="sites">站点</button>
<button type="button" data-view="settings">设置</button>
<button type="button" data-view="library">库</button>
<button type="button" data-view="logs">日志</button>
</nav>
</header>
<main id="main" tabindex="-1">
<div class="view-head"><h1>任务</h1></div>
<p id="status" role="status" aria-live="polite">正在读取</p>
<div id="list"></div>
<section id="note" hidden></section>
<section id="detail" hidden aria-labelledby="detail-title">
<button type="button" id="back">← 返回列表</button>
<h2 id="detail-title"></h2>
<dl id="detail-fields"></dl>
<div id="detail-actions">
<button type="button" id="pause-job">暂停</button>
<button type="button" id="resume-job">继续</button>
<button type="button" id="cancel-job" class="danger">取消</button>
<button type="button" id="retry-job">重试</button>
</div>
<div id="torrent-actions" hidden>
<button type="button" id="torrent-pause">暂停下载</button>
<button type="button" id="torrent-resume">继续下载</button>
<button type="button" id="torrent-recheck">重新校验</button>
<button type="button" id="torrent-delete" class="danger">删除种子（保留文件）</button>
</div>
<div id="review-box" class="review-box" hidden></div>
</section>
</main>
</div>
<script src="/app.js"></script>
</body></html>`

// Handler is the local application API. Request bodies are not written to the log.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /v1/openapi.json", a.openAPI)
	mux.HandleFunc("GET /v1/open/info", a.openInfo)
	mux.HandleFunc("POST /v1/open/playlists/parse", a.fetchPlaylist)
	mux.HandleFunc("POST /v1/open/search", a.startSearch)
	mux.HandleFunc("GET /v1/open/jobs/{id}", a.job)
	mux.HandleFunc("GET /v1/open/jobs/{id}/review", a.reviewData)
	mux.HandleFunc("POST /v1/open/jobs/{id}/review", a.reviewJob)
	mux.HandleFunc("POST /v1/open/jobs/{id}/download", a.downloadJob)
	mux.HandleFunc("POST /v1/open/jobs/{id}/download/control", a.controlDownload)
	mux.HandleFunc("POST /v1/open/catalog/identify", a.identifyCatalog)
	mux.HandleFunc("POST /v1/open/library/import", a.openImport)
	mux.HandleFunc("GET /v1/open/roon/status", a.roonStatus)
	mux.HandleFunc("POST /v1/open/roon/connect", a.roonConnect)
	mux.HandleFunc("POST /v1/open/roon/control", a.roonControl)
	mux.HandleFunc("POST /v1/open/roon/disconnect", a.roonDisconnect)
	mux.HandleFunc("GET /v1/open/library/tags", a.libraryTags)
	mux.HandleFunc("POST /v1/open/library/tags", a.saveLibraryTags)
	mux.HandleFunc("GET /{$}", a.home)
	mux.HandleFunc("GET /app.css", cssAsset.serve)
	mux.HandleFunc("GET /app.js", jsAsset.serve)
	mux.HandleFunc("GET /v1/jobs", a.jobs)
	mux.HandleFunc("POST /v1/archive/import", a.importArchive)
	mux.HandleFunc("POST /v1/archive/project", a.projectArchive)
	mux.HandleFunc("POST /v1/archive/verify", a.verifyArchive)
	mux.HandleFunc("POST /v1/archive/rollback", a.rollbackArchive)
	mux.HandleFunc("GET /v1/archive/record", a.archiveRecord)
	mux.HandleFunc("POST /v1/search", a.startSearch)
	mux.HandleFunc("POST /v1/catalog/identify", a.identifyCatalog)
	mux.HandleFunc("POST /v1/audio/release", a.releaseAudio)
	mux.HandleFunc("POST /v1/audio/publish", a.publishAudio)
	mux.HandleFunc("POST /v1/audio/incoming", a.uploadIncoming)
	mux.HandleFunc("POST /v1/library/transcode", a.transcodeEnqueue)
	mux.HandleFunc("GET /v1/library/transcode", a.transcodeList)
	mux.HandleFunc("POST /v1/library/transcode/batch", a.transcodeBatch)
	mux.HandleFunc("GET /v1/library/transcode/batch", a.transcodeBatchStatus)
	mux.HandleFunc("POST /v1/library/transcode/batch/control", a.transcodeBatchControl)
	mux.HandleFunc("POST /v1/library/transcode/restore", a.restoreRecycledWAV)
	mux.HandleFunc("GET /v1/settings/wav", a.wavSettingsHandler)
	mux.HandleFunc("POST /v1/settings/wav", a.saveWAVSettings)
	mux.HandleFunc("POST /v1/library/ncm/decrypt", a.decryptNCM)
	mux.HandleFunc("GET /v1/library/tags", a.libraryTags)
	mux.HandleFunc("POST /v1/library/tags", a.saveLibraryTags)
	mux.HandleFunc("GET /v1/library/lyrics", a.libraryLyrics)
	mux.HandleFunc("POST /v1/library/lyrics/backfill", a.startLyricsBackfill)
	mux.HandleFunc("GET /v1/library/lyrics/backfill", a.lyricsBackfillStatus)
	mux.HandleFunc("POST /v1/library/lyrics", a.saveLibraryLyrics)
	mux.HandleFunc("GET /v1/library/guards", a.guardsHandler)
	mux.HandleFunc("POST /v1/library/guards", a.saveGuardsHandler)
	mux.HandleFunc("GET /v1/library/duplicates", a.duplicateScan)
	mux.HandleFunc("POST /v1/library/duplicates/merge", a.duplicateMerge)
	mux.HandleFunc("POST /v1/library/duplicates/restore", a.duplicateRestore)
	mux.HandleFunc("GET /v1/library/artist-folders", a.artistFolderPlan)
	mux.HandleFunc("POST /v1/library/artist-folders", a.artistFolderRelocate)
	mux.HandleFunc("GET /v1/library", a.library)
	mux.HandleFunc("GET /v1/logs", a.logs)
	mux.HandleFunc("GET /v1/control", a.pipelineControl)
	mux.HandleFunc("POST /v1/control", a.pipelineControl)
	mux.HandleFunc("POST /v1/library/reindex", a.reindexLibrary)
	mux.HandleFunc("GET /v1/library/gaps", a.libraryGaps)
	mux.HandleFunc("POST /v1/library/workflow", a.libraryWorkflow)
	mux.HandleFunc("POST /v1/library/metadata", a.completeMetadata)
	mux.HandleFunc("POST /v1/library/scrape", a.scrapeLibrary)
	mux.HandleFunc("GET /v1/library/files", a.libraryFiles)
	mux.HandleFunc("GET /v1/library/compilations", a.compilationPreview)
	mux.HandleFunc("POST /v1/library/compilations/merge", a.compilationMerge)
	mux.HandleFunc("POST /v1/settings/qbittorrent/tags", a.unifyQBittorrentTags)
	mux.HandleFunc("POST /v1/settings/servers/roon/test", a.testRoon)
	mux.HandleFunc("GET /v1/roon/status", a.roonStatus)
	mux.HandleFunc("POST /v1/roon/connect", a.roonConnect)
	mux.HandleFunc("POST /v1/roon/control", a.roonControl)
	mux.HandleFunc("POST /v1/roon/disconnect", a.roonDisconnect)
	mux.HandleFunc("GET /v1/migration", a.migrationStatus)
	mux.HandleFunc("POST /v1/migration/rollback", a.migrationRollback)
	mux.HandleFunc("GET /v1/settings/updates", a.updateSettings)
	mux.HandleFunc("GET /v1/sites", a.sites)
	mux.HandleFunc("POST /v1/sites", a.saveSites)
	mux.HandleFunc("POST /v1/tokens/recheck", a.recheckToken)
	mux.HandleFunc("GET /v1/jobs/{id}", a.job)
	mux.HandleFunc("POST /v1/jobs/{id}/control", a.controlJob)
	mux.HandleFunc("GET /v1/jobs/{id}/review", a.reviewData)
	mux.HandleFunc("POST /v1/jobs/{id}/review", a.reviewJob)
	mux.HandleFunc("POST /v1/jobs/{id}/download", a.downloadJob)
	mux.HandleFunc("POST /v1/jobs/{id}/download/control", a.controlDownload)
	mux.HandleFunc("POST /v1/jobs/batch", a.batchJobs)
	mux.HandleFunc("POST /v1/sites/preflight", a.preflightSite)
	mux.HandleFunc("GET /v1/settings/download", a.downloadSettings)
	mux.HandleFunc("POST /v1/settings/download", a.saveDownload)
	mux.HandleFunc("GET /v1/settings/cookies", a.cookieSettingsHandler)
	mux.HandleFunc("GET /v1/settings/cookies/check", a.cookieCheck)
	mux.HandleFunc("POST /v1/settings/cookies", a.saveCookie)
	mux.HandleFunc("GET /v1/settings/ai", a.aiSettingsHandler)
	mux.HandleFunc("POST /v1/settings/ai", a.saveAI)
	mux.HandleFunc("POST /v1/settings/ai/test", a.testAI)
	mux.HandleFunc("GET /v1/settings/qbittorrent", a.qbtSettingsHandler)
	mux.HandleFunc("POST /v1/settings/qbittorrent", a.saveQBittorrent)
	mux.HandleFunc("POST /v1/settings/qbittorrent/test", a.testQBittorrent)
	mux.HandleFunc("GET /v1/settings/solver", a.solverSettingsHandler)
	mux.HandleFunc("POST /v1/settings/solver", a.saveSolver)
	mux.HandleFunc("POST /v1/settings/solver/test", a.testSolver)
	mux.HandleFunc("POST /v1/settings/streaming/test", a.testStreamingEngine)
	mux.HandleFunc("POST /v1/sources/lookup", a.lookupSources)
	mux.HandleFunc("GET /v1/settings/streaming/netease", a.neteaseCookieView)
	mux.HandleFunc("POST /v1/settings/streaming/netease", a.saveNeteaseCookie)
	mux.HandleFunc("GET /v1/settings/lyrics", a.lyricsSettingsHandler)
	mux.HandleFunc("POST /v1/settings/lyrics", a.saveLyrics)
	mux.HandleFunc("POST /v1/settings/lyrics/test", a.testLyrics)
	mux.HandleFunc("GET /v1/settings/discogs", a.discogsSettingsHandler)
	mux.HandleFunc("POST /v1/settings/discogs", a.saveDiscogs)
	mux.HandleFunc("POST /v1/settings/discogs/test", a.testDiscogs)
	mux.HandleFunc("GET /v1/settings/library", a.librarySettingsHandler)
	mux.HandleFunc("POST /v1/settings/library", a.saveLibrary)
	mux.HandleFunc("GET /v1/settings/servers", a.serverSettingsHandler)
	mux.HandleFunc("POST /v1/settings/servers/plex", a.savePlex)
	mux.HandleFunc("POST /v1/settings/servers/plex/test", a.testPlex)
	mux.HandleFunc("POST /v1/settings/servers/navidrome", a.saveNavidrome)
	mux.HandleFunc("POST /v1/settings/servers/navidrome/test", a.testNavidrome)
	mux.HandleFunc("GET /v1/playlists", a.listPlaylists)
	mux.HandleFunc("POST /v1/playlists", a.savePlaylist)
	mux.HandleFunc("POST /v1/playlists/locate", a.locatePlaylist)
	mux.HandleFunc("POST /v1/playlists/fetch", a.fetchPlaylist)
	mux.HandleFunc("GET /v1/playlists/tracks", a.playlistTracksView)
	mux.HandleFunc("POST /v1/playlists/enqueue", a.enqueuePlaylist)
	mux.HandleFunc("GET /v1/subscriptions", a.listSubscriptions)
	mux.HandleFunc("POST /v1/subscriptions", a.saveSubscription)
	mux.HandleFunc("POST /v1/subscriptions/check", a.checkSubscription)
	mux.HandleFunc("POST /v1/subscriptions/pause", a.pauseSubscription)
	mux.HandleFunc("POST /v1/subscriptions/delete", a.deleteSubscription)
	mux.HandleFunc("GET /v1/jobs/{id}/playlist", a.jobPlaylistHandler)
	mux.HandleFunc("POST /v1/jobs/{id}/playlist", a.saveJobPlaylist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; connect-src 'self'; img-src data:; frame-ancestors 'none'; base-uri 'none'")
		if r.URL.Path != "/healthz" && !a.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="MusicOps", charset="UTF-8"`)
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "code": "AUTH_REQUIRED"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *App) authorized(r *http.Request) bool {
	// Other software authenticates with a key instead of the UI password.
	// The key is only honoured when the host sets MUSICOPS_API_KEY and it is
	// long enough to be worth comparing; unset means the old Basic-only mode.
	if key := os.Getenv("MUSICOPS_API_KEY"); len(key) >= 16 {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-API-Key")), []byte(key)) == 1 {
			return true
		}
	}
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
	// The version is baked in at serve time: the page is a const, and the
	// build stamp only exists in the binary.
	_, _ = io.WriteString(w, strings.Replace(page, "{{version}}", "v"+version.Version, 1))
}

// staticAsset is one immutable, in-binary asset. The validator and the gzip
// body are computed once: the CSS and JS only change when the binary changes,
// so a client can revalidate with a 304 instead of downloading them again on
// every navigation (P2-1). The blanket "no-store" in the middleware stays for
// JSON and HTML; these handlers overwrite it.
type staticAsset struct {
	contentType string
	etag        string
	body        []byte
	gzipped     []byte
}

var (
	cssAsset = newStaticAsset("text/css; charset=utf-8", appCSS)
	jsAsset  = newStaticAsset("text/javascript; charset=utf-8", appJS)
)

func newStaticAsset(contentType, body string) *staticAsset {
	sum := sha256.Sum256([]byte(body))
	fingerprint := hex.EncodeToString(sum[:8])
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write([]byte(body))
	_ = writer.Close()
	return &staticAsset{contentType: contentType, etag: `"` + fingerprint + `"`, body: []byte(body), gzipped: compressed.Bytes()}
}

func (asset *staticAsset) serve(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	header.Set("ETag", asset.etag)
	header.Set("Cache-Control", "private, max-age=0, must-revalidate")
	header.Set("Vary", "Accept-Encoding")
	if etagMatches(r.Header.Get("If-None-Match"), asset.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	header.Set("Content-Type", asset.contentType)
	body := asset.body
	if len(asset.gzipped) < len(asset.body) && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		header.Set("Content-Encoding", "gzip")
		body = asset.gzipped
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// etagMatches implements the If-None-Match comparison for a strong validator:
// a comma-separated list, each entry optionally weak-prefixed.
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

// acceptsGzip honours an explicit q=0 refusal; a bare "gzip" or wildcard
// counts as acceptance.
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		name := strings.ToLower(strings.TrimSpace(fields[0]))
		if name != "gzip" && name != "*" {
			continue
		}
		quality := 1.0
		for _, param := range fields[1:] {
			param = strings.ToLower(strings.TrimSpace(param))
			if strings.HasPrefix(param, "q=") {
				if value, err := strconv.ParseFloat(strings.TrimPrefix(param, "q="), 64); err == nil {
					quality = value
				}
			}
		}
		if quality > 0 {
			return true
		}
	}
	return false
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
	// review=1 returns only the jobs a human has to look at. The page used to
	// fetch the newest 100 jobs and filter in the browser, so a queue longer
	// than one page silently hid its older half (P1-22). The scan stops on a
	// page boundary: the cursor always points at the next page, never at a
	// job inside the page just returned.
	reviewOnly := r.URL.Query().Get("review") == "1"
	collected := make([]workflow.Job, 0, 100)
	next := ""
	for scanned := 0; scanned < 2000; {
		page, err := a.flows.List(after, 100)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "JOBS_UNAVAILABLE"})
			return
		}
		if len(page.Jobs) == 0 {
			next = ""
			break
		}
		scanned += len(page.Jobs)
		for _, job := range page.Jobs {
			if !reviewOnly || job.Stage == "needs_review" {
				collected = append(collected, job)
			}
		}
		if len(page.Next) == 0 {
			next = ""
			break
		}
		after = page.Next
		next = base64.RawURLEncoding.EncodeToString(page.Next)
		if !reviewOnly || len(collected) >= 100 {
			break
		}
	}
	jobs := make([]jobView, 0, len(collected))
	for _, job := range collected {
		jobs = append(jobs, publicJob(job))
	}
	jobs = presentJobs(jobs)
	roots := make([]string, 0, len(jobs))
	for _, job := range jobs {
		root := job.RootID
		if root == "" {
			root = job.ID
		}
		roots = append(roots, root)
	}
	albums, err := audiorelease.PublishedRoots(a.engine, roots)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "JOBS_UNAVAILABLE"})
		return
	}
	jobs = markReleased(jobs, albums)
	downloaded, published := false, false
	for _, job := range jobs {
		downloaded = downloaded || job.Downloaded
		published = published || job.LibraryPublished
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "jobs": jobs, "next": next, "downloaded": downloaded, "library_published": published, "production_verified": false, "python": false,
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "found": found, "record": publicRecord(record)})
}

func (a *App) startSearch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		JobID              string   `json:"job_id"`
		BatchID            string   `json:"batch_id"`
		Scene              string   `json:"scene"`
		Query              string   `json:"query"`
		ExpectedTitle      string   `json:"expected_title"`
		ExpectedArtist     string   `json:"expected_artist"`
		SourceTrackIDs     []string `json:"source_track_ids"`
		EnabledSites       []string `json:"enabled_sites"`
		StreamingEnabled   *bool    `json:"streaming_enabled"`
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
		PlaylistID         string   `json:"playlist_id"`
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
	// Name the offending field instead of letting the repository turn a
	// malformed request into a bare 409 (P1-2). The page never sent a batch
	// id, so it defaulted to empty and every search from the UI was refused;
	// the job id is a stable grouping for a single search.
	if strings.TrimSpace(body.BatchID) == "" {
		body.BatchID = body.JobID
	}
	if !workflow.ValidID(body.JobID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "field": "job_id", "error": "job_id 缺失或格式不合法"})
		return
	}
	if !workflow.ValidID(body.BatchID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "field": "batch_id", "error": "batch_id 格式不合法"})
		return
	}
	if err := workflow.ValidateEvidenceInput(workflow.EvidenceInput{
		ID: body.Identity.ID, Kind: body.Identity.Kind, Source: body.Identity.Source, SubjectID: body.Identity.SubjectID,
		TracklistVerified: body.Identity.TracklistVerified, IdentityConfirmed: body.Identity.IdentityConfirmed,
		Payload: []byte(body.Identity.Payload),
	}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "field": "identity", "error": "身份证据不完整：evidence_id、kind、source 必填且格式合法"})
		return
	}
	// The search page used to offer "补全" while the entrance always started at
	// "identifying", which planStart refuses for scene complete. The repair
	// executor is not registered either, so the honest answer is a refusal
	// with a reason instead of a generic 409 or a job that can never move
	// (P1-1).
	if body.Scene == "complete" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "code": "SCENE_UNSUPPORTED", "error": "补全场景的修复执行器尚未接入，本次没有创建任务"})
		return
	}
	// Caller flags are accepted so an older client is not rejected.
	// They are not stored. A page and the account document decide the download.
	body.Balance = 0
	body.BalanceKnown = false
	body.FreeConfirmed = false
	body.TokenCost = 0
	// When the request names no sites at all, fall back to the saved site
	// defaults. An explicit empty list still means "no sites". A settings
	// read failure keeps the old behaviour instead of blocking the search;
	// the failure surfaces on GET /v1/sites.
	if body.EnabledSites == nil {
		if settings, err := a.loadSiteSettings(); err == nil {
			body.EnabledSites = settings.EnabledSites
		}
	}
	// Streaming follows the same rule: an explicit value wins, omitting it
	// falls back to the saved download default, and a settings read failure
	// keeps streaming off rather than blocking the search.
	streaming := false
	if body.StreamingEnabled != nil {
		streaming = *body.StreamingEnabled
	} else if settings, err := a.loadDownloadSettings(); err == nil {
		streaming = settings.StreamingEnabled
	}
	plan := SearchPlan{
		RootJobID: body.JobID, Scene: body.Scene, Query: body.Query, ExpectedTitle: body.ExpectedTitle, ExpectedArtist: body.ExpectedArtist, EnabledSites: body.EnabledSites,
		StreamingEnabled: streaming, Synthetic: body.Synthetic, BaseURL: body.BaseURL, Identity: body.Identity,
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
	if body.PlaylistID != "" {
		if err := a.attachPlaylistForSearch(body.JobID, body.PlaylistID); err != nil {
			a.forget(body.JobID)
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": err.Error()})
			return
		}
	}
	_, err = a.flows.Start("start-"+body.JobID, workflow.Start{
		JobID: body.JobID, BatchID: body.BatchID, Scene: body.Scene, Stage: "identifying", Label: body.ExpectedTitle,
		SourceTrackIDs: body.SourceTrackIDs, MatchedCount: matched, Payload: raw, Evidence: []workflow.EvidenceInput{plan.evidence()},
		NowNS: time.Now().UnixNano(),
	})
	if err != nil {
		a.forget(body.JobID)
		if body.PlaylistID != "" {
			_ = os.Remove(a.workPath("incoming", playlistFileName(body.JobID)))
		}
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
	family, err := a.flows.Family(inspection.Job.RootID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "JOBS_UNAVAILABLE"})
		return
	}
	views := make([]jobView, 0, len(family))
	for _, job := range family {
		views = append(views, publicJob(job))
	}
	views = presentJobs(views)
	albums, err := audiorelease.Albums(a.engine)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "JOBS_UNAVAILABLE"})
		return
	}
	views = markReleased(views, albums)
	progress, headline := "", ""
	downloaded, published := false, false
	for _, job := range views {
		if job.ID == inspection.Job.ID {
			progress, headline = job.Progress, job.Headline
			downloaded, published = job.Downloaded, job.LibraryPublished
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "inspection": scrubInspection(inspection), "progress": progress, "headline": headline,
		"downloaded": downloaded, "library_published": published, "production_verified": false,
	})
}

// controlJob pauses, resumes or cancels one workflow job. It only exposes
// actions the workflow repository already supports; retry has no workflow
// meaning yet (taskrepo retry applies to single tasks, not jobs), so it is
// rejected rather than invented.
func (a *App) controlJob(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string `json:"action"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	switch body.Action {
	case "pause", "resume", "cancel":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	id := r.PathValue("id")
	requestID := "control-" + id + "-" + body.Action + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	result, err := a.flows.Control(requestID, id, body.Action, time.Now().UnixNano())
	if err != nil {
		status, code := http.StatusInternalServerError, "JOBS_UNAVAILABLE"
		switch {
		case errors.Is(err, workflow.ErrNotFound):
			status, code = http.StatusNotFound, "NOT_FOUND"
		case errors.Is(err, dataengine.ErrInvalidRequest):
			status, code = http.StatusBadRequest, "INVALID_REQUEST"
		case errors.Is(err, workflow.ErrStale):
			status, code = http.StatusConflict, "WORKFLOW_REJECTED"
		}
		writeJSON(w, status, map[string]any{"ok": false, "code": code, "error": err.Error()})
		return
	}
	job := result.Job
	if job == nil {
		fresh, readErr := a.flows.Job(id)
		if readErr != nil || fresh == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "JOBS_UNAVAILABLE"})
			return
		}
		job = fresh
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": publicJob(*job)})
}

func (a *App) releaseAudio(w http.ResponseWriter, r *http.Request) {
	var body struct {
		JobID      string `json:"job_id"`
		Artist     string `json:"artist"`
		Album      string `json:"album"`
		Title      string `json:"title"`
		Track      int    `json:"track"`
		SourceName string `json:"source_name"`
		// Origin is optional: "manual" when a person placed the file by hand,
		// "local_import" otherwise (the default). Anything else is rejected.
		Origin string `json:"origin"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "downloaded": false, "library_published": false, "production_verified": false})
		return
	}
	job, err := a.flows.Job(body.JobID)
	if err != nil || job == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "code": "NOT_FOUND", "downloaded": false, "library_published": false, "production_verified": false})
		return
	}
	root := job.RootID
	if root == "" {
		root = job.ID
	}
	// The stage gate runs on the root job: the release advances the album,
	// not the child that happened to be named. RecordRelease re-checks the
	// same rules atomically, so a concurrent cancel still cannot slip in.
	rootJob := job
	if root != job.ID {
		rootJob, err = a.flows.Job(root)
		if err != nil || rootJob == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "code": "NOT_FOUND", "downloaded": false, "library_published": false, "production_verified": false})
			return
		}
	}
	if err := releaseAllowed(rootJob); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "WORKFLOW_REJECTED", "error": err.Error(), "stage": rootJob.Stage, "downloaded": false, "library_published": false, "production_verified": false})
		return
	}
	incoming, library := a.workPath("incoming"), a.libraryDir()
	if err = os.MkdirAll(incoming, 0o700); err != nil || os.MkdirAll(library, 0o700) != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE", "downloaded": false, "library_published": false, "production_verified": false})
		return
	}
	// One request id per (root, track): a second track for the same root is a
	// separate publish, not a replay of the first track's receipt.
	sourceName := body.SourceName
	artist, album, title := body.Artist, body.Album, body.Title
	var compared identityCompare
	var identified bool
	if strings.EqualFold(filepath.Ext(sourceName), ".wav") || strings.EqualFold(filepath.Ext(sourceName), ".flac") {
		var convErr error
		compared, convErr = a.compareIncomingIdentity(incoming, sourceName)
		if convErr != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "code": "AUDIO_REJECTED", "error": "入库识别没有读到音频标签", "downloaded": false, "library_published": false, "production_verified": false, "needs_review": true})
			return
		}
		identified = true
		sourceName = compared.PublishedName
		if artist == "" && album == "" && title == "" {
			if compared.After.NeedsReview {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
					"ok": false, "code": "NEEDS_REVIEW", "error": compared.After.Reason,
					"downloaded": false, "library_published": false, "production_verified": false,
					"needs_review": true, "identity_confirmed": false,
					"before": compared.Before, "after": compared.After,
				})
				return
			}
			artist, album, title = compared.After.Artist, compared.After.Album, compared.After.Title
		}
	}
	sum := sha256.Sum256([]byte("album-audio/" + root + "/track/" + strconv.Itoa(body.Track)))
	result, err := audiorelease.Release(r.Context(), a.engine, audiorelease.Request{
		RequestID: "audio-" + hex.EncodeToString(sum[:16]), RootID: root, Artist: artist, Album: album, Title: title,
		Track: body.Track, SourceName: sourceName, Origin: body.Origin, Incoming: incoming, Library: library, Decoder: audiorelease.SystemDecoder(),
	})
	if err != nil {
		status, code := http.StatusBadRequest, "INVALID_REQUEST"
		switch {
		case errors.Is(err, audiorelease.ErrMissing):
			status, code = http.StatusNotFound, "AUDIO_MISSING"
		case errors.Is(err, audiorelease.ErrRejected):
			status, code = http.StatusUnprocessableEntity, "AUDIO_REJECTED"
		case errors.Is(err, audiorelease.ErrTool):
			status, code = http.StatusUnprocessableEntity, "DECODER_MISSING"
		case errors.Is(err, audiorelease.ErrConflict):
			status, code = http.StatusConflict, "LIBRARY_CONFLICT"
		}
		writeJSON(w, status, map[string]any{
			"ok": false, "code": code, "class": result.Class, "downloaded": false, "library_published": false, "production_verified": false,
		})
		return
	}
	// The ingest is committed; now record it on the workflow and move the job
	// to publishing. The request id is deterministic per (root, track), so a
	// retried call replays the release and replays this record instead of
	// queuing a second publish task.
	receipt, err := json.Marshal(publishPayload{
		RootID: root, TrackID: strconv.Itoa(body.Track), RequestID: result.RequestID,
		SHA256: result.SHA256, Format: result.Format, Origin: result.Origin,
		SourceName: result.SourceName, DecodeState: result.DecodeState,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE", "downloaded": false, "library_published": false, "production_verified": false})
		return
	}
	recorded, err := a.flows.RecordRelease(workflow.RecordReleaseInput{
		JobID: root, RequestID: "record-" + result.RequestID,
		Evidence: workflow.EvidenceInput{ID: "rel-" + result.RequestID, Kind: "catalog", Source: "local-library", SubjectID: root, TracklistVerified: true, Payload: receipt},
		Payload:  receipt, NowNS: time.Now().UnixNano(),
	})
	if err != nil {
		// The file and the library record are already committed; only the
		// workflow step failed. Report it honestly with the receipt so the
		// caller can retry: a retry replays the release and records it.
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "code": "WORKFLOW_UNAVAILABLE", "error": "library ingest committed but the workflow record failed: " + err.Error(),
			"sha256": result.SHA256, "format": result.Format, "origin": result.Origin,
			"downloaded": false, "library_published": true, "production_verified": false,
		})
		return
	}
	stage := "publishing"
	if recorded.Job != nil {
		stage = recorded.Job.Stage
	}
	publishTask := ""
	if recorded.Task != nil {
		publishTask = recorded.Task.ID
	}
	response := map[string]any{
		"ok": true, "format": result.Format, "decode_state": result.DecodeState, "sha256": result.SHA256,
		"origin": result.Origin, "source_name": result.SourceName, "stage": stage, "publish_task_id": publishTask,
		"downloaded": result.Downloaded, "library_published": result.LibraryPublished, "production_verified": false,
		"identity_confirmed": false,
	}
	if identified {
		response["needs_review"] = compared.After.NeedsReview
		response["tag_source"] = compared.After.TagSource
		response["before"] = compared.Before
		response["after"] = compared.After
	}
	writeJSON(w, http.StatusOK, response)
}

// publishAudio queues a publish task for a root job. Two shapes are
// accepted, never both: a controlled-audio source (a loopback URL plus album
// names, organised into the library by audiopublish) or a list of already
// verified ingest receipts (the multi-track download path). The publish task
// re-verifies everything before the job completes; this endpoint only records
// the request, so library_published stays false for the controlled path
// until the task runs.
func (a *App) publishAudio(w http.ResponseWriter, r *http.Request) {
	var body struct {
		JobID     string           `json:"job_id"`
		RequestID string           `json:"request_id"`
		SourceURL string           `json:"source_url"`
		Artist    string           `json:"artist"`
		Album     string           `json:"album"`
		Title     string           `json:"title"`
		Track     int              `json:"track"`
		Receipts  []publishReceipt `json:"receipts"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "downloaded": false, "library_published": false, "production_verified": false})
		return
	}
	job, err := a.flows.Job(body.JobID)
	if err != nil || job == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "code": "NOT_FOUND", "downloaded": false, "library_published": false, "production_verified": false})
		return
	}
	root := job.RootID
	if root == "" {
		root = job.ID
	}
	rootJob := job
	if root != job.ID {
		rootJob, err = a.flows.Job(root)
		if err != nil || rootJob == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "code": "NOT_FOUND", "downloaded": false, "library_published": false, "production_verified": false})
			return
		}
	}
	if err := releaseAllowed(rootJob); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "WORKFLOW_REJECTED", "error": err.Error(), "stage": rootJob.Stage, "downloaded": false, "library_published": false, "production_verified": false})
		return
	}
	controlled := body.SourceURL != ""
	multi := len(body.Receipts) > 0
	if controlled == multi {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "downloaded": false, "library_published": false, "production_verified": false})
		return
	}
	var payload publishPayload
	payload.RootID = root
	source := "controlled-audio"
	if controlled {
		if body.Artist == "" || body.Album == "" || body.Title == "" || body.Track < 1 || body.Track > 999 || !validLoopbackURL(body.SourceURL) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "downloaded": false, "library_published": false, "production_verified": false})
			return
		}
		requestID := body.RequestID
		if requestID == "" {
			requestID = publishRequestID(root, "track", strconv.Itoa(body.Track))
		}
		if !validPublishID(requestID) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "downloaded": false, "library_published": false, "production_verified": false})
			return
		}
		payload.RequestID = requestID
		payload.SourceURL = body.SourceURL
		payload.Artist = body.Artist
		payload.Album = body.Album
		payload.Title = body.Title
		payload.Track = body.Track
	} else {
		for _, item := range body.Receipts {
			if item.RequestID == "" || item.SHA256 == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "downloaded": false, "library_published": false, "production_verified": false})
				return
			}
		}
		encoded, err := json.Marshal(body.Receipts)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE", "downloaded": false, "library_published": false, "production_verified": false})
			return
		}
		payload.RequestID = publishRequestID(root, "receipts", hex.EncodeToString(encoded)[:16])
		payload.Receipts = body.Receipts
		source = "local-library"
	}
	receipt, err := json.Marshal(payload)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE", "downloaded": false, "library_published": false, "production_verified": false})
		return
	}
	recorded, err := a.flows.RecordRelease(workflow.RecordReleaseInput{
		JobID: root, RequestID: "record-" + payload.RequestID,
		Evidence: workflow.EvidenceInput{ID: "pub-" + payload.RequestID, Kind: "catalog", Source: source, SubjectID: root, TracklistVerified: true, Payload: receipt},
		Payload:  receipt, NowNS: time.Now().UnixNano(),
	})
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "code": "WORKFLOW_UNAVAILABLE", "error": "publish record failed: " + err.Error(),
			"downloaded": false, "library_published": multi, "production_verified": false,
		})
		return
	}
	stage := "publishing"
	if recorded.Job != nil {
		stage = recorded.Job.Stage
	}
	publishTask := ""
	if recorded.Task != nil {
		publishTask = recorded.Task.ID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "stage": stage, "publish_task_id": publishTask, "request_id": payload.RequestID,
		"downloaded": false, "library_published": multi, "production_verified": false,
	})
}

// validLoopbackURL accepts only http://127.0.0.1:<port>/... URLs: the
// controlled-audio source must be a service on this machine, never a remote
// host. audiopublish re-checks the same rule before fetching.
func validLoopbackURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" {
		return false
	}
	return true
}

// validPublishID is the audiopublish request-id rule: 1-80 chars of letters,
// digits, hyphen and underscore.
func validPublishID(id string) bool {
	if len(id) < 1 || len(id) > 80 {
		return false
	}
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

// releaseAllowed rejects jobs whose stage cannot receive audio. A paused job
// or a job with a cancel requested never accepts audio. Terminal stages
// (failed, cancelled, skipped) and stages before identity is confirmed
// (preparing, identifying, snapshotting, metadata, searching) reject as
// well. needs_review is allowed: the review concerns the acquisition path,
// not the audio that was just verified, and the established release state is
// a completed root with a needs_review search child.
func releaseAllowed(job *workflow.Job) error {
	if job == nil {
		return errors.New("job not found")
	}
	if job.Paused || job.CancelRequested {
		return errors.New("job is paused or cancel requested")
	}
	switch job.Stage {
	case "needs_review", "completed", "requesting", "downloading", "validating", "publishing", "fallback", "notifying":
		return nil
	default:
		return errors.New("job stage " + job.Stage + " cannot receive audio")
	}
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
