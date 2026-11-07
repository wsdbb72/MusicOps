// Package torrentfetch retrieves one allowlisted torrent file.
// A saved metainfo is not a downloaded album and is not a library publication.
package torrentfetch

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/trackersearch"
)

var positiveID = regexp.MustCompile(`^[1-9][0-9]*$`)

// Request is one torrent GET. UseToken adds the Gazelle free-token parameter and nothing else.
type Request struct {
	Site      string
	Resource  string
	Cookie    string
	Synthetic bool
	BaseURL   string
	UseToken  bool
	Directory string
	Timeout   time.Duration
}

// Receipt describes stored torrent bytes. Downloaded and library publication stay false.
type Receipt struct {
	Site               string `json:"site"`
	ResourceID         string `json:"resource_id"`
	InfoHash           string `json:"infohash"`
	MetaSHA256         string `json:"metainfo_sha256"`
	Size               int    `json:"size"`
	Path               string `json:"path"`
	UseToken           bool   `json:"use_token"`
	Downloaded         bool   `json:"downloaded"`
	LibraryPublished   bool   `json:"library_published"`
	ProductionVerified bool   `json:"production_verified"`
	Synthetic          bool   `json:"synthetic"`
}

// Fetch stores a parsed torrent. It does not follow redirects and does not mark an album downloaded.
func Fetch(ctx context.Context, req Request) (Receipt, error) {
	if !positiveID.MatchString(req.Resource) || strings.ContainsAny(req.Cookie, "\r\n") || strings.TrimSpace(req.Cookie) == "" {
		return Receipt{}, dataengine.ErrInvalidRequest
	}
	endpoint, official, err := downloadURL(req)
	if err != nil {
		return Receipt{}, err
	}
	if !allowedDownload(req.Site, endpoint) {
		return Receipt{}, dataengine.ErrInvalidRequest
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return Receipt{}, err
	}
	httpReq.Header.Set("Cookie", req.Cookie)
	httpReq.Header.Set("Accept", "application/x-bittorrent")
	if req.Synthetic {
		httpReq.Header.Set("X-MusicOps-Site", req.Site)
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(httpReq)
	if err != nil {
		return Receipt{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20+1))
	if err != nil {
		return Receipt{}, err
	}
	if response.StatusCode != http.StatusOK || len(body) == 0 || len(body) > 8<<20 || looksLikeHTML(body) {
		return Receipt{}, errors.New("torrent response was not a metainfo file")
	}
	infoHash, err := infoHashV1(body)
	if err != nil {
		return Receipt{}, err
	}
	sum := sha256.Sum256(body)
	path, err := writeAtomic(req.Directory, hex.EncodeToString(sum[:])+".torrent", body)
	if err != nil {
		return Receipt{}, err
	}
	return Receipt{
		Site: req.Site, ResourceID: req.Resource, InfoHash: infoHash, MetaSHA256: hex.EncodeToString(sum[:]),
		Size: len(body), Path: path, UseToken: req.UseToken, Synthetic: req.Synthetic && !official,
	}, nil
}

func downloadURL(req Request) (*url.URL, bool, error) {
	host, official := trackersearch.Hosts[req.Site]
	if req.Synthetic {
		parsed, err := url.Parse(req.BaseURL)
		if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" {
			return nil, false, dataengine.ErrInvalidRequest
		}
		parsed.Path = ""
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return withDownloadQuery(parsed, req), false, nil
	}
	if !official || host == "" {
		return nil, false, dataengine.ErrInvalidRequest
	}
	endpoint, err := url.Parse("https://" + host + "/")
	if err != nil {
		return nil, false, err
	}
	return withDownloadQuery(endpoint, req), true, nil
}

func withDownloadQuery(endpoint *url.URL, req Request) *url.URL {
	values := url.Values{}
	if req.Site == "open.cd" {
		endpoint.Path = "/download.php"
		values.Set("id", req.Resource)
	} else {
		endpoint.Path = "/torrents.php"
		values.Set("action", "download")
		values.Set("id", req.Resource)
		if req.UseToken {
			values.Set("usetoken", "1")
		}
	}
	endpoint.RawQuery = values.Encode()
	return endpoint
}

func allowedDownload(site string, endpoint *url.URL) bool {
	values := endpoint.Query()
	switch site {
	case "open.cd":
		return endpoint.Path == "/download.php" && values.Get("id") != "" && len(values) == 1
	case "dicmusic", "orpheus", "redacted":
		if endpoint.Path != "/torrents.php" || values.Get("action") != "download" || values.Get("id") == "" {
			return false
		}
		switch len(values) {
		case 2:
			return values.Get("usetoken") == ""
		case 3:
			return values.Get("usetoken") == "1"
		default:
			return false
		}
	default:
		return false
	}
}

