package app

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const defaultAudioCacheBytes int64 = 8 << 30

var audioCacheMu sync.Mutex

// reserveAudioCache serializes bounded audio writes, including concurrent
// NetEase downloads and the temporary copy created by metadata writes.
func reserveAudioCache(dir string, bytes int64) (func(), error) {
	audioCacheMu.Lock()
	limit := defaultAudioCacheBytes
	if n, err := strconv.ParseInt(os.Getenv("MUSICOPS_AUDIO_CACHE_BYTES"), 10, 64); err == nil && n > 0 {
		limit = n
	}
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
		limit -= info.Size()
		return nil
	})
	if err == nil && bytes <= limit {
		available, spaceErr := audioCacheFreeBytes(dir)
		if spaceErr != nil {
			err = spaceErr
		} else if available < uint64(bytes)+(2<<30) {
			err = errors.New("缓存盘可用空间不足，需保留 2 GiB")
		}
	}
	if err != nil || bytes > limit {
		audioCacheMu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("音频缓存达到容量上限，暂缓写入")
	}
	return audioCacheMu.Unlock, nil
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
