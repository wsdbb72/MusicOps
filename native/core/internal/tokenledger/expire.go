package tokenledger

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

// UnknownExpire is how long a token request may stay unanswered. After it the
// row leaves the in-flight set as EXPIRED: the site stops being held (one
// unknown spend must not block every later request on that site) while the
// resource still refuses a second usetoken, because no evidence says the
// first request was free.
const UnknownExpire = 30 * time.Minute

// Pending is one unsettled token request: a row that may have spent a token
// and has no answer yet.
type Pending struct {
	Site       string
	RequestID  string
	ResourceID string
	State      string
	Cost       int
	UpdatedNS  int64
	// The row's own balance observation at reservation time. A reconciler
	// compares it with a fresh read to decide whether the spend happened.
	Balance       int
	BalanceKnown  bool
	ObservedAtNS  int64
}

// Pending lists the unsettled rows of every token site. It reads the per-site
// index only: the rows it returns are the ones an operator or a reconciler has
// to resolve.
func (l *Ledger) Pending() ([]Pending, error) {
	if l == nil || l.engine == nil {
		return nil, dataengine.ErrInvalidRequest
	}
	out := []Pending{}
	for _, site := range TokenSites {
		var snap indexDoc
		err := l.engine.View(func(v *dataengine.View) error {
			doc, _, _, err := readIndex(v, site)
			snap = doc
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, item := range snap.Items {
			if !inFlight(item.State) {
				continue
			}
			// The row carries the balance that was observed when the token was
			// reserved; a reconciler compares it with a fresh read. A row that
			// cannot be read is skipped rather than reported with a zero
			// balance, which would look like a confirmed spend.
			var current row
			readErr := l.engine.View(func(v *dataengine.View) error {
				raw, ok, err := v.Get(rowKey(item.ID))
				if err != nil || !ok {
					return err
				}
				return json.Unmarshal(raw, &current)
			})
			if readErr != nil {
				continue
			}
			out = append(out, Pending{
				Site: site, RequestID: item.RequestID, ResourceID: item.ResourceID,
				State: item.State, Cost: item.Cost, UpdatedNS: item.UpdatedNS,
				Balance: current.Balance, BalanceKnown: current.BalanceKnown, ObservedAtNS: current.ObservedAtNS,
			})
		}
	}
	return out, nil
}

// SweepExpired moves every request that has been unanswered for maxAge to
// EXPIRED and returns the rows it moved. A row that changed under it (for
// example a user recheck that confirmed the spend) is skipped, not forced:
// Advance refuses the transition and the sweeper keeps going.
func (l *Ledger) SweepExpired(now time.Time, maxAge time.Duration) ([]Pending, error) {
	if l == nil || l.engine == nil || now.IsZero() || maxAge <= 0 {
		return nil, dataengine.ErrInvalidRequest
	}
	pending, err := l.Pending()
	if err != nil {
		return nil, err
	}
	expired := []Pending{}
	for _, item := range pending {
		if now.Sub(time.Unix(0, item.UpdatedNS)) < maxAge {
			continue
		}
		if _, err := l.Advance(now, item.Site, item.RequestID, StateExpired, "超过对账时限仍未拿到站点回答，自动到期；未证明扣费"); err != nil {
			if errors.Is(err, dataengine.ErrInvalidRequest) {
				// The row moved (confirmed, released, ...) between the index
				// read and the transition. Its new state wins.
				continue
			}
			return expired, err
		}
		expired = append(expired, item)
	}
	return expired, nil
}
