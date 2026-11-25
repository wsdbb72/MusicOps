package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
	"github.com/wsdbb72/MusicOps/native/core/internal/musicbrainz"
)

var libraryAutomationSettingsKey = []byte("app/settings/library-automation")
var libraryAutomationScanKey = []byte("app/library/automation-scan-cache")
var libraryDuplicateCacheKey = []byte("app/library/duplicate-fingerprint-cache-v1")
var libraryAlbumAuditKey = []byte("app/library/album-audit-cache-v1")

type automationWorkflowPriorityKey struct{}

const (
	automationWorkflowPriority      = 50
	libraryAutomationInitialDelay   = 5 * time.Minute
	libraryAutomationMaxMerges      = 1000
	libraryAutomationMaxJobs        = 200
	libraryAutomationScanWorkers    = 8
	duplicateFingerprintWorkers     = 8
	libraryAutomationCheckpoint     = 20_000
	libraryAutomationScanVersion    = 4
	libraryAutomationStallTimeout   = 45 * time.Minute
	libraryAutomationScrapeSlice    = 45 * time.Minute
	libraryAutomationArtworkSlice   = 30 * time.Minute
	libraryAutomationDedupeSlice    = 15 * time.Minute
	libraryAutomationArtworkWorkers = 6
	libraryAutomationPolicyVersion  = 3
	libraryAutomationSkippedRetry   = 24 * time.Hour
	libraryAlbumAuditTTL            = 30 * 24 * time.Hour
	libraryAutomationAlbumChecks    = 200
	libraryArtworkValidationTTL     = 7 * 24 * time.Hour
	libraryArtworkRefreshTTL        = 30 * 24 * time.Hour
)

type libraryAutomationSettings struct {
	Enabled       bool  `json:"enabled"`
	Scrape        bool  `json:"scrape"`
	Dedupe        bool  `json:"dedupe"`
	Upgrade       bool  `json:"upgrade"`
	Expand        bool  `json:"expand"`
	IntervalHours int   `json:"interval_hours"`
	UpdatedNS     int64 `json:"updated_ns,omitempty"`
}

func defaultLibraryAutomationSettings() libraryAutomationSettings {
	return libraryAutomationSettings{Enabled: true, Scrape: true, Dedupe: true, Upgrade: true, Expand: true, IntervalHours: 6}
}

func (a *App) loadLibraryAutomationSettings() (libraryAutomationSettings, error) {
	out := defaultLibraryAutomationSettings()
	found, err := a.loadJSONSetting(libraryAutomationSettingsKey, &out)
	if err != nil {
		return libraryAutomationSettings{}, err
	}
	if !found {
		return defaultLibraryAutomationSettings(), nil
	}
	if out.IntervalHours < 1 || out.IntervalHours > 168 {
		out.IntervalHours = 6
	}
	return out, nil
}

func (a *App) loadLibraryAutomationScanCache(root string) libraryAutomationScanDocument {
	empty := libraryAutomationScanDocument{Version: libraryAutomationScanVersion, Root: filepath.Clean(root), Files: map[string]cachedLibraryScan{}}
	doc := libraryAutomationScanDocument{}
	found, err := a.loadJSONSetting(libraryAutomationScanKey, &doc)
	if err != nil || !found || doc.Files == nil || doc.Version != libraryAutomationScanVersion || !strings.EqualFold(filepath.Clean(doc.Root), empty.Root) {
		return empty
	}
	return doc
}

