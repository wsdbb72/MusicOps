package app

// Roon control, embedded: this app is the extension. One WebSocket to the core
// carries registration, the zone subscription and transport commands. Roon
// asks a person to approve the extension once (Settings → Extensions); the
// token it returns is kept in the Vault so a later connect does not ask again.
//
// Registration can block for as long as it takes a person to click Enable, so
// connect returns as soon as the socket is up and the waiting happens on a
// background goroutine while the page polls the status.

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/roon"
	"github.com/wsdbb72/MusicOps/native/core/internal/version"
)

// roonTokenRef is where Roon's extension token is stored.
const roonTokenRef = "vault://roon/extension-token"

// roonExtensionID, displayName and version identify this extension to Roon.
// Changing the required/provided service list invalidates an existing approval,
// so these stay stable once shipped.
const (
	roonExtensionID = "com.musicops.desktop"
	roonDisplayName = "MusicOps"
)

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
		"transport": a.roonService,
	}
}

func (a *App) setRoonState(state, note string) {
	a.roonMu.Lock()
	defer a.roonMu.Unlock()
	a.roonState, a.roonNote = state, note
}

// connectRoon dials the core, starts registration in the background and
// returns the pending state. An error here only means the socket or info step
// failed; "waiting for approval" is not an error.
func (a *App) connectRoon(host string, port int) (map[string]any, error) {
	a.roonMu.Lock()
	if a.roonClient != nil {
		_ = a.roonClient.Close()
		a.roonClient = nil
	}
	a.roonGeneration++
	generation := a.roonGeneration
	a.roonState, a.roonNote, a.roonZones, a.roonCore = "connecting", "", nil, roon.Info{}
	a.roonMu.Unlock()

	address := host + ":" + strconv.Itoa(port)
	client, err := roon.Dial(address, 5*time.Second)
	if err != nil {
		a.setRoonState("failed", "连不上 Roon："+err.Error())
		return a.roonStatusView(), err
	}
	info, err := client.Info()
	if err != nil {
		_ = client.Close()
		a.setRoonState("failed", "读不到 Roon 的 core 信息："+err.Error())
		return a.roonStatusView(), err
	}
	client.OnMessage = func(message roon.Message) {
		if payload, ok := roon.ZoneUpdate(message); ok {
			a.applyRoonZones(payload)
		}
	}

	token := ""
	if a.vault != nil {
		if raw, ok, _ := a.vault.Get(roonTokenRef, "roon"); ok {
			token = strings.TrimSpace(raw)
		}
	}

	a.roonMu.Lock()
	a.roonClient, a.roonCore = client, info
	a.roonState = "pending"
	a.roonNote = "已连接 " + info.DisplayName + "，等待在 Roon 的「设置 → 扩展」里启用 MusicOps（只需一次）"
	a.roonMu.Unlock()

	ready := make(chan struct{})
	a.roonMu.Lock()
	a.roonReady = ready
	a.roonMu.Unlock()
	go a.runRoonRegistration(client, info, token, generation, ready)
	// Registration with a stored token finishes in milliseconds; wait a
	// moment so the page shows "connected" instead of flashing "pending".
	// A core that needs approval keeps the socket and answers much later.
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
	}
	return a.roonStatusView(), nil
}

// runRoonRegistration waits for the core to accept the extension, then keeps
// the zone subscription alive. Roon answers Registered only after a person
// enables the extension, so the wait is minutes, not seconds.
func (a *App) runRoonRegistration(client *roon.Client, info roon.Info, token string, generation int, ready chan struct{}) {
	finish := func() {
		select {
		case <-ready:
		default:
			close(ready)
		}
	}
	defer finish()
	// The service list is deliberately the one Roon approved for this
	// extension: changing required/provided services invalidates the stored
	// token and forces the user to enable MusicOps again. Roon 2.70 still
	// serves transport:1 (zones, control, volume all work), and it grants it
	// for this list, so no click is needed. If a future core stops serving
	// transport:1 the subscribe step fails and the note says so.
	registration, err := client.Register(roon.RegisterOptions{
		ExtensionID: roonExtensionID, DisplayName: roonDisplayName,
		DisplayVersion: a.version(), Publisher: "MusicOps", Email: "musicops@localhost",
		RequiredServices: []string{roon.TransportV1},
		OptionalServices: []string{roon.BrowseV1},
		ProvidedServices: []string{},
		Website:          "https://github.com/wsdbb72/MusicOps",
		Token:            token,
	}, 10*time.Minute)
	if !a.roonGenerationMatches(generation) {
		return
	}
	if err != nil {
		// A timeout means nobody clicked Enable. Keep the socket and say so;
		// the request is still pending on Roon's side.
		if strings.Contains(err.Error(), "超时") {
			a.setRoonState("pending", "仍在等待启用：打开 Roon 的「设置 → 扩展」，把 MusicOps 打开")
			return
		}
		_ = client.Close()
		a.setRoonState("failed", "注册失败："+err.Error())
		return
	}
	if a.vault != nil && registration.Token != "" {
		_, _ = a.vault.Set(roonTokenRef, registration.Token, "roon")
	}

	zones, service, err := client.SubscribeZones()
	if !a.roonGenerationMatches(generation) {
		return
	}
	if err != nil {
		a.setRoonState("failed", "已启用，但订阅播放区失败："+err.Error())
		return
	}
	a.roonMu.Lock()
	a.roonService = service
	a.roonZones = zones.Zones
	a.roonToken = registration.Token
	a.roonState = "connected"
	a.roonNote = ""
	a.roonMu.Unlock()
}

