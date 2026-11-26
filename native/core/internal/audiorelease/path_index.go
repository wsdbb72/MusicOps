package audiorelease

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

const pathIndexVersion = "v1"

var pathIndexReadyKey = []byte("library/path-index-" + pathIndexVersion + "/ready")

type pathIndexEntry struct {
	key   []byte
	value []byte
}

func pathIndexNamespace(path string) string {
	sum := sha256.Sum256([]byte(pathKey(path)))
	return hex.EncodeToString(sum[:])
}

func pathReleaseIndexPrefix(path string) []byte {
	return []byte("library/path-release-index-" + pathIndexVersion + "/" + pathIndexNamespace(path) + "/")
}

func pathReleaseIndexKey(path, requestID string) []byte {
	return append(pathReleaseIndexPrefix(path), []byte(requestID)...)
}

func pathAlbumIndexPrefix(path string) []byte {
	return []byte("library/path-album-index-" + pathIndexVersion + "/" + pathIndexNamespace(path) + "/")
}

func pathAlbumIndexKey(path, requestID string, albumKey []byte) []byte {
	sum := sha256.Sum256(albumKey)
	suffix := requestID + "/" + hex.EncodeToString(sum[:])
	return append(pathAlbumIndexPrefix(path), []byte(suffix)...)
}

func pathReleaseIndexEntry(path, requestID string) pathIndexEntry {
	return pathIndexEntry{key: pathReleaseIndexKey(path, requestID), value: []byte(pathKey(path))}
}

func pathAlbumIndexEntry(path, requestID string, albumKey []byte) pathIndexEntry {
	return pathIndexEntry{key: pathAlbumIndexKey(path, requestID, albumKey), value: append([]byte(nil), albumKey...)}
}

func appendPathReleaseIndex(conditions *[]dataengine.Condition, mutations *[]dataengine.Mutation, path, requestID string) {
	entry := pathReleaseIndexEntry(path, requestID)
	*conditions = append(*conditions, dataengine.Condition{Key: entry.key, Absent: true})
	*mutations = append(*mutations, dataengine.Mutation{Key: entry.key, Value: entry.value})
}

func appendPathAlbumIndex(conditions *[]dataengine.Condition, mutations *[]dataengine.Mutation, path, requestID string, albumKeys ...[]byte) {
	for _, albumKey := range albumKeys {
		if len(albumKey) == 0 {
			continue
		}
		entry := pathAlbumIndexEntry(path, requestID, albumKey)
		*conditions = append(*conditions, dataengine.Condition{Key: entry.key, Absent: true})
		*mutations = append(*mutations, dataengine.Mutation{Key: entry.key, Value: entry.value})
	}
}
func appendPathReleaseIndexIfMissing(engine *dataengine.Engine, conditions *[]dataengine.Condition, mutations *[]dataengine.Mutation, path, requestID string) error {
	entry := pathReleaseIndexEntry(path, requestID)
	value, found, err := readRaw(engine, entry.key)
	if err != nil {
		return err
	}
	if found {
		if !bytes.Equal(value, entry.value) {
			return ErrConflict
		}
		return nil
	}
	*conditions = append(*conditions, dataengine.Condition{Key: entry.key, Absent: true})
	*mutations = append(*mutations, dataengine.Mutation{Key: entry.key, Value: entry.value})
	return nil
}

func appendPathAlbumIndexIfMissing(engine *dataengine.Engine, conditions *[]dataengine.Condition, mutations *[]dataengine.Mutation, path, requestID string, albumKeys ...[]byte) error {
	for _, albumKey := range albumKeys {
		if len(albumKey) == 0 {
			continue
		}
		entry := pathAlbumIndexEntry(path, requestID, albumKey)
		value, found, err := readRaw(engine, entry.key)
		if err != nil {
			return err
		}
		if found {
			if !bytes.Equal(value, entry.value) {
				return ErrConflict
			}
			continue
		}
		*conditions = append(*conditions, dataengine.Condition{Key: entry.key, Absent: true})
		*mutations = append(*mutations, dataengine.Mutation{Key: entry.key, Value: entry.value})
	}
	return nil
}

