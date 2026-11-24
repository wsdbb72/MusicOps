package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/qbittorrent"
)

const defaultAudioCacheBytes int64 = 8 << 30
const audioCacheStaleAfter = time.Hour
const audioCacheTrimTargetBytes int64 = 256 << 20
const audioCacheDiskReserveBytes int64 = 2 << 30

var audioCacheMu sync.Mutex

type audioCacheCapacityError struct{ reason string }

func (e *audioCacheCapacityError) Error() string { return e.reason }

func isTemporaryAudioCacheError(err error) bool {
	var target *audioCacheCapacityError
	return errors.As(err, &target)
}

func audioCacheLimitBytes() int64 {
	if n, err := strconv.ParseInt(os.Getenv("MUSICOPS_AUDIO_CACHE_BYTES"), 10, 64); err == nil && n > 0 {
		return n
	}
	return defaultAudioCacheBytes
}

func (a *App) audioCacheUsageSnapshot() (used, limit int64, roots []string, err error) {
	roots = a.audioCacheRoots("")
	audioCacheMu.Lock()
	defer audioCacheMu.Unlock()
	used, err = audioCacheUsageRoots(roots)
	return used, audioCacheLimitBytes(), roots, err
}

// audioCacheRoots returns every managed staging tree that contributes to the
// one global cache limit. Legacy state-directory trees remain counted after a
// user moves the work directory, so an old cache cannot silently coexist with
// a fresh one and consume twice the configured quota.
func (a *App) audioCacheRoots(current string) []string {
	if a == nil {
		return normalizeAudioCacheRoots([]string{current})
	}
	candidates := []string{current}
	work := a.workDir()
	for _, root := range []string{a.Dir, work} {
		for _, name := range []string{"incoming", "downloads", "seeds", "quarantine"} {
			candidates = append(candidates, filepath.Join(root, name))
		}
	}
	return normalizeAudioCacheRoots(candidates)
}

func normalizeAudioCacheRoots(in []string) []string {
	unique := map[string]string{}
	for _, raw := range in {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		absolute, err := filepath.Abs(raw)
		if err != nil {
			continue
		}
		absolute = filepath.Clean(absolute)
		if evaluated, err := filepath.EvalSymlinks(absolute); err == nil {
			absolute = filepath.Clean(evaluated)
		}
		key := absolute
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		unique[key] = absolute
	}
	ordered := make([]string, 0, len(unique))
	for _, root := range unique {
		ordered = append(ordered, root)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if len(ordered[i]) != len(ordered[j]) {
			return len(ordered[i]) < len(ordered[j])
		}
		return ordered[i] < ordered[j]
	})
	roots := make([]string, 0, len(ordered))
	for _, candidate := range ordered {
		nested := false
		for _, root := range roots {
			rel, err := filepath.Rel(root, candidate)
			if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				nested = true
				break
			}
		}
		if !nested {
			roots = append(roots, candidate)
		}
	}
	return roots
}

// reserveAudioCache is the single-root helper used by focused tests. Runtime
// writes use App.reserveAudioCache so every managed root shares one quota.
func reserveAudioCache(dir string, bytes int64) (func(), error) {
	return reserveAudioCacheRoots([]string{dir}, dir, bytes, nil)
}

func (a *App) reserveAudioCache(dir string, bytes int64) (func(), error) {
	references, err := a.activeAudioCacheReferences()
	if err != nil {
		return nil, err
	}
	roots := a.audioCacheRoots(dir)
	return reserveAudioCacheRootsProtected(roots, dir, bytes, func(need int64, now time.Time) (int64, int, error) {
		return a.prunePublishedAudioCacheRootsLocked(roots, audioCacheStaleAfter, now)
	}, func(path string) bool { return cachePathReferencedByTasks(path, references) })
}

// reserveAudioCacheRoots serializes bounded audio writes, including concurrent
// stream downloads and temporary copies created by metadata writes. All roots
// are measured together before any write is admitted.
func reserveAudioCacheRoots(roots []string, writeDir string, bytes int64, prunePublished func(int64, time.Time) (int64, int, error)) (func(), error) {
	return reserveAudioCacheRootsProtected(roots, writeDir, bytes, prunePublished, nil)
}

