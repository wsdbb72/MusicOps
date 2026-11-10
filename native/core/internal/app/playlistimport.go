package app

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Playlist documents other than M3U are normalized to M3U before they are
// stored. The URI line written here is a placeholder and is never opened.
// Track URLs inside an imported file are not fetched.

const maxPlaylistField = 10000

var titleAliases = []string{"title", "name", "songname", "trackname", "song name", "track title", "歌曲名", "歌曲名称", "歌名", "曲名", "歌曲", "曲目名称"}
var artistAliases = []string{"artists", "artist", "artist name", "singer", "singername", "歌手", "歌手名", "艺人", "演唱者"}
var albumAliases = []string{"album", "albumname", "album name", "专辑", "专辑名", "专辑名称"}

// normalizePlaylist turns M3U, JSON, CSV, TSV or TXT into the M3U the
// rest of the app stores. textOrder is artist-title or title-artist and
// is required only for a TXT file that does not declare its own order.
func normalizePlaylist(data []byte, name, textOrder string) ([]playlistTrack, string, []byte, error) {
	tracks, parsedName, err := parsePlaylistDocument(data, textOrder)
	if err != nil {
		return nil, "", nil, err
	}
	if name == "" {
		name = parsedName
	}
	if len(tracks) == 0 {
		return nil, "", nil, errors.New("歌单里没有曲目")
	}
	return tracks, name, encodeM3U(name, tracks), nil
}

func parsePlaylistDocument(data []byte, textOrder string) ([]playlistTrack, string, error) {
	if !utf8.Valid(data) {
		return nil, "", errors.New("歌单须为 UTF-8 文本")
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, "", errors.New("歌单内容为空")
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		return parseJSONPlaylist(trimmed)
	}
	text := string(trimmed)
	if strings.Contains(text, "#EXTM3U") || strings.Contains(text, "#EXTINF:") {
		return parseM3U(data)
	}
	if looksLikeTable(text) {
		return parseTablePlaylist(text)
	}
	return parseTextPlaylist(text, textOrder)
}

func looksLikeTable(text string) bool {
	line, _, _ := strings.Cut(text, "\n")
	if strings.Contains(line, "\t") || strings.Count(line, ",") >= 1 {
		header, err := splitTableLine(line)
		if err != nil || len(header) < 2 {
			return false
		}
		for _, cell := range header {
			if aliasKind(cell) == "title" {
				return true
			}
		}
	}
	return false
}

func parseJSONPlaylist(data []byte) ([]playlistTrack, string, error) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, "", errors.New("JSON 歌单无法解析")
	}
	name := ""
	for range 6 {
		switch node := value.(type) {
		case []any:
			return jsonTracks(node, name)
		case map[string]any:
			if text, ok := stringField(node, "name", "title", "歌单名称"); ok && name == "" {
				name = text
			}
			if next, ok := nestedList(node, "tracks", "songs", "songlist", "songList", "items"); ok {
				value = next
				continue
			}
			if cds, ok := node["cdlist"].([]any); ok {
				if len(cds) != 1 {
					return nil, "", errors.New("文件包含多个歌单，请逐个导出再导入")
				}
				value = cds[0]
				continue
			}
			moved := false
			for _, key := range []string{"playlist", "result", "data"} {
				if next, ok := node[key]; ok {
					value = next
					moved = true
					break
				}
			}
			if moved {
				continue
			}
			return nil, "", errors.New("JSON 没有已识别的 tracks / songs / songlist 数组")
		default:
			return nil, "", errors.New("JSON 没有已识别的 tracks / songs / songlist 数组")
		}
	}
	return nil, "", errors.New("JSON 没有已识别的 tracks / songs / songlist 数组")
}

func nestedList(node map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		next, ok := node[key]
		if !ok {
			continue
		}
		switch next.(type) {
		case []any, map[string]any:
			return next, true
		}
	}
	return nil, false
}

