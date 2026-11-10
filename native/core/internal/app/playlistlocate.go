package app

import (
	"errors"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Offline playlist-link recognition. Short links are identified and then
// refused: this process does not follow them or call the platform.

var playlistPlatforms = map[string]string{
	"netease": "网易云音乐", "qqmusic": "QQ 音乐", "qishui": "汽水音乐", "kugou": "酷狗音乐",
	"kuwo": "酷我音乐", "migu": "咪咕音乐", "bilibili": "bilibili", "applemusic": "Apple Music",
	"spotify": "Spotify", "youtube": "YouTube Music",
}

var playlistHosts = map[string][]string{
	"netease":    {"music.163.com", "m.music.163.com", "y.music.163.com", "163cn.tv"},
	"qqmusic":    {"y.qq.com", "i.y.qq.com", "c.y.qq.com", "c6.y.qq.com", "c7.y.qq.com", "c8.y.qq.com", "c9.y.qq.com"},
	"qishui":     {"music.douyin.com", "qishui.douyin.com", "qishui.com", "www.qishui.com", "v.douyin.com"},
	"kugou":      {"www.kugou.com", "m.kugou.com", "m3ws.kugou.com", "t1.kugou.com", "t2.kugou.com", "kugou.com"},
	"kuwo":       {"www.kuwo.cn", "kuwo.cn", "m.kuwo.cn", "h5.kuwo.cn"},
	"migu":       {"music.migu.cn", "m.music.migu.cn", "c.musicapp.migu.cn"},
	"bilibili":   {"www.bilibili.com", "bilibili.com", "space.bilibili.com", "b23.tv"},
	"applemusic": {"music.apple.com"},
	"spotify":    {"open.spotify.com", "spotify.link"},
	"youtube":    {"music.youtube.com", "www.youtube.com", "youtube.com", "m.youtube.com", "youtu.be"},
}

var shortPlaylistHosts = map[string]bool{
	"163cn.tv": true, "c6.y.qq.com": true, "c7.y.qq.com": true, "c8.y.qq.com": true, "c9.y.qq.com": true,
	"v.douyin.com": true, "t1.kugou.com": true, "t2.kugou.com": true, "b23.tv": true, "spotify.link": true,
}

type playlistLocator struct {
	Platform   string `json:"platform"`
	Name       string `json:"name"`
	PlaylistID string `json:"playlist_id,omitempty"`
	URL        string `json:"url"`
	Short      bool   `json:"short_url,omitempty"`
	Note       string `json:"note"`
}

func (a *App) locatePlaylist(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text     string `json:"text"`
		Platform string `json:"platform"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	if body.Platform == "" {
		body.Platform = "auto"
	}
	found, err := parsePlaylistLocator(body.Text, body.Platform)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "playlists": found, "count": len(found),
		"fetched": false,
		"note":    "只识别链接里的平台和歌单 ID，不访问平台，也不读取曲目。曲目请导入歌单文件。",
	})
}

func parsePlaylistLocator(value, platform string) ([]playlistLocator, error) {
	if platform != "auto" && playlistPlatforms[platform] == "" {
		return nil, errors.New("不支持所选歌单平台")
	}
	text := strings.TrimSpace(html.UnescapeString(value))
	if text == "" || len(text) > 100000 {
		return nil, errors.New("请粘贴歌单链接或分享文本")
	}
	urls := regexp.MustCompile(`https?://[^\s<>"“”]+`).FindAllString(text, -1)
	if len(urls) == 0 {
		chosen := platform
		if chosen == "auto" {
			chosen = "netease"
		}
		id := strings.TrimSpace(text)
		if !validPlaylistExternalID(id) {
			return nil, errors.New("纯数字或 ID 请先选择平台；其他内容请粘贴完整链接或导入歌单文件")
		}
		return []playlistLocator{canonicalPlaylist(chosen, id)}, nil
	}
	if len(urls) > 20 {
		return nil, errors.New("一次最多解析 20 个歌单链接")
	}
	found := []playlistLocator{}
	seen := map[string]bool{}
	for _, raw := range urls {
		raw = strings.TrimRight(raw, ")）]】。，,;；")
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Hostname() == "" {
			return nil, errors.New("歌单链接无效")
		}
		if parsed.User != nil || (parsed.Port() != "" && parsed.Port() != "80" && parsed.Port() != "443") {
			return nil, errors.New("歌单链接不可包含登录信息或自定义端口")
		}
		host := strings.ToLower(parsed.Hostname())
		chosen := ""
		for name, hosts := range playlistHosts {
			for _, candidate := range hosts {
				if host == candidate {
					chosen = name
				}
			}
		}
		if chosen == "" || (platform != "auto" && platform != chosen) {
			return nil, errors.New("歌单域名与所选平台不匹配或不受支持")
		}
		if shortPlaylistHosts[host] && playlistIDFromURL(chosen, parsed) == "" {
			item := playlistLocator{Platform: chosen, Name: playlistPlatforms[chosen], URL: "https://" + host + parsed.RequestURI(), Short: true, Note: "短链不会被跟随，请粘贴完整歌单链接或导入歌单文件"}
			if !seen[item.URL] {
				seen[item.URL] = true
				found = append(found, item)
			}
			continue
		}
		id := playlistIDFromURL(chosen, parsed)
		if id == "" {
			return nil, errors.New("这是平台链接，但没有找到歌单 ID；请复制歌单而非单曲的分享链接")
		}
		item := canonicalPlaylist(chosen, id)
		if chosen == "applemusic" {
			item.URL = "https://" + host + parsed.EscapedPath()
		}
		if !seen[item.Platform+"\x00"+item.PlaylistID] {
			seen[item.Platform+"\x00"+item.PlaylistID] = true
			found = append(found, item)
		}
	}
	return found, nil
}

func canonicalPlaylist(platform, id string) playlistLocator {
	urls := map[string]string{
		"netease":    "https://music.163.com/playlist?id=" + id,
		"qqmusic":    "https://y.qq.com/n/ryqq/playlist/" + id,
		"qishui":     "https://music.douyin.com/qishui/playlist?playlist_id=" + id,
		"kugou":      "https://www.kugou.com/yy/special/single/" + id + ".html",
		"kuwo":       "https://www.kuwo.cn/playlist_detail/" + id,
		"migu":       "https://music.migu.cn/v3/music/playlist/" + id,
		"bilibili":   "https://www.bilibili.com/medialist/detail/ml" + id,
		"applemusic": "https://music.apple.com/cn/playlist/" + id,
		"spotify":    "https://open.spotify.com/playlist/" + id,
		"youtube":    "https://music.youtube.com/playlist?list=" + id,
	}
	return playlistLocator{Platform: platform, Name: playlistPlatforms[platform], PlaylistID: id, URL: urls[platform], Note: "已识别歌单 ID。曲目不会向该平台请求。"}
}

func playlistIDFromURL(platform string, parsed *url.URL) string {
	path := parsed.EscapedPath()
	query := parsed.Query()
	switch platform {
	case "netease":
		if strings.TrimRight(path, "/") == "/playlist" || strings.TrimRight(path, "/") == "/m/playlist" {
			return oneQuery(query, "id")
		}
	case "qqmusic":
		if match := regexp.MustCompile(`/playlist/([0-9]+)`).FindStringSubmatch(path); match != nil {
			return match[1]
		}
		return oneQuery(query, "id", "disstid", "dissid", "songlistid")
	case "qishui":
		if match := regexp.MustCompile(`/playlist/([A-Za-z0-9_-]+)`).FindStringSubmatch(path); match != nil {
			return match[1]
		}
		return oneQuery(query, "playlist_id", "playlistId", "id")
	case "kugou":
		if match := regexp.MustCompile(`/(?:special/single|songlist)/((?:collection|gcid)_[A-Za-z0-9_-]+|[0-9]+)`).FindStringSubmatch(path); match != nil {
			return match[1]
		}
		if id := oneQuery(query, "gcid", "global_collection_id"); id != "" {
			return id
		}
		return oneQuery(query, "specialid", "specialId", "collectionid", "id")
	case "kuwo":
		if match := regexp.MustCompile(`/(?:playlist_detail|playlist|npl)/([0-9]+)`).FindStringSubmatch(path); match != nil {
			return match[1]
		}
		return oneQuery(query, "pid", "id")
	case "migu":
		if match := regexp.MustCompile(`/playlist/([A-Za-z0-9]+)`).FindStringSubmatch(path); match != nil {
			return match[1]
		}
		return oneQuery(query, "playlistId", "playlistid", "id")
	case "bilibili":
		if match := regexp.MustCompile(`/medialist/detail/ml(\d+)`).FindStringSubmatch(path); match != nil {
			return match[1]
		}
		return oneQuery(query, "fid", "media_id")
	case "applemusic":
		if match := regexp.MustCompile(`(?i)/([a-z]{2})/playlist/(?:[^/]+/)?(pl\.[A-Za-z0-9-]+)`).FindStringSubmatch(path); match != nil {
			return match[2]
		}
	case "spotify":
		if match := regexp.MustCompile(`/playlist/([A-Za-z0-9]+)`).FindStringSubmatch(path); match != nil {
			return match[1]
		}
	case "youtube":
		return oneQuery(query, "list")
	}
	return ""
}

func oneQuery(query url.Values, keys ...string) string {
	found := ""
	for _, key := range keys {
		for _, value := range query[key] {
			if value == "" {
				continue
			}
			if found != "" && found != value {
				return ""
			}
			found = value
		}
	}
	if !validPlaylistExternalID(found) {
		return ""
	}
	return found
}

func validPlaylistExternalID(id string) bool {
	if id == "" || id == "0" || len(id) > 200 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '~' || r == '=' || r == '-':
		default:
			return false
		}
	}
	return true
}
