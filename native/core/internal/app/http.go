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
	"sort"
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
<html lang="zh-CN" data-type="software"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark"><meta name="theme-color" content="#2457b8"><meta name="page-type" content="software"><meta name="site-kind" content="software">
<title>MusicOps</title><link rel="icon" type="image/png" sizes="1024x1024" href="/brand-icon.png"><link rel="apple-touch-icon" sizes="1024x1024" href="/apple-touch-icon.png"><link rel="manifest" href="/app.webmanifest"><link rel="stylesheet" href="/app.css"></head>
<body>
<a class="skip" href="#main">跳到主要内容</a>
<div class="app">
<header class="app-bar">
<p class="mark"><img class="brand-icon" alt="" width="24" height="24" src="/brand-icon.png"><span class="brand-name">MusicOps</span><span class="brand-sub">本地音乐流水线</span><span class="app-version">{{version}}</span></p>
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
	mux.HandleFunc("GET /brand-icon.png", appIconAsset.serve)
	mux.HandleFunc("GET /apple-touch-icon.png", appIconAsset.serve)
	mux.HandleFunc("GET /favicon.ico", appIconAsset.serve)
	mux.HandleFunc("GET /app.webmanifest", appManifestAsset.serve)
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
	mux.HandleFunc("POST /v1/library/scrape/whole", a.runLibraryScrapeHandler)
	mux.HandleFunc("POST /v1/library/upgrade/whole", a.runLibraryUpgradeHandler)
	mux.HandleFunc("POST /v1/library/expand/whole", a.runLibraryExpansionHandler)
	mux.HandleFunc("GET /v1/library/automation", a.libraryAutomationHandler)
	mux.HandleFunc("POST /v1/library/automation", a.saveLibraryAutomationHandler)
	mux.HandleFunc("POST /v1/library/automation/run", a.runLibraryAutomationHandler)
	mux.HandleFunc("POST /v1/library/automation/stop", a.stopLibraryAutomationHandler)
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
	mux.HandleFunc("GET /v1/settings/proxy", a.outboundProxySettingsHandler)
	mux.HandleFunc("POST /v1/settings/proxy", a.saveOutboundProxy)
	mux.HandleFunc("POST /v1/settings/proxy/test", a.testOutboundProxy)
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
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; connect-src 'self'; img-src 'self' data:; manifest-src 'self'; frame-ancestors 'none'; base-uri 'none'")
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
	body := map[string]any{"ok": false, "engine": "pebble", "application": "musicops", "python": false, "data_schema": dataengine.SchemaVersion}
	if a == nil || a.engine == nil {
		body["error"] = "native store unavailable"
		writeJSON(w, http.StatusServiceUnavailable, body)
		return
	}
	// Docker and the desktop must not keep a wedged process alive merely
	// because its HTTP loop can answer. Read the schema through the same
	// engine every request uses, then include scheduler liveness.
	err := a.engine.Health()
	if err != nil {
		body["error"] = "native store read failed"
		writeJSON(w, http.StatusServiceUnavailable, body)
		return
	}
	if a.runner != nil {
		health := a.runner.Health()
		body["scheduler_active"] = len(health.Active)
		body["scheduler_errors"] = len(health.Errors)
		body["scheduler_stopping"] = health.Stopping
		if health.Stopping {
			body["error"] = "scheduler is stopping"
			writeJSON(w, http.StatusServiceUnavailable, body)
			return
		}
	}
	body["auto_review"] = a.autoReviewHealth()
	body["ok"] = true
	writeJSON(w, http.StatusOK, body)
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
	cssAsset         = newStaticAsset("text/css; charset=utf-8", appCSS)
	jsAsset          = newStaticAsset("text/javascript; charset=utf-8", appJS)
	appIconAsset     = newStaticAssetBytes("image/png", appIconPNG)
	appManifestAsset = newStaticAsset("application/manifest+json; charset=utf-8", appWebManifest)
)

func newStaticAsset(contentType, body string) *staticAsset {
	return newStaticAssetBytes(contentType, []byte(body))
}

func newStaticAssetBytes(contentType string, body []byte) *staticAsset {
	sum := sha256.Sum256(body)
	fingerprint := hex.EncodeToString(sum[:8])
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write(body)
	_ = writer.Close()
	return &staticAsset{contentType: contentType, etag: `"` + fingerprint + `"`, body: body, gzipped: compressed.Bytes()}
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

// jobCounts rolls up the entire catalogue by root, including pending
// children. "running" means a root has a task currently claimed by this
// scheduler; active workflow stages that are waiting for a future retry or a
// free lane are counted separately as "queued". This distinction prevents a
// large retry backlog from looking like hundreds of concurrent workers.
func (a *App) workflowJobsSnapshot() ([]workflow.Job, error) {
	jobs := []workflow.Job{}
	var cursor []byte
	for {
		page, err := a.flows.List(cursor, 200)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, page.Jobs...)
		if len(page.Next) == 0 {
			break
		}
		cursor = page.Next
	}
	return jobs, nil
}

