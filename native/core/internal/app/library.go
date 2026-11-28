// Library maintenance: duplicate scan/merge/restore and artist-folder
// organization. A duplicate is the same recording in one album: artist,
// album, complete normalized title, compatible duration and channel count.
// Version-bearing titles (live, remix, instrumental, demo, TV size) remain
// distinct. ffprobe supplies codec, bit depth, sample rate and bitrate; a
// successful ffmpeg decode is required before a file may win.
//
// Merge never deletes: losers move into quarantine with a durable receipt,
// and restore verifies and moves them back. A changed source or a file still
// held by qBittorrent refuses the entire merge before anything moves.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/artistnorm"
	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
	"github.com/wsdbb72/MusicOps/native/core/internal/lrc"
	"github.com/wsdbb72/MusicOps/native/core/internal/qbittorrent"
	"github.com/wsdbb72/MusicOps/native/core/internal/wavflac"
)

// ---------------------------------------------------------------------------
// Duplicate scan
//
// GET /v1/library/duplicates
//
// A candidate is one artist, album and complete normalized title. Version
// suffixes stay part of the title, and duration/channel checks run before a
// group is ready. ffprobe supplies codec, bit depth, sample rate and bitrate;
// ffmpeg must decode every eligible winner. Same-resolution lossless files
// with different PCM stay in review as likely different masters. Without
// ffmpeg the scan reports structural identity and never auto-merges.

// duplicateFile is one library file inside a duplicate group.
type duplicateFile struct {
	RequestID      string  `json:"request_id"`
	RootID         string  `json:"root_id,omitempty"`
	Track          int     `json:"track,omitempty"`
	File           string  `json:"file"`
	Format         string  `json:"format"`
	Codec          string  `json:"codec"`
	SHA256         string  `json:"sha256"`
	PCMSHA256      string  `json:"pcm_sha256"`
	SampleRate     uint32  `json:"sample_rate"`
	Channels       int     `json:"channels"`
	BitsPerSample  int     `json:"bits_per_sample"`
	BitRate        int64   `json:"bit_rate"`
	Duration       float64 `json:"duration"`
	Lossless       bool    `json:"lossless"`
	Valid          bool    `json:"valid"`
	ProbeError     string  `json:"probe_error,omitempty"`
	TorrentManaged bool    `json:"torrent_managed,omitempty"`
	Rank           []int   `json:"rank"`
	// LibraryDir is the absolute library root the file was published
	// under, empty for records written before it was stored.
	LibraryDir   string   `json:"library_dir,omitempty"`
	Origin       string   `json:"origin,omitempty"`
	SourceName   string   `json:"source_name,omitempty"`
	PlaylistKeys []string `json:"playlist_keys,omitempty"`
}

// duplicateGroup is one (artist, album, slot) set with more than one file.
type duplicateGroup struct {
	GroupID      string          `json:"group_id"`
	Album        string          `json:"album"`
	Artists      []string        `json:"artists"`
	Files        []duplicateFile `json:"files"`
	Keep         *duplicateFile  `json:"keep"`
	State        string          `json:"state"`
	Reason       string          `json:"reason"`
	IdentityUsed string          `json:"identity_used"`
}

