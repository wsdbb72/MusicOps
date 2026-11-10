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
	RequestID  string
	RootID     string
	Track      int
	Path       string
	Library    string
	Format     string
	SourceName string
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
	rec := record{
		RequestID: item.RequestID, RootID: item.RootID, Path: item.Path, SHA256: sum,
		Format: format, DecodeState: "structural", Origin: "local_import",
		SourceName: item.SourceName, State: "published", Verified: true, LibraryPublished: true,
		Library: item.Library, Downloaded: false,
	}
	if rec.SourceName == "" {
		rec.SourceName = "migrated"
	}
	encoded, err := json.Marshal(rec)
	if err != nil {
		return false, err
	}
	ok, err := engine.CompareAndSwap(
		[]dataengine.Condition{
			{Key: releaseKey(item.RequestID), Absent: true},
			{Key: albumKey(item.RootID, item.Track), Absent: true},
		},
		[]dataengine.Mutation{
			{Key: releaseKey(item.RequestID), Value: encoded},
			{Key: albumKey(item.RootID, item.Track), Value: encoded},
		},
	)
	if err != nil || ok {
		return ok, err
	}
	var current record
	err = engine.View(func(v *dataengine.View) error {
		raw, found, err := v.Get(releaseKey(item.RequestID))
		if err != nil || !found {
			return err
		}
		if json.Unmarshal(raw, &current) != nil {
			return dataengine.ErrInvalidRequest
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return current.Path == item.Path && current.SourceName == rec.SourceName, nil
}

// RemoveIndexed deletes one indexed record. It does not delete the audio file.
func RemoveIndexed(engine *dataengine.Engine, requestID, rootID string, track int) error {
	if engine == nil || !validID(requestID) || !validID(rootID) {
		return dataengine.ErrInvalidRequest
	}
	ok, err := engine.CompareAndSwap(nil, []dataengine.Mutation{
		{Key: releaseKey(requestID), Delete: true},
		{Key: albumKey(rootID, track), Delete: true},
	})
	if err != nil {
		return err
	}
	if !ok {
		return dataengine.ErrInvalidRequest
	}
	return nil
}
