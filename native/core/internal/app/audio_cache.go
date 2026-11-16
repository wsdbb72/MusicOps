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
const audioCacheStaleAfter = 4 * time.Hour
const audioCacheTrimTargetBytes int64 = 256 << 20

var audioCacheMu sync.Mutex

// reserveAudioCache serializes bounded audio writes, including concurrent
// NetEase downloads and the temporary copy created by metadata writes.
func reserveAudioCache(dir string, bytes int64) (func(), error) {
	audioCacheMu.Lock()
	limit := defaultAudioCacheBytes
	if n, err := strconv.ParseInt(os.Getenv("MUSICOPS_AUDIO_CACHE_BYTES"), 10, 64); err == nil && n > 0 {
		limit = n
	}
	used, err := audioCacheUsage(dir)
	if err == nil && bytes > 0 && used+bytes > limit {
		need := used + bytes - limit + audioCacheTrimTargetBytes
		if need > limit {
			need = limit
		}
		_, err = pruneStaleAudioCache(dir, need, audioCacheStaleAfter, time.Now())
		if err == nil {
			used, err = audioCacheUsage(dir)
		}
	}
	if err == nil && bytes <= limit-used {
		available, spaceErr := audioCacheFreeBytes(dir)
		if spaceErr != nil {
			err = spaceErr
		} else if available < uint64(bytes)+(2<<30) {
			err = errors.New("缓存盘可用空间不足，需保留 2 GiB")
		}
	}
	if err != nil || bytes > limit-used {
		audioCacheMu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("音频缓存达到容量上限，暂缓写入")
	}
	return audioCacheMu.Unlock, nil
}

func audioCacheUsage(dir string) (int64, error) {
	var used int64
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
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
		name := entry.Name()
		if !(strings.HasPrefix(name, "netease-") && strings.HasSuffix(name, ".flac") ||
			strings.HasPrefix(name, "scraped-netease-") && strings.HasSuffix(name, ".flac")) {
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

func removeNeteaseStaging(dir, source, staged string) {
	base := filepath.Base(source)
	if !strings.HasPrefix(base, "netease-") || !strings.HasSuffix(base, ".flac") || staged != "scraped-"+base {
		return
	}
	audioCacheMu.Lock()
	defer audioCacheMu.Unlock()
	_ = os.Remove(filepath.Join(dir, base))
	_ = os.Remove(filepath.Join(dir, staged))
}