func (a *App) roonGenerationMatches(generation int) bool {
	a.roonMu.Lock()
	defer a.roonMu.Unlock()
	return a.roonGeneration == generation && a.roonClient != nil
}

// version is the running build, used as the extension's display version.
func (a *App) version() string {
	return version.Version
}

func (a *App) applyRoonZones(payload roon.ZonesPayload) {
	a.roonMu.Lock()
	defer a.roonMu.Unlock()
	if len(payload.Zones) > 0 {
		a.roonZones = payload.Zones
		return
	}
	for _, added := range payload.ZonesAdded {
		a.upsertRoonZone(added)
	}
	for _, changed := range payload.ZonesChanged {
		a.upsertRoonZone(changed)
	}
	for _, removed := range payload.ZonesRemoved {
		kept := a.roonZones[:0]
		for _, zone := range a.roonZones {
			if zone.ZoneID != removed {
				kept = append(kept, zone)
			}
		}
		a.roonZones = kept
	}
	for _, seek := range payload.ZonesSeekChanged {
		for i := range a.roonZones {
			if a.roonZones[i].ZoneID == seek.ZoneID && a.roonZones[i].NowPlaying != nil {
				position := seek.SeekPosition
				a.roonZones[i].NowPlaying.SeekPosition = &position
			}
		}
	}
}

func (a *App) upsertRoonZone(zone roon.Zone) {
	for i := range a.roonZones {
		if a.roonZones[i].ZoneID == zone.ZoneID {
			// A changed push may omit outputs; keep what we already know.
			if len(zone.Outputs) == 0 {
				zone.Outputs = a.roonZones[i].Outputs
			}
			a.roonZones[i] = zone
			return
		}
	}
	a.roonZones = append(a.roonZones, zone)
}

// roonStatus is GET /v1/roon/status.
func (a *App) roonStatus(w http.ResponseWriter, r *http.Request) {
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
	view, err := a.connectRoon(host, body.Port)
	if err != nil {
		view["ok"] = false
		view["code"] = "ROON_UNAVAILABLE"
		view["error"] = err.Error()
		writeJSON(w, http.StatusBadGateway, view)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// roonControl is POST /v1/roon/control.
func (a *App) roonControl(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ZoneID    string `json:"zone_id"`
		OutputID  string `json:"output_id"`
		Control   string `json:"control"`
		Value     int    `json:"value"`
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
	client, service := a.roonClient, a.roonService
	var zone *roon.Zone
	for i := range a.roonZones {
		if a.roonZones[i].ZoneID == body.ZoneID {
			zone = &a.roonZones[i]
			break
		}
	}
	a.roonMu.Unlock()
	if client == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "ROON_NOT_CONNECTED", "error": "还没有连接 Roon"})
		return
	}
	if service == "" {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "ROON_NOT_READY", "error": "还在等待 Roon 启用扩展"})
		return
	}
	if zone == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "code": "ROON_ZONE_UNKNOWN", "error": "没有这个播放区"})
		return
	}
	if body.Control == "volume" {
		outputID, target := "", 0
		for _, output := range zone.Outputs {
			if output.OutputID == body.OutputID && output.Volume != nil {
				outputID, target = output.OutputID, body.Value
				break
			}
		}
		if outputID == "" {
			for _, output := range zone.Outputs {
				if output.Volume != nil && output.Volume.Max > 0 {
					outputID, target = output.OutputID, body.Value
					break
				}
			}
		}
		if outputID == "" {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "ROON_NO_VOLUME", "error": "这个播放区在 Roon 里没有音量控制"})
			return
		}
		// The page speaks 0-100; Roon wants the output's own units.
		absolute := target
		for _, output := range zone.Outputs {
			if output.OutputID == outputID && output.Volume != nil && output.Volume.Max > 0 {
				absolute = output.Volume.Min + target*(output.Volume.Max-output.Volume.Min)/100
			}
		}
		if err := client.ChangeVolume(service, outputID, "absolute", absolute); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "ROON_CONTROL_FAILED", "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "zone_id": body.ZoneID, "control": "volume", "value": target})
		return
	}
	if err := client.Control(service, body.ZoneID, body.Control); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "ROON_CONTROL_FAILED", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "zone_id": body.ZoneID, "control": body.Control})
}

// roonDisconnect is POST /v1/roon/disconnect.
func (a *App) roonDisconnect(w http.ResponseWriter, r *http.Request) {
	a.roonMu.Lock()
	a.roonGeneration++
	if a.roonClient != nil {
		_ = a.roonClient.Close()
		a.roonClient = nil
	}
	a.roonState, a.roonNote, a.roonZones, a.roonService = "disconnected", "", nil, ""
	a.roonMu.Unlock()
	writeJSON(w, http.StatusOK, a.roonStatusView())
}

// closeRoon is called from App.Close so the socket does not outlive the app.
func (a *App) closeRoon() {
	a.roonMu.Lock()
	defer a.roonMu.Unlock()
	a.roonGeneration++
	if a.roonClient != nil {
		_ = a.roonClient.Close()
		a.roonClient = nil
	}
}
