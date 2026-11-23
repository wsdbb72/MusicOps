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

type automationWorkflowPriorityKey struct{}

const (
	automationWorkflowPriority      = 50
	libraryAutomationInitialDelay   = 5 * time.Minute
	libraryAutomationMaxMerges      = 1000
	libraryAutomationMaxJobs        = 200
	libraryAutomationScanWorkers    = 8
	duplicateFingerprintWorkers     = 8
	libraryAutomationCheckpoint     = 20_000
	libraryAutomationScanVersion    = 2
	libraryAutomationStallTimeout   = 45 * time.Minute
	libraryAutomationScrapeSlice    = 45 * time.Minute
	libraryAutomationArtworkWorkers = 6
	libraryAutomationPolicyVersion  = 2
	libraryAutomationSkippedRetry   = 24 * time.Hour
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
	Size          int64                       `json:"size"`
	ModNS         int64                       `json:"mod_ns"`
	NeedsCatalog  bool                        `json:"needs_catalog"`
	NeedsArtwork  bool                        `json:"needs_artwork"`
	Codec         string                      `json:"codec,omitempty"`
	Duration      float64                     `json:"duration,omitempty"`
	Artist        string                      `json:"artist,omitempty"`
	Album         string                      `json:"album,omitempty"`
	Title         string                      `json:"title,omitempty"`
	Duplicate     *cachedDuplicateFingerprint `json:"duplicate,omitempty"`
	DuplicateOnly bool                        `json:"duplicate_only,omitempty"`
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
	UpgradeJobs         int      `json:"upgrade_jobs"`
	ExpandJobs          int      `json:"expand_jobs"`
	Errors              []string `json:"errors,omitempty"`
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
	if len(s.report.Errors) < 20 {
		s.report.Errors = append(s.report.Errors, scrub(err.Error()))
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
	if input.hasCache && !input.cached.DuplicateOnly && input.cached.Size == info.Size() && input.cached.ModNS == info.ModTime().UnixNano() {
		result.entry = input.cached
		result.ok = true
		result.reused = true
		return result
	}
	entry := cachedLibraryScan{Size: info.Size(), ModNS: info.ModTime().UnixNano()}
	if input.hasCache && input.cached.Size == info.Size() && input.cached.ModNS == info.ModTime().UnixNano() && input.cached.Duplicate != nil {
		entry.Duplicate = input.cached.Duplicate
	}
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
		entry.Album = genericInfo.Album
		entry.Title = genericInfo.Title
	}
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
			artworkCount++
		}
	}
	state.update(func(report *libraryAutomationReport) {
		report.ScrapeCandidates = candidateCount
		report.ArtworkCandidates = artworkCount
	})
	state.touch("scrape", fmt.Sprintf("扫描完成：待补标签 %d，待补歌词封面 %d", candidateCount, artworkCount))

	scrapeDeadline := time.Now().Add(libraryAutomationScrapeSlice)
	scrapeSliceExpired := false
	sliceExpired := func() bool {
		if scrapeSliceExpired || time.Now().Before(scrapeDeadline) {
			return scrapeSliceExpired
		}
		scrapeSliceExpired = true
		state.update(func(report *libraryAutomationReport) {
			report.Backlog = true
			report.Note = "本轮刮削已达到安全时间片，下一轮自动继续；去重和音质升级不会被长期阻塞"
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
		if ctx.Err() != nil || catalogPaused || sliceExpired() {
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
			if ctx.Err() != nil || sliceExpired() {
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
		if ctx.Err() != nil || catalogPaused || sliceExpired() {
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
			if ctx.Err() != nil || sliceExpired() {
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
	if ctx.Err() == nil && !sliceExpired() {
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
						wrote, err := a.writeAutomationArtwork(workCtx, path)
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
			if ctx.Err() != nil || sliceExpired() || retryAfterNS.Load() > 0 {
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
	return localWorkflowTrack{Path: path, Artist: entry.Artist, Album: entry.Album, Title: entry.Title, Codec: entry.Codec, Duration: entry.Duration}, true
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
	_, err := os.Stat(cover)
	return os.IsNotExist(err)
}

func maintenanceMissingArtwork(path string, tags *flactags.Tags) bool {
	if tags == nil {
		return false
	}
	if !hasLyricsSidecar(path) {
		return true
	}
	if !tags.HasFrontCover {
		sidecar := strings.TrimSuffix(path, filepath.Ext(path)) + ".jpg"
		if _, err := os.Stat(sidecar); os.IsNotExist(err) {
			return true
		}
	}
	return false
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
	wroteLyrics, wroteCover, writeErr := a.writeScrapeArtworkSidecars(path, scrapeArtworkData{Lyrics: lyrics, Cover: cover}, false)
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
	state := a.libraryAutomation
	state.touch("dedupe", "正在扫描重复项")
	root := a.libraryDir()
	cache := a.loadLibraryAutomationScanCache(root)
	checkpoint := func(updates map[string]cachedDuplicateFingerprint) {
		mergeDuplicateFingerprintCache(&cache, updates)
		if err := a.saveLibraryAutomationScanCache(cache); err != nil {
			state.addError(fmt.Errorf("重复扫描缓存检查点保存失败: %w", err))
		}
	}
	groups, _, truncated, updates, err := a.scanDuplicatesWithOptions(ctx, 0, root, cache.Files, func(checked, total int, current string) {
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
	if err := a.saveLibraryAutomationScanCache(cache); err != nil {
		state.addError(fmt.Errorf("重复扫描缓存保存失败: %w", err))
	}
	if err != nil {
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
			// seeding guard cannot answer; retry the whole pass later.
			return true
		}
	}
	return ctx.Err() == nil && (truncated || len(candidates) > merged)
}

func (a *App) enqueueLibraryMaintenanceWorkflows(ctx context.Context, byDir map[string][]string, settings libraryAutomationSettings) {
	state := a.libraryAutomation
	root := a.libraryDir()
	cache := a.loadLibraryAutomationScanCache(root)
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
		queuedUpgrade := false
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
					queuedUpgrade = true
					state.update(func(report *libraryAutomationReport) { report.UpgradeJobs++ })
				}
			}
		}
		if settings.Expand && !queuedUpgrade && jobs < libraryAutomationMaxJobs && len(tracks) == 1 {
			expand, checkErr := a.albumNeedsExpansion(ctx, catalogClient, tracks[0])
			if checkErr != nil {
				state.addError(fmt.Errorf("%s: 整专补全核查失败: %w", filepath.Base(dir), checkErr))
				state.update(func(report *libraryAutomationReport) {
					report.Backlog = true
					if delay, backoff := catalogBackoffDelay(checkErr); backoff {
						report.RetryAfterSec = int(delay / time.Second)
						report.Note = fmt.Sprintf("目录服务暂不可用，%d 分钟后继续核查整专", (int(delay/time.Second)+59)/60)
					}
				})
			} else if expand && a.enqueueMaintenanceWorkflow(ctx, "expand", tracks, dir) {
				jobs++
				state.update(func(report *libraryAutomationReport) { report.ExpandJobs++ })
			}
		}
	}
	if jobs >= libraryAutomationMaxJobs {
		state.update(func(report *libraryAutomationReport) { report.Backlog = true })
	}
}

// albumNeedsExpansion consults the catalogue before creating an expansion
// workflow. A one-file directory is often a legitimate single, and an
// unconfirmed release can still be a different edition; neither proves that
// tracks are missing. Only a confirmed local track on a release with at least
// two catalogue tracks is safe enough to expand automatically.
func (a *App) albumNeedsExpansion(ctx context.Context, client *musicbrainz.Client, track localWorkflowTrack) (bool, error) {
	duration := track.Duration
	lookup, err := identifyCatalogLookup(ctx, client, a.engine, catalogIdentifyRequest{
		Artist: track.Artist,
		Album:  track.Album,
		Tracks: []catalogTrackRequest{{Title: track.Title, Artists: []string{track.Artist}, Duration: &duration}},
	}, catalogBudget, catalogTTL)
	if err != nil {
		return false, err
	}
	view := lookup.MusicBrainz
	if view == nil || view.TrackCount <= 1 || len(view.Tracks) <= 1 {
		return false, nil
	}
	if len(lookup.Results) == 0 || !lookup.Results[0].IdentityConfirmed {
		return false, nil
	}
	return true, nil
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
