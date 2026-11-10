package app

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

var pipelinePauseKey = []byte("app/control/pipeline")

func (a *App) loadPipelinePause() {
	var doc struct {
		Paused bool `json:"paused"`
	}
	found, err := a.loadJSONSetting(pipelinePauseKey, &doc)
	if err != nil || !found {
		return
	}
	a.mu.Lock()
	a.paused = doc.Paused
	a.mu.Unlock()
}

func (a *App) pipelinePaused() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.paused
}

func (a *App) setPipelinePaused(paused bool) error {
	encoded, err := json.Marshal(map[string]any{"paused": paused, "updated_ns": a.now().UnixNano()})
	if err != nil {
		return err
	}
	if err = a.casSaveJSON(pipelinePauseKey, encoded); err != nil {
		return err
	}
	a.mu.Lock()
	a.paused = paused
	a.mu.Unlock()
	return nil
}

// waitIfPaused holds a worker until the pipeline is resumed. It does not
// contact a site and does not mark the task failed.
func (a *App) waitIfPaused(ctx context.Context) error {
	for a.pipelinePaused() {
		if err := a.wait(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) pipelineControl(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "paused": a.pipelinePaused()})
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	switch body.Action {
	case "pause":
		if err := a.setPipelinePaused(true); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "CONTROL_UNAVAILABLE"})
			return
		}
	case "resume":
		if err := a.setPipelinePaused(false); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "CONTROL_UNAVAILABLE"})
			return
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "paused": a.pipelinePaused()})
}