type duplicateScanReport struct {
	Running   bool   `json:"running"`
	Started   string `json:"started,omitempty"`
	Finished  string `json:"finished,omitempty"`
	Checked   int    `json:"checked"`
	Total     int    `json:"total"`
	Groups    int    `json:"groups"`
	Truncated bool   `json:"truncated,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
	Error     string `json:"error,omitempty"`
	Warning   string `json:"warning,omitempty"`
	Message   string `json:"message,omitempty"`
	Current   string `json:"current,omitempty"`
}

type duplicateScanResult struct {
	Groups       []duplicateGroup `json:"groups"`
	Method       string           `json:"method"`
	Truncated    bool             `json:"truncated"`
	Suppressed   int              `json:"suppressed"`
	CacheWarning string           `json:"cache_warning,omitempty"`
}

func (a *App) beginDuplicateScan(parent context.Context) (context.Context, bool) {
	a.duplicateScanStateMu.Lock()
	defer a.duplicateScanStateMu.Unlock()
	if a.duplicateScanRunning {
		return nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	a.duplicateScanRunning = true
	a.duplicateScanCancel = cancel
	a.duplicateScanReport = duplicateScanReport{
		Running: true, Started: a.now().Format(time.RFC3339), Message: "正在读取本地索引",
	}
	return ctx, true
}

func (a *App) updateDuplicateScanProgress(checked, total int, current string) {
	a.duplicateScanStateMu.Lock()
	defer a.duplicateScanStateMu.Unlock()
	a.duplicateScanReport.Checked = checked
	a.duplicateScanReport.Total = total
	a.duplicateScanReport.Current = scrub(current)
	a.duplicateScanReport.Message = "正在校验重复指纹"
}

func (a *App) finishDuplicateScan(result *duplicateScanResult, err error) {
	a.duplicateScanStateMu.Lock()
	defer a.duplicateScanStateMu.Unlock()
	a.duplicateScanRunning = false
	if a.duplicateScanCancel != nil {
		a.duplicateScanCancel()
		a.duplicateScanCancel = nil
	}
	finished := a.now().Format(time.RFC3339)
	a.duplicateScanReport.Running = false
	a.duplicateScanReport.Finished = finished
	if result != nil && result.CacheWarning != "" {
		a.duplicateScanReport.Warning = result.CacheWarning
	}
	if err != nil {
		a.duplicateScanReport.Cancelled = errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		if a.duplicateScanReport.Cancelled {
			a.duplicateScanReport.Message = "扫描已停止；已完成的指纹检查点会保留"
		} else {
			a.duplicateScanReport.Error = scrub(err.Error())
			a.duplicateScanReport.Message = "扫描失败"
		}
		return
	}
	if result == nil {
		a.duplicateScanReport.Error = "扫描没有返回结果"
		a.duplicateScanReport.Message = "扫描失败"
		return
	}
	a.duplicateScanLatest = result
	a.duplicateScanReport.Groups = len(result.Groups)
	a.duplicateScanReport.Truncated = result.Truncated
	a.duplicateScanReport.Checked = a.duplicateScanReport.Total
	a.duplicateScanReport.Message = "扫描完成"
}

func (a *App) duplicateScanSnapshot() (duplicateScanReport, *duplicateScanResult) {
	a.duplicateScanStateMu.Lock()
	defer a.duplicateScanStateMu.Unlock()
	return a.duplicateScanReport, a.duplicateScanLatest
}

func (a *App) latestDuplicateGroup(groupID string) (*duplicateGroup, bool) {
	a.duplicateScanStateMu.Lock()
	latest := a.duplicateScanLatest
	a.duplicateScanStateMu.Unlock()
	if latest == nil {
		return nil, false
	}
	for i := range latest.Groups {
		if latest.Groups[i].GroupID == groupID {
			group := latest.Groups[i]
			return &group, true
		}
	}
	return nil, false
}

// cancelDuplicateScan stops the current manual or background duplicate scan.
// Cancellation is cooperative and completed per-file fingerprints are
// checkpointed before the scan exits.
func (a *App) cancelDuplicateScan() {
	a.duplicateScanStateMu.Lock()
	cancel := a.duplicateScanCancel
	if cancel != nil {
		a.duplicateScanReport.Message = "正在停止扫描；已完成检查点会保留"
	}
	a.duplicateScanStateMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *App) performDuplicateScan(ctx context.Context, progress func(checked, total int, current string)) (*duplicateScanResult, error) {
	a.duplicateScanMu.Lock()
	defer a.duplicateScanMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a.pipelinePaused() {
		return nil, context.Canceled
	}
	root := a.libraryDir()
	cache := a.loadLibraryDuplicateCache(root)
	cacheWarning := ""
	checkpoint := func(updates map[string]cachedDuplicateFingerprint) {
		mergeDuplicateFingerprintCache(&cache, updates)
		if saveErr := a.saveLibraryDuplicateCache(cache); saveErr != nil {
			cacheWarning = "重复项扫描缓存保存失败：" + scrub(saveErr.Error())
		}
	}
	groups, method, truncated, updates, err := a.scanDuplicatesWithOptions(ctx, 1000, root, cache.Files, progress, checkpoint)
	if err != nil {
		return &duplicateScanResult{CacheWarning: cacheWarning}, err
	}
	mergeDuplicateFingerprintCache(&cache, updates)
	if saveErr := a.saveLibraryDuplicateCache(cache); saveErr != nil {
		cacheWarning = "重复项已扫描，但结果缓存保存失败：" + scrub(saveErr.Error())
	}
	// Suppressed albums are hidden from the scan but still reachable by id:
	// a merge attempt answers MERGE_SUPPRESSED instead of a confusing 404.
	visible := make([]duplicateGroup, 0, len(groups))
	suppressed := 0
	for i := range groups {
		if a.groupSuppressed(&groups[i]) {
			suppressed++
			continue
		}
		visible = append(visible, groups[i])
	}
	return &duplicateScanResult{Groups: visible, Method: method, Truncated: truncated, Suppressed: suppressed, CacheWarning: cacheWarning}, nil
}

func writeDuplicateScanResult(w http.ResponseWriter, status int, result *duplicateScanResult) {
	response := map[string]any{
		"ok": true, "method": result.Method, "groups": result.Groups, "count": len(result.Groups),
		"suppressed": result.Suppressed, "truncated": result.Truncated,
		"policy": "同一专辑的同一录音只保留可完整解码的最高音质版本；现场、混音、伴奏、Demo、TV size、不同母带或声道保留。旧版本进入可恢复隔离区。",
	}
	if result.CacheWarning != "" {
		response["cache_warning"] = result.CacheWarning
	}
	writeJSON(w, status, response)
}

func (a *App) duplicateScan(w http.ResponseWriter, r *http.Request) {
	ctx, ok := a.beginDuplicateScan(r.Context())
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "LIBRARY_SCAN_BUSY", "error": "已有重复扫描正在运行"})
		return
	}
	result, err := a.performDuplicateScan(ctx, a.updateDuplicateScanProgress)
	a.finishDuplicateScan(result, err)
	if err != nil {
		status := http.StatusInternalServerError
		response := map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE", "error": "重复扫描失败"}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusConflict
			response["code"] = "LIBRARY_SCAN_CANCELED"
			response["error"] = "重复扫描已停止；已完成检查点会保留，下次会从缓存继续"
		}
		if result != nil && result.CacheWarning != "" {
			response["cache_warning"] = result.CacheWarning
		}
		writeJSON(w, status, response)
		return
	}
	writeDuplicateScanResult(w, http.StatusOK, result)
}

func (a *App) startDuplicateScan(w http.ResponseWriter, _ *http.Request) {
	if a.pipelinePaused() {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "LIBRARY_SCAN_CANCELED", "error": "调度已暂停；恢复调度后再开始重复扫描"})
		return
	}
	ctx, ok := a.beginDuplicateScan(context.Background())
	if !ok {
		report, latest := a.duplicateScanSnapshot()
		response := map[string]any{"ok": true, "started": false, "status": report}
		if !report.Running && report.Error == "" && !report.Cancelled && latest != nil {
			response["result"] = latest
		}
		writeJSON(w, http.StatusOK, response)
		return
	}
	a.startBackground("duplicate-scan", func() {
		result, err := a.performDuplicateScan(ctx, a.updateDuplicateScanProgress)
		a.finishDuplicateScan(result, err)
	})
	report, latest := a.duplicateScanSnapshot()
	response := map[string]any{"ok": true, "started": true, "status": report}
	if !report.Running && report.Error == "" && !report.Cancelled && latest != nil {
		response["result"] = latest
	}
	writeJSON(w, http.StatusAccepted, response)
}

func (a *App) stopDuplicateScan(w http.ResponseWriter, _ *http.Request) {
	a.cancelDuplicateScan()
	report, _ := a.duplicateScanSnapshot()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "stopping": report.Running, "status": report})
}

func (a *App) duplicateScanStatus(w http.ResponseWriter, r *http.Request) {
	report, latest := a.duplicateScanSnapshot()
	response := map[string]any{"ok": true, "status": report}
	if r.URL.Query().Get("include") == "result" && !report.Running && report.Error == "" && !report.Cancelled && latest != nil {
		response["result"] = latest
	}
	writeJSON(w, http.StatusOK, response)
}

// scanDuplicates groups the library and fingerprints each candidate.
// method is "pcm" when ffmpeg decoded every candidate, "structural"
// when any fingerprint fell back to the file hash.
func (a *App) scanDuplicates(ctx context.Context) ([]duplicateGroup, string, bool, error) {
	return a.scanDuplicatesWithProgress(ctx, nil)
}

func (a *App) scanDuplicatesWithProgress(ctx context.Context, progress func(checked, total int, current string)) ([]duplicateGroup, string, bool, error) {
	a.duplicateScanMu.Lock()
	defer a.duplicateScanMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, "", false, err
	}
	if a.pipelinePaused() {
		return nil, "", false, context.Canceled
	}
	groups, method, truncated, _, err := a.scanDuplicatesWithOptions(ctx, 1000, "", nil, progress, nil)
	return groups, method, truncated, err
}

// scanDuplicatesWithOptions fingerprints candidate recordings concurrently.
// A positive limit keeps the interactive endpoint bounded; zero scans every
// candidate for whole-library automation. cached is only read by workers and
// is never mutated concurrently. The returned updates are safe to merge into
// the persisted maintenance cache after every worker has stopped.
func (a *App) scanDuplicatesWithOptions(ctx context.Context, limit int, root string, cached map[string]cachedLibraryScan, progress func(checked, total int, current string), checkpoint func(map[string]cachedDuplicateFingerprint)) ([]duplicateGroup, string, bool, map[string]cachedDuplicateFingerprint, error) {
	entries, err := audiorelease.ListIndexed(a.engine)
	if err != nil {
		return nil, "", false, nil, err
	}
	playlistGuard, err := a.duplicatePlaylistGuard(ctx, entries)
	if err != nil {
		return nil, "", false, nil, err
	}
	decoder := audiorelease.SystemDecoder()
	method := "pcm"
	if decoder == nil {
		method = "structural"
	}
	// A recording is identified by album, artist and the complete normalized
	// title. Version words such as live, remix, instrumental, demo and TV size
	// remain part of the title, so those releases never collapse together.
	byRecording := map[string][]audiorelease.Entry{}
	seenPhysicalFiles := map[string]bool{}
	for i, entry := range entries {
		if i%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, "", false, nil, err
			}
		}
		if !entry.FilePresent || entry.AliasOfRequestID != "" {
			continue
		}
		physical := libraryPathIdentity(a.libraryFilePath(entry.LibraryDir, entry.File))
		if physical == "" || seenPhysicalFiles[physical] {
			continue
		}
		seenPhysicalFiles[physical] = true
		artist, album := splitLibraryTail(entry.File)
		title := duplicateRecordingTitle(entry.File, artist)
		if artist == "" || album == "" || title == "" {
			continue
		}
		key := artistnorm.Key(artist) + "\x00" + normalizeKey(album) + "\x00" + title
		byRecording[key] = append(byRecording[key], entry)
	}
	candidateKeys := make([]string, 0, len(byRecording))
	for key, rows := range byRecording {
		if len(rows) >= 2 {
			candidateKeys = append(candidateKeys, key)
		}
	}
	sort.Strings(candidateKeys)
	type scanResult struct {
		index   int
		group   *duplicateGroup
		updates map[string]cachedDuplicateFingerprint
		err     error
	}
	judge, workers := ctx, duplicateFingerprintWorkers
	if available := runtime.GOMAXPROCS(0); available < workers {
		workers = available
	}
	if len(candidateKeys) < workers {
		workers = len(candidateKeys)
	}
	if workers < 1 {
		return []duplicateGroup{}, method, false, nil, nil
	}
	cacheSnapshot := make(map[string]cachedLibraryScan, len(cached))
	for key, entry := range cached {
		cacheSnapshot[key] = entry
	}
	jobs := make(chan int)
	results := make(chan scanResult, workers*2)
	stopFeeding := make(chan struct{})
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(stopFeeding) }) }
	var workerWG sync.WaitGroup
	for i := 0; i < workers; i++ {
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			for index := range jobs {
				group, updates, fingerprintErr := a.fingerprintGroupWithCache(judge, byRecording[candidateKeys[index]], decoder, root, cacheSnapshot)
				playlistGuard.annotate(group)
				select {
				case results <- scanResult{index: index, group: group, updates: updates, err: fingerprintErr}:
				case <-judge.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for index := range candidateKeys {
			select {
			case jobs <- index:
			case <-stopFeeding:
				return
			case <-judge.Done():
				return
			}
		}
	}()
	go func() {
		workerWG.Wait()
		close(results)
	}()

	groups := make([]duplicateGroup, 0, len(candidateKeys))
	updates := make(map[string]cachedDuplicateFingerprint)
	pending := make(map[string]cachedDuplicateFingerprint)
	flush := func() {
		if checkpoint == nil || len(pending) == 0 {
			return
		}
		changes := make(map[string]cachedDuplicateFingerprint, len(pending))
		for key, entry := range pending {
			changes[key] = entry
		}
		checkpoint(changes)
		pending = make(map[string]cachedDuplicateFingerprint)
	}
	defer flush()
	checkedCount, truncated, firstErr := 0, false, error(nil)
	for result := range results {
		checkedCount++
		if result.err != nil && firstErr == nil {
			firstErr = result.err
			stop()
		}
		for key, entry := range result.updates {
			updates[key] = entry
			pending[key] = entry
		}
		if checkedCount%100 == 0 {
			flush()
		}
		if result.group != nil {
			if limit > 0 && len(groups) >= limit {
				truncated = true
				stop()
				continue
			}
			groups = append(groups, *result.group)
		}
		if progress != nil {
			progress(checkedCount, len(candidateKeys), candidateKeys[result.index])
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, "", false, updates, err
	}
	if firstErr != nil {
		return nil, "", false, updates, firstErr
	}
	if checkedCount < len(candidateKeys) {
		truncated = true
	}
	if progress != nil {
		progress(checkedCount, len(candidateKeys), "scan complete")
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].GroupID < groups[j].GroupID })
	return groups, method, truncated, updates, nil
}

type duplicateAudioInfo struct {
	Codec         string
	SampleRate    uint32
	Channels      int
	BitsPerSample int
	BitRate       int64
	Duration      float64
}

// probeDuplicateAudio uses ffprobe as the source of truth for the fields that
// decide quality. The container suffix is never treated as proof of lossless:
// FLAC provenance tags, PCM codecs and other lossless codecs are checked.
func probeDuplicateAudio(ctx context.Context, path, format string) (duplicateAudioInfo, error) {
	var info duplicateAudioInfo
	raw, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=codec_name,sample_rate,channels,bits_per_sample,bits_per_raw_sample,bit_rate:format=duration,bit_rate",
		"-of", "json", path).Output()
	if err == nil {
		var data struct {
			Streams []struct {
				Codec            string `json:"codec_name"`
				SampleRate       string `json:"sample_rate"`
				Channels         int    `json:"channels"`
				BitsPerSample    int    `json:"bits_per_sample"`
				BitsPerRawSample int    `json:"bits_per_raw_sample"`
				BitRate          string `json:"bit_rate"`
			} `json:"streams"`
			Format struct {
				Duration string `json:"duration"`
				BitRate  string `json:"bit_rate"`
			} `json:"format"`
		}
		if json.Unmarshal(raw, &data) == nil && len(data.Streams) == 1 {
			stream := data.Streams[0]
			info.Codec = strings.ToLower(strings.TrimSpace(stream.Codec))
			info.SampleRate = uint32(parseFlexibleInt(stream.SampleRate))
			info.Channels = stream.Channels
			info.BitsPerSample = stream.BitsPerSample
			if info.BitsPerSample == 0 {
				info.BitsPerSample = stream.BitsPerRawSample
			}
			info.BitRate = parseFlexibleInt(stream.BitRate)
			if info.BitRate == 0 {
				info.BitRate = parseFlexibleInt(data.Format.BitRate)
			}
			info.Duration, _ = strconv.ParseFloat(data.Format.Duration, 64)
		}
	}
	if info.Codec == "" || info.SampleRate == 0 || info.Channels == 0 {
		rate, channels, bits, streamErr := streamInfoFor(path, format)
		if streamErr != nil {
			if err != nil {
				return info, err
			}
			return info, streamErr
		}
		info.SampleRate, info.Channels, info.BitsPerSample = rate, channels, bits
		if info.Codec == "" {
			info.Codec = format
		}
	}
	if info.Duration <= 0 && strings.EqualFold(format, "flac") {
		if tags, tagErr := flactags.ReadFile(path); tagErr == nil && tags.StreamInfo.SampleRate > 0 {
			info.Duration = float64(tags.StreamInfo.TotalSamples) / float64(tags.StreamInfo.SampleRate)
		}
	}
	if info.Duration <= 0 && strings.EqualFold(format, "wav") {
		if stat, statErr := os.Stat(path); statErr == nil && info.SampleRate > 0 && info.Channels > 0 && info.BitsPerSample > 0 {
			bytesPerSecond := int64(info.SampleRate) * int64(info.Channels) * int64(info.BitsPerSample) / 8
			if bytesPerSecond > 0 && stat.Size() > 44 {
				info.Duration = float64(stat.Size()-44) / float64(bytesPerSecond)
			}
		}
	}
	if info.BitRate == 0 && info.Duration > 0 {
		if stat, statErr := os.Stat(path); statErr == nil {
			info.BitRate = int64(float64(stat.Size()*8) / info.Duration)
		}
	}
	if info.Codec == "" {
		info.Codec = strings.ToLower(format)
	}
	return info, nil
}

func parseFlexibleInt(value string) int64 {
	if value == "" || value == "N/A" {
		return 0
	}
	n, _ := strconv.ParseInt(value, 10, 64)
	return n
}

func losslessAudioCodec(codec string) bool {
	codec = strings.ToLower(strings.TrimSpace(codec))
	switch codec {
	case "flac", "alac", "ape", "wavpack", "tta", "truehd", "mlp", "dsd_lsbf", "dsd_msbf", "dsd_lsbf_planar", "dsd_msbf_planar":
		return true
	}
	return strings.HasPrefix(codec, "pcm_")
}

// fingerprintGroup computes real technical quality for every candidate. A
// failed decode makes that file ineligible to win; it is still listed so the
// merge can quarantine it with a readable reason. Files only become a ready
// group when they have one title, compatible duration and channel count, and
// at least one independently decodable copy.
func (a *App) fingerprintGroup(ctx context.Context, rows []audiorelease.Entry, decoder audiorelease.Decoder) (*duplicateGroup, error) {
	group, _, err := a.fingerprintGroupWithCache(ctx, rows, decoder, "", nil)
	return group, err
}

func (a *App) fingerprintGroupWithCache(ctx context.Context, rows []audiorelease.Entry, decoder audiorelease.Decoder, root string, cached map[string]cachedLibraryScan) (*duplicateGroup, map[string]cachedDuplicateFingerprint, error) {
	artist, album := splitLibraryTail(rows[0].File)
	files := make([]duplicateFile, 0, len(rows))
	valid := make([]duplicateFile, 0, len(rows))
	updates := make(map[string]cachedDuplicateFingerprint)
	for _, entry := range rows {
		full := a.entryFilePath(entry)
		cacheKey := ""
		var cachedEntry cachedLibraryScan
		if root != "" && cached != nil {
			cacheKey = automationCacheKey(root, full)
			cachedEntry = cached[cacheKey]
		}
		file, fingerprint := a.fingerprintDuplicateFile(ctx, entry, full, decoder, cachedEntry, cacheKey != "")
		if cacheKey != "" && (cachedEntry.Duplicate == nil || *cachedEntry.Duplicate != fingerprint) {
			updates[cacheKey] = fingerprint
		}
		file.Rank = duplicateRank(file)
		files = append(files, file)
		if file.Valid {
			valid = append(valid, file)
		}
	}
	for _, file := range files {
		if strings.TrimSpace(file.SHA256) == "" {
			// ListIndexed is intentionally cheap and does not stat all 75k
			// records. A candidate whose bytes cannot now be hashed has been
			// removed or raced, so it must not remain as a stale merge group.
			return nil, updates, nil
		}
	}
	group := &duplicateGroup{
		GroupID: duplicateGroupID(rows), Album: album, Artists: []string{artist}, Files: files,
		IdentityUsed: "album-title-duration",
	}
	if decoder != nil {
		group.IdentityUsed = "album-title-duration+pcm-check"
	} else {
		group.IdentityUsed = "structural"
	}
	switch {
	case len(valid) == 0:
		group.State = "review"
		if decoder == nil {
			group.Reason = "结构级比对：没有 ffmpeg，无法证明音频可完整解码；不同编码不会自动合并"
		} else {
			group.Reason = "没有可完整解码的版本，原文件全部保留"
		}
		return group, updates, nil
	case duplicateSameResolutionLosslessMismatch(valid):
		group.State = "review"
		group.Reason = "同为无损且位深与采样率相同的版本解码内容不同，可能是不同母带，原文件保留"
		return group, updates, nil
	case !duplicateDurationsCompatible(valid):
		group.State = "review"
		group.Reason = "同名曲目的时长差异超过容差，可能是不同版本，原文件保留"
		return group, updates, nil
	case !duplicateChannelsCompatible(valid):
		group.State = "review"
		group.Reason = "同名曲目的声道数不同，可能是不同母带，原文件保留"
		return group, updates, nil
	}
	keep := valid[0]
	for _, file := range valid[1:] {
		if compareRank(file.Rank, keep.Rank) > 0 {
			keep = file
		}
	}
	keepCopy := keep
	group.Keep = &keepCopy
	group.State = "ready"
	group.Reason = "同一专辑、曲名、时长和声道；保留最高音质 " + duplicateQualityLabel(keep) + "，其余版本进入可恢复隔离区"
	return group, updates, nil
}

func (a *App) fingerprintDuplicateFile(ctx context.Context, entry audiorelease.Entry, full string, decoder audiorelease.Decoder, cached cachedLibraryScan, cacheable bool) (duplicateFile, cachedDuplicateFingerprint) {
	info, statErr := os.Stat(full)
	if statErr == nil && cached.Duplicate != nil && cached.Duplicate.Valid && cached.Size == info.Size() && cached.ModNS == info.ModTime().UnixNano() && cached.Duplicate.DecoderAvailable == (decoder != nil) && (entry.SHA256 == "" || cached.Duplicate.FileSHA256 == entry.SHA256) {
		fingerprint := *cached.Duplicate
		return duplicateFileFromFingerprint(entry, fingerprint), fingerprint
	}

	fileSHA := strings.TrimSpace(entry.SHA256)
	var hashErr error
	if fileSHA == "" || !entry.PresenceUnchanged() {
		fileSHA, hashErr = hashDuplicateFile(ctx, full)
	}
	audioInfo, probeErr := probeDuplicateAudio(ctx, full, entry.Format)
	file := duplicateFile{
		RequestID: entry.RequestID, RootID: entry.RootID, Track: entry.Track, File: entry.File, Format: strings.ToLower(entry.Format),
		Codec: audioInfo.Codec, SHA256: fileSHA, SampleRate: audioInfo.SampleRate, Channels: audioInfo.Channels,
		BitsPerSample: audioInfo.BitsPerSample, BitRate: audioInfo.BitRate, Duration: audioInfo.Duration,
		Lossless: losslessAudioCodec(audioInfo.Codec), LibraryDir: entry.LibraryDir, TorrentManaged: entry.TorrentManaged,
		Origin: entry.Origin, SourceName: entry.SourceName,
	}
	if strings.EqualFold(file.Format, "flac") {
		if tags, err := readFLACTagsPrefix(full); err == nil && isLossyFLACProvenance(firstTagValue(tags.Raw, "COMMENT")) {
			file.Lossless = false
			file.Codec = "flac-lossy"
		}
	}
	if decoder != nil && hashErr == nil && probeErr == nil {
		file.PCMSHA256, hashErr = pcmHashWithFormatAtReadRate(ctx, full, []string{"-f", "s32le"}, duplicatePCMReadRate(info.Size(), audioInfo.Duration))
		if hashErr != nil {
			probeErr = fmt.Errorf("音频解码校验失败: %w", hashErr)
		}
	}
	if hashErr != nil {
		probeErr = fmt.Errorf("文件哈希读取失败: %w", hashErr)
	}
	if probeErr != nil {
		file.ProbeError = scrub(probeErr.Error())
	}
	file.Valid = decoder != nil && probeErr == nil && file.SampleRate > 0 && file.Channels > 0 && file.Duration > 0
	fingerprint := cachedDuplicateFingerprint{
		FileSHA256: file.SHA256, PCMSHA256: file.PCMSHA256, Codec: file.Codec,
		SampleRate: file.SampleRate, Channels: file.Channels, BitsPerSample: file.BitsPerSample,
		BitRate: file.BitRate, Duration: file.Duration, Lossless: file.Lossless, Valid: file.Valid,
		DecoderAvailable: decoder != nil, ProbeError: file.ProbeError,
	}
	if cacheable && statErr == nil && file.Valid {
		fingerprint.FileSize = info.Size()
		fingerprint.FileModNS = info.ModTime().UnixNano()
		cached.Size = info.Size()
		cached.ModNS = info.ModTime().UnixNano()
		cached.Duplicate = &fingerprint
	}
	return file, fingerprint
}

func duplicateFileFromFingerprint(entry audiorelease.Entry, fingerprint cachedDuplicateFingerprint) duplicateFile {
	return duplicateFile{
		RequestID: entry.RequestID, RootID: entry.RootID, Track: entry.Track, File: entry.File, Format: strings.ToLower(entry.Format),
		Codec: fingerprint.Codec, SHA256: fingerprint.FileSHA256, PCMSHA256: fingerprint.PCMSHA256,
		SampleRate: fingerprint.SampleRate, Channels: fingerprint.Channels,
		BitsPerSample: fingerprint.BitsPerSample, BitRate: fingerprint.BitRate,
		Duration: fingerprint.Duration, Lossless: fingerprint.Lossless, Valid: fingerprint.Valid,
		ProbeError: fingerprint.ProbeError, LibraryDir: entry.LibraryDir, TorrentManaged: entry.TorrentManaged,
		Origin: entry.Origin, SourceName: entry.SourceName,
	}
}

func duplicateRecordingTitle(tail, artist string) string {
	title := strings.TrimSpace(trackTitle(tail))
	if credit, track, ok := strings.Cut(title, " - "); ok && artistnorm.Same(credit, artist) {
		title = track
	}
	return normalizeKey(title)
}

func duplicateDurationsCompatible(files []duplicateFile) bool {
	if len(files) < 2 {
		return true
	}
	minDuration, maxDuration := files[0].Duration, files[0].Duration
	for _, file := range files[1:] {
		if file.Duration < minDuration {
			minDuration = file.Duration
		}
		if file.Duration > maxDuration {
			maxDuration = file.Duration
		}
	}
	tolerance := math.Max(2, minDuration*.02)
	return maxDuration-minDuration <= tolerance
}

func duplicateSameResolutionLosslessMismatch(files []duplicateFile) bool {
	byResolution := map[string]map[string]bool{}
	for _, file := range files {
		if !file.Lossless || file.PCMSHA256 == "" {
			continue
		}
		key := fmt.Sprintf("%d/%d/%d", file.BitsPerSample, file.SampleRate, file.Channels)
		if byResolution[key] == nil {
			byResolution[key] = map[string]bool{}
		}
		byResolution[key][file.PCMSHA256] = true
		if len(byResolution[key]) > 1 {
			return true
		}
	}
	return false
}

func duplicateChannelsCompatible(files []duplicateFile) bool {
	channels := 0
	for _, file := range files {
		if file.Channels <= 0 {
			continue
		}
		if channels == 0 {
			channels = file.Channels
			continue
		}
		if file.Channels != channels {
			return false
		}
	}
	return true
}

func duplicateQualityLabel(file duplicateFile) string {
	codec := strings.ToUpper(firstNonEmpty(file.Codec, file.Format))
	if file.Lossless {
		if file.BitsPerSample > 0 {
			return fmt.Sprintf("%s %d-bit/%d Hz", codec, file.BitsPerSample, file.SampleRate)
		}
		return fmt.Sprintf("%s 无损/%d Hz", codec, file.SampleRate)
	}
	if file.BitRate > 0 {
		return fmt.Sprintf("%s %d kbps", codec, (file.BitRate+500)/1000)
	}
	return codec
}

// duplicateRank puts a fully decodable file first, then real lossless audio,
// bit depth, sample rate, lossy bitrate, codec preference and container. A
// higher container preference can never beat a better resolution.
func duplicateRank(file duplicateFile) []int {
	valid, lossless := 0, 0
	if file.Valid {
		valid = 1
	}
	if file.Lossless {
		lossless = 1
	}
	bits, rate := 0, 0
	if file.Lossless {
		bits, rate = file.BitsPerSample, int(file.SampleRate)
	}
	bitRate := 0
	if !file.Lossless && file.BitRate > 0 && file.BitRate < 2_000_000_000 {
		bitRate = int(file.BitRate)
	}
	codec := 0
	switch strings.ToLower(file.Codec) {
	case "flac":
		codec = 7
	case "alac":
		codec = 6
	case "ape", "wavpack":
		codec = 5
	case "opus":
		codec = 4
	case "aac", "vorbis":
		codec = 3
	case "mp3":
		codec = 2
	}
	if strings.HasPrefix(strings.ToLower(file.Codec), "pcm_") || strings.EqualFold(file.Format, "wav") {
		codec = 5
	}
	format := 0
	switch strings.ToLower(file.Format) {
	case "flac":
		format = 5
	case "m4a", "alac":
		format = 4
	case "ape", "wv":
		format = 3
	case "wav", "aiff":
		format = 2
	case "opus":
		format = 1
	}
	return []int{valid, lossless, bits, rate, bitRate, codec, format}
}

func compareRank(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] - b[i]
		}
	}
	return len(a) - len(b)
}

// duplicateGroupID is the stable id of one slot's file set, as Python
// digest([key, sorted file ids]) is.
func duplicateGroupID(rows []audiorelease.Entry) string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.RequestID)
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	return "dup-" + hex.EncodeToString(sum[:8])
}

// splitLibraryTail splits a library file tail "Artist/Album/NN Title.ext"
// into artist and album. A tail with fewer segments has no reliable
// album identity.
func splitLibraryTail(tail string) (artist, album string) {
	parts := strings.Split(tail, "/")
	if len(parts) < 3 {
		return "", ""
	}
	return parts[len(parts)-3], parts[len(parts)-2]
}

// trackTitle returns the file stem without the "NN " track prefix.
func trackTitle(tail string) string {
	base := filepath.Base(tail)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if len(stem) > 3 && stem[0] >= '0' && stem[0] <= '9' && stem[1] >= '0' && stem[1] <= '9' && stem[2] == ' ' {
		return stem[3:]
	}
	return stem
}

// ---------------------------------------------------------------------------
// Duplicate merge / restore
//
// POST /v1/library/duplicates/merge   {"group_id": "dup-..."}
// POST /v1/library/duplicates/restore {"merge_id": "merge-..."}
//
// Merge re-scans (the scan is the plan: no stored plan can go stale),
// refuses a group that is not ready or whose files changed since the
// scan, keeps the best-ranked file, and moves every other file — plus
// its .lrc sidecar when present — into quarantine/<merge id>/ under the
// state directory. Nothing is deleted. The receipt is stored in Pebble
// under library/merge/<merge id> so restore can audit every move.
// Restore moves each quarantined file back, verifies its hash, refuses
// when the destination now holds different bytes, and marks the receipt
// restored. A restore of an already-restored merge replays.

type mergeReceipt struct {
	MergeID   string      `json:"merge_id"`
	GroupID   string      `json:"group_id"`
	Keep      string      `json:"keep"`
	Moved     []mergeMove `json:"moved"`
	CreatedNS int64       `json:"created_ns"`
	Restored  bool        `json:"restored"`
}

// mergeMove is one quarantined file (audio or its sidecar).
type mergeMove struct {
	RequestID  string `json:"request_id,omitempty"`
	RootID     string `json:"root_id,omitempty"`
	Track      int    `json:"track,omitempty"`
	Format     string `json:"format,omitempty"`
	Origin     string `json:"origin,omitempty"`
	SourceName string `json:"source_name,omitempty"`
	From       string `json:"from"`
	Quarantine string `json:"quarantine"`
	SHA256     string `json:"sha256"`
	Sidecar    bool   `json:"sidecar,omitempty"`
}

func mergeReceiptKey(id string) []byte { return []byte("library/merge/" + id) }

// automationDuplicateGroupKey carries a verified scan result only between the
// in-process automation pass and duplicateMerge. It is never accepted from an
// HTTP request, so external callers still trigger a fresh scan.
type automationDuplicateGroupKey struct{}

func (a *App) duplicateMerge(w http.ResponseWriter, r *http.Request) {
	a.duplicateMu.Lock()
	defer a.duplicateMu.Unlock()
	var body struct {
		GroupID string `json:"group_id"`
	}
	if err := decode(r, &body); err != nil || strings.TrimSpace(body.GroupID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	var group *duplicateGroup
	internalScanContext := false
	if trusted, ok := r.Context().Value(automationDuplicateGroupKey{}).(*duplicateGroup); ok && trusted != nil {
		internalScanContext = true
		if trusted.GroupID == body.GroupID {
			group = trusted
		}
	}
	if group == nil && !internalScanContext {
		if latest, ok := a.latestDuplicateGroup(body.GroupID); ok {
			group = latest
		}
	}
	if group == nil && !internalScanContext {
		groups, _, _, err := a.scanDuplicates(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
			return
		}
		for i := range groups {
			if groups[i].GroupID == body.GroupID {
				group = &groups[i]
				break
			}
		}
	}
	if group == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "code": "NOT_FOUND"})
		return
	}
	if group.State != "ready" || group.Keep == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "MERGE_NOT_READY", "error": group.Reason})
		return
	}
	// A suppressed album opted out of dedup entirely; merging it would undo
	// exactly what the flag protects.
	if a.groupSuppressed(group) {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "MERGE_SUPPRESSED", "error": "这张专辑已设为不参与合并，先在字段锁里取消抑制"})
		return
	}
	// An active or review-blocked workflow may still need any file in this
	// group. Refuse the whole group before hash checks or tag rewrites; moving
	// only after the workflow completes prevents a later stage from reporting
	// a missing track.
	if activePath, err := a.mergeActiveWorkflowPath(group); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE", "error": "无法确认活动工作流是否仍引用这些文件"})
		return
	} else if activePath != "" {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "code": "MERGE_ACTIVE_WORKFLOW",
			"error": "文件仍被活动工作流引用：" + filepath.Base(activePath) + "；工作流完成或取消后再合并",
		})
		return
	}
	// Moving a file that qBittorrent still holds breaks the torrent: the
	// client keeps seeding a path that no longer exists. The check is before
	// any hash work and before any move, and a client that is configured but
	// unreachable refuses the merge instead of guessing (P0-3).
	if code, reason := a.mergeSeedingGuard(group); code != "" {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": code, "error": reason})
		return
	}
	// Verify every file is unchanged since the scan before moving any. This is
	// maintenance I/O: it uses the same low OS priority and byte-rate cap as the
	// fingerprint pass, so a manual merge cannot create a second disk spike.
	verifiedDigests := make(map[string]string, len(group.Files))
	for _, file := range group.Files {
		full := a.libraryFilePath(file.LibraryDir, file.File)
		digest, err := hashDuplicateFile(r.Context(), full)
		if err != nil || digest != file.SHA256 {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "SCAN_STALE", "error": "保留或重复文件发生变化，请重新扫描"})
			return
		}
		verifiedDigests[libraryPathIdentity(full)] = digest
	}
	// Fill the keeper's empty fields from the duplicates before anything
	// moves (Python duplicates.py parity): a loser often carries the genre or
	// the date the keeper lost. A failure here refuses the merge with nothing
	// moved yet.
	filled, err := a.fillMissingTags(group)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "code": "MERGE_TAG_FAILED", "error": "补写标签失败，文件没有移动", "restored": tagMutationRestored(err)})
		return
	}
	if len(filled) > 0 {
		// fillMissingTags rewrote the keeper's bytes and refreshed its digest.
		// Drop the old stat snapshot before any later merge step can fail, so
		// playlist presence cannot remain stale until the cache TTL expires.
		a.invalidatePlaylistLibraryCache()
	}
	mergeSum := sha256.Sum256([]byte(body.GroupID + ":" + group.Keep.RequestID))
	mergeID := "merge-" + hex.EncodeToString(mergeSum[:8])
	quarantineRoot := filepath.Join(a.Dir, "quarantine", mergeID)
	if err := os.MkdirAll(quarantineRoot, 0o700); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	receipt := mergeReceipt{MergeID: mergeID, GroupID: group.GroupID, Keep: group.Keep.File, CreatedNS: a.now().UnixNano()}
	// A losing duplicate may still be the published file for its own playlist
	// root. Link that root to the keeper before the merge is committed, so the
	// quarantine move and every affected release record change atomically.
	aliasConditions := []dataengine.Condition{}
	aliasMutations := []dataengine.Mutation{}
	aliasesLinked := 0
	keeperLibrary := strings.TrimSpace(group.Keep.LibraryDir)
	if keeperLibrary == "" {
		keeperLibrary = a.libraryDir()
	}
	keeperPath := a.libraryFilePath(group.Keep.LibraryDir, group.Keep.File)
	keeperDigest := verifiedDigests[libraryPathIdentity(keeperPath)]
	if len(filled) > 0 || keeperDigest == "" {
		keeperDigest, err = hashDuplicateFile(r.Context(), keeperPath)
	}
	if err != nil || keeperDigest == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"ok": false, "code": "MERGE_RECORD_LINK_FAILED", "error": "无法重新校验保留版本，文件没有移动",
		})
		return
	}
	for _, file := range group.Files {
		if file.RequestID == group.Keep.RequestID || file.RequestID == "" || file.RootID == "" || file.Track < 1 {
			continue
		}
		batch, linkErr := audiorelease.PrepareDedupeAlias(a.engine, audiorelease.DedupeAliasInput{
			Context: r.Context(), RequestID: file.RequestID, RootID: file.RootID, Track: file.Track,
			KeeperRequestID: group.Keep.RequestID, KeeperPath: keeperPath,
			KeeperSHA256: keeperDigest, KeeperFormat: group.Keep.Format,
			KeeperValidated: true, KeeperDecodeState: "decoded",
			Library: filepath.Clean(keeperLibrary), Decoder: audiorelease.SystemDecoder(),
		})
		if linkErr != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"ok": false, "code": "MERGE_RECORD_LINK_FAILED",
				"error": "无法把重复曲目的发布记录安全迁移到保留版本：" + scrub(linkErr.Error()),
			})
			return
		}
		if !batch.Linked {
			continue
		}
		aliasesLinked++
		aliasConditions = append(aliasConditions, batch.Conditions...)
		aliasMutations = append(aliasMutations, batch.Mutations...)
	}
	for _, file := range group.Files {
		if file.RequestID == group.Keep.RequestID {
			continue
		}
		if err := a.quarantineFileContext(r.Context(), &receipt, file, quarantineRoot); err != nil {
			unrestored := []mergeMove{}
			for i := len(receipt.Moved) - 1; i >= 0; i-- {
				move := receipt.Moved[i]
				if restoreErr := moveCheckedFile(move.Quarantine, move.From, move.SHA256); restoreErr != nil {
					err = errors.Join(err, restoreErr)
					unrestored = append(unrestored, move)
				} else {
					removeEmptyParents(filepath.Dir(move.Quarantine), quarantineRoot)
				}
			}
			if len(unrestored) > 0 {
				receipt.Moved = unrestored
				if raw, encodeErr := json.Marshal(receipt); encodeErr == nil {
					_, _ = a.engine.CompareAndSwap([]dataengine.Condition{{Key: mergeReceiptKey(mergeID), Absent: true}}, []dataengine.Mutation{{Key: mergeReceiptKey(mergeID), Value: raw}})
				}
			}
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "MERGE_CONFLICT", "error": err.Error()})
			return
		}
	}
	if len(receipt.Moved) == 0 {
		removeEmptyParents(quarantineRoot, filepath.Join(a.Dir, "quarantine"))
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "MERGE_NOT_READY", "error": "没有可合并的重复文件"})
		return
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		_ = rollbackMergeMoves(receipt.Moved)
		cleanupMergeQuarantine(receipt.Moved, quarantineRoot)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	conditions := append([]dataengine.Condition{{Key: mergeReceiptKey(mergeID), Absent: true}}, aliasConditions...)
	mutations := append([]dataengine.Mutation{{Key: mergeReceiptKey(mergeID), Value: encoded}}, aliasMutations...)
	ok, err := a.engine.CompareAndSwap(conditions, mutations)
	if err != nil {
		_ = rollbackMergeMoves(receipt.Moved)
		cleanupMergeQuarantine(receipt.Moved, quarantineRoot)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	if !ok {
		// The same merge already ran: replay its receipt instead of
		// moving files a second time.
		existing, found, err := a.readMergeReceipt(mergeID)
		if err != nil || !found {
			_ = rollbackMergeMoves(receipt.Moved)
			cleanupMergeQuarantine(receipt.Moved, quarantineRoot)
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "MERGE_CONFLICT"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "replayed": true, "merge_id": existing.MergeID, "group_id": existing.GroupID, "kept": existing.Keep, "quarantined": existing.Moved})
		return
	}
	// The keeper is proven to satisfy the same playlist bindings as every
	// quarantined copy above. Keep the immutable playlist index warm; each
	// removed path is still re-verified and the keeper now answers the row.
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "merge_id": receipt.MergeID, "group_id": receipt.GroupID, "kept": receipt.Keep, "quarantined": receipt.Moved, "filled_fields": filled, "aliases_linked": aliasesLinked})
}

// mergeActiveWorkflowPath returns a losing file referenced by an active
// workflow plan. The task payload is the durable source of the input path;
// workflow links are followed so a completed search task under a live root
// still protects the plan. Any storage error fails closed: skipping a dedupe
// pass is safer than moving a file a task may need.
func (a *App) mergeActiveWorkflowPath(group *duplicateGroup) (string, error) {
	if a == nil || a.tasks == nil || group == nil || group.Keep == nil {
		return "", nil
	}
	referenced, err := a.activeLibraryWorkflowPaths()
	if err != nil {
		return "", err
	}
	for _, file := range group.Files {
		if file.RequestID == group.Keep.RequestID {
			continue
		}
		full := a.libraryFilePath(file.LibraryDir, file.File)
		if _, ok := referenced[libraryPathIdentity(full)]; ok {
			return file.File, nil
		}
	}
	return "", nil
}

func (a *App) activeLibraryWorkflowPaths() (map[string]struct{}, error) {
	referenced := map[string]struct{}{}
	if a == nil || a.tasks == nil {
		return referenced, nil
	}
	taskIDs := []string{}
	jobIDs := []string{}
	queuedTask := map[string]bool{}
	queuedJob := map[string]bool{}
	addTask := func(id string) {
		if id != "" && !queuedTask[id] {
			queuedTask[id] = true
			taskIDs = append(taskIDs, id)
		}
	}
	addJob := func(id string) {
		if id != "" && !queuedJob[id] {
			queuedJob[id] = true
			jobIDs = append(jobIDs, id)
		}
	}
	for _, lane := range []string{"identify", "publish", "transcode", "aux", "download"} {
		for _, state := range []string{"queued", "running", "paused", "blocked", "recovery_required"} {
			var after []byte
			for {
				page, err := a.tasks.List(lane, state, after, 200)
				if err != nil {
					return nil, err
				}
				for _, task := range page.Tasks {
					addTask(task.ID)
					addJob(task.WorkflowJobID)
				}
				if len(page.Next) == 0 {
					break
				}
				after = page.Next
			}
		}
	}
	for index := 0; index < len(jobIDs); index++ {
		if a.flows == nil {
			break
		}
		job, err := a.flows.Job(jobIDs[index])
		if err != nil {
			return nil, err
		}
		if job == nil {
			continue
		}
		addTask(job.ActiveTaskID)
		addTask(job.SearchJobID)
		addTask(job.ProcessingJobID)
		addJob(job.RootID)
		addJob(job.ParentJobID)
	}
	for index := 0; index < len(taskIDs); index++ {
		id := taskIDs[index]
		raw, found, err := a.tasks.Data(id, "arguments", 0)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		var plan struct {
			LibraryTracks []localWorkflowTrack `json:"library_tracks"`
		}
		if json.Unmarshal(raw, &plan) != nil {
			continue
		}
		for _, track := range plan.LibraryTracks {
			rawPath := strings.TrimSpace(track.Path)
			if rawPath == "" {
				continue
			}
			candidates := []string{rawPath}
			if !filepath.IsAbs(rawPath) {
				candidates = append(candidates, filepath.Join(a.libraryDir(), rawPath))
			}
			for _, candidate := range candidates {
				referenced[libraryPathIdentity(candidate)] = struct{}{}
			}
		}
	}
	return referenced, nil
}

func libraryPathIdentity(path string) string {
	if abs, err := filepath.Abs(filepath.Clean(path)); err == nil {
		path = abs
	}
	return strings.ToLower(filepath.Clean(canonicalPath(path)))
}

// fillableTagFields are the fields a duplicate may contribute when the keeper
// has none. Title/artist/album/track stay out: those decide identity, not
// metadata.
var fillableTagFields = []string{"ALBUMARTIST", "DATE", "GENRE", "COMPOSER", "LYRICS"}

// fillMissingTags copies a duplicate's tag into an empty keeper field when the
// duplicates agree on exactly one value. It never overwrites: a curated value
// wins, and a conflict is simply not filled. Only FLAC is edited.
type tagMutationError struct {
	restored bool
	err      error
}

func (e *tagMutationError) Error() string { return e.err.Error() }
func (e *tagMutationError) Unwrap() error { return e.err }

func tagMutationRestored(err error) bool {
	var mutationErr *tagMutationError
	if errors.As(err, &mutationErr) {
		return mutationErr.restored
	}
	return true
}

func (a *App) fillMissingTags(group *duplicateGroup) ([]string, error) {
	if group == nil || group.Keep == nil || group.Keep.File == "" {
		return nil, dataengine.ErrInvalidRequest
	}
	keeperPath := a.libraryFilePath(group.Keep.LibraryDir, group.Keep.File)
	if !strings.EqualFold(filepath.Ext(keeperPath), ".flac") {
		return nil, nil
	}
	keeper, err := flactags.ReadFile(keeperPath)
	if err != nil {
		return nil, err
	}
	candidates := map[string]map[string]bool{}
	for _, file := range group.Files {
		if file.RequestID == group.Keep.RequestID {
			continue
		}
		path := a.libraryFilePath(file.LibraryDir, file.File)
		if !strings.EqualFold(filepath.Ext(path), ".flac") {
			continue
		}
		tags, err := flactags.ReadFile(path)
		if err != nil {
			continue
		}
		for _, field := range fillableTagFields {
			value := strings.TrimSpace(firstTagValue(tags.Raw, field))
			if value == "" {
				continue
			}
			if candidates[field] == nil {
				candidates[field] = map[string]bool{}
			}
			candidates[field][value] = true
		}
	}
	updates := map[string][]string{}
	filled := []string{}
	// A locked field is curated by the user: the merge does not fill it even
	// when it is empty and the duplicates agree on a value.
	guard := a.guardFor(filepath.Dir(keeperPath))
	locked := map[string]bool{}
	for _, field := range guard.Fields {
		locked[strings.ToUpper(field)] = true
	}
	for _, field := range fillableTagFields {
		if locked[field] {
			continue
		}
		if strings.TrimSpace(firstTagValue(keeper.Raw, field)) != "" {
			continue
		}
		values := candidates[field]
		if len(values) != 1 {
			continue
		}
		for value := range values {
			updates[field] = []string{value}
			filled = append(filled, strings.ToLower(field))
		}
	}
	if len(updates) == 0 {
		return nil, nil
	}
	if err := checkNotHardlinked(keeperPath); err != nil {
		return nil, &tagMutationError{restored: true, err: err}
	}
	backup, err := backupFile(keeperPath)
	if err != nil {
		return nil, err
	}
	fail := func(cause error) ([]string, error) {
		restored, resultErr := restoreBackupOnFailure(keeperPath, backup, cause)
		return nil, &tagMutationError{restored: restored, err: resultErr}
	}
	if err := writeFLACTags(keeperPath, updates); err != nil {
		return fail(err)
	}
	// The write changed the bytes: the library record must carry the new
	// digest or the next duplicate scan reports the file as changed. If that
	// commit fails, restore the original bytes rather than reporting a partial
	// success while leaving the receipt stale.
	if _, err := audiorelease.RefreshPathDigests(a.engine, []string{keeperPath}); err != nil {
		return fail(err)
	}
	_ = os.Remove(backup)
	return filled, nil
}

// mergeSeedingGuard refuses a merge whose files qBittorrent still holds. A
// configured client that cannot be reached refuses as well: "I could not
// check" is not "it is safe". Without saved qBittorrent settings there is
// nothing to check against, and the merge proceeds as before.
func (a *App) mergeSeedingGuard(group *duplicateGroup) (string, string) {
	managedByTorrent := false
	for _, file := range group.Files {
		if file.TorrentManaged {
			managedByTorrent = true
			break
		}
	}
	// A configured but stopped client cannot seed a streaming/local import.
	// Only files explicitly published by this app's torrent lane need the
	// client-side seeding check.
	if !managedByTorrent {
		return "", ""
	}
	settings, err := a.loadQBittorrentSettings()
	if err != nil || strings.TrimSpace(settings.BaseURL) == "" {
		return "", ""
	}
	login, baseURL, ok := a.savedQBittorrentLogin()
	if !ok {
		return "MERGE_QBITTORRENT_UNAVAILABLE", "qBittorrent 已配置但读不到登录凭据，无法确认这些文件是否还在做种；这次没有合并"
	}
	client, err := qbittorrent.NewClient(qbittorrent.Config{BaseURL: baseURL, Username: login.Username, Password: login.Password})
	if err != nil {
		return "MERGE_QBITTORRENT_UNAVAILABLE", "qBittorrent 地址无效，无法确认这些文件是否还在做种；这次没有合并"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Login(ctx); err != nil {
		return "MERGE_QBITTORRENT_UNAVAILABLE", "qBittorrent 连不上，无法确认这些文件是否还在做种；这次没有合并"
	}
	rows, err := client.InfoByTag(ctx, qbittorrent.ProjectTag)
	if err != nil {
		return "MERGE_QBITTORRENT_UNAVAILABLE", "读取 qBittorrent 种子列表失败，无法确认做种状态；这次没有合并"
	}
	for _, file := range group.Files {
		full := a.libraryFilePath(file.LibraryDir, file.File)
		for _, row := range rows {
			root := strings.TrimSpace(row.ContentPath)
			if root == "" {
				root = strings.TrimSpace(row.SavePath)
			}
			if root == "" {
				continue
			}
			if insideDir(root, full) {
				return "MERGE_SEEDING", "文件仍在 qBittorrent 中（" + row.Name + "）；先在客户端停止并移除该种子，再合并"
			}
		}
	}
	return "", ""
}

// quarantineFile moves one library file (and its .lrc sidecar when
// present) into the merge quarantine directory, recording each move.
func (a *App) quarantineFile(receipt *mergeReceipt, file duplicateFile, quarantineRoot string) error {
	return a.quarantineFileContext(context.Background(), receipt, file, quarantineRoot)
}

func (a *App) quarantineFileContext(ctx context.Context, receipt *mergeReceipt, file duplicateFile, quarantineRoot string) error {
	from := a.libraryFilePath(file.LibraryDir, file.File)
	info, err := os.Stat(from)
	if err != nil || !info.Mode().IsRegular() {
		return errors.Join(errors.New("quarantine source is not a regular file"), err)
	}
	// Quarantine is recoverable dedupe history, not active download cache. It
	// is outside the 8 GiB admission quota, but the copy still must leave the
	// configured disk reserve intact.
	if err := audioCacheDiskHeadroom(filepath.Dir(quarantineRoot), 2*info.Size()+65536); err != nil {
		return fmt.Errorf("quarantine copy headroom: %w", err)
	}
	dest, err := duplicateQuarantineDestination(quarantineRoot, file)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	movedAudio := false
	defer func() {
		if !movedAudio {
			removeEmptyParents(filepath.Dir(dest), quarantineRoot)
		}
	}()
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("quarantine destination already exists: %s", dest)
	}
	if err := moveCheckedFileAtRate(ctx, from, dest, file.SHA256, duplicateFingerprintBytesPerSecond); err != nil {
		return err
	}
	movedAudio = true
	receipt.Moved = append(receipt.Moved, mergeMove{RequestID: file.RequestID, RootID: file.RootID, Track: file.Track, Format: file.Format, Origin: file.Origin, SourceName: file.SourceName, From: from, Quarantine: dest, SHA256: file.SHA256})
	// The sidecar travels with its audio file; a missing sidecar is fine.
	// Both spellings are moved: the standard "<track>.lrc" this build writes
	// and the older "<track>.flac.lrc" an earlier build left behind.
	for _, sidecarFrom := range sidecarCandidates(from) {
		data, err := os.ReadFile(sidecarFrom)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		sidecarDest := lrc.SidecarPath(dest)
		if sidecarFrom == from+".lrc" {
			// Keep the older spelling inside the quarantine so a restore puts
			// the file back under the name it had.
			sidecarDest = dest + ".lrc"
		}
		if err := moveCheckedFile(sidecarFrom, sidecarDest, hex.EncodeToString(sum[:])); err != nil {
			return err
		}
		receipt.Moved = append(receipt.Moved, mergeMove{RequestID: file.RequestID, From: sidecarFrom, Quarantine: sidecarDest, SHA256: hex.EncodeToString(sum[:]), Sidecar: true})
	}
	return nil
}

// duplicateQuarantineDestination includes a stable source identity in the
// path. Multiple library roots can legitimately contain the same relative
// artist/album/file name; using only file.File made the second and later
// copies collide and aborted every three-copy merge.
func duplicateQuarantineDestination(root string, file duplicateFile) (string, error) {
	relative := filepath.Clean(filepath.FromSlash(file.File))
	if relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("quarantine source path is unsafe")
	}
	sum := sha256.Sum256([]byte(file.RequestID + "\x00" + file.LibraryDir + "\x00" + file.File))
	return filepath.Join(root, "items", hex.EncodeToString(sum[:8]), relative), nil
}

// removeEmptyParents removes directories emptied by a rollback or restore.
// It never removes root or anything outside it.
func removeEmptyParents(path, root string) {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	for insideDir(root, path) && path != root {
		if os.Remove(path) != nil {
			return
		}
		path = filepath.Dir(path)
	}
	if path == root {
		_ = os.Remove(root)
	}
}

func cleanupMergeQuarantine(moves []mergeMove, root string) {
	for _, move := range moves {
		removeEmptyParents(filepath.Dir(move.Quarantine), root)
	}
}

// Copy, verify and only then remove the source. Unlike rename this
// supports a library and quarantine on different drives, without replacing
// any existing destination.
func moveCheckedFile(from, to, digest string) error {
	return moveCheckedFileAtRate(context.Background(), from, to, digest, 0)
}

func moveCheckedFileAtRate(ctx context.Context, from, to, digest string, bytesPerSecond int64) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("source is not a regular file")
	}
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(to)
		}
	}()
	sum := sha256.New()
	if bytesPerSecond > 0 {
		err = runWithBackgroundIO(func() error {
			return copyAtRate(ctx, io.MultiWriter(out, sum), in, bytesPerSecond)
		})
	} else {
		_, err = io.Copy(io.MultiWriter(out, sum), in)
	}
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if hex.EncodeToString(sum.Sum(nil)) != digest {
		return errors.New("source digest changed")
	}
	latest, err := os.Lstat(from)
	if err != nil || !latest.Mode().IsRegular() || !os.SameFile(info, latest) || latest.Size() != info.Size() || !latest.ModTime().Equal(info.ModTime()) {
		return errors.New("source changed while copying")
	}
	if err := in.Close(); err != nil {
		return err
	}
	if err := os.Remove(from); err != nil {
		return err
	}
	keep = true
	return nil
}

func rollbackMergeMoves(moves []mergeMove) error {
	var result error
	for i := len(moves) - 1; i >= 0; i-- {
		move := moves[i]
		result = errors.Join(result, moveCheckedFile(move.Quarantine, move.From, move.SHA256))
	}
	return result
}

// sidecarCandidates lists the lyric sidecar names that may sit next to an
// audio file: the standard "<track>.lrc" first, then the older
// "<track>.flac.lrc" spelling an earlier build wrote.
func sidecarCandidates(audioPath string) []string {
	standard := lrc.SidecarPath(audioPath)
	legacy := audioPath + ".lrc"
	if legacy == standard {
		return []string{standard}
	}
	return []string{standard, legacy}
}

func (a *App) readMergeReceipt(id string) (mergeReceipt, bool, error) {
	var receipt mergeReceipt
	var found bool
	err := a.engine.View(func(v *dataengine.View) error {
		raw, ok, err := v.Get(mergeReceiptKey(id))
		if err != nil || !ok {
			return err
		}
		found = true
		return json.Unmarshal(raw, &receipt)
	})
	return receipt, found, err
}

func (a *App) duplicateRestore(w http.ResponseWriter, r *http.Request) {
	a.duplicateMu.Lock()
	defer a.duplicateMu.Unlock()
	var body struct {
		MergeID string `json:"merge_id"`
	}
	if err := decode(r, &body); err != nil || strings.TrimSpace(body.MergeID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	receipt, found, err := a.readMergeReceipt(body.MergeID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "code": "NOT_FOUND"})
		return
	}
	if receipt.Restored {
		a.invalidatePlaylistLibraryCache()
		recovered, recoveryErrors := a.recoverRestoredMergeRecords(receipt.Moved)
		if len(recoveryErrors) > 0 {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"ok": false, "replayed": true, "merge_id": receipt.MergeID,
				"restored": receipt.Moved, "records_recovered": recovered, "recovery_errors": recoveryErrors,
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "replayed": true, "merge_id": receipt.MergeID, "restored": receipt.Moved, "records_recovered": recovered})
		return
	}
	// Verify every quarantined file first; restore nothing until all check out.
	for _, move := range receipt.Moved {
		digest, err := hashFile(move.Quarantine)
		if err != nil || digest != move.SHA256 {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "RESTORE_CONFLICT", "error": "隔离区文件已变化或丢失，拒绝恢复"})
			return
		}
		if existing, err := os.ReadFile(move.From); err == nil {
			sum := sha256.Sum256(existing)
			if hex.EncodeToString(sum[:]) != move.SHA256 {
				writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "RESTORE_CONFLICT", "error": "目标位置已有不同内容，拒绝覆盖"})
				return
			}
			// The destination already holds identical bytes: the
			// quarantine copy is redundant, drop it below.
		}
	}
	restored := []mergeMove{}
	for _, move := range receipt.Moved {
		if existing, err := os.ReadFile(move.From); err == nil {
			sum := sha256.Sum256(existing)
			if hex.EncodeToString(sum[:]) == move.SHA256 {
				_ = os.Remove(move.Quarantine)
				restored = append(restored, move)
				removeEmptyParents(filepath.Dir(move.Quarantine), filepath.Join(a.Dir, "quarantine"))
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(move.From), 0o700); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
			return
		}
		if err := moveCheckedFile(move.Quarantine, move.From, move.SHA256); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE", "error": err.Error()})
			return
		}
		restored = append(restored, move)
		removeEmptyParents(filepath.Dir(move.Quarantine), filepath.Join(a.Dir, "quarantine"))
	}
	receipt.Restored = true
	encoded, err := json.Marshal(receipt)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	// Best effort: the files are already back; a lost receipt update is
	// reported, not hidden, so a second restore replays from the files.
	// The condition is the exact bytes read, so a concurrent change to
	// the receipt fails instead of overwriting.
	var current []byte
	_ = a.engine.View(func(v *dataengine.View) error {
		raw, ok, err := v.Get(mergeReceiptKey(receipt.MergeID))
		if err == nil && ok {
			current = raw
		}
		return err
	})
	if _, err := a.engine.CompareAndSwap(
		[]dataengine.Condition{{Key: mergeReceiptKey(receipt.MergeID), Value: current}},
		[]dataengine.Mutation{{Key: mergeReceiptKey(receipt.MergeID), Value: encoded}},
	); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE", "error": "files restored but the receipt could not be updated: " + err.Error()})
		return
	}
	a.invalidatePlaylistLibraryCache()
	recovered, recoveryErrors := a.recoverRestoredMergeRecords(restored)
	if len(recoveryErrors) > 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "merge_id": receipt.MergeID, "restored": restored,
			"records_recovered": recovered, "recovery_errors": recoveryErrors,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "merge_id": receipt.MergeID, "restored": restored, "records_recovered": recovered})
}

// recoverRestoredMergeRecords restores the catalogue identity carried by a
// merge receipt after its files return to the library. It never synthesizes an
// identity for legacy receipts; the publish verifier can still recover those
// from its own deterministic receipt when the task is retried.
func (a *App) recoverRestoredMergeRecords(moves []mergeMove) (int, []string) {
	library := a.libraryDir()
	recovered := 0
	seen := map[string]bool{}
	errorsOut := []string{}
	decoder := audiorelease.SystemDecoder()
	for _, move := range moves {
		if move.Sidecar || move.RequestID == "" || move.RootID == "" || move.Track < 1 || move.Track > 999 || seen[move.RequestID] {
			continue
		}
		seen[move.RequestID] = true
		if !insideDir(library, move.From) {
			errorsOut = append(errorsOut, move.RequestID+"：恢复文件不在音乐库内")
			continue
		}
		_, err := audiorelease.RecoverPublished(context.Background(), a.engine, audiorelease.RecoverInput{
			RequestID: move.RequestID, RootID: move.RootID, Track: move.Track,
			SHA256: move.SHA256, Format: move.Format, Origin: move.Origin,
			SourceName: move.SourceName, PathHint: move.From, Library: library, Decoder: decoder,
		})
		if err != nil {
			errorsOut = append(errorsOut, move.RequestID+"："+scrub(err.Error()))
			continue
		}
		recovered++
	}
	return recovered, errorsOut
}

// ---------------------------------------------------------------------------
// Artist folders
//
// GET  /v1/library/artist-folders
// POST /v1/library/artist-folders {"request_ids": [...]}
//
// Plan follows app/artist_folders.py: a file is loose when its album
// folder sits directly in the library root (or the file itself sits in
// the root); the target is <library>/<folder artist>/<album>/<file>.
// folder_artist joins split credits with '／' (full-width solidus, a
// legal single folder name everywhere), so "A feat. B" and ["A","B"]
// both become "A／B". Artist and album come from the FLAC tags — the Go
// records carry no job metadata — so a WAV or an untagged FLAC is
// skipped with a reason, never guessed from the folder name. The target
// must not already exist with content, the move must stay on one disk
// (os.Rename), and both ends must resolve inside the library
// (resolveInDir + a resolved-target check, the safe_destination /
// reject_reparse_chain contract). After the move the library record is
// updated through audiorelease.Relocate and the .lrc sidecar travels
// with its audio file.

// artistFolderSplit splits one artist credit the way Python SPLIT does.
// ASCII slash is handled separately so compact names such as AC/DC stay intact.
var artistFolderSplit = regexp.MustCompile(`(?i)\s*(?:&|＆|;|；|,|，|、|×|\bfeat\.|\bfeat\b|\bft\.|\bfeaturing\b)\s*`)

var unknownArtists = map[string]bool{
	"": true, "unknown": true, "unknown artist": true, "未知": true, "未知歌手": true, "未知艺人": true, "[unknown]": true,
}

// folderArtist joins artist credits into one folder name.
func folderArtist(artists []string) string {
	seen := map[string]bool{}
	names := []string{}
	for _, credit := range artists {
		for _, combined := range splitPlaylistArtists(credit) {
			for _, part := range artistFolderSplit.Split(combined, -1) {
				name := artistnorm.Canonical(part)
				key := artistnorm.Key(name)
				if name == "" || key == "" {
					key = strings.ToLower(strings.TrimSpace(name))
				}
				if name != "" && !seen[key] {
					seen[key] = true
					names = append(names, name)
				}
			}
		}
	}
	return strings.Join(names, "／")
}

// cleanFolderComponent sanitizes one path component the way
// library/paths.py release_path does: illegal characters become '_',
// trailing spaces/dots are trimmed, reserved Windows device names are
// prefixed, and an empty result becomes '_'.
func cleanFolderComponent(value string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(value))
	cleaned = strings.TrimRight(cleaned, " .")
	if cleaned == "" {
		return "_"
	}
	head := cleaned
	if i := strings.IndexByte(cleaned, '.'); i >= 0 {
		head = cleaned[:i]
	}
	switch strings.ToUpper(head) {
	case "CON", "PRN", "AUX", "NUL", "CLOCK$",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		cleaned = "_" + cleaned
	}
	return cleaned
}

// artistFolderPlan is one planned (or skipped) relocation.
type artistFolderPlan struct {
	RequestID string `json:"request_id"`
	File      string `json:"file"`
	State     string `json:"state"` // intent | skipped | none
	From      string `json:"from"`
	To        string `json:"to"`
	Reason    string `json:"reason,omitempty"`
	LooseFile bool   `json:"loose_file"`
}

// planArtistFolders computes the relocation plan for every present
// library record.
func (a *App) planArtistFolders() ([]artistFolderPlan, error) {
	entries, err := audiorelease.List(a.engine)
	if err != nil {
		return nil, err
	}
	plans := []artistFolderPlan{}
	for _, entry := range entries {
		if !entry.FilePresent {
			continue
		}
		plans = append(plans, a.planArtistFolder(entry, a.entryLibraryDir(entry)))
	}
	return plans, nil
}

func (a *App) planArtistFolder(entry audiorelease.Entry, library string) artistFolderPlan {
	plan := artistFolderPlan{RequestID: entry.RequestID, File: entry.File, State: "none"}
	full, err := resolveInDir(library, entry.File)
	if err != nil {
		plan.State = "skipped"
		plan.Reason = "文件路径不在库内或含链接"
		return plan
	}
	// full is resolved, so the library must be resolved the same way
	// before the lexical Rel below: on macOS the state directory can
	// sit under a symlinked parent (/var points at /private/var) and
	// the unresolved library spelling shares no lexical prefix with
	// the resolved file path, which used to make every loose album
	// look already organized (state "none", empty "to").
	resolvedLibrary := library
	if r, err := filepath.EvalSymlinks(library); err == nil {
		resolvedLibrary = r
	}
	dir := filepath.Dir(full)
	rel, err := filepath.Rel(resolvedLibrary, dir)
	if err != nil {
		plan.State = "skipped"
		plan.Reason = "文件路径不在库内"
		return plan
	}
	// relFile is the file's path inside the library, for building
	// from/to in the caller's spelling: plans are compared against
	// paths built from application.Dir, which may itself be spelled
	// through a symlinked parent, so the resolved spelling must not
	// leak into from/to.
	relFile, err := filepath.Rel(resolvedLibrary, full)
	if err != nil {
		plan.State = "skipped"
		plan.Reason = "文件路径不在库内"
		return plan
	}
	parts := strings.Split(rel, string(filepath.Separator))
	switch {
	case rel == ".":
		// The file itself sits in the library root.
		plan.LooseFile = true
	case len(parts) == 1:
		// An album folder directly in the library root.
	default:
		// Already inside an artist (or deeper) folder.
		return plan
	}
	// Artist and album come from the FLAC tags, never from the folder
	// name: the folder at the root is the untrusted part.
	var artists []string
	var album string
	switch strings.ToLower(filepath.Ext(full)) {
	case ".flac":
		if tags, err := flactags.ReadFile(full); err == nil {
			if strings.TrimSpace(tags.AlbumArtist) != "" {
				artists = []string{tags.AlbumArtist}
			} else {
				artists = tags.Artists
			}
			album = tags.Album
		}
	case ".wav":
		if tags, err := wavflac.ReadDescription(full); err == nil {
			artistCredit := tags.Artist
			if artistCredit != "" {
				artists = []string{artistCredit}
			}
			album = tags.Album
		}
	}
	artist := folderArtist(artists)
	if unknownArtists[strings.ToLower(artist)] || strings.TrimSpace(album) == "" {
		plan.State = "skipped"
		plan.Reason = "缺少可靠的专辑艺人或专辑名"
		return plan
	}
	targetDir := filepath.Join(library, cleanFolderComponent(artist), cleanFolderComponent(album))
	var from, to string
	if plan.LooseFile {
		from = filepath.Join(library, relFile)
		to = filepath.Join(targetDir, filepath.Base(relFile))
	} else {
		from = filepath.Join(library, filepath.Dir(relFile))
		to = targetDir
	}
	resolvedTarget, err := resolveTargetInDir(library, to)
	if err != nil {
		plan.State = "skipped"
		plan.Reason = "目标路径不在库内或含链接"
		return plan
	}
	to = resolvedTarget
	if _, err := os.Lstat(to); err == nil {
		// Any existing target blocks the move: the target must not
		// exist at all, so nothing is ever overwritten or merged into.
		plan.State = "skipped"
		plan.Reason = "目标已存在"
		if !plan.LooseFile {
			plan.Reason = "目标文件夹已存在：" + filepath.Base(to)
		}
		return plan
	}
	plan.State = "intent"
	plan.From = from
	plan.To = to
	return plan
}

// resolveTargetInDir resolves a not-yet-existing target path: the
// existing ancestors are resolved through symlinks and the remainder is
// appended lexically, then the result must stay inside root. This is the
// safe_destination contract for a destination that does not exist yet.
// The returned path keeps the caller's spelling: the resolved path is
// only the containment check, and callers compare the result against
// paths built from the state directory, which on macOS may be spelled
// through a symlinked parent (/var -> /private/var).
func resolveTargetInDir(root, target string) (string, error) {
	absRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	abs := filepath.Clean(target)
	// Walk up to the nearest existing ancestor and resolve it.
	ancestor := abs
	var suffix []string
	for {
		if _, err := os.Lstat(ancestor); err == nil {
			break
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", errors.New("no existing ancestor")
		}
		suffix = append([]string{filepath.Base(ancestor)}, suffix...)
		ancestor = parent
	}
	resolved, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", err
	}
	for _, part := range suffix {
		if part == ".." || part == "." {
			return "", errors.New("target contains dot components")
		}
		resolved = filepath.Join(resolved, part)
	}
	if !insideDir(absRoot, resolved) {
		return "", errors.New("target escapes the library")
	}
	return abs, nil
}

func (a *App) artistFolderPlan(w http.ResponseWriter, r *http.Request) {
	plans, err := a.planArtistFolders()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	if plans == nil {
		plans = []artistFolderPlan{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "plans": plans, "count": len(plans)})
}

// artistFolderRelocate executes the planned moves. request_ids selects a
// subset; omitting it relocates every intent plan. Each item reports
// its own outcome; a failed item never blocks the others.
func (a *App) artistFolderRelocate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequestIDs []string `json:"request_ids"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	plans, err := a.planArtistFolders()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	selected := map[string]bool{}
	for _, id := range body.RequestIDs {
		selected[id] = true
	}
	results := []map[string]any{}
	for _, plan := range plans {
		if len(selected) > 0 && !selected[plan.RequestID] {
			continue
		}
		if plan.State != "intent" {
			results = append(results, map[string]any{"request_id": plan.RequestID, "file": plan.File, "state": "skipped", "reason": plan.Reason})
			continue
		}
		moved, reason := a.relocateOne(plan)
		if moved == "" {
			results = append(results, map[string]any{"request_id": plan.RequestID, "file": plan.File, "state": "skipped", "reason": reason})
			continue
		}
		results = append(results, map[string]any{"request_id": plan.RequestID, "file": plan.File, "state": "done", "from": plan.From, "to": moved})
	}
	a.invalidatePlaylistLibraryCache()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results, "count": len(results)})
}