func reserveAudioCacheRootsProtected(roots []string, writeDir string, bytes int64, prunePublished func(int64, time.Time) (int64, int, error), protect func(string) bool) (func(), error) {
	audioCacheMu.Lock()
	limit := audioCacheLimitBytes()
	roots = normalizeAudioCacheRoots(roots)
	used, err := audioCacheUsageRoots(roots)
	if err == nil && bytes > 0 {
		target := limit * 3 / 4
		need := int64(0)
		if used > target {
			need = used - target + audioCacheTrimTargetBytes
		}
		if used+bytes > limit {
			overLimit := used + bytes - limit + audioCacheTrimTargetBytes
			if overLimit > need {
				need = overLimit
			}
		}
		if need > limit {
			need = limit
		}
		if need > 0 {
			now := time.Now()
			_, _, err = pruneStaleAudioCacheRootsWithCountProtected(roots, need, audioCacheStaleAfter, now, protect)
			if err == nil {
				used, err = audioCacheUsageRoots(roots)
			}
			if err == nil && used+bytes > target && prunePublished != nil {
				_, _, err = prunePublished(used-target+audioCacheTrimTargetBytes, now)
				if err == nil {
					used, err = audioCacheUsageRoots(roots)
				}
			}
		}
	}
	if err == nil && bytes <= limit-used {
		err = audioCacheDiskHeadroom(writeDir, bytes)
	}
	if err != nil || bytes > limit-used {
		audioCacheMu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, &audioCacheCapacityError{reason: fmt.Sprintf("音频缓存总占用 %.2f GiB，达到 %.2f GiB 上限，暂缓写入", float64(used)/(1<<30), float64(limit)/(1<<30))}
	}
	return audioCacheMu.Unlock, nil
}

func audioCacheUsage(dir string) (int64, error) {
	return audioCacheUsageRoots([]string{dir})
}

func audioCacheUsageRoots(roots []string) (int64, error) {
	var used int64
	seen := map[string]struct{}{}
	for _, root := range normalizeAudioCacheRoots(roots) {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if !entry.Type().IsRegular() || !stagedAudioCacheFile(root, path) {
				return nil
			}
			key := path
			if absolute, err := filepath.Abs(path); err == nil {
				key = absolute
			}
			if runtime.GOOS == "windows" {
				key = strings.ToLower(key)
			}
			if _, ok := seen[key]; ok {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			seen[key] = struct{}{}
			used += info.Size()
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return 0, err
		}
	}
	return used, nil
}

