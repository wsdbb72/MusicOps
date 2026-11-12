package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
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
		if ext == "wav" {
			path = a.maybeConvertIndexedWAV(path)
			ext = strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
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
		Scene     string   `json:"scene"`
		Root      string   `json:"root_id"`
		Label     string   `json:"label"`
		Path      string   `json:"path"`
		Paths     []string `json:"paths"`
		Confirmed bool     `json:"confirmed"`
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
	rawPaths := []string{}
	if strings.TrimSpace(body.Path) != "" {
		rawPaths = append(rawPaths, body.Path)
	}
	rawPaths = append(rawPaths, body.Paths...)
	chosen := []string{}
	seen := map[string]bool{}
	for _, raw := range rawPaths {
		raw = strings.TrimSpace(raw)
		if raw == "" || seen[raw] {
			continue
		}
		seen[raw] = true
		resolved, err := a.resolveLibraryPath(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "请选择音乐库里的文件"})
			return
		}
		info, statErr := os.Stat(resolved)
		if statErr != nil || info.IsDir() {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "本地文件不存在"})
			return
		}
		chosen = append(chosen, resolved)
	}
	if !body.Confirmed {
		files, total, offset := a.libraryFilePage(0, 20)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "started": false, "downloaded": false, "imported": false,
			"files": files, "total": total, "offset": offset, "paths": chosen,
			"note": "请逐页勾选曲目并确认。这一步尚未下载、尚未入库。",
		})
		return
	}
	if len(chosen) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "请先逐首确认，再进入下载", "downloaded": false})
		return
	}
	label := strings.TrimSpace(body.Label)
	if label == "" {
		label = body.Scene + " " + firstNonEmpty(body.Root, chosen[0], "本地专辑")
	}
	for _, path := range chosen {
		if !strings.Contains(label, path) {
			label = label + " " + path
		}
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
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "started": true, "job_id": id, "scene": body.Scene, "path": chosen[0], "paths": chosen,
		"downloaded": false, "imported": false,
		"note": "已逐首确认并进入检索。下载仍受令牌规则约束，这一步尚未下载、尚未入库。",
	})
}

