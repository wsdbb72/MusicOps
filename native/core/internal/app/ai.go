// AI settings and the production review model.
//
// Python SetupWizard page 6 carries the AI block (enabled, base_url,
// model, API key, test) and config/schema.py validates it: protocol is
// "responses", enabled requires a model, timeout 1-300, max_output_tokens
// 16-32768, temperature 0-2. The key lives in the Vault under
// vault://ai/default/key, bound to the AI service hostname, exactly like
// Python's auth_secret_ref default. The Go search executor already calls
// a.reviews.Advise(ctx, a.reviewModel, ...) whenever reviewModel is set;
// until this change it was always nil in production, so AI review never
// ran. refreshReviewModel wires it from the saved settings, and every AI
// settings save re-wires it.
//
// When the AI configuration changes and AI is enabled, the cached verdicts
// are dropped (reviews.Reset): the Python setup does the same through
// review_queue.submit({'all': True}), re-queuing needs_review jobs. Go has
// no queued AI-review jobs — advice happens inline in the search executor —
// so dropping the cache is the faithful equivalent: the next search
// re-advises under the new model instead of reusing a verdict advised
// under the old one.
package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/aiclient"
)

// aiSettingsKey is the persisted AI configuration. The key itself is in the
// Vault; this document only says whether one is saved.
var aiSettingsKey = []byte("app/settings/ai")

// aiKeyRef is the Vault reference for the AI key, the same ref Python's
// config/schema.py uses as the ai.auth_secret_ref default.
const aiKeyRef = "vault://ai/default/key"

// aiSettings is the persisted AI configuration, mirroring Python's ai
// section (protocol is always "responses" in this core).
type aiSettings struct {
	Enabled         bool    `json:"enabled"`
	BaseURL         string  `json:"base_url"`
	Model           string  `json:"model"`
	Timeout         int     `json:"timeout"`
	MaxOutputTokens int     `json:"max_output_tokens"`
	Temperature     float64 `json:"temperature"`
	UpdatedNS       int64   `json:"updated_ns"`
}

func defaultAISettings() aiSettings {
	return aiSettings{Timeout: 60, MaxOutputTokens: 2048, Temperature: 0.2}
}

func (a *App) loadAISettings() (aiSettings, error) {
	out := defaultAISettings()
	found, err := a.loadJSONSetting(aiSettingsKey, &out)
	if err != nil {
		return aiSettings{}, err
	}
	if !found {
		return defaultAISettings(), nil
	}
	if out.Timeout == 0 {
		out.Timeout = 60
	}
	if out.MaxOutputTokens == 0 {
		out.MaxOutputTokens = 2048
	}
	return out, nil
}

func (a *App) saveAISettings(in aiSettings) (aiSettings, error) {
	in.UpdatedNS = a.now().UnixNano()
	encoded, err := json.Marshal(in)
	if err != nil {
		return aiSettings{}, err
	}
	if err := a.casSaveJSON(aiSettingsKey, encoded); err != nil {
		return aiSettings{}, err
	}
	return in, nil
}

// aiKeySaved reports whether a key is stored for the given AI host, and its
// vault revision. The key value is never returned.
func (a *App) aiKeySaved(host string) (bool, int) {
	if a.vault == nil || host == "" {
		return false, 0
	}
	_, found, err := a.vault.Get(aiKeyRef, host)
	if err != nil || !found {
		return false, 0
	}
	return true, a.vault.Revision(aiKeyRef)
}

func aiView(settings aiSettings, keySaved bool, keyRevision int) map[string]any {
	return map[string]any{
		"enabled": settings.Enabled, "base_url": settings.BaseURL, "model": settings.Model,
		"timeout": settings.Timeout, "max_output_tokens": settings.MaxOutputTokens,
		"temperature": settings.Temperature, "key_saved": keySaved, "key_revision": keyRevision,
	}
}

// aiSettingsHandler returns the saved AI configuration and key state.
func (a *App) aiSettingsHandler(w http.ResponseWriter, r *http.Request) {
	settings, err := a.loadAISettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	_, host, _ := cleanURL(settings.BaseURL, false)
	saved, revision := a.aiKeySaved(host)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ai": aiView(settings, saved, revision)})
}

