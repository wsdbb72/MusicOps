package app

// Lyrics and cover fetching against an LrcApi-compatible service
// (github.com/HisAtri/LrcApi): GET {base}{lyrics_path}?title=&artist=&album=
// answers LRC text, GET {base}{cover_path}?title=&artist=&album= answers image
// bytes, and an optional raw authorization header protects both. The saved
// settings decide the address, the paths and the credential; a request may
// override the address for one call, but the same origin rules apply.
//
// The rules that matter:
//   - plain HTTP is only allowed for a loopback host; anything else must be
//     HTTPS, so the authorization value never travels in the clear;
//   - redirects are refused: the credential is bound to the configured host;
//   - a 404 means "this service has no lyrics/cover for that track" and lets
//     the tag write continue, while a network error or 5xx refuses the write
//     (the caller reports ARTWORK_UNAVAILABLE) instead of silently writing a
//     file with half the metadata.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// artworkTarget is one resolved request: the full URL without the query, the
// header name and the raw value. An empty target means "do not fetch".
type artworkTarget struct {
	URL    string
	Header string
	Value  string
}

func (t artworkTarget) configured() bool { return t.URL != "" }

// artworkTargets resolves the lyrics and cover endpoints for one call. Body
// overrides win; otherwise the saved service is used when it is not the
// placeholder address.
func (a *App) artworkTargets(bodyLyrics, bodyCover string) (artworkTarget, artworkTarget, error) {
	settings, err := a.loadLyricsSettings()
	if err != nil {
		return artworkTarget{}, artworkTarget{}, err
	}
	var lyrics, cover artworkTarget
	if strings.TrimSpace(bodyLyrics) != "" {
		base, err := artworkBase(strings.TrimSpace(bodyLyrics))
		if err != nil {
			return artworkTarget{}, artworkTarget{}, err
		}
		lyrics.URL = base + "/lyrics"
	} else if base, ok := savedLyricsBase(settings); ok {
		lyrics.URL = base + settings.LyricsPath
		lyrics.Header, lyrics.Value = a.lyricsAuth(settings)
	}
	if strings.TrimSpace(bodyCover) != "" {
		base, err := artworkBase(strings.TrimSpace(bodyCover))
		if err != nil {
			return artworkTarget{}, artworkTarget{}, err
		}
		cover.URL = base + "/cover"
	} else if base, ok := savedLyricsBase(settings); ok {
		cover.URL = base + settings.CoverPath
		cover.Header, cover.Value = a.lyricsAuth(settings)
	}
	return lyrics, cover, nil
}

// savedLyricsBase returns the configured service origin, or ok=false when the
// settings still hold the placeholder that ships with a fresh install.
func savedLyricsBase(settings lyricsSettings) (string, bool) {
	base, err := artworkBase(settings.BaseURL)
	if err != nil {
		return "", false
	}
	if strings.HasSuffix(strings.ToLower(hostOf(base)), ".invalid") {
		return "", false
	}
	return base, true
}

// artworkBase validates one base URL: HTTPS, or HTTP only for loopback.
func artworkBase(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return "", errors.New("歌词/封面地址无效")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("歌词/封面地址不得带凭据、查询或 fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", errors.New("歌词/封面地址只填服务根地址")
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !loopbackURL(parsed.String()) {
			return "", errors.New("歌词/封面地址须为 HTTPS（本机回环可用 HTTP）")
		}
	default:
		return "", errors.New("歌词/封面地址须为 http 或 https")
	}
	return strings.TrimRight(parsed.Scheme+"://"+parsed.Host, "/"), nil
}

func hostOf(base string) string {
	parsed, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// lyricsAuth reads the saved authorization value. The value is sent raw: the
// service compares the header verbatim and does not expect a Bearer prefix.
func (a *App) lyricsAuth(settings lyricsSettings) (string, string) {
	if a.vault == nil {
		return "", ""
	}
	host := hostOf(settings.BaseURL)
	if host == "" {
		return "", ""
	}
	raw, ok, err := a.vault.Get(lyricsAuthRef, host)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return "", ""
	}
	header := strings.TrimSpace(settings.AuthHeader)
	if header == "" {
		header = "Authorization"
	}
	return header, raw
}

// artworkGET performs one read. A 404 is reported as (nil, false, nil): the
// service answered that it has nothing, which is not a failure of the write.
func artworkGET(target artworkTarget, query url.Values) ([]byte, bool, error) {
	parsed, err := url.Parse(target.URL)
	if err != nil {
		return nil, false, errors.New("歌词/封面地址无效")
	}
	merged := parsed.Query()
	for key, values := range query {
		for _, value := range values {
			if value != "" {
				merged.Set(key, value)
			}
		}
	}
	parsed.RawQuery = merged.Encode()
	request, err := http.NewRequest(http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, false, err
	}
	if target.Value != "" {
		request.Header.Set(target.Header, target.Value)
	}
	// A cover is often a 301 to a CDN (the public service does exactly that),
	// so redirects are followed when no credential is attached. With a saved
	// authorization value they are refused: that value belongs to the
	// configured host and must not travel to wherever a redirect points.
	client := &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if target.Value != "" {
			return http.ErrUseLastResponse
		}
		if len(via) >= 3 {
			return errors.New("歌词/封面服务重定向过多")
		}
		return nil
	}}
	response, err := client.Do(request)
	if err != nil {
		return nil, false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		// Only reachable when a credential was attached and the redirect was
		// therefore refused. Say so instead of pretending the service has no
		// lyrics: the address is misconfigured for authenticated use.
		return nil, false, errors.New("歌词/封面服务要求跳转到其他主机，带凭据时不跟随")
	}
	if response.StatusCode != http.StatusOK {
		return nil, false, errors.New("歌词/封面服务返回失败")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return nil, false, err
	}
	return body, true, nil
}

// parseLyricsBody accepts what LrcApi answers: plain LRC text, or a JSON object
// carrying a lyrics field (the older shape some deployments use).
func parseLyricsBody(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "{") {
		var decoded struct {
			Lyrics string `json:"lyrics"`
		}
		if json.Unmarshal(body, &decoded) == nil {
			return strings.TrimSpace(decoded.Lyrics)
		}
	}
	return trimmed
}
