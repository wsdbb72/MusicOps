package app

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/downloadpolicy"
)

const (
	jooxTrackAPIBase = "https://cache.api.joox.com/openjoox2/v1/track"
	jooxSecretSalt   = "Jo0x@t3Nc3nT"
	jooxUserAgent    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/149.0.0.0 Safari/537.36"
)

type jooxTrackMetadata struct {
	ID           string   `json:"id"`
	PlayURLList  []string `json:"play_url_list"`
	RefrainURL   string   `json:"refrain_url"`
	PlayDuration float64  `json:"play_duration"`
}

// fetchJOOXAudio is the official-playback fallback for go-music-dl. The
// bundled engine can return 404 for a JOOX card even when the public track
// response still carries playable MP3/M4A URLs. Nothing is guessed: the exact
// card id is resolved through JOOX's public track endpoint, and only URLs on
// joox.com are followed.
func (a *App) fetchJOOXAudio(ctx context.Context, id string) ([]byte, string, error) {
	base := a.jooxAPIBaseURL
	if base == "" {
		base = jooxTrackAPIBase
	}
	client := a.jooxHTTPClient
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Minute}
	}
	metadataURL, err := jooxTrackURL(base, id)
	if err != nil {
		return nil, downloadpolicy.ClassNoResult, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return nil, downloadpolicy.ClassNoResult, err
	}
	request.Header.Set("User-Agent", jooxUserAgent)
	request.Header.Set("Accept", "application/json, text/plain, */*")
	request.Header.Set("Origin", "https://www.joox.com")
	request.Header.Set("Referer", "https://www.joox.com/")
	response, err := client.Do(request)
	if err != nil {
		return nil, downloadpolicy.ClassNetwork, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, jooxHTTPClass(response.StatusCode), fmt.Errorf("JOOX track metadata HTTP %d", response.StatusCode)
	}
	var metadata jooxTrackMetadata
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&metadata); err != nil {
		return nil, downloadpolicy.ClassNoResult, err
	}
	if metadata.ID != "" && metadata.ID != id {
		return nil, downloadpolicy.ClassNoResult, errors.New("JOOX returned a different track")
	}
	audioURLs := append([]string(nil), metadata.PlayURLList...)
	if strings.TrimSpace(metadata.RefrainURL) != "" {
		audioURLs = append(audioURLs, metadata.RefrainURL)
	}
	class := downloadpolicy.ClassNoResult
	for _, raw := range audioURLs {
		raw = html.UnescapeString(strings.TrimSpace(raw))
		if raw == "" {
			continue
		}
		parsed, parseErr := url.Parse(raw)
		if parseErr != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || !jooxAudioHost(parsed.Hostname()) {
			continue
		}
		audioRequest, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
		if requestErr != nil {
			continue
		}
		audioRequest.Header.Set("User-Agent", jooxUserAgent)
		audioRequest.Header.Set("Referer", "https://www.joox.com/")
		audioResponse, requestErr := client.Do(audioRequest)
		if requestErr != nil {
			class = downloadpolicy.ClassNetwork
			continue
		}
		if audioResponse.StatusCode != http.StatusOK {
			if next := jooxHTTPClass(audioResponse.StatusCode); next != "" {
				class = preferStreamFailure(class, next)
			}
			audioResponse.Body.Close()
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(audioResponse.Body, int64(audiorelease.MaxStagedAudioBytes)+1))
		audioResponse.Body.Close()
		if readErr != nil {
			class = downloadpolicy.ClassNetwork
			continue
		}
		if len(body) == 0 || len(body) > audiorelease.MaxStagedAudioBytes || !isKnownAudio(body) {
			class = downloadpolicy.ClassNoResult
			continue
		}
		return body, "", nil
	}
	return nil, class, errors.New("JOOX returned no playable audio")
}

func jooxTrackURL(base, id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 4096 || strings.ContainsRune(id, 0) {
		return "", errors.New("invalid JOOX track id")
	}
	endpoint, err := url.Parse(strings.TrimSpace(base))
	if err != nil || endpoint.User != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return "", errors.New("invalid JOOX API base")
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/" + strings.ReplaceAll(id, "/", "_")
	// The endpoint authenticates the ordered query as well as the signed
	// parameters. url.Values.Encode sorts keys and is rejected with HTTP 401.
	endpoint.RawQuery = "country=hk&lang=zh_TW&lyric=1&fs=1&im=0&uid=142420656&usk=2a5d97d05dc8fe238150184eaf3519ad&secret=" + jooxTrackSecret(id)
	return endpoint.String(), nil
}

func jooxTrackSecret(id string) string {
	query := "country=hk&lang=zh_TW&lyric=1&fs=1&im=0&uid=142420656&usk=2a5d97d05dc8fe238150184eaf3519ad&id=" + url.QueryEscape(id)
	sum := md5.Sum([]byte(jooxSecretSalt + query))
	return hex.EncodeToString(sum[:])
}

func jooxAudioHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "joox.com" || strings.HasSuffix(host, ".joox.com")
}

func jooxHTTPClass(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return downloadpolicy.ClassAuth
	case status == http.StatusTooManyRequests:
		return downloadpolicy.ClassRateLimited
	case status >= 500:
		return downloadpolicy.ClassNetwork
	default:
		return downloadpolicy.ClassNoResult
	}
}
