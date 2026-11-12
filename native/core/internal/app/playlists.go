// Playlist import and the saved playlist library.
//
// Python's playlist flow (app/playlists.py, providers/playlist_input.py)
// accepts three inputs — a pasted link/share text, a track ID, or an
// imported file (absolute path, .json/.csv/.tsv/.txt/.m3u/.m3u8/.xlsx,
// at most 20MB and 50000 tracks) — and refresh_playlist turns it into
// target tracks. Uploaded M3U, JSON, CSV, TSV and TXT are normalized to
// M3U. The URI line is metadata-only and is never opened or fetched.
// Pasted platform links are recognized locally and are not requested.
//
// Two places hold playlists:
//
//   - per-job: incoming/playlist-<rootJobID>.m3u — exactly the file the
//     fallback step (acquireFallback) reads. POST /v1/jobs/{id}/playlist
//     writes it; GET reports whether it exists and how many tracks it
//     holds. POST /v1/search with playlist_id copies a saved playlist
//     there before the search starts, so the playlist scene wires into
//     the fallback.
//   - saved library: <state>/playlists/<slug>.m3u — POST /v1/playlists
//     saves one (upload or paste), GET /v1/playlists lists them with
//     track counts. A saved playlist is attached to a job with
//     {playlist_id} on the per-job endpoint.
//
// Platform playlist credentials are not stored: there is no reader that
// would call those platforms. Link text is recognized locally and the
// user imports an exported file for the tracks.
package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// maxPlaylistBytes is the per-file import cap, the Python MAX_BYTES of
// 20MB from playlist_input.py.
const maxPlaylistBytes = 20 * 1024 * 1024

var playlistSlugPattern = regexp.MustCompile(`^[a-z0-9_-]{1,80}$`)

// playlistFileName is the per-job playlist file the fallback step reads.
func playlistFileName(jobID string) string {
	return "playlist-" + jobID + ".m3u"
}

// validPlaylistJobID rejects job ids that could escape the incoming
// directory when used as a file name.
func validPlaylistJobID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\")
}

