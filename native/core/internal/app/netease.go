package app

// Native NetEase streaming: search the catalogue, resolve one song's media
// URL and keep it only when the service hands back lossless FLAC. This is the
// built-in replacement for asking an external engine for the netease source;
// the other platforms stay engine-only, and nothing here pretends a lossy
// file is an album.
//
// The account cookie is required for anything above 320 kbps: an anonymous
// request answers with an mp3 even when lossless is asked for, so the native
// path reports "只有有损音频" instead of downloading a lossy file into a
// library that is FLAC-only by design.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/wsdbb72/MusicOps/native/core/internal/cookies"
	"github.com/wsdbb72/MusicOps/native/core/internal/downloadpolicy"
	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
)

const (
	// neteaseHost is the site the cookie is bound to.
	neteaseHost = "music.163.com"
	// neteaseMediaMaxBytes bounds one download. A long lossless track is
	// well under this; anything larger is not the file that was asked for.
	neteaseMediaMaxBytes = 300 << 20
)

// neteaseStreamingRef is the Vault reference for the NetEase cookie. The
// playlist fetcher reads the same value through playlist-sources, so one
// cookie serves both reading a playlist and falling back to one track.
const neteaseStreamingRef = "vault://streaming/netease"

var (
	errNeteaseUnexpectedHTML = errors.New("netease: 返回了网页而不是 JSON")
	errNeteaseUnexpectedJSON = errors.New("netease: 返回的 JSON 为空、截断或结构不符合预期")
)

func neteaseContractError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errNeteaseUnexpectedJSON, fmt.Sprintf(format, args...))
}

// neteaseBases lets a test point the client at a local server. Production
// leaves it nil and uses the official origin.
func (a *App) neteaseBase() string {
	if a != nil && a.neteaseBases != nil {
		if base := strings.TrimRight(a.neteaseBases["api"], "/"); base != "" {
			return base
		}
	}
	return "https://" + neteaseHost
}

// neteaseCookie returns the saved Cookie header for NetEase, or "".
func (a *App) neteaseCookie() string {
	if a == nil || a.vault == nil {
		return ""
	}
	raw, found, err := a.vault.Get(neteaseStreamingRef, neteaseHost)
	if err != nil || !found || raw == "" {
		return ""
	}
	parsed, err := cookies.Unmarshal(raw)
	if err != nil {
		return ""
	}
	header, err := cookies.Header(parsed, "/", a.now())
	if err != nil {
		return ""
	}
	return header
}

// neteaseSaved reports whether a cookie is stored, without reading it.
func (a *App) neteaseSaved() bool {
	_, found, err := a.vault.Get(neteaseStreamingRef, neteaseHost)
	return err == nil && found
}

type neteaseHTTPError struct {
	operation string
	status    int
}

func (e *neteaseHTTPError) Error() string {
	return "netease: " + e.operation + "返回 " + strconv.Itoa(e.status)
}

func classifyNeteaseFailure(err error) string {
	if err == nil {
		return downloadpolicy.ClassNoResult
	}
	// Cancellation is not a terminal no-result. acquireFallback rechecks the
	// context after every source and propagates it before committing a skip.
	if errors.Is(err, context.Canceled) {
		return downloadpolicy.ClassNetwork
	}
	if errors.Is(err, errNeteaseUnexpectedHTML) || errors.Is(err, errNeteaseUnexpectedJSON) {
		return downloadpolicy.ClassNetwork
	}
	var syntaxError *json.SyntaxError
	var typeError *json.UnmarshalTypeError
	if errors.As(err, &syntaxError) || errors.As(err, &typeError) {
		return downloadpolicy.ClassNetwork
	}
	var httpErr *neteaseHTTPError
	if errors.As(err, &httpErr) {
		if httpErr.status == http.StatusTooManyRequests || httpErr.status == http.StatusMethodNotAllowed {
			return downloadpolicy.ClassRateLimited
		}
		if httpErr.status == http.StatusUnauthorized || httpErr.status == http.StatusForbidden {
			return downloadpolicy.ClassAuth
		}
		if httpErr.status == http.StatusRequestTimeout || httpErr.status >= 500 {
			return downloadpolicy.ClassNetwork
		}
		return downloadpolicy.ClassNoResult
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return downloadpolicy.ClassNetwork
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return downloadpolicy.ClassNetwork
	}
	return downloadpolicy.ClassNoResult
}

// neteaseGet performs one read against the API with the headers the web
// client sends. Without them the service answers HTML instead of JSON.
func (a *App) neteaseGet(ctx context.Context, endpoint, cookie string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Referer", "https://"+neteaseHost+"/")
	request.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	if cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, &neteaseHTTPError{operation: "服务", status: response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, errNeteaseUnexpectedJSON
	}
	if body[0] == '<' {
		return nil, errNeteaseUnexpectedHTML
	}
	if !json.Valid(body) {
		return nil, errNeteaseUnexpectedJSON
	}
	if body[0] != '{' {
		return nil, neteaseContractError("顶层必须是 JSON 对象")
	}
	return body, nil
}