func (a *App) completeMetadata(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path           string `json:"path"`
		Artist         string `json:"artist"`
		Album          string `json:"album"`
		Title          string `json:"title"`
		CatalogBaseURL string `json:"catalog_base_url"`
		LyricsBaseURL  string `json:"lyrics_base_url"`
		CoverBaseURL   string `json:"cover_base_url"`
		CoverMode      string `json:"cover_mode"`
		// Overwrite lets the caller replace a field whose current value
		// differs from the catalogue. Without it a difference is reported as
		// a conflict and the curated value stays (F10: 冲突默认不覆盖).
		// Genre follows the same rule, so a file that already says
		// "Chinese Rock" is not silently rewritten to "Rock".
		Overwrite bool `json:"overwrite"`
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
	// The file supplies what the caller left empty and the real duration the
	// catalogue comparison needs. A hardcoded duration made the 3-second
	// match fail for every real track (P1-15).
	current, err := flactags.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "code": "TAG_READ_FAILED", "written": false, "error": err.Error()})
		return
	}
	duration, ok := flacDuration(current.StreamInfo)
	if !ok {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "code": "AUDIO_UNREADABLE", "written": false, "note": "读不到音频时长，无法与目录核对；文件没有改。"})
		return
	}
	artist := strings.TrimSpace(body.Artist)
	if artist == "" {
		artist = strings.TrimSpace(firstNonEmpty(current.Artist, strings.Join(current.Artists, "")))
	}
	album := strings.TrimSpace(body.Album)
	if album == "" {
		album = strings.TrimSpace(current.Album)
	}
	title := strings.TrimSpace(body.Title)
	if title == "" {
		title = strings.TrimSpace(current.Title)
	}
	if artist == "" || album == "" || title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "written": false, "error": "艺人、专辑、曲名至少要从文件标签或请求里得到一项"})
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
	lookup, err := identifyCatalogLookup(client, a.engine, catalogIdentifyRequest{
		Artist: artist, Album: album, Title: title,
		Tracks: []catalogTrackRequest{{Title: title, Artists: []string{artist}, Duration: &duration}},
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
	view := lookup.MusicBrainz
	if view == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "written": false, "note": "目录只返回了检索摘要，没有可写的发行详情；标签和文件都没有改。"})
		return
	}
	track, found := matchCatalogTrack(view, title, duration)
	if !found {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "written": false, "note": "目录确认了发行，但没有找到对应曲目；标签和文件都没有改。"})
		return
	}
	updates, completion := planTagWrite(current, desiredTags(view, track), body.Overwrite)
	lyrics, cover, err := a.fetchArtwork(body.LyricsBaseURL, body.CoverBaseURL, artist, album, title)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "ARTWORK_UNAVAILABLE", "written": false, "note": "歌词或封面没有读到，文件没有改。"})
		return
	}
	if lyrics != "" {
		if len(current.Raw["LYRICS"]) == 0 || body.Overwrite {
			updates["LYRICS"] = []string{lyrics}
			completion.Written = append(completion.Written, "lyrics")
		} else {
			completion.Kept = append(completion.Kept, tagConflict{Field: "lyrics", Current: "已有歌词", Catalog: "目录歌词"})
		}
	}
	// Nothing to write and no cover to add: leave the file byte-identical.
	if len(updates) == 0 && cover == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "written": false, "path": path,
			"written_fields": []string{}, "kept_fields": completion.Kept, "unchanged_fields": completion.Unchanged,
			"catalog": catalogTagSummary(view), "note": "标签已经和目录一致，没有改动文件。",
		})
		return
	}
	backup, err := backupFile(path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "TAG_UNAVAILABLE", "written": false})
		return
	}
	// A cover-only completion must not rewrite the comment block.
	if len(updates) > 0 {
		if err = writeFLACTags(path, updates); err != nil {
			_ = os.WriteFile(path, before, 0o600)
			_ = os.Remove(backup)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "code": "TAG_WRITE_FAILED", "written": false, "restored": true})
			return
		}
	}
	settings, _ := a.loadLyricsSettings()
	mode := coverWriteMode(body.CoverMode, settings)
	sidecar := strings.TrimSuffix(path, filepath.Ext(path)) + ".jpg"
	if cover != nil {
		updated, readErr := os.ReadFile(path)
		var embedded []byte
		if readErr == nil {
			embedded, err = flactags.EmbedFrontCover(updated, cover)
		}
		if readErr != nil || err != nil || os.WriteFile(path, embedded, 0o600) != nil || !bytes.Equal(flactags.FrontCover(embedded), cover) {
			restoreBackup(path, backup)
			_ = os.Remove(sidecar)
			_ = os.Remove(backup)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "code": "COVER_WRITE_FAILED", "written": false, "restored": true})
			return
		}
		if mode == "sidecar" {
			if err = os.WriteFile(sidecar, cover, 0o600); err != nil {
				restoreBackup(path, backup)
				_ = os.Remove(sidecar)
				_ = os.Remove(backup)
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "code": "COVER_WRITE_FAILED", "written": false, "restored": true})
				return
			}
		} else {
			_ = os.Remove(sidecar)
		}
	}
	_ = os.Remove(backup)
	written := []string{}
	if len(completion.Written) > 0 {
		written = append(written, "标签 "+strings.Join(completion.Written, "、"))
	}
	if cover != nil {
		written = append(written, "内嵌封面")
		if mode == "sidecar" {
			written = append(written, "同名 jpg")
		}
	}
	note := "已写入 " + strings.Join(written, "、")
	if len(completion.Kept) > 0 {
		note += "；有 " + strconv.Itoa(len(completion.Kept)) + " 个字段与目录不同，已保留原值（勾选覆盖后重试）"
	}
	for _, kept := range completion.Kept {
		if kept.Field == "genre" {
			note += "。分类保留为「" + kept.Current + "」，目录建议「" + kept.Catalog + "」"
		}
	}
	note += "。写之前有备份，失败会恢复原文件。这不是下载。"
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "written": true, "path": path, "lyrics": lyrics != "", "cover": cover != nil,
		"cover_mode": mode, "embedded_cover": cover != nil,
		"written_fields": completion.Written, "kept_fields": completion.Kept, "unchanged_fields": completion.Unchanged,
		"catalog": catalogTagSummary(view), "note": note,
	})
}

