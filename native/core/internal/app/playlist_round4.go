package app

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// fetchNetease reads a public playlist. When the detail response only carries
// trackIds, the song names come from /api/v3/song/detail in batches. Nothing
// here downloads audio.
func (a *App) fetchNetease(ctx context.Context, spec remotePlatform, origin, id, cookie, bearer string) (remotePlaylist, error) {
	body, err := a.readPlaylistPage(ctx, spec, origin, id, 0, cookie, bearer)
	if err != nil {
		return remotePlaylist{}, err
	}
	var decoded struct {
		Code     *int `json:"code"`
		Playlist struct {
			ID         any    `json:"id"`
			Name       string `json:"name"`
			TrackCount int    `json:"trackCount"`
			Tracks     []any  `json:"tracks"`
			TrackIDs   []any  `json:"trackIds"`
		} `json:"playlist"`
	}
	if json.Unmarshal(body, &decoded) != nil || decoded.Playlist.Name == "" && len(decoded.Playlist.Tracks) == 0 && len(decoded.Playlist.TrackIDs) == 0 {
		return remotePlaylist{}, errors.New("网易云音乐歌单不存在或未公开")
	}
	if decoded.Playlist.ID != nil && !sameRemoteID(decoded.Playlist.ID, id) {
		return remotePlaylist{}, errors.New("网易云返回了其他歌单")
	}
	if decoded.Code != nil && *decoded.Code != 200 {
		return remotePlaylist{}, errors.New("网易云音乐歌单未公开或需要登录")
	}
	tracks := neteaseArtists(decoded.Playlist.Tracks)
	ids := neteaseIDs(decoded.Playlist.TrackIDs)
	truncated := false
	if len(ids) > len(tracks) {
		missing := ids
		if len(tracks) > 0 && len(tracks) < len(ids) {
			missing = ids[len(tracks):]
		}
		more, short, err := a.neteaseSongDetails(ctx, spec, origin, missing, cookie, bearer)
		if err != nil {
			return remotePlaylist{}, err
		}
		tracks = append(tracks, more...)
		truncated = short
	}
	if len(tracks) == 0 {
		return remotePlaylist{}, errors.New("网易云音乐歌单没有可读曲目")
	}
	name := decoded.Playlist.Name
	if name == "" {
		name = spec.Name
	}
	return remotePlaylist{Platform: "netease", ID: id, Name: name, Tracks: tracks, Truncated: truncated}, nil
}