func (a *App) jobCounts() (map[string]int, error) {
	jobs, err := a.workflowJobsSnapshot()
	if err != nil {
		return nil, err
	}
	active := map[string]bool{}
	if a.runner != nil {
		for taskID := range a.runner.Health().Active {
			active[taskID] = true
		}
	}
	failed, err := a.failedTaskIDs()
	if err != nil {
		return nil, err
	}
	return classifyJobCounts(jobs, active, failed), nil
}

// failedTaskIDs is deliberately limited to the failure indexes. A multi-track
// workflow can complete its root after one child task failed; walking every
// task on each counts refresh would be wasteful, while the few failed rows are
// exactly the terminal state needed to keep the root counters honest.
func (a *App) failedTaskIDs() (map[string]bool, error) {
	failed := map[string]bool{}
	if a == nil || a.tasks == nil {
		return failed, nil
	}
	for _, lane := range []string{"identify", "publish", "transcode", "aux", "download"} {
		var after []byte
		for {
			page, err := a.tasks.List(lane, "failed", after, 200)
			if err != nil {
				return nil, err
			}
			for _, task := range page.Tasks {
				failed[task.ID] = true
			}
			if len(page.Next) == 0 {
				break
			}
			after = page.Next
		}
	}
	return failed, nil
}

// reviewWorkflowJobs returns root records whose durable family contains a
// needs_review child. The counter already classifies roots this way, so a
// direct stage filter in the list endpoint made the count and the visible
// review list disagree.
func (a *App) reviewWorkflowJobs(after []byte, limit int) ([]jobView, string, error) {
	if limit < 1 {
		limit = 100
	}
	jobs, err := a.workflowJobsSnapshot()
	if err != nil {
		return nil, "", err
	}
	roots := map[string]*workflow.Job{}
	reviews := map[string]workflow.Job{}
	order := []string{}
	for i := range jobs {
		job := &jobs[i]
		root := job.RootID
		if root == "" {
			root = job.ID
		}
		if _, ok := roots[root]; !ok {
			roots[root] = nil
			order = append(order, root)
		}
		if job.ID == root {
			roots[root] = job
		}
		if job.Stage == "needs_review" {
			if current, ok := reviews[root]; !ok || job.UpdatedNS >= current.UpdatedNS {
				reviews[root] = *job
			}
		}
	}
	out := make([]jobView, 0, limit)
	next := ""
	for _, root := range order {
		review, ok := reviews[root]
		if !ok {
			continue
		}
		rootJob := roots[root]
		if rootJob == nil {
			rootJob = &review
		}
		cursor := workflow.ListCursor(*rootJob)
		if len(after) > 0 && bytes.Compare(cursor, after) <= 0 {
			continue
		}
		family, err := a.flows.Family(root)
		if err != nil {
			return nil, "", err
		}
		familyViews := make([]jobView, 0, len(family))
		reviewJobIDs := []string{}
		for _, job := range family {
			entry := a.publicJobWithResult(job)
			eligible := a.aiReviewEligible(job)
			entry.AIReviewEligible = eligible
			if eligible {
				reviewJobIDs = append(reviewJobIDs, job.ID)
			}
			if task, taskErr := a.tasks.Get(job.ActiveTaskID); taskErr == nil && task != nil {
				entry.ExecutionState = task.State
			}
			familyViews = append(familyViews, entry)
		}
		familyViews = presentJobs(familyViews)
		published, publishErr := audiorelease.PublishedRoots(a.engine, []string{root})
		if publishErr != nil {
			return nil, "", publishErr
		}
		familyViews = markReleased(familyViews, published)
		summary := summarizeRootJobs(familyViews)
		entry := a.publicJobWithResult(*rootJob)
		entry.RootStatus = &summary
		entry.Stage = summary.Stage
		entry.ExecutionState = summary.ExecutionState
		entry.Progress = summary.Progress
		entry.Headline = summary.Headline
		entry.Message = summary.Message
		entry.ReviewKind = summary.ReviewKind
		entry.ErrorClass = summary.ErrorClass
		entry.Paused = summary.Paused
		entry.CancelRequested = summary.CancelRequested
		entry.Downloaded = summary.Downloaded
		entry.LibraryPublished = summary.LibraryPublished
		if summary.Result != nil {
			entry.Result = summary.Result
		}
		entry.ReviewJobIDs = reviewJobIDs
		entry.AIReviewEligible = len(reviewJobIDs) > 0
		out = append(out, entry)
		if len(out) >= limit {
			next = base64.RawURLEncoding.EncodeToString(workflow.ListCursor(*rootJob))
			break
		}
	}
	return out, next, nil
}