// flacDuration derives the exact duration from STREAMINFO. FLAC always carries
// both fields, so a zero means the file is not a usable FLAC.
func flacDuration(info flactags.StreamInfo) (float64, bool) {
	if info.SampleRate == 0 || info.TotalSamples == 0 {
		return 0, false
	}
	return float64(info.TotalSamples) / float64(info.SampleRate), true
}

// matchCatalogTrack finds the catalogue row the local file was matched to. The
// identity check already proved a title+duration match exists; this picks the
// closest duration so a release with two same-titled recordings writes the
// right track number.
func matchCatalogTrack(view *catalogReleaseView, title string, duration float64) (catalogReleaseTrack, bool) {
	want := foldTagText(title)
	pool := []catalogReleaseTrack{}
	for _, track := range view.Tracks {
		if foldTagText(track.Title) == want {
			pool = append(pool, track)
		}
	}
	if len(pool) == 0 {
		for _, track := range view.Tracks {
			if track.Duration > 0 && absFloat(track.Duration-duration) <= 3 {
				pool = append(pool, track)
			}
		}
	}
	if len(pool) == 0 {
		return catalogReleaseTrack{}, false
	}
	best := pool[0]
	for _, track := range pool[1:] {
		if track.Duration > 0 && absFloat(track.Duration-duration) < absFloat(best.Duration-duration) {
			best = track
		}
	}
	return best, true
}

func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

// desiredTags is the catalogue answer for one file. Only fields the catalogue
// actually carries are included: an absent value never clears a tag.
func desiredTags(view *catalogReleaseView, track catalogReleaseTrack) map[string][]string {
	desired := map[string][]string{}
	if view.Title != "" {
		desired["ALBUM"] = []string{view.Title}
	}
	if view.AlbumArtist != "" {
		desired["ALBUMARTIST"] = []string{view.AlbumArtist}
	}
	if view.Date != "" {
		desired["DATE"] = []string{view.Date}
	}
	if len(view.Genres) > 0 {
		// One classification tag: the most-voted genre. The full list is
		// still reported in the response so a caller can choose another.
		desired["GENRE"] = []string{view.Genres[0]}
	}
	if track.Title != "" {
		desired["TITLE"] = []string{track.Title}
	}
	if len(track.Artists) > 0 {
		desired["ARTIST"] = append([]string(nil), track.Artists...)
	}
	if track.Position > 0 {
		desired["TRACKNUMBER"] = []string{strconv.Itoa(track.Position)}
	}
	if track.Disc > 0 && (track.Disc > 1 || multiDisc(view)) {
		desired["DISCNUMBER"] = []string{strconv.Itoa(track.Disc)}
	}
	return desired
}

func multiDisc(view *catalogReleaseView) bool {
	maxDisc := 0
	for _, track := range view.Tracks {
		if track.Disc > maxDisc {
			maxDisc = track.Disc
		}
	}
	return maxDisc > 1
}

// tagConflict is one field the catalogue disagrees with. It is reported, not
// silently overwritten.
type tagConflict struct {
	Field   string `json:"field"`
	Current string `json:"current"`
	Catalog string `json:"catalog"`
}

type metadataCompletion struct {
	Written   []string
	Kept      []tagConflict
	Unchanged []string
}

// planTagWrite compares every desired field with the file current value.
// Missing -> write; equal -> unchanged; different -> kept unless the caller
// asked to overwrite. A field the catalogue does not carry is absent from
// desired and stays untouched, so a write never deletes a tag.
func planTagWrite(current *flactags.Tags, desired map[string][]string, overwrite bool) (map[string][]string, metadataCompletion) {
	updates := map[string][]string{}
	result := metadataCompletion{Written: []string{}, Kept: []tagConflict{}, Unchanged: []string{}}
	keys := make([]string, 0, len(desired))
	for key := range desired {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		want := desired[key]
		have := current.Raw[key]
		switch {
		case len(have) == 0:
			updates[key] = want
			result.Written = append(result.Written, strings.ToLower(key))
		case sameTagValue(key, have, want):
			result.Unchanged = append(result.Unchanged, strings.ToLower(key))
		case overwrite:
			updates[key] = want
			result.Written = append(result.Written, strings.ToLower(key))
		default:
			result.Kept = append(result.Kept, tagConflict{Field: strings.ToLower(key), Current: strings.Join(have, " / "), Catalog: strings.Join(want, " / ")})
		}
	}
	return updates, result
}