func jsonTracks(rows []any, name string) ([]playlistTrack, string, error) {
	if len(rows) > maxPlaylistTracks {
		return nil, "", errors.New("单个文件最多导入 50000 首")
	}
	tracks := make([]playlistTrack, 0, len(rows))
	for _, row := range rows {
		obj, ok := row.(map[string]any)
		if !ok {
			return nil, "", errors.New("JSON 曲目须为对象")
		}
		title, ok := stringField(obj, titleAliases...)
		if !ok || title == "" {
			return nil, "", errors.New("JSON 曲目缺少歌名")
		}
		artist, _ := stringField(obj, artistAliases...)
		album, _ := stringField(obj, albumAliases...)
		track, err := cleanTrack(artist, title, album)
		if err != nil {
			return nil, "", err
		}
		tracks = append(tracks, track)
	}
	return tracks, name, nil
}

func stringField(obj map[string]any, keys ...string) (string, bool) {
	folded := map[string]any{}
	for key, value := range obj {
		folded[foldKey(key)] = value
	}
	for _, key := range keys {
		value, ok := folded[foldKey(key)]
		if !ok || value == nil {
			continue
		}
		switch typed := value.(type) {
		case string:
			return strings.TrimSpace(typed), true
		case []any:
			parts := []string{}
			for _, item := range typed {
				text, ok := item.(string)
				if !ok {
					return "", false
				}
				if strings.TrimSpace(text) != "" {
					parts = append(parts, strings.TrimSpace(text))
				}
			}
			return strings.Join(parts, ", "), true
		default:
			return "", false
		}
	}
	return "", false
}

func parseTablePlaylist(text string) ([]playlistTrack, string, error) {
	comma := strings.Count(text, ",") >= strings.Count(text, "\t")
	reader := csv.NewReader(strings.NewReader(text))
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = false
	if !comma {
		reader.Comma = '\t'
	}
	rows, err := reader.ReadAll()
	if err != nil {
		return nil, "", errors.New("表格无法解析，请检查引号与分隔符")
	}
	if len(rows) == 0 {
		return nil, "", errors.New("表格没有列名")
	}
	header := rows[0]
	if len(header) > 128 {
		return nil, "", errors.New("表格列数过多")
	}
	seen := map[string]bool{}
	titleCol, artistCol, albumCol := -1, -1, -1
	for i, cell := range header {
		key := foldKey(cell)
		if key == "" || seen[key] {
			return nil, "", errors.New("表格列名重复或为空")
		}
		seen[key] = true
		switch aliasKind(cell) {
		case "title":
			if titleCol < 0 {
				titleCol = i
			}
		case "artist":
			if artistCol < 0 {
				artistCol = i
			}
		case "album":
			if albumCol < 0 {
				albumCol = i
			}
		}
	}
	if titleCol < 0 {
		return nil, "", errors.New("无法识别歌名列，请使用 title / 歌名 等明确列名")
	}
	tracks := []playlistTrack{}
	for _, row := range rows[1:] {
		if len(row) > len(header) {
			return nil, "", errors.New("表格行比列名多")
		}
		blank := true
		for _, cell := range row {
			if strings.TrimSpace(cell) != "" {
				blank = false
				break
			}
		}
		if blank {
			continue
		}
		title := cellAt(row, titleCol)
		artist := cellAt(row, artistCol)
		album := cellAt(row, albumCol)
		track, err := cleanTrack(artist, title, album)
		if err != nil {
			return nil, "", err
		}
		tracks = append(tracks, track)
		if len(tracks) > maxPlaylistTracks {
			return nil, "", errors.New("单个文件最多导入 50000 首")
		}
	}
	return tracks, "", nil
}

func cellAt(row []string, index int) string {
	if index < 0 || index >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[index])
}

func splitTableLine(line string) ([]string, error) {
	comma := strings.Count(line, ",") >= strings.Count(line, "\t")
	reader := csv.NewReader(strings.NewReader(line))
	if !comma {
		reader.Comma = '\t'
	}
	return reader.Read()
}

