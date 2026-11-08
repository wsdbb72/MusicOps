// Package audiorelease copies one verified audio file into this process's library.
// A torrent payload, a fixture marker, or a failed check never becomes a download.
// Production verification stays false: this is file acceptance, not a live-site receipt.
// A local file that was never fetched from a tracker or streaming service is not
// a download either: Downloaded stays false for every local import.
package audiorelease

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

const maxAudioBytes = 32 << 20

var (
	ErrRejected = errors.New("audio rejected")
	ErrMissing  = errors.New("audio file missing")
	ErrTool     = errors.New("audio decoder missing")
	ErrConflict = errors.New("library release conflict")
)

// Decoder checks one file that already passed the container rules.
// A nil decoder means the host has no ffmpeg/ffprobe; the file is then
// accepted on its structural check alone and DecodeState stays "structural".
// WAV and FLAC share this rule: the container check always runs, and a
// decoder only upgrades the check to a full decode.
type Decoder func(context.Context, string) error

// Request names one file already stored under Incoming. Library is this process's
// own library directory, never an arbitrary user music root.
// Origin says how the file reached Incoming: "manual" when a person placed it by
// hand, "local_import" otherwise. An empty Origin means "local_import".
type Request struct {
	RequestID  string
	RootID     string
	Artist     string
	Album      string
	Title      string
	Track      int
	SourceName string
	Origin     string
	Incoming   string
	Library    string
	Decoder    Decoder
}

// Result is the file outcome. Downloaded stays false: the bytes were verified
// from a local file, never fetched from a tracker or streaming service.
// Origin and SourceName record how the file reached Incoming, so a library file
// can be traced back to its source.
type Result struct {
	RequestID          string `json:"request_id"`
	RootID             string `json:"root_id"`
	Format             string `json:"format,omitempty"`
	DecodeState        string `json:"decode_state,omitempty"`
	SHA256             string `json:"sha256,omitempty"`
	Origin             string `json:"origin,omitempty"`
	SourceName         string `json:"source_name,omitempty"`
	Verified           bool   `json:"verified"`
	LibraryPublished   bool   `json:"library_published"`
	Downloaded         bool   `json:"downloaded"`
	SourcePreserved    bool   `json:"source_preserved"`
	Replayed           bool   `json:"replayed"`
	ProductionVerified bool   `json:"production_verified"`
	Class              string `json:"class,omitempty"`
}

type record struct {
	RequestID        string `json:"request_id"`
	RootID           string `json:"root_id"`
	Path             string `json:"path"`
	SHA256           string `json:"sha256"`
	Format           string `json:"format"`
	DecodeState      string `json:"decode_state"`
	Origin           string `json:"origin,omitempty"`
	SourceName       string `json:"source_name,omitempty"`
	State            string `json:"state"`
	Verified         bool   `json:"verified"`
	LibraryPublished bool   `json:"library_published"`
	// Downloaded is retained only so records written before the local-import
	// correction still decode. New records always leave it false: a verified
	// local import is not a download.
	Downloaded bool `json:"downloaded"`
}