// sameTagValue compares one field. A track/disc "3/12" equals "3"; text
// comparison ignores case, fullwidth forms and surrounding spaces.
func sameTagValue(key string, have, want []string) bool {
	if key == "TRACKNUMBER" || key == "DISCNUMBER" {
		if len(have) != 1 || len(want) != 1 {
			return false
		}
		number := leadingNumber(want[0])
		return number > 0 && leadingNumber(have[0]) == number
	}
	if len(have) != len(want) {
		return false
	}
	for i := range have {
		if foldTagText(have[i]) != foldTagText(want[i]) {
			return false
		}
	}
	return true
}

func leadingNumber(value string) int {
	if i := strings.IndexByte(value, '/'); i >= 0 {
		value = value[:i]
	}
	number, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0
	}
	return number
}

// foldTagText is the comparison form for tag text: fullwidth ASCII folded to
// ASCII, case folded, whitespace collapsed.
func foldTagText(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		}
		if r == 0x3000 {
			r = ' '
		}
		b.WriteRune(r)
	}
	return strings.ToLower(strings.Join(strings.Fields(b.String()), " "))
}

func catalogTagSummary(view *catalogReleaseView) map[string]any {
	return map[string]any{
		"release_id": view.ReleaseID, "title": view.Title, "album_artist": view.AlbumArtist,
		"date": view.Date, "genres": view.Genres,
	}
}

func coverWriteMode(requested string, settings lyricsSettings) string {
	mode := strings.TrimSpace(requested)
	if mode == "" {
		mode = strings.TrimSpace(settings.WriteMode)
	}
	if mode == "embedded" {
		return "embedded"
	}
	return "sidecar"
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

func (a *App) libraryFiles(w http.ResponseWriter, r *http.Request) {
	offset, limit := 0, 20
	if r != nil {
		if value, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && value > 0 {
			offset = value
		}
		if value, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && value > 0 && value <= 100 {
			limit = value
		}
	}
	files, total, offset := a.libraryFilePage(offset, limit)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "files": files, "total": total, "offset": offset, "downloaded": false})
}

func (a *App) libraryFilePage(offset, limit int) ([]string, int, int) {
	root := a.libraryDir()
	files := []string{}
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return nil
		}
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
		if !audioExtensions[ext] {
			return nil
		}
		files = append(files, path)
		if len(files) >= 500 {
			return fs.SkipAll
		}
		return nil
	})
	sort.Strings(files)
	if offset > len(files) {
		offset = len(files)
	}
	if limit <= 0 {
		limit = 20
	}
	end := offset + limit
	if end > len(files) {
		end = len(files)
	}
	return append([]string(nil), files[offset:end]...), len(files), offset
}

// fetchArtwork reads the lyrics and the cover for one track from the lyrics
// service. An explicit address in the request overrides the saved service for
// this call; both go through the same origin rules and send the saved
// authorization value only when the address comes from the settings.
func (a *App) fetchArtwork(lyricsBase, coverBase, artist, album, title string) (string, []byte, error) {
	lyricsTarget, coverTarget, err := a.artworkTargets(lyricsBase, coverBase)
	if err != nil {
		return "", nil, err
	}
	query := url.Values{"artist": {artist}, "title": {title}, "album": {album}}
	var lyrics string
	if lyricsTarget.configured() {
		body, found, err := artworkGET(lyricsTarget, query)
		if err != nil {
			return "", nil, err
		}
		if found {
			lyrics = parseLyricsBody(body)
		}
	}
	var cover []byte
	if coverTarget.configured() {
		body, found, err := artworkGET(coverTarget, query)
		if err != nil {
			return "", nil, err
		}
		if found {
			if !imageBytes(body) {
				return "", nil, errors.New("cover is not an image")
			}
			cover = body
		}
	}
	return lyrics, cover, nil
}

func imageBytes(body []byte) bool {
	if len(body) < 32 {
		return false
	}
	if body[0] == 0xff && body[1] == 0xd8 {
		return true
	}
	return len(body) >= 8 && string(body[:8]) == "\x89PNG\r\n\x1a\n"
}

func loopbackURL(raw string) bool {
	lower := strings.ToLower(raw)
	return strings.HasPrefix(lower, "http://127.0.0.1") || strings.HasPrefix(lower, "http://localhost") || strings.HasPrefix(lower, "http://[::1]")
}
