// Package tokenledger records DicMusic and Orpheus free-token reservations in
// the existing embedded engine. It does not contact a site, invent a balance,
// or turn a reservation into a download.
package tokenledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

const (
	StateReserved       = "RESERVED"
	StateRequesting     = "REQUESTING"
	StateUnknown        = "UNKNOWN"
	StateRequestUnknown = "REQUEST_UNKNOWN"
	StateConsumed       = "CONSUMED"
	StateConfirmed      = "CONFIRMED"
	StateReleased       = "RELEASED"
	StateRejected       = "REJECTED"

	ClassReserved    = "reserved"
	ClassTokenBudget = "token_budget"
	// maxQuoteBudget is the largest batch cap the ledger will store. A site
	// balance may be higher; callers must cap the batch instead of copying
	// the whole balance into Budget.
	maxQuoteBudget       = 100
	ClassBalanceUnknown  = "balance_unknown"
	ClassBalanceStale    = "balance_stale"
	ClassPending         = "token_pending"
	ClassAccountHold     = "account_token_hold"
	ClassAlreadyReserved = "already_reserved"
	ClassAuthorization   = "authorization"
	ClassUncertain       = "token_uncertain"
	ClassSpent           = "token_spent"

	balanceTTL = 300 * time.Second
)

// TokenSites are the only sites whose personal free-token button is a spend.
// OpenCD is direct, and RED's search slot is not a token debit here.
var TokenSites = [2]string{"dicmusic", "orpheus"}

var ErrReconciliation = errors.New("token reconciliation required")

// Quote carries a spend request. ReserveObserved replaces Balance with the
// stored site observation; a number on this struct is not a site balance.
type Quote struct {
	Site              string
	AccountID         string
	CookieID          string
	ResourceID        string
	RequestID         string
	Cost              int
	BalanceKnown      bool
	Balance           int
	Budget            int
	SafetyMargin      int
	ObservedAt        time.Time
	BudgetRef         string
	Scene             string
	IdentityConfirmed bool
	StreamingEnabled  bool
}

// Result is a ledger outcome. Downloaded and ProductionVerified stay false.
type Result struct {
	Class              string `json:"class"`
	State              string `json:"state"`
	ReservationID      string `json:"reservation_id,omitempty"`
	Cost               int    `json:"cost"`
	Spent              bool   `json:"spent"`
	LedgerRecorded     bool   `json:"ledger_recorded"`
	FallbackAllowed    bool   `json:"fallback_allowed"`
	Downloaded         bool   `json:"downloaded"`
	ProductionVerified bool   `json:"production_verified"`
	Scope              string `json:"scope,omitempty"`
	Basis              string `json:"basis,omitempty"`
}

type Ledger struct{ engine *dataengine.Engine }

func New(engine *dataengine.Engine) *Ledger { return &Ledger{engine: engine} }

type row struct {
	ID                string `json:"id"`
	Site              string `json:"site"`
	AccountID         string `json:"account_id,omitempty"`
	ResourceID        string `json:"resource_id"`
	RequestID         string `json:"request_id"`
	Cost              int    `json:"cost"`
	RequestedCost     int    `json:"requested_cost"`
	State             string `json:"state"`
	Class             string `json:"class"`
	BudgetRef         string `json:"budget_ref"`
	Balance           int    `json:"balance,omitempty"`
	BalanceKnown      bool   `json:"balance_known"`
	Budget            int    `json:"budget"`
	SafetyMargin      int    `json:"safety_margin"`
	ObservedAtNS      int64  `json:"observed_at_ns,omitempty"`
	UpdatedNS         int64  `json:"updated_ns"`
	Note              string `json:"note,omitempty"`
	Scene             string `json:"scene,omitempty"`
	IdentityConfirmed bool   `json:"identity_confirmed,omitempty"`
	StreamingEnabled  bool   `json:"streaming_enabled,omitempty"`
	Scope             string `json:"scope,omitempty"`
	Basis             string `json:"basis,omitempty"`
}

