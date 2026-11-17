package audiorelease

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

// Existing is one audio file that already lives in the user's library.
// IndexExisting records it without copying or modifying the file.
type Existing struct {
	RequestID    string
	RootID       string
	Track        int
	Path         string
	Library      string
	Format       string
	SourceName   string
	MetadataOnly bool
}

// IndexExisting writes a library record that points at an existing file.
// A second call for the same request id does nothing. Downloaded stays false.
func IndexExisting(engine *dataengine.Engine, item Existing) (bool, error) {
	if engine == nil || !validID(item.RequestID) || !validID(item.RootID) || item.Track < 1 || item.Track > 999 {
		return false, dataengine.ErrInvalidRequest
	}
	if !filepath.IsAbs(item.Path) || item.Path != filepath.Clean(item.Path) {
		return false, dataengine.ErrInvalidRequest
	}
	format := strings.ToLower(strings.TrimPrefix(filepath.Ext(item.Path), "."))
	if item.Format != "" {
		format = item.Format
	}
	switch format {
	case "flac", "mp3", "wav", "aac", "alac", "aiff", "m4a", "ogg", "ape":
	default:
		return false, dataengine.ErrInvalidRequest
	}
	sum := ""
	var snapshot *indexedStat
	if item.MetadataOnly {
		info, err := os.Lstat(item.Path)
		if err != nil {
			return false, err
		}
		if !info.Mode().IsRegular() {
			return false, dataengine.ErrInvalidRequest
		}
		snapshot = &indexedStat{Size: info.Size(), ModifiedNS: info.ModTime().UnixNano()}
	} else {
		file, err := os.Open(item.Path)
		if err == nil {
			hash := sha256.New()
			_, copyErr := io.Copy(hash, file)
			_ = file.Close()
			if copyErr != nil {
				return false, copyErr
			}
			sum = hex.EncodeToString(hash.Sum(nil))
		} else if !os.IsNotExist(err) {
			return false, err
		}
	}
	rec := record{
		RequestID: item.RequestID, RootID: item.RootID, Path: item.Path, SHA256: sum,
		Format: format, DecodeState: "structural", Origin: "local_import",
		SourceName: item.SourceName, State: "published", Verified: true, LibraryPublished: true,
		Library: item.Library, Downloaded: false, IndexedStat: snapshot,
	}
	if rec.SourceName == "" {
		rec.SourceName = "migrated"
	}
	encoded, err := json.Marshal(rec)
	if err != nil {
		return false, err
	}
	primary := albumKey(item.RootID, item.Track)
	for _, key := range [][]byte{primary, alternateAlbumKey(item.RootID, item.Track, item.RequestID)} {
		ok, err := indexRecordAt(engine, item.RequestID, item.Path, key, encoded)
		if err != nil || ok {
			return ok, err
		}
		if _, found, err := readRelease(engine, item.RequestID); err != nil || found {
			return false, err
		}
	}
	return false, dataengine.ErrInvalidRequest
}

// indexRecordAt writes the request record and one album mapping atomically.
// A stale metadata snapshot for the same logical album track is replaced in
// place. This is the KV equivalent of an UPSERT on (root, track, file): a
// converted or renamed file does not leave the old format mapped alongside
// the new one. Verified downloaded audio is never displaced by a metadata
// index; that case falls back to the path-qualified key.
func indexRecordAt(engine *dataengine.Engine, requestID, path string, key, encoded []byte) (bool, error) {
	if _, found, err := readRaw(engine, releaseKey(requestID)); err != nil || found {
		return false, err
	}
	currentRaw, found, err := readRaw(engine, key)
	if err != nil {
		return false, err
	}
	conditions := []dataengine.Condition{{Key: releaseKey(requestID), Absent: true}}
	mutations := []dataengine.Mutation{
		{Key: releaseKey(requestID), Value: encoded},
		{Key: key, Value: encoded},
	}
	if found {
		var current record
		if json.Unmarshal(currentRaw, &current) != nil {
			return false, ErrConflict
		}
		if current.RequestID == requestID {
			return false, nil
		}
		// Only a different extension/re-encode of the same file stem is a
		// version replacement. Two unnumbered files in one folder share a
		// track number but are separate recordings and must keep separate keys.
		if !sameLogicalStem(current.Path, path) {
			return false, nil
		}
		if current.IndexedStat == nil && current.SourceName != "wav-convert" {
			return false, nil
		}
		conditions = append(conditions, dataengine.Condition{Key: key, Value: currentRaw})
		if oldRaw, oldFound, readErr := readRaw(engine, releaseKey(current.RequestID)); readErr != nil {
			return false, readErr
		} else if oldFound {
			var old record
			if json.Unmarshal(oldRaw, &old) != nil || old.RequestID != current.RequestID {
				return false, ErrConflict
			}
			conditions = append(conditions, dataengine.Condition{Key: releaseKey(current.RequestID), Value: oldRaw})
			mutations = append(mutations, dataengine.Mutation{Key: releaseKey(current.RequestID), Delete: true})
		}
	} else {
		conditions = append(conditions, dataengine.Condition{Key: key, Absent: true})
	}
	return engine.CompareAndSwap(conditions, mutations)
}