// saveAI stores the AI configuration. The key field follows the Python
// wizard's "已保存，输入即可替换": blank keeps the saved key, a value
// replaces it. Changing the base URL clears the key binding, the way
// Python's host-bound secret() does: a key saved for the old host is not
// carried over to the new one.
func (a *App) saveAI(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled         *bool    `json:"enabled"`
		BaseURL         string   `json:"base_url"`
		Model           string   `json:"model"`
		APIKey          string   `json:"api_key"`
		Timeout         *int     `json:"timeout"`
		MaxOutputTokens *int     `json:"max_output_tokens"`
		Temperature     *float64 `json:"temperature"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	previous, err := a.loadAISettings()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	next := previous
	if body.Enabled != nil {
		next.Enabled = *body.Enabled
	}
	if body.BaseURL != "" {
		if _, _, err := cleanURL(body.BaseURL, false); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "ai base_url 无效：" + err.Error()})
			return
		}
		next.BaseURL = strings.TrimSuffix(strings.TrimSpace(body.BaseURL), "/")
	}
	if body.Model != "" {
		next.Model = strings.TrimSpace(body.Model)
	}
	if body.Timeout != nil {
		next.Timeout = *body.Timeout
	}
	if body.MaxOutputTokens != nil {
		next.MaxOutputTokens = *body.MaxOutputTokens
	}
	if body.Temperature != nil {
		next.Temperature = *body.Temperature
	}
	if next.Enabled && strings.TrimSpace(next.Model) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "启用 AI 需要填写模型名称"})
		return
	}
	if next.Timeout < 1 || next.Timeout > 300 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "ai timeout 须为 1-300 秒"})
		return
	}
	if next.MaxOutputTokens < 16 || next.MaxOutputTokens > 32768 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "ai max_output_tokens 须为 16-32768"})
		return
	}
	if next.Temperature < 0 || next.Temperature > 2 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "ai temperature 须为 0-2"})
		return
	}
	changed := next != previous
	_, host, _ := cleanURL(next.BaseURL, false)
	keyChanged := false
	if a.vault == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	if body.APIKey != "" {
		if strings.ContainsAny(body.APIKey, "\r\n") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "api key 不能含换行"})
			return
		}
		if host == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "保存 API Key 需要先填 AI 地址"})
			return
		}
		if _, err := a.vault.Set(aiKeyRef, body.APIKey, host); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
			return
		}
		keyChanged = true
	}
	saved, err := a.saveAISettings(next)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	if (changed || keyChanged) && saved.Enabled {
		// AI changed while enabled: re-advise under the new configuration.
		a.reviews.Reset()
	}
	// A changed base URL without a fresh key unwires the model by itself:
	// refreshReviewModel reads the key bound to the new host and finds
	// none, so reviewModel stays nil until a key for the new host is saved.
	a.refreshReviewModel()
	keySaved, revision := a.aiKeySaved(host)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ai": aiView(saved, keySaved, revision)})
}

// testAI checks the AI connection, mirroring Python's connection_tests ai
// kind: POST {base_url}/responses with the connection-test prompt, success
// when the model answers. Body fields override the saved settings for this
// one test only; changing the address without supplying a key is refused,
// the way Python's address-change protection does it.
func (a *App) testAI(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BaseURL string `json:"base_url"`
		Model   string `json:"model"`
		APIKey  string `json:"api_key"`
	}
	if err := decode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	saved, err := a.loadAISettings()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
		return
	}
	baseURL := saved.BaseURL
	if body.BaseURL != "" {
		baseURL = strings.TrimSuffix(strings.TrimSpace(body.BaseURL), "/")
	}
	model := saved.Model
	if body.Model != "" {
		model = strings.TrimSpace(body.Model)
	}
	if _, _, err := cleanURL(baseURL, false); err != nil || baseURL == "" || model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "AI 地址与模型不能为空"})
		return
	}
	if body.BaseURL != "" && body.BaseURL != saved.BaseURL && body.APIKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": "地址已改变，请填写 API Key"})
		return
	}
	_, host, _ := cleanURL(baseURL, false)
	key := body.APIKey
	if key == "" {
		if a.vault == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "SETTINGS_UNAVAILABLE"})
			return
		}
		stored, found, err := a.vault.Get(aiKeyRef, host)
		if err != nil || !found || stored == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "AI_NOT_CONFIGURED", "error": "尚未保存该地址的 API Key"})
			return
		}
		key = stored
	}
	client, err := aiclient.New(baseURL, model, key)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST", "error": err.Error()})
		return
	}
	a.useAIHTTPClient(client)
	client.Timeout = time.Duration(saved.Timeout) * time.Second
	client.MaxOutputTokens = saved.MaxOutputTokens
	client.Temperature = saved.Temperature
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(saved.Timeout)*time.Second)
	defer cancel()
	if err := client.TestConnection(ctx); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "AI_UNAVAILABLE", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "AI 连接、鉴权及当前模型响应成功"})
}

// refreshReviewModel wires (or unwires) the production review model from
// the saved AI settings. It never fails the open: an unconfigured model
// simply leaves reviewModel nil and search keeps local classification.
func (a *App) refreshReviewModel() {
	settings, err := a.loadAISettings()
	if err != nil || !settings.Enabled || settings.BaseURL == "" || settings.Model == "" || a.vault == nil {
		a.reviewModel = nil
		return
	}
	_, host, err := cleanURL(settings.BaseURL, false)
	if err != nil {
		a.reviewModel = nil
		return
	}
	key, found, err := a.vault.Get(aiKeyRef, host)
	if err != nil || !found || key == "" {
		a.reviewModel = nil
		return
	}
	client, err := aiclient.New(settings.BaseURL, settings.Model, key)
	if err != nil {
		a.reviewModel = nil
		return
	}
	a.useAIHTTPClient(client)
	client.Timeout = time.Duration(settings.Timeout) * time.Second
	client.MaxOutputTokens = settings.MaxOutputTokens
	client.Temperature = settings.Temperature
	a.reviewModel = client
}

// useAIHTTPClient applies the App's aiHTTPClient seam to a freshly built
// AI client. Nil in production; tests point it at a fake server.
func (a *App) useAIHTTPClient(client *aiclient.Client) {
	if a.aiHTTPClient != nil {
		client.HTTPClient = a.aiHTTPClient
	}
}