func audioCacheDiskHeadroom(dir string, bytes int64) error {
	if bytes < 0 {
		bytes = 0
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(absolute)
	check := absolute
	if volume != "" {
		check = volume + string(filepath.Separator)
	}
	available, err := audioCacheFreeBytes(check)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if available < uint64(audioCacheDiskReserveBytes+bytes) {
		if volume == "" {
			volume = check
		}
		return &audioCacheCapacityError{reason: fmt.Sprintf("缓存盘 %s 可用空间不足，写入后需保留 2 GiB", volume)}
	}
	return nil
}

func stagedAudioCacheFile(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	// A cache quota is a disk-space guarantee, not an audio-extension guess.
	// Torrent payloads, sidecars, archives and temporary manifests in these
	// managed roots all consume the same SSD. Deletion remains governed by the
	// much narrower managedAudioCacheFile/prunableAudioCacheFile predicates.
	return true
}

// managedAudioCacheFile recognizes the atoms written by our download,
// stream, transcode, and scraping paths. Only these are candidates for
// automatic cleanup; arbitrary user audio remains protected by the wider
// usage-only check.
func managedAudioCacheFile(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	name := strings.ToLower(filepath.Base(rel))
	parent := strings.ToLower(filepath.Base(filepath.Dir(rel)))
	if parent == "engine-transcode" {
		return strings.HasPrefix(name, "engine-") && (strings.HasSuffix(name, ".audio") || strings.HasSuffix(name, ".flac"))
	}
	if strings.HasPrefix(name, ".scraped-") && strings.HasSuffix(name, ".tmp") {
		return true
	}
	if !(strings.HasSuffix(name, ".flac") || strings.HasSuffix(name, ".wav")) {
		return false
	}
	for _, prefix := range []string{"dl-", "scraped-dl-", "netease-", "scraped-netease-", "stream-", "scraped-stream-"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// prunableAudioCacheFile is deliberately narrower than the usage filter.
// Download/stream copies can still be the recovery source for a reviewed
// task, so age alone is not enough evidence to delete them. NetEase staging
// and our own failed temp/transcode files are reproducible and safely
// reclaimable once they are stale.
func prunableAudioCacheFile(root, path string) bool {
	if !managedAudioCacheFile(root, path) {
		return false
	}
	name := strings.ToLower(filepath.Base(path))
	parent := strings.ToLower(filepath.Base(filepath.Dir(path)))
	if parent == "engine-transcode" {
		return strings.HasPrefix(name, "engine-")
	}
	if strings.HasPrefix(name, ".scraped-") && strings.HasSuffix(name, ".tmp") {
		return true
	}
	return strings.HasPrefix(name, "netease-") || strings.HasPrefix(name, "scraped-netease-")
}

// pruneStaleAudioCache reclaims failed or abandoned NetEase staging files.
// Playlist manifests, recent files, downloads and any other user-owned input
// are never deleted; the caller runs under audioCacheMu so no reservation is
// concurrently writing one of these paths.
func pruneStaleAudioCache(dir string, need int64, maxAge time.Duration, now time.Time) (int64, error) {
	return pruneStaleAudioCacheRoots([]string{dir}, need, maxAge, now)
}

func pruneStaleAudioCacheRoots(roots []string, need int64, maxAge time.Duration, now time.Time) (int64, error) {
	freed, _, err := pruneStaleAudioCacheRootsWithCount(roots, need, maxAge, now)
	return freed, err
}

func pruneStaleAudioCacheRootsWithCount(roots []string, need int64, maxAge time.Duration, now time.Time) (int64, int, error) {
	return pruneStaleAudioCacheRootsWithCountProtected(roots, need, maxAge, now, nil)
}

func pruneStaleAudioCacheRootsWithCountProtected(roots []string, need int64, maxAge time.Duration, now time.Time, protect func(string) bool) (int64, int, error) {
	if need <= 0 {
		return 0, 0, nil
	}
	type candidate struct {
		path string
		size int64
		mod  time.Time
	}
	var candidates []candidate
	seen := map[string]struct{}{}
	for _, root := range normalizeAudioCacheRoots(roots) {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if !entry.Type().IsRegular() || !prunableAudioCacheFile(root, path) {
				return nil
			}
			key := path
			if absolute, err := filepath.Abs(path); err == nil {
				key = absolute
			}
			if runtime.GOOS == "windows" {
				key = strings.ToLower(key)
			}
			if _, ok := seen[key]; ok {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.ModTime().After(now.Add(-maxAge)) || protect != nil && protect(path) {
				return nil
			}
			seen[key] = struct{}{}
			candidates = append(candidates, candidate{path: path, size: info.Size(), mod: info.ModTime()})
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return 0, 0, err
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].mod.Before(candidates[j].mod) })
	var freed int64
	removed := 0
	for _, item := range candidates {
		if freed >= need {
			break
		}
		if err := os.Remove(item.path); err != nil && !os.IsNotExist(err) {
			return freed, removed, err
		}
		freed += item.size
		removed++
	}
	pruneEmptyQuarantineDirs(roots)
	return freed, removed, nil
}

// pruneEmptyQuarantineDirs only removes directories; it never deletes a
// quarantined recording or sidecar. Old failed merges used to leave these
// directories behind even after every file had rolled back successfully.
// pruneEmptyCacheDirs removes directories emptied by a safe cache cleanup. It
// never removes a cache root itself and never removes a non-empty directory.
func pruneEmptyCacheDirs(roots []string) {
	for _, root := range normalizeAudioCacheRoots(roots) {
		var directories []string
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if entry.IsDir() && path != root {
				directories = append(directories, path)
			}
			return nil
		})
		sort.Slice(directories, func(i, j int) bool {
			return len(directories[i]) > len(directories[j])
		})
		for _, path := range directories {
			_ = os.Remove(path)
		}
	}
}
func pruneEmptyQuarantineDirs(roots []string) {
	quarantine := []string{}
	for _, root := range roots {
		if strings.EqualFold(filepath.Base(filepath.Clean(root)), "quarantine") {
			quarantine = append(quarantine, root)
		}
	}
	pruneEmptyCacheDirs(quarantine)
}