func looksLikeHTML(body []byte) bool {
	trimmed := strings.TrimSpace(strings.ToLower(string(body[:min(64, len(body))])))
	return strings.HasPrefix(trimmed, "<html") || strings.HasPrefix(trimmed, "<!doctype") || strings.HasPrefix(trimmed, "<form") || strings.HasPrefix(trimmed, "<body")
}

func infoHashV1(data []byte) (string, error) {
	if len(data) == 0 || data[0] != 'd' {
		return "", errors.New("metainfo is not a dictionary")
	}
	pos := 1
	var previous []byte
	var span [2]int
	found := false
	for pos < len(data) && data[pos] != 'e' {
		key, next, err := readBytes(data, pos)
		if err != nil {
			return "", err
		}
		if previous != nil && string(key) <= string(previous) {
			return "", errors.New("metainfo keys are not ordered")
		}
		previous = key
		end, err := skipOne(data, next)
		if err != nil {
			return "", err
		}
		if string(key) == "info" {
			span = [2]int{next, end}
			found = true
		}
		pos = end
	}
	if !found || pos >= len(data) || data[pos] != 'e' {
		return "", errors.New("metainfo has no info dictionary")
	}
	sum := sha1.Sum(data[span[0]:span[1]])
	return hex.EncodeToString(sum[:]), nil
}

func skipOne(data []byte, pos int) (int, error) {
	if pos >= len(data) {
		return 0, errors.New("truncated metainfo")
	}
	switch data[pos] {
	case 'i':
		end := strings.IndexByte(string(data[pos:]), 'e')
		if end < 0 {
			return 0, errors.New("truncated integer")
		}
		text := string(data[pos+1 : pos+end])
		if _, err := strconv.ParseInt(text, 10, 64); err != nil || len(text) > 1 && text[0] == '0' {
			return 0, errors.New("noncanonical integer")
		}
		return pos + end + 1, nil
	case 'l':
		return skipList(data, pos)
	case 'd':
		return skipDict(data, pos)
	default:
		return skipBytes(data, pos)
	}
}

func skipList(data []byte, pos int) (int, error) {
	pos++
	for pos < len(data) && data[pos] != 'e' {
		next, err := skipOne(data, pos)
		if err != nil {
			return 0, err
		}
		pos = next
	}
	if pos >= len(data) {
		return 0, errors.New("unterminated list")
	}
	return pos + 1, nil
}

func skipDict(data []byte, pos int) (int, error) {
	pos++
	var previous []byte
	for pos < len(data) && data[pos] != 'e' {
		key, next, err := readBytes(data, pos)
		if err != nil {
			return 0, err
		}
		if previous != nil && string(key) <= string(previous) {
			return 0, errors.New("unordered dictionary")
		}
		previous = key
		pos, err = skipOne(data, next)
		if err != nil {
			return 0, err
		}
	}
	if pos >= len(data) {
		return 0, errors.New("unterminated dictionary")
	}
	return pos + 1, nil
}

func skipBytes(data []byte, pos int) (int, error) {
	_, next, err := readBytes(data, pos)
	return next, err
}

func readBytes(data []byte, pos int) ([]byte, int, error) {
	colon := -1
	for i := pos; i < len(data) && i < pos+12; i++ {
		if data[i] == ':' {
			colon = i
			break
		}
	}
	if colon < 0 {
		return nil, 0, errors.New("bad bencode string")
	}
	length, err := strconv.Atoi(string(data[pos:colon]))
	if err != nil || length < 0 || (length > 0 && data[pos] == '0') || (length == 0 && colon != pos+1) {
		return nil, 0, errors.New("bad bencode length")
	}
	start := colon + 1
	end := start + length
	if end > len(data) {
		return nil, 0, errors.New("truncated bencode string")
	}
	return data[start:end], end, nil
}

func writeAtomic(dir, name string, body []byte) (string, error) {
	if dir == "" || strings.Contains(name, "/") || strings.Contains(name, `\`) {
		return "", dataengine.ErrInvalidRequest
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	target := filepath.Join(dir, name)
	temp, err := os.CreateTemp(dir, "torrent-*")
	if err != nil {
		return "", err
	}
	tempName := temp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tempName)
		}
	}()
	if _, err = temp.Write(body); err != nil {
		temp.Close()
		return "", err
	}
	if err = temp.Sync(); err != nil {
		temp.Close()
		return "", err
	}
	if err = temp.Close(); err != nil {
		return "", err
	}
	if err = os.Rename(tempName, target); err != nil {
		return "", err
	}
	cleanup = false
	if err = syncDir(dir); err != nil {
		return "", err
	}
	return target, nil
}

func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = file.Sync()
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}
