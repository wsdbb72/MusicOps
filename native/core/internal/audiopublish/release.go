// Package audiopublish copies one controlled audio file into an album folder.
// The source stays in place. This is not a site download and not a production verification.
package audiopublish

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

var ErrConflict = errors.New("library release conflict")

// Request publishes one WAV that the caller already obtained from a controlled source.
type Request struct {
	RequestID string
	Artist    string
	Album     string
	Title     string
	Track     int
	Library   string
	SourceURL string
	Now       time.Time
}

// Result is the organized file. Downloaded stays false because the bytes were not taken from a tracker or streaming service.
type Result struct {
	RequestID          string `json:"request_id"`
	Path               string `json:"path"`
	SHA256             string `json:"sha256"`
	SourcePreserved    bool   `json:"source_preserved"`
	Organized          bool   `json:"organized"`
	Origin             string `json:"origin"`
	Replayed           bool   `json:"replayed"`
	Downloaded         bool   `json:"downloaded"`
	ProductionVerified bool   `json:"production_verified"`
}

type record struct {
	RequestID string `json:"request_id"`
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	Origin    string `json:"origin"`
	State     string `json:"state"`
}

// Publish reads a loopback WAV, checks the header, and renames a synced copy inside the library.
func Publish(ctx context.Context, engine *dataengine.Engine, input Request) (Result, error) {
	if engine == nil || !validID(input.RequestID) || input.Track < 1 || input.Track > 999 || input.Now.IsZero() {
		return Result{}, dataengine.ErrInvalidRequest
	}
	artist, album, title, err := cleanNames(input.Artist, input.Album, input.Title)
	if err != nil {
		return Result{}, err
	}
	library, err := filepath.Abs(input.Library)
	if err != nil {
		return Result{}, dataengine.ErrInvalidRequest
	}
	endpoint, err := loopback(input.SourceURL)
	if err != nil {
		return Result{}, err
	}
	if existing, found, err := read(engine, input.RequestID); err != nil {
		return Result{}, err
	} else if found && existing.State == "published" {
		if err := sameFile(existing.Path, existing.SHA256); err != nil {
			return Result{}, err
		}
		return resultFrom(existing, true), nil
	}
	body, err := fetch(ctx, endpoint)
	if err != nil {
		return Result{}, err
	}
	if err := validateWAV(body); err != nil {
		return Result{}, err
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	folder := filepath.Join(library, artist, album)
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return Result{}, err
	}
	name := strconv.Itoa(input.Track) + " " + title + ".wav"
	if input.Track < 100 {
		name = "0" + name
	}
	dest := filepath.Join(folder, name)
	temp := filepath.Join(folder, ".musicops-"+input.RequestID+".publish")
	if err := writeNew(temp, body); err != nil {
		os.Remove(temp)
		return Result{}, err
	}
	select {
	case <-ctx.Done():
		os.Remove(temp)
		return Result{}, ctx.Err()
	default:
	}
	if _, err := os.Stat(dest); err == nil {
		os.Remove(temp)
		return Result{}, ErrConflict
	}
	if err := os.Rename(temp, dest); err != nil {
		os.Remove(temp)
		return Result{}, err
	}
	if err := sameFile(dest, digest); err != nil {
		return Result{}, err
	}
	rec := record{RequestID: input.RequestID, Path: dest, SHA256: digest, Origin: "controlled_audio", State: "published"}
	encoded, err := json.Marshal(rec)
	if err != nil {
		return Result{}, err
	}
	pathKey := []byte("library/path/" + hex.EncodeToString(sha256.New().Sum([]byte(dest))[:]))
	// The line above hashes an empty digest. Use the path bytes explicitly.
	pathSum := sha256.Sum256([]byte(dest))
	pathKey = []byte("library/path/" + hex.EncodeToString(pathSum[:]))
	ok, err := engine.CompareAndSwap(
		[]dataengine.Condition{{Key: []byte("library/release/" + input.RequestID), Absent: true}, {Key: pathKey, Absent: true}},
		[]dataengine.Mutation{{Key: []byte("library/release/" + input.RequestID), Value: encoded}, {Key: pathKey, Value: []byte(input.RequestID)}},
	)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Publish(ctx, engine, input)
	}
	return resultFrom(rec, false), nil
}

func fetch(ctx context.Context, endpoint *url.URL) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20+1))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK || len(body) == 0 || len(body) > 8<<20 {
		return nil, errors.New("controlled audio response was rejected")
	}
	return body, nil
}

func validateWAV(body []byte) error {
	if len(body) < 44 || string(body[0:4]) != "RIFF" || string(body[8:12]) != "WAVE" {
		return errors.New("audio is not a WAV file")
	}
	format := binary.LittleEndian.Uint16(body[20:22])
	channels := binary.LittleEndian.Uint16(body[22:24])
	rate := binary.LittleEndian.Uint32(body[24:28])
	if format != 1 || (channels != 1 && channels != 2) || rate == 0 {
		return errors.New("WAV layout is not supported")
	}
	if !bytesContains(body[12:], []byte("data")) {
		return errors.New("WAV data chunk is missing")
	}
	return nil
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

func loopback(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" {
		return nil, dataengine.ErrInvalidRequest
	}
	return parsed, nil
}

func cleanNames(artist, album, title string) (string, string, string, error) {
	out := []string{}
	for _, value := range []string{artist, album, title} {
		value = strings.TrimSpace(value)
		if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\`) || strings.ContainsRune(value, 0) || !utf8.ValidString(value) || len(value) > 80 {
			return "", "", "", dataengine.ErrInvalidRequest
		}
		out = append(out, value)
	}
	return out[0], out[1], out[2], nil
}

func validID(id string) bool {
	if len(id) < 1 || len(id) > 80 {
		return false
	}
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func read(engine *dataengine.Engine, id string) (record, bool, error) {
	var rec record
	var found bool
	err := engine.View(func(v *dataengine.View) error {
		raw, ok, err := v.Get([]byte("library/release/" + id))
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

func resultFrom(rec record, replayed bool) Result {
	return Result{RequestID: rec.RequestID, Path: rec.Path, SHA256: rec.SHA256, SourcePreserved: true, Organized: true, Origin: rec.Origin, Replayed: replayed}
}

func bytesContains(haystack, needle []byte) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle []byte) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