// Release verifies the incoming file and publishes a copy. The source stays in place.
// The album is keyed by (RootID, Track): one album root holds one record per track,
// so a second track for the same root publishes as its own file instead of replaying
// the first track's receipt.
func Release(ctx context.Context, engine *dataengine.Engine, input Request) (Result, error) {
	empty := Result{RequestID: input.RequestID, RootID: input.RootID}
	if engine == nil || ctx == nil || !validID(input.RequestID) || !validID(input.RootID) || input.Track < 1 || input.Track > 999 {
		return empty, dataengine.ErrInvalidRequest
	}
	origin := input.Origin
	if origin == "" {
		origin = "local_import"
	}
	if origin != "manual" && origin != "local_import" {
		return empty, dataengine.ErrInvalidRequest
	}
	artist, album, title, err := cleanNames(input.Artist, input.Album, input.Title)
	if err != nil {
		return empty, err
	}
	incoming, library, source, err := locate(input)
	if err != nil {
		if errors.Is(err, ErrMissing) {
			// The source is gone but the release may already be committed:
			// return the stored receipt instead of reporting a failure.
			if receipt, ok, rerr := replayMissing(engine, input.RequestID); rerr == nil && ok {
				return receipt, nil
			}
			empty.Class = "audio_missing"
		}
		if errors.Is(err, ErrRejected) {
			empty.Class = "audio_rejected"
		}
		return empty, err
	}
	if existing, found, err := readRelease(engine, input.RequestID); err != nil {
		return empty, err
	} else if found {
		return replay(existing, source)
	}
	body, err := os.ReadFile(source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if receipt, ok, rerr := replayMissing(engine, input.RequestID); rerr == nil && ok {
				return receipt, nil
			}
			empty.Class = "audio_missing"
			return empty, ErrMissing
		}
		return empty, err
	}
	before := sha256.Sum256(body)
	format, reason := classify(input.SourceName, body)
	if reason != "" {
		empty.Class = reason
		return empty, ErrRejected
	}
	decodeState := "structural"
	if input.Decoder != nil {
		if err = input.Decoder(ctx, source); err != nil {
			empty.Class = "decode_failed"
			empty.Format = format
			return empty, ErrRejected
		}
		decodeState = "decoded"
	}
	digest := hex.EncodeToString(before[:])
	folder := filepath.Join(library, artist, album)
	if err = os.MkdirAll(folder, 0o700); err != nil {
		return empty, err
	}
	if !inside(library, folder) {
		return empty, dataengine.ErrInvalidRequest
	}
	name := trackName(input.Track, title, format)
	dest := filepath.Join(folder, name)
	if !inside(library, dest) || inside(incoming, dest) {
		return empty, dataengine.ErrInvalidRequest
	}
	if _, err = os.Lstat(dest); err == nil {
		return empty, ErrConflict
	}
	temp := filepath.Join(folder, ".musicops-"+input.RequestID+".partial")
	if err = writeNew(temp, body); err != nil {
		os.Remove(temp)
		return empty, err
	}
	if err = os.Rename(temp, dest); err != nil {
		os.Remove(temp)
		return empty, err
	}
	if err = sameFile(dest, digest); err != nil {
		os.Remove(dest)
		return empty, err
	}
	rec := record{
		RequestID: input.RequestID, RootID: input.RootID, Path: dest, SHA256: digest, Format: format,
		DecodeState: decodeState, Origin: origin, SourceName: input.SourceName,
		State: "published", Verified: true, LibraryPublished: true,
		// Downloaded stays false: a verified local import is not a download.
	}
	encoded, err := json.Marshal(rec)
	if err != nil {
		os.Remove(dest)
		return empty, err
	}
	ok, err := engine.CompareAndSwap(
		[]dataengine.Condition{
			{Key: releaseKey(input.RequestID), Absent: true},
			{Key: albumKey(input.RootID, input.Track), Absent: true},
		},
		[]dataengine.Mutation{
			{Key: releaseKey(input.RequestID), Value: encoded},
			{Key: albumKey(input.RootID, input.Track), Value: encoded},
		},
	)
	if err != nil {
		return empty, err
	}
	if !ok {
		current, found, readErr := readRelease(engine, input.RequestID)
		if readErr != nil {
			return empty, readErr
		}
		if found && current.SHA256 == digest && current.RootID == input.RootID {
			return replay(current, source)
		}
		// Our bytes are already in the library but no record claims them.
		// Remove our file when it still holds our bytes, so a lost CAS race
		// cannot leave an orphaned library file behind. A destination whose
		// bytes differ from our digest belongs to another publish and is left alone.
		if data, err := os.ReadFile(dest); err == nil {
			if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) == digest {
				os.Remove(dest)
			}
		}
		return empty, ErrConflict
	}
	// The file and the record are committed. A source that vanished after the
	// copy is reported as not preserved, never as a failed release.
	return resultFrom(rec, sameFile(source, digest) == nil, false), nil
}

// Verify re-checks a published library record: the stored file must still
// hash to the stored digest. It copies nothing and does not look at the
// original source file again, so SourcePreserved is left false: Verify
// attests the library file, not the source.
func Verify(engine *dataengine.Engine, requestID string) (Result, error) {
	rec, found, err := readRelease(engine, requestID)
	if err != nil {
		return Result{}, err
	}
	if !found {
		return Result{}, ErrMissing
	}
	if rec.State != "published" || !rec.Verified || !rec.LibraryPublished {
		return Result{}, ErrConflict
	}
	if err := sameFile(rec.Path, rec.SHA256); err != nil {
		return Result{}, err
	}
	return resultFrom(rec, false, false), nil
}

