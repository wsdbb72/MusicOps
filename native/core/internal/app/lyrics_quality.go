package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/artistnorm"
)

const defaultLyricsFallbackURL = "https://lrclib.net/api/search"

var maskedLyricsPattern = regexp.MustCompile(`(?i)(?:\*{2,}|[[:alpha:]]\*+[[:alpha:]]*)`)

// hasMaskedLyrics rejects common catalogue placeholders such as "****",
// "f***", "p*ssy" and "s**t". A single decorative asterisk is not enough to
// discard otherwise complete lyrics.
func hasMaskedLyrics(text string) bool {
	return maskedLyricsPattern.MatchString(text)
}

func lyricsTextUsable(text string) bool {
	return strings.TrimSpace(text) != "" && !hasMaskedLyrics(text)
}

// lyricsNeedsWrite treats an embedded or sidecar placeholder as absent. The
// scraped replacement must repair both copies rather than preserving a user's
// existing censored text merely because the file is non-empty.
func lyricsNeedsWrite(audioPath string, embedded []string) bool {
	embeddedClean := false
	for _, value := range embedded {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if !lyricsTextUsable(value) {
			return true
		}
		embeddedClean = true
	}
	// FLAC is the library's writable embedded-lyrics format. Full scraping
	// must keep both the player-visible tag and the portable sidecar in sync;
	// a clean sidecar alone does not repair a censored or absent embedded tag.
	if strings.EqualFold(filepath.Ext(audioPath), ".flac") && !embeddedClean {
		return true
	}
	return !lyricsSidecarUsable(audioPath)
}

// lyricsSidecarUsable reads only the standard sidecar first, then the legacy
// spelling. A censored sidecar is treated as missing so maintenance can repair
// it instead of preserving the placeholder forever.
func lyricsSidecarUsable(audioPath string) bool {
	usable := false
	for _, path := range []string{lyricsSidecarPath(audioPath), audioPath + ".lrc"} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 4<<20 {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil || !lyricsTextUsable(string(body)) {
			return false
		}
		usable = true
	}
	return usable
}

// cleanFetchedLyrics accepts the configured LrcApi result only when it has no
// masked words. For a censored result it asks LRCLIB for the exact track and
// requires an exact normalized title plus artist match. If no clean timed text
// exists, it returns empty so a placeholder is never newly written.
func (a *App) cleanFetchedLyrics(ctx context.Context, artist, album, title, lyrics string) string {
	lyrics = strings.TrimSpace(lyrics)
	if lyricsTextUsable(lyrics) {
		return lyrics
	}
	fallback, err := a.fetchLRCLIBCleanLyrics(ctx, artist, album, title)
	if err != nil || !lyricsTextUsable(fallback) {
		return ""
	}
	return fallback
}

type lrclibSearchResult struct {
	TrackName    string  `json:"trackName"`
	ArtistName   string  `json:"artistName"`
	AlbumName    string  `json:"albumName"`
	Duration     float64 `json:"duration"`
	Instrumental bool    `json:"instrumental"`
	SyncedLyrics string  `json:"syncedLyrics"`
	PlainLyrics  string  `json:"plainLyrics"`
}

func (a *App) fetchLRCLIBCleanLyrics(ctx context.Context, artist, album, title string) (string, error) {
	if strings.TrimSpace(artist) == "" || strings.TrimSpace(title) == "" {
		return "", errors.New("LRCLIB lookup requires artist and title")
	}
	base := strings.TrimSpace(a.lyricsFallbackBase)
	if base == "" {
		base = defaultLyricsFallbackURL
	}
	endpoint, err := url.Parse(base)
	if err != nil || endpoint.User != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return "", errors.New("invalid lyrics fallback URL")
	}
	query := endpoint.Query()
	query.Set("track_name", strings.TrimSpace(title))
	query.Set("artist_name", strings.TrimSpace(artist))
	if strings.TrimSpace(album) != "" {
		query.Set("album_name", strings.TrimSpace(album))
	}
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "MusicOps (+https://github.com/wsdbb72/MusicOps)")
	request.Header.Set("Accept", "application/json")
	client := a.lyricsHTTPClient
	if client == nil {
		client = &http.Client{Timeout: 12 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("LRCLIB returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20+1))
	if err != nil {
		return "", err
	}
	if len(body) == 0 || len(body) > 4<<20 {
		return "", errors.New("LRCLIB response is empty or too large")
	}
	var results []lrclibSearchResult
	if err := json.Unmarshal(body, &results); err != nil {
		return "", fmt.Errorf("decode LRCLIB response: %w", err)
	}
	wantTitle := foldTagText(title)
	wantAlbum := foldTagText(album)
	var fallback string
	for _, result := range results {
		if result.Instrumental || foldTagText(result.TrackName) != wantTitle || !artistCreditEquivalent(result.ArtistName, artist) {
			continue
		}
		text := strings.TrimSpace(result.SyncedLyrics)
		if !lyricsTextUsable(text) {
			continue
		}
		if wantAlbum != "" && foldTagText(result.AlbumName) == wantAlbum {
			return text, nil
		}
		if fallback == "" {
			fallback = text
		}
	}
	return fallback, nil
}

func artistCreditEquivalent(left, right string) bool {
	leftParts := flattenPlaylistArtists([]string{left})
	rightParts := flattenPlaylistArtists([]string{right})
	if len(leftParts) == 0 || len(rightParts) == 0 {
		return false
	}
	leftKeys := make(map[string]bool, len(leftParts))
	for _, part := range leftParts {
		if key := artistnorm.Key(part); key != "" {
			leftKeys[key] = true
		}
	}
	rightKeys := make(map[string]bool, len(rightParts))
	for _, part := range rightParts {
		if key := artistnorm.Key(part); key != "" {
			rightKeys[key] = true
		}
	}
	if len(leftKeys) == 0 || len(leftKeys) != len(rightKeys) {
		return false
	}
	for key := range leftKeys {
		if !rightKeys[key] {
			return false
		}
	}
	return true
}