// prunePublishedAudioCache removes a managed cache copy only when a different,
// currently present indexed library file has the same SHA-256 bytes. It covers
// stream copies and complete torrent/download staging albums; the method name
// is retained for compatibility with existing callers and tests.
func (a *App) prunePublishedStreamAudioCache(dir string, maxAge time.Duration, now time.Time) (int64, int, error) {
	if a == nil || a.engine == nil || strings.TrimSpace(dir) == "" {
		return 0, 0, nil
	}
	audioCacheMu.Lock()
	defer audioCacheMu.Unlock()
	return a.prunePublishedAudioCacheRootsLocked([]string{dir}, maxAge, now)
}

// prunePublishedStreamAudioCacheAll scans both the current work tree and the
// legacy state tree. Automation uses this entry point so a moved work directory
// cannot leave old copies permanently outside cache reclamation.
func (a *App) prunePublishedStreamAudioCacheAll(maxAge time.Duration, now time.Time) (int64, int, error) {
	if a == nil || a.engine == nil {
		return 0, 0, nil
	}
	audioCacheMu.Lock()
	defer audioCacheMu.Unlock()
	return a.prunePublishedAudioCacheRootsLocked(a.audioCacheRoots(""), maxAge, now)
}

// pruneAudioCacheAll performs both safe passes under one cache lock and returns
// the combined reclaim count. Stale NetEase/transcode staging is reproducible;
// all other managed copies are removed only after their exact bytes are present
// in the indexed library.
func (a *App) pruneAudioCacheAll(maxAge time.Duration, now time.Time) (int64, int, error) {
	if a == nil || a.engine == nil {
		return 0, 0, nil
	}
	audioCacheMu.Lock()
	defer audioCacheMu.Unlock()
	roots := a.audioCacheRoots("")
	used, err := audioCacheUsageRoots(roots)
	if err != nil {
		return 0, 0, err
	}
	limit := audioCacheLimitBytes()
	need := int64(0)
	if target := limit * 3 / 4; used > target {
		need = used - target + audioCacheTrimTargetBytes
		if need > limit {
			need = limit
		}
	}
	activeReferences, err := a.activeAudioCacheReferences()
	if err != nil {
		return 0, 0, err
	}
	staleBytes, staleFiles, err := pruneStaleAudioCacheRootsWithCountProtected(roots, need, maxAge, now, func(path string) bool {
		return cachePathReferencedByTasks(path, activeReferences)
	})
	if err != nil {
		return staleBytes, staleFiles, err
	}
	publishedBytes, publishedFiles, err := a.prunePublishedAudioCacheRootsLocked(roots, maxAge, now)
	if err != nil {
		return staleBytes + publishedBytes, staleFiles + publishedFiles, err
	}
	quarantineBytes, quarantineFiles, err := a.pruneSuccessfulQuarantineLocked(maxAge, now)
	return staleBytes + publishedBytes + quarantineBytes, staleFiles + publishedFiles + quarantineFiles, err
}

