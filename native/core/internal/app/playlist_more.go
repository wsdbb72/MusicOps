package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// The remaining 0.4.31 playlist providers. Each call uses the same public
// JSON contract as the old client, the user's own cookie or token, and a
// loopback base only when a test supplies one. YouTube is never scraped.

var remoteIDPatterns = map[string]*regexp.Regexp{
	"qqmusic":    remotePlaylistID,
	"kuwo":       remotePlaylistID,
	"netease":    remotePlaylistID,
	"kugou":      regexp.MustCompile(`^(?:[1-9][0-9]{0,18}|(?:gcid|collection)_[A-Za-z0-9_-]{1,64})$`),
	"migu":       remotePlaylistID,
	"qishui":     regexp.MustCompile(`^[A-Za-z0-9_-]{4,80}$`),
	"spotify":    regexp.MustCompile(`^[A-Za-z0-9]{22}$`),
	"applemusic": regexp.MustCompile(`^pl\.[A-Za-z0-9.-]{1,80}$`),
	"youtube":    regexp.MustCompile(`^[A-Za-z0-9_-]{11,80}$`),
}

func init() {
	remotePlatforms["netease"] = remotePlatform{Name: "网易云音乐", Host: "music.163.com", Origin: "https://music.163.com"}
	remotePlatforms["kugou"] = remotePlatform{Name: "酷狗音乐", Host: "mobilecdn.kugou.com", Origin: "https://mobilecdn.kugou.com", Path: "/api/v3/special/song"}
	remotePlatforms["qishui"] = remotePlatform{Name: "汽水音乐", Host: "beta-luna.douyin.com", Origin: "https://beta-luna.douyin.com", Path: "/luna/playlist/detail"}
	remotePlatforms["migu"] = remotePlatform{Name: "咪咕音乐", Host: "app.c.nf.migu.cn", Origin: "https://app.c.nf.migu.cn", Path: "/MIGUM3.0/resource/playlist/song/v2.0"}
	remotePlatforms["spotify"] = remotePlatform{Name: "Spotify", Host: "api.spotify.com", Origin: "https://api.spotify.com", Path: "/v1/playlists/"}
	remotePlatforms["applemusic"] = remotePlatform{Name: "Apple Music", Host: "api.music.apple.com", Origin: "https://api.music.apple.com", Path: "/v1/catalog/cn/playlists/"}
	remotePlatforms["youtube"] = remotePlatform{Name: "YouTube", Host: "www.googleapis.com", Origin: "https://www.googleapis.com", Path: "/youtube/v3/playlistItems"}
}

func playlistPageRequest(ctx context.Context, spec remotePlatform, origin, id string, page int) (*http.Request, error) {
	switch spec.Host {
	case "music.163.com":
		endpoint := origin + "/api/v3/playlist/detail?id=" + url.QueryEscape(id) + "&n=10000"
		return http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	case "mobilecdn.kugou.com":
		endpoint, err := url.Parse(origin + spec.Path)
		if err != nil {
			return nil, err
		}
		values := endpoint.Query()
		values.Set("specialid", id)
		values.Set("page", strconv.Itoa(page+1))
		values.Set("pagesize", "100")
		endpoint.RawQuery = values.Encode()
		return http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	case "beta-luna.douyin.com":
		body, _ := json.Marshal(map[string]any{"playlist_id": id, "cursor": page * 100, "count": 100})
		return http.NewRequestWithContext(ctx, http.MethodPost, origin+spec.Path, bytes.NewReader(body))
	case "app.c.nf.migu.cn":
		endpoint, err := url.Parse(origin + spec.Path)
		if err != nil {
			return nil, err
		}
		values := endpoint.Query()
		values.Set("playlistId", id)
		values.Set("pageNo", strconv.Itoa(page+1))
		values.Set("pageSize", "100")
		endpoint.RawQuery = values.Encode()
		return http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	case "api.spotify.com":
		return http.NewRequestWithContext(ctx, http.MethodGet, origin+spec.Path+url.PathEscape(id), nil)
	case "api.music.apple.com":
		return http.NewRequestWithContext(ctx, http.MethodGet, origin+spec.Path+url.PathEscape(id), nil)
	case "www.googleapis.com":
		endpoint, err := url.Parse(origin + spec.Path)
		if err != nil {
			return nil, err
		}
		values := endpoint.Query()
		values.Set("part", "snippet,contentDetails")
		values.Set("playlistId", id)
		values.Set("maxResults", "50")
		endpoint.RawQuery = values.Encode()
		return http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	default:
		endpoint, err := url.Parse(origin + spec.Path)
		if err != nil {
			return nil, err
		}
		values := endpoint.Query()
		if spec.Host == "c.y.qq.com" {
			values.Set("disstid", id)
			values.Set("type", "1")
			values.Set("json", "1")
			values.Set("utf8", "1")
			values.Set("onlysong", "0")
			values.Set("new_format", "1")
			values.Set("format", "json")
			values.Set("song_begin", strconv.Itoa(page*1000))
			values.Set("song_num", "1000")
		} else {
			values.Set("pid", id)
			values.Set("pn", strconv.Itoa(page+1))
			values.Set("rn", "100")
		}
		endpoint.RawQuery = values.Encode()
		return http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	}
}

