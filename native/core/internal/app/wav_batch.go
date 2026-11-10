package app

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/flactags"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/wavflac"
)

// WAV conversion settings. Automatic conversion is on unless a saved
// document explicitly turns it off. Disposal stays keep unless chosen.
type wavSettings struct {
	Auto      bool   `json:"auto_convert_wav"`
	Disposal  string `json:"wav_disposal"`
	UpdatedNS int64  `json:"updated_ns"`
}

func defaultWAVSettings() wavSettings {
	return wavSettings{Auto: true, Disposal: "keep"}
}

func wavSettingsKey() []byte { return []byte("app/settings/wav") }

func (a *App) loadWAVSettings() wavSettings {
	var raw json.RawMessage
	found, err := a.loadJSONSetting(wavSettingsKey(), &raw)
	if err != nil || !found || len(raw) == 0 {
		return defaultWAVSettings()
	}
	var doc struct {
		Auto      *bool  `json:"auto_convert_wav"`
		Disposal  string `json:"wav_disposal"`
		UpdatedNS int64  `json:"updated_ns"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return defaultWAVSettings()
	}
	settings := defaultWAVSettings()
	settings.UpdatedNS = doc.UpdatedNS
	if doc.Auto != nil {
		settings.Auto = *doc.Auto
	}
	switch doc.Disposal {
	case "keep", "recycle", "delete":
		settings.Disposal = doc.Disposal
	}
	return settings
}

func (a *App) wavSettingsHandler(w http.ResponseWriter, r *http.Request) {
	settings := a.loadWAVSettings()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "wav": settings, "note": wavSettingNote(settings)})
}

func (a *App) saveWAVSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Auto     *bool  `json:"auto_convert_wav"`
		Disposal string `json:"wav_disposal"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	switch body.Disposal {
	case "", "keep", "recycle", "delete":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "原 WAV 只能保留、回收或删除"})
		return
	}
	saved := a.loadWAVSettings()
	if body.Auto != nil {
		saved.Auto = *body.Auto
	}
	if body.Disposal != "" {
		saved.Disposal = body.Disposal
	}
	saved.UpdatedNS = a.now().UnixNano()
	encoded, err := json.Marshal(saved)
	if err != nil || a.casSaveJSON(wavSettingsKey(), encoded) != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "wav": saved, "note": wavSettingNote(saved)})
}

func wavSettingNote(settings wavSettings) string {
	note := "默认开启：下载进入待入库或重新索引时，先把整数 PCM WAV 无损转成 FLAC 并迁移标签，再做识别入库。关闭后 WAV 保持原样，识别仍会读取里面的 LIST/INFO 和 ID3。"
	switch settings.Disposal {
	case "recycle":
		note += "原 WAV 移入可恢复的回收目录。"
	case "delete":
		note += "校验通过后删除原 WAV。"
	default:
		note += "原 WAV 默认保留。"
	}
	if settings.Auto {
		note += "当前已打开自动转换。"
	} else {
		note += "当前已关闭自动转换。"
	}
	return note
}

// finishWAV applies the disposal setting only after the FLAC has been verified.
// keep leaves the WAV in place. recycle moves it. delete removes it.
func (a *App) finishWAV(source, output, disposal string) string {
	switch disposal {
	case "delete":
		if os.Remove(source) == nil {
			return "delete"
		}
		return "keep"
	case "recycle":
		if a.recycleWAV(source, output) == nil {
			return "recycle"
		}
		return "keep"
	default:
		return "keep"
	}
}

func (a *App) recycleWAV(source, output string) error {
	dir := filepath.Join(a.Dir, "recycle", "wav")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	name := filepath.Base(source)
	dest := filepath.Join(dir, name)
	if _, err := os.Stat(dest); err == nil {
		dest = filepath.Join(dir, strings.TrimSuffix(name, filepath.Ext(name))+"-"+strconvHex(a.now().UnixNano())+filepath.Ext(name))
	}
	if err := os.Rename(source, dest); err != nil {
		return err
	}
	note, _ := json.Marshal(map[string]string{"original": source, "output": output, "recycled": dest})
	return os.WriteFile(dest+".json", note, 0o600)
}

