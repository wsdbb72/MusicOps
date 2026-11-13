package app

// Roon control, embedded: this app is the extension. One WebSocket to the core
// carries registration, the zone subscription and transport commands. Roon
// asks a person to approve the extension once (Settings → Extensions); the
// token it returns is kept in the Vault so a later connect does not ask again.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/roon"
)

// roonTokenRef is where Roon's extension token is stored.
const roonTokenRef = "vault://roon/extension-token"

// roonStatusView is the state the page and the API read.
func (a *App) roonStatusView() map[string]any {
	a.roonMu.Lock()
	defer a.roonMu.Unlock()
	zones := a.roonZones
	if zones == nil {
		zones = []roon.Zone{}
	}
	return map[string]any{
		"ok": true, "state": a.roonState, "note": a.roonNote,
		"core_name": a.roonCore.DisplayName, "core_version": a.roonCore.DisplayVersion,
		"core_id": a.roonCore.CoreID, "zones": zones,
	}
}

func (a *App) setRoonState(state, note string) {
	a.roonMu.Lock()
	defer a.roonMu.Unlock()
	a.roonState, a.roonNote = state, note
}

// connectRoon dials, registers and subscribes. A pending approval is a state,
// not an error the caller can retry faster: Roon answers Unauthorized until
// the person enables the extension.
func (a *App) connectRoon(ctx context.Context, host string, port int) error {
	a.roonMu.Lock()
	if a.roonClient != nil {
		_ = a.roonClient.Close()
		a.roonClient = nil
	}
	a.roonState, a.roonNote, a.roonZones = "connecting", "", nil
	a.roonMu.Unlock()

	client, err := roon.Dial(host+":"+strconv.Itoa(port), 5*time.Second)
	if err != nil {
		a.setRoonState("failed", "连不上 Roon："+err.Error())
		return err
	}
	info, err := client.Info()
	if err != nil {
		client.Close()
		a.setRoonState("failed", "读不到 Roon 的 info："+err.Error())
		return err
	}
	savedToken := ""
	if a.vault != nil {
		if raw, found, _ := a.vault.Get(roonTokenRef, "roon"); found {
			savedToken = strings.TrimSpace(raw)
		}
	}
	a.setRoonState("pending", "等待在 Roon 的「设置 → 扩展」里启用 MusicOps")
	token, err := client.Register(roon.RegisterOptions{
		ExtensionID: "com.musicops.desktop", DisplayName: "MusicOps", DisplayVersion: roonExtensionVersion,
		Publisher: "MusicOps", Email: "musicops@localhost",
		RequiredServices: []string{"com.roonlabs.transport:1"},
		OptionalServices: []string{"com.roonlabs.browse:1"},
		Website:          "https://github.com/wsdbb72/MusicOps",
		Token:            savedToken,
	}, 120*time.Second)
	if err != nil {
		client.Close()
		a.setRoonState("failed", err.Error())
		return err
	}
	if a.vault != nil {
		_, _ = a.vault.Set(roonTokenRef, token, "roon")
	}
	client.OnMessage = func(message roon.Message) {
		if payload, ok := roon.ZoneUpdate(message); ok {
			a.applyRoonZones(payload)
		}
	}
	zones, err := client.SubscribeZones()
	if err != nil {
		client.Close()
		a.setRoonState("failed", "订阅播放区失败："+err.Error())
		return err
	}
	a.roonMu.Lock()
	a.roonClient, a.roonCore = client, info
	a.roonState, a.roonNote, a.roonToken = "connected", "", token
	a.roonZones = zones.Zones
	a.roonMu.Unlock()
	return nil
}

// roonExtensionVersion is reported to Roon; the build version is injected by
// the release pipeline, so this stays a name the core can display.
const roonExtensionVersion = "0.4.38"

// applyRoonZones folds one subscription push into the snapshot.
func (a *App) applyRoonZones(payload roon.ZonesPayload) {
	a.roonMu.Lock()
	defer a.roonMu.Unlock()
	if len(payload.Zones) > 0 {
		a.roonZones = payload.Zones
		return
	}
	for _, changed := range payload.ZonesChanged {
		replaced := false
		for i := range a.roonZones {
			if a.roonZones[i].ZoneID == changed.ZoneID {
				a.roonZones[i] = changed
				replaced = true
				break
			}
		}
		if !replaced {
			a.roonZones = append(a.roonZones, changed)
		}
	}
	for _, removed := range payload.ZonesRemoved {
		kept := a.roonZones[:0]
		for _, zone := range a.roonZones {
			if zone.ZoneID != removed.ZoneID {
				kept = append(kept, zone)
			}
		}
		a.roonZones = kept
	}
	for _, seek := range payload.ZonesSeekChanged {
		for i := range a.roonZones {
			if a.roonZones[i].ZoneID == seek.ZoneID {
				a.roonZones[i].NowPlaying.SeekPosition = seek.SeekPosition
			}
		}
	}
}

// roonStatus is GET /v1/roon/status.
func (a *App) roonStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.roonStatusView())
}

// roonConnect is POST /v1/roon/connect.
func (a *App) roonConnect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	host := strings.TrimSpace(body.Host)
	if host == "" {
		host = "127.0.0.1"
	}
	if body.Port == 0 {
		body.Port = 9330
	}
	if body.Port < 1 || body.Port > 65535 || strings.ContainsAny(host, " /\r\n:") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "主机或端口不合法"})
		return
	}
	if err := a.connectRoon(r.Context(), host, body.Port); err != nil {
		view := a.roonStatusView()
		view["ok"] = false
		view["code"] = "ROON_UNAVAILABLE"
		view["error"] = err.Error()
		writeJSON(w, http.StatusBadGateway, view)
		return
	}
	writeJSON(w, http.StatusOK, a.roonStatusView())
}

// roonControl is POST /v1/roon/control.
func (a *App) roonControl(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ZoneID  string `json:"zone_id"`
		Control string `json:"control"`
		Value   int    `json:"value"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	allowed := map[string]bool{"play": true, "pause": true, "playpause": true, "stop": true, "next": true, "previous": true, "volume": true}
	if !allowed[body.Control] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "不支持的动作：" + body.Control})
		return
	}
	if body.Control == "volume" && (body.Value < 0 || body.Value > 100) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "音量必须在 0-100"})
		return
	}
	a.roonMu.Lock()
	client := a.roonClient
	a.roonMu.Unlock()
	if client == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "ROON_NOT_CONNECTED", "error": "还没有连接 Roon"})
		return
	}
	if err := client.Control(body.ZoneID, body.Control, body.Value); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "ROON_CONTROL_FAILED", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "zone_id": body.ZoneID, "control": body.Control})
}

// roonDisconnect is POST /v1/roon/disconnect.
func (a *App) roonDisconnect(w http.ResponseWriter, _ *http.Request) {
	a.roonMu.Lock()
	if a.roonClient != nil {
		_ = a.roonClient.Close()
		a.roonClient = nil
	}
	a.roonState, a.roonNote, a.roonZones = "disconnected", "", nil
	a.roonMu.Unlock()
	writeJSON(w, http.StatusOK, a.roonStatusView())
}
