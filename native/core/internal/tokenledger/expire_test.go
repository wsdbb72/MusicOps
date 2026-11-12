package tokenledger

import (
	"testing"
	"time"
)

// quoteAt is one reservation quote whose balance observation is as fresh as
// the call that uses it.
func quoteAt(site, resource, request string, cost, balance, budget int, observed time.Time) Quote {
	entry := quote(site, resource, request, cost, balance, budget, 0)
	entry.ObservedAt = observed
	return entry
}

// One unanswered request used to hold its whole site forever: every later
// token request was refused as ACCOUNT_HOLD. After the deadline the row
// expires, the site is usable again, and the same resource still cannot spend
// a second token (P1-6, D3).
func TestSweepExpiredReleasesTheSiteButNotTheResource(t *testing.T) {
	ledger := openLedger(t)
	start := time.Unix(1_700_000_100, 0)
	first, err := ledger.Reserve(start, quoteAt("dicmusic", "resource-1", "request-1", 1, 5, 5, start))
	if err != nil || !first.Spent {
		t.Fatal(first, err)
	}
	if _, err := ledger.Advance(start, "dicmusic", "request-1", StateRequestUnknown, "站点没有回答"); err != nil {
		t.Fatal(err)
	}

	// While the request is unanswered, a different resource is held.
	held, err := ledger.Reserve(start.Add(time.Minute), quoteAt("dicmusic", "resource-2", "request-2", 1, 5, 5, start.Add(time.Minute)))
	if err != nil || held.Spent || held.Class != ClassAccountHold {
		t.Fatal("expected an account hold while the spend is unanswered:", held, err)
	}

	// Before the deadline nothing expires.
	if expired, err := ledger.SweepExpired(start.Add(29*time.Minute), UnknownExpire); err != nil || len(expired) != 0 {
		t.Fatal(len(expired), err)
	}

	// After it the row leaves the in-flight set without pretending the token
	// was not spent.
	expired, err := ledger.SweepExpired(start.Add(31*time.Minute), UnknownExpire)
	if err != nil || len(expired) != 1 || expired[0].RequestID != "request-1" {
		t.Fatal(expired, err)
	}
	row, found, err := ledger.Get("dicmusic", "request-1")
	if err != nil || !found || row.State != StateExpired {
		t.Fatal(row, found, err)
	}
	pending, err := ledger.Pending()
	if err != nil || len(pending) != 0 {
		t.Fatal("an expired row is still pending:", pending, err)
	}

	// The site is free again for a different resource.
	second, err := ledger.Reserve(start.Add(31*time.Minute), quoteAt("dicmusic", "resource-2", "request-3", 1, 5, 5, start.Add(31*time.Minute)))
	if err != nil || !second.Spent {
		t.Fatal("the site stayed held after the expiry:", second, err)
	}

	// The same resource still refuses a second usetoken: no proof says the
	// first request was free.
	again, err := ledger.Reserve(start.Add(32*time.Minute), quoteAt("dicmusic", "resource-1", "request-4", 1, 5, 5, start.Add(32*time.Minute)))
	if err != nil || again.Spent || again.Class != ClassAlreadyReserved {
		t.Fatal("a second usetoken for an expired resource was allowed:", again, err)
	}
}

// A row that reached a final state before the sweep is not touched: Advance
// refuses the transition and the sweeper moves on.
func TestSweepExpiredSkipsRowsThatAlreadySettled(t *testing.T) {
	ledger := openLedger(t)
	start := time.Unix(1_700_000_200, 0)
	if _, err := ledger.Reserve(start, quoteAt("orpheus", "resource-9", "request-9", 1, 4, 4, start)); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Advance(start, "orpheus", "request-9", StateConfirmed, "站点已确认"); err != nil {
		t.Fatal(err)
	}
	if expired, err := ledger.SweepExpired(start.Add(time.Hour), UnknownExpire); err != nil || len(expired) != 0 {
		t.Fatal(expired, err)
	}
	row, found, err := ledger.Get("orpheus", "request-9")
	if err != nil || !found || row.State != StateConfirmed {
		t.Fatal(row, found, err)
	}
}

// Pending reports every unsettled site independently, so one site's silence
// does not hide another's.
func TestPendingListsEachSite(t *testing.T) {
	ledger := openLedger(t)
	start := time.Unix(1_700_000_300, 0)
	if _, err := ledger.Reserve(start, quoteAt("dicmusic", "resource-a", "request-a", 1, 5, 5, start)); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Reserve(start, quoteAt("orpheus", "resource-b", "request-b", 1, 5, 5, start)); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Advance(start, "dicmusic", "request-a", StateRequestUnknown, "没有回答"); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Advance(start, "orpheus", "request-b", StateRequesting, "请求已发出"); err != nil {
		t.Fatal(err)
	}
	pending, err := ledger.Pending()
	if err != nil || len(pending) != 2 {
		t.Fatal(pending, err)
	}
	seen := map[string]bool{}
	for _, item := range pending {
		seen[item.Site+"/"+item.RequestID] = true
	}
	if !seen["dicmusic/request-a"] || !seen["orpheus/request-b"] {
		t.Fatal(seen)
	}
}