func ensurePathIndex(engine *dataengine.Engine) error {
	var ready bool
	if err := engine.View(func(view *dataengine.View) error {
		raw, found, err := view.Get(pathIndexReadyKey)
		ready = err == nil && found && string(raw) == pathIndexVersion
		return err
	}); err != nil || ready {
		if err != nil {
			return fmt.Errorf("read path index marker: %w", err)
		}
		return nil
	}

	entries := map[string]pathIndexEntry{}
	err := engine.View(func(view *dataengine.View) error {
		scan := func(prefix []byte, visit func([]byte, []byte) error) error {
			var after []byte
			for {
				page, err := view.Scan(prefix, after, 200)
				if err != nil {
					return err
				}
				for _, item := range page.Items {
					if err := visit(item.Key, item.Value); err != nil {
						return err
					}
				}
				if len(page.Next) == 0 {
					return nil
				}
				after = page.Next
			}
		}
		if err := scan([]byte("library/audio/"), func(_ []byte, raw []byte) error {
			var rec record
			if json.Unmarshal(raw, &rec) != nil || !validID(rec.RequestID) || strings.TrimSpace(rec.Path) == "" {
				return nil
			}
			entry := pathReleaseIndexEntry(rec.Path, rec.RequestID)
			entries[string(entry.key)] = entry
			return nil
		}); err != nil {
			return err
		}
		return scan([]byte("library/album-audio/"), func(key, raw []byte) error {
			var rec record
			if json.Unmarshal(raw, &rec) != nil || !validID(rec.RequestID) || strings.TrimSpace(rec.Path) == "" {
				return nil
			}
			entry := pathAlbumIndexEntry(rec.Path, rec.RequestID, key)
			entries[string(entry.key)] = entry
			return nil
		})
	})
	if err != nil {
		return fmt.Errorf("scan path index backfill: %w", err)
	}
	ordered := make([]pathIndexEntry, 0, len(entries))
	for _, entry := range entries {
		ordered = append(ordered, entry)
	}
	sort.Slice(ordered, func(i, j int) bool { return string(ordered[i].key) < string(ordered[j].key) })
	if err := installPathIndexEntries(engine, ordered); err != nil {
		return fmt.Errorf("install path index backfill: %w", err)
	}
	ok, err := engine.CompareAndSwap(
		[]dataengine.Condition{{Key: pathIndexReadyKey, Absent: true}},
		[]dataengine.Mutation{{Key: pathIndexReadyKey, Value: []byte(pathIndexVersion)}},
	)
	if err != nil {
		return fmt.Errorf("mark path index ready: %w", err)
	}
	if !ok {
		// A concurrent backfill completed the same deterministic image.
		return nil
	}
	return nil
}

func installPathIndexEntries(engine *dataengine.Engine, entries []pathIndexEntry) error {
	const batchSize = 512
	for start := 0; start < len(entries); start += batchSize {
		end := start + batchSize
		if end > len(entries) {
			end = len(entries)
		}
		if err := installPathIndexBatch(engine, entries[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func installPathIndexBatch(engine *dataengine.Engine, entries []pathIndexEntry) error {
	if len(entries) == 0 {
		return nil
	}
	conditions := make([]dataengine.Condition, 0, len(entries))
	mutations := make([]dataengine.Mutation, 0, len(entries))
	for _, entry := range entries {
		conditions = append(conditions, dataengine.Condition{Key: entry.key, Absent: true})
		mutations = append(mutations, dataengine.Mutation{Key: entry.key, Value: entry.value})
	}
	ok, err := engine.CompareAndSwap(conditions, mutations)
	if err != nil || ok {
		return err
	}
	if len(entries) == 1 {
		// The only conflict is an already present deterministic index row.
		return nil
	}
	middle := len(entries) / 2
	if err := installPathIndexBatch(engine, entries[:middle]); err != nil {
		return err
	}
	return installPathIndexBatch(engine, entries[middle:])
}