func (a *App) saveLibraryAutomationScanCache(doc libraryAutomationScanDocument) error {
	if doc.Files == nil {
		doc.Files = map[string]cachedLibraryScan{}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return a.casSaveJSON(libraryAutomationScanKey, raw)
}

func (a *App) loadLibraryDuplicateCache(root string) libraryAutomationScanDocument {
	empty := libraryAutomationScanDocument{Version: libraryAutomationScanVersion, Root: filepath.Clean(root), Files: map[string]cachedLibraryScan{}}
	doc := libraryAutomationScanDocument{}
	found, err := a.loadJSONSetting(libraryDuplicateCacheKey, &doc)
	if err == nil && found && doc.Files != nil && doc.Version == libraryAutomationScanVersion && strings.EqualFold(filepath.Clean(doc.Root), empty.Root) {
		return doc
	}
	// Migrate fingerprints from the old shared document once. Keeping dedupe
	// on its own key removes the scrape/dedupe last-writer-wins race.
	legacy := libraryAutomationScanDocument{}
	if found, err := a.loadJSONSetting(libraryAutomationScanKey, &legacy); err == nil && found &&
		legacy.Version >= 3 && strings.EqualFold(filepath.Clean(legacy.Root), empty.Root) {
		for key, entry := range legacy.Files {
			if entry.Duplicate != nil {
				empty.Files[key] = entry
			}
		}
	}
	return empty
}

func (a *App) saveLibraryDuplicateCache(doc libraryAutomationScanDocument) error {
	if doc.Files == nil {
		doc.Files = map[string]cachedLibraryScan{}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return a.casSaveJSON(libraryDuplicateCacheKey, raw)
}

func (a *App) loadLibraryAlbumAudit(root string) libraryAlbumAuditDocument {
	empty := libraryAlbumAuditDocument{Version: 1, Root: filepath.Clean(root), Albums: map[string]libraryAlbumAuditRecord{}}
	doc := libraryAlbumAuditDocument{}
	found, err := a.loadJSONSetting(libraryAlbumAuditKey, &doc)
	if err != nil || !found || doc.Version != 1 || doc.Albums == nil || !strings.EqualFold(filepath.Clean(doc.Root), empty.Root) {
		return empty
	}
	return doc
}

func (a *App) saveLibraryAlbumAudit(doc libraryAlbumAuditDocument) error {
	if doc.Albums == nil {
		doc.Albums = map[string]libraryAlbumAuditRecord{}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return a.casSaveJSON(libraryAlbumAuditKey, raw)
}

func (a *App) saveLibraryAutomationSettings(in libraryAutomationSettings) (libraryAutomationSettings, error) {
	if in.IntervalHours < 1 || in.IntervalHours > 168 {
		in.IntervalHours = 6
	}
	in.UpdatedNS = a.now().UnixNano()
	raw, err := json.Marshal(in)
	if err != nil {
		return libraryAutomationSettings{}, err
	}
	if err := a.casSaveJSON(libraryAutomationSettingsKey, raw); err != nil {
		return libraryAutomationSettings{}, err
	}
	return in, nil
}

type cachedLibraryScan struct {
	Size             int64                       `json:"size"`
	ModNS            int64                       `json:"mod_ns"`
	NeedsCatalog     bool                        `json:"needs_catalog"`
	NeedsArtwork     bool                        `json:"needs_artwork"`
	Codec            string                      `json:"codec,omitempty"`
	Duration         float64                     `json:"duration,omitempty"`
	Artist           string                      `json:"artist,omitempty"`
	AlbumArtist      string                      `json:"album_artist,omitempty"`
	Album            string                      `json:"album,omitempty"`
	Title            string                      `json:"title,omitempty"`
	ArtworkSize      int64                       `json:"artwork_size,omitempty"`
	ArtworkModNS     int64                       `json:"artwork_mod_ns,omitempty"`
	ArtworkCheckedNS int64                       `json:"artwork_checked_ns,omitempty"`
	ArtworkValid     bool                        `json:"artwork_valid,omitempty"`
	ArtworkRefresh   bool                        `json:"artwork_refresh,omitempty"`
	Duplicate        *cachedDuplicateFingerprint `json:"duplicate,omitempty"`
	DuplicateOnly    bool                        `json:"duplicate_only,omitempty"`
}

// cachedDuplicateFingerprint stores the expensive decode result for an
// unchanged file. It is persisted in the existing maintenance scan document,
// so repeated whole-library dedupe passes do not hash and decode the same
// candidates again.
type cachedDuplicateFingerprint struct {
	FileSize         int64   `json:"file_size,omitempty"`
	FileModNS        int64   `json:"file_mod_ns,omitempty"`
	FileSHA256       string  `json:"file_sha256,omitempty"`
	PCMSHA256        string  `json:"pcm_sha256,omitempty"`
	Codec            string  `json:"codec,omitempty"`
	SampleRate       uint32  `json:"sample_rate,omitempty"`
	Channels         int     `json:"channels,omitempty"`
	BitsPerSample    int     `json:"bits_per_sample,omitempty"`
	BitRate          int64   `json:"bit_rate,omitempty"`
	Duration         float64 `json:"duration,omitempty"`
	Lossless         bool    `json:"lossless,omitempty"`
	Valid            bool    `json:"valid,omitempty"`
	DecoderAvailable bool    `json:"decoder_available"`
	ProbeError       string  `json:"probe_error,omitempty"`
}

type libraryAutomationScanDocument struct {
	Version int                          `json:"version"`
	Root    string                       `json:"root"`
	Files   map[string]cachedLibraryScan `json:"files"`
}

type libraryAlbumAuditRecord struct {
	ExpectedTracks int    `json:"expected_tracks"`
	LocalFiles     int    `json:"local_files"`
	Expandable     bool   `json:"expandable"`
	Signature      string `json:"signature"`
	CheckedNS      int64  `json:"checked_ns"`
}

type libraryAlbumAuditDocument struct {
	Version int                                `json:"version"`
	Root    string                             `json:"root"`
	Albums  map[string]libraryAlbumAuditRecord `json:"albums"`
}

type libraryAutomationReport struct {
	Running             bool     `json:"running"`
	Phase               string   `json:"phase,omitempty"`
	Started             string   `json:"started,omitempty"`
	Finished            string   `json:"finished,omitempty"`
	LastProgress        string   `json:"last_progress,omitempty"`
	Current             string   `json:"current,omitempty"`
	Cancelled           bool     `json:"cancelled,omitempty"`
	IndexedAdded        int      `json:"indexed_added"`
	IndexedPruned       int      `json:"indexed_pruned"`
	IndexChecked        int      `json:"index_checked"`
	ScanTotal           int      `json:"scan_total"`
	ScanChecked         int      `json:"scan_checked"`
	ScanReused          int      `json:"scan_reused"`
	ScanFailed          int      `json:"scan_failed"`
	ScrapeCandidates    int      `json:"scrape_candidates"`
	ArtworkCandidates   int      `json:"artwork_candidates"`
	ScrapeProcessed     int      `json:"scrape_processed"`
	ScrapeSkipped       int      `json:"scrape_skipped"`
	ArtworkProcessed    int      `json:"artwork_processed"`
	ArtworkSkipped      int      `json:"artwork_skipped"`
	ScrapeWritten       int      `json:"scrape_written"`
	ArtworkWritten      int      `json:"artwork_written"`
	ArtworkFailed       int      `json:"artwork_failed"`
	Backlog             bool     `json:"backlog,omitempty"`
	ScrapeFailed        int      `json:"scrape_failed"`
	RetryAfterSec       int      `json:"retry_after_sec,omitempty"`
	DuplicateCandidates int      `json:"duplicate_candidates"`
	DuplicateChecked    int      `json:"duplicate_checked"`
	DuplicateGroups     int      `json:"duplicate_groups"`
	Deduplicated        int      `json:"deduplicated"`
	CacheReclaimedFiles int      `json:"cache_reclaimed_files"`
	CacheReclaimedBytes int64    `json:"cache_reclaimed_bytes"`
	CacheUsedBytes      int64    `json:"cache_used_bytes"`
	CacheLimitBytes     int64    `json:"cache_limit_bytes"`
	CacheOverLimit      bool     `json:"cache_over_limit,omitempty"`
	UpgradeJobs         int      `json:"upgrade_jobs"`
	ExpandJobs          int      `json:"expand_jobs"`
	Errors              []string `json:"errors,omitempty"`
	ErrorCount          int      `json:"error_count"`
	BlockedBy           string   `json:"blocked_by,omitempty"`
	BlockedReason       string   `json:"blocked_reason,omitempty"`
	Note                string   `json:"note,omitempty"`
}

type libraryAutomationState struct {
	mu           sync.Mutex
	running      bool
	report       libraryAutomationReport
	cancel       context.CancelFunc
	done         chan struct{}
	attempts     map[string]time.Time
	lastProgress time.Time
}

func (s *libraryAutomationState) snapshot() (bool, libraryAutomationReport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	report := s.report
	report.Running = s.running
	return s.running, report
}

func (s *libraryAutomationState) setPhase(phase string) {
	now := time.Now()
	s.mu.Lock()
	s.report.Phase = phase
	s.report.LastProgress = now.Format(time.RFC3339)
	s.lastProgress = now
	s.mu.Unlock()
}

func (s *libraryAutomationState) touch(phase, current string) {
	now := time.Now()
	s.mu.Lock()
	if phase != "" {
		s.report.Phase = phase
	}
	s.report.Current = scrub(current)
	s.report.LastProgress = now.Format(time.RFC3339)
	s.lastProgress = now
	s.mu.Unlock()
}

func (s *libraryAutomationState) addError(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.report.ErrorCount++
	message := scrub(err.Error())
	if len(s.report.Errors) < 20 {
		s.report.Errors = append(s.report.Errors, message)
	} else {
		copy(s.report.Errors, s.report.Errors[1:])
		s.report.Errors[len(s.report.Errors)-1] = message
	}
	s.mu.Unlock()
}

func (s *libraryAutomationState) update(fn func(*libraryAutomationReport)) {
	s.mu.Lock()
	fn(&s.report)
	s.mu.Unlock()
}

func (s *libraryAutomationState) claimAttempt(key string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempts == nil {
		s.attempts = map[string]time.Time{}
	}
	if last := s.attempts[key]; !last.IsZero() && now.Sub(last) < 24*time.Hour {
		return false
	}
	s.attempts[key] = now
	return true
}

func (s *libraryAutomationState) begin() (context.Context, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return nil, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.running = true
	s.cancel = cancel
	s.done = make(chan struct{})
	started := time.Now()
	s.lastProgress = started
	s.report = libraryAutomationReport{Running: true, Started: started.Format(time.RFC3339), LastProgress: started.Format(time.RFC3339), Phase: "queued"}
	return ctx, true
}

func (s *libraryAutomationState) finish(err error, note string) {
	s.mu.Lock()
	if errors.Is(err, context.Canceled) {
		s.report.Cancelled = true
	} else if err != nil && len(s.report.Errors) < 20 {
		s.report.Errors = append(s.report.Errors, scrub(err.Error()))
	}
	s.report.Running = false
	s.report.Finished = time.Now().Format(time.RFC3339)
	s.report.Note = note
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.done != nil {
		close(s.done)
		s.done = nil
	}
	s.running = false
	s.mu.Unlock()
}

func (s *libraryAutomationState) wait(ctx context.Context) bool {
	s.mu.Lock()
	done := s.done
	running := s.running
	s.mu.Unlock()
	if !running || done == nil {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-done:
		return true
	}
}

func (s *libraryAutomationState) stop() {
	s.mu.Lock()
	if s.running {
		s.report.Note = "正在停止；当前网络请求或文件操作结束后退出"
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
}

func (s *libraryAutomationState) stopIfStalled(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running || s.lastProgress.IsZero() || now.Sub(s.lastProgress) < libraryAutomationStallTimeout {
		return false
	}
	if len(s.report.Errors) < 20 {
		s.report.Errors = append(s.report.Errors, scrub(fmt.Sprintf("自动维护超过 %s 没有进度，已安全停止并保留已完成结果", libraryAutomationStallTimeout)))
	}
	if s.cancel != nil {
		s.cancel()
	}
	return true
}

func (a *App) runLibraryAutomationScheduler(stop <-chan struct{}) {
	nextRun := time.Now().Add(libraryAutomationInitialDelay)
	lastBacklogFinished := ""
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			if a.pipelinePaused() {
				continue
			}
			settings, err := a.loadLibraryAutomationSettings()
			if err != nil {
				nextRun = now.Add(5 * time.Minute)
				continue
			}
			delay := time.Duration(settings.IntervalHours) * time.Hour
			if delay <= 0 {
				delay = 6 * time.Hour
			}
			if !settings.Enabled {
				nextRun = now.Add(delay)
				continue
			}
			running, report := a.libraryAutomation.snapshot()
			if !running && report.Backlog && report.Finished != "" && report.Finished != lastBacklogFinished {
				delay := time.Minute
				if report.RetryAfterSec > int(delay/time.Second) {
					delay = time.Duration(report.RetryAfterSec) * time.Second
				}
				soon := now.Add(delay)
				if soon.Before(nextRun) {
					nextRun = soon
				}
				lastBacklogFinished = report.Finished
			}
			if now.Before(nextRun) {
				continue
			}
			if a.startLibraryAutomationRun("scheduled") {
				nextRun = now.Add(delay)
			} else {
				// A manual or long-running pass may still be active. Recheck soon after it ends.
				nextRun = now.Add(time.Minute)
			}
		}
	}
}

func (a *App) startLibraryAutomationRun(reason string) bool {
	if a == nil || a.libraryAutomation == nil {
		return false
	}
	settings, err := a.loadLibraryAutomationSettings()
	if err != nil {
		return false
	}
	return a.startLibraryAutomationRunWithSettings(reason, settings)
}

func (a *App) startLibraryScrapeRun(reason string) bool {
	settings, err := a.loadLibraryAutomationSettings()
	if err != nil {
		return false
	}
	settings.Scrape = true
	settings.Dedupe = false
	settings.Upgrade = false
	settings.Expand = false
	return a.startLibraryAutomationRunWithSettings(reason, settings)
}

func (a *App) startLibraryUpgradeRun(reason string) bool {
	settings, err := a.loadLibraryAutomationSettings()
	if err != nil {
		return false
	}
	settings.Scrape = false
	settings.Dedupe = false
	settings.Upgrade = true
	settings.Expand = false
	return a.startLibraryAutomationRunWithSettings(reason, settings)
}

func (a *App) startLibraryAutomationRunWithSettings(reason string, settings libraryAutomationSettings) bool {
	ctx, started := a.libraryAutomation.begin()
	if !started {
		return false
	}
	go a.watchLibraryAutomationRun(ctx)
	go func() {
		var runErr error
		defer func() {
			if recovered := recover(); recovered != nil {
				runErr = fmt.Errorf("自动维护内部异常: %v", recovered)
			}
			a.libraryAutomation.finish(runErr, "本轮自动维护由"+reason+"触发")
		}()
		runErr = a.runLibraryAutomationPass(ctx, settings)
	}()
	return true
}

func (a *App) watchLibraryAutomationRun(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if a.libraryAutomation.stopIfStalled(now) {
				return
			}
		}
	}
}

