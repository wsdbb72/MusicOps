package audiorelease

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

// RecoverInput describes a verified release receipt whose catalogue record is
// missing even though the published audio may still be present. Recovery never
// trusts a path from the receipt: it searches only inside Library, requires one
// unambiguous file with the recorded digest, and records that exact path under
// the original RequestID.
type RecoverInput struct {
	RequestID  string
	RootID     string
	Track      int
	SHA256     string
	Format     string
	Origin     string
	SourceName string
	PathHint   string
	Library    string
	Decoder    Decoder
}

// RecoverPublished rebuilds a missing published receipt after a library move or
// a stale index prune. A file is adopted only when it is the sole library file
// whose bytes match SHA256. FLAC recovery also requires a successful complete
// decoder pass; a missing decoder is reported as ErrTool instead of silently
// weakening verification.
func RecoverPublished(ctx context.Context, engine *dataengine.Engine, input RecoverInput) (Result, error) {
	empty := Result{RequestID: input.RequestID, RootID: input.RootID}
	if engine == nil || ctx == nil || !validID(input.RequestID) || !validID(input.RootID) ||
		input.Track < 1 || input.Track > 999 || len(input.SHA256) != sha256.Size*2 {
		return empty, dataengine.ErrInvalidRequest
	}
	expected := strings.ToLower(strings.TrimSpace(input.SHA256))
	if _, err := hex.DecodeString(expected); err != nil || expected != input.SHA256 {
		return empty, dataengine.ErrInvalidRequest
	}
	format := strings.ToLower(strings.TrimSpace(input.Format))
	if format != "" && !supportedIndexFormat(format) {
		return empty, dataengine.ErrInvalidRequest
	}
	library, err := filepath.Abs(input.Library)
	if err != nil || library != filepath.Clean(library) {
		return empty, dataengine.ErrInvalidRequest
	}
	info, err := os.Lstat(library)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return empty, fmt.Errorf("%w: library root is unavailable", ErrMissing)
	}

	// A complete release already present is validated exactly like any other
	// replay. A record for another root, digest, or format is never overwritten.
	if rec, found, err := readRelease(engine, input.RequestID); err != nil {
		return empty, err
	} else if found {
		aliasRestore := rec.AliasedFrom != nil && (rec.AliasedFrom.SHA256 == expected || rec.AliasedFrom.SHA256 == "") &&
			(format == "" || rec.AliasedFrom.Format == format)
		if rec.RootID != input.RootID || !aliasRestore && (rec.SHA256 != expected || format != "" && rec.Format != format) {
			return empty, fmt.Errorf("%w: existing release does not match the receipt", ErrConflict)
		}
		if aliasRestore {
			path, findErr := findRecoveryFile(ctx, library, expected, format, input.Track, input.SourceName, input.PathHint)
			if findErr != nil {
				return empty, findErr
			}
			actualFormat, decodeState, validateErr := validateRecoveryCandidate(ctx, path, expected, format, input.Decoder)
			if validateErr != nil {
				return empty, validateErr
			}
			return restoreAliasedRelease(engine, input.RequestID, path, expected, actualFormat, decodeState, library)
		}
		if verified, verifyErr := Verify(engine, input.RequestID); verifyErr == nil {
			return verified, nil
		}
		// A move can leave the catalogue pointing at the old path while the
		// unique bytes already exist back in the library. Relocate only after the
		// same strict digest/decoder scan used for a missing receipt.
		path, findErr := findRecoveryFile(ctx, library, expected, format, input.Track, input.SourceName, input.PathHint)
		if findErr != nil {
			return empty, findErr
		}
		if _, _, err := validateRecoveryCandidate(ctx, path, expected, format, input.Decoder); err != nil {
			return empty, err
		}
		if err := Relocate(engine, input.RequestID, path); err != nil {
			return empty, err
		}
		return Verify(engine, input.RequestID)
	}

	path, err := findRecoveryFile(ctx, library, expected, format, input.Track, input.SourceName, input.PathHint)
	if err != nil {
		return empty, err
	}
	actualFormat, decodeState, err := validateRecoveryCandidate(ctx, path, expected, format, input.Decoder)
	if err != nil {
		return empty, err
	}
	snapshot, err := indexedStatForPath(path)
	if err != nil {
		return empty, err
	}
	origin := strings.TrimSpace(input.Origin)
	if origin == "" {
		origin = "recovered"
	}
	sourceName := strings.TrimSpace(input.SourceName)
	if sourceName == "" || sourceName != filepath.Base(sourceName) {
		sourceName = filepath.Base(path)
	}
	rec := record{
		RequestID: input.RequestID, RootID: input.RootID, Path: path, SHA256: expected,
		Format: actualFormat, DecodeState: decodeState, Origin: origin, SourceName: sourceName,
		State: "published", Verified: true, LibraryPublished: true, Library: library, IndexedStat: snapshot,
	}
	encoded, err := json.Marshal(rec)
	if err != nil {
		return empty, err
	}
	ok, err := writeRecoveredRelease(engine, input.RequestID, albumKey(input.RootID, input.Track), encoded)
	if err != nil {
		return empty, err
	}
	if !ok {
		current, found, readErr := readRelease(engine, input.RequestID)
		if readErr != nil {
			return empty, readErr
		}
		if found && current.RootID == input.RootID && current.SHA256 == expected &&
			(format == "" || current.Format == format) {
			return Verify(engine, input.RequestID)
		}
		if !found {
			// A metadata-only row may legitimately occupy the primary
			// (root, track) slot. Keep that row and use the request-qualified
			// album key, the same collision rule used by reindexing.
			ok, err = writeRecoveredRelease(engine, input.RequestID, alternateAlbumKey(input.RootID, input.Track, input.RequestID), encoded)
			if err != nil {
				return empty, err
			}
		}
	}
	if !ok {
		return empty, fmt.Errorf("%w: release slot changed during recovery", ErrConflict)
	}
	// The original incoming source is not known here; only the library file
	// is attested.
	return resultFrom(rec, false, false), nil
}

