package tokenledger

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

func openLedger(t *testing.T) *Ledger {
	t.Helper()
	engine := openAt(t, filepath.Join(t.TempDir(), "data"))
	t.Cleanup(func() { engine.Close() })
	return New(engine)
}

func openAt(t *testing.T, dir string) *dataengine.Engine {
	t.Helper()
	engine, err := dataengine.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func quote(site, resource, request string, cost, balance, budget, margin int) Quote {
	return Quote{
		Site: site, ResourceID: resource, RequestID: request, Cost: cost,
		BalanceKnown: true, Balance: balance, Budget: budget, SafetyMargin: margin,
		ObservedAt: time.Unix(1_700_000_000, 0), BudgetRef: "batch-1",
		Scene: "playlist", IdentityConfirmed: true, StreamingEnabled: true,
	}
}

func TestReserveSpendsOnceAndReplaysTheSameRequest(t *testing.T) {
	ledger := openLedger(t)
	now := time.Unix(1_700_000_100, 0)
	first, err := ledger.Reserve(now, quote("dicmusic", "resource-1", "request-1", 1, 3, 3, 1))
	if err != nil || !first.Spent || first.Cost != 1 || first.Class != ClassReserved || first.Downloaded || first.ProductionVerified || first.FallbackAllowed {
		t.Fatal(first, err)
	}
	again, err := ledger.Reserve(now, quote("dicmusic", "resource-1", "request-1", 1, 3, 3, 1))
	if err != nil || again.ReservationID != first.ReservationID || !again.Spent {
		t.Fatal(again, err)
	}
	var snap indexDoc
	err = ledger.engine.View(func(v *dataengine.View) error {
		var found bool
		var err error
		snap, _, found, err = readIndex(v, "dicmusic")
		if err == nil && !found {
			err = dataengine.ErrInvalidRequest
		}
		return err
	})
	if err != nil || len(snap.Items) != 1 || snap.Items[0].Cost != 1 {
		t.Fatal(snap, err)
	}
}

func TestExhaustedBudgetIsRecordedAndDoesNotSpend(t *testing.T) {
	ledger := openLedger(t)
	now := time.Unix(1_700_000_100, 0)
	observed := time.Unix(1_700_000_000, 0)
	for i, id := range []string{"request-1", "request-2"} {
		got, err := ledger.Reserve(now, quote("orpheus", "resource-"+string(rune('a'+i)), id, 1, 4, 2, 1))
		if err != nil || !got.Spent {
			t.Fatal(i, got, err)
		}
	}
	blocked, err := ledger.Reserve(now, Quote{
		Site: "orpheus", ResourceID: "resource-c", RequestID: "request-3", Cost: 1,
		BalanceKnown: true, Balance: 4, Budget: 2, SafetyMargin: 1, ObservedAt: observed,
		BudgetRef: "batch-1", Scene: "playlist", IdentityConfirmed: true, StreamingEnabled: true,
	})
	if err != nil || blocked.Spent || blocked.Cost != 0 || blocked.Class != ClassTokenBudget || !blocked.LedgerRecorded || !blocked.FallbackAllowed || blocked.Downloaded {
		t.Fatal(blocked, err)
	}
	kept, err := ledger.Reserve(now.Add(time.Second), Quote{
		Site: "orpheus", ResourceID: "resource-d", RequestID: "request-4", Cost: 1,
		BalanceKnown: true, Balance: 4, Budget: 2, SafetyMargin: 1, ObservedAt: observed,
		BudgetRef: "batch-1", Scene: "complete", IdentityConfirmed: true, StreamingEnabled: true,
	})
	if err != nil || kept.FallbackAllowed || kept.Class != ClassTokenBudget || kept.Spent {
		t.Fatal(kept, err)
	}
}

func TestRejectedReservationsDoNotFillTheIndex(t *testing.T) {
	ledger := openLedger(t)
	now := time.Unix(1_700_000_100, 0)
	for i := 0; i < 250; i++ {
		got, err := ledger.Reserve(now, quote("dicmusic", fmt.Sprintf("resource-%d", i), fmt.Sprintf("request-%d", i), 1, 0, 1, 0))
		if err != nil || got.Class != ClassTokenBudget || got.Spent {
			t.Fatal(i, got, err)
		}
	}
	spent, err := ledger.Reserve(now, quote("dicmusic", "resource-ok", "request-ok", 1, 5, 5, 0))
	if err != nil || spent.State != StateReserved || !spent.Spent {
		t.Fatal(spent, err)
	}
	confirmed, err := ledger.Advance(now.Add(time.Second), "dicmusic", "request-ok", StateConfirmed, "settled")
	if err != nil || confirmed.State != StateConfirmed {
		t.Fatal(confirmed, err)
	}
	again, err := ledger.Reserve(now.Add(2*time.Second), quote("dicmusic", "resource-ok", "request-again", 1, 5, 5, 0))
	if err != nil || again.Class != ClassSpent || again.Spent {
		t.Fatal(again, err)
	}
}

func TestUnknownBalanceDoesNotInventTokens(t *testing.T) {
	ledger := openLedger(t)
	now := time.Unix(1_700_000_100, 0)
	got, err := ledger.Reserve(now, Quote{
		Site: "dicmusic", ResourceID: "resource-1", RequestID: "request-1", Cost: 1,
		BalanceKnown: false, Balance: 99, Budget: 3, SafetyMargin: 1,
		BudgetRef: "batch-1", Scene: "fill", IdentityConfirmed: true, StreamingEnabled: true,
	})
	if err != nil || got.Class != ClassBalanceUnknown || got.Spent || got.Cost != 0 || got.FallbackAllowed || !got.LedgerRecorded {
		t.Fatal(got, err)
	}
	var current row
	err = ledger.engine.View(func(v *dataengine.View) error {
		raw, found, err := v.Get(rowKey(got.ReservationID))
		if err != nil || !found || jsonUnmarshal(raw, &current) != nil {
			if err == nil {
				err = dataengine.ErrInvalidRequest
			}
		}
		return err
	})
	if err != nil || current.BalanceKnown || current.Balance != 0 || current.RequestedCost != 1 {
		t.Fatal(current, err)
	}
}

func TestStaleObservationAndOtherSitesDoNotSpend(t *testing.T) {
	ledger := openLedger(t)
	now := time.Unix(1_700_000_100, 0)
	stale := quote("dicmusic", "resource-1", "request-1", 1, 3, 3, 1)
	stale.ObservedAt = now.Add(-balanceTTL - time.Second)
	got, err := ledger.Reserve(now, stale)
	if err != nil || got.Class != ClassBalanceStale || got.Spent || got.FallbackAllowed {
		t.Fatal(got, err)
	}
	for _, site := range []string{"open.cd", "redacted", "netease"} {
		_, err = ledger.Reserve(now, quote(site, "resource-1", "request-2", 1, 3, 3, 1))
		if err == nil {
			t.Fatal(site)
		}
	}
}

func TestInFlightReservationBlocksAnotherSpend(t *testing.T) {
	ledger := openLedger(t)
	now := time.Unix(1_700_000_100, 0)
	if _, err := ledger.Reserve(now, quote("dicmusic", "resource-1", "request-1", 1, 5, 3, 1)); err != nil {
		t.Fatal(err)
	}
	asking, err := ledger.Advance(now.Add(time.Second), "dicmusic", "request-1", StateRequesting, "")
	if err != nil || asking.State != StateRequesting || asking.FallbackAllowed {
		t.Fatal(asking, err)
	}
	blocked, err := ledger.Reserve(now.Add(2*time.Second), quote("dicmusic", "resource-2", "request-2", 1, 5, 3, 1))
	if err != nil || blocked.Class != ClassAccountHold || blocked.Scope != "account" || blocked.Spent || blocked.FallbackAllowed || !blocked.LedgerRecorded {
		t.Fatal(blocked, err)
	}
	same, err := ledger.Reserve(now.Add(2*time.Second), quote("dicmusic", "resource-1", "request-1b", 1, 5, 3, 1))
	if err != nil || same.Class != ClassPending || same.Scope != "resource" || same.Spent {
		t.Fatal(same, err)
	}
	otherSite, err := ledger.Reserve(now.Add(2*time.Second), quote("orpheus", "resource-9", "request-9", 1, 5, 3, 1))
	if err != nil || !otherSite.Spent || otherSite.Class != ClassReserved {
		t.Fatal(otherSite, err)
	}
	if _, err = ledger.Advance(now.Add(3*time.Second), "dicmusic", "request-1", StateReleased, ""); err != ErrReconciliation {
		t.Fatal(err)
	}
	released, err := ledger.Advance(now.Add(4*time.Second), "dicmusic", "request-1", StateReleased, "fixture-reconciled")
	if err != nil || released.Spent || released.State != StateReleased {
		t.Fatal(released, err)
	}
	next, err := ledger.Reserve(now.Add(5*time.Second), quote("dicmusic", "resource-3", "request-3", 1, 5, 3, 1))
	if err != nil || !next.Spent || next.Cost != 1 {
		t.Fatal(next, err)
	}
}

func TestConcurrentReservationsSpendAtMostTheObservedBalance(t *testing.T) {
	ledger := openLedger(t)
	now := time.Unix(1_700_000_100, 0)
	var wg sync.WaitGroup
	results := make([]Result, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "request-a"
			resource := "resource-1"
			if i == 1 {
				id = "request-b"
			}
			results[i], errs[i] = ledger.Reserve(now, quote("orpheus", resource, id, 1, 2, 3, 1))
		}(i)
	}
	wg.Wait()
	spent := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if results[i].Spent {
			spent += results[i].Cost
		}
	}
	if spent != 1 {
		t.Fatalf("spent %d results %#v", spent, results)
	}
}