func (a *App) runLibraryAutomationPass(ctx context.Context, settings libraryAutomationSettings) error {
	state := a.libraryAutomation
	root := a.libraryDir()
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return fmt.Errorf("音乐库目录不可用")
	}
	// Persist a separately migrated dedupe cache before the metadata scan can
	// rewrite the legacy shared document under the new scan-cache version.
	if duplicates := a.loadLibraryDuplicateCache(root); len(duplicates.Files) > 0 {
		if err := a.saveLibraryDuplicateCache(duplicates); err != nil {
			state.addError(fmt.Errorf("迁移去重指纹缓存失败: %w", err))
		}
	}

	state.setPhase("reindex")
	state.touch("reindex", "正在读取现有索引")
	added, _, pruned, err := a.indexLibraryTreeWithProgress(ctx, root, func(checked int, current string) {
		state.update(func(report *libraryAutomationReport) { report.IndexChecked = checked })
		state.touch("reindex", current)
	})
	if err != nil {
		return fmt.Errorf("增量索引失败: %w", err)
	}
	state.update(func(report *libraryAutomationReport) {
		report.IndexedAdded = added
		report.IndexedPruned = pruned
	})

	paths, byDir, err := listLibraryAudio(root)
	if err != nil {
		return err
	}
	state.setPhase("cache")
	state.touch("cache", "正在回收已入库的旧流媒体缓存")
	freedBytes, freedFiles, cacheErr := a.pruneAudioCacheAll(audioCacheStaleAfter, a.now())
	if cacheErr != nil {
		state.addError(fmt.Errorf("流媒体缓存回收失败: %w", cacheErr))
	}
	cacheUsed, cacheLimit, _, cacheUsageErr := a.audioCacheUsageSnapshot()
	if cacheUsageErr != nil {
		state.addError(fmt.Errorf("读取 SSD 缓存占用失败: %w", cacheUsageErr))
	}
	state.update(func(report *libraryAutomationReport) {
		report.CacheReclaimedBytes = freedBytes
		report.CacheReclaimedFiles = freedFiles
		report.CacheUsedBytes = cacheUsed
		report.CacheLimitBytes = cacheLimit
		report.CacheOverLimit = cacheLimit > 0 && cacheUsed > cacheLimit
	})
	if settings.Scrape {
		state.setPhase("scrape")
		a.runAutomatedScrape(ctx, root, paths, settings.Upgrade)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if settings.Dedupe {
		state.setPhase("dedupe")
		if a.runAutomatedDeduplication(ctx) {
			state.update(func(report *libraryAutomationReport) { report.Backlog = true })
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if settings.Upgrade || settings.Expand {
		state.setPhase("library_workflows")
		a.enqueueLibraryMaintenanceWorkflows(ctx, byDir, settings)
	}
	return ctx.Err()
}

func listLibraryAudio(root string) ([]string, map[string][]string, error) {
	paths := []string{}
	byDir := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if skipLibraryWalkError(root, path, walkErr) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
		if !audioExtensions[ext] || !entry.Type().IsRegular() {
			return nil
		}
		clean := filepath.Clean(path)
		paths = append(paths, clean)
		byDir[filepath.Dir(clean)] = append(byDir[filepath.Dir(clean)], clean)
		return nil
	})
	sort.Strings(paths)
	for dir := range byDir {
		sort.Strings(byDir[dir])
	}
	return paths, byDir, err
}

type libraryAutomationScanInput struct {
	path     string
	key      string
	cached   cachedLibraryScan
	hasCache bool
}

type libraryAutomationScanResult struct {
	input  libraryAutomationScanInput
	entry  cachedLibraryScan
	ok     bool
	reused bool
	err    error
}

func scanAutomationFile(ctx context.Context, input libraryAutomationScanInput) libraryAutomationScanResult {
	result := libraryAutomationScanResult{input: input}
	if ctx.Err() != nil {
		result.err = ctx.Err()
		return result
	}
	info, err := os.Stat(input.path)
	if err != nil {
		result.err = fmt.Errorf("%s: 文件状态读取失败: %w", filepath.Base(input.path), err)
		return result
	}
	cacheAudioMatch := input.hasCache && !input.cached.DuplicateOnly && input.cached.Size == info.Size() && input.cached.ModNS == info.ModTime().UnixNano()
	if cacheAudioMatch && cachedArtworkFresh(input.path, input.cached, time.Now()) {
		result.entry = input.cached
		result.ok = true
		result.reused = true
		return result
	}
	entry := cachedLibraryScan{Size: info.Size(), ModNS: info.ModTime().UnixNano()}
	if cacheAudioMatch {
		entry = input.cached
		entry.Size = info.Size()
		entry.ModNS = info.ModTime().UnixNano()
	} else if input.hasCache && input.cached.Duplicate != nil {
		entry.Duplicate = input.cached.Duplicate
	}
	refreshArtwork := cacheAudioMatch && artworkRefreshDue(input.cached, time.Now())
	if strings.EqualFold(filepath.Ext(input.path), ".flac") {
		tags, readErr := readFLACTagsPrefix(input.path)
		if readErr != nil {
			result.err = fmt.Errorf("%s: 标签读取失败: %w", filepath.Base(input.path), readErr)
			return result
		}
		entry.NeedsCatalog = maintenanceNeedsCatalogTags(tags)
		entry.NeedsArtwork = maintenanceMissingArtwork(input.path, tags)
		entry.Codec = "flac"
		if isLossyFLACProvenance(firstTagValue(tags.Raw, "COMMENT")) {
			entry.Codec = "flac-lossy"
		}
		entry.Duration, _ = flacDuration(tags.StreamInfo)
		entry.Artist = strings.TrimSpace(tags.Artist)
		entry.AlbumArtist = strings.TrimSpace(tags.AlbumArtist)
		entry.Album = strings.TrimSpace(tags.Album)
		entry.Title = strings.TrimSpace(tags.Title)
	} else {
		genericInfo, probeErr := probeGenericAudio(ctx, input.path)
		if probeErr != nil {
			result.err = fmt.Errorf("%s: 音频探测失败: %w", filepath.Base(input.path), probeErr)
			return result
		}
		entry.NeedsCatalog = maintenanceNeedsGenericTags(genericInfo)
		entry.NeedsArtwork = maintenanceMissingSidecars(input.path)
		entry.Codec = genericInfo.Codec
		entry.Duration = genericInfo.Duration
		entry.Artist = genericInfo.Artist
		entry.AlbumArtist = genericInfo.AlbumArtist
		entry.Album = genericInfo.Album
		entry.Title = genericInfo.Title
	}
	if refreshArtwork && !entry.NeedsArtwork {
		entry.NeedsArtwork = true
	}
	entry.ArtworkRefresh = refreshArtwork
	recordArtworkValidation(&entry, input.path, time.Now())
	result.entry = entry
	result.ok = true
	return result
}

func automationCacheKey(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	return filepath.ToSlash(rel)
}