func restoreAliasedRelease(engine *dataengine.Engine, requestID, path, expected, format, decodeState, library string) (Result, error) {
	raw, found, err := readRaw(engine, releaseKey(requestID))
	if err != nil || !found {
		return Result{}, err
	}
	var rec record
	if json.Unmarshal(raw, &rec) != nil || rec.AliasedFrom == nil || (rec.AliasedFrom.SHA256 != expected && rec.AliasedFrom.SHA256 != "") {
		return Result{}, ErrConflict
	}
	snapshot, err := indexedStatForPath(path)
	if err != nil {
		return Result{}, err
	}
	original := *rec.AliasedFrom
	updated := rec
	updated.Path = path
	updated.SHA256 = expected
	updated.Format = format
	updated.DecodeState = decodeState
	updated.Library = library
	updated.IndexedStat = snapshot
	if original.Origin != "" {
		updated.Origin = original.Origin
	}
	if original.SourceName != "" {
		updated.SourceName = original.SourceName
	}
	updated.AliasedFrom = nil
	updated.AliasOfRequestID = ""
	encoded, err := json.Marshal(updated)
	if err != nil {
		return Result{}, err
	}
	conditions := []dataengine.Condition{{Key: releaseKey(requestID), Value: raw}}
	mutations := []dataengine.Mutation{{Key: releaseKey(requestID), Value: encoded}}
	keys, err := albumKeysFor(engine, rec.RootID)
	if err != nil {
		return Result{}, err
	}
	for _, key := range keys {
		keyRaw, ok, readErr := readRaw(engine, key)
		if readErr != nil || !ok {
			continue
		}
		var keyed record
		if json.Unmarshal(keyRaw, &keyed) != nil || keyed.RequestID != requestID {
			continue
		}
		conditions = append(conditions, dataengine.Condition{Key: key, Value: keyRaw})
		mutations = append(mutations, dataengine.Mutation{Key: key, Value: encoded})
	}
	ok, err := engine.CompareAndSwap(conditions, mutations)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, ErrConflict
	}
	return Verify(engine, requestID)
}

