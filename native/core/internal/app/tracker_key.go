package app

// Tracker download keys.
//
// dicmusic needs an authkey and orpheus needs a torrent_pass in the download
// URL. Both sites put that key in the link behind their download / FL button,
// which means a logged-in cookie is enough to read it: fetch the torrent page
// and take the key out of the link. The discovered value is cached in the
// Vault per site, so the page is read once instead of on every request.

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

func trackerHost(site string) string {
	switch site {
	case "dicmusic":
		return "dicmusic.com"
	case "orpheus":
		return "orpheus.network"
	case "open.cd":
		return "www.open.cd"
	case "redacted":
		return "redacted.sh"
	}
	return ""
}

func trackerKeyRef(site string) string { return "vault://pt/key/" + site }

var trackerKeyPattern = map[string]*regexp.Regexp{
	"dicmusic": regexp.MustCompile(`(?i)authkey=([A-Za-z0-9]{16,64})`),
	"orpheus":  regexp.MustCompile(`(?i)torrent_pass=([A-Za-z0-9]{16,64})`),
}

// trackerKey returns the download key for site: the one remembered for this
// job first, then the cached one, then whatever the torrent page's download
// link carries.
func (a *App) trackerKey(ctx context.Context, jobID, site, cookie, resource string) string {
	if key := a.secret(jobID + "/" + site); key != "" {
		return key
	}
	host := trackerHost(site)
	if host == "" {
		return ""
	}
	if a.vault != nil {
		if value, ok, err := a.vault.Get(trackerKeyRef(site), host); err == nil && ok && value != "" {
			return value
		}
	}
	key := a.discoverTrackerKey(ctx, site, cookie, resource)
	if key != "" {
		if a.vault != nil {
			_, _ = a.vault.Set(trackerKeyRef(site), key, host)
		}
		a.rememberDownload(jobID, site, key)
	}
	return key
}

// discoverTrackerKey reads the torrent page and pulls the key out of the
// download link. The link is HTML-escaped (`&amp;authkey=`), which is why the
// match is on the parameter name alone. It tries the torrent-scoped page
// first (Gazelle calls it torrentid) and falls back to the group page.
func (a *App) discoverTrackerKey(ctx context.Context, site, cookie, resource string) string {
	pattern := trackerKeyPattern[site]
	host := trackerHost(site)
	if pattern == nil || host == "" || cookie == "" {
		return ""
	}
	client := &http.Client{Timeout: 20 * time.Second}
	for _, query := range []string{"torrentid=", "id="} {
		target := "https://" + host + "/torrents.php?" + query + url.QueryEscape(resource)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return ""
		}
		req.Header.Set("Cookie", cookie)
		req.Header.Set("User-Agent", "MusicOps/1.0")
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			continue
		}
		if match := pattern.FindSubmatch(body); len(match) == 2 {
			return string(match[1])
		}
	}
	return ""
}

// trackerKeyState reports whether a key is already cached, for diagnostics.
func (a *App) trackerKeyState() map[string]bool {
	out := map[string]bool{}
	if a.vault == nil {
		return out
	}
	for site := range trackerKeyPattern {
		_, ok, err := a.vault.Get(trackerKeyRef(site), trackerHost(site))
		out[site] = err == nil && ok
	}
	return out
}

// trackerSession reports whether the saved cookie still authenticates on the
// site. A stale cookie is silent: search still answers as a guest, so the
// job only fails much later ("获取未完成"). Asking the site directly turns
// that into one sentence the user can act on.
func (a *App) trackerSession(ctx context.Context, site, cookie string) (bool, string) {
	host := trackerHost(site)
	if host == "" || cookie == "" {
		return false, "没有保存 Cookie"
	}
	client := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/index.php", nil)
	if err != nil {
		return false, err.Error()
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) MusicOps/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return false, "连不上站点"
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return false, "读不到首页"
	}
	lower := strings.ToLower(string(body))
	switch site {
	case "open.cd":
		if strings.Contains(lower, "登錄") || strings.Contains(lower, "login") && !strings.Contains(lower, "logout") {
			return false, "站点返回登录页，Cookie 已失效"
		}
		return strings.Contains(lower, "logout"), "已登录"
	default:
		if strings.Contains(lower, "logout.php") || strings.Contains(lower, "action=logout") {
			return true, "已登录"
		}
		return false, "站点把请求当作游客，Cookie 已失效"
	}
}

// cookieCheck answers GET /v1/settings/cookies/check: for each saved site, is
// the cookie still good?
func (a *App) cookieCheck(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for _, site := range []string{"open.cd", "dicmusic", "orpheus", "redacted"} {
		cookie, ok := a.savedCookie(site)
		if !ok {
			out = append(out, map[string]any{"site": site, "saved": false, "logged_in": false, "detail": "没有保存 Cookie"})
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		ok2, detail := a.trackerSession(ctx, site, cookie)
		cancel()
		out = append(out, map[string]any{"site": site, "saved": true, "logged_in": ok2, "detail": detail})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sites": out})
}