func neteaseIDs(raw []any) []int {
	ids := make([]int, 0, len(raw))
	for _, item := range raw {
		switch value := item.(type) {
		case float64:
			if value > 0 && value < 1_000_000_000_000 {
				ids = append(ids, int(value))
			}
		case map[string]any:
			if id := wideID(value["id"]); id > 0 {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func wideID(raw any) int {
	switch value := raw.(type) {
	case float64:
		if value > 0 && value < 1e12 && value == float64(int(value)) {
			return int(value)
		}
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err == nil && parsed > 0 {
			return parsed
		}
	}
	return 0
}

func (a *App) neteaseSongDetails(ctx context.Context, spec remotePlatform, origin string, ids []int, cookie, bearer string) ([]playlistTrack, bool, error) {
	// 100 ids per /api/v3/song/detail call is what the endpoint accepts in
	// practice; 60 batches covers 6000 tracks, which is above the largest
	// playlist NetEase serves (a detail response only carries the first few
	// track objects, so the rest must come from here).
	const batch = 100
	const maxBatches = 60
	var tracks []playlistTrack
	for start, n := 0, 0; start < len(ids) && n < maxBatches; start, n = start+batch, n+1 {
		end := start + batch
		if end > len(ids) {
			end = len(ids)
		}
		query := make([]map[string]int, 0, end-start)
		for _, id := range ids[start:end] {
			query = append(query, map[string]int{"id": id, "v": 0})
		}
		encoded, err := json.Marshal(query)
		if err != nil {
			return nil, false, err
		}
		endpoint := origin + "/api/v3/song/detail?c=" + url.QueryEscape(string(encoded))
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, false, errors.New("网易云音乐地址无效")
		}
		body, err := a.playlistDo(request, spec.Name, cookie, bearer)
		if err != nil {
			return nil, false, err
		}
		var decoded struct {
			Code  *int  `json:"code"`
			Songs []any `json:"songs"`
		}
		if json.Unmarshal(body, &decoded) != nil {
			return nil, false, errors.New("网易云音乐歌曲详情无法解析")
		}
		if decoded.Code != nil && *decoded.Code != 200 {
			return nil, false, errors.New("网易云音乐歌曲详情未公开")
		}
		part := neteaseArtists(decoded.Songs)
		if len(part) == 0 {
			break
		}
		tracks = append(tracks, part...)
	}
	return tracks, len(tracks) < len(ids), nil
}

// fetchKugouList reads collection_ and gcid_ playlists. A share page that only
// exposes a short preview is marked incomplete and is not treated as the full list.
func (a *App) fetchKugouList(ctx context.Context, origin, id string) (remotePlaylist, error) {
	collectionID := id
	if strings.HasPrefix(id, "gcid_") {
		body, err := a.kugouGET(ctx, kugouShareOrigin(origin)+"/songlist/"+url.PathEscape(id)+"/")
		if err != nil {
			return remotePlaylist{}, err
		}
		payload := kugouEmbedded(body)
		if payload == nil {
			return remotePlaylist{}, errors.New("酷狗分享页没有可解析的完整清单")
		}
		var decoded map[string]any
		if json.Unmarshal(payload, &decoded) != nil {
			return remotePlaylist{}, errors.New("酷狗分享页没有可解析的完整清单")
		}
		collectionID = firstNonEmpty(textField(decoded["encode_gic"]), textField(decoded["global_collection_id"]))
		if info, _ := decoded["info"].(map[string]any); info != nil {
			if list, _ := info["listinfo"].(map[string]any); list != nil && collectionID == "" {
				collectionID = textField(list["global_collection_id"])
			}
		}
		if !strings.HasPrefix(collectionID, "collection_") {
			rows := kugouRows(decoded)
			if len(rows) == 0 {
				return remotePlaylist{}, errors.New("酷狗分享页没有可读曲目")
			}
			return remotePlaylist{Platform: "kugou", ID: id, Name: kugouName(decoded), Tracks: rows, Truncated: true}, nil
		}
	}
	tracks, name, truncated, err := a.kugouCollectionPages(ctx, origin, collectionID)
	if err != nil {
		return remotePlaylist{}, err
	}
	if len(tracks) == 0 {
		return remotePlaylist{}, errors.New("酷狗歌单没有可读曲目")
	}
	return remotePlaylist{Platform: "kugou", ID: id, Name: name, Tracks: tracks, Truncated: truncated}, nil
}

func (a *App) kugouCollectionPages(ctx context.Context, origin, id string) ([]playlistTrack, string, bool, error) {
	var tracks []playlistTrack
	name := "酷狗音乐"
	total := 0
	for page := 1; page <= 20; page++ {
		endpoint, err := kugouCollectionURL(kugouCollectionOrigin(origin), id, page)
		if err != nil {
			return nil, "", false, err
		}
		body, err := a.kugouGET(ctx, endpoint)
		if err != nil {
			return nil, "", false, err
		}
		part, partName, count, err := decodeKugouCollection(body)
		if err != nil {
			return nil, "", false, err
		}
		if partName != "" {
			name = partName
		}
		if count > 0 {
			total = count
		}
		if len(part) == 0 {
			break
		}
		tracks = append(tracks, part...)
		if total > 0 && len(tracks) >= total {
			return tracks, name, false, nil
		}
	}
	return tracks, name, total > len(tracks), nil
}

func kugouCollectionURL(origin, id string, page int) (string, error) {
	values := url.Values{}
	values.Set("srcappid", "2919")
	values.Set("clientver", "20000")
	stamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	values.Set("clienttime", stamp)
	values.Set("mid", stamp)
	values.Set("uuid", stamp)
	values.Set("dfid", "-")
	values.Set("uid", "0")
	values.Set("appid", "1058")
	values.Set("token", "")
	values.Set("type", "0")
	values.Set("module", "playlist")
	values.Set("page", strconv.Itoa(page))
	values.Set("pagesize", "100")
	values.Set("global_collection_id", id)
	// Public H5 client signing string from the old reader. It is not a user secret.
	values.Set("signature", kugouSignature(values))
	endpoint, err := url.Parse(strings.TrimRight(origin, "/") + "/v2/get_other_list_file")
	if err != nil {
		return "", errors.New("酷狗地址无效")
	}
	endpoint.RawQuery = values.Encode()
	return endpoint.String(), nil
}

func kugouSignature(values url.Values) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		if key == "signature" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var ordered strings.Builder
	for _, key := range keys {
		ordered.WriteString(key)
		ordered.WriteByte('=')
		ordered.WriteString(values.Get(key))
	}
	const material = "NVPh5oo715z5DIWAeQlhMDsWXXQV4hwt"
	sum := md5.Sum([]byte(material + ordered.String() + material))
	return hex.EncodeToString(sum[:])
}

func decodeKugouCollection(body []byte) ([]playlistTrack, string, int, error) {
	var decoded map[string]any
	if json.Unmarshal(body, &decoded) != nil {
		return nil, "", 0, errors.New("酷狗歌单无法解析")
	}
	if status := intField(decoded["status"]); decoded["status"] != nil && status != 1 {
		return nil, "", 0, errors.New("酷狗歌单未公开")
	}
	data, _ := decoded["data"].(map[string]any)
	if data == nil {
		data = decoded
	}
	rows := kugouRows(data)
	name := kugouName(data)
	total := intField(data["count"])
	if total == 0 {
		total = intField(decoded["count"])
	}
	return rows, name, total, nil
}

func kugouRows(decoded map[string]any) []playlistTrack {
	raw := decoded["info"]
	if info, _ := decoded["info"].(map[string]any); info != nil {
		if songs := info["songs"]; songs != nil {
			raw = songs
		}
	}
	if raw == nil {
		raw = decoded["songs"]
	}
	items, _ := raw.([]any)
	out := make([]playlistTrack, 0, len(items))
	for _, item := range items {
		row, _ := item.(map[string]any)
		if row == nil {
			continue
		}
		title := firstNonEmpty(textField(row["name"]), textField(row["songname"]))
		if title == "" {
			continue
		}
		artist := firstNonEmpty(textField(row["singername"]), joinNames(row["singerinfo"]))
		album := firstNonEmpty(textField(row["album_name"]), textField(row["albumname"]))
		duration := secondsText(row["duration"])
		if duration == "" {
			duration = secondsText(row["timelen"])
		}
		out = append(out, playlistTrack{Title: title, Artist: artist, Album: album, Duration: duration})
	}
	return out
}

func kugouName(decoded map[string]any) string {
	if name := textField(decoded["name"]); name != "" {
		return name
	}
	if info, _ := decoded["info"].(map[string]any); info != nil {
		if list, _ := info["listinfo"].(map[string]any); list != nil {
			if name := textField(list["name"]); name != "" {
				return name
			}
		}
	}
	return "酷狗音乐"
}

func kugouEmbedded(body []byte) []byte {
	text := string(body)
	if json.Valid(body) && (strings.HasPrefix(strings.TrimSpace(text), "{") || strings.HasPrefix(strings.TrimSpace(text), "[")) {
		return body
	}
	for _, marker := range []string{"window.$output =", "var data ="} {
		if raw := embeddedJSON(text, marker); raw != nil {
			return raw
		}
	}
	return nil
}

func (a *App) kugouGET(ctx context.Context, endpoint string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("酷狗地址无效")
	}
	return a.playlistDo(request, "酷狗音乐", "", "")
}

func kugouCollectionOrigin(origin string) string {
	if loopbackURL(origin) {
		return origin
	}
	return "https://pubsongscdn.kugou.com"
}

func kugouShareOrigin(origin string) string {
	if loopbackURL(origin) {
		return origin
	}
	return "https://m.kugou.com"
}

// fetchSpotifyEmbed reads the public embed the old client read when the user
// has no token. The result is a preview, not a confirmed complete playlist.
func (a *App) fetchSpotifyEmbed(ctx context.Context, origin, id string) (remotePlaylist, error) {
	endpoint := strings.TrimRight(spotifyEmbedOrigin(origin), "/") + "/embed/playlist/" + url.PathEscape(id)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return remotePlaylist{}, errors.New("Spotify 地址无效")
	}
	body, err := a.playlistDo(request, "Spotify", "", "")
	if err != nil {
		return remotePlaylist{}, err
	}
	raw := scriptJSON(string(body), "__NEXT_DATA__")
	if raw == nil {
		return remotePlaylist{}, errors.New("Spotify 公开嵌入没有可读清单")
	}
	var decoded map[string]any
	if json.Unmarshal(raw, &decoded) != nil {
		return remotePlaylist{}, errors.New("Spotify 公开嵌入没有可读清单")
	}
	entity := spotifyEntity(decoded)
	if entity == nil {
		return remotePlaylist{}, errors.New("Spotify 公开嵌入没有可读清单")
	}
	if uri := textField(entity["uri"]); uri != "" && uri != "spotify:playlist:"+id {
		return remotePlaylist{}, errors.New("Spotify 公开嵌入返回了其他歌单")
	}
	rows := spotifyEmbedRows(entity["trackList"])
	if len(rows) == 0 {
		return remotePlaylist{}, errors.New("Spotify 公开嵌入没有可读曲目")
	}
	name := textField(entity["name"])
	if name == "" {
		name = "Spotify"
	}
	return remotePlaylist{Platform: "spotify", ID: id, Name: name, Tracks: rows, Truncated: true, Preview: true}, nil
}