func sameLogicalStem(left, right string) bool {
	left, right = filepath.Clean(left), filepath.Clean(right)
	if filepath.Dir(left) != filepath.Dir(right) {
		return false
	}
	left = strings.TrimSuffix(filepath.Base(left), filepath.Ext(left))
	right = strings.TrimSuffix(filepath.Base(right), filepath.Ext(right))
	return left != "" && strings.EqualFold(left, right)
}

func alternateAlbumKey(root string, track int, request string) []byte {
	return append(albumKey(root, track), []byte(":"+request)...)
}

// RemoveIndexed deletes one indexed record. It does not delete the audio file.
func RemoveIndexed(engine *dataengine.Engine, requestID, rootID string, track int) error {
	if engine == nil || !validID(requestID) || !validID(rootID) {
		return dataengine.ErrInvalidRequest
	}
	for _, key := range [][]byte{albumKey(rootID, track), alternateAlbumKey(rootID, track, requestID)} {
		var raw []byte
		var found bool
		err := engine.View(func(v *dataengine.View) error {
			var err error
			raw, found, err = v.Get(key)
			return err
		})
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		var current record
		if json.Unmarshal(raw, &current) != nil {
			return dataengine.ErrInvalidRequest
		}
		if current.RequestID != requestID {
			continue
		}
		ok, err := engine.CompareAndSwap(
			[]dataengine.Condition{{Key: key, Value: raw}},
			[]dataengine.Mutation{{Key: releaseKey(requestID), Delete: true}, {Key: key, Delete: true}},
		)
		if err != nil {
			return err
		}
		if !ok {
			return dataengine.ErrInvalidRequest
		}
		return nil
	}
	return nil
}

type indexedStat struct {
	Size       int64 `json:"size"`
	ModifiedNS int64 `json:"modified_ns"`
}

// PresenceUnchanged checks the published digest or the metadata snapshot of
// an existing library file. A metadata index is not a verified download.
func PresenceUnchanged(engine *dataengine.Engine, requestID string) bool {
	rec, found, err := readRelease(engine, requestID)
	if err != nil || !found || rec.State != "published" || !rec.LibraryPublished {
		return false
	}
	if rec.IndexedStat == nil {
		return rec.SHA256 != "" && sameFile(rec.Path, rec.SHA256) == nil
	}
	info, err := os.Lstat(rec.Path)
	return err == nil && info.Mode().IsRegular() && info.Size() == rec.IndexedStat.Size && info.ModTime().UnixNano() == rec.IndexedStat.ModifiedNS
}