func decodeExtraPlaylist(platform, id string, decoded map[string]any) ([]playlistTrack, string, int, bool, error) {
	switch platform {
	case "netease":
		meta, _ := decoded["playlist"].(map[string]any)
		if meta == nil {
			return nil, "", 0, true, errors.New("网易云歌单不存在或未公开")
		}
		if meta["id"] != nil && !sameRemoteID(meta["id"], id) {
			return nil, "", 0, true, errors.New("网易云返回了其他歌单")
		}
		rows := neteaseArtists(meta["tracks"])
		total := intField(meta["trackCount"])
		if total < len(rows) {
			total = len(rows)
		}
		return rows, textField(meta["name"]), total, true, nil
	case "kugou":
		if intField(decoded["status"]) != 1 {
			return nil, "", 0, true, errors.New("酷狗歌单未公开或需要登录")
		}
		meta, _ := decoded["data"].(map[string]any)
		if meta == nil {
			return nil, "", 0, true, errors.New("酷狗歌单不存在或未公开")
		}
		rows := playlistRows(meta["info"], "songname", "singername", "album_name", "duration")
		return rows, textField(meta["specialname"]), intField(meta["total"]), true, nil
	case "qishui":
		if decoded["status_code"] != nil && intField(decoded["status_code"]) != 0 {
			return nil, "", 0, true, errors.New("汽水歌单未公开或需要登录")
		}
		meta, _ := decoded["playlist"].(map[string]any)
		if meta == nil {
			return nil, "", 0, true, errors.New("汽水歌单不存在或未公开")
		}
		return qishuiRows(decoded["media_resources"]), textField(meta["title"]), intField(meta["count_tracks"]), true, nil
	case "migu":
		if textField(decoded["code"]) != "000000" {
			return nil, "", 0, true, errors.New("咪咕歌单未公开或需要登录")
		}
		meta, _ := decoded["data"].(map[string]any)
		if meta == nil {
			return nil, "", 0, true, errors.New("咪咕歌单不存在或未公开")
		}
		return miguRows(meta["songList"]), textField(meta["name"]), intField(meta["totalCount"]), true, nil
	case "spotify":
		if decoded["id"] != nil && textField(decoded["id"]) != id {
			return nil, "", 0, true, errors.New("Spotify 返回了其他歌单")
		}
		tracks, _ := decoded["tracks"].(map[string]any)
		return spotifyRows(tracks["items"]), textField(decoded["name"]), intField(tracks["total"]), true, nil
	case "applemusic":
		list, _ := decoded["data"].([]any)
		if len(list) == 0 {
			return nil, "", 0, true, errors.New("Apple Music 歌单不存在或未公开")
		}
		meta, _ := list[0].(map[string]any)
		if meta == nil {
			return nil, "", 0, true, errors.New("Apple Music 歌单不存在或未公开")
		}
		if meta["id"] != nil && textField(meta["id"]) != id {
			return nil, "", 0, true, errors.New("Apple Music 返回了其他歌单")
		}
		attrs, _ := meta["attributes"].(map[string]any)
		rels, _ := meta["relationships"].(map[string]any)
		trackRel, _ := rels["tracks"].(map[string]any)
		rows := appleRows(trackRel["data"])
		return rows, textField(attrs["name"]), len(rows), true, nil
	case "youtube":
		rows := youtubeRows(decoded["items"])
		return rows, "YouTube 歌单", len(rows), true, nil
	default:
		return nil, "", 0, false, nil
	}
}