type rootJobState uint8

const (
	rootFinished rootJobState = iota
	rootFailed
	rootReview
	rootQueued
	rootRunning
)

func classifyJobCounts(jobs []workflow.Job, active, failedTasks map[string]bool) map[string]int {
	states := map[string]rootJobState{}
	for _, job := range jobs {
		root := job.RootID
		if root == "" {
			root = job.ID
		}
		state := rootFinished
		if job.Stage == "completed" && job.ActiveTaskID != "" && failedTasks[job.ActiveTaskID] {
			state = rootFailed
		}
		switch job.Stage {
		case "needs_review":
			state = rootReview
		case "failed":
			state = rootFailed
		default:
			if !liveWorkflowStage(job.Stage) {
				break
			}
			state = rootQueued
			if job.ActiveTaskID != "" && active[job.ActiveTaskID] {
				state = rootRunning
			}
		}
		if current, ok := states[root]; !ok || state > current {
			states[root] = state
		}
	}
	counts := map[string]int{"total": len(states), "running": 0, "executing": 0, "queued": 0, "review": 0, "failed": 0, "finished": 0}
	for _, state := range states {
		switch state {
		case rootReview:
			counts["review"]++
		case rootFailed:
			counts["failed"]++
		case rootRunning:
			counts["running"]++
			counts["executing"]++
		case rootQueued:
			counts["queued"]++
		default:
			counts["finished"]++
		}
	}
	return counts
}

func liveWorkflowStage(stage string) bool {
	switch stage {
	case "preparing", "identifying", "snapshotting", "metadata", "searching", "fallback", "requesting", "downloading", "validating", "publishing", "notifying":
		return true
	default:
		return false
	}
}