// IndexSnapshots commits a bounded group of existing files in one durable
// write. Unique (root, track) rows use the primary mapping; a collision such
// as two unnumbered files falls back to a path-qualified key so neither file
// is lost.
func IndexSnapshots(engine *dataengine.Engine, items []Existing) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	conditions := make([]dataengine.Condition, 0, len(items)*2)
	mutations := make([]dataengine.Mutation, 0, len(items)*2)
	keys := make(map[string]bool, len(items))
	batchable := true
	for _, item := range items {
		if !validID(item.RequestID) || !validID(item.RootID) || item.Track < 1 || item.Track > 999 ||
			!filepath.IsAbs(item.Path) || item.Path != filepath.Clean(item.Path) {
			return 0, dataengine.ErrInvalidRequest
		}
		info, err := os.Lstat(item.Path)
		if err != nil {
			return 0, err
		}
		if !info.Mode().IsRegular() {
			return 0, dataengine.ErrInvalidRequest
		}
		rec := record{RequestID: item.RequestID, RootID: item.RootID, Path: item.Path, Format: item.Format,
			Library: item.Library, SourceName: item.SourceName, Origin: "local_import", State: "published",
			Verified: true, LibraryPublished: true, DecodeState: "not_run",
			IndexedStat: &indexedStat{Size: info.Size(), ModifiedNS: info.ModTime().UnixNano()}}
		if rec.SourceName == "" {
			rec.SourceName = "migrated"
		}
		encoded, err := json.Marshal(rec)
		if err != nil {
			return 0, err
		}
		primary := albumKey(item.RootID, item.Track)
		if keys[string(primary)] {
			batchable = false
			continue
		}
		keys[string(primary)] = true
		conditions = append(conditions,
			dataengine.Condition{Key: releaseKey(item.RequestID), Absent: true},
			dataengine.Condition{Key: primary, Absent: true},
		)
		mutations = append(mutations,
			dataengine.Mutation{Key: releaseKey(item.RequestID), Value: encoded},
			dataengine.Mutation{Key: primary, Value: encoded},
		)
	}
	if batchable {
		ok, err := engine.CompareAndSwap(conditions, mutations)
		if err != nil {
			return 0, err
		}
		if ok {
			return len(items), nil
		}
	}
	added := 0
	for _, item := range items {
		item.MetadataOnly = true
		ok, err := IndexExisting(engine, item)
		if err != nil {
			return added, err
		}
		if ok {
			added++
		}
	}
	return added, nil
}

// PruneMissingIndexed removes metadata-only mappings whose files are gone.
// A missing verified download stays in the catalogue so the gap and its
// history remain visible. Only one full library scan calls this, after the
// walk has succeeded, and paths outside the configured library are ignored.
func PruneMissingIndexed(engine *dataengine.Engine, root string) (int, error) {
	if engine == nil || root == "" || !filepath.IsAbs(root) || root != filepath.Clean(root) {
		return 0, dataengine.ErrInvalidRequest
	}
	type staleRecord struct {
		rec  record
		keys [][]byte
		raws [][]byte
	}
	stale := map[string]*staleRecord{}
	err := engine.View(func(v *dataengine.View) error {
		var after []byte
		for {
			page, err := v.Scan([]byte("library/album-audio/"), after, 200)
			if err != nil {
				return err
			}
			for _, item := range page.Items {
				var rec record
				if json.Unmarshal(item.Value, &rec) != nil || rec.RequestID == "" {
					continue
				}
				if rec.IndexedStat == nil && rec.SourceName != "wav-convert" {
					continue
				}
				if rec.Library != root && !inside(root, rec.Path) {
					continue
				}
				if _, statErr := os.Stat(rec.Path); statErr == nil {
					continue
				} else if !os.IsNotExist(statErr) {
					return statErr
				}
				group := stale[rec.RequestID]
				if group == nil {
					group = &staleRecord{rec: rec}
					stale[rec.RequestID] = group
				}
				group.keys = append(group.keys, append([]byte(nil), item.Key...))
				group.raws = append(group.raws, append([]byte(nil), item.Value...))
			}
			if len(page.Next) == 0 {
				return nil
			}
			after = page.Next
		}
	})
	if err != nil {
		return 0, err
	}
	removed := 0
	for requestID, group := range stale {
		conditions := make([]dataengine.Condition, 0, len(group.keys)+1)
		mutations := make([]dataengine.Mutation, 0, len(group.keys)+1)
		for i, key := range group.keys {
			conditions = append(conditions, dataengine.Condition{Key: key, Value: group.raws[i]})
			mutations = append(mutations, dataengine.Mutation{Key: key, Delete: true})
		}
		if raw, found, readErr := readRaw(engine, releaseKey(requestID)); readErr != nil {
			return removed, readErr
		} else if found {
			conditions = append(conditions, dataengine.Condition{Key: releaseKey(requestID), Value: raw})
			mutations = append(mutations, dataengine.Mutation{Key: releaseKey(requestID), Delete: true})
		}
		ok, err := engine.CompareAndSwap(conditions, mutations)
		if err != nil {
			return removed, err
		}
		if ok {
			removed++
		}
	}
	return removed, nil
}
