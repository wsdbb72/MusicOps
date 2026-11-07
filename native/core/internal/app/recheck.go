package app

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/tokenledger"
)

// recheckToken reads the account document again and applies one correlated
// fixture record. It never requests a torrent and never sends usetoken=1.
// Live DicMusic and Orpheus per-request charge history is not a verified schema
// in this repository, so only musicops.fixture.v1 can move a reservation.
func (a *App) recheckToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Site      string          `json:"site"`
		AccountID string          `json:"account_id"`
		RequestID string          `json:"request_id"`
		Cookie    string          `json:"cookie"`
		Synthetic bool            `json:"synthetic"`
		BaseURL   string          `json:"base_url"`
		Proof     json.RawMessage `json:"proof"`
	}
	if err := decode(r, &body); err != nil || strings.ContainsAny(body.Cookie, "\r\n") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	if body.Site != "dicmusic" && body.Site != "orpheus" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	if body.BaseURL != "" && !body.Synthetic {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "LIVE_SITE_NOT_CALLED", "downloaded": false})
		return
	}
	now := a.now()
	observation := tokenledger.Observation{Contract: tokenledger.BalanceContract, Site: body.Site}
	siteRead := false
	allowed, class, until := a.ledger.ReadAllowed(body.Site, now)
	if !allowed {
		observation.ErrorClass = class
		observation.RecheckAtNS = until
	} else if body.Cookie != "" && body.BaseURL != "" {
		read, called, err := a.observeBalance(r.Context(), body.Site, body.Cookie, body.Synthetic, body.BaseURL)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "BALANCE_UNREAD", "downloaded": false, "production_verified": false})
			return
		}
		observation = read
		siteRead = called
	} else if body.AccountID != "" {
		stored, found, err := a.ledger.Load(body.Site, body.AccountID)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "BALANCE_UNREAD", "downloaded": false, "production_verified": false})
			return
		}
		if found {
			observation = stored
		}
	}
	reservation := tokenledger.Result{Downloaded: false, ProductionVerified: false}
	if len(body.Proof) > 0 && string(body.Proof) != "null" {
		proof, proofClass := tokenledger.ParseSpend(body.Proof)
		if proofClass != "" {
			reservation = tokenledger.Result{Class: proofClass, Scope: "request", Basis: "扣费记录不能关联账号、资源和请求，不改账", Downloaded: false, ProductionVerified: false}
		} else {
			result, err := a.ledger.Reconcile(now, body.Site, proof)
			if err != nil {
				writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "RECONCILE_REJECTED", "downloaded": false, "production_verified": false})
				return
			}
			reservation = result
			reservation.Downloaded = false
			reservation.ProductionVerified = false
		}
	} else if body.RequestID != "" {
		result, found, err := a.ledger.Get(body.Site, body.RequestID)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "RECONCILE_REJECTED", "downloaded": false, "production_verified": false})
			return
		}
		if found {
			reservation = result
			reservation.Downloaded = false
			reservation.ProductionVerified = false
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "downloaded": false, "production_verified": false, "site_read": siteRead,
		"observation": map[string]any{
			"account_id": observation.AccountID, "known": observation.Known, "balance": observation.Balance,
			"error_class": observation.ErrorClass, "generation": observation.Generation,
			"recheck_at_ns": observation.RecheckAtNS, "production_verified": false,
		},
		"reservation": reservation,
	})
}
