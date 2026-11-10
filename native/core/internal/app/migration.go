package app

import (
	"net/http"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/livemigrate"
)

const migrationNote = "旧版若开启了自动更新，会安装当时的官方发行版。本版启动时把旧音乐库索引、未完成任务和未对账令牌迁入正在使用的数据；音乐文件不移动。迁移失败则启动失败，可以原样重试。回退只撤索引、任务和迁入的令牌记录，不删除音乐文件，也不删除旧库归档。本版不会在启动时自动安装更新。"

// migrationStatus explains the 0.4.31 upgrade. The cold archive stays even
// after a live rollback. This process does not auto-install a newer release.
func (a *App) migrationStatus(w http.ResponseWriter, _ *http.Request) {
	report, found, err := livemigrate.Read(a.engine)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "MIGRATION_UNAVAILABLE"})
		return
	}
	cold, err := a.coldArchivePresent()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "MIGRATION_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "found": found, "report": report, "cold_archive": cold,
		"auto_install": false, "note": migrationNote,
	})
}

func (a *App) migrationRollback(w http.ResponseWriter, _ *http.Request) {
	if err := livemigrate.Rollback(a.engine, a.flows, a.ledger); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "MIGRATION_ROLLBACK_FAILED", "error": "回退没有完成，音乐文件未改动"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rolled_back": true, "audio_unchanged": true})
}

func (a *App) updateSettings(w http.ResponseWriter, _ *http.Request) {
	var doc struct {
		Enabled bool `json:"enabled"`
	}
	found, err := a.loadJSONSetting([]byte("app/settings/updates"), &doc)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "enabled": found && doc.Enabled, "stored": found, "auto_install": false,
		"note": "本版不会在启动时自动安装更新。托盘里的检查更新仍需手动确认。",
	})
}

func (a *App) coldArchivePresent() (bool, error) {
	var found bool
	err := a.engine.View(func(v *dataengine.View) error {
		var err error
		_, found, err = v.Get([]byte("archive/meta/manifest"))
		return err
	})
	return found, err
}
