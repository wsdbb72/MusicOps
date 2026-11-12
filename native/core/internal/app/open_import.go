package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

// openImportRequest is the body of POST /v1/open/library/import.
//
// It is deliberately independent of any workflow job: external software that
// fetched or ripped a file itself can put it into the library with the
// metadata it already knows. The source must be a file in the state
// directory's incoming/, which is where /v1/audio/incoming uploads land; a
// path outside it is refused instead of being guessed at.
type openImportRequest struct {
	Source string `json:"source"`
	Artist string `json:"artist"`
	Album  string `json:"album"`
	Title  string `json:"title"`
	Track  int    `json:"track"`
	RootID string `json:"root_id"`
	Origin string `json:"origin"`
}

// openImport is POST /v1/open/library/import.
func (a *App) openImport(w http.ResponseWriter, r *http.Request) {
	var body openImportRequest
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "written": false})
		return
	}
	source := strings.TrimSpace(body.Source)
	if source == "" || body.Track < 1 || body.Track > 999 || strings.TrimSpace(body.Artist) == "" || strings.TrimSpace(body.Album) == "" || strings.TrimSpace(body.Title) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "source、artist、album、title、track 都必填", "written": false})
		return
	}
	path, err := a.resolveLibraryPath(source)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "源文件必须在状态目录或音乐库里", "written": false})
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "code": "NOT_FOUND", "error": "源文件不存在", "written": false})
		return
	}
	incoming := filepath.Join(a.Dir, "incoming")
	// Release reads from incoming/<basename> only; anything else is a
	// different operation (a move, an organise) and is not this endpoint.
	if !insideDir(incoming, path) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "先把文件放到 incoming/：用 POST /v1/audio/incoming 上传", "written": false})
		return
	}
	root := strings.TrimSpace(body.RootID)
	if root == "" {
		root = derivedImportRoot(body.Artist, body.Album)
	}
	origin := strings.TrimSpace(body.Origin)
	if origin == "" {
		origin = "local_import"
	}
	result, err := audiorelease.Release(r.Context(), a.engine, audiorelease.Request{
		RequestID: audioRequestID(root, body.Track), RootID: root,
		Artist: body.Artist, Album: body.Album, Title: body.Title, Track: body.Track,
		SourceName: filepath.Base(path), Origin: origin,
		Incoming: incoming, Library: a.libraryDir(), Decoder: audiorelease.SystemDecoder(),
	})
	if err != nil {
		status, code, message := http.StatusBadGateway, "IMPORT_FAILED", "入库失败"
		switch {
		case errors.Is(err, audiorelease.ErrConflict):
			status, code, message = http.StatusConflict, "LIBRARY_CONFLICT", "目标位置已有同名文件，没有覆盖"
		case errors.Is(err, audiorelease.ErrRejected):
			status, code, message = http.StatusUnprocessableEntity, "AUDIO_REJECTED", "音频被拒绝（不是可解析的 WAV/FLAC，或解码失败）"
		case errors.Is(err, audiorelease.ErrMissing):
			status, code, message = http.StatusNotFound, "NOT_FOUND", "源文件不在了"
		case errors.Is(err, dataengine.ErrInvalidRequest):
			status, code, message = http.StatusBadRequest, "INVALID_REQUEST", "请求字段不合法"
		}
		writeJSON(w, status, map[string]any{"ok": false, "code": code, "error": message, "written": false})
		return
	}
	placed, _, _ := audiorelease.PathFor(a.engine, result.RequestID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "written": true, "request_id": result.RequestID, "root_id": result.RootID,
		"path": placed, "sha256": result.SHA256, "format": result.Format,
		"replayed": result.Replayed, "source_preserved": result.SourcePreserved,
		"note": "已入库并校验；源文件保留在 incoming/，可以自行清理。这不是下载。",
	})
}

// derivedImportRoot is a stable album root for one (artist, album) pair, so
// two tracks of the same album land in the same folder and a repeated import
// replays its receipt instead of creating a second album.
func derivedImportRoot(artist, album string) string {
	sum := sha256.Sum256([]byte("open-import\x00" + foldTagText(artist) + "\x00" + foldTagText(album)))
	return "imp-" + hex.EncodeToString(sum[:8])
}