// playlistSlug derives a file slug from a display name: lowercase,
// non-alphanumerics become "-", runs collapse, trimmed to 80 chars.
func playlistSlug(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	lastDash := true
	for _, r := range lower {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 80 {
		slug = strings.Trim(slug[:80], "-")
	}
	return slug
}

// readPlaylistUpload reads playlist content from either a multipart file
// part named "file" or a JSON body {content}. The content is capped at
// 20MB. It returns the content and the suggested name (the uploaded file
// name without extension, or the JSON name field).
func readPlaylistUpload(r *http.Request) ([]byte, string, string, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return nil, "", "", errors.New("无法解析上传表单")
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			return nil, "", "", errors.New("请上传一个歌单文件")
		}
		defer file.Close()
		name := strings.TrimSuffix(header.Filename, filepath.Ext(header.Filename))
		if field := strings.TrimSpace(r.FormValue("name")); field != "" {
			name = field
		}
		ext := strings.ToLower(filepath.Ext(header.Filename))
		switch ext {
		case ".m3u", ".m3u8", ".json", ".csv", ".tsv", ".txt", ".xlsx":
		default:
			return nil, "", "", errors.New("歌单文件须为 .m3u、.m3u8、.json、.csv、.tsv、.txt 或 .xlsx")
		}
		data, err := io.ReadAll(io.LimitReader(file, maxPlaylistBytes+1))
		if err != nil {
			return nil, "", "", err
		}
		if len(data) > maxPlaylistBytes {
			return nil, "", "", errors.New("单个歌单文件最多 20MB")
		}
		return data, name, strings.TrimSpace(r.FormValue("text_order")), nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxPlaylistBytes+1))
	if err != nil {
		return nil, "", "", err
	}
	if len(raw) > maxPlaylistBytes {
		return nil, "", "", errors.New("单个歌单最多 20MB")
	}
	var body struct {
		Content    string `json:"content"`
		Name       string `json:"name"`
		PlaylistID string `json:"playlist_id"`
		TextOrder  string `json:"text_order"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, "", "", errors.New("请求体须为歌单内容 JSON 或 multipart 文件上传")
	}
	if body.PlaylistID != "" {
		return nil, "", "", errAttachPlaylist{ID: body.PlaylistID}
	}
	if strings.TrimSpace(body.Content) == "" {
		return nil, "", "", errors.New("歌单内容为空")
	}
	return []byte(body.Content), body.Name, body.TextOrder, nil
}

// errAttachPlaylist signals that the caller asked to attach a saved
// playlist instead of uploading content.
type errAttachPlaylist struct{ ID string }

func (e errAttachPlaylist) Error() string { return "attach playlist " + e.ID }

// savedPlaylistsDir is the directory holding saved playlists.
func (a *App) savedPlaylistsDir() string {
	return filepath.Join(a.Dir, "playlists")
}

// attachSavedPlaylist copies a saved playlist to the per-job file the
// fallback step reads. It returns the track count and playlist name.
func (a *App) attachSavedPlaylist(jobID, playlistID string) (int, string, error) {
	if !validPlaylistJobID(jobID) || !playlistSlugPattern.MatchString(playlistID) {
		return 0, "", errors.New("歌单 ID 无效")
	}
	source := filepath.Join(a.savedPlaylistsDir(), playlistID+".m3u")
	data, err := os.ReadFile(source)
	if err != nil {
		return 0, "", errors.New("找不到该歌单")
	}
	tracks, name, err := parseM3U(data)
	if err != nil {
		return 0, "", errors.New("已存歌单无法解析: " + err.Error())
	}
	incoming := a.workPath("incoming")
	if err := os.MkdirAll(incoming, 0o755); err != nil {
		return 0, "", err
	}
	if err := os.WriteFile(filepath.Join(incoming, playlistFileName(jobID)), data, 0o644); err != nil {
		return 0, "", err
	}
	return len(tracks), name, nil
}

// jobPlaylistHandler reports the per-job playlist file: whether it
// exists, its track count and its #PLAYLIST name.
func (a *App) jobPlaylistHandler(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	if !validPlaylistJobID(jobID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	path := a.workPath("incoming", playlistFileName(jobID))
	data, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "playlist": map[string]any{"exists": false, "track_count": 0, "name": ""}})
		return
	}
	tracks, name, err := parseM3U(data)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "playlist": map[string]any{"exists": true, "track_count": 0, "name": "", "error": err.Error()}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "playlist": map[string]any{"exists": true, "track_count": len(tracks), "name": name}})
}

// saveJobPlaylist writes the per-job playlist file from an upload, a
// paste, or a saved playlist attach ({playlist_id}).
func (a *App) saveJobPlaylist(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	if !validPlaylistJobID(jobID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	data, name, textOrder, err := readPlaylistUpload(r)
	if err != nil {
		var attach errAttachPlaylist
		if errors.As(err, &attach) {
			count, playlistName, err := a.attachSavedPlaylist(jobID, attach.ID)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "playlist": map[string]any{"exists": true, "track_count": count, "name": playlistName, "playlist_id": attach.ID}})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": err.Error()})
		return
	}
	tracks, playlistName, data, err := normalizePlaylist(data, name, textOrder)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": err.Error()})
		return
	}
	if name == "" {
		name = playlistName
	}
	incoming := a.workPath("incoming")
	if err := os.MkdirAll(incoming, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "PLAYLIST_UNAVAILABLE"})
		return
	}
	if err := os.WriteFile(filepath.Join(incoming, playlistFileName(jobID)), data, 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "PLAYLIST_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "playlist": map[string]any{"exists": true, "track_count": len(tracks), "name": name}})
}

// listPlaylists lists the saved playlists with their track counts. A
// file that no longer parses is listed with its error, never silently
// dropped.
func (a *App) listPlaylists(w http.ResponseWriter, r *http.Request) {
	dir := a.savedPlaylistsDir()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "PLAYLIST_UNAVAILABLE"})
		return
	}
	items := []map[string]any{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".m3u") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".m3u")
		item := map[string]any{"id": id, "track_count": 0, "name": ""}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			item["error"] = err.Error()
			items = append(items, item)
			continue
		}
		tracks, name, err := parseM3U(data)
		if err != nil {
			item["error"] = err.Error()
		} else {
			item["track_count"] = len(tracks)
			item["name"] = name
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "playlists": items, "count": len(items)})
}

// savePlaylist saves one playlist to the library from an upload or a
// paste. The slug comes from the name (uploaded file name or JSON name
// field); re-uploading the same slug replaces the saved file.
func (a *App) savePlaylist(w http.ResponseWriter, r *http.Request) {
	data, name, textOrder, err := readPlaylistUpload(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": err.Error()})
		return
	}
	tracks, playlistName, data, err := normalizePlaylist(data, name, textOrder)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": err.Error()})
		return
	}
	if name == "" {
		name = playlistName
	}
	if name == "" {
		name = "playlist"
	}
	slug := playlistSlug(name)
	if slug == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "歌单名称无法用作文件名"})
		return
	}
	dir := a.savedPlaylistsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "PLAYLIST_UNAVAILABLE"})
		return
	}
	if err := os.WriteFile(filepath.Join(dir, slug+".m3u"), data, 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "PLAYLIST_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "playlist": map[string]any{"id": slug, "track_count": len(tracks), "name": name}})
}

// attachPlaylistForSearch copies a saved playlist into the per-job file
// before a search starts, so the playlist scene wires into the fallback.
// The file is removed again when the search fails to start.
func (a *App) attachPlaylistForSearch(jobID, playlistID string) error {
	if playlistID == "" {
		return nil
	}
	_, _, err := a.attachSavedPlaylist(jobID, playlistID)
	return err
}
