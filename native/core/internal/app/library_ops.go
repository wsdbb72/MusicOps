package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/musicbrainz"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

var audioExtensions = map[string]bool{"flac": true, "mp3": true, "wav": true, "aac": true, "alac": true, "aiff": true, "m4a": true, "ogg": true}

func (a *App) reindexLibrary(w http.ResponseWriter, _ *http.Request) {
	root := a.libraryDir()
	added, kept, err := a.indexLibraryTree(root)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE", "error": "没有可索引的音乐库目录"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "added": added, "kept": kept, "deleted_files": 0, "note": "只补充索引。不删除磁盘上的文件，也不把缺少的文件说成已下载。"})
}

func (a *App) indexLibraryTree(root string) (added, kept int, err error) {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return 0, 0, err
	}
	existing, err := audiorelease.List(a.engine)
	if err != nil {
		return 0, 0, err
	}
	seen := map[string]bool{}
	for _, entry := range existing {
		seen[entry.RequestID] = true
	}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return nil
		}
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
		if !audioExtensions[ext] {
			return nil
		}
		abs, err := filepath.Abs(path)
		if err != nil || abs != filepath.Clean(abs) {
			return nil
		}
		track := trackNumberFromName(filepath.Base(path))
		requestID := libraryFileID(abs)
		if seen[requestID] {
			kept++
			return nil
		}
		ok, err := audiorelease.IndexExisting(a.engine, audiorelease.Existing{
			RequestID: requestID, RootID: libraryRootID(filepath.Dir(abs)), Track: track,
			Path: abs, Library: root, Format: ext, SourceName: "reindex",
		})
		if err != nil {
			return err
		}
		if ok {
			added++
		} else {
			kept++
		}
		return nil
	})
	return added, kept, err
}

func (a *App) libraryGaps(w http.ResponseWriter, _ *http.Request) {
	entries, err := audiorelease.List(a.engine)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	type album struct {
		Root    string `json:"root_id"`
		Tracks  []int  `json:"tracks"`
		Missing []int  `json:"missing"`
	}
	byRoot := map[string]map[int]bool{}
	for _, entry := range entries {
		if byRoot[entry.RootID] == nil {
			byRoot[entry.RootID] = map[int]bool{}
		}
		if entry.Track > 0 {
			byRoot[entry.RootID][entry.Track] = true
		}
	}
	var albums []album
	for root, tracks := range byRoot {
		maxTrack := 0
		var present []int
		for track := range tracks {
			present = append(present, track)
			if track > maxTrack {
				maxTrack = track
			}
		}
		sort.Ints(present)
		var missing []int
		for track := 1; track <= maxTrack; track++ {
			if !tracks[track] {
				missing = append(missing, track)
			}
		}
		if len(missing) > 0 {
			albums = append(albums, album{Root: root, Tracks: present, Missing: missing})
		}
	}
	sort.Slice(albums, func(i, j int) bool { return albums[i].Root < albums[j].Root })
	if albums == nil {
		albums = []album{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "albums": albums, "note": "这里只列出编号缺口，还没有下载。"})
}