func (a *App) runAutomatedScrape(ctx context.Context, root string, paths []string, repairInvalid bool) {
	state := a.libraryAutomation
	cache := a.loadLibraryAutomationScanCache(root)
	nextCache := libraryAutomationScanDocument{Version: libraryAutomationScanVersion, Root: filepath.Clean(root), Files: make(map[string]cachedLibraryScan, len(cache.Files))}
	for key, entry := range cache.Files {
		nextCache.Files[key] = entry
	}
	inputs := make([]libraryAutomationScanInput, 0, len(paths))
	for _, path := range paths {
		key := automationCacheKey(root, path)
		entry, ok := cache.Files[key]
		inputs = append(inputs, libraryAutomationScanInput{path: path, key: key, cached: entry, hasCache: ok})
	}
	state.update(func(report *libraryAutomationReport) {
		report.ScanTotal = len(inputs)
		report.ScanChecked = 0
		report.ScanReused = 0
		report.ScanFailed = 0
	})
	state.touch("scrape", "正在扫描音乐库")

	workers := libraryAutomationScanWorkers
	if runtime.GOMAXPROCS(0) < workers {
		workers = runtime.GOMAXPROCS(0)
	}
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan libraryAutomationScanInput)
	results := make(chan libraryAutomationScanResult, workers*2)
	var workersWG sync.WaitGroup
	for i := 0; i < workers; i++ {
		workersWG.Add(1)
		go func() {
			defer workersWG.Done()
			for input := range jobs {
				results <- scanAutomationFile(ctx, input)
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, input := range inputs {
			select {
			case <-ctx.Done():
				return
			case jobs <- input:
			}
		}
	}()
	go func() {
		workersWG.Wait()
		close(results)
	}()

	seen := make(map[string]bool, len(inputs))
	repairTracks := make([]localWorkflowTrack, 0)
	repairSeen := map[string]bool{}
	checked, reused, failed := 0, 0, 0
	for result := range results {
		checked++
		seen[result.input.key] = true
		if result.ok {
			nextCache.Files[result.input.key] = result.entry
			if result.reused {
				reused++
			}
		} else {
			delete(nextCache.Files, result.input.key)
			failed++
			state.addError(result.err)
			if repairInvalid && strings.EqualFold(filepath.Ext(result.input.path), ".flac") && !repairSeen[result.input.path] {
				if track, probeErr := probeLocalWorkflowTrackForRepair(ctx, result.input.path); probeErr == nil && isLossyUpgradeSource(track.Codec) {
					repairSeen[result.input.path] = true
					repairTracks = append(repairTracks, track)
				}
			}
		}
		if checked%100 == 0 || checked == len(inputs) {
			state.update(func(report *libraryAutomationReport) {
				report.ScanChecked = checked
				report.ScanReused = reused
				report.ScanFailed = failed
			})
			state.touch("scrape", result.input.path)
		}
		if checked%libraryAutomationCheckpoint == 0 {
			if err := a.saveLibraryAutomationScanCache(nextCache); err != nil {
				state.addError(fmt.Errorf("扫描缓存检查点保存失败: %w", err))
			}
		}
	}
	if checked == len(inputs) && ctx.Err() == nil {
		for key := range nextCache.Files {
			if !seen[key] {
				delete(nextCache.Files, key)
			}
		}
	}
	if err := a.saveLibraryAutomationScanCache(nextCache); err != nil {
		state.addError(fmt.Errorf("扫描缓存保存失败: %w", err))
	}
	if ctx.Err() != nil {
		return
	}
	for _, track := range repairTracks {
		// Old policy versions may have left a skipped/no-result job under the
		// same path. A versioned repair key creates exactly one new attempt for
		// genuinely corrupt audio while normal no-source tracks keep their
		// longer skipped-task cooldown.
		if a.enqueueMaintenanceWorkflowKey(ctx, "upgrade", []localWorkflowTrack{track}, filepath.Dir(track.Path), "corrupt-repair-v2") {
			state.update(func(report *libraryAutomationReport) { report.UpgradeJobs++ })
		}
	}

	catalogByDir := map[string][]string{}
	genericByDir := map[string][]string{}
	artworkDirect := []string{}
	artworkOverwrite := map[string]bool{}
	candidateCount := 0
	artworkCount := 0
	for _, input := range inputs {
		entry, ok := nextCache.Files[input.key]
		if !ok {
			continue
		}
		if entry.NeedsCatalog {
			if strings.EqualFold(filepath.Ext(input.path), ".flac") {
				catalogByDir[filepath.Dir(input.path)] = append(catalogByDir[filepath.Dir(input.path)], input.path)
			} else {
				genericByDir[filepath.Dir(input.path)] = append(genericByDir[filepath.Dir(input.path)], input.path)
			}
			candidateCount++
		}
		if entry.NeedsArtwork {
			artworkDirect = append(artworkDirect, input.path)
			artworkOverwrite[input.path] = entry.ArtworkRefresh
			artworkCount++
		}
	}
	state.update(func(report *libraryAutomationReport) {
		report.ScrapeCandidates = candidateCount
		report.ArtworkCandidates = artworkCount
	})
	state.touch("scrape", fmt.Sprintf("扫描完成：待补标签 %d，待补歌词封面 %d", candidateCount, artworkCount))

	catalogDeadline := time.Now().Add(libraryAutomationScrapeSlice)
	catalogSliceExpired := false
	catalogExpired := func() bool {
		if catalogSliceExpired || time.Now().Before(catalogDeadline) {
			return catalogSliceExpired
		}
		catalogSliceExpired = true
		state.update(func(report *libraryAutomationReport) {
			report.Backlog = true
			report.Note = "本轮标签刮削已达到独立安全时间片，下一轮自动继续；歌词封面、去重、升级与补全会继续获得执行时间"
		})
		return true
	}

	invalidated := map[string]bool{}
	processed, skipped := 0, 0
	catalogPaused := false
	recordCatalogPause := func(err error) bool {
		var backoff *catalogBackoffError
		if !errors.As(err, &backoff) {
			return false
		}
		delaySec := int(backoff.delay / time.Second)
		state.update(func(report *libraryAutomationReport) {
			report.Backlog = true
			report.RetryAfterSec = delaySec
			report.Note = fmt.Sprintf("目录服务暂不可用，%d 分钟后自动继续刮削", (delaySec+59)/60)
		})
		return true
	}
	dirs := make([]string, 0, len(catalogByDir))
	for dir := range catalogByDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		if ctx.Err() != nil || catalogPaused || catalogExpired() {
			break
		}
		eligible := make([]string, 0, len(catalogByDir[dir]))
		for _, path := range catalogByDir[dir] {
			if state.claimAttempt("catalog|"+path, time.Now()) {
				eligible = append(eligible, path)
			} else {
				skipped++
			}
		}
		for start := 0; start < len(eligible); start += scrapeBatchMaxFiles {
			if ctx.Err() != nil || catalogExpired() {
				break
			}
			end := start + scrapeBatchMaxFiles
			if end > len(eligible) {
				end = len(eligible)
			}
			chunk := eligible[start:end]
			for _, path := range chunk {
				invalidated[automationCacheKey(root, path)] = true
			}
			counts, err := a.runAutomatedScrapeChunk(ctx, chunk, false)
			processed += len(chunk)
			state.update(func(report *libraryAutomationReport) {
				report.ScrapeProcessed = processed
				report.ScrapeSkipped = skipped
				report.ScrapeWritten += counts.Written
				report.ScrapeFailed += counts.Failed
			})
			state.touch("scrape", dir)
			if err != nil {
				state.addError(fmt.Errorf("%s: 整库刮削失败: %w", filepath.Base(dir), err))
				catalogPaused = recordCatalogPause(err)
				if catalogPaused {
					break
				}
			}
		}
	}

	genericDirs := make([]string, 0, len(genericByDir))
	for dir := range genericByDir {
		genericDirs = append(genericDirs, dir)
	}
	sort.Strings(genericDirs)
	for _, dir := range genericDirs {
		if ctx.Err() != nil || catalogPaused || catalogExpired() {
			break
		}
		eligible := make([]string, 0, len(genericByDir[dir]))
		for _, path := range genericByDir[dir] {
			if state.claimAttempt("catalog-generic|"+path, time.Now()) {
				eligible = append(eligible, path)
			} else {
				skipped++
			}
		}
		for start := 0; start < len(eligible); start += scrapeBatchMaxFiles {
			if ctx.Err() != nil || catalogExpired() {
				break
			}
			end := start + scrapeBatchMaxFiles
			if end > len(eligible) {
				end = len(eligible)
			}
			chunk := eligible[start:end]
			for _, path := range chunk {
				invalidated[automationCacheKey(root, path)] = true
			}
			counts, err := a.runAutomatedGenericScrapeDir(ctx, dir, chunk)
			processed += len(chunk)
			state.update(func(report *libraryAutomationReport) {
				report.ScrapeProcessed = processed
				report.ScrapeSkipped = skipped
				report.ScrapeWritten += counts.Written
				report.ScrapeFailed += counts.Failed
			})
			state.touch("scrape", dir)
			if err != nil {
				state.addError(fmt.Errorf("%s: 通用格式刮削失败: %w", filepath.Base(dir), err))
				catalogPaused = recordCatalogPause(err)
				if catalogPaused {
					break
				}
			}
		}
	}
	state.update(func(report *libraryAutomationReport) { report.ScrapeSkipped = skipped })

	artworkProcessed, artworkSkipped, artworkFailed, artworkWritten := 0, 0, 0, 0
	if ctx.Err() == nil {
		artworkDeadline := time.Now().Add(libraryAutomationArtworkSlice)
		artworkSliceExpired := false
		artworkExpired := func() bool {
			if artworkSliceExpired || time.Now().Before(artworkDeadline) {
				return artworkSliceExpired
			}
			artworkSliceExpired = true
			state.update(func(report *libraryAutomationReport) {
				report.Backlog = true
				report.Note = "本轮歌词封面已达到独立安全时间片，下一轮自动继续；标签刮削、去重、升级与补全会继续获得执行时间"
			})
			return true
		}
		jobs := make(chan string)
		workCtx, stopArtwork := context.WithCancel(ctx)
		defer stopArtwork()
		var workers sync.WaitGroup
		var otherProcessed, otherSkipped, otherFailed, otherWritten atomic.Int64
		var stopOnce sync.Once
		var retryAfterNS atomic.Int64
		stopForService := func(err error) {
			delay, transient := artworkRetryDelay(err)
			if !transient {
				return
			}
			stopOnce.Do(func() {
				retryAfterNS.Store(int64(delay))
				state.update(func(report *libraryAutomationReport) {
					report.Backlog = true
					report.RetryAfterSec = int(delay / time.Second)
					report.Note = fmt.Sprintf("歌词封面服务暂不可用，已停止本轮后续请求，约 %d 分钟后自动继续", (int(delay/time.Second)+59)/60)
				})
				state.addError(fmt.Errorf("歌词封面服务暂不可用，已停止本轮后续请求: %w", err))
				stopArtwork()
			})
		}
		for worker := 0; worker < libraryAutomationArtworkWorkers; worker++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for {
					select {
					case <-workCtx.Done():
						return
					case path, ok := <-jobs:
						if !ok {
							return
						}
						wrote, err := a.writeAutomationArtworkMode(workCtx, path, artworkOverwrite[path])
						if wrote {
							otherWritten.Add(1)
						} else if err != nil {
							otherFailed.Add(1)
						} else {
							otherSkipped.Add(1)
						}
						if err != nil {
							// A service-wide failure must not consume the whole
							// artwork backlog. Record one diagnostic, cancel the
							// feeder and retry after a durable backoff window.
							stopForService(err)
							if retryAfterNS.Load() == 0 {
								state.addError(fmt.Errorf("%s: 歌词封面服务失败: %w", path, err))
							}
						}
						processed := otherProcessed.Add(1)
						if processed%20 == 0 {
							state.update(func(report *libraryAutomationReport) {
								report.ArtworkProcessed = artworkProcessed + int(otherProcessed.Load())
								report.ArtworkSkipped = artworkSkipped + int(otherSkipped.Load())
								report.ArtworkFailed = artworkFailed + int(otherFailed.Load())
								report.ArtworkWritten = artworkWritten + int(otherWritten.Load())
							})
							state.touch("scrape", path)
						}
					}
				}
			}()
		}
	feedArtwork:
		for _, path := range artworkDirect {
			if ctx.Err() != nil || artworkExpired() || retryAfterNS.Load() > 0 {
				break
			}
			if !state.claimAttempt("artwork-other|"+path, time.Now()) {
				otherSkipped.Add(1)
				continue
			}
			invalidated[automationCacheKey(root, path)] = true
			select {
			case jobs <- path:
			case <-workCtx.Done():
				break feedArtwork
			}
		}
		close(jobs)
		workers.Wait()
		artworkProcessed += int(otherProcessed.Load())
		artworkSkipped += int(otherSkipped.Load())
		artworkFailed += int(otherFailed.Load())
		artworkWritten += int(otherWritten.Load())
		if artworkFailed > 0 {
			state.update(func(report *libraryAutomationReport) { report.Backlog = true })
		}
	}
	state.update(func(report *libraryAutomationReport) {
		report.ArtworkProcessed = artworkProcessed
		report.ArtworkSkipped = artworkSkipped
		report.ArtworkFailed = artworkFailed
		report.ArtworkWritten = artworkWritten
	})
	for key := range invalidated {
		delete(nextCache.Files, key)
	}
	if err := a.saveLibraryAutomationScanCache(nextCache); err != nil {
		state.addError(fmt.Errorf("扫描缓存更新失败: %w", err))
	}
}

