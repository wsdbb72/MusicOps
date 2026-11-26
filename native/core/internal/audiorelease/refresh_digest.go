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

	type plannedUpdate struct {
		key []byte
		raw []byte
		new []byte
	}
	var updates []plannedUpdate
	releaseTargets := make(map[string]pathDigest)
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

		if err := scan([]byte("library/audio/"), func(key, raw []byte) error {
			var rec record
			if json.Unmarshal(raw, &rec) != nil {
				return nil
			}
			target, ok := targets[pathKey(rec.Path)]
			if !ok {
				return nil
			}
			updated, changed, err := refreshedRecord(raw, target)
			if err != nil {
				return err
			}
			if changed {
				updates = append(updates, plannedUpdate{key: append([]byte(nil), key...), raw: append([]byte(nil), raw...), new: updated})
			}
			releaseTargets[rec.RequestID] = target
			return nil
		}); err != nil {
			return err
		}

		return scan([]byte("library/album-audio/"), func(key, raw []byte) error {
			var rec record
			if json.Unmarshal(raw, &rec) != nil {
				return nil
			}
			target, ok := releaseTargets[rec.RequestID]
			if !ok || !sameReleasePath(rec.Path, target.Path) {
				return nil
			}
			updated, changed, err := refreshedRecord(raw, target)
			if err != nil {
				return err
			}
			if changed {
				updates = append(updates, plannedUpdate{key: append([]byte(nil), key...), raw: append([]byte(nil), raw...), new: updated})
			}
			return nil
		})
	})
	if err != nil {
		return 0, err
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
	if size != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
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
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func sameReleasePath(left, right string) bool {
	return pathKey(left) == pathKey(right)
}