func parseTextPlaylist(text, textOrder string) ([]playlistTrack, string, error) {
	name := ""
	lines := strings.Split(text, "\n")
	for _, raw := range lines {
		value := strings.TrimSpace(raw)
		if !strings.HasPrefix(value, "#") {
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(value, "#"))
		key, item, ok := strings.Cut(body, ":")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "order", "format":
			item = strings.TrimSpace(item)
			if item != "artist-title" && item != "title-artist" {
				return nil, "", errors.New("文本 order 注释无效")
			}
			if textOrder != "" && textOrder != item {
				return nil, "", errors.New("所选文本顺序与文件 order 注释不一致")
			}
			textOrder = item
		case "playlist":
			name = strings.TrimSpace(item)
		}
	}
	if textOrder != "artist-title" && textOrder != "title-artist" {
		return nil, "", errors.New("TXT 歌名与歌手顺序不明确，请选择“歌手 - 歌名”或“歌名 - 歌手”")
	}
	tracks := []playlistTrack{}
	for number, raw := range lines {
		value := strings.TrimSpace(raw)
		if value == "" || strings.HasPrefix(value, "#") {
			continue
		}
		parts := strings.Split(value, " - ")
		if (len(parts) != 2 && len(parts) != 3) || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return nil, "", fmt.Errorf("第 %d 行需要明确的“歌手 - 歌名”或“歌名 - 歌手”", number+1)
		}
		artist, title := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if textOrder == "title-artist" {
			artist, title = title, artist
		}
		album := ""
		if len(parts) == 3 {
			album = strings.TrimSpace(parts[2])
			if album == "" {
				return nil, "", fmt.Errorf("第 %d 行的专辑名为空", number+1)
			}
		}
		track, err := cleanTrack(artist, title, album)
		if err != nil {
			return nil, "", err
		}
		tracks = append(tracks, track)
		if len(tracks) > maxPlaylistTracks {
			return nil, "", errors.New("单个文件最多导入 50000 首")
		}
	}
	return tracks, name, nil
}

func cleanTrack(artist, title, album string) (playlistTrack, error) {
	for _, field := range []struct{ name, value string }{{"歌名", title}, {"艺人", artist}, {"专辑", album}} {
		if len(field.value) > maxPlaylistField {
			return playlistTrack{}, errors.New(field.name + "过长")
		}
		if strings.ContainsAny(field.value, "\r\n") {
			return playlistTrack{}, errors.New(field.name + "不能换行")
		}
	}
	if strings.TrimSpace(title) == "" {
		return playlistTrack{}, errors.New("歌名为空")
	}
	return playlistTrack{Artist: strings.TrimSpace(artist), Title: strings.TrimSpace(title), Album: strings.TrimSpace(album), Duration: "0"}, nil
}

func aliasKind(cell string) string {
	key := foldKey(cell)
	for _, alias := range titleAliases {
		if key == foldKey(alias) {
			return "title"
		}
	}
	for _, alias := range artistAliases {
		if key == foldKey(alias) {
			return "artist"
		}
	}
	for _, alias := range albumAliases {
		if key == foldKey(alias) {
			return "album"
		}
	}
	return ""
}

func foldKey(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch r {
		case ' ', '_', '-', '(', ')', '（', '）', '[', ']', '.', ':', '/':
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func encodeM3U(name string, tracks []playlistTrack) []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	if strings.TrimSpace(name) != "" {
		b.WriteString("#PLAYLIST:" + oneLine(name) + "\n")
	}
	for _, track := range tracks {
		if track.Album != "" {
			b.WriteString("#MUSICOPS-ALBUM:" + oneLine(track.Album) + "\n")
		}
		duration := track.Duration
		if duration == "" {
			duration = "0"
		}
		description := oneLine(track.Title)
		if track.Artist != "" {
			description = oneLine(track.Artist) + " - " + description
		}
		b.WriteString("#EXTINF:" + oneLine(duration) + "," + description + "\n")
		b.WriteString(".\n")
	}
	return []byte(b.String())
}

func oneLine(value string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(strings.TrimSpace(value))
}