func writeRecoveredRelease(engine *dataengine.Engine, requestID string, key, encoded []byte) (bool, error) {
	return engine.CompareAndSwap(
		[]dataengine.Condition{
			{Key: releaseKey(requestID), Absent: true},
			{Key: key, Absent: true},
		},
		[]dataengine.Mutation{
			{Key: releaseKey(requestID), Value: encoded},
			{Key: key, Value: encoded},
		},
	)
}

func validateRecoveryCandidate(ctx context.Context, path, expected, format string, decoder Decoder) (string, string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("%w: recovery candidate disappeared", ErrMissing)
		}
		return "", "", err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return "", "", ErrRejected
	}
	if info.Size() > MaxStagedAudioBytes {
		format = strings.ToLower(strings.TrimSpace(format))
		if format == "" || !supportedIndexFormat(format) ||
			!strings.EqualFold(strings.TrimPrefix(filepath.Ext(path), "."), format) {
			return "", "", fmt.Errorf("%w: large indexed candidate needs a supported explicit format", ErrRejected)
		}
		decodeState := "structural"
		if decoder != nil {
			if err := decoder(ctx, path); err != nil {
				return "", "", fmt.Errorf("%w: audio decode failed: %v", ErrRejected, err)
			}
			decodeState = "decoded"
		} else if format == "flac" {
			return "", "", ErrTool
		}
		if err := sameFile(path, expected); err != nil {
			return "", "", fmt.Errorf("%w: recovery candidate changed before catalogue commit", ErrConflict)
		}
		return format, decodeState, nil
	}
	body, err := readRecoveryAudio(path, MaxStagedAudioBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("%w: recovery candidate disappeared", ErrMissing)
		}
		return "", "", err
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != expected {
		return "", "", fmt.Errorf("%w: recovery candidate changed after discovery", ErrConflict)
	}
	actualFormat, reason := classifyLimited(filepath.Base(path), body, MaxStagedAudioBytes)
	if reason != "" && format != "" && supportedIndexFormat(format) &&
		strings.EqualFold(strings.TrimPrefix(filepath.Ext(path), "."), format) {
		actualFormat, reason = format, ""
	}
	if reason != "" || format != "" && actualFormat != format {
		if reason == "" {
			reason = "format_mismatch"
		}
		return "", "", fmt.Errorf("%w: %s", ErrRejected, reason)
	}
	decodeState := "structural"
	if decoder != nil {
		if err := decoder(ctx, path); err != nil {
			return "", "", fmt.Errorf("%w: audio decode failed: %v", ErrRejected, err)
		}
		decodeState = "decoded"
	} else if actualFormat == "flac" {
		return "", "", ErrTool
	}
	if err := sameFile(path, expected); err != nil {
		return "", "", fmt.Errorf("%w: recovery candidate changed before catalogue commit", ErrConflict)
	}
	return actualFormat, decodeState, nil
}

