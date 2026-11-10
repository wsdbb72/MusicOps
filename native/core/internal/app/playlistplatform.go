package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/cookies"
)

// Public playlist reads follow the 0.4.31 domestic provider for the two
// platforms whose public metadata does not need a login. A saved cookie is
// attached only when its vault host is the platform host. Audio is never
// requested. A redirect is refused. base_url is accepted only for a loopback
// stand-in; an empty value uses the public origin.

var remotePlaylistID = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

type remotePlatform struct {
	Name   string
	Host   string
	Origin string
	Path   string
}

var remotePlatforms = map[string]remotePlatform{
	"qqmusic": {Name: "QQ 音乐", Host: "c.y.qq.com", Origin: "https://c.y.qq.com", Path: "/qzone/fcg-bin/fcg_ucc_getcdinfo_byids_cp.fcg"},
	"kuwo":    {Name: "酷我音乐", Host: "m.kuwo.cn", Origin: "https://m.kuwo.cn", Path: "/newh5app/wapi/api/www/playlist/playListInfo"},
}

type remotePlaylist struct {
	Platform  string
	ID        string
	Name      string
	Tracks    []playlistTrack
	Truncated bool
}

func (a *App) fetchPlaylist(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Platform   string `json:"platform"`
		PlaylistID string `json:"playlist_id"`
		BaseURL    string `json:"base_url"`
		Save       bool   `json:"save"`
		Subscribe  bool   `json:"subscribe"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	remote, err := a.fetchRemotePlaylist(r.Context(), body.Platform, body.PlaylistID, body.BaseURL)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "PLAYLIST_UNAVAILABLE", "error": err.Error(), "downloaded": false})
		return
	}
	slug := ""
	if body.Save || body.Subscribe {
		slug = playlistSlug(body.Platform + "-" + body.PlaylistID)
		if err = a.writeSavedPlaylist(slug, remote.Name, remote.Tracks); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "PLAYLIST_UNAVAILABLE", "downloaded": false})
			return
		}
	}
	if body.Subscribe && slug != "" {
		if err = a.rememberRemoteSubscription(slug, remote); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SUBSCRIPTION_UNAVAILABLE", "downloaded": false})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "platform": remote.Platform, "playlist_id": slug, "remote_id": remote.ID,
		"name": remote.Name, "track_count": len(remote.Tracks), "truncated": remote.Truncated,
		"downloaded": false, "imported": false,
		"note": "只读取了公开曲目名单。没有下载音频，也没有写入音乐库。",
	})
}

func (a *App) fetchRemotePlaylist(ctx context.Context, platform, id, base string) (remotePlaylist, error) {
	spec, ok := remotePlatforms[platform]
	if !ok || !remotePlaylistID.MatchString(id) {
		return remotePlaylist{}, errors.New("只支持公开的 QQ 音乐或酷我数字歌单 ID")
	}
	origin := spec.Origin
	if strings.TrimSpace(base) == "" {
		base = a.playlistBase(platform)
	}
	if strings.TrimSpace(base) != "" {
		if !loopbackURL(base) {
			return remotePlaylist{}, errors.New("歌单测试地址只能指向本机")
		}
		origin = strings.TrimRight(base, "/")
	}
	header, err := a.playlistCookie(platform, spec.Host)
	if err != nil {
		return remotePlaylist{}, err
	}
	var tracks []playlistTrack
	name := spec.Name
	truncated := false
	for page := 0; page < 3; page++ {
		payload, err := a.readPlaylistPage(ctx, spec, origin, id, page, header)
		if err != nil {
			return remotePlaylist{}, err
		}
		part, partName, total, err := decodePlaylistPage(platform, id, payload)
		if err != nil {
			return remotePlaylist{}, err
		}
		if partName != "" {
			name = partName
		}
		tracks = append(tracks, part...)
		if total <= len(tracks) || len(part) == 0 {
			truncated = false
			break
		}
		truncated = page == 2
	}
	if len(tracks) == 0 {
		return remotePlaylist{}, errors.New(spec.Name + " 歌单没有可读曲目")
	}
	return remotePlaylist{Platform: platform, ID: id, Name: name, Tracks: tracks, Truncated: truncated}, nil
}

func (a *App) playlistBase(platform string) string {
	if a.playlistBases == nil {
		return ""
	}
	return a.playlistBases[platform]
}

func (a *App) playlistCookie(platform, host string) (string, error) {
	var saved map[string]struct {
		CredentialRef string `json:"credential_ref"`
	}
	found, err := a.loadJSONSetting([]byte("app/settings/playlist-sources"), &saved)
	if err != nil || !found {
		return "", err
	}
	ref := strings.TrimSpace(saved[platform].CredentialRef)
	if ref == "" || a.vault == nil {
		return "", nil
	}
	raw, ok, err := a.vault.Get(ref, host)
	if err != nil || !ok || raw == "" {
		return "", errors.New("歌单 Cookie 未配置或不适用于该平台")
	}
	parsed, err := cookies.Unmarshal(raw)
	if err != nil {
		return "", errors.New("歌单 Cookie 无法读取")
	}
	header, err := cookies.Header(parsed, "/", a.now())
	if err != nil || header == "" {
		return "", errors.New("歌单 Cookie 无法用于这个请求")
	}
	return header, nil
}

func (a *App) readPlaylistPage(ctx context.Context, spec remotePlatform, origin, id string, page int, cookie string) ([]byte, error) {
	endpoint, err := url.Parse(origin + spec.Path)
	if err != nil {
		return nil, errors.New("歌单地址无效")
	}
	values := endpoint.Query()
	switch spec.Host {
	case "c.y.qq.com":
		values.Set("disstid", id)
		values.Set("type", "1")
		values.Set("json", "1")
		values.Set("utf8", "1")
		values.Set("onlysong", "0")
		values.Set("new_format", "1")
		values.Set("format", "json")
		values.Set("song_begin", strconv.Itoa(page*1000))
		values.Set("song_num", "1000")
	default:
		values.Set("pid", id)
		values.Set("pn", strconv.Itoa(page+1))
		values.Set("rn", "100")
	}
	endpoint.RawQuery = values.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, errors.New("歌单地址无效")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "MusicOps")
	if cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New(spec.Name + " 没有连上")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New(spec.Name + " 没有返回歌单")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, errors.New(spec.Name + " 响应不完整")
	}
	return body, nil
}

func decodePlaylistPage(platform, id string, payload []byte) ([]playlistTrack, string, int, error) {
	var decoded map[string]any
	if json.Unmarshal(payload, &decoded) != nil {
		return nil, "", 0, errors.New("歌单响应不是 JSON")
	}
	if platform == "qqmusic" {
		if code, _ := decoded["code"].(float64); code != 0 {
			return nil, "", 0, errors.New("QQ 音乐歌单未公开或需要登录")
		}
		list, _ := decoded["cdlist"].([]any)
		if len(list) == 0 {
			return nil, "", 0, errors.New("QQ 音乐歌单不存在或未公开")
		}
		meta, _ := list[0].(map[string]any)
		if meta == nil || !sameRemoteID(meta["disstid"], id) && meta["disstid"] != nil {
			return nil, "", 0, errors.New("QQ 音乐返回了其他歌单")
		}
		return playlistRows(meta["songlist"], "name", "singer", "albumname", "interval"), textField(meta["dissname"]), intField(meta["songnum"]), nil
	}
	if code, _ := decoded["code"].(float64); code != 200 {
		return nil, "", 0, errors.New("酷我歌单未公开或需要登录")
	}
	meta, _ := decoded["data"].(map[string]any)
	if meta == nil || !sameRemoteID(meta["id"], id) && meta["id"] != nil {
		return nil, "", 0, errors.New("酷我返回了其他歌单")
	}
	return playlistRows(meta["musicList"], "name", "artist", "album", "duration"), textField(meta["name"]), intField(meta["total"]), nil
}

func playlistRows(raw any, titleKey, artistKey, albumKey, durationKey string) []playlistTrack {
	items, _ := raw.([]any)
	var tracks []playlistTrack
	for _, item := range items {
		row, _ := item.(map[string]any)
		if row == nil {
			continue
		}
		title := textField(row[titleKey])
		if title == "" {
			title = textField(row["songname"])
		}
		if title == "" {
			continue
		}
		artist := artistField(row[artistKey])
		album := textField(row[albumKey])
		if album == "" {
			if albumObj, _ := row["album"].(map[string]any); albumObj != nil {
				album = textField(albumObj["name"])
			}
		}
		tracks = append(tracks, playlistTrack{Title: title, Artist: artist, Album: album, Duration: strconv.Itoa(intField(row[durationKey]))})
	}
	return tracks
}

func artistField(raw any) string {
	switch value := raw.(type) {
	case string:
		return textField(value)
	case []any:
		var names []string
		for _, item := range value {
			switch artist := item.(type) {
			case string:
				if textField(artist) != "" {
					names = append(names, textField(artist))
				}
			case map[string]any:
				if name := textField(artist["name"]); name != "" {
					names = append(names, name)
				}
			}
		}
		return strings.Join(names, " / ")
	default:
		return ""
	}
}

func textField(raw any) string {
	text, _ := raw.(string)
	text = strings.TrimSpace(text)
	if len([]rune(text)) > 500 {
		text = string([]rune(text)[:500])
	}
	return text
}

func intField(raw any) int {
	switch value := raw.(type) {
	case float64:
		if value > 0 && value < 1_000_000 {
			return int(value)
		}
	case string:
		parsed, err := strconv.Atoi(value)
		if err == nil && parsed > 0 && parsed < 1_000_000 {
			return parsed
		}
	}
	return 0
}

func sameRemoteID(raw any, id string) bool {
	switch value := raw.(type) {
	case string:
		return value == id
	case float64:
		return strconv.FormatInt(int64(value), 10) == id
	default:
		return false
	}
}

func (a *App) writeSavedPlaylist(slug, name string, tracks []playlistTrack) error {
	if !playlistSlugPattern.MatchString(slug) {
		return errors.New("歌单名称无法保存")
	}
	dir := a.savedPlaylistsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, slug+".m3u"), encodeM3U(name, tracks), 0o644)
}

func (a *App) rememberRemoteSubscription(slug string, remote remotePlaylist) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	rows, err := a.loadSubscriptions()
	if err != nil {
		return err
	}
	row := playlistSubscription{
		PlaylistID: slug, Name: remote.Name, IntervalHours: 24, Platform: remote.Platform, RemoteID: remote.ID,
		NextCheck: a.now().Add(24 * time.Hour).UTC().Format(time.RFC3339), LastResult: "已订阅公开歌单，尚未检查",
	}
	replaced := false
	for i := range rows {
		if rows[i].PlaylistID != slug {
			continue
		}
		row.Seen = rows[i].Seen
		row.Paused = rows[i].Paused
		row.LastChecked = rows[i].LastChecked
		row.IntervalHours = rows[i].IntervalHours
		rows[i] = row
		replaced = true
	}
	if !replaced {
		rows = append(rows, row)
	}
	return a.storeSubscriptions(rows)
}