func (entry cachedLibraryScan) localWorkflowTrack(path string) (localWorkflowTrack, bool) {
	if entry.Codec == "" || entry.Duration <= 0 || entry.Duration >= 86400 || strings.TrimSpace(entry.Artist) == "" || strings.TrimSpace(entry.Album) == "" || strings.TrimSpace(entry.Title) == "" {
		return localWorkflowTrack{}, false
	}
	return localWorkflowTrack{Path: path, Artist: entry.Artist, AlbumArtist: entry.AlbumArtist, Album: entry.Album, Title: entry.Title, Codec: entry.Codec, Duration: entry.Duration}, true
}

func (a *App) runAutomatedScrapeChunk(ctx context.Context, paths []string, withArtwork bool) (automationScrapeCounts, error) {
	counts, err := a.runScrapeBatch(ctx, paths, withArtwork)
	if err == nil || len(paths) <= 1 || !strings.Contains(err.Error(), "BATCH_TOO_LARGE") {
		return counts, err
	}
	middle := len(paths) / 2
	left, leftErr := a.runAutomatedScrapeChunk(ctx, paths[:middle], withArtwork)
	counts.Written += left.Written
	counts.Failed += left.Failed
	counts.LyricsWritten += left.LyricsWritten
	counts.CoverWritten += left.CoverWritten
	if leftErr != nil {
		return counts, leftErr
	}
	right, rightErr := a.runAutomatedScrapeChunk(ctx, paths[middle:], withArtwork)
	counts.Written += right.Written
	counts.Failed += right.Failed
	counts.LyricsWritten += right.LyricsWritten
	counts.CoverWritten += right.CoverWritten
	return counts, rightErr
}

func (a *App) runAutomatedGenericScrapeDir(ctx context.Context, dir string, paths []string) (automationScrapeCounts, error) {
	counts := automationScrapeCounts{}
	groups := map[string][]genericAudioInfo{}
	order := []string{}
	for _, path := range paths {
		if ctx.Err() != nil {
			return counts, ctx.Err()
		}
		info, err := probeGenericAudio(ctx, path)
		if err != nil || info.Artist == "" || info.Album == "" || info.Title == "" {
			counts.Failed++
			continue
		}
		albumArtist := firstNonEmpty(info.AlbumArtist, info.Artist)
		key := foldTagText(albumArtist) + "\x00" + foldTagText(info.Album)
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], info)
	}
	sort.Strings(order)
	var firstErr error
	for _, key := range order {
		if ctx.Err() != nil {
			return counts, ctx.Err()
		}
		infos := groups[key]
		if len(infos) == 0 {
			continue
		}
		artist := firstNonEmpty(infos[0].AlbumArtist, infos[0].Artist)
		album := infos[0].Album
		tracks := make([]catalogTrackRequest, 0, len(infos))
		for _, info := range infos {
			duration := info.Duration
			tracks = append(tracks, catalogTrackRequest{Title: info.Title, Artists: []string{info.Artist}, Duration: &duration})
		}
		lookup, err := identifyCatalogLookup(ctx, catalogClient, a.engine, catalogIdentifyRequest{
			Artist: artist, Album: album, Tracks: tracks,
		}, catalogBudget, catalogTTL)
		if err != nil {
			counts.Failed += len(infos)
			if delay, backoff := catalogBackoffDelay(err); backoff {
				return counts, &catalogBackoffError{cause: err, delay: delay}
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if lookup.MusicBrainz == nil {
			counts.Failed += len(infos)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s — %s：目录没有确认发行版", artist, album)
			}
			continue
		}
		for i, info := range infos {
			confirmedTrack := i >= len(lookup.Results) || lookup.Results[i].IdentityConfirmed
			track, found := matchCatalogTrackForResult(lookup.MusicBrainz, info.Title, info.Duration, confirmedTrack)
			if !found {
				counts.Failed++
				continue
			}
			desired := desiredTags(lookup.MusicBrainz, track)
			missing := genericMissingFields(info)
			updates := map[string][]string{}
			for field, value := range desired {
				if missing[field] {
					updates[field] = value
				}
			}
			updates, _ = a.applyFieldLocks(dir, updates)
			if len(updates) == 0 {
				continue
			}
			if _, err := writeGenericTags(ctx, info.Path, updates); err != nil {
				counts.Failed++
				continue
			}
			counts.Written++
		}
	}
	return counts, firstErr
}

type catalogBackoffError struct {
	cause error
	delay time.Duration
}

func (e *catalogBackoffError) Error() string {
	if e == nil || e.cause == nil {
		return "catalog service temporarily unavailable"
	}
	return e.cause.Error()
}