func (a *App) libraryWorkflow(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Scene string `json:"scene"`
		Root  string `json:"root_id"`
		Label string `json:"label"`
		Path  string `json:"path"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	switch body.Scene {
	case "fill", "upgrade", "expand":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	label := strings.TrimSpace(body.Label)
	if label == "" {
		label = body.Scene + " " + firstNonEmpty(body.Root, body.Path, "本地专辑")
	}
	if len(label) > 500 {
		label = label[:500]
	}
	id := libraryFileID(body.Scene + "|" + label + "|" + strconv.FormatInt(a.now().UnixNano(), 10))
	_, err := a.flows.Start(id, workflow.Start{
		JobID: id, BatchID: id, Scene: body.Scene, Stage: "snapshotting", Label: label, NowNS: a.now().UnixNano(),
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "没有创建任务，也没有下载"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "job_id": id, "scene": body.Scene, "downloaded": false, "note": "已创建任务。尚未下载，尚未入库。"})
}

func (a *App) completeMetadata(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path           string `json:"path"`
		Artist         string `json:"artist"`
		Album          string `json:"album"`
		Title          string `json:"title"`
		CatalogBaseURL string `json:"catalog_base_url"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	path, err := a.resolveLibraryPath(body.Path)
	if err != nil || !strings.EqualFold(filepath.Ext(path), ".flac") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	before, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	client := musicbrainz.NewClient()
	if strings.TrimSpace(body.CatalogBaseURL) != "" {
		if !loopbackURL(body.CatalogBaseURL) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
			return
		}
		client.BaseURL = body.CatalogBaseURL
	}
	duration := 200.0
	lookup, err := identifyCatalogLookup(client, a.engine, catalogIdentifyRequest{
		Artist: body.Artist, Album: body.Album, Title: body.Title,
		Tracks: []catalogTrackRequest{{Title: body.Title, Artists: []string{body.Artist}, Duration: &duration}},
	}, catalogBudget, catalogTTL)
	if err != nil {
		_ = os.WriteFile(path, before, 0o600)
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "CATALOG_UNAVAILABLE", "written": false})
		return
	}
	confirmed := false
	for _, result := range lookup.Results {
		if result.IdentityConfirmed && result.Status == "confirmed" {
			confirmed = true
		}
	}
	if !confirmed {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "written": false, "note": "目录没有确认这首曲目，标签和文件都没有改。"})
		return
	}
	updates := map[string][]string{"ARTIST": {body.Artist}, "ALBUM": {body.Album}, "TITLE": {body.Title}}
	if err = writeFLACTags(path, updates); err != nil {
		_ = os.WriteFile(path, before, 0o600)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "code": "TAG_WRITE_FAILED", "written": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "written": true, "path": path, "note": "已按确认到的曲目写入标签。这不是下载。"})
}

func (a *App) compilationPreview(w http.ResponseWriter, _ *http.Request) {
	moves, err := a.compilationMoves(false)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "LIBRARY_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "moves": moves, "applied": false, "note": "只预览，没有移动文件。"})
}

func (a *App) compilationMerge(w http.ResponseWriter, _ *http.Request) {
	moves, err := a.compilationMoves(true)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "MERGE_FAILED", "error": "合并没有完成，已移动的文件记在回执里"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "moves": moves, "applied": true})
}

type compilationMove struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func (a *App) compilationMoves(apply bool) ([]compilationMove, error) {
	root := a.libraryDir()
	var moves []compilationMove
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return nil
		}
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
		if !audioExtensions[ext] {
			return nil
		}
		parent := filepath.Base(filepath.Dir(path))
		if !strings.Contains(parent, "合辑") && !strings.EqualFold(parent, "Various Artists") && !strings.EqualFold(parent, "VA") {
			return nil
		}
		if strings.Contains(path, string(filepath.Separator)+"Compilations"+string(filepath.Separator)) {
			return nil
		}
		dest := filepath.Join(root, "Compilations", parent, filepath.Base(path))
		if dest == path {
			return nil
		}
		moves = append(moves, compilationMove{From: path, To: dest})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !apply {
		if moves == nil {
			moves = []compilationMove{}
		}
		return moves, nil
	}
	for i, move := range moves {
		if err := os.MkdirAll(filepath.Dir(move.To), 0o700); err != nil {
			return moves[:i], err
		}
		if _, err := os.Stat(move.To); err == nil {
			return moves[:i], os.ErrExist
		}
		if err := os.Rename(move.From, move.To); err != nil {
			return moves[:i], err
		}
	}
	if moves == nil {
		moves = []compilationMove{}
	}
	encoded, _ := json.Marshal(map[string]any{"moves": moves, "at": time.Now().UTC().Format(time.RFC3339)})
	_ = a.casSaveJSON([]byte("app/library/compilation-last"), encoded)
	return moves, nil
}

func libraryFileID(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "lib-" + hex.EncodeToString(sum[:8])
}

func libraryRootID(dir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(dir)))
	return "alb-" + hex.EncodeToString(sum[:8])
}

func trackNumberFromName(name string) int {
	digits := 0
	value := 0
	for _, r := range name {
		if r < '0' || r > '9' {
			break
		}
		digits++
		value = value*10 + int(r-'0')
		if digits > 3 {
			return 1
		}
	}
	if value < 1 || value > 999 {
		return 1
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func loopbackURL(raw string) bool {
	lower := strings.ToLower(raw)
	return strings.HasPrefix(lower, "http://127.0.0.1") || strings.HasPrefix(lower, "http://localhost") || strings.HasPrefix(lower, "http://[::1]")
}
