package audiorelease

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

// DedupeAliasInput links one published track to the keeper chosen by a
// completed duplicate merge. The original publication remains auditable in
// AliasedFrom so restoring the quarantine can move the record back.
type DedupeAliasInput struct {
	Context         context.Context
	RequestID       string
	RootID          string
	Track           int
	KeeperRequestID string
	KeeperPath      string
	KeeperSHA256    string
	KeeperFormat    string
	Library         string
	Decoder         Decoder
}

// DedupeAliasBatch is ready to join the merge receipt and every affected
// release record in one Pebble CompareAndSwap.
type DedupeAliasBatch struct {
	Conditions []dataengine.Condition
	Mutations  []dataengine.Mutation
	Linked     bool
}

// PrepareDedupeAlias validates the keeper bytes and builds an atomic catalogue
// update for one quarantined duplicate. A metadata-only file has no published
// receipt and returns Linked=false without inventing one.
func PrepareDedupeAlias(engine *dataengine.Engine, input DedupeAliasInput) (DedupeAliasBatch, error) {
	empty := DedupeAliasBatch{}
	if engine == nil || input.Context == nil || !validID(input.RequestID) || !validID(input.RootID) || input.Track < 1 || input.Track > 999 {
		return empty, dataengine.ErrInvalidRequest
	}
	raw, found, err := readRaw(engine, releaseKey(input.RequestID))
	if err != nil || !found {
		return empty, err
	}
	var rec record
	if json.Unmarshal(raw, &rec) != nil || rec.State != "published" || !rec.Verified || !rec.LibraryPublished {
		return empty, ErrConflict
	}
	if rec.RootID != input.RootID {
		return empty, fmt.Errorf("%w: published root does not match merge receipt", ErrConflict)
	}
	library, err := filepath.Abs(filepath.Clean(input.Library))
	if err != nil || library != filepath.Clean(library) {
		return empty, dataengine.ErrInvalidRequest
	}
	keeper, err := filepath.Abs(filepath.Clean(input.KeeperPath))
	if err != nil || keeper != filepath.Clean(keeper) || !inside(library, keeper) {
		return empty, dataengine.ErrInvalidRequest
	}
	realLibrary, err := filepath.EvalSymlinks(library)
	if err != nil {
		return empty, fmt.Errorf("%w: keeper library is unavailable", ErrMissing)
	}
	realKeeper, err := filepath.EvalSymlinks(keeper)
	if err != nil {
		return empty, fmt.Errorf("%w: dedupe keeper is unavailable", ErrMissing)
	}
	if !inside(realLibrary, realKeeper) {
		return empty, dataengine.ErrInvalidRequest
	}
	info, err := os.Lstat(keeper)
	if err != nil || !info.Mode().IsRegular() {
		return empty, fmt.Errorf("%w: dedupe keeper is not a regular file", ErrMissing)
	}
	if originalInfo, statErr := os.Lstat(rec.Path); statErr == nil && os.SameFile(originalInfo, info) {
		return empty, fmt.Errorf("%w: dedupe keeper cannot alias the same physical publication", ErrConflict)
	}
	format := strings.ToLower(strings.TrimSpace(input.KeeperFormat))
	if format == "" {
		format = strings.ToLower(strings.TrimPrefix(filepath.Ext(keeper), "."))
	}
	if !supportedIndexFormat(format) {
		return empty, fmt.Errorf("%w: dedupe keeper format %q is unsupported", dataengine.ErrInvalidRequest, format)
	}
	digest := strings.ToLower(strings.TrimSpace(input.KeeperSHA256))
	if digest == "" {
		body, readErr := readRecoveryAudio(keeper, MaxStagedAudioBytes)
		if readErr != nil {
			return empty, readErr
		}
		sum := sha256.Sum256(body)
		digest = hex.EncodeToString(sum[:])
	}
	if len(digest) != sha256.Size*2 {
		return empty, dataengine.ErrInvalidRequest
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return empty, dataengine.ErrInvalidRequest
	}
	if err := sameFile(keeper, digest); err != nil {
		return empty, fmt.Errorf("%w: dedupe keeper changed before catalogue commit", ErrConflict)
	}
	actualFormat, decodeState, err := validateRecoveryCandidate(input.Context, keeper, digest, format, input.Decoder)
	if err != nil {
		return empty, err
	}
	snapshot, err := indexedStatForPath(keeper)
	if err != nil {
		return empty, err
	}

	pathChanged := !sameReleasePath(rec.Path, keeper)
	updated := rec
	if updated.AliasedFrom == nil {
		updated.AliasedFrom = &aliasOrigin{
			Path: rec.Path, SHA256: rec.SHA256, Format: rec.Format, DecodeState: rec.DecodeState,
			Origin: rec.Origin, SourceName: rec.SourceName, Library: rec.Library,
		}
	}
	updated.Path = keeper
	updated.SHA256 = digest
	updated.Format = actualFormat
	updated.DecodeState = decodeState
	updated.Library = library
	updated.IndexedStat = snapshot
	updated.AliasOfRequestID = strings.TrimSpace(input.KeeperRequestID)
	if rec.Path == updated.Path && rec.SHA256 == updated.SHA256 && rec.AliasOfRequestID == updated.AliasOfRequestID {
		return empty, nil
	}
	encoded, err := json.Marshal(updated)
	if err != nil {
		return empty, err
	}
	batch := DedupeAliasBatch{
		Conditions: []dataengine.Condition{{Key: releaseKey(input.RequestID), Value: raw}},
		Mutations:  []dataengine.Mutation{{Key: releaseKey(input.RequestID), Value: encoded}},
		Linked:     true,
	}
	if pathChanged {
		if err := appendPathReleaseIndexIfMissing(engine, &batch.Conditions, &batch.Mutations, keeper, input.RequestID); err != nil {
			return empty, err
		}
	}
	keys, err := albumKeysFor(engine, rec.RootID)
	if err != nil {
		return empty, err
	}
	matched := 0
	for _, key := range keys {
		keyRaw, ok, readErr := readRaw(engine, key)
		if readErr != nil || !ok {
			continue
		}
		var keyed record
		if json.Unmarshal(keyRaw, &keyed) != nil || keyed.RequestID != input.RequestID {
			continue
		}
		matched++
		batch.Conditions = append(batch.Conditions, dataengine.Condition{Key: key, Value: keyRaw})
		batch.Mutations = append(batch.Mutations, dataengine.Mutation{Key: key, Value: encoded})
		if pathChanged {
			if err := appendPathAlbumIndexIfMissing(engine, &batch.Conditions, &batch.Mutations, keeper, input.RequestID, key); err != nil {
				return empty, err
			}
		}
	}
	if matched == 0 {
		key := albumKey(rec.RootID, input.Track)
		if _, ok, readErr := readRaw(engine, key); readErr != nil {
			return empty, readErr
		} else if ok {
			key = alternateAlbumKey(rec.RootID, input.Track, input.RequestID)
			if _, existing, alternateErr := readRaw(engine, key); alternateErr != nil || existing {
				if alternateErr != nil {
					return empty, alternateErr
				}
				return empty, ErrConflict
			}
		}
		batch.Conditions = append(batch.Conditions, dataengine.Condition{Key: key, Absent: true})
		batch.Mutations = append(batch.Mutations, dataengine.Mutation{Key: key, Value: encoded})
		if pathChanged {
			if err := appendPathAlbumIndexIfMissing(engine, &batch.Conditions, &batch.Mutations, keeper, input.RequestID, key); err != nil {
				return empty, err
			}
		}
	}
	return batch, nil
}