func strconvHex(v int64) string {
	const digits = "0123456789abcdef"
	var buf [16]byte
	for i := 15; i >= 0; i-- {
		buf[i] = digits[v&0xf]
		v >>= 4
	}
	return string(buf[:])
}

func (a *App) indexConverted(path string) {
	library, err := filepath.Abs(a.libraryDir())
	if err != nil {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return
	}
	rel, err := filepath.Rel(library, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return
	}
	track := trackNumberFromName(filepath.Base(abs))
	_, _ = audiorelease.IndexExisting(a.engine, audiorelease.Existing{
		RequestID: libraryFileID(abs), RootID: libraryRootID(filepath.Dir(abs)), Track: track,
		Path: abs, Library: library, Format: "flac", SourceName: "wav-convert",
	})
}

type wavBatch struct {
	ID      string   `json:"batch_id"`
	TaskIDs []string `json:"task_ids"`
	Paths   []string `json:"paths"`
}

func wavBatchKey(id string) []byte { return []byte("transcode/batch/" + id) }

func (a *App) transcodeBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paths     []string `json:"paths"`
		Dir       string   `json:"dir"`
		Disposal  string   `json:"wav_disposal"`
		RequestID string   `json:"request_id"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	if body.Disposal != "" {
		switch body.Disposal {
		case "keep", "recycle", "delete":
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "原 WAV 只能保留、回收或删除", "downloaded": false})
			return
		}
		current := a.loadWAVSettings()
		current.Disposal = body.Disposal
		encoded, err := json.Marshal(current)
		if err != nil || a.casSaveJSON(wavSettingsKey(), encoded) != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "原 WAV 的处理方式无效"})
			return
		}
	}
	paths, err := a.collectWAV(body.Paths, body.Dir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": err.Error(), "downloaded": false})
		return
	}
	if len(paths) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "没有可转换的 WAV", "downloaded": false})
		return
	}
	batchID := body.RequestID
	if !validTranscodeID(batchID) {
		batchID = "batch" + strconvHex(a.now().UnixNano())
	}
	var taskIDs []string
	for i, path := range paths {
		requestID := batchID + "-" + strconvHex(int64(i + 1))[12:]
		if !validTranscodeID(requestID) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
			return
		}
		encoded, err := json.Marshal(transcodePayload{RequestID: requestID, Source: path, Format: "flac"})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "TRANSCODE_UNAVAILABLE"})
			return
		}
		taskID := "transcode:" + requestID
		if _, err := a.tasks.Enqueue("enqueue-"+requestID, taskrepo.Spec{ID: taskID, Command: transcodeCommand, Lane: "transcode", Priority: 1, CreatedNS: a.now().UnixNano()}, encoded); err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "TRANSCODE_CONFLICT", "error": "有文件已经在转换", "downloaded": false})
			return
		}
		taskIDs = append(taskIDs, taskID)
	}
	record := wavBatch{ID: batchID, TaskIDs: taskIDs, Paths: paths}
	encoded, _ := json.Marshal(record)
	if a.casSaveJSON(wavBatchKey(batchID), encoded) != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "TRANSCODE_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "batch_id": batchID, "count": len(paths), "task_ids": taskIDs,
		"downloaded": false, "note": "已开始批量无损转换。校验通过前不会改原 WAV。",
	})
}

func (a *App) collectWAV(paths []string, dir string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(path string) error {
		resolved, err := a.resolveLibraryPath(path)
		if err != nil || !fileExists(resolved) || !strings.EqualFold(filepath.Ext(resolved), ".wav") {
			return errors.New("请选择音乐库或待入库目录里的 WAV")
		}
		if !seen[resolved] {
			seen[resolved] = true
			out = append(out, resolved)
		}
		return nil
	}
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if err := add(path); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(dir) != "" {
		root, err := a.resolveLibraryPath(dir)
		if err != nil {
			return nil, errors.New("批量目录不在音乐库或应用目录内")
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			return nil, errors.New("批量目录不存在")
		}
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() || len(out) >= 200 {
				return nil
			}
			if strings.EqualFold(filepath.Ext(path), ".wav") {
				_ = add(path)
			}
			return nil
		})
	}
	if len(out) > 200 {
		out = out[:200]
	}
	return out, nil
}

func (a *App) transcodeBatchStatus(w http.ResponseWriter, r *http.Request) {
	record, err := a.readWAVBatch(r.URL.Query().Get("batch_id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "code": "NOT_FOUND"})
		return
	}
	summary := a.summarizeBatch(record)
	writeJSON(w, http.StatusOK, summary)
}

func (a *App) transcodeBatchControl(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BatchID string `json:"batch_id"`
		Action  string `json:"action"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	switch body.Action {
	case "pause", "resume", "cancel":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	record, err := a.readWAVBatch(body.BatchID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "code": "NOT_FOUND"})
		return
	}
	for _, id := range record.TaskIDs {
		_, err := a.tasks.Control("wav-"+body.Action+"-"+id, id, body.Action, a.now().UnixNano())
		if errors.Is(err, taskrepo.ErrStale) {
			continue
		}
	}
	summary := a.summarizeBatch(record)
	summary["action"] = body.Action
	summary["note"] = "已提交" + batchActionName(body.Action) + "。已完成的文件不会重做，原 WAV 只在校验通过后按设置处理。"
	writeJSON(w, http.StatusOK, summary)
}

