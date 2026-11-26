package audiorelease

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

// RefreshPathDigests records the current bytes of files that this process has
// intentionally edited in place, such as writing tags or embedding artwork.
// Every published record attached to the same physical path is updated in one
// CAS, including duplicate aliases, so a later verification does not mistake a
// legitimate metadata-only edit for a changed audio publication.
func RefreshPathDigests(engine *dataengine.Engine, paths []string) (int, error) {
	if engine == nil {
		return 0, dataengine.ErrInvalidRequest
	}
	targets := make(map[string]pathDigest, len(paths))
	for _, raw := range paths {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		path, err := filepath.Abs(filepath.Clean(raw))
		if err != nil || path != filepath.Clean(path) {
			return 0, dataengine.ErrInvalidRequest
		}
		if _, seen := targets[pathKey(path)]; seen {
			continue
		}
		target, err := digestRegularFile(path)
		if err != nil {
			return 0, err
		}
		targets[pathKey(path)] = target
	}
	if len(targets) == 0 {
		return 0, nil
	}
	if err := ensurePathIndex(engine); err != nil {
		return 0, err
	}

	type plannedUpdate struct {
		key []byte
		raw []byte
		new []byte
	}
	updates := []plannedUpdate{}
	planned := map[string]bool{}
	lateIndexes := map[string]pathIndexEntry{}
	addUpdate := func(key, raw []byte, target pathDigest) error {
		updated, changed, err := refreshedRecord(raw, target)
		if err != nil || !changed || planned[string(key)] {
			return err
		}
		planned[string(key)] = true
		updates = append(updates, plannedUpdate{key: append([]byte(nil), key...), raw: append([]byte(nil), raw...), new: updated})
		return nil
	}
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

		for targetKey, target := range targets {
			releasePrefix := pathReleaseIndexPrefix(target.Path)
			releaseIDs := map[string]bool{}
			if err := scan(releasePrefix, func(key, raw []byte) error {
				if string(raw) != targetKey {
					return nil
				}
				id := strings.TrimPrefix(string(key), string(releasePrefix))
				if validID(id) {
					releaseIDs[id] = true
				}
				return nil
			}); err != nil {
				return err
			}
			for id := range releaseIDs {
				raw, found, err := view.Get(releaseKey(id))
				if err != nil {
					return err
				}
				if found {
					if err := addUpdate(releaseKey(id), raw, target); err != nil {
						return err
					}
				}
			}

			albumPrefix := pathAlbumIndexPrefix(target.Path)
			if err := scan(albumPrefix, func(_ []byte, albumKey []byte) error {
				raw, found, err := view.Get(albumKey)
				if err != nil {
					return err
				}
				if found {
					return addUpdate(albumKey, raw, target)
				}
				return nil
			}); err != nil {
				return err
			}

			if len(releaseIDs) > 0 {
				continue
			}
			// A record created by an older process after the one-time backfill
			// may lack an index row. Repair that path conservatively with a full
			// scan; normal writes keep this fallback out of the hot path.
			if err := scan([]byte("library/audio/"), func(key, raw []byte) error {
				var rec record
				if json.Unmarshal(raw, &rec) != nil || !sameReleasePath(rec.Path, target.Path) {
					return nil
				}
				lateIndexes[string(pathReleaseIndexKey(rec.Path, rec.RequestID))] = pathReleaseIndexEntry(rec.Path, rec.RequestID)
				return addUpdate(key, raw, target)
			}); err != nil {
				return err
			}
			if err := scan([]byte("library/album-audio/"), func(key, raw []byte) error {
				var rec record
				if json.Unmarshal(raw, &rec) != nil || !sameReleasePath(rec.Path, target.Path) {
					return nil
				}
				lateIndexes[string(pathAlbumIndexKey(rec.Path, rec.RequestID, key))] = pathAlbumIndexEntry(rec.Path, rec.RequestID, key)
				return addUpdate(key, raw, target)
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if len(lateIndexes) > 0 {
		ordered := make([]pathIndexEntry, 0, len(lateIndexes))
		for _, entry := range lateIndexes {
			ordered = append(ordered, entry)
		}
		if err := installPathIndexEntries(engine, ordered); err != nil {
			return 0, err
		}
	}
	if len(updates) == 0 {
		return 0, nil
	}

	conditions := make([]dataengine.Condition, 0, len(updates))
	mutations := make([]dataengine.Mutation, 0, len(updates))
	for _, update := range updates {
		conditions = append(conditions, dataengine.Condition{Key: update.key, Value: update.raw})
		mutations = append(mutations, dataengine.Mutation{Key: update.key, Value: update.new})
	}
	ok, err := engine.CompareAndSwap(conditions, mutations)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, ErrConflict
	}
	return len(updates), nil
}

type pathDigest struct {
	Path        string
	SHA256      string
	IndexedStat indexedStat
}

func digestRegularFile(path string) (pathDigest, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return pathDigest{}, err
	}
	if !before.Mode().IsRegular() {
		return pathDigest{}, ErrConflict
	}
	file, err := os.Open(path)
	if err != nil {
		return pathDigest{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return pathDigest{}, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return pathDigest{}, ErrConflict
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return pathDigest{}, err
	}
	after, err := file.Stat()
	if err != nil {
		return pathDigest{}, err
	}
	afterPath, err := os.Lstat(path)
	if err != nil || afterPath.Mode()&os.ModeSymlink != 0 || afterPath.Mode()&os.ModeNamedPipe != 0 || !afterPath.Mode().IsRegular() || !os.SameFile(before, afterPath) {
		return pathDigest{}, ErrConflict
	}
	if size != before.Size() || after.Size() != before.Size() || afterPath.Size() != before.Size() ||
		!after.ModTime().Equal(before.ModTime()) || !afterPath.ModTime().Equal(before.ModTime()) {
		return pathDigest{}, ErrConflict
	}
	return pathDigest{
		Path:        path,
		SHA256:      hex.EncodeToString(hash.Sum(nil)),
		IndexedStat: indexedStat{Size: before.Size(), ModifiedNS: before.ModTime().UnixNano()},
	}, nil
}

func refreshedRecord(raw []byte, target pathDigest) ([]byte, bool, error) {
	var rec record
	if json.Unmarshal(raw, &rec) != nil || rec.State != "published" || !rec.Verified || !rec.LibraryPublished || !sameReleasePath(rec.Path, target.Path) {
		return nil, false, fmt.Errorf("%w: release record does not match edited path", ErrConflict)
	}
	if rec.SHA256 == target.SHA256 && rec.IndexedStat != nil && rec.IndexedStat.Size == target.IndexedStat.Size && rec.IndexedStat.ModifiedNS == target.IndexedStat.ModifiedNS {
		return nil, false, nil
	}
	rec.SHA256 = target.SHA256
	rec.IndexedStat = &indexedStat{Size: target.IndexedStat.Size, ModifiedNS: target.IndexedStat.ModifiedNS}
	encoded, err := json.Marshal(rec)
	if err != nil {
		return nil, false, err
	}
	return encoded, true, nil
}

func pathKey(path string) string {
	if absolute, err := filepath.Abs(filepath.Clean(path)); err == nil {
		path = absolute
	}
	// Windows temporary directories and caller-supplied library paths can use
	// 8.3 aliases or a symlinked parent. Resolve them before comparing so a
	// normal tag write cannot silently miss every receipt for the same file.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func sameReleasePath(left, right string) bool {
	return pathKey(left) == pathKey(right)
}