func findRecoveryFile(ctx context.Context, library, digest, format string, track int, sourceName, pathHint string) (string, error) {
	if strings.TrimSpace(pathHint) != "" {
		path, err := filepath.Abs(filepath.Clean(strings.TrimSpace(pathHint)))
		if err != nil || !inside(library, path) {
			return "", dataengine.ErrInvalidRequest
		}
		realLibrary, rootErr := filepath.EvalSymlinks(library)
		realPath, pathErr := filepath.EvalSymlinks(path)
		if rootErr == nil && pathErr == nil && !inside(realLibrary, realPath) {
			return "", dataengine.ErrInvalidRequest
		}
		// A path hint narrows recovery but is not authoritative: a dedupe or
		// maintenance move can leave the old receipt path behind. Accept the
		// exact candidate only when its bytes and container are valid; otherwise
		// continue with the unique-digest scan below. Escaping the library is
		// still rejected immediately.
		if rootErr == nil && pathErr == nil {
			if info, statErr := os.Lstat(path); statErr == nil && info.Mode().IsRegular() {
				if _, _, validateErr := validateRecoveryCandidate(ctx, path, digest, format, nil); validateErr == nil || errors.Is(validateErr, ErrTool) {
					return path, nil
				}
			}
		}
	}
	hint := filepath.Base(strings.TrimSpace(sourceName))
	trackPrefix := fmt.Sprintf("%02d ", track)
	if hint != "." && hint != string(filepath.Separator) {
		hinted, err := scanRecoveryFiles(ctx, library, digest, format, func(name string) bool {
			return strings.EqualFold(name, hint) || strings.HasPrefix(strings.ToLower(name), strings.ToLower(trackPrefix))
		}, 0)
		if err != nil {
			return "", err
		}
		if hinted != "" {
			// The filename narrows the first hash pass. A second walk over all
			// files of the same size proves the digest is still unique even if
			// the same bytes were copied under another name.
			info, statErr := os.Stat(hinted)
			if statErr != nil {
				return "", fmt.Errorf("%w: recovery candidate disappeared", ErrMissing)
			}
			return scanRecoveryFiles(ctx, library, digest, format, nil, info.Size())
		}
	}
	// Older receipts may not carry a useful source name, and a user may have
	// renamed the file. Fall back to the complete library so the recovery is
	// functional rather than dependent on a filename heuristic.
	return scanRecoveryFiles(ctx, library, digest, format, nil, 0)
}

func scanRecoveryFiles(ctx context.Context, library, digest, format string, include func(string) bool, size int64) (string, error) {
	var match string
	err := filepath.WalkDir(library, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) || errors.Is(walkErr, fs.ErrPermission) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		name := entry.Name()
		if include != nil && !include(name) {
			return nil
		}
		if size > 0 {
			info, infoErr := entry.Info()
			if infoErr != nil {
				if errors.Is(infoErr, fs.ErrNotExist) || errors.Is(infoErr, fs.ErrPermission) {
					return nil
				}
				return infoErr
			}
			if info.Size() != size {
				return nil
			}
		}
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
		if format != "" && (!supportedIndexFormat(format) || ext != format) ||
			format == "" && ext != "flac" && ext != "wav" {
			return nil
		}
		candidateDigest, err := recoveryFileDigest(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, fs.ErrPermission) || errors.Is(err, dataengine.ErrInvalidRequest) {
				return nil
			}
			return err
		}
		if candidateDigest != digest {
			return nil
		}
		if match != "" {
			return fmt.Errorf("%w: multiple library files match digest %s (%s and %s)", ErrConflict, digest, match, path)
		}
		match = path
		return nil
	})
	if err != nil {
		return "", err
	}
	if match == "" {
		return "", fmt.Errorf("%w: no library file matches digest %s", ErrMissing, digest)
	}
	return match, nil
}

func recoveryFileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() || before.Size() <= 0 {
		return "", dataengine.ErrInvalidRequest
	}
	hash := sha256.New()
	written, err := io.Copy(hash, file)
	if err != nil {
		return "", err
	}
	after, err := file.Stat()
	if err != nil {
		return "", err
	}
	if written != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || !os.SameFile(before, after) {
		return "", fmt.Errorf("%w: recovery candidate changed while hashing", ErrConflict)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func readRecoveryAudio(path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > int64(limit) {
		return nil, dataengine.ErrInvalidRequest
	}
	body, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) ||
		!os.SameFile(before, after) {
		return nil, fmt.Errorf("%w: recovery candidate changed while reading", ErrConflict)
	}
	return body, nil
}

// RecoveryTrackFromRequestID verifies the deterministic receipt relationship
// used by the download pipeline. It prevents a stale task from asking recovery
// to attach a file to an unrelated request id.
func RecoveryTrackFromRequestID(rootID, requestID string) (int, bool) {
	for track := 1; track <= 999; track++ {
		sum := sha256.Sum256([]byte("album-audio/" + rootID + "/track/" + strconv.Itoa(track)))
		if requestID == "audio-"+hex.EncodeToString(sum[:16]) {
			return track, true
		}
	}
	return 0, false
}