// liveWorkflowJobs reads the scheduler's small state indexes instead of
// walking every historical workflow. A root index is intentionally ordered by
// stable IDs, so a queued child can otherwise sit hundreds of pages away from
// the root row and make a live job look invisible in the desktop UI.
func (a *App) liveWorkflowJobs(limit int) ([]workflow.Job, error) {
	if a == nil || a.flows == nil || a.tasks == nil || limit <= 0 {
		return []workflow.Job{}, nil
	}
	seen := map[string]bool{}
	jobs := make([]workflow.Job, 0, limit)
	for _, lane := range []string{"identify", "publish", "transcode", "aux", "download"} {
		for _, state := range []string{"running", "recovery_required", "queued", "paused"} {
			var after []byte
			for {
				page, err := a.tasks.List(lane, state, after, 200)
				if err != nil {
					return nil, err
				}
				for _, task := range page.Tasks {
					if task.WorkflowJobID == "" || seen[task.WorkflowJobID] {
						continue
					}
					job, err := a.flows.Job(task.WorkflowJobID)
					if err != nil {
						return nil, err
					}
					if job == nil || !liveWorkflowStage(job.Stage) {
						continue
					}
					seen[job.ID] = true
					jobs = append(jobs, *job)
				}
				if len(page.Next) == 0 {
					break
				}
				after = page.Next
			}
		}
	}
	sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].UpdatedNS > jobs[j].UpdatedNS })
	if len(jobs) > limit {
		jobs = jobs[:limit]
	}
	return jobs, nil
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
	rootsOnly := r.URL.Query().Get("roots") == "1"
	liveOnly := r.URL.Query().Get("live") == "1"
	var counts map[string]int
	countsMode := r.URL.Query().Get("counts")
	if countsMode == "1" || countsMode == "only" {
		var err error
		counts, err = a.jobCounts()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "JOBS_UNAVAILABLE"})
			return
		}
	}
	if countsMode == "only" {
		activeCount := 0
		if a.runner != nil {
			activeCount = len(a.runner.Health().Active)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "counts": counts, "active_count": activeCount, "pipeline_paused": a.pipelinePaused(), "auto_review": a.autoReviewHealth()})
		return
	}

	collected := make([]workflow.Job, 0, 100)
	next := ""
	if reviewOnly {
		jobs, next, err := a.reviewWorkflowJobs(after, 100)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "JOBS_UNAVAILABLE"})
			return
		}
		activeCount := 0
		if a.runner != nil {
			activeCount = len(a.runner.Health().Active)
		}
		downloaded, published := false, false
		for _, job := range jobs {
			downloaded = downloaded || job.Downloaded
			published = published || job.LibraryPublished
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "jobs": jobs, "next": next, "counts": counts, "active_count": activeCount,
			"pipeline_paused": a.pipelinePaused(), "auto_review": a.autoReviewHealth(),
			"downloaded": downloaded, "library_published": published, "production_verified": false, "python": false,
		})
		return
	}
	if liveOnly && !reviewOnly && !rootsOnly {
		live, err := a.liveWorkflowJobs(100)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "JOBS_UNAVAILABLE"})
			return
		}
		collected = append(collected, live...)
	} else {
		// A filtered scan can advance across many pages before finding a match.
		// Avoid returning an empty page with a continuation cursor.
		for scanned := 0; scanned < 2000 || len(collected) == 0; {
			if r.Context().Err() != nil {
				return
			}
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
				if (!reviewOnly || job.Stage == "needs_review") && (!rootsOnly || job.ParentJobID == "") {
					collected = append(collected, job)
				}
			}
			if len(page.Next) == 0 {
				next = ""
				break
			}
			after = page.Next
			next = base64.RawURLEncoding.EncodeToString(page.Next)
			if (!reviewOnly && !rootsOnly) || len(collected) >= 100 {
				break
			}
		}
		if liveOnly {
			live, err := a.liveWorkflowJobs(100)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "JOBS_UNAVAILABLE"})
				return
			}
			seen := map[string]bool{}
			for _, job := range collected {
				seen[job.ID] = true
			}
			for _, job := range live {
				if !seen[job.ID] {
					collected = append(collected, job)
					seen[job.ID] = true
				}
			}
		}
	}
	jobs := make([]jobView, 0, len(collected))
	for _, job := range collected {
		entry := a.publicJobWithResult(job)
		entry.AIReviewEligible = a.aiReviewEligible(job)
		if task, err := a.tasks.Get(job.ActiveTaskID); err == nil && task != nil {
			entry.ExecutionState = task.State
		}
		jobs = append(jobs, entry)
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
	if rootsOnly {
		for i := range jobs {
			rootID := jobs[i].RootID
			if rootID == "" {
				rootID = jobs[i].ID
			}
			family, familyErr := a.flows.Family(rootID)
			if familyErr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "JOBS_UNAVAILABLE"})
				return
			}
			familyViews := make([]jobView, 0, len(family))
			for _, job := range family {
				entry := a.publicJobWithResult(job)
				entry.AIReviewEligible = a.aiReviewEligible(job)
				if task, taskErr := a.tasks.Get(job.ActiveTaskID); taskErr == nil && task != nil {
					entry.ExecutionState = task.State
				}
				familyViews = append(familyViews, entry)
			}
			familyViews = presentJobs(familyViews)
			familyViews = markReleased(familyViews, map[string]bool{rootID: albums[rootID]})
			summary := summarizeRootJobs(familyViews)
			jobs[i].RootStatus = &summary
			jobs[i].Stage = summary.Stage
			jobs[i].ExecutionState = summary.ExecutionState
			jobs[i].Progress = summary.Progress
			jobs[i].Headline = summary.Headline
			jobs[i].Message = summary.Message
			jobs[i].ErrorClass = summary.ErrorClass
			jobs[i].ReviewKind = summary.ReviewKind
			jobs[i].Paused = summary.Paused
			jobs[i].CancelRequested = summary.CancelRequested
			jobs[i].Downloaded = summary.Downloaded
			jobs[i].LibraryPublished = summary.LibraryPublished
			jobs[i].UpdatedNS = summary.UpdatedNS
			jobs[i].RetryAfterNS = summary.RetryAfterNS
			if summary.Result != nil {
				jobs[i].Result = summary.Result
			}
		}
	}
	if liveOnly || rootsOnly {
		sortJobsForLiveView(jobs)
	}
	downloaded, published := false, false
	for _, job := range jobs {
		downloaded = downloaded || job.Downloaded
		published = published || job.LibraryPublished
	}
	activeCount := 0
	if a.runner != nil {
		activeCount = len(a.runner.Health().Active)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "jobs": jobs, "next": next, "counts": counts, "active_count": activeCount, "pipeline_paused": a.pipelinePaused(), "auto_review": a.autoReviewHealth(), "downloaded": downloaded, "library_published": published, "production_verified": false, "python": false,
	})
}