// Albums returns root ids whose verified audio is in the library.
func Albums(engine *dataengine.Engine) (map[string]bool, error) {
	if engine == nil {
		return nil, dataengine.ErrInvalidRequest
	}
	prefix := []byte("library/album-audio/")
	out := map[string]bool{}
	var after []byte
	for {
		var page dataengine.Page
		err := engine.View(func(v *dataengine.View) error {
			var scanErr error
			page, scanErr = v.Scan(prefix, after, 200)
			return scanErr
		})
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			var rec record
			if json.Unmarshal(item.Value, &rec) != nil || rec.State != "published" || !rec.Verified || !rec.LibraryPublished {
				continue
			}
			id := strings.TrimPrefix(string(item.Key), "library/album-audio/")
			// Per-track keys are "<root>/<track>"; legacy keys are just "<root>".
			if i := strings.Index(id, "/"); i >= 0 {
				id = id[:i]
			}
			if id == rec.RootID {
				out[id] = true
			}
		}
		if len(page.Next) == 0 {
			break
		}
		after = page.Next
	}
	return out, nil
}

// SystemDecoder returns a ffmpeg/ffprobe check when both tools are on PATH.
func SystemDecoder() Decoder {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		return nil
	}
	return func(ctx context.Context, path string) error {
		probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		probe := exec.CommandContext(probeCtx, ffprobe, "-v", "error", "-select_streams", "a", "-show_entries", "stream=codec_type", "-of", "csv=p=0", path)
		out, err := probe.Output()
		if err != nil || !bytes.Contains(out, []byte("audio")) {
			return errors.New("audio probe failed")
		}
		decodeCtx, cancelDecode := context.WithTimeout(ctx, 30*time.Second)
		defer cancelDecode()
		command := exec.CommandContext(decodeCtx, ffmpeg, "-nostdin", "-v", "error", "-xerror", "-err_detect", "explode", "-threads", "1", "-i", path, "-map", "0:a:0", "-f", "null", "-")
		stderr, err := command.CombinedOutput()
		if err != nil || strings.TrimSpace(string(stderr)) != "" {
			return errors.New("audio decode failed")
		}
		return nil
	}
}

func replay(rec record, source string) (Result, error) {
	if rec.State != "published" || !rec.Verified || !rec.LibraryPublished {
		return Result{}, ErrConflict
	}
	if err := sameFile(rec.Path, rec.SHA256); err != nil {
		return Result{}, err
	}
	preserved := sameFile(source, rec.SHA256) == nil
	return resultFrom(rec, preserved, true), nil
}

// replayMissing returns the stored receipt when the source file is gone but
// the library file still hashes to the stored digest. The release is
// committed; the missing source is reported as not preserved, never as a
// failed release. It returns ok=false when there is no committed release to
// return, so the caller reports the missing file as usual.
func replayMissing(engine *dataengine.Engine, requestID string) (Result, bool, error) {
	rec, found, err := readRelease(engine, requestID)
	if err != nil || !found {
		return Result{}, false, err
	}
	if rec.State != "published" || !rec.Verified || !rec.LibraryPublished {
		return Result{}, false, nil
	}
	if err := sameFile(rec.Path, rec.SHA256); err != nil {
		return Result{}, false, nil
	}
	return resultFrom(rec, false, true), true, nil
}

func resultFrom(rec record, preserved, replayed bool) Result {
	return Result{
		RequestID: rec.RequestID, RootID: rec.RootID, Format: rec.Format, DecodeState: rec.DecodeState,
		SHA256: rec.SHA256, Origin: rec.Origin, SourceName: rec.SourceName,
		Verified: true, LibraryPublished: true,
		// Downloaded stays false: a verified local import is not a download.
		SourcePreserved: preserved, Replayed: replayed,
	}
}

func classify(name string, body []byte) (string, string) {
	if len(body) == 0 || len(body) > maxAudioBytes {
		return "", "audio_rejected"
	}
	if torrent(name, body) {
		return "", "torrent_rejected"
	}
	if bytes.Contains(body, []byte("synthetic-fixture")) || bytes.Contains(body, []byte("musicops.fixture")) {
		return "", "fixture_rejected"
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".wav":
		if err := verifyWAV(body); err != nil {
			return "", "audio_rejected"
		}
		return "wav", ""
	case ".flac":
		if err := verifyFLAC(body); err != nil {
			return "", "audio_rejected"
		}
		return "flac", ""
	default:
		return "", "audio_rejected"
	}
}

func torrent(name string, body []byte) bool {
	if strings.EqualFold(filepath.Ext(name), ".torrent") {
		return true
	}
	return len(body) > 0 && body[0] == 'd' && bytes.Contains(body, []byte("4:info"))
}