type indexItem struct {
	ID         string `json:"id"`
	ResourceID string `json:"resource_id"`
	RequestID  string `json:"request_id"`
	State      string `json:"state"`
	Cost       int    `json:"cost"`
	BudgetRef  string `json:"budget_ref"`
	UpdatedNS  int64  `json:"updated_ns"`
}

type indexDoc struct {
	Generation uint64      `json:"generation"`
	Items      []indexItem `json:"items"`
}

func (l *Ledger) Reserve(now time.Time, quote Quote) (Result, error) {
	if l == nil || l.engine == nil || now.IsZero() {
		return Result{}, dataengine.ErrInvalidRequest
	}
	if quote.Budget > maxQuoteBudget {
		quote.Budget = maxQuoteBudget
	}
	if quote.Budget < 0 {
		quote.Budget = 0
	}
	if err := validateQuote(quote); err != nil {
		return Result{}, err
	}
	for range 8 {
		var snap indexDoc
		var raw []byte
		var found bool
		err := l.engine.View(func(v *dataengine.View) error {
			var err error
			snap, raw, found, err = readIndex(v, quote.Site)
			return err
		})
		if err != nil {
			return Result{}, err
		}
		if existing, ok, err := l.replay(quote); err != nil || ok {
			return existing, err
		}
		next, item, reject := decide(now, quote, snap)
		if reject.Class == ClassAlreadyReserved || reject.Class == ClassPending || reject.Class == ClassAccountHold || reject.Class == ClassTokenBudget || reject.Class == ClassBalanceUnknown || reject.Class == ClassBalanceStale || reject.Class == ClassAuthorization || reject.Class == ClassSpent {
			next = rejection(now, quote, reject.Class)
			next.Scope = reject.Scope
			next.Basis = reject.Basis
			item = indexItem{ID: next.ID, ResourceID: next.ResourceID, RequestID: next.RequestID, State: next.State, Cost: 0, BudgetRef: next.BudgetRef, UpdatedNS: next.UpdatedNS}
		}
		encodedRow, err := json.Marshal(next)
		if err != nil {
			return Result{}, err
		}
		snap.Generation++
		snap.Items = append(snap.Items, item)
		if len(snap.Items) > 200 {
			return Result{}, dataengine.ErrInvalidRequest
		}
		encodedIndex, err := json.Marshal(snap)
		if err != nil {
			return Result{}, err
		}
		condition := dataengine.Condition{Key: indexKey(quote.Site), Absent: !found, Value: raw}
		ok, err := l.engine.CompareAndSwap(
			[]dataengine.Condition{
				condition,
				{Key: rowKey(next.ID), Absent: true},
				{Key: requestKey(quote.Site, quote.RequestID), Absent: true},
			},
			[]dataengine.Mutation{
				{Key: indexKey(quote.Site), Value: encodedIndex},
				{Key: rowKey(next.ID), Value: encodedRow},
				{Key: requestKey(quote.Site, quote.RequestID), Value: []byte(next.ID)},
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

// Advance moves one reservation. Leaving an in-flight request requires a note,
// so an unknown spend cannot be dropped from the ledger.
func (l *Ledger) Advance(now time.Time, site, requestID, state, note string) (Result, error) {
	if l == nil || l.engine == nil || now.IsZero() || !tokenSite(site) || !validID(requestID) || !legalState(state) {
		return Result{}, dataengine.ErrInvalidRequest
	}
	if !validNote(note) {
		return Result{}, dataengine.ErrInvalidRequest
	}
	for range 8 {
		var current row
		var rawRow []byte
		var snap indexDoc
		var rawIndex []byte
		var found bool
		err := l.engine.View(func(v *dataengine.View) error {
			id, ok, err := readRequest(v, site, requestID)
			if err != nil || !ok {
				if err == nil {
					err = dataengine.ErrInvalidRequest
				}
				return err
			}
			rawRow, ok, err = v.Get(rowKey(id))
			if err != nil || !ok || json.Unmarshal(rawRow, &current) != nil || current.ID != id {
				if err == nil {
					err = dataengine.ErrInvalidRequest
				}
				return err
			}
			snap, rawIndex, found, err = readIndex(v, site)
			return err
		})
		if err != nil {
			return Result{}, err
		}
		if current.State == state && note == current.Note {
			return resultFrom(current), nil
		}
		if !allow(current.State, state) {
			return Result{}, dataengine.ErrInvalidRequest
		}
		if (state == StateReleased || state == StateRejected) && inFlight(current.State) && note == "" {
			return Result{}, ErrReconciliation
		}
		current.State = state
		current.Note = note
		current.UpdatedNS = now.UnixNano()
		if state == StateReleased || state == StateRejected {
			current.Class = ClassAuthorization
		}
		encodedRow, err := json.Marshal(current)
		if err != nil {
			return Result{}, err
		}
		replaced := false
		for i := range snap.Items {
			if snap.Items[i].ID == current.ID {
				snap.Items[i].State = current.State
				snap.Items[i].UpdatedNS = current.UpdatedNS
				replaced = true
			}
		}
		if !found || !replaced {
			return Result{}, dataengine.ErrInvalidRequest
		}
		snap.Generation++
		encodedIndex, err := json.Marshal(snap)
		if err != nil {
			return Result{}, err
		}
		ok, err := l.engine.CompareAndSwap(
			[]dataengine.Condition{
				{Key: indexKey(site), Value: rawIndex},
				{Key: rowKey(current.ID), Value: rawRow},
			},
			[]dataengine.Mutation{
				{Key: indexKey(site), Value: encodedIndex},
				{Key: rowKey(current.ID), Value: encodedRow},
			},
		)
		if err != nil {
			return Result{}, err
		}
		if ok {
			return resultFrom(current), nil
		}
	}
	return Result{}, dataengine.ErrInvalidRequest
}

func (l *Ledger) replay(quote Quote) (Result, bool, error) {
	var result Result
	var found bool
	err := l.engine.View(func(v *dataengine.View) error {
		id, ok, err := readRequest(v, quote.Site, quote.RequestID)
		if err != nil || !ok {
			return err
		}
		raw, ok, err := v.Get(rowKey(id))
		if err != nil || !ok {
			if err == nil {
				err = dataengine.ErrInvalidRequest
			}
			return err
		}
		var current row
		if json.Unmarshal(raw, &current) != nil || current.RequestID != quote.RequestID || current.Site != quote.Site {
			return dataengine.ErrInvalidRequest
		}
		if current.ResourceID != quote.ResourceID || current.BudgetRef != quote.BudgetRef || current.RequestedCost != quote.Cost {
			return dataengine.ErrInvalidRequest
		}
		result = resultFrom(current)
		found = true
		return nil
	})
	return result, found, err
}

// Get reads one reservation. It does not contact a site or change the ledger.
func (l *Ledger) Get(site, requestID string) (Result, bool, error) {
	var result Result
	var found bool
	err := l.engine.View(func(v *dataengine.View) error {
		id, ok, err := readRequest(v, site, requestID)
		if err != nil || !ok {
			return err
		}
		raw, ok, err := v.Get(rowKey(id))
		if err != nil || !ok {
			if err == nil {
				err = dataengine.ErrInvalidRequest
			}
			return err
		}
		var current row
		if json.Unmarshal(raw, &current) != nil {
			return dataengine.ErrInvalidRequest
		}
		result = resultFrom(current)
		found = true
		return nil
	})
	return result, found, err
}

func decide(now time.Time, quote Quote, snap indexDoc) (row, indexItem, Result) {
	if !quote.BalanceKnown {
		return row{}, indexItem{}, Result{Class: ClassBalanceUnknown}
	}
	if quote.ObservedAt.IsZero() || now.Before(quote.ObservedAt) || now.Sub(quote.ObservedAt) > balanceTTL {
		return row{}, indexItem{}, Result{Class: ClassBalanceStale}
	}
	otherFlight := false
	for _, item := range snap.Items {
		if item.ResourceID == quote.ResourceID && (item.State == StateConfirmed || item.State == StateConsumed) {
			return row{}, indexItem{}, Result{Class: ClassSpent, Scope: "resource", Basis: "该资源的令牌消耗已经确认，不重复扣费"}
		}
		if item.ResourceID == quote.ResourceID && (inFlight(item.State) || active(item.State)) {
			if inFlight(item.State) {
				return row{}, indexItem{}, Result{Class: ClassPending, Scope: "resource", Basis: "该资源已有未完成的令牌请求，不重发，也不把限制扩大到整站"}
			}
			return row{}, indexItem{}, Result{Class: ClassAlreadyReserved, Scope: "resource", Basis: "该资源已有活动预留"}
		}
		if inFlight(item.State) {
			otherFlight = true
		}
	}
	if otherFlight {
		return row{}, indexItem{}, Result{Class: ClassAccountHold, Scope: "account", Basis: "站点只返回账号总余额，无法把未对账扣费分到单个资源；暂停新的令牌请求。只读检索和已证明不消费令牌的获取不在此限制内"}
	}
	spendable := quote.Balance - quote.SafetyMargin
	outstanding, used := totals(snap, quote, quote.ObservedAt.UnixNano())
	if spendable < quote.Cost || outstanding+quote.Cost > spendable || used+quote.Cost > quote.Budget {
		return row{}, indexItem{}, Result{Class: ClassTokenBudget}
	}
	next := row{
		ID: quote.RequestID, Site: quote.Site, AccountID: quote.AccountID, ResourceID: quote.ResourceID, RequestID: quote.RequestID,
		Cost: quote.Cost, RequestedCost: quote.Cost, State: StateReserved, Class: ClassReserved, BudgetRef: quote.BudgetRef,
		Balance: quote.Balance, BalanceKnown: true, Budget: quote.Budget, SafetyMargin: quote.SafetyMargin,
		ObservedAtNS: quote.ObservedAt.UnixNano(), UpdatedNS: now.UnixNano(),
		Scene: quote.Scene, IdentityConfirmed: quote.IdentityConfirmed, StreamingEnabled: quote.StreamingEnabled,
	}
	return next, indexItem{ID: next.ID, ResourceID: next.ResourceID, RequestID: next.RequestID, State: next.State, Cost: next.Cost, BudgetRef: next.BudgetRef, UpdatedNS: next.UpdatedNS}, Result{}
}

func rejection(now time.Time, quote Quote, class string) row {
	balance := 0
	if quote.BalanceKnown && class != ClassBalanceUnknown {
		balance = quote.Balance
	}
	return row{
		ID: quote.RequestID, Site: quote.Site, ResourceID: quote.ResourceID, RequestID: quote.RequestID,
		Cost: 0, RequestedCost: quote.Cost, State: StateRejected, Class: class, BudgetRef: quote.BudgetRef,
		Balance: balance, BalanceKnown: quote.BalanceKnown && class != ClassBalanceUnknown,
		Budget: quote.Budget, SafetyMargin: quote.SafetyMargin, ObservedAtNS: observed(quote), UpdatedNS: now.UnixNano(),
		Scene: quote.Scene, IdentityConfirmed: quote.IdentityConfirmed, StreamingEnabled: quote.StreamingEnabled,
	}
}

func observed(quote Quote) int64 {
	if quote.ObservedAt.IsZero() {
		return 0
	}
	return quote.ObservedAt.UnixNano()
}

func totals(snap indexDoc, quote Quote, observedNS int64) (int, int) {
	outstanding, used := 0, 0
	for _, item := range snap.Items {
		if item.State == StateReleased || item.State == StateRejected {
			continue
		}
		if item.BudgetRef == quote.BudgetRef {
			used += item.Cost
		}
		if (item.State != StateConsumed && item.State != StateConfirmed) || item.UpdatedNS >= observedNS {
			outstanding += item.Cost
		}
	}
	return outstanding, used
}

func resultFrom(current row) Result {
	spent := current.Cost > 0 && current.State != StateRejected && current.State != StateReleased
	return Result{
		Class: current.Class, State: current.State, ReservationID: current.ID, Cost: current.Cost,
		Spent: spent, LedgerRecorded: true, Scope: current.Scope, Basis: current.Basis,
		FallbackAllowed: current.Class == ClassTokenBudget && current.Cost == 0 && current.State == StateRejected && current.IdentityConfirmed && current.StreamingEnabled && fallbackScene(current.Scene),
	}
}

func validateQuote(quote Quote) error {
	if !tokenSite(quote.Site) || !validID(quote.ResourceID) || !validID(quote.RequestID) || !validID(quote.BudgetRef) {
		return dataengine.ErrInvalidRequest
	}
	if quote.Cost < 1 || quote.Cost > maxQuoteBudget || quote.Budget < 0 || quote.Budget > maxQuoteBudget || quote.SafetyMargin < 0 || quote.SafetyMargin > maxQuoteBudget {
		return dataengine.ErrInvalidRequest
	}
	if quote.BalanceKnown && (quote.Balance < 0 || quote.Balance > 1_000_000) {
		return dataengine.ErrInvalidRequest
	}
	if !fallbackScene(quote.Scene) && quote.Scene != "complete" && quote.Scene != "upgrade" && quote.Scene != "expand" {
		return dataengine.ErrInvalidRequest
	}
	return nil
}

func readIndex(v *dataengine.View, site string) (indexDoc, []byte, bool, error) {
	raw, found, err := v.Get(indexKey(site))
	if err != nil || !found {
		return indexDoc{}, nil, false, err
	}
	var snap indexDoc
	if json.Unmarshal(raw, &snap) != nil {
		return indexDoc{}, nil, false, dataengine.ErrInvalidRequest
	}
	return snap, bytes.Clone(raw), true, nil
}

func readRequest(v *dataengine.View, site, requestID string) (string, bool, error) {
	raw, found, err := v.Get(requestKey(site, requestID))
	if err != nil || !found {
		return "", false, err
	}
	return string(raw), true, nil
}

func allow(from, to string) bool {
	switch from {
	case StateReserved:
		return oneOf(to, StateRequesting, StateUnknown, StateRequestUnknown, StateConsumed, StateConfirmed, StateReleased, StateRejected)
	case StateRequesting:
		return oneOf(to, StateUnknown, StateRequestUnknown, StateConsumed, StateConfirmed, StateReleased, StateRejected)
	case StateUnknown:
		return oneOf(to, StateRequestUnknown, StateConsumed, StateConfirmed, StateReleased, StateRejected)
	case StateRequestUnknown:
		return oneOf(to, StateUnknown, StateConsumed, StateConfirmed, StateReleased, StateRejected)
	case StateConsumed:
		return to == StateConfirmed
	default:
		return false
	}
}

func inFlight(state string) bool {
	return state == StateUnknown || state == StateRequestUnknown || state == StateRequesting
}

func active(state string) bool {
	return state != StateReleased && state != StateRejected && state != StateConfirmed && state != StateConsumed
}

func tokenSite(site string) bool {
	return site == TokenSites[0] || site == TokenSites[1]
}

func fallbackScene(scene string) bool { return scene == "playlist" || scene == "fill" }

func legalState(state string) bool {
	switch state {
	case StateReserved, StateRequesting, StateUnknown, StateRequestUnknown, StateConsumed, StateConfirmed, StateReleased, StateRejected:
		return true
	default:
		return false
	}
}

func validID(value string) bool {
	if len(value) < 1 || len(value) > 80 {
		return false
	}
	for i, r := range value {
		digit := r >= '0' && r <= '9'
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		mark := r == ':' || r == '.' || r == '_' || r == '@' || r == '+' || r == '-'
		if i == 0 && !letter && !digit || !letter && !digit && !mark {
			return false
		}
	}
	return true
}

func validNote(note string) bool {
	if len(note) > 80 || bytes.Contains([]byte(note), []byte("://")) {
		return false
	}
	for _, r := range note {
		if r < 0x20 {
			return false
		}
	}
	return true
}

func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}

func indexKey(site string) []byte { return []byte("fl/idx/" + site) }
func rowKey(id string) []byte     { return []byte("fl/row/" + id) }
func requestKey(site, request string) []byte {
	return []byte("fl/req/" + site + "/" + request)
}
