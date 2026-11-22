package app

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultAudioCacheBytes int64 = 8 << 30
const audioCacheStaleAfter = time.Hour
const audioCacheTrimTargetBytes int64 = 256 << 20

var audioCacheMu sync.Mutex

type audioCacheCapacityError struct{ reason string }

func (e *audioCacheCapacityError) Error() string { return e.reason }

func isTemporaryAudioCacheError(err error) bool {
	var target *audioCacheCapacityError
	return errors.As(err, &target)
}

// reserveAudioCache serializes bounded audio writes, including concurrent
// NetEase downloads and the temporary copy created by metadata writes.
func reserveAudioCache(dir string, bytes int64) (func(), error) {
	audioCacheMu.Lock()
	limit := defaultAudioCacheBytes
	if n, err := strconv.ParseInt(os.Getenv("MUSICOPS_AUDIO_CACHE_BYTES"), 10, 64); err == nil && n > 0 {
		limit = n
	}
	used, err := audioCacheUsage(dir)
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
			_, err = pruneStaleAudioCache(dir, need, audioCacheStaleAfter, time.Now())
			if err == nil {
				used, err = audioCacheUsage(dir)
			}
		}
	}
	if err == nil && bytes <= limit-used {
		available, spaceErr := audioCacheFreeBytes(dir)
		if spaceErr != nil {
			err = spaceErr
		} else if available < uint64(bytes)+(2<<30) {
			err = &audioCacheCapacityError{reason: "缓存盘可用空间不足，需保留 2 GiB"}
		}
	}
	if err != nil || bytes > limit-used {
		audioCacheMu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, &audioCacheCapacityError{reason: "音频缓存达到容量上限，暂缓写入"}
	}
	return audioCacheMu.Unlock, nil
}

func audioCacheUsage(dir string) (int64, error) {
	var used int64
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() || !stagedAudioCacheFile(dir, path) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		used += info.Size()
		return nil
	})
	return used, err
}

func stagedAudioCacheFile(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	name := strings.ToLower(filepath.Base(rel))
	if strings.HasPrefix(name, ".scraped-") && strings.HasSuffix(name, ".tmp") {
		return true
	}
	for _, suffix := range []string{".flac", ".wav", ".audio"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
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
	if need <= 0 {
		return 0, nil
	}
	type candidate struct {
		path string
		size int64
		mod  time.Time
	}
	var candidates []candidate
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if !prunableAudioCacheFile(dir, path) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(now.Add(-maxAge)) {
			return nil
		}
		candidates = append(candidates, candidate{path: path, size: info.Size(), mod: info.ModTime()})
		return nil
	})
	if err != nil {
		return 0, err
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].mod.Before(candidates[j].mod) })
	var freed int64
	for _, item := range candidates {
		if freed >= need {
			break
		}
		if err := os.Remove(item.path); err != nil && !os.IsNotExist(err) {
			return freed, err
		}
		freed += item.size
	}
	return freed, nil
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