func (a *App) prunePublishedAudioCacheRootsLocked(roots []string, maxAge time.Duration, now time.Time) (int64, int, error) {
	if a == nil || a.engine == nil {
		return 0, 0, nil
	}
	activeReferences, err := a.activeAudioCacheReferences()
	if err != nil {
		return 0, 0, err
	}
	seedRoots, err := a.activeQBittorrentSeedRoots()
	if err != nil {
		return 0, 0, err
	}
	entries, err := audiorelease.ListIndexed(a.engine)
	if err != nil {
		return 0, 0, err
	}
	bySHA := map[string][]audiorelease.Entry{}
	for _, entry := range entries {
		if entry.SHA256 != "" && entry.FilePresent {
			bySHA[entry.SHA256] = append(bySHA[entry.SHA256], entry)
		}
	}
	roots = normalizeAudioCacheRoots(roots)
	type candidate struct {
		path string
		root string
		size int64
		mod  time.Time
	}
	var candidates []candidate
	seen := map[string]struct{}{}
	for _, root := range roots {
		if strings.EqualFold(filepath.Base(root), "quarantine") {
			// Non-empty quarantine files remain recoverable until the merge
			// receipt and retention policy proves otherwise.
			continue
		}
		err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if os.IsNotExist(walkErr) {
					return nil
				}
				return walkErr
			}
			if !entry.Type().IsRegular() || !reclaimablePublishedCacheCopy(root, path) {
				return nil
			}
			info, statErr := entry.Info()
			if statErr != nil {
				return statErr
			}
			if !info.ModTime().Before(now.Add(-maxAge)) || cachePathReferencedByTasks(path, activeReferences) || cachePathInsideRoots(path, seedRoots) {
				return nil
			}
			key := path
			if absolute, err := filepath.Abs(path); err == nil {
				key = absolute
			}
			if runtime.GOOS == "windows" {
				key = strings.ToLower(key)
			}
			if _, ok := seen[key]; ok {
				return nil
			}
			seen[key] = struct{}{}
			candidates = append(candidates, candidate{path: path, root: root, size: info.Size(), mod: info.ModTime()})
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return 0, 0, err
		}
	}
	var freed int64
	removed := 0
	for _, item := range candidates {
		digest, hashErr := hashFile(item.path)
		if hashErr != nil {
			continue
		}
		published := false
		for _, entry := range bySHA[digest] {
			if a.libraryAddsDifferentPublishedCopy(entry, item.path, roots) {
				published = true
				break
			}
		}
		if !published {
			continue
		}
		latest, statErr := os.Lstat(item.path)
		if statErr != nil || !latest.Mode().IsRegular() || latest.Size() != item.size || !latest.ModTime().Equal(item.mod) {
			continue
		}
		if removeErr := os.Remove(item.path); removeErr != nil && !os.IsNotExist(removeErr) {
			return freed, removed, removeErr
		}
		freed += item.size
		removed++
	}
	pruneEmptyCacheDirs(roots)
	return freed, removed, nil
}

// reclaimablePublishedCacheCopy is intentionally narrow about extensions but
// not about filenames: torrent releases keep their original album/track names
// instead of the app's stream-/netease- prefixes.
func reclaimablePublishedCacheCopy(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".flac", ".wav", ".wave", ".ape", ".alac", ".m4a", ".mp3", ".mp2", ".aac", ".ogg", ".opus", ".wv", ".aiff", ".aif", ".dsf", ".dff", ".mka", ".audio":
		return true
	default:
		return false
	}
}

func (a *App) libraryAddsDifferentPublishedCopy(entry audiorelease.Entry, cachePath string, roots []string) bool {
	if !entry.PresenceUnchanged() {
		return false
	}
	published, found, err := audiorelease.PathFor(a.engine, entry.RequestID)
	if err != nil || !found || sameLibraryPath(cachePath, published) {
		return false
	}
	for _, root := range roots {
		if insideDir(root, published) {
			return false
		}
	}
	info, err := os.Lstat(published)
	return err == nil && info.Mode().IsRegular()
}