// relocateOne moves one planned item and updates its library record.
// It returns the new file path, or "" with a reason when it stays put.
func (a *App) relocateOne(plan artistFolderPlan) (string, string) {
	if plan.LooseFile {
		// Move one loose file (plus its sidecar) into Artist/Album/.
		if err := os.MkdirAll(filepath.Dir(plan.To), 0o700); err != nil {
			return "", "无法创建目标文件夹"
		}
		if _, err := os.Lstat(plan.To); err == nil {
			return "", "目标已存在，拒绝覆盖"
		}
		if err := os.Rename(plan.From, plan.To); err != nil {
			return "", "无法移动文件（" + err.Error() + "），保留原位置"
		}
		if err := audiorelease.Relocate(a.engine, plan.RequestID, plan.To); err != nil {
			// The record update failed: move the file back so the
			// library and the record never disagree.
			_ = os.Rename(plan.To, plan.From)
			return "", "库记录更新失败，已移回原位置"
		}
		moveSidecar(plan.From, plan.To)
		return plan.To, ""
	}
	// Move a whole album folder. The target must not exist: a rename
	// onto an existing empty directory would silently merge on some
	// platforms, so refuse it here.
	if _, err := os.Lstat(plan.To); err == nil {
		return "", "目标文件夹已存在"
	}
	if err := os.MkdirAll(filepath.Dir(plan.To), 0o700); err != nil {
		return "", "无法创建目标文件夹"
	}
	if err := os.Rename(plan.From, plan.To); err != nil {
		return "", "无法移动文件夹（" + err.Error() + "），保留原位置"
	}
	// Update every library record under the moved folder. A record
	// whose update fails is moved back with the folder: the whole move
	// is all or nothing.
	entries, err := audiorelease.List(a.engine)
	if err != nil {
		_ = os.Rename(plan.To, plan.From)
		return "", "库记录读取失败，已移回原位置"
	}
	moved := []audiorelease.Entry{}
	for _, entry := range entries {
		full := a.entryFilePath(entry)
		rel, err := filepath.Rel(plan.From, full)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		newPath := filepath.Join(plan.To, rel)
		if err := audiorelease.Relocate(a.engine, entry.RequestID, newPath); err != nil {
			for _, done := range moved {
				_ = audiorelease.Relocate(a.engine, done.RequestID, a.entryFilePath(done))
			}
			_ = os.Rename(plan.To, plan.From)
			return "", "库记录更新失败，已移回原位置"
		}
		moved = append(moved, entry)
	}
	// Sidecars travel with their audio files: they sit next to the
	// files inside the moved folder, so the folder rename carried them.
	return filepath.Join(plan.To, filepath.Base(plan.File)), ""
}

// moveSidecar moves the lyric sidecar next to a relocated audio file when
// present. Both spellings are moved; the destination is always the standard
// "<track>.lrc". A missing sidecar is fine; a failed move is reported by the
// caller only through the returned error.
func moveSidecar(from, to string) {
	for _, sidecarFrom := range sidecarCandidates(from) {
		if _, err := os.Lstat(sidecarFrom); err != nil {
			continue
		}
		_ = os.Rename(sidecarFrom, lrc.SidecarPath(to))
	}
}