func TestRejectionSurvivesReopen(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "engine")
	engine, err := dataengine.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_100, 0)
	got, err := New(engine).Reserve(now, Quote{
		Site: "dicmusic", ResourceID: "resource-1", RequestID: "request-1", Cost: 1,
		BalanceKnown: false, Balance: 40, Budget: 3, SafetyMargin: 1,
		BudgetRef: "batch-1", Scene: "playlist", IdentityConfirmed: true, StreamingEnabled: true,
	})
	if err != nil || got.Class != ClassBalanceUnknown || got.Spent {
		t.Fatal(got, err)
	}
	engine.Close()
	reopened, err := dataengine.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	again, err := New(reopened).Reserve(now, Quote{
		Site: "dicmusic", ResourceID: "resource-1", RequestID: "request-1", Cost: 1,
		BalanceKnown: false, Budget: 3, SafetyMargin: 1,
		BudgetRef: "batch-1", Scene: "playlist", IdentityConfirmed: true, StreamingEnabled: true,
	})
	if err != nil || again.ReservationID != got.ReservationID || again.Class != ClassBalanceUnknown || again.Spent || again.Cost != 0 || again.FallbackAllowed {
		t.Fatal(again, err)
	}
}

func jsonUnmarshal(raw []byte, dest *row) error {
	return json.Unmarshal(raw, dest)
}