// neteaseSong is one search hit.
type neteaseSong struct {
	ID              int64
	Name            string
	Artists         []string
	Album           string
	DurationMS      int
	AlbumID         int64
	CoverURL        string
	CatalogVerified bool
}

// neteaseSearchItem is the search shape shared by direct-song lookup and
// original-release artwork lookup.
type neteaseSearchItem struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Artists []struct {
		Name string `json:"name"`
	} `json:"artists"`
	Album struct {
		Name   string `json:"name"`
		ID     int64  `json:"id"`
		Size   int    `json:"size"`
		PicURL string `json:"picUrl"`
	} `json:"album"`
	Duration int `json:"duration"`
}

// requiredNeteaseField separates a missing/null JSON member from a valid empty
// value. A malformed contract is retryable; an explicit empty songs array is a
// genuine no-result answer.
func requiredNeteaseField(raw []byte, field, operation string) (json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, neteaseContractError("%s响应不是 JSON 对象：%v", operation, err)
	}
	value, ok := object[field]
	if !ok {
		return nil, neteaseContractError("%s响应缺少 %s 字段", operation, field)
	}
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil, neteaseContractError("%s响应的 %s 字段为 null", operation, field)
	}
	return value, nil
}

func decodeNeteaseSearch(body []byte) (int, []neteaseSearchItem, error) {
	var envelope struct {
		Code *int `json:"code"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return 0, nil, neteaseContractError("搜索响应结构异常：%v", err)
	}
	code := 0
	if envelope.Code != nil {
		code = *envelope.Code
	}
	if code != 0 && code != 200 {
		// Surface NetEase's embedded application code (notably 405 rate limiting)
		// before looking for fields that such a response omits.
		return code, nil, &neteaseHTTPError{operation: "搜索", status: code}
	}
	resultRaw, err := requiredNeteaseField(body, "result", "搜索")
	if err != nil {
		return 0, nil, err
	}
	songsRaw, err := requiredNeteaseField(resultRaw, "songs", "搜索")
	if err != nil {
		return 0, nil, err
	}
	var songs []neteaseSearchItem
	if err := json.Unmarshal(songsRaw, &songs); err != nil {
		return 0, nil, neteaseContractError("搜索 songs 结构异常：%v", err)
	}
	return code, songs, nil
}

// neteaseSearchSong finds the best match for a query. A hit counts only when
// the folded title equals the query's expected title and every credited
// source artist matches; otherwise nothing is returned and the caller records
// "no result".
func (a *App) neteaseSearchSong(ctx context.Context, plan SearchPlan) (*neteaseSong, error) {
	query := strings.TrimSpace(plan.Query)
	if plan.PlaylistTrack != "" {
		query = strings.TrimSpace(plan.ExpectedArtist + " " + plan.PlaylistTrack)
	}
	if query == "" {
		query = strings.TrimSpace(plan.ExpectedArtist + " " + plan.ExpectedTitle)
	}
	if query == "" {
		return nil, nil
	}
	_, songs, err := a.neteaseSearchSongs(ctx, query, 10)
	if err != nil {
		return nil, err
	}
	wantTitle := foldTagText(firstNonEmpty(plan.PlaylistTrack, plan.ExpectedTitle))
	if wantTitle == "" {
		wantTitle = foldTagText(plan.Query)
	}
	wantParts := splitPlaylistArtists(plan.ExpectedArtist)
	best := (*neteaseSong)(nil)
	var durable *neteaseSong
	ambiguous := false
	for _, item := range songs {
		if item.ID <= 0 || strings.TrimSpace(item.Name) == "" {
			continue
		}
		if wantTitle != "" && foldTagText(item.Name) != wantTitle {
			continue
		}
		artists := make([]string, 0, len(item.Artists))
		for _, artist := range item.Artists {
			if name := strings.TrimSpace(artist.Name); name != "" {
				artists = append(artists, name)
			}
		}
		artistMatch := len(wantParts) == 0 || playlistArtistsMatch(wantParts, artists)
		if plan.PlaylistTrack != "" && !(plan.NeteaseSongID > 0 && item.ID == plan.NeteaseSongID) {
			artistMatch = artistMatch && normalizeKey(item.Album.Name) == normalizeKey(plan.ExpectedTitle)
		}
		if !artistMatch || len(artists) == 0 {
			continue
		}
		candidate := &neteaseSong{ID: item.ID, Name: item.Name, Artists: artists, Album: item.Album.Name, DurationMS: item.Duration, AlbumID: item.Album.ID, CoverURL: item.Album.PicURL, CatalogVerified: true}
		if plan.NeteaseSongID > 0 && item.ID == plan.NeteaseSongID {
			durable = candidate
			continue
		}
		// Prefer the first exact match: the catalogue ranks by relevance and
		// the title/artist check above already rejected the rest.
		if plan.PlaylistTrack != "" && best != nil && (best.AlbumID != candidate.AlbumID || best.ID != candidate.ID) {
			ambiguous = true
			continue
		}
		if best == nil {
			best = candidate
		}
		if plan.PlaylistTrack == "" {
			break
		}
	}
	if durable != nil {
		// A durable id from the playlist is stronger evidence than duplicate
		// album labels, which NetEase often shares across reissues.
		return durable, nil
	}
	if ambiguous {
		return nil, errors.New("网易云返回多个同名录音，无法唯一确认发行版")
	}
	return best, nil
}

// neteaseURLFor asks for one song's media URL, lossless first and the highest
// bitrate as a fallback. Only the FLAC answer is usable here.
func (a *App) neteaseURLFor(ctx context.Context, song *neteaseSong) (string, string, error) {
	cookie := a.neteaseCookie()
	ids := "[" + strconv.FormatInt(song.ID, 10) + "]"
	attempts := []struct{ path, values string }{
		{"/api/song/enhance/player/url/v1", "ids=" + url.QueryEscape(ids) + "&level=lossless&encodeType=flac"},
		{"/api/song/enhance/player/url/v1", "ids=" + url.QueryEscape(ids) + "&level=hires&encodeType=flac"},
		{"/api/song/enhance/player/url/v1", "ids=" + url.QueryEscape(ids) + "&level=exhigh&encodeType=mp3"},
		{"/api/song/enhance/player/url/v1", "ids=" + url.QueryEscape(ids) + "&level=higher&encodeType=mp3"},
		{"/api/song/enhance/player/url/v1", "ids=" + url.QueryEscape(ids) + "&level=standard&encodeType=mp3"},
		{"/api/song/enhance/player/url", "ids=" + url.QueryEscape(ids) + "&br=999000"},
		{"/api/song/enhance/player/url", "ids=" + url.QueryEscape(ids) + "&br=320000"},
	}
	var lastErr error
	sawResponse := false
	for _, attempt := range attempts {
		body, err := a.neteaseGet(ctx, a.neteaseBase()+attempt.path+"?"+attempt.values, cookie)
		if err != nil {
			lastErr = err
			if classifyNeteaseFailure(err) != downloadpolicy.ClassNoResult {
				return "", "", err
			}
			continue
		}
		sawResponse = true
		dataRaw, contractErr := requiredNeteaseField(body, "data", "音源地址")
		if contractErr != nil {
			return "", "", contractErr
		}
		var data []struct {
			URL   string `json:"url"`
			Type  string `json:"type"`
			Code  int    `json:"code"`
			Level string `json:"level"`
		}
		if contractErr := json.Unmarshal(dataRaw, &data); contractErr != nil {
			return "", "", neteaseContractError("音源地址 data 结构异常：%v", contractErr)
		}
		if len(data) == 0 {
			continue
		}
		item := data[0]
		if item.Code != 0 && item.Code != 200 {
			continue
		}
		if strings.TrimSpace(item.URL) == "" {
			continue
		}
		return item.URL, strings.ToLower(item.Type), nil
	}
	if outerURL, outerErr := a.neteaseOuterMediaURL(ctx, song.ID); outerErr == nil && outerURL != "" {
		return outerURL, "mp3", nil
	} else if outerErr != nil && (lastErr == nil || classifyNeteaseFailure(outerErr) != downloadpolicy.ClassNoResult) {
		lastErr = outerErr
	}
	if lastErr != nil && !sawResponse {
		return "", "", lastErr
	}
	return "", "", errors.New("netease: 没有取到音源地址")
}

// neteaseOuterMediaURL is the official legacy playback redirect used by the
// web player. Some free catalogue rows expose a 302 here even when the JSON
// player endpoint returns an empty url. Only NetEase's own hosts (or the
// explicit test origin) are accepted before the media download follows it.
func (a *App) neteaseOuterMediaURL(ctx context.Context, songID int64) (string, error) {
	if songID <= 0 {
		return "", nil
	}
	endpoint := a.neteaseBase() + "/song/media/outer/url?id=" + strconv.FormatInt(songID, 10) + ".mp3"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "audio/*,application/json;q=0.8,*/*;q=0.5")
	request.Header.Set("Referer", "https://"+neteaseHost+"/")
	request.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	if cookie := a.neteaseCookie(); cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		location, parseErr := url.Parse(strings.TrimSpace(response.Header.Get("Location")))
		if parseErr != nil || location == nil {
			return "", neteaseContractError("outer 跳转地址无效")
		}
		location = request.URL.ResolveReference(location)
		if !a.neteaseMediaURLAllowed(location) || strings.TrimSpace(location.Path) == "/404" {
			return "", nil
		}
		return location.String(), nil
	case http.StatusOK:
		if strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "audio/") {
			return endpoint, nil
		}
		return "", nil
	case http.StatusNotFound:
		return "", nil
	case http.StatusTooManyRequests, http.StatusMethodNotAllowed, http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout:
		return "", &neteaseHTTPError{operation: "outer", status: response.StatusCode}
	default:
		if response.StatusCode >= 500 {
			return "", &neteaseHTTPError{operation: "outer", status: response.StatusCode}
		}
		return "", nil
	}
}

func (a *App) neteaseMediaURLAllowed(target *url.URL) bool {
	if target == nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return false
	}
	host := strings.ToLower(target.Hostname())
	if host == neteaseHost || host == "www."+neteaseHost || strings.HasSuffix(host, ".music.126.net") || strings.HasSuffix(host, ".126.net") {
		return true
	}
	if rawBase := strings.TrimSpace(a.neteaseBases["api"]); rawBase != "" {
		base, err := url.Parse(rawBase)
		return err == nil && base != nil && strings.EqualFold(target.Scheme, base.Scheme) && strings.EqualFold(target.Host, base.Host)
	}
	return false
}

// neteasePreferredCoverBytes prefers the original release artwork for a
// compilation track. NetEase often exposes only the compilation cover on the
// selected track; an exact title/artist search can still identify the source
// release whose duration matches this recording.
func (a *App) neteasePreferredCoverBytes(ctx context.Context, song *neteaseSong) ([]byte, error) {
	if isHotSlowCompilationAlbum(song.Album) {
		if source, err := a.neteaseOriginalCoverSong(ctx, song); err == nil && source != nil {
			if cover, fetchErr := a.neteaseCoverBytes(ctx, source); fetchErr == nil {
				return cover, nil
			}
		}
		if cover, err := a.appleOriginalCoverBytes(ctx, song); err == nil {
			return cover, nil
		}
		if cover, err := a.deezerOriginalCoverBytes(ctx, song); err == nil {
			return cover, nil
		}
	}
	return a.neteaseCoverBytes(ctx, song)
}

func (a *App) neteaseOriginalCoverSong(ctx context.Context, song *neteaseSong) (*neteaseSong, error) {
	if song == nil || strings.TrimSpace(song.Name) == "" || len(song.Artists) == 0 {
		return nil, errors.New("网易云歌曲缺少原发行版检索信息")
	}
	_, songs, err := a.neteaseSearchSongs(ctx, strings.Join(song.Artists, " ")+" "+song.Name, 30)
	if err != nil {
		return nil, err
	}
	var best *neteaseSong
	bestDelta := 5001
	for _, item := range songs {
		if item.ID <= 0 || item.Album.ID <= 0 || normalizeKey(item.Name) != normalizeKey(song.Name) || isHotSlowCompilationAlbum(item.Album.Name) {
			continue
		}
		artists := make([]string, 0, len(item.Artists))
		for _, artist := range item.Artists {
			if n := strings.TrimSpace(artist.Name); n != "" {
				artists = append(artists, n)
			}
		}
		if !playlistArtistsOverlap(song.Artists, artists) {
			continue
		}
		delta := 5001
		if item.Duration > 0 && song.DurationMS > 0 {
			delta = item.Duration - song.DurationMS
			if delta < 0 {
				delta = -delta
			}
		}
		if delta > 5000 || (best != nil && delta >= bestDelta) {
			continue
		}
		bestDelta = delta
		best = &neteaseSong{ID: item.ID, Name: item.Name, Artists: artists, Album: item.Album.Name, AlbumID: item.Album.ID, DurationMS: item.Duration, CoverURL: item.Album.PicURL, CatalogVerified: true}
	}
	return best, nil
}

func playlistArtistsOverlap(want, got []string) bool {
	want = flattenPlaylistArtists(want)
	got = flattenPlaylistArtists(got)
	for _, expected := range want {
		for _, actual := range got {
			if normalizeNeteaseArtist(expected) != "" && normalizeNeteaseArtist(expected) == normalizeNeteaseArtist(actual) {
				return true
			}
		}
	}
	return false
}

// neteaseCoverURLFor resolves the selected song's album artwork. Album
// tracks carry the release cover from the album document; direct song
// fallbacks recover it from the same album id.
func (a *App) neteaseCoverURLFor(ctx context.Context, song *neteaseSong) (string, error) {
	if song == nil {
		return "", nil
	}
	if strings.TrimSpace(song.CoverURL) != "" {
		return strings.TrimSpace(song.CoverURL), nil
	}
	if song.AlbumID <= 0 {
		return "", nil
	}
	raw, err := a.neteaseGet(ctx, a.neteaseBase()+"/api/v1/album/"+strconv.FormatInt(song.AlbumID, 10), a.neteaseCookie())
	if err != nil {
		return "", err
	}
	albumRaw, err := requiredNeteaseField(raw, "album", "专辑封面")
	if err != nil {
		return "", err
	}
	picRaw, err := requiredNeteaseField(albumRaw, "picUrl", "专辑封面")
	if err != nil {
		return "", err
	}
	var picURL string
	if err := json.Unmarshal(picRaw, &picURL); err != nil {
		return "", neteaseContractError("专辑封面 picUrl 结构异常：%v", err)
	}
	song.CoverURL = strings.TrimSpace(picURL)
	return song.CoverURL, nil
}

// neteaseCoverBytes downloads and fully decodes the selected track's artwork.
// A valid image header is not enough: truncated CDN responses produced half
// covers in older releases.
func (a *App) neteaseCoverBytes(ctx context.Context, song *neteaseSong) ([]byte, error) {
	rawURL, err := a.neteaseCoverURLFor(ctx, song)
	if err != nil {
		return nil, err
	}
	if rawURL == "" || !a.neteaseMediaAllowed(rawURL) {
		return nil, errors.New("网易云歌曲封面地址为空或不在网易云域名下")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Referer", "https://"+neteaseHost+"/")
	request.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("网易云歌曲封面返回失败")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (10<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 10<<20 || !imageBytes(body) {
		return nil, errors.New("网易云歌曲封面不完整")
	}
	return body, nil
}

// neteaseMediaAllowed keeps the download on NetEase's own CDN: the URL comes
// from a remote document, so the host is checked before any byte is fetched.
// The loopback branch exists only when a test has replaced the API origin.
func (a *App) neteaseMediaAllowed(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	if a != nil && a.neteaseBases != nil && loopbackURL(raw) {
		return true
	}
	host := strings.ToLower(parsed.Hostname())
	return host == neteaseHost || strings.HasSuffix(host, ".music.126.net") || strings.HasSuffix(host, ".126.net")
}

// neteaseDownload reads the media file. The size is bounded and the caller
// still checks the FLAC header before treating it as audio.
func (a *App) neteaseDownload(ctx context.Context, mediaURL string) ([]byte, error) {
	if !a.neteaseMediaAllowed(mediaURL) {
		return nil, errors.New("netease: 音源地址不在网易云域名下")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, mediaURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Referer", "https://"+neteaseHost+"/")
	request.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	client := &http.Client{Timeout: 90 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, &neteaseHTTPError{operation: "音频", status: response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, neteaseMediaMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > neteaseMediaMaxBytes {
		return nil, errors.New("netease: 音频超过大小上限")
	}
	return body, nil
}

// tryNeteaseStream is the built-in NetEase step of the fallback walk. handled
// is false when no cookie is saved, so the caller keeps the historical engine
// path instead of reporting a failure for a source that was never configured.
func (a *App) tryNeteaseStream(ctx context.Context, plan SearchPlan) (string, string, string, bool) {
	if a.neteaseCookie() == "" {
		return "", "", "", false
	}
	song, err := a.neteaseSearchSong(ctx, plan)
	if err != nil {
		note := "netease 没有连上（" + err.Error() + "）"
		return a.stageNeteaseEngineFallback(ctx, plan, note, classifyNeteaseFailure(err))
	}
	if song == nil {
		return a.stageNeteaseEngineFallback(ctx, plan, "netease 没有找到标题与艺人同时对得上的曲目", downloadpolicy.ClassNoResult)
	}
	return a.stageNeteaseSongWithEngine(ctx, plan, song)
}

func (a *App) stageNeteaseSong(ctx context.Context, plan SearchPlan, song *neteaseSong) (string, string, string, bool) {
	mediaURL, _, err := a.neteaseURLFor(ctx, song)
	if err != nil {
		return "", "netease 取不到音源地址（" + err.Error() + "）", classifyNeteaseFailure(err), true
	}
	body, err := a.neteaseDownload(ctx, mediaURL)
	if err != nil {
		return "", "netease 没有取到音频（" + err.Error() + "）", classifyNeteaseFailure(err), true
	}
	if len(body) < 1024 || !isKnownAudio(body) {
		return "", "netease 返回的内容不是可识别音频", downloadpolicy.ClassNoResult, true
	}
	lossySource := !strings.HasPrefix(string(body[:4]), "fLaC")
	if lossySource {
		// NetEase is the last-resort source for NetEase playlists. Accounts
		// without lossless access receive a decodable MP3. Convert that source
		// to the library's FLAC container while retaining the explicit lossy
		// provenance, so the playlist can finish and a later quality-upgrade
		// pass can still recognise the original as lossy.
		converted, convErr := a.transcodeEngineAudio(ctx, body, "netease", plan)
		if convErr != nil {
			return "", "netease 有损源转换 FLAC 失败：" + convErr.Error(), downloadpolicy.ClassRateLimited, true
		}
		body = converted
	}
	tags, err := flactags.ReadBytes(body)
	if err != nil || tags.StreamInfo.SampleRate == 0 || tags.StreamInfo.TotalSamples == 0 {
		return "", "netease 返回的 FLAC 没有可用时长", downloadpolicy.ClassNoResult, true
	}
	if cover := flactags.FrontCover(body); len(cover) > 0 && !imageBytes(cover) {
		body, err = flactags.RemoveFrontCover(body)
		if err != nil {
			return "", "netease 封面损坏且无法清理", downloadpolicy.ClassNoResult, true
		}
		tags, err = flactags.ReadBytes(body)
		if err != nil {
			return "", "netease 清理损坏封面后无法读取音频", downloadpolicy.ClassNoResult, true
		}
	}
	if isHotSlowCompilationAlbum(song.Album) {
		cover, coverErr := a.neteasePreferredCoverBytes(ctx, song)
		if coverErr != nil {
			return "", "网易云歌曲封面不可用：" + coverErr.Error(), classifyNeteaseFailure(coverErr), true
		}
		body, err = flactags.EmbedFrontCover(body, cover)
		if err != nil {
			return "", "网易云歌曲封面写入失败", downloadpolicy.ClassNoResult, true
		}
		tags, err = flactags.ReadBytes(body)
		if err != nil {
			return "", "网易云歌曲封面写入后无法读取音频", downloadpolicy.ClassNoResult, true
		}
	}
	if plan.PlaylistTrack != "" && song.DurationMS > 0 {
		actual := int64(tags.StreamInfo.TotalSamples) * 1000 / int64(tags.StreamInfo.SampleRate)
		drift := actual - int64(song.DurationMS)
		if !neteaseDurationAccepted(drift, song.CatalogVerified) {
			return "", "网易云音频时长与目录差异过大，未保存", downloadpolicy.ClassNoResult, true
		}
	}
	if err := os.MkdirAll(a.workPath("incoming"), 0o700); err != nil {
		return "", "netease 校验通过但没有写入：" + err.Error(), downloadpolicy.ClassRateLimited, true
	}
	dir, err := resolveInDir(a.workDir(), "incoming")
	if err != nil {
		return "", "netease 校验通过但没有写入：" + err.Error(), downloadpolicy.ClassRateLimited, true
	}
	quality := ""
	if lossySource {
		quality = "lossy-"
	}
	name := fmt.Sprintf("netease-%s%s-%x-%d.flac", quality, safeStreamID(plan.RootJobID), streamStagingDigest(body, plan.RootJobID, song.Name, song.Album, strings.Join(song.Artists, " / ")), song.ID)
	path := filepath.Join(dir, name)
	release, err := a.reserveAudioCache(dir, 2*int64(len(body))+65536)
	if err != nil {
		return "", err.Error(), downloadpolicy.ClassRateLimited, true
	}
	defer release()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return "", "netease 校验通过但没有写入：" + err.Error(), downloadpolicy.ClassRateLimited, true
	}
	// The media file carries NetEase's own tags; write the identity the
	// search confirmed so the rest of the pipeline sees a known track. The
	// marker survives scraping and lets a later quality-upgrade pass treat a
	// FLAC container made from an MP3 as lossy while still allowing playback.
	albumTitle := song.Album
	albumArtist := strings.Join(song.Artists, " / ")
	if isHotSlowCompilationAlbum(song.Album) {
		albumTitle = hotSlowCompilationAlbum
		albumArtist = hotSlowCompilationArtist
	}
	writeTags := flactags.Tags{
		Title: song.Name, Artists: song.Artists, Album: albumTitle, AlbumArtist: albumArtist,
	}
	if lossySource {
		writeTags.Raw = map[string][]string{"COMMENT": {"MusicOps source=netease-lossy"}}
	}
	if err := flactags.Write(path, writeTags); err != nil {
		_ = os.Remove(path)
		return "", "netease 写入标签失败，文件已删除：" + err.Error(), downloadpolicy.ClassRateLimited, true
	}
	if lossySource {
		return path, "netease 仅提供有损音频，已转换并校验为 FLAC（来源仍有损：" + song.Name + "）", downloadpolicy.ClassDownload, true
	}
	return path, "netease 已取到无损音频并通过校验（" + song.Name + "）", downloadpolicy.ClassDownload, true
}

func playlistArtistsMatch(want, got []string) bool {
	want = flattenPlaylistArtists(want)
	got = flattenPlaylistArtists(got)
	for _, expected := range want {
		found := false
		for _, actual := range got {
			if normalizeNeteaseArtist(expected) == normalizeNeteaseArtist(actual) && normalizeNeteaseArtist(expected) != "" {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return len(want) > 0
}

// A catalog-verified NetEase song id is stronger identity evidence than a
// duration rounded by an album or compilation listing. Several legitimate
// lossless files differ by a few seconds because of silence trimming, while a
// 30-second preview remains unmistakably outside this window.
func neteaseDurationAccepted(drift int64, catalogVerified bool) bool {
	if drift < 0 {
		drift = -drift
	}
	if catalogVerified {
		return drift <= 30000
	}
	return drift <= 1500
}
func (a *App) stageNeteaseSongWithEngine(ctx context.Context, plan SearchPlan, song *neteaseSong) (string, string, string, bool) {
	path, note, class, handled := a.stageNeteaseSong(ctx, plan, song)
	if path != "" || !handled {
		return path, note, class, handled
	}
	primaryNote, primaryClass := note, class
	if song.CatalogVerified && song.ID > 0 && song.ID == plan.NeteaseSongID {
		// The playlist already supplied the identity. Text search can omit the
		// exact ID even when the engine's download endpoint can serve it.
		exact := &streamSong{
			ID: strconv.FormatInt(song.ID, 10), Source: "netease", Name: song.Name,
			Artist: strings.Join(song.Artists, " / "), Album: song.Album, Duration: (song.DurationMS + 500) / 1000,
		}
		if enginePath, engineNote, engineClass := a.tryConfiguredStreamSong(ctx, "netease", plan, exact); enginePath != "" {
			return enginePath, primaryNote + "；网易云内置下载引擎：" + engineNote, downloadpolicy.ClassDownload, true
		} else {
			primaryNote += "；网易云内置下载引擎：" + engineNote
			primaryClass = preferStreamFailure(primaryClass, engineClass)
		}
	} else if enginePath, engineNote, engineClass := a.tryConfiguredStreamSong(ctx, "netease", plan, nil); enginePath != "" {
		return enginePath, primaryNote + "；网易云内置下载引擎：" + engineNote, downloadpolicy.ClassDownload, true
	} else {
		primaryNote += "；网易云内置下载引擎：" + engineNote
		primaryClass = preferStreamFailure(primaryClass, engineClass)
	}

	if strings.TrimSpace(plan.PlaylistTrack) == "" {
		return "", primaryNote, primaryClass, true
	}
	if plan.NeteaseSongID > 0 && plan.NeteaseAlbumID > 0 {
		if recovered, recoveredNote, recoveredClass := a.recoverNeteaseFromSecondaryStreams(ctx, plan, song); recovered != "" {
			return recovered, primaryNote + "；" + recoveredNote, downloadpolicy.ClassDownload, true
		} else {
			primaryNote += "；" + recoveredNote
			primaryClass = preferStreamFailure(primaryClass, recoveredClass)
		}
	}
	alternates, alternateErr := a.neteaseAlternateSongs(ctx, plan, song)
	if alternateErr != nil {
		return "", primaryNote + "；网易云替代录音检索失败：" + alternateErr.Error(), preferStreamFailure(primaryClass, classifyNeteaseFailure(alternateErr)), true
	}
	if len(alternates) == 0 {
		return "", primaryNote + "；没有找到曲名、歌手和时长都匹配的安全替代录音", primaryClass, true
	}
	notes := make([]string, 0, len(alternates))
	for _, alternate := range alternates {
		if recovered, recoveredNote, _, _ := a.stageNeteaseSong(ctx, plan, &alternate); recovered != "" {
			if err := a.finalizeNeteaseAlternate(ctx, recovered, plan, &alternate, song, song.DurationMS); err != nil {
				_ = os.Remove(recovered)
				notes = append(notes, "ID "+strconv.FormatInt(alternate.ID, 10)+"："+err.Error())
				continue
			}
			return recovered, primaryNote + "；网易云精确录音没有可用音源，已自动选用同曲同艺人的替代发行版 ID " + strconv.FormatInt(alternate.ID, 10) + "（" + alternate.Album + "）；" + recoveredNote, downloadpolicy.ClassDownload, true
		} else {
			notes = append(notes, "ID "+strconv.FormatInt(alternate.ID, 10)+"："+recoveredNote)
		}
		exact := &streamSong{
			ID: strconv.FormatInt(alternate.ID, 10), Source: "netease", Name: alternate.Name,
			Artist: strings.Join(alternate.Artists, " / "), Album: alternate.Album, Duration: (alternate.DurationMS + 500) / 1000,
		}
		if enginePath, engineNote, _ := a.tryConfiguredStreamSong(ctx, "netease", plan, exact); enginePath != "" {
			if err := a.finalizeNeteaseAlternate(ctx, enginePath, plan, &alternate, song, song.DurationMS); err != nil {
				_ = os.Remove(enginePath)
				notes = append(notes, "ID "+strconv.FormatInt(alternate.ID, 10)+" 引擎："+err.Error())
				continue
			}
			return enginePath, primaryNote + "；网易云精确录音没有可用音源，已自动选用同曲同艺人的替代发行版 ID " + strconv.FormatInt(alternate.ID, 10) + "（" + alternate.Album + "）；" + engineNote, downloadpolicy.ClassDownload, true
		} else {
			notes = append(notes, "ID "+strconv.FormatInt(alternate.ID, 10)+" 引擎："+engineNote)
		}
	}
	return "", primaryNote + "；安全替代录音均未取得可用音频：" + strings.Join(notes, "；"), primaryClass, true
}

// recoverNeteaseFromSecondaryStreams is the final source fallback after the
// exact NetEase recording and its own download engine returned no playable
// audio. QQ Music, Kugou and Kuwo are different licensed catalogues; a hit is accepted
// only when title and an overlapping credited artist match, then the file is
// duration-checked and rewritten with the NetEase row's album, artists and
// artwork before it can enter the library.
func (a *App) recoverNeteaseFromSecondaryStreams(ctx context.Context, plan SearchPlan, anchor *neteaseSong) (string, string, string) {
	if anchor == nil || strings.TrimSpace(anchor.Name) == "" {
		return "", "备用流媒体无法恢复：缺少网易云目标曲目", downloadpolicy.ClassNoResult
	}
	artists := append([]string(nil), splitPlaylistArtists(plan.ExpectedArtist)...)
	artists = append(artists, anchor.Artists...)
	artists = neteaseRecoveryArtistAliases(artists...)
	seenArtists := map[string]bool{}
	notes := []string{}
	class := downloadpolicy.ClassNoResult
	for _, source := range []string{"qqmusic", "kugou", "kuwo"} {
		for _, artist := range artists {
			artist = strings.TrimSpace(artist)
			key := normalizeNeteaseArtist(artist)
			if artist == "" || key == "" || seenArtists[source+"\x00"+key] {
				continue
			}
			seenArtists[source+"\x00"+key] = true
			recoveryPlan := plan
			recoveryPlan.ExpectedTitle = anchor.Name
			recoveryPlan.ExpectedArtist = artist
			recoveryPlan.PlaylistTrack = ""
			recoveryPlan.Query = anchor.Name + " " + artist
			path, note, stepClass := a.tryConfiguredStreamSong(ctx, source, recoveryPlan, nil)
			if path == "" {
				if len(notes) < 6 {
					notes = append(notes, source+" / "+artist+"："+note)
				}
				class = preferStreamFailure(class, stepClass)
				continue
			}
			if err := a.finalizeNeteaseAlternate(ctx, path, plan, nil, anchor, anchor.DurationMS); err != nil {
				_ = os.Remove(path)
				if len(notes) < 6 {
					notes = append(notes, source+" / "+artist+"："+err.Error())
				}
				class = preferStreamFailure(class, downloadpolicy.ClassNoResult)
				continue
			}
			return path, "网易云原曲无可用音频，已由 " + source + " 取得同曲同艺人录音，并恢复网易云专辑、歌手和封面；" + note, downloadpolicy.ClassDownload
		}
	}
	if recovered, recoveredNote, recoveredClass := a.recoverNeteaseByTitleDuration(ctx, plan, anchor); recovered != "" {
		return recovered, recoveredNote, downloadpolicy.ClassDownload
	} else {
		if len(notes) < 8 {
			notes = append(notes, recoveredNote)
		}
		class = preferStreamFailure(class, recoveredClass)
	}
	if recovered, recoveredNote, recoveredClass := a.recoverNeteaseByTitleDuration(ctx, plan, anchor); recovered != "" {
		return recovered, recoveredNote, downloadpolicy.ClassDownload
	} else {
		if len(notes) < 8 {
			notes = append(notes, recoveredNote)
		}
		class = preferStreamFailure(class, recoveredClass)
	}
	if len(notes) == 0 {
		notes = append(notes, "没有可用的备用流媒体检索词")
	}
	return "", "备用流媒体均未取得可安全发布的原曲：" + strings.Join(notes, "；"), class
}

// neteaseRecoveryArtistAliases adds only spelling variants that describe the
// same credited act, such as a trailing "Official" account suffix or a DJ
// prefix on a Chinese stage name. Each alias is still followed by the normal
// title, artist and duration checks before bytes can enter the library.
func neteaseRecoveryArtistAliases(values ...string) []string {
	out := make([]string, 0, len(values)*2)
	seen := map[string]bool{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		key := normalizeNeteaseArtist(value)
		if value == "" || key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, value)
	}
	for _, value := range values {
		add(value)
		trimmed := strings.TrimSpace(value)
		lower := strings.ToLower(trimmed)
		runes := []rune(trimmed)
		if len(runes) > 2 && strings.HasPrefix(lower, "dj") && unicode.Is(unicode.Han, runes[2]) {
			add(strings.TrimSpace(string(runes[2:])))
		}
		if strings.HasSuffix(lower, "official") {
			add(strings.TrimSpace(trimmed[:len(trimmed)-len("official")]))
		}
	}
	return out
}
func (a *App) stageNeteaseEngineFallback(ctx context.Context, plan SearchPlan, primaryNote, primaryClass string) (string, string, string, bool) {
	return a.finishNeteaseEngineFallback(ctx, plan, primaryNote, primaryClass, nil)
}

func (a *App) finishNeteaseEngineFallback(ctx context.Context, plan SearchPlan, primaryNote, primaryClass string, exact *streamSong) (string, string, string, bool) {
	enginePath, engineNote, engineClass := a.tryConfiguredStreamSong(ctx, "netease", plan, exact)
	if enginePath != "" {
		return enginePath, primaryNote + "；网易云内置下载引擎：" + engineNote, downloadpolicy.ClassDownload, true
	}
	return "", primaryNote + "；" + engineNote, preferStreamFailure(primaryClass, engineClass), true
}

func preferStreamFailure(primary, fallback string) string {
	if isTemporaryStreamFailure(primary) {
		return primary
	}
	if isTemporaryStreamFailure(fallback) {
		return fallback
	}
	if primary != "" && primary != downloadpolicy.ClassNoResult {
		return primary
	}
	return fallback
}
