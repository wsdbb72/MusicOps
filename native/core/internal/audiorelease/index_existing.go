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
	case "flac", "mp3", "wav", "aac", "alac", "aiff", "m4a", "ogg":
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
		ok, err := engine.CompareAndSwap(
			[]dataengine.Condition{{Key: releaseKey(item.RequestID), Absent: true}, {Key: key, Absent: true}},
			[]dataengine.Mutation{{Key: releaseKey(item.RequestID), Value: encoded}, {Key: key, Value: encoded}},
		)
		if err != nil || ok {
			return ok, err
		}
		if _, found, err := readRelease(engine, item.RequestID); err != nil || found {
			return false, err
		}
	}
	return false, dataengine.ErrInvalidRequest
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
// write. Indexed metadata uses a path-qualified album key, so equally
// numbered files and multi-disc folders have independent records.
func IndexSnapshots(engine *dataengine.Engine, items []Existing) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	conditions := make([]dataengine.Condition, 0, len(items)*2)
	mutations := make([]dataengine.Mutation, 0, len(items)*2)
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
		encoded, err := json.Marshal(rec)
		if err != nil {
			return 0, err
		}
		for _, key := range [][]byte{releaseKey(item.RequestID), alternateAlbumKey(item.RootID, item.Track, item.RequestID)} {
			conditions = append(conditions, dataengine.Condition{Key: key, Absent: true})
			mutations = append(mutations, dataengine.Mutation{Key: key, Value: encoded})
		}
	}
	ok, err := engine.CompareAndSwap(conditions, mutations)
	if err != nil {
		return 0, err
	}
	if ok {
		return len(items), nil
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
