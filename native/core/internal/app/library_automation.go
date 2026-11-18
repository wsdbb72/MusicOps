package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
	"github.com/wsdbb72/MusicOps/native/core/internal/musicbrainz"
)

var libraryAutomationSettingsKey = []byte("app/settings/library-automation")

const (
	libraryAutomationInitialDelay = 5 * time.Minute
	libraryAutomationMaxScrape    = 200
	libraryAutomationMaxArtwork   = 90
	libraryAutomationMaxMerges    = 5
	libraryAutomationMaxJobs      = 20
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

type libraryAutomationReport struct {
	Running          bool     `json:"running"`
	Phase            string   `json:"phase,omitempty"`
	Started          string   `json:"started,omitempty"`
	Finished         string   `json:"finished,omitempty"`
	IndexedAdded     int      `json:"indexed_added"`
	IndexedPruned    int      `json:"indexed_pruned"`
	ScrapeCandidates int      `json:"scrape_candidates"`
	ScrapeWritten    int      `json:"scrape_written"`
	ArtworkWritten   int      `json:"artwork_written"`
	ScrapeFailed     int      `json:"scrape_failed"`
	DuplicateGroups  int      `json:"duplicate_groups"`
	Deduplicated     int      `json:"deduplicated"`
	UpgradeJobs      int      `json:"upgrade_jobs"`
	ExpandJobs       int      `json:"expand_jobs"`
	Errors           []string `json:"errors,omitempty"`
	Note             string   `json:"note,omitempty"`
}

type libraryAutomationState struct {
	mu       sync.Mutex
	running  bool
	report   libraryAutomationReport
	cancel   context.CancelFunc
	attempts map[string]time.Time
}

func (s *libraryAutomationState) snapshot() (bool, libraryAutomationReport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	report := s.report
	report.Running = s.running
	return s.running, report
}

func (s *libraryAutomationState) setPhase(phase string) {
	s.mu.Lock()
	s.report.Phase = phase
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	s.running = true
	s.cancel = cancel
	s.report = libraryAutomationReport{Running: true, Started: time.Now().Format(time.RFC3339), Phase: "queued"}
	return ctx, true
}

func (s *libraryAutomationState) finish(err error, note string) {
	s.mu.Lock()
	if err != nil && len(s.report.Errors) < 20 {
		s.report.Errors = append(s.report.Errors, scrub(err.Error()))
	}
	s.report.Running = false
	s.report.Finished = time.Now().Format(time.RFC3339)
	s.report.Note = note
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.running = false
	s.mu.Unlock()
}

func (s *libraryAutomationState) stop() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
}

func (a *App) runLibraryAutomationScheduler(stop <-chan struct{}) {
	timer := time.NewTimer(libraryAutomationInitialDelay)
	defer timer.Stop()
	select {
	case <-stop:
		return
	case <-timer.C:
	}
	for {
		settings, err := a.loadLibraryAutomationSettings()
		if err == nil && settings.Enabled {
			a.startLibraryAutomationRun("scheduled")
		}
		delay := 6 * time.Hour
		if err == nil {
			delay = time.Duration(settings.IntervalHours) * time.Hour
		}
		timer.Reset(delay)
		select {
		case <-stop:
			return
		case <-timer.C:
		}
	}
}

func (a *App) startLibraryAutomationRun(reason string) bool {
	if a == nil || a.libraryAutomation == nil {
		return false
	}
	ctx, started := a.libraryAutomation.begin()
	if !started {
		return false
	}
	settings, err := a.loadLibraryAutomationSettings()
	if err != nil {
		a.libraryAutomation.finish(err, "自动维护设置读取失败")
		return false
	}
	go func() {
		err := a.runLibraryAutomationPass(ctx, settings)
		a.libraryAutomation.finish(err, "本轮自动维护由"+reason+"触发")
	}()
	return true
}

