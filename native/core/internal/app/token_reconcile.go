// Background token reconciliation. A token request whose answer never arrived
// is expensive to resolve: the ledger cannot tell "the site charged one token"
// from "the site never saw the request". The sweep below reads the account
// balance again and settles the row from that comparison, so a stuck request
// stops blocking the site with evidence instead of only with a deadline.
//
// It never sends a request to a tracker itself: the balance read goes through
// the same site client and the same 60-second-per-site read hold as the
// foreground path, and it only uses a saved cookie. A site without a saved
// cookie is left to the 30-minute expiry.
package app

import (
	"context"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/tokenledger"
)

const (
	// tokenReconcileInterval is how often the sweep looks for unsettled rows.
	tokenReconcileInterval = 30 * time.Second
	// tokenReconcileMinAge keeps a row that was just created out of the first
	// sweep: the foreground path may still be writing its own verdict.
	tokenReconcileMinAge = 90 * time.Second
	// tokenReleaseMinAge is the "two reads, no change" window. The
	// reservation's own observation is the first read; only a later read at
	// least this far apart counts as the second one.
	tokenReleaseMinAge = 5 * time.Minute
	// tokenReconcileBudget bounds one sweep so a slow site cannot hold the
	// ticker for long.
	tokenReconcileBudget = 20 * time.Second
	// tokenReconcileReadGap is the minimum gap between two background balance
	// reads of the same site.
	tokenReconcileReadGap = 60 * time.Second
)

// runTokenReconciler is the loop started by Open and stopped by Close.
func (a *App) runTokenReconciler(stop <-chan struct{}) {
	ticker := time.NewTicker(tokenReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), tokenReconcileBudget)
			a.reconcileTokens(ctx, a.now())
			cancel()
		}
	}
}

// reconcileTokens settles unanswered token requests from the account balance.
// It returns the number of rows it moved, which is what a test asserts on.
func (a *App) reconcileTokens(ctx context.Context, now time.Time) int {
	if a == nil || a.ledger == nil {
		return 0
	}
	// A paused pipeline does not read sites in the background either.
	if a.pipelinePaused() {
		return 0
	}
	// Expiry first: a row past the deadline must leave the in-flight set even
	// if the balance cannot be read.
	_, _ = a.ledger.SweepExpired(now, tokenledger.UnknownExpire)
	pending, err := a.ledger.Pending()
	if err != nil {
		return 0
	}
	bySite := map[string][]tokenledger.Pending{}
	for _, row := range pending {
		if now.Sub(time.Unix(0, row.UpdatedNS)) < tokenReconcileMinAge {
			continue
		}
		bySite[row.Site] = append(bySite[row.Site], row)
	}
	moved := 0
	for site, rows := range bySite {
		if !a.tokenReadAllowed(site, now) {
			continue
		}
		// A saved cookie is the only credential a background sweep may use.
		cookie, ok := a.savedCookie(site)
		if !ok || cookie == "" {
			continue
		}
		a.markTokenRead(site, now)
		observation, called, err := a.observeBalance(ctx, site, cookie, false, "")
		if err != nil || !called || !observation.Known || observation.ErrorClass != "" {
			continue
		}
		for _, row := range rows {
			state, note := reconcileVerdict(now, row, observation)
			if state == "" {
				continue
			}
			if _, err := a.ledger.Advance(now, site, row.RequestID, state, note); err == nil {
				moved++
			}
		}
	}
	return moved
}

// tokenReadAllowed keeps the background reads to one per site per
// tokenReconcileReadGap, independent of the ledger's failure holds.
func (a *App) tokenReadAllowed(site string, now time.Time) bool {
	a.reconcileMu.Lock()
	defer a.reconcileMu.Unlock()
	if last, ok := a.lastTokenRead[site]; ok && now.Sub(last) < tokenReconcileReadGap {
		return false
	}
	return true
}

func (a *App) markTokenRead(site string, now time.Time) {
	a.reconcileMu.Lock()
	defer a.reconcileMu.Unlock()
	if a.lastTokenRead == nil {
		a.lastTokenRead = map[string]time.Time{}
	}
	a.lastTokenRead[site] = now
}

// reconcileVerdict is the decision table for one unsettled row:
//
//   - balance lower than the reservation saw -> the token was spent: CONFIRMED
//   - balance unchanged after the release window -> it was not: RELEASED
//   - balance higher, or too early, or nothing known -> wait for the next sweep
//
// A row without a known balance at reservation time cannot be judged; it is
// left to the expiry deadline.
func reconcileVerdict(now time.Time, row tokenledger.Pending, observation tokenledger.Observation) (string, string) {
	if !row.BalanceKnown || !observation.Known {
		return "", ""
	}
	if observation.Balance < row.Balance {
		return tokenledger.StateConfirmed, "对账：站点余额已减少，判定这次请求扣了令牌"
	}
	if observation.Balance > row.Balance {
		// Tokens were added since the reservation: no conclusion about the
		// spend, and the difference cannot be used as evidence either way.
		return "", ""
	}
	if row.ObservedAtNS == 0 || now.Sub(time.Unix(0, row.ObservedAtNS)) < tokenReleaseMinAge {
		return "", ""
	}
	return tokenledger.StateReleased, "对账：余额两次读数一致，判定这次请求没有扣费"
}
