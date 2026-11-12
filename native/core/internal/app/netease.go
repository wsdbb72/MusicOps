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
	"context"
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
		return nil, errors.New("netease: 服务返回 " + strconv.Itoa(response.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 || body[0] == '<' {
		return nil, errors.New("netease: 返回了网页而不是 JSON")
	}
	return body, nil
}

// neteaseSong is one search hit.
type neteaseSong struct {
	ID         int64
	Name       string
	Artists    []string
	Album      string
	DurationMS int
}

// neteaseSearchSong finds the best match for a query. A hit counts only when
// the folded title equals the query's expected title and at least one artist
// matches; otherwise nothing is returned and the caller records "no result".
func (a *App) neteaseSearchSong(ctx context.Context, plan SearchPlan) (*neteaseSong, error) {
	query := strings.TrimSpace(plan.Query)
	if query == "" {
		query = strings.TrimSpace(plan.ExpectedArtist + " " + plan.ExpectedTitle)
	}
	if query == "" {
		return nil, nil
	}
	endpoint, err := url.Parse(a.neteaseBase() + "/api/search/get/web")
	if err != nil {
		return nil, err
	}
	values := endpoint.Query()
	values.Set("s", query)
	values.Set("type", "1")
	values.Set("offset", "0")
	values.Set("limit", "10")
	endpoint.RawQuery = values.Encode()
	body, err := a.neteaseGet(ctx, endpoint.String(), a.neteaseCookie())
	if err != nil {
		return nil, err
	}
	var decoded struct {
		Code   int `json:"code"`
		Result struct {
			Songs []struct {
				ID      int64   `json:"id"`
				Name    string  `json:"name"`
				Artists []struct {
					Name string `json:"name"`
				} `json:"artists"`
				Album struct {
					Name string `json:"name"`
				} `json:"album"`
				Duration int `json:"duration"`
			} `json:"songs"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, err
	}
	if decoded.Code != 200 {
		return nil, errors.New("netease: 搜索被拒绝")
	}
	wantTitle := foldTagText(plan.ExpectedTitle)
	if wantTitle == "" {
		wantTitle = foldTagText(plan.Query)
	}
	wantArtist := foldTagText(plan.ExpectedArtist)
	best := (*neteaseSong)(nil)
	for _, item := range decoded.Result.Songs {
		if item.ID <= 0 || strings.TrimSpace(item.Name) == "" {
			continue
		}
		if wantTitle != "" && foldTagText(item.Name) != wantTitle {
			continue
		}
		artists := make([]string, 0, len(item.Artists))
		artistMatch := wantArtist == ""
		for _, artist := range item.Artists {
			name := strings.TrimSpace(artist.Name)
			if name == "" {
				continue
			}
			artists = append(artists, name)
			if wantArtist != "" && (foldTagText(name) == wantArtist || strings.Contains(foldTagText(name), wantArtist)) {
				artistMatch = true
			}
		}
		if !artistMatch || len(artists) == 0 {
			continue
		}
		candidate := &neteaseSong{ID: item.ID, Name: item.Name, Artists: artists, Album: item.Album.Name, DurationMS: item.Duration}
		// Prefer the first exact match: the catalogue ranks by relevance and
		// the title/artist check above already rejected the rest.
		best = candidate
		break
	}
	return best, nil
}

// neteaseURLFor asks for one song's media URL, lossless first and the highest
// bitrate as a fallback. Only the FLAC answer is usable here.
func (a *App) neteaseURLFor(ctx context.Context, song *neteaseSong) (string, string, error) {
	cookie := a.neteaseCookie()
	ids := "[" + strconv.FormatInt(song.ID, 10) + "]"
	for _, attempt := range []struct{ path, values string }{
		{"/api/song/enhance/player/url/v1", "ids=" + url.QueryEscape(ids) + "&level=lossless&encodeType=flac"},
		{"/api/song/enhance/player/url", "ids=" + url.QueryEscape(ids) + "&br=999000"},
	} {
		body, err := a.neteaseGet(ctx, a.neteaseBase()+attempt.path+"?"+attempt.values, cookie)
		if err != nil {
			continue
		}
		var decoded struct {
			Data []struct {
				URL   string `json:"url"`
				Type  string `json:"type"`
				Code  int    `json:"code"`
				Level string `json:"level"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &decoded) != nil || len(decoded.Data) == 0 {
			continue
		}
		item := decoded.Data[0]
		if item.Code != 0 && item.Code != 200 {
			continue
		}
		if strings.TrimSpace(item.URL) == "" {
			continue
		}
		return item.URL, strings.ToLower(item.Type), nil
	}
	return "", "", errors.New("netease: 没有取到音源地址")
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
		return nil, errors.New("netease: 音频返回 " + strconv.Itoa(response.StatusCode))
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

// tryNeteaseStream is the built-in netease step of the fallback walk. handled
// is false when no cookie is saved, so the caller keeps the historical engine
// path instead of reporting a failure for a source that was never configured.
func (a *App) tryNeteaseStream(ctx context.Context, plan SearchPlan) (string, string, string, bool) {
	if a.neteaseCookie() == "" {
		return "", "", "", false
	}
	song, err := a.neteaseSearchSong(ctx, plan)
	if err != nil {
		return "", "netease 没有连上（" + err.Error() + "）", downloadpolicy.ClassNoResult, true
	}
	if song == nil {
		return "", "netease 没有找到标题与艺人同时对得上的曲目", downloadpolicy.ClassNoResult, true
	}
	mediaURL, kind, err := a.neteaseURLFor(ctx, song)
	if err != nil {
		return "", "netease 取不到音源地址", downloadpolicy.ClassNoResult, true
	}
	if kind != "flac" {
		// A 320k mp3 is what an account without lossless permission gets.
		// The library is FLAC-only, so it is reported rather than stored.
		return "", "netease 只返回了有损音频（" + kind + "），未保存", downloadpolicy.ClassNoResult, true
	}
	body, err := a.neteaseDownload(ctx, mediaURL)
	if err != nil {
		return "", "netease 没有取到音频", downloadpolicy.ClassNoResult, true
	}
	if len(body) < 1024 || !strings.HasPrefix(string(body[:4]), "fLaC") {
		return "", "netease 返回的内容不是 FLAC", downloadpolicy.ClassNoResult, true
	}
	tags, err := flactags.ReadBytes(body)
	if err != nil || tags.StreamInfo.SampleRate == 0 || tags.StreamInfo.TotalSamples == 0 {
		return "", "netease 返回的 FLAC 没有可用时长", downloadpolicy.ClassNoResult, true
	}
	if err := os.MkdirAll(a.workPath("incoming"), 0o700); err != nil {
		return "", "netease 校验通过但没有写入", downloadpolicy.ClassNoResult, true
	}
	dir, err := resolveInDir(a.workDir(), "incoming")
	if err != nil {
		return "", "netease 校验通过但没有写入", downloadpolicy.ClassNoResult, true
	}
	name := "netease-" + safeStreamID(plan.RootJobID) + "-" + strconv.FormatInt(song.ID, 10) + ".flac"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return "", "netease 校验通过但没有写入", downloadpolicy.ClassNoResult, true
	}
	// The media file carries NetEase's own tags; write the identity the
	// search confirmed so the rest of the pipeline sees a known track.
	if err := flactags.Write(path, flactags.Tags{
		Title: song.Name, Artists: song.Artists, Album: song.Album, AlbumArtist: strings.Join(song.Artists, " / "),
	}); err != nil {
		_ = os.Remove(path)
		return "", "netease 写入标签失败，文件已删除", downloadpolicy.ClassNoResult, true
	}
	return path, "netease 已取到无损音频并通过校验（" + song.Name + "）", downloadpolicy.ClassDownload, true
}