func neteaseArtists(raw any) []playlistTrack {
	items, _ := raw.([]any)
	out := make([]playlistTrack, 0, len(items))
	for _, item := range items {
		row, _ := item.(map[string]any)
		if row == nil {
			continue
		}
		title := textField(row["name"])
		artist := joinNames(row["ar"])
		if artist == "" {
			artist = joinNames(row["artists"])
		}
		album, _ := row["al"].(map[string]any)
		if title == "" {
			continue
		}
		out = append(out, playlistTrack{Title: title, Artist: artist, Album: textField(album["name"]), Duration: millisField(row["dt"])})
	}
	return out
}

func joinNames(raw any) string {
	items, _ := raw.([]any)
	names := make([]string, 0, len(items))
	for _, item := range items {
		row, _ := item.(map[string]any)
		if name := textField(row["name"]); name != "" {
			names = append(names, name)
		}
	}
	return strings.Join(names, " / ")
}

func qishuiRows(raw any) []playlistTrack {
	items, _ := raw.([]any)
	out := make([]playlistTrack, 0, len(items))
	for _, item := range items {
		row, _ := item.(map[string]any)
		entity, _ := row["entity"].(map[string]any)
		wrapper, _ := entity["track_wrapper"].(map[string]any)
		track, _ := wrapper["track"].(map[string]any)
		if track == nil {
			continue
		}
		album, _ := track["album"].(map[string]any)
		title := textField(track["name"])
		if title == "" {
			continue
		}
		out = append(out, playlistTrack{Title: title, Artist: joinNames(track["artists"]), Album: textField(album["name"]), Duration: millisField(track["duration"])})
	}
	return out
}

func miguRows(raw any) []playlistTrack {
	items, _ := raw.([]any)
	out := make([]playlistTrack, 0, len(items))
	for _, item := range items {
		row, _ := item.(map[string]any)
		if row == nil || textField(row["name"]) == "" {
			continue
		}
		out = append(out, playlistTrack{Title: textField(row["name"]), Artist: joinNames(row["singers"]), Album: textField(row["album"]), Duration: secondsText(row["duration"])})
	}
	return out
}

func spotifyRows(raw any) []playlistTrack {
	items, _ := raw.([]any)
	out := make([]playlistTrack, 0, len(items))
	for _, item := range items {
		row, _ := item.(map[string]any)
		track, _ := row["track"].(map[string]any)
		if track == nil || textField(track["name"]) == "" {
			continue
		}
		album, _ := track["album"].(map[string]any)
		out = append(out, playlistTrack{Title: textField(track["name"]), Artist: joinNames(track["artists"]), Album: textField(album["name"]), Duration: millisField(track["duration_ms"])})
	}
	return out
}

func appleRows(raw any) []playlistTrack {
	items, _ := raw.([]any)
	out := make([]playlistTrack, 0, len(items))
	for _, item := range items {
		row, _ := item.(map[string]any)
		attrs, _ := row["attributes"].(map[string]any)
		if attrs == nil || textField(attrs["name"]) == "" {
			continue
		}
		out = append(out, playlistTrack{Title: textField(attrs["name"]), Artist: textField(attrs["artistName"]), Album: textField(attrs["albumName"]), Duration: millisField(attrs["durationInMillis"])})
	}
	return out
}

func millisField(raw any) string {
	ms := intField(raw)
	if ms <= 0 {
		return ""
	}
	return strconv.Itoa(ms / 1000)
}

func secondsText(raw any) string {
	seconds := intField(raw)
	if seconds <= 0 {
		return ""
	}
	return strconv.Itoa(seconds)
}

func youtubeRows(raw any) []playlistTrack {
	items, _ := raw.([]any)
	out := make([]playlistTrack, 0, len(items))
	for _, item := range items {
		row, _ := item.(map[string]any)
		snippet, _ := row["snippet"].(map[string]any)
		if snippet == nil || textField(snippet["title"]) == "" {
			continue
		}
		out = append(out, playlistTrack{Title: textField(snippet["title"]), Artist: textField(snippet["videoOwnerChannelTitle"])})
	}
	return out
}