func spotifyEntity(decoded map[string]any) map[string]any {
	props, _ := decoded["props"].(map[string]any)
	page, _ := props["pageProps"].(map[string]any)
	state, _ := page["state"].(map[string]any)
	data, _ := state["data"].(map[string]any)
	entity, _ := data["entity"].(map[string]any)
	return entity
}

func spotifyEmbedRows(raw any) []playlistTrack {
	items, _ := raw.([]any)
	out := make([]playlistTrack, 0, len(items))
	for _, item := range items {
		row, _ := item.(map[string]any)
		if row == nil {
			continue
		}
		title := firstNonEmpty(textField(row["title"]), textField(row["name"]))
		if title == "" {
			continue
		}
		artist := textField(row["subtitle"])
		if artist == "" {
			artist = joinNames(row["artists"])
		}
		out = append(out, playlistTrack{Title: title, Artist: artist, Album: textField(row["album"]), Duration: millisField(row["duration_ms"])})
	}
	return out
}

func spotifyEmbedOrigin(origin string) string {
	if loopbackURL(origin) {
		return origin
	}
	return "https://open.spotify.com"
}

func embeddedJSON(text, marker string) []byte {
	rest := text
	if marker != "" {
		i := strings.Index(text, marker)
		if i < 0 {
			return nil
		}
		rest = text[i+len(marker):]
	}
	rest = strings.TrimSpace(rest)
	decoder := json.NewDecoder(strings.NewReader(rest))
	var raw json.RawMessage
	if decoder.Decode(&raw) != nil {
		return nil
	}
	return raw
}

func scriptJSON(text, id string) []byte {
	needle := `id="` + id + `"`
	i := strings.Index(text, needle)
	if i < 0 {
		return nil
	}
	rest := text[i:]
	gt := strings.Index(rest, ">")
	if gt < 0 {
		return nil
	}
	return embeddedJSON(rest[gt+1:], "")
}

func (a *App) playlistDo(request *http.Request, name, cookie, bearer string) ([]byte, error) {
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "MusicOps")
	if cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New(name + " 没有连上")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New(name + " 没有返回歌单")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, errors.New(name + " 响应不完整")
	}
	return body, nil
}

func (a *App) fetchPaged(ctx context.Context, spec remotePlatform, platform, origin, id, header, bearer string) (remotePlaylist, error) {
	var tracks []playlistTrack
	name := spec.Name
	truncated := false
	for page := 0; page < 20; page++ {
		payload, err := a.readPlaylistPage(ctx, spec, origin, id, page, header, bearer)
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
		truncated = page == 19
	}
	if len(tracks) == 0 {
		return remotePlaylist{}, errors.New(spec.Name + " 歌单没有可读曲目")
	}
	return remotePlaylist{Platform: platform, ID: id, Name: name, Tracks: tracks, Truncated: truncated}, nil
}
