package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
)

// netease compilation rows often point at a shared "hot songs" sleeve. The
// selected NetEase cover remains the final fallback, while these public music
// catalogues provide a track-specific original release artwork when available.
// Every endpoint and image host is fixed; artwork URLs from search documents
// never grant access to arbitrary hosts.

type externalCoverCandidate struct {
	URL        string
	Title      string
	Artist     string
	DurationMS int64
}

func (a *App) appleOriginalCoverBytes(ctx context.Context, song *neteaseSong) ([]byte, error) {
	if song == nil || strings.TrimSpace(song.Name) == "" || len(song.Artists) == 0 {
		return nil, errors.New("缺少跨平台封面检索信息")
	}
	query := strings.Join(song.Artists, " ") + " " + song.Name
	endpoint := "https://itunes.apple.com/search?media=music&entity=song&limit=50&term=" + url.QueryEscape(query)
	var decoded struct {
		Results []struct {
			TrackName       string `json:"trackName"`
			ArtistName      string `json:"artistName"`
			TrackTimeMillis int64  `json:"trackTimeMillis"`
			ArtworkURL      string `json:"artworkUrl100"`
		} `json:"results"`
	}
	if err := metadataJSON(ctx, endpoint, &decoded); err != nil {
		return nil, err
	}
	candidates := make([]externalCoverCandidate, 0, len(decoded.Results))
	for _, item := range decoded.Results {
		artwork := strings.TrimSpace(item.ArtworkURL)
		artwork = strings.Replace(artwork, "100x100bb", "1200x1200bb", 1)
		candidates = append(candidates, externalCoverCandidate{URL: artwork, Title: item.TrackName, Artist: item.ArtistName, DurationMS: item.TrackTimeMillis})
	}
	return downloadBestCover(ctx, song, candidates)
}

func (a *App) deezerOriginalCoverBytes(ctx context.Context, song *neteaseSong) ([]byte, error) {
	if song == nil || strings.TrimSpace(song.Name) == "" || len(song.Artists) == 0 {
		return nil, errors.New("缺少跨平台封面检索信息")
	}
	query := strings.Join(song.Artists, " ") + " " + song.Name
	endpoint := "https://api.deezer.com/search?limit=50&q=" + url.QueryEscape(query)
	var decoded struct {
		Data []struct {
			Title    string `json:"title"`
			Duration int64  `json:"duration"`
			Artist   struct {
				Name string `json:"name"`
			} `json:"artist"`
			Album struct {
				CoverBig string `json:"cover_big"`
				CoverXL  string `json:"cover_xl"`
			} `json:"album"`
		} `json:"data"`
	}
	if err := metadataJSON(ctx, endpoint, &decoded); err != nil {
		return nil, err
	}
	candidates := make([]externalCoverCandidate, 0, len(decoded.Data))
	for _, item := range decoded.Data {
		cover := strings.TrimSpace(item.Album.CoverXL)
		if cover == "" {
			cover = strings.TrimSpace(item.Album.CoverBig)
		}
		candidates = append(candidates, externalCoverCandidate{URL: cover, Title: item.Title, Artist: item.Artist.Name, DurationMS: item.Duration * 1000})
	}
	return downloadBestCover(ctx, song, candidates)
}

func metadataJSON(ctx context.Context, endpoint string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "MusicOps/0.4 (+https://github.com/wsdbb72/MusicOps)")
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("封面目录返回失败")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, target)
}

func downloadBestCover(ctx context.Context, song *neteaseSong, candidates []externalCoverCandidate) ([]byte, error) {
	type scored struct {
		candidate externalCoverCandidate
		delta     int64
		artist    bool
	}
	matches := make([]scored, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.URL == "" || !coverTitleEquivalent(song.Name, candidate.Title) {
			continue
		}
		delta := int64(1 << 60)
		if song.DurationMS > 0 && candidate.DurationMS > 0 {
			delta = candidate.DurationMS - int64(song.DurationMS)
			if delta < 0 {
				delta = -delta
			}
		}
		artist := playlistArtistsOverlap(song.Artists, splitPlaylistArtists(candidate.Artist))
		if !artist || delta > 5000 {
			continue
		}
		matches = append(matches, scored{candidate: candidate, delta: delta, artist: artist})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].artist != matches[j].artist {
			return matches[i].artist
		}
		return matches[i].delta < matches[j].delta
	})
	for _, match := range matches {
		cover, err := downloadExternalCover(ctx, match.candidate.URL)
		if err == nil {
			return cover, nil
		}
	}
	return nil, errors.New("跨平台目录没有唯一匹配封面")
}

func coverTitleEquivalent(want, got string) bool {
	return coverTitleKey(want) != "" && coverTitleKey(want) == coverTitleKey(got)
}

func coverTitleKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	for _, r := range value {
		if r >= 0xff01 && r <= 0xff5e {
			r -= 0xfee0
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			out.WriteRune(unicode.ToLower(r))
		}
	}
	return out.String()
}

func downloadExternalCover(ctx context.Context, raw string) ([]byte, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil {
		return nil, errors.New("封面地址不安全")
	}
	host := strings.ToLower(parsed.Hostname())
	allowed := host == "mzstatic.com" || strings.HasSuffix(host, ".mzstatic.com") ||
		host == "dzcdn.net" || strings.HasSuffix(host, ".dzcdn.net")
	if !allowed {
		return nil, errors.New("封面域名不在允许列表")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/126.0 Safari/537.36")
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("封面下载失败")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (10<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 10<<20 || !imageBytes(body) {
		return nil, errors.New("封面不完整")
	}
	return body, nil
}