func (a *App) runLibraryAutomationPass(ctx context.Context, settings libraryAutomationSettings) error {
	state := a.libraryAutomation
	root := a.libraryDir()
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return fmt.Errorf("音乐库目录不可用")
	}

	state.setPhase("reindex")
	added, _, pruned, err := a.indexLibraryTree(ctx, root)
	if err != nil {
		return err
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
		a.runAutomatedScrape(ctx, paths, byDir)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if settings.Dedupe {
		state.setPhase("dedupe")
		a.runAutomatedDeduplication(ctx)
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

func (a *App) runAutomatedScrape(ctx context.Context, paths []string, byDir map[string][]string) {
	state := a.libraryAutomation
	catalogByDir := map[string][]string{}
	genericByDir := map[string][]string{}
	artworkFLACByDir := map[string][]string{}
	artworkOther := []string{}
	candidateCount := 0
	for _, path := range paths {
		if ctx.Err() != nil {
			return
		}
		if !strings.EqualFold(filepath.Ext(path), ".flac") {
			if info, err := probeGenericAudio(ctx, path); err == nil && maintenanceNeedsGenericTags(info) {
				genericByDir[filepath.Dir(path)] = append(genericByDir[filepath.Dir(path)], path)
				candidateCount++
			}
			if maintenanceMissingSidecars(path) {
				artworkOther = append(artworkOther, path)
			}
			continue
		}
		tags, err := readFLACTagsPrefix(path)
		if err != nil {
			state.addError(fmt.Errorf("%s: 标签读取失败", filepath.Base(path)))
			continue
		}
		if maintenanceNeedsCatalogTags(tags) {
			catalogByDir[filepath.Dir(path)] = append(catalogByDir[filepath.Dir(path)], path)
			candidateCount++
		}
		if maintenanceMissingArtwork(path, tags) {
			artworkFLACByDir[filepath.Dir(path)] = append(artworkFLACByDir[filepath.Dir(path)], path)
		}
	}
	state.update(func(report *libraryAutomationReport) { report.ScrapeCandidates = candidateCount })

	processed := 0
	dirs := make([]string, 0, len(catalogByDir))
	for dir := range catalogByDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		if ctx.Err() != nil || processed >= libraryAutomationMaxScrape {
			break
		}
		eligible := make([]string, 0, len(catalogByDir[dir]))
		for _, path := range catalogByDir[dir] {
			if processed+len(eligible) >= libraryAutomationMaxScrape {
				break
			}
			if state.claimAttempt("catalog|"+path, time.Now()) {
				eligible = append(eligible, path)
			}
		}
		files := eligible
		if len(files) == 0 {
			continue
		}
		counts, err := a.runScrapeBatch(ctx, files, false)
		if err != nil {
			state.addError(fmt.Errorf("%s: 整库刮削失败: %w", filepath.Base(dir), err))
			continue
		}
		processed += len(files)
		state.update(func(report *libraryAutomationReport) {
			report.ScrapeWritten += counts.Written
			report.ScrapeFailed += counts.Failed
		})
	}

	genericDirs := make([]string, 0, len(genericByDir))
	for dir := range genericByDir {
		genericDirs = append(genericDirs, dir)
	}
	sort.Strings(genericDirs)
	for _, dir := range genericDirs {
		if ctx.Err() != nil || processed >= libraryAutomationMaxScrape {
			break
		}
		files := make([]string, 0, len(genericByDir[dir]))
		for _, path := range genericByDir[dir] {
			if processed+len(files) >= libraryAutomationMaxScrape {
				break
			}
			if state.claimAttempt("catalog-generic|"+path, time.Now()) {
				files = append(files, path)
			}
		}
		if len(files) == 0 {
			continue
		}
		counts, err := a.runAutomatedGenericScrapeDir(ctx, dir, files)
		if err != nil {
			state.addError(fmt.Errorf("%s: 通用格式刮削失败: %w", filepath.Base(dir), err))
		}
		processed += len(files)
		state.update(func(report *libraryAutomationReport) {
			report.ScrapeWritten += counts.Written
			report.ScrapeFailed += counts.Failed
		})
	}

	artworkProcessed := 0
	flacDirs := make([]string, 0, len(artworkFLACByDir))
	for dir := range artworkFLACByDir {
		flacDirs = append(flacDirs, dir)
	}
	sort.Strings(flacDirs)
	for _, dir := range flacDirs {
		files := artworkFLACByDir[dir]
		for start := 0; start < len(files) && artworkProcessed < libraryAutomationMaxArtwork; start += scrapeArtworkMaxFiles {
			if ctx.Err() != nil {
				return
			}
			end := start + scrapeArtworkMaxFiles
			if end > len(files) {
				end = len(files)
			}
			if end > libraryAutomationMaxArtwork-artworkProcessed {
				end = start + libraryAutomationMaxArtwork - artworkProcessed
			}
			claimed := make([]string, 0, end-start)
			for _, path := range files[start:end] {
				if state.claimAttempt("artwork-flac|"+path, time.Now()) {
					claimed = append(claimed, path)
				}
			}
			if len(claimed) == 0 {
				continue
			}
			counts, err := a.runScrapeBatch(ctx, claimed, true)
			if err != nil {
				state.addError(fmt.Errorf("%s: 歌词封面刮削失败: %w", filepath.Base(dir), err))
				continue
			}
			artworkProcessed += len(claimed)
			state.update(func(report *libraryAutomationReport) {
				report.ScrapeWritten += counts.Written
				report.ArtworkWritten += counts.CoverWritten + counts.LyricsWritten
				report.ScrapeFailed += counts.Failed
			})
		}
	}
	for _, path := range artworkOther {
		if ctx.Err() != nil || artworkProcessed >= libraryAutomationMaxArtwork {
			break
		}
		if !state.claimAttempt("artwork-other|"+path, time.Now()) {
			continue
		}
		if a.writeAutomationArtwork(ctx, path) {
			artworkProcessed++
			state.update(func(report *libraryAutomationReport) { report.ArtworkWritten++ })
		}
	}
}

func (a *App) runAutomatedGenericScrapeDir(ctx context.Context, dir string, paths []string) (automationScrapeCounts, error) {
	counts := automationScrapeCounts{}
	infos := make([]genericAudioInfo, 0, len(paths))
	for _, path := range paths {
		info, err := probeGenericAudio(ctx, path)
		if err != nil {
			counts.Failed++
			continue
		}
		infos = append(infos, info)
	}
	if len(infos) == 0 {
		return counts, nil
	}
	artist, album := infos[0].Artist, infos[0].Album
	tracks := make([]catalogTrackRequest, 0, len(infos))
	for _, info := range infos {
		duration := info.Duration
		tracks = append(tracks, catalogTrackRequest{Title: info.Title, Artists: []string{info.Artist}, Duration: &duration})
	}
	lookup, err := identifyCatalogLookup(musicbrainz.NewClient(), a.engine, catalogIdentifyRequest{
		Artist: artist, Album: album, Tracks: tracks,
	}, catalogBudget, catalogTTL)
	if err != nil {
		return counts, err
	}
	if lookup.MusicBrainz == nil {
		return counts, fmt.Errorf("目录没有确认发行版")
	}
	for i, info := range infos {
		if i < len(lookup.Results) && !lookup.Results[i].IdentityConfirmed {
			counts.Failed++
			continue
		}
		track, found := matchCatalogTrack(lookup.MusicBrainz, info.Title, info.Duration)
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
	return counts, nil
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
	if body, err := os.ReadFile(path); err == nil && len(flactags.FrontCover(body)) == 0 {
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
		track, probeErr := probeLocalWorkflowTrack(context.Background(), path)
		if probeErr != nil {
			return "", "", "", probeErr
		}
		artist, album, title = track.Artist, track.Album, track.Title
	}
	return artist, album, title, nil
}

func (a *App) writeAutomationArtwork(ctx context.Context, path string) bool {
	artist, album, title, err := automationTrackIdentity(path)
	if err != nil {
		return false
	}
	request, cancel := context.WithTimeout(ctx, 20*time.Second)
	lyrics, cover, fetchErr := a.fetchArtworkContext(request, "", "", artist, album, title)
	cancel()
	if fetchErr != nil {
		return false
	}
	wrote := false
	if strings.TrimSpace(lyrics) != "" && !hasLyricsSidecar(path) {
		if writeLyricsSidecar(path, lyrics) == nil {
			wrote = true
		}
	}
	if len(cover) > 0 {
		sidecar := strings.TrimSuffix(path, filepath.Ext(path)) + ".jpg"
		if _, err := os.Stat(sidecar); os.IsNotExist(err) {
			if writeAutomationSidecar(sidecar, cover) == nil {
				wrote = true
			}
		}
	}
	return wrote
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
	return os.Rename(name, path)
}

func (a *App) runAutomatedDeduplication(ctx context.Context) {
	state := a.libraryAutomation
	for merged := 0; merged < libraryAutomationMaxMerges; merged++ {
		if ctx.Err() != nil {
			return
		}
		groups, _, _, err := a.scanDuplicates(ctx)
		if err != nil {
			state.addError(fmt.Errorf("重复扫描失败: %w", err))
			return
		}
		groupID := ""
		readyCount := 0
		for i := range groups {
			if groups[i].State == "ready" && !a.groupSuppressed(&groups[i]) {
				readyCount++
				if groupID == "" {
					groupID = groups[i].GroupID
				}
			}
		}
		state.update(func(report *libraryAutomationReport) {
			if readyCount > report.DuplicateGroups {
				report.DuplicateGroups = readyCount
			}
		})
		if groupID == "" {
			return
		}
		status, raw, mergeErr := a.libraryMaintenancePost(ctx, "/v1/library/duplicates/merge", a.duplicateMerge, map[string]any{"group_id": groupID})
		if mergeErr != nil || status < 200 || status >= 300 {
			state.addError(fmt.Errorf("自动去重 %s 失败: %v %s", groupID, mergeErr, strings.TrimSpace(string(raw))))
			return
		}
		state.update(func(report *libraryAutomationReport) { report.Deduplicated++ })
	}
}

func (a *App) enqueueLibraryMaintenanceWorkflows(ctx context.Context, byDir map[string][]string, settings libraryAutomationSettings) {
	state := a.libraryAutomation
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
		files := byDir[dir]
		tracks := make([]localWorkflowTrack, 0, len(files))
		for _, path := range files {
			track, err := probeLocalWorkflowTrack(ctx, path)
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
			for start := 0; start < len(lossy) && jobs < libraryAutomationMaxJobs; start += 32 {
				end := start + 32
				if end > len(lossy) {
					end = len(lossy)
				}
				if a.enqueueMaintenanceWorkflow(ctx, "upgrade", lossy[start:end], dir) {
					jobs++
					state.update(func(report *libraryAutomationReport) { report.UpgradeJobs++ })
				}
			}
		}
		if settings.Expand && len(tracks) == 1 && !isLossyUpgradeSource(tracks[0].Codec) && jobs < libraryAutomationMaxJobs {
			if a.enqueueMaintenanceWorkflow(ctx, "expand", tracks, dir) {
				jobs++
				state.update(func(report *libraryAutomationReport) { report.ExpandJobs++ })
			}
		}
	}
}

func (a *App) enqueueMaintenanceWorkflow(ctx context.Context, scene string, tracks []localWorkflowTrack, dir string) bool {
	if len(tracks) == 0 {
		return false
	}
	paths := make([]string, 0, len(tracks))
	for _, track := range tracks {
		paths = append(paths, track.Path)
	}
	sort.Strings(paths)
	key := scene + "|" + filepath.Clean(dir) + "|" + strings.Join(paths, "|")
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
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)).WithContext(ctx)
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