func (e *catalogBackoffError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func catalogBackoffDelay(err error) (time.Duration, bool) {
	var rateErr *musicbrainz.RateLimitError
	if errors.As(err, &rateErr) {
		delay := rateErr.RetryAfter
		if delay < time.Minute {
			delay = time.Minute
		}
		if delay > 30*time.Minute {
			delay = 30 * time.Minute
		}
		return delay, true
	}
	var serverErr *musicbrainz.ServerError
	if errors.As(err, &serverErr) {
		return 15 * time.Minute, true
	}
	return 0, false
}

func wrapCatalogBackoff(err error) error {
	if delay, ok := catalogBackoffDelay(err); ok {
		return &catalogBackoffError{cause: err, delay: delay}
	}
	return err
}

type automationScrapeCounts struct {
	Written       int
	Failed        int
	LyricsWritten int
	CoverWritten  int
}

func (a *App) runScrapeBatch(ctx context.Context, paths []string, withArtwork bool) (automationScrapeCounts, error) {
	status, raw, err := a.libraryMaintenancePost(ctx, "/v1/library/scrape", a.scrapeLibrary, map[string]any{
		"paths": paths, "dry_run": false, "with_artwork": withArtwork,
	})
	if err != nil {
		return automationScrapeCounts{}, err
	}
	if status < 200 || status >= 300 {
		var failure struct {
			Code          string `json:"code"`
			RetryAfterSec int    `json:"retry_after_sec"`
		}
		_ = json.Unmarshal(raw, &failure)
		if failure.Code == "CATALOG_TEMPORARILY_UNAVAILABLE" {
			delay := time.Duration(failure.RetryAfterSec) * time.Second
			if delay <= 0 {
				delay = 15 * time.Minute
			}
			return automationScrapeCounts{}, &catalogBackoffError{cause: fmt.Errorf("HTTP %d: %s", status, strings.TrimSpace(string(raw))), delay: delay}
		}
		return automationScrapeCounts{}, fmt.Errorf("HTTP %d: %s", status, strings.TrimSpace(string(raw)))
	}
	var decoded struct {
		Written       int `json:"written"`
		Failed        int `json:"failed"`
		LyricsWritten int `json:"lyrics_written"`
		CoverWritten  int `json:"cover_written"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return automationScrapeCounts{}, err
	}
	return automationScrapeCounts{Written: decoded.Written, Failed: decoded.Failed, LyricsWritten: decoded.LyricsWritten, CoverWritten: decoded.CoverWritten}, nil
}

func maintenanceNeedsCatalogTags(tags *flactags.Tags) bool {
	if tags == nil {
		return false
	}
	if strings.TrimSpace(tags.Title) == "" || strings.TrimSpace(tags.Artist) == "" || strings.TrimSpace(tags.Album) == "" {
		return true
	}
	if strings.TrimSpace(tags.AlbumArtist) == "" || tags.TrackNumber <= 0 || strings.TrimSpace(tags.Date) == "" || strings.TrimSpace(tags.Genre) == "" {
		return true
	}
	return false
}

func maintenanceMissingSidecars(path string) bool {
	if !hasLyricsSidecar(path) {
		return true
	}
	cover := strings.TrimSuffix(path, filepath.Ext(path)) + ".jpg"
	return !validCoverSidecar(cover)
}

func maintenanceMissingArtwork(path string, tags *flactags.Tags) bool {
	if tags == nil {
		return false
	}
	if !hasLyricsSidecar(path) {
		return true
	}
	sidecar := strings.TrimSuffix(path, filepath.Ext(path)) + ".jpg"
	if _, err := os.Stat(sidecar); err == nil {
		return !validCoverSidecar(sidecar)
	}
	if !tags.HasFrontCover {
		return true
	}
	return false
}

// validCoverBytes fully decodes an image before it is accepted into a user's
// library. Header-only checks miss truncated JPEGs and half-written covers.
func validCoverBytes(raw []byte) bool {
	return imageBytes(raw)
}

func validCoverSidecar(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 10_000_000 {
		return false
	}
	raw, err := os.ReadFile(path)
	return err == nil && validCoverBytes(raw)
}

func artworkSidecarInfo(trackPath string) (size, modNS int64, exists bool) {
	info, err := os.Stat(strings.TrimSuffix(trackPath, filepath.Ext(trackPath)) + ".jpg")
	if err != nil || !info.Mode().IsRegular() {
		return 0, 0, false
	}
	return info.Size(), info.ModTime().UnixNano(), true
}

func cachedArtworkFresh(path string, entry cachedLibraryScan, now time.Time) bool {
	if entry.ArtworkCheckedNS <= 0 {
		return false
	}
	age := now.UnixNano() - entry.ArtworkCheckedNS
	if age < 0 || age >= int64(libraryArtworkValidationTTL) {
		return false
	}
	size, modNS, exists := artworkSidecarInfo(path)
	if exists {
		return entry.ArtworkSize == size && entry.ArtworkModNS == modNS
	}
	return entry.ArtworkSize == 0 && entry.ArtworkModNS == 0 && !entry.NeedsArtwork
}

func artworkRefreshDue(entry cachedLibraryScan, now time.Time) bool {
	if entry.ArtworkCheckedNS <= 0 {
		return false
	}
	age := now.UnixNano() - entry.ArtworkCheckedNS
	return age >= 0 && age >= int64(libraryArtworkRefreshTTL)
}

func recordArtworkValidation(entry *cachedLibraryScan, path string, now time.Time) {
	if entry == nil {
		return
	}
	entry.ArtworkSize, entry.ArtworkModNS, _ = artworkSidecarInfo(path)
	entry.ArtworkCheckedNS = now.UnixNano()
	entry.ArtworkValid = !entry.NeedsArtwork
}

func automationTrackIdentity(path string) (artist, album, title string, err error) {
	if strings.EqualFold(filepath.Ext(path), ".flac") {
		if tags, readErr := readFLACTagsPrefix(path); readErr == nil {
			artist, album, title = strings.TrimSpace(tags.Artist), strings.TrimSpace(tags.Album), strings.TrimSpace(tags.Title)
		}
	}
	if artist == "" || album == "" || title == "" {
		// Artwork lookup is search evidence, not a tag write. Fill missing
		// fields from the managed path before invoking ffprobe: this keeps
		// untagged files recoverable even when ffprobe itself cannot parse a
		// damaged stream.
		if pathArtist, pathAlbum, pathTitle, ok := localWorkflowIdentityFromPath(path); ok {
			if artist == "" {
				artist = pathArtist
			}
			if album == "" {
				album = pathAlbum
			}
			if title == "" {
				title = pathTitle
			}
		}
	}
	if artist == "" || album == "" || title == "" {
		track, probeErr := probeLocalWorkflowTrackForRepair(context.Background(), path)
		if probeErr != nil {
			return "", "", "", probeErr
		}
		artist, album, title = track.Artist, track.Album, track.Title
	}
	return artist, album, title, nil
}

func (a *App) writeAutomationArtwork(ctx context.Context, path string) (bool, error) {
	return a.writeAutomationArtworkMode(ctx, path, false)
}

func (a *App) writeAutomationArtworkMode(ctx context.Context, path string, overwrite bool) (bool, error) {
	artist, album, title, err := automationTrackIdentity(path)
	if err != nil {
		return false, err
	}
	request, cancel := context.WithTimeout(ctx, 20*time.Second)
	lyrics, cover, fetchErr := a.fetchArtworkContext(request, "", "", artist, album, title)
	cancel()
	if fetchErr != nil {
		return false, fetchErr
	}
	wroteLyrics, wroteCover, writeErr := a.writeScrapeArtworkSidecars(path, scrapeArtworkData{Lyrics: lyrics, Cover: cover}, overwrite)
	return wroteLyrics || wroteCover, writeErr
}

func artworkRetryDelay(err error) (time.Duration, bool) {
	var statusErr *artworkHTTPError
	if errors.As(err, &statusErr) {
		switch {
		case statusErr.StatusCode == http.StatusTooManyRequests:
			return 10 * time.Minute, true
		case statusErr.StatusCode >= 500 && statusErr.StatusCode <= 599:
			return 15 * time.Minute, true
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 2 * time.Minute, true
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return 2 * time.Minute, true
	}
	return 0, false
}
func writeAutomationSidecar(path string, body []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".musicops-artwork-*.tmp")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err = temp.Write(body); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	return renameFileWithRetry(name, path)
}

func mergeDuplicateFingerprintCache(cache *libraryAutomationScanDocument, updates map[string]cachedDuplicateFingerprint) {
	if cache == nil || len(updates) == 0 {
		return
	}
	if cache.Files == nil {
		cache.Files = make(map[string]cachedLibraryScan, len(updates))
	}
	for key, fingerprint := range updates {
		if fingerprint.FileSize <= 0 {
			continue
		}
		entry := cache.Files[key]
		wasMetadataComplete := entry.Size != 0 && !entry.DuplicateOnly
		entry.Size = fingerprint.FileSize
		entry.ModNS = fingerprint.FileModNS
		entry.DuplicateOnly = !wasMetadataComplete
		copied := fingerprint
		entry.Duplicate = &copied
		cache.Files[key] = entry
	}
}
func (a *App) runAutomatedDeduplication(ctx context.Context) bool {
	a.duplicateScanMu.Lock()
	defer a.duplicateScanMu.Unlock()
	state := a.libraryAutomation
	state.touch("dedupe", "正在扫描重复项")
	root := a.libraryDir()
	cache := a.loadLibraryDuplicateCache(root)
	checkpoint := func(updates map[string]cachedDuplicateFingerprint) {
		mergeDuplicateFingerprintCache(&cache, updates)
		if err := a.saveLibraryDuplicateCache(cache); err != nil {
			state.addError(fmt.Errorf("重复扫描缓存检查点保存失败: %w", err))
		}
	}
	scanCtx, cancel := context.WithTimeout(ctx, libraryAutomationDedupeSlice)
	defer cancel()
	groups, _, truncated, updates, err := a.scanDuplicatesWithOptions(scanCtx, 0, root, cache.Files, func(checked, total int, current string) {
		state.update(func(report *libraryAutomationReport) {
			report.DuplicateCandidates = total
			report.DuplicateChecked = checked
		})
		label := current
		if parts := strings.Split(current, "\x00"); len(parts) >= 2 {
			label = parts[0] + " - " + parts[1]
		}
		state.touch("dedupe", fmt.Sprintf("正在比对重复项 %d/%d · %s", checked, total, label))
	}, checkpoint)
	mergeDuplicateFingerprintCache(&cache, updates)
	if err := a.saveLibraryDuplicateCache(cache); err != nil {
		state.addError(fmt.Errorf("重复扫描缓存保存失败: %w", err))
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			state.touch("dedupe", "本轮去重指纹已达到独立安全时间片，下一轮从持久缓存继续")
			return true
		}
		state.addError(fmt.Errorf("重复扫描失败: %w", err))
		return true
	}
	candidates := make([]duplicateGroup, 0, len(groups))
	for i := range groups {
		if groups[i].State == "ready" && !a.groupSuppressed(&groups[i]) {
			candidates = append(candidates, groups[i])
		}
	}
	state.update(func(report *libraryAutomationReport) {
		report.DuplicateGroups = len(candidates)
		report.DuplicateChecked = report.DuplicateCandidates
	})
	merged := 0
	for i := range candidates {
		if ctx.Err() != nil || merged >= libraryAutomationMaxMerges {
			break
		}
		group := candidates[i]
		state.touch("dedupe", group.Album)
		mergeCtx := context.WithValue(ctx, automationDuplicateGroupKey{}, &group)
		status, raw, mergeErr := a.libraryMaintenancePost(mergeCtx, "/v1/library/duplicates/merge", a.duplicateMerge, map[string]any{"group_id": group.GroupID})
		if mergeErr == nil && status >= 200 && status < 300 {
			merged++
			state.update(func(report *libraryAutomationReport) { report.Deduplicated++ })
			continue
		}
		var failure struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &failure)
		state.addError(fmt.Errorf("自动去重 %s 跳过: %v %s", group.GroupID, mergeErr, strings.TrimSpace(string(raw))))
		if failure.Code == "MERGE_QBITTORRENT_UNAVAILABLE" {
			// The client state is unknown. Do not try every candidate while the
			// seeding guard cannot answer; expose one persistent blocker instead
			// of letting later scrape errors bury the cause.
			state.update(func(report *libraryAutomationReport) {
				report.BlockedBy = "qBittorrent"
				report.BlockedReason = strings.TrimSpace(failure.Error)
				if report.BlockedReason == "" {
					report.BlockedReason = "qBittorrent 不可达，无法确认种子文件是否仍在做种"
				}
			})
			return true
		}
	}
	return ctx.Err() == nil && (truncated || len(candidates) > merged)
}

// queueIncompleteAlbumExpansion is the immediate follow-up for a publish that
// verified some tracks but was told the source release still has missing
// tracks. It is synchronous so the publish evidence can record whether the
// follow-up was started; the actual expansion workflow runs normally.
func (a *App) queueIncompleteAlbumExpansion(ctx context.Context, byDir map[string][]string) int {
	if a == nil || len(byDir) == 0 {
		return 0
	}
	if hook := a.missingAlbumFollowup; hook != nil {
		return hook(ctx, byDir)
	}
	return a.enqueueIncompleteAlbumExpansion(ctx, byDir)
}

func incompleteAlbumOnlySettings(settings libraryAutomationSettings) libraryAutomationSettings {
	settings.Enabled = true
	settings.Scrape = false
	settings.Dedupe = false
	settings.Upgrade = false
	settings.Expand = true
	return settings
}

func (a *App) startIncompleteAlbumAutomationFollowup() bool {
	settings, err := a.loadLibraryAutomationSettings()
	if err != nil {
		if a.libraryAutomation != nil {
			a.libraryAutomation.addError(fmt.Errorf("读取部分专辑补全设置失败: %w", err))
			a.libraryAutomation.update(func(report *libraryAutomationReport) { report.Backlog = true })
		}
		return false
	}
	settings = incompleteAlbumOnlySettings(settings)
	if a.startLibraryAutomationRunWithSettings("partial-album-followup", settings) {
		return true
	}
	if a.libraryAutomation != nil {
		a.libraryAutomation.update(func(report *libraryAutomationReport) { report.Backlog = true })
	}
	return false
}
func (a *App) enqueueIncompleteAlbumExpansion(ctx context.Context, byDir map[string][]string) int {
	state := a.libraryAutomation
	root := canonicalPath(a.libraryDir())
	cache := a.loadLibraryAutomationScanCache(root)
	dirs := make([]string, 0, len(byDir))
	for dir, paths := range byDir {
		if dir != "" && len(paths) > 0 && insideDir(root, dir) {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	jobs := 0
	for _, dir := range dirs {
		if ctx.Err() != nil || jobs >= libraryAutomationMaxJobs {
			break
		}
		paths := append([]string(nil), byDir[dir]...)
		sort.Strings(paths)
		seen := map[string]bool{}
		tracks := make([]localWorkflowTrack, 0, len(paths))
		for _, path := range paths {
			key := automationCacheKey(root, path)
			if seen[key] || !insideDir(root, path) {
				continue
			}
			seen[key] = true
			if entry, ok := cache.Files[key]; ok {
				if track, valid := entry.localWorkflowTrack(path); valid {
					tracks = append(tracks, track)
					continue
				}
			}
			track, err := probeLocalWorkflowTrackForRepair(ctx, path)
			if err != nil {
				if state != nil {
					state.addError(fmt.Errorf("部分入库后整专补全读取曲目失败 %s: %w", filepath.Base(path), err))
				}
				continue
			}
			tracks = append(tracks, track)
		}
		if len(tracks) == 0 {
			if state != nil {
				state.update(func(report *libraryAutomationReport) { report.Backlog = true })
			}
			continue
		}
		if state != nil {
			state.touch("partial_album_followup", dir)
		}
		for _, albumTracks := range groupWorkflowTracksByAlbum(tracks) {
			if ctx.Err() != nil || jobs >= libraryAutomationMaxJobs {
				break
			}
			if a.enqueueMaintenanceWorkflow(ctx, "expand", albumTracks, dir) {
				jobs++
				if state != nil {
					state.update(func(report *libraryAutomationReport) { report.ExpandJobs++ })
				}
			}
		}
	}
	if jobs >= libraryAutomationMaxJobs && state != nil {
		state.update(func(report *libraryAutomationReport) { report.Backlog = true })
	}
	return jobs
}
func (a *App) enqueueLibraryMaintenanceWorkflows(ctx context.Context, byDir map[string][]string, settings libraryAutomationSettings) {
	state := a.libraryAutomation
	root := a.libraryDir()
	cache := a.loadLibraryAutomationScanCache(root)
	audit := a.loadLibraryAlbumAudit(root)
	auditDirty := false
	albumChecks := 0
	nowNS := a.now().UnixNano()
	dirs := make([]string, 0, len(byDir))
	for dir := range byDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	jobs := 0
	for _, dir := range dirs {
		if ctx.Err() != nil || jobs >= libraryAutomationMaxJobs {
			break
		}
		state.touch("library_workflows", dir)
		files := byDir[dir]
		tracks := make([]localWorkflowTrack, 0, len(files))
		for _, path := range files {
			if entry, ok := cache.Files[automationCacheKey(root, path)]; ok {
				if track, valid := entry.localWorkflowTrack(path); valid {
					tracks = append(tracks, track)
					continue
				}
			}
			track, err := probeLocalWorkflowTrackForRepair(ctx, path)
			if err == nil {
				tracks = append(tracks, track)
			}
		}
		if len(tracks) == 0 {
			continue
		}
		if settings.Upgrade {
			lossy := make([]localWorkflowTrack, 0)
			for _, track := range tracks {
				if isLossyUpgradeSource(track.Codec) {
					lossy = append(lossy, track)
				}
			}
			for _, track := range lossy {
				if jobs >= libraryAutomationMaxJobs {
					break
				}
				// Each maintenance workflow carries one track so the single-track
				// streaming plan cannot silently leave the other 31 tracks behind.
				if a.enqueueMaintenanceWorkflow(ctx, "upgrade", []localWorkflowTrack{track}, dir) {
					jobs++
					state.update(func(report *libraryAutomationReport) { report.UpgradeJobs++ })
				}
			}
		}
		if settings.Expand && jobs < libraryAutomationMaxJobs {
			for _, albumTracks := range groupWorkflowTracksByAlbum(tracks) {
				if ctx.Err() != nil || jobs >= libraryAutomationMaxJobs {
					break
				}
				auditKey := albumAuditIdentity(dir, albumTracks)
				signature := albumWorkflowSignature(albumTracks)
				record, known := audit.Albums[auditKey]
				age := nowNS - record.CheckedNS
				fresh := known && record.Signature == signature && record.CheckedNS > 0 && age >= 0 && age < int64(libraryAlbumAuditTTL)
				expand := false
				if fresh {
					expand = record.Expandable && record.ExpectedTracks > len(albumTracks)
				} else {
					if albumChecks >= libraryAutomationAlbumChecks {
						state.update(func(report *libraryAutomationReport) { report.Backlog = true })
						continue
					}
					var checkErr error
					var expected int
					expand, expected, checkErr = a.albumExpansionState(ctx, catalogClient, albumTracks)
					if checkErr != nil {
						state.addError(fmt.Errorf("%s / %s: 整专补全核查失败: %w", filepath.Base(dir), albumTracks[0].Album, checkErr))
						state.update(func(report *libraryAutomationReport) {
							report.Backlog = true
							if delay, backoff := catalogBackoffDelay(checkErr); backoff {
								report.RetryAfterSec = int(delay / time.Second)
								report.Note = fmt.Sprintf("目录服务暂不可用，%d 分钟后继续核查整专", (int(delay/time.Second)+59)/60)
							}
						})
						if delay, backoff := catalogBackoffDelay(checkErr); backoff && delay >= time.Minute {
							break
						}
						continue
					}
					albumChecks++
					audit.Albums[auditKey] = libraryAlbumAuditRecord{
						ExpectedTracks: expected, LocalFiles: len(albumTracks), Expandable: expand,
						Signature: signature, CheckedNS: nowNS,
					}
					auditDirty = true
				}
				if expand && a.enqueueMaintenanceWorkflow(ctx, "expand", albumTracks, dir) {
					jobs++
					state.update(func(report *libraryAutomationReport) { report.ExpandJobs++ })
				}
			}
		}
	}
	if jobs >= libraryAutomationMaxJobs {
		state.update(func(report *libraryAutomationReport) { report.Backlog = true })
	}
	if auditDirty {
		if err := a.saveLibraryAlbumAudit(audit); err != nil {
			state.addError(fmt.Errorf("专辑核查缓存保存失败: %w", err))
			state.update(func(report *libraryAutomationReport) { report.Backlog = true })
		}
	}
}

// groupWorkflowTracksByAlbum keeps mixed-album directories valid for the
// expansion endpoint. Album artist is retained as the grouping identity while
// every track keeps its own artist credit.
func groupWorkflowTracksByAlbum(tracks []localWorkflowTrack) [][]localWorkflowTrack {
	groups := map[string][]localWorkflowTrack{}
	for _, track := range tracks {
		key := normalizeKey(track.Album)
		groups[key] = append(groups[key], track)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([][]localWorkflowTrack, 0, len(keys))
	for _, key := range keys {
		group := groups[key]
		sort.SliceStable(group, func(i, j int) bool { return group[i].Path < group[j].Path })
		out = append(out, group)
	}
	return out
}

func albumAuditIdentity(dir string, tracks []localWorkflowTrack) string {
	if len(tracks) == 0 {
		return ""
	}
	albumArtist := localWorkflowAlbumArtist(tracks)
	return filepath.Clean(dir) + "\x00" + normalizeKey(tracks[0].Album) + "\x00" + normalizeKey(albumArtist)
}

func albumWorkflowSignature(tracks []localWorkflowTrack) string {
	parts := make([]string, 0, len(tracks))
	for _, track := range tracks {
		parts = append(parts, filepath.Clean(track.Path)+"\x00"+track.Artist+"\x00"+track.AlbumArtist+"\x00"+track.Album+"\x00"+track.Title)
	}
	sort.Strings(parts)
	return libraryFileID(strings.Join(parts, "\x01"))
}

// localAlbumNeedsCatalogCheck bounds catalogue lookups to small albums and
// folders whose numbered filenames already prove a gap. Full 10-track folders
// are not queried again on every maintenance pass.
func localAlbumNeedsCatalogCheck(tracks []localWorkflowTrack) bool {
	if len(tracks) == 0 {
		return false
	}
	if len(tracks) <= 3 {
		return true
	}
	seen := map[int]bool{}
	maxTrack := 0
	for _, track := range tracks {
		number := trackNumberFromName(filepath.Base(track.Path))
		if number > 0 {
			seen[number] = true
			if number > maxTrack {
				maxTrack = number
			}
		}
	}
	return maxTrack > len(seen)
}

// albumNeedsExpansion consults the catalogue before creating an expansion
// workflow. A one-file directory is often a legitimate single, and an
// unconfirmed release can still be a different edition; neither proves that
// tracks are missing. Only a confirmed local track on a release with at least
// two catalogue tracks is safe enough to expand automatically.
func (a *App) albumNeedsExpansion(ctx context.Context, client *musicbrainz.Client, tracks []localWorkflowTrack) (bool, error) {
	expand, _, err := a.albumExpansionState(ctx, client, tracks)
	return expand, err
}

func (a *App) albumExpansionState(ctx context.Context, client *musicbrainz.Client, tracks []localWorkflowTrack) (bool, int, error) {
	if len(tracks) == 0 {
		return false, 0, nil
	}
	requestTracks := make([]catalogTrackRequest, 0, len(tracks))
	for _, track := range tracks {
		duration := track.Duration
		requestTracks = append(requestTracks, catalogTrackRequest{Title: track.Title, Artists: []string{track.Artist}, Duration: &duration})
	}
	lookup, err := identifyCatalogLookup(ctx, client, a.engine, catalogIdentifyRequest{
		Artist: localWorkflowAlbumArtist(tracks),
		Album:  tracks[0].Album,
		Tracks: requestTracks,
	}, catalogBudget, catalogTTL)
	if err != nil {
		return false, 0, err
	}
	view := lookup.MusicBrainz
	if view == nil {
		return false, 0, nil
	}
	if view.TrackCount <= len(tracks) {
		return false, view.TrackCount, nil
	}
	if len(lookup.Results) != len(tracks) {
		return false, view.TrackCount, nil
	}
	for _, result := range lookup.Results {
		if !result.IdentityConfirmed {
			return false, view.TrackCount, nil
		}
	}
	return true, view.TrackCount, nil
}

func localWorkflowAlbumArtist(tracks []localWorkflowTrack) string {
	for _, track := range tracks {
		if artist := strings.TrimSpace(track.AlbumArtist); artist != "" {
			return artist
		}
	}
	if len(tracks) == 0 {
		return ""
	}
	if artist, _, _, ok := localWorkflowIdentityFromPath(tracks[0].Path); ok {
		if artist = strings.TrimSpace(artist); artist != "" {
			return artist
		}
	}
	return strings.TrimSpace(tracks[0].Artist)
}

func (a *App) enqueueMaintenanceWorkflow(ctx context.Context, scene string, tracks []localWorkflowTrack, dir string) bool {
	return a.enqueueMaintenanceWorkflowKey(ctx, scene, tracks, dir, "")
}

func (a *App) enqueueMaintenanceWorkflowKey(ctx context.Context, scene string, tracks []localWorkflowTrack, dir, keyPrefix string) bool {
	if len(tracks) == 0 {
		return false
	}
	paths := make([]string, 0, len(tracks))
	for _, track := range tracks {
		paths = append(paths, track.Path)
	}
	sort.Strings(paths)
	key := scene + "|" + filepath.Clean(dir) + "|" + strings.Join(paths, "|")
	if keyPrefix != "" {
		key = keyPrefix + "|" + key
	} else if scene == "upgrade" {
		// The corrupt-file pass uses a separately versioned key. Do not create a
		// second ordinary upgrade job for the same path in the same maintenance
		// run; the versioned repair job owns future retry cooldown.
		if _, existing, lookupErr := a.automationWorkflowID(scene, "corrupt-repair-v2|"+key); lookupErr == nil && existing != nil {
			return false
		}
	}
	status, raw, err := a.libraryMaintenancePost(ctx, "/v1/library/workflow", a.libraryWorkflow, map[string]any{
		"scene": scene, "label": tracks[0].Artist + " — " + tracks[0].Album, "paths": paths, "confirmed": true, "key": key,
	})
	if err != nil {
		a.libraryAutomation.addError(fmt.Errorf("自动%s任务创建失败: %w", scene, err))
		return false
	}
	if status < 200 || status >= 300 {
		a.libraryAutomation.addError(fmt.Errorf("自动%s任务创建失败: HTTP %d %s", scene, status, strings.TrimSpace(string(raw))))
		return false
	}
	var decoded struct {
		Started bool `json:"started"`
	}
	_ = json.Unmarshal(raw, &decoded)
	return decoded.Started
}

func (a *App) libraryMaintenancePost(ctx context.Context, path string, handler http.HandlerFunc, body any) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	requestCtx := context.WithValue(ctx, automationWorkflowPriorityKey{}, automationWorkflowPriority)
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)).WithContext(requestCtx)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	return recorder.Code, recorder.Body.Bytes(), nil
}

func (a *App) libraryAutomationHandler(w http.ResponseWriter, r *http.Request) {
	settings, err := a.loadLibraryAutomationSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	running, report := a.libraryAutomation.snapshot()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": settings, "running": running, "report": report})
}

func (a *App) saveLibraryAutomationHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled       *bool `json:"enabled"`
		Scrape        *bool `json:"scrape"`
		Dedupe        *bool `json:"dedupe"`
		Upgrade       *bool `json:"upgrade"`
		Expand        *bool `json:"expand"`
		IntervalHours *int  `json:"interval_hours"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	settings, err := a.loadLibraryAutomationSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	if body.Enabled != nil {
		settings.Enabled = *body.Enabled
	}
	if body.Scrape != nil {
		settings.Scrape = *body.Scrape
	}
	if body.Dedupe != nil {
		settings.Dedupe = *body.Dedupe
	}
	if body.Upgrade != nil {
		settings.Upgrade = *body.Upgrade
	}
	if body.Expand != nil {
		settings.Expand = *body.Expand
	}
	if body.IntervalHours != nil {
		settings.IntervalHours = *body.IntervalHours
	}
	settings, err = a.saveLibraryAutomationSettings(settings)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": settings})
}

func (a *App) runLibraryAutomationHandler(w http.ResponseWriter, r *http.Request) {
	if !a.startLibraryAutomationRun("manual") {
		running, report := a.libraryAutomation.snapshot()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": false, "running": running, "report": report})
		return
	}
	running, report := a.libraryAutomation.snapshot()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "started": true, "running": running, "report": report})
}

func (a *App) runLibraryScrapeHandler(w http.ResponseWriter, r *http.Request) {
	if !a.startLibraryScrapeRun("whole-library-scrape") {
		running, report := a.libraryAutomation.snapshot()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": false, "running": running, "report": report})
		return
	}
	running, report := a.libraryAutomation.snapshot()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "started": true, "running": running, "report": report})
}

func (a *App) runLibraryUpgradeHandler(w http.ResponseWriter, r *http.Request) {
	if !a.startLibraryUpgradeRun("whole-library-upgrade") {
		running, report := a.libraryAutomation.snapshot()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": false, "running": running, "report": report})
		return
	}
	running, report := a.libraryAutomation.snapshot()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "started": true, "running": running, "report": report})
}

func (a *App) stopLibraryAutomationHandler(w http.ResponseWriter, r *http.Request) {
	a.libraryAutomation.stop()
	running, report := a.libraryAutomation.snapshot()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "running": running, "stopping": running, "report": report})
}
