package tokenledger

import (
	"encoding/json"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

// ImportHistorical copies one old reservation into the live ledger.
// Active or unreconciled states become REQUEST_UNKNOWN with cost 0, so
// they block another spend without being treated as a confirmed charge.
// CONFIRMED and CONSUMED become CONFIRMED for that resource only.
// RELEASED and REJECTED are ignored. The same request id is a no-op.
func (l *Ledger) ImportHistorical(now time.Time, site, resourceID, requestID, state string, cost int) (Result, error) {
	if l == nil || l.engine == nil || now.IsZero() || !tokenSite(site) || !validID(resourceID) || !validID(requestID) {
		return Result{}, dataengine.ErrInvalidRequest
	}
	if cost < 0 || cost > maxQuoteBudget {
		cost = 1
	}
	target := StateRequestUnknown
	class := ClassUncertain
	storedCost := 0
	switch state {
	case StateReleased, StateRejected:
		return Result{Class: state, State: state, LedgerRecorded: false}, nil
	case StateConfirmed, StateConsumed:
		target = StateConfirmed
		class = ClassSpent
		if cost < 1 {
			cost = 1
		}
		storedCost = cost
	}
	for range 8 {
		var snap indexDoc
		var raw []byte
		var found bool
		err := l.engine.View(func(v *dataengine.View) error {
			var err error
			snap, raw, found, err = readIndex(v, site)
			return err
		})
		if err != nil {
			return Result{}, err
		}
		if existing, ok, err := l.replay(Quote{Site: site, ResourceID: resourceID, RequestID: requestID, BudgetRef: "migrated", Cost: cost}); err != nil || ok {
			return existing, err
		}
		snap.Items = compactIndex(snap.Items)
		next := row{
			ID: requestID, Site: site, ResourceID: resourceID, RequestID: requestID,
			Cost: storedCost, RequestedCost: cost, State: target, Class: class,
			BudgetRef: "migrated", Budget: maxQuoteBudget, UpdatedNS: now.UnixNano(),
			Note: "migrated", Basis: "从旧账本迁入，先只读对账，不向站点重发令牌请求",
		}
		item := indexItem{ID: next.ID, ResourceID: next.ResourceID, RequestID: next.RequestID, State: next.State, Cost: next.Cost, BudgetRef: next.BudgetRef, UpdatedNS: next.UpdatedNS}
		encodedRow, err := json.Marshal(next)
		if err != nil {
			return Result{}, err
		}
		snap.Generation++
		snap.Items = append(snap.Items, item)
		if len(snap.Items) > maxIndexItems {
			return Result{}, dataengine.ErrInvalidRequest
		}
		encodedIndex, err := json.Marshal(snap)
		if err != nil {
			return Result{}, err
		}
		ok, err := l.engine.CompareAndSwap(
			[]dataengine.Condition{
				{Key: indexKey(site), Absent: !found, Value: raw},
				{Key: rowKey(next.ID), Absent: true},
				{Key: requestKey(site, requestID), Absent: true},
			},
			[]dataengine.Mutation{
				{Key: indexKey(site), Value: encodedIndex},
				{Key: rowKey(next.ID), Value: encodedRow},
				{Key: requestKey(site, requestID), Value: []byte(next.ID)},
			},
		)
		if err != nil {
			return Result{}, err
		}
		if ok {
			return resultFrom(next), nil
		}
	}
	return Result{}, dataengine.ErrInvalidRequest
}

// RemoveImported drops ledger rows whose note is migrated. It rebuilds the
// site index from the rows that remain. It does not contact a site.
func (l *Ledger) RemoveImported(site string, requestIDs []string) error {
	if l == nil || l.engine == nil || !tokenSite(site) {
		return dataengine.ErrInvalidRequest
	}
	want := map[string]bool{}
	for _, id := range requestIDs {
		want[id] = true
	}
	for range 8 {
		var snap indexDoc
		var raw []byte
		var found bool
		err := l.engine.View(func(v *dataengine.View) error {
			var err error
			snap, raw, found, err = readIndex(v, site)
			return err
		})
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		kept := snap.Items[:0]
		var conditions []dataengine.Condition
		var writes []dataengine.Mutation
		conditions = append(conditions, dataengine.Condition{Key: indexKey(site), Value: raw})
		for _, item := range snap.Items {
			if !want[item.RequestID] {
				kept = append(kept, item)
				continue
			}
			rowRaw, ok, err := func() ([]byte, bool, error) {
				var raw []byte
				var found bool
				err := l.engine.View(func(v *dataengine.View) error {
					var err error
					raw, found, err = v.Get(rowKey(item.ID))
					return err
				})
				return raw, found, err
			}()
			if err != nil {
				return err
			}
			if ok {
				var current row
				if json.Unmarshal(rowRaw, &current) == nil && current.Note == "migrated" {
					conditions = append(conditions, dataengine.Condition{Key: rowKey(item.ID), Value: rowRaw})
					writes = append(writes, dataengine.Mutation{Key: rowKey(item.ID), Delete: true})
					conditions = append(conditions, dataengine.Condition{Key: requestKey(site, item.RequestID), Value: []byte(item.ID)})
					writes = append(writes, dataengine.Mutation{Key: requestKey(site, item.RequestID), Delete: true})
					continue
				}
			}
			kept = append(kept, item)
		}
		snap.Items = kept
		snap.Generation++
		encoded, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		writes = append(writes, dataengine.Mutation{Key: indexKey(site), Value: encoded})
		ok, err := l.engine.CompareAndSwap(conditions, writes)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return dataengine.ErrInvalidRequest
}