// sortJobsForLiveView keeps the work an operator is waiting for at the top.
// Root IDs and durable cursors are stable-key ordered, so a newly active
// download could otherwise sit behind hundreds of older queued retries.
func sortJobsForLiveView(jobs []jobView) {
	priority := func(job jobView) int {
		if job.ExecutionState == "running" {
			return 0
		}
		switch job.Stage {
		case "requesting", "downloading", "validating", "publishing", "notifying":
			return 1
		case "fallback":
			return 2
		case "searching", "identifying", "snapshotting", "metadata", "preparing":
			return 3
		case "needs_review":
			return 4
		case "failed":
			return 5
		default:
			if job.ExecutionState == "queued" || job.ExecutionState == "paused" {
				return 3
			}
			return 6
		}
	}
	sort.SliceStable(jobs, func(i, j int) bool {
		left, right := priority(jobs[i]), priority(jobs[j])
		if left != right {
			return left < right
		}
		return jobs[i].UpdatedNS > jobs[j].UpdatedNS
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
	var selectedResult *jobResultView
	for _, job := range family {
		view := a.publicJobWithResult(job)
		if job.ID == inspection.Job.ID {
			selectedResult = view.Result
		}
		views = append(views, view)
	}
	views = presentJobs(views)
	albums, err := audiorelease.Albums(a.engine)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "JOBS_UNAVAILABLE"})
		return
	}
	views = markReleased(views, albums)
	summary := summarizeRootJobs(views)
	progress, headline := summary.Progress, summary.Headline
	downloaded, published := summary.Downloaded, summary.LibraryPublished
	if summary.JobID != inspection.Job.ID && summary.Result != nil {
		selectedResult = summary.Result
	}
	response := map[string]any{
		"ok": true, "inspection": scrubInspection(inspection), "summary": summary, "progress": progress, "headline": headline,
		"downloaded": downloaded, "library_published": published, "production_verified": false,
	}
	if selectedResult != nil {
		response["result"] = selectedResult
	}
	writeJSON(w, http.StatusOK, response)
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
	a.invalidatePlaylistLibraryCache()
	publishedPath, pathFound, pathErr := audiorelease.PathFor(a.engine, result.RequestID)
	if pathErr != nil || !pathFound {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE", "downloaded": false, "library_published": true, "production_verified": false})
		return
	}
	// The ingest is committed; now record it on the workflow and move the job
	// to publishing. The request id is deterministic per (root, track), so a
	// retried call replays the release and replays this record instead of
	// queuing a second publish task.
	receipt, err := json.Marshal(publishPayload{
		RootID: root, TrackID: strconv.Itoa(body.Track), RequestID: result.RequestID, Path: publishedPath,
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
		JobID         string           `json:"job_id"`
		RequestID     string           `json:"request_id"`
		SourceURL     string           `json:"source_url"`
		Artist        string           `json:"artist"`
		Album         string           `json:"album"`
		Title         string           `json:"title"`
		Track         int              `json:"track"`
		Receipts      []publishReceipt `json:"receipts"`
		MissingTracks []string         `json:"missing_tracks,omitempty"`
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
		if len(body.MissingTracks) > 1000 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "downloaded": false, "library_published": false, "production_verified": false})
			return
		}
		for _, missing := range body.MissingTracks {
			if strings.TrimSpace(missing) == "" || len(missing) > 4096 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "downloaded": false, "library_published": false, "production_verified": false})
				return
			}
		}
		for _, item := range body.Receipts {
			if item.RequestID == "" || item.SHA256 == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "downloaded": false, "library_published": false, "production_verified": false})
				return
			}
		}
		requestID, err := publishReceiptRequestID(root, body.Receipts, body.MissingTracks)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE", "downloaded": false, "library_published": false, "production_verified": false})
			return
		}
		payload.RequestID = requestID
		payload.Receipts = body.Receipts
		payload.MissingTracks = append([]string(nil), body.MissingTracks...)
		source = "local-library"
	}
	receipt, err := json.Marshal(payload)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE", "downloaded": false, "library_published": false, "production_verified": false})
		return
	}
	recorded, err := a.flows.RecordRelease(workflow.RecordReleaseInput{
		JobID: root, RequestID: "record-" + payload.RequestID,
		Evidence: workflow.EvidenceInput{ID: "pub-" + payload.RequestID, Kind: "catalog", Source: source, SubjectID: root, TracklistVerified: len(payload.MissingTracks) == 0, Payload: receipt},
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