func verifyWAV(body []byte) error {
	if len(body) < 44 || string(body[0:4]) != "RIFF" || string(body[8:12]) != "WAVE" {
		return ErrRejected
	}
	offset := 12
	var sawFmt, sawData bool
	var channels, bits uint16
	var rate uint32
	for offset+8 <= len(body) {
		id := string(body[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(body[offset+4 : offset+8]))
		offset += 8
		if size < 0 || offset+size > len(body) {
			return ErrRejected
		}
		payload := body[offset : offset+size]
		switch id {
		case "fmt ":
			if len(payload) < 16 {
				return ErrRejected
			}
			format := binary.LittleEndian.Uint16(payload[0:2])
			channels = binary.LittleEndian.Uint16(payload[2:4])
			rate = binary.LittleEndian.Uint32(payload[4:8])
			bits = binary.LittleEndian.Uint16(payload[14:16])
			if format != 1 {
				return ErrRejected
			}
			sawFmt = true
		case "data":
			if size == 0 {
				return ErrRejected
			}
			sawData = true
			if sawFmt {
				if channels < 1 || channels > 2 || rate == 0 || rate > 384000 {
					return ErrRejected
				}
				if bits != 8 && bits != 16 && bits != 24 && bits != 32 {
					return ErrRejected
				}
				block := int(channels) * int(bits) / 8
				if block == 0 || size%block != 0 {
					return ErrRejected
				}
			}
		}
		offset += size
		if size%2 == 1 {
			offset++
		}
	}
	if !sawFmt || !sawData {
		return ErrRejected
	}
	return nil
}

func verifyFLAC(body []byte) error {
	if len(body) < 42 || string(body[0:4]) != "fLaC" {
		return ErrRejected
	}
	kind := body[4] & 0x7f
	size := int(body[5])<<16 | int(body[6])<<8 | int(body[7])
	if kind != 0 || size < 34 || 8+size > len(body) {
		return ErrRejected
	}
	info := body[8 : 8+size]
	rate := uint32(info[10])<<12 | uint32(info[11])<<4 | uint32(info[12])>>4
	if rate == 0 || rate > 384000 {
		return ErrRejected
	}
	frame := 8 + size
	if frame+1 >= len(body) || body[frame] != 0xFF || body[frame+1]&0xFE != 0xF8 {
		return ErrRejected
	}
	return nil
}

func locate(input Request) (string, string, string, error) {
	if filepath.Base(input.SourceName) != input.SourceName || input.SourceName == "" || input.SourceName == "." || strings.ContainsAny(input.SourceName, `/\`) {
		return "", "", "", dataengine.ErrInvalidRequest
	}
	incoming, err := filepath.Abs(input.Incoming)
	if err != nil {
		return "", "", "", dataengine.ErrInvalidRequest
	}
	library, err := filepath.Abs(input.Library)
	if err != nil {
		return "", "", "", dataengine.ErrInvalidRequest
	}
	if incoming == library || inside(incoming, library) || inside(library, incoming) {
		return "", "", "", dataengine.ErrInvalidRequest
	}
	source := filepath.Join(incoming, input.SourceName)
	if !inside(incoming, source) {
		return "", "", "", dataengine.ErrInvalidRequest
	}
	info, err := os.Lstat(source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", "", ErrMissing
		}
		return "", "", "", err
	}
	if !info.Mode().IsRegular() {
		return "", "", "", ErrRejected
	}
	return incoming, library, source, nil
}

func inside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func writeNew(path string, body []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(body); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func sameFile(path, digest string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != digest {
		return ErrConflict
	}
	return nil
}

func cleanNames(values ...string) (string, string, string, error) {
	out := make([]string, 0, 3)
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\`) || strings.ContainsRune(value, 0) || !utf8.ValidString(value) || len(value) > 80 {
			return "", "", "", dataengine.ErrInvalidRequest
		}
		out = append(out, value)
	}
	return out[0], out[1], out[2], nil
}

func trackName(track int, title, format string) string {
	return fmt.Sprintf("%02d %s.%s", track, title, format)
}

func validID(id string) bool {
	if len(id) < 1 || len(id) > 200 {
		return false
	}
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == ':' {
			continue
		}
		return false
	}
	return true
}

func releaseKey(id string) []byte { return []byte("library/audio/" + id) }

// albumKey is per (root, track): one album root holds one record per track.
func albumKey(id string, track int) []byte {
	return []byte("library/album-audio/" + id + "/" + strconv.Itoa(track))
}

func readRelease(engine *dataengine.Engine, id string) (record, bool, error) {
	var rec record
	var found bool
	err := engine.View(func(v *dataengine.View) error {
		raw, ok, err := v.Get(releaseKey(id))
		if err != nil || !ok {
			return err
		}
		found = true
		if json.Unmarshal(raw, &rec) != nil {
			return ErrConflict
		}
		return nil
	})
	return rec, found, err
}
