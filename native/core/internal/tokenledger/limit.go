package tokenledger

import (
	"encoding/json"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

type readHold struct {
	Class       string `json:"class"`
	RecheckAtNS int64  `json:"recheck_at_ns"`
}

func limitKey(site string) []byte { return []byte("fl/limit/" + site) }

// HoldRead records a transient read restriction for one token site.
// It does not apply to any other site.
func (l *Ledger) HoldRead(site, class string, until time.Time) error {
	if l == nil || l.engine == nil || !tokenSite(site) || until.IsZero() {
		return dataengine.ErrInvalidRequest
	}
	switch class {
	case "rate_limited", "auth", "network", "parse", "read_error":
	default:
		return dataengine.ErrInvalidRequest
	}
	encoded, err := json.Marshal(readHold{Class: class, RecheckAtNS: until.UnixNano()})
	if err != nil {
		return err
	}
	for range 4 {
		var raw []byte
		var found bool
		err = l.engine.View(func(v *dataengine.View) error {
			value, ok, err := v.Get(limitKey(site))
			if err != nil || !ok {
				return err
			}
			found = true
			raw = append([]byte(nil), value...)
			return nil
		})
		if err != nil {
			return err
		}
		ok, err := l.engine.CompareAndSwap(
			[]dataengine.Condition{{Key: limitKey(site), Absent: !found, Value: raw}},
			[]dataengine.Mutation{{Key: limitKey(site), Value: encoded}},
		)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return dataengine.ErrInvalidRequest
}

// ClearReadHold removes a site read restriction after a newer known balance.
func (l *Ledger) ClearReadHold(site string) error {
	if l == nil || l.engine == nil || !tokenSite(site) {
		return dataengine.ErrInvalidRequest
	}
	for range 4 {
		var raw []byte
		var found bool
		err := l.engine.View(func(v *dataengine.View) error {
			value, ok, err := v.Get(limitKey(site))
			if err != nil || !ok {
				return err
			}
			found = true
			raw = append([]byte(nil), value...)
			return nil
		})
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		ok, err := l.engine.CompareAndSwap(
			[]dataengine.Condition{{Key: limitKey(site), Value: raw}},
			[]dataengine.Mutation{{Key: limitKey(site), Delete: true}},
		)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return dataengine.ErrInvalidRequest
}

// ReadAllowed reports whether a read-only account request may be sent.
// A hold that has reached its recheck time allows exactly the next read.
func (l *Ledger) ReadAllowed(site string, now time.Time) (bool, string, int64) {
	if l == nil || l.engine == nil || !tokenSite(site) || now.IsZero() {
		return false, "read_error", 0
	}
	var hold readHold
	var found bool
	err := l.engine.View(func(v *dataengine.View) error {
		raw, ok, err := v.Get(limitKey(site))
		if err != nil || !ok {
			return err
		}
		found = true
		if json.Unmarshal(raw, &hold) != nil {
			return dataengine.ErrInvalidRequest
		}
		return nil
	})
	if err != nil {
		return false, "read_error", now.Add(time.Second).UnixNano()
	}
	if !found || now.UnixNano() >= hold.RecheckAtNS {
		return true, "", 0
	}
	return false, hold.Class, hold.RecheckAtNS
}