func batchActionName(action string) string {
	switch action {
	case "pause":
		return "暂停"
	case "resume":
		return "继续"
	default:
		return "取消"
	}
}

func (a *App) readWAVBatch(id string) (wavBatch, error) {
	var record wavBatch
	if !validTranscodeID(id) {
		return record, os.ErrNotExist
	}
	found, err := a.loadJSONSetting(wavBatchKey(id), &record)
	if err != nil || !found {
		return record, os.ErrNotExist
	}
	return record, nil
}

func (a *App) summarizeBatch(record wavBatch) map[string]any {
	counts := map[string]int{}
	rows := []map[string]any{}
	for _, id := range record.TaskIDs {
		task, err := a.tasks.Get(id)
		state, progress, message := "missing", 0.0, ""
		if err == nil && task != nil {
			state, progress, message = task.State, task.Progress, task.Message
		}
		counts[state]++
		rows = append(rows, map[string]any{"task_id": id, "state": state, "progress": progress, "message": message})
	}
	return map[string]any{
		"ok": true, "batch_id": record.ID, "count": len(record.TaskIDs), "states": counts,
		"tasks": rows, "downloaded": false, "note": "批量转换只产生 FLAC。校验通过前不改原 WAV。",
	}
}

// importIdentity is what the importer can read from one file. IdentityConfirmed
// stays false: file tags and filename hints are not a catalogue confirmation.
type importIdentity struct {
	Path              string `json:"path"`
	Format            string `json:"format"`
	Artist            string `json:"artist"`
	Album             string `json:"album"`
	Title             string `json:"title"`
	Track             string `json:"track"`
	TagSource         string `json:"tag_source"`
	FromFile          bool   `json:"from_file"`
	NeedsReview       bool   `json:"needs_review"`
	Reason            string `json:"reason"`
	IdentityConfirmed bool   `json:"identity_confirmed"`
}

// identityCompare is the same file before and after the import step.
// Before is the original container. After is the FLAC when automatic
// conversion ran, otherwise the same file with its original tags read.
type identityCompare struct {
	Before        importIdentity `json:"before"`
	After         importIdentity `json:"after"`
	Converted     bool           `json:"converted"`
	PublishedName string         `json:"published_name"`
}

func fileIdentity(path string) (importIdentity, error) {
	ext := strings.ToLower(filepath.Ext(path))
	id := importIdentity{Path: path, Format: strings.TrimPrefix(ext, "."), IdentityConfirmed: false}
	switch ext {
	case ".wav":
		tags, err := wavflac.ReadDescription(path)
		if err != nil {
			return id, err
		}
		id.Artist, id.Album, id.Title, id.Track, id.TagSource = tags.Artist, tags.Album, tags.Title, tags.Track, tags.Source
	case ".flac":
		tags, err := flactags.ReadFile(path)
		if err != nil {
			return id, err
		}
		id.Artist, id.Album, id.Title = tags.Artist, tags.Album, tags.Title
		if tags.TrackNumber > 0 {
			id.Track = strconv.Itoa(tags.TrackNumber)
		}
		if raw := tags.Raw["MUSICOPS_TAG_SOURCE"]; len(raw) > 0 {
			id.TagSource = raw[0]
		} else if id.Artist != "" || id.Album != "" || id.Title != "" {
			id.TagSource = "vorbis"
		}
	default:
		return id, errors.New("不是 WAV 或 FLAC")
	}
	id.FromFile = fileTagSource(id.TagSource)
	if id.FromFile && id.Artist != "" && id.Album != "" && id.Title != "" {
		id.NeedsReview = false
		id.Reason = "文件自带艺人、专辑和曲名，不必因为 WAV 标签难读而进入待核对"
		return id, nil
	}
	id.NeedsReview = true
	if strings.Contains(id.TagSource, "path") && !id.FromFile {
		id.Reason = "只有文件名提示，身份未确认，需要核对"
	} else {
		id.Reason = "没有艺人、专辑或曲名，需要核对"
	}
	return id, nil
}