// activeQBittorrentSeedRoots protects files that the configured qBittorrent
// client is still using. A stopped client cannot be seeding, so a connection
// failure is treated as "no active root"; an authentication or API-contract
// failure is not silently treated as safe and aborts the cache pass.
func (a *App) activeQBittorrentSeedRoots() ([]string, error) {
	if a == nil {
		return nil, nil
	}
	settings, err := a.loadQBittorrentSettings()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(settings.BaseURL) == "" {
		return nil, nil
	}
	login, baseURL, ok := a.savedQBittorrentLogin()
	if !ok {
		return nil, nil
	}
	client, err := qbittorrent.NewClient(qbittorrent.Config{BaseURL: baseURL, Username: login.Username, Password: login.Password})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Login(ctx); err != nil {
		if errors.Is(err, qbittorrent.ErrAuthFailed) || errors.Is(err, qbittorrent.ErrBanned) {
			return nil, err
		}
		// A refused connection means the client is not running and therefore
		// cannot be seeding these local files.
		return nil, nil
	}
	rows, err := client.InfoByTag(ctx, qbittorrent.ProjectTag)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	roots := []string{}
	for _, row := range rows {
		for _, root := range []string{strings.TrimSpace(row.ContentPath), strings.TrimSpace(row.SavePath)} {
			if root == "" {
				continue
			}
			root = canonicalPath(root)
			key := strings.ToLower(filepath.Clean(root))
			if seen[key] {
				continue
			}
			seen[key] = true
			roots = append(roots, root)
		}
	}
	sort.Strings(roots)
	return roots, nil
}

func cachePathInsideRoots(path string, roots []string) bool {
	for _, root := range roots {
		if root != "" && insideDir(root, path) {
			return true
		}
	}
	return false
}

// activeAudioCacheReferences protects a cache file whose path still contains a
// running, queued, paused, blocked or recovery task/job id. The published-byte
// hash proof alone is not enough while a task may still need its staged input.
func (a *App) activeAudioCacheReferences() ([]string, error) {
	if a == nil || a.tasks == nil {
		return nil, nil
	}
	seen := map[string]bool{}
	add := func(value string) {
		for _, candidate := range []string{value, safeStreamID(value)} {
			normalized := normalizeCacheReference(candidate)
			if len(normalized) >= 8 {
				seen[normalized] = true
			}
		}
	}
	jobIDs := map[string]bool{}
	for _, lane := range []string{"identify", "publish", "transcode", "aux", "download"} {
		for _, state := range []string{"queued", "running", "paused", "blocked", "recovery_required"} {
			var after []byte
			for {
				page, err := a.tasks.List(lane, state, after, 200)
				if err != nil {
					return nil, err
				}
				for _, task := range page.Tasks {
					add(task.ID)
					add(task.ParentID)
					add(task.WorkflowJobID)
					if task.WorkflowJobID == "" || jobIDs[task.WorkflowJobID] || a.flows == nil {
						continue
					}
					jobIDs[task.WorkflowJobID] = true
					job, err := a.flows.Job(task.WorkflowJobID)
					if err != nil {
						return nil, err
					}
					if job != nil {
						add(job.ID)
						add(job.RootID)
						add(job.BatchID)
					}
				}
				if len(page.Next) == 0 {
					break
				}
				after = page.Next
			}
		}
	}
	references := make([]string, 0, len(seen))
	for value := range seen {
		references = append(references, value)
	}
	sort.Strings(references)
	return references, nil
}

func cachePathReferencedByTasks(path string, references []string) bool {
	normalized := normalizeCacheReference(path)
	for _, reference := range references {
		if reference != "" && strings.Contains(normalized, reference) {
			return true
		}
	}
	return false
}

func normalizeCacheReference(value string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func removePublishedStreamStaging(dir, source, staged string) {
	base := filepath.Base(source)
	if !(strings.HasPrefix(base, "netease-") || strings.HasPrefix(base, "stream-")) || !strings.HasSuffix(base, ".flac") || staged != "scraped-"+base {
		return
	}
	audioCacheMu.Lock()
	defer audioCacheMu.Unlock()
	_ = os.Remove(filepath.Join(dir, base))
	_ = os.Remove(filepath.Join(dir, staged))
}