func fileTagSource(source string) bool {
	for _, part := range strings.Split(source, "+") {
		switch part {
		case "info", "id3", "bext", "vorbis":
			return true
		}
	}
	return false
}

// compareIncomingIdentity reads the original tags, converts a WAV when the
// setting is on, then reads the file that import will use. Conversion is
// finished before the review decision on the published file.
func (a *App) compareIncomingIdentity(dir, name string) (identityCompare, error) {
	source := filepath.Join(dir, name)
	before, err := fileIdentity(source)
	if err != nil {
		return identityCompare{}, err
	}
	published := name
	if strings.EqualFold(filepath.Ext(name), ".wav") && a.loadWAVSettings().Auto {
		published, err = a.prepareIncomingWAV(dir, name)
		if err != nil {
			return identityCompare{}, err
		}
	}
	after, err := fileIdentity(filepath.Join(dir, published))
	if err != nil {
		return identityCompare{}, err
	}
	return identityCompare{Before: before, After: after, Converted: published != name, PublishedName: published}, nil
}

// prepareIncomingWAV converts a WAV in incoming before release when automatic
// conversion is on. The returned name is the file release should publish.
func (a *App) prepareIncomingWAV(dir, name string) (string, error) {
	if !strings.EqualFold(filepath.Ext(name), ".wav") || !a.loadWAVSettings().Auto {
		return name, nil
	}
	source := filepath.Join(dir, name)
	outputName := strings.TrimSuffix(name, filepath.Ext(name)) + ".flac"
	output := filepath.Join(dir, outputName)
	converted, err := wavflac.ConvertFile(source, output, true)
	if err != nil {
		return "", err
	}
	if !converted.Replayed {
		a.finishWAV(source, output, a.loadWAVSettings().Disposal)
	}
	return outputName, nil
}

func (a *App) maybeConvertIndexedWAV(path string) string {
	if !strings.EqualFold(filepath.Ext(path), ".wav") || !a.loadWAVSettings().Auto {
		return path
	}
	output := strings.TrimSuffix(path, filepath.Ext(path)) + ".flac"
	converted, err := wavflac.ConvertFile(path, output, true)
	if err != nil {
		return path
	}
	if !converted.Replayed {
		a.finishWAV(path, output, a.loadWAVSettings().Disposal)
	}
	return output
}

// restoreRecycledWAV moves one recycled WAV back when the original path is free.
func (a *App) restoreRecycledWAV(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := decode(r, &body); err != nil || strings.TrimSpace(body.Name) == "" || strings.Contains(body.Name, "/") || strings.Contains(body.Name, "\\") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	base := filepath.Base(body.Name)
	notePath := filepath.Join(a.Dir, "recycle", "wav", base+".json")
	raw, err := os.ReadFile(notePath)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "code": "NOT_FOUND"})
		return
	}
	var note struct {
		Original string `json:"original"`
		Recycled string `json:"recycled"`
	}
	if json.Unmarshal(raw, &note) != nil || note.Recycled == "" || note.Original == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "code": "NOT_FOUND"})
		return
	}
	if _, err := os.Stat(note.Original); err == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "CONFLICT", "error": "原位置已有文件，没有覆盖"})
		return
	}
	if err := os.MkdirAll(filepath.Dir(note.Original), 0o700); err != nil || os.Rename(note.Recycled, note.Original) != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "CONFLICT", "error": "没有恢复原 WAV"})
		return
	}
	_ = os.Remove(notePath)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": note.Original, "note": "已把 WAV 放回原位置。"})
}
