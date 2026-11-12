package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/scheduler"
	"github.com/wsdbb72/MusicOps/native/core/internal/sitefree"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/tokenledger"
	"github.com/wsdbb72/MusicOps/native/core/internal/torrentfetch"
	"github.com/wsdbb72/MusicOps/native/core/internal/trackersearch"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func (a *App) acquire(ctx context.Context, execution *scheduler.Execution, payload []byte) (scheduler.Outcome, error) {
	if err := a.cancelled(ctx, execution); err != nil {
		return scheduler.Outcome{}, err
	}
	// The validating stage task carries a publish payload, not a search
	// plan, so the task and job are read before the plan is decoded: the
	// validating branch below never decodes a search plan at all.
	held := execution.Token()
	task, err := a.tasks.Get(held.ID)
	if err != nil || task == nil {
		return scheduler.Outcome{}, err
	}
	job, err := a.flows.Job(task.WorkflowJobID)
	if err != nil || job == nil {
		return scheduler.Outcome{}, err
	}
	if job.Stage == "validating" {
		// The download executor advanced the job from downloading to
		// validating and queued this task with the ingest receipts; see
		// finishValidating in download.go.
		return a.finishValidating(execution, held, *job, payload)
	}
	plan, err := decodePlan(payload)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	defer a.forget(plan.RootJobID)
	if job.Stage == "fallback" {
		return a.acquireFallback(ctx, execution, held, *job, plan)
	}
	if job.Stage == "downloading" {
		return a.finishAcquire(held, *job, "candidate", "下载尚未接入；种子保存不等于已下载、尚未入库", "", nil)
	}
	if job.Stage != "requesting" {
		return a.finishAcquire(held, *job, "candidate", "当前阶段不能获取；尚未下载、尚未入库", "", nil)
	}
	site, resource, ok := strings.Cut(plan.SelectedCandidateID, ":")
	if !ok || site == "" || site != plan.SelectedSite || resource == "" {
		return a.finishAcquire(held, *job, "candidate", "没有唯一候选，不能获取；尚未下载、尚未入库", "", nil)
	}
	if a.cookieFor(plan.RootJobID, site) == "" {
		return a.finishAcquire(held, *job, "candidate", "没有可用的登录状态，不能获取；尚未下载、尚未入库", "", nil)
	}
	decision, err := a.readEvidence(ctx, plan, site, resource)
	if err != nil {
		return a.finishAcquire(held, *job, "candidate", "站点详情读取失败，未发下载或令牌请求；尚未下载、尚未入库", "detail", nil)
	}
	// Personal free is free everywhere. Global free (a site-wide freeleech
	// event, proven for this exact torrent row) costs no token on any of the
	// three token sites either: charging a token for a global-free torrent is
	// a real loss with no benefit. sitefree only reports Global when the
	// response was about this resource id, so a stray flag cannot route an
	// unrelated torrent here.
	if decision.Personal || decision.Global {
		receipt, err := a.fetchTorrent(ctx, execution, plan, site, resource, false)
		if err != nil {
			return a.finishAcquire(held, *job, "candidate", "种子响应不是有效元信息；尚未下载、尚未入库", "PARSE_ERROR", nil)
		}
		return a.finishAcquire(held, *job, "candidate", "种子字节已保存，专辑尚未下载、尚未入库", "", &receipt)
	}
	if tokenledger.IsTokenSite(site) && decision.TokenCost > 0 {
		return a.acquireWithToken(ctx, execution, held, *job, plan, site, resource, decision)
	}
	return a.finishAcquire(held, *job, "token", "站点页面没有个人免费，也没有唯一的令牌按钮，未发令牌请求；尚未下载、尚未入库", "evidence", nil)
}

func (a *App) readEvidence(ctx context.Context, plan SearchPlan, site, resource string) (sitefree.Decision, error) {
	query := trackersearch.Query{Site: site, Cookie: a.cookieFor(plan.RootJobID, site), Synthetic: plan.Synthetic, BaseURL: plan.BaseURL, SolverURL: a.solverURL()}
	var bodies [][]byte
	if site == "open.cd" {
		body, err := a.searcher.ReadDetail(ctx, query, "/details.php", url.Values{"id": {resource}})
		if err != nil {
			return sitefree.Decision{}, err
		}
		bodies = append(bodies, body)
	} else {
		body, err := a.searcher.ReadDetail(ctx, query, "/ajax.php", url.Values{"action": {"torrent"}, "id": {resource}})
		if err != nil {
			return sitefree.Decision{}, err
		}
		bodies = append(bodies, body)
		if group := sitefree.GroupID(body, resource); group != "" {
			page, err := a.searcher.ReadDetail(ctx, query, "/torrents.php", url.Values{"id": {group}, "torrentid": {resource}})
			if err != nil {
				return sitefree.Decision{}, err
			}
			bodies = append(bodies, page)
		}
	}
	return sitefree.Read(site, resource, bodies, a.now())
}

func (a *App) acquireWithToken(ctx context.Context, execution *scheduler.Execution, held taskrepo.Token, job workflow.Job, plan SearchPlan, site, resource string, decision sitefree.Decision) (scheduler.Outcome, error) {
	if !tokenledger.IsTokenSite(site) {
		return a.finishAcquire(held, job, "token", "该站不使用这条令牌账本；尚未下载、尚未入库", "", nil)
	}
	// One unanswered request used to hold the whole site forever: every later
	// token request was refused as ACCOUNT_HOLD. Release the ones that passed
	// the reconciliation deadline before looking at this site again (P1-6).
	// The sweep is lazy on purpose until a background reconciler exists; this
	// is the point where a held site would block a user.
	_, _ = a.ledger.SweepExpired(a.now(), tokenledger.UnknownExpire)
	observation, _, err := a.observeBalance(ctx, site, a.cookieFor(plan.RootJobID, site), plan.Synthetic, plan.BaseURL)
	if err != nil {
		return a.finishAcquire(held, job, "token", "余额读取失败，未发令牌请求；尚未下载、尚未入库", observation.ErrorClass, nil)
	}
	if bound, bindErr := a.bindPageBalance(observation, decision); bindErr != nil {
		return a.finishAcquire(held, job, "token", "余额读取失败，未发令牌请求；尚未下载、尚未入库", "read_error", nil)
	} else {
		observation = bound
	}
	if !observation.Known || observation.ErrorClass != "" {
		message := "站点余额未知或无法绑定账号，未发令牌请求；尚未下载、尚未入库"
		class := observation.ErrorClass
		if class == "" {
			class = tokenledger.ClassBalanceUnknown
		}
		switch class {
		case "rate_limited", "auth", "network", "parse", "read_error":
			message = "站点限制或读取失败只暂停这次账号读取，未发令牌请求；尚未下载、尚未入库"
		}
		return a.finishAcquire(held, job, "token", message, class, nil)
	}
	if decision.Balance != nil && *decision.Balance != observation.Balance {
		return a.finishAcquire(held, job, "token", "页面令牌数与账号余额不一致，未发令牌请求；尚未下载、尚未入库", "balance_conflict", nil)
	}
	quote := tokenledger.Quote{
		Site: site, AccountID: observation.AccountID, CookieID: tokenledger.Fingerprint(a.cookieFor(plan.RootJobID, site)),
		ResourceID: resource, RequestID: job.ID, Cost: decision.TokenCost,
		Budget: plan.Budget, SafetyMargin: plan.SafetyMargin, BudgetRef: job.BatchID, Scene: job.Scene,
		IdentityConfirmed: plan.Identity.IdentityConfirmed, StreamingEnabled: plan.StreamingEnabled,
	}
	reserved, err := a.ledger.ReserveObserved(a.now(), quote, observation.AccountID)
	if err != nil {
		// The site has not been asked yet. A ledger rejection stays on this
		// task for review instead of failing the executor, which would retry
		// the same quote.
		return a.finishAcquire(held, job, "token", "令牌预留没有成立，未向站点请求；尚未下载、尚未入库", "ledger_rejected", nil)
	}
	switch reserved.State {
	case tokenledger.StateRequesting, tokenledger.StateRequestUnknown, tokenledger.StateUnknown:
		// The token request was already sent or may have been sent. Read the
		// site again and do not add usetoken a second time.
		return a.reconcileInFlightToken(ctx, execution, held, job, plan, site, resource)
	case tokenledger.StateReserved:
	default:
		return a.finishAcquire(held, job, "token", "令牌预留没有成立，未向站点请求；尚未下载、尚未入库", reserved.Class, nil)
	}
	if allowed, class, _ := a.ledger.ReadAllowed(site, a.now()); !allowed {
		if _, err = a.ledger.Advance(a.now(), site, job.ID, tokenledger.StateReleased, "read-hold"); err != nil {
			return scheduler.Outcome{}, err
		}
		return a.finishAcquire(held, job, "token", "站点限制了账号读取，未发令牌请求；尚未下载、尚未入库", class, nil)
	}
	if _, err = a.ledger.Advance(a.now(), site, job.ID, tokenledger.StateRequesting, ""); err != nil {
		return scheduler.Outcome{}, err
	}
	receipt, fetchErr := a.fetchTorrent(ctx, execution, plan, site, resource, true)
	var receiptPtr *torrentfetch.Receipt
	if fetchErr == nil {
		receiptPtr = &receipt
	}
	// A fixture proof is an explicit charge record. It settles the reservation
	// before the site is read again. Torrent bytes alone are not a proof.
	if proof, found := a.confirmSpend(ctx, site, job.ID); found {
		if reconciled, err := a.ledger.Reconcile(a.now(), site, proof); err == nil && reconciled.Class != tokenledger.ClassUncertain {
			switch proof.State {
			case "CONSUMED":
				if fetchErr != nil {
					return a.finishAcquire(held, job, "token", "令牌已确认消耗，但获取未完成；尚未下载、尚未入库", "SPEND_CONFIRMED", nil)
				}
				return a.finishConfirmedToken(held, job, "令牌已确认消耗，种子已保存；尚未下载、尚未入库", "SPEND_CONFIRMED", receiptPtr)
			case "NOT_CONSUMED":
				if fetchErr != nil {
					return a.finishAcquire(held, job, "token", "令牌未消耗，但获取未完成；尚未下载、尚未入库", "SPEND_NOT_CONSUMED", nil)
				}
				return a.finishAcquire(held, job, "token", "令牌未消耗，种子已保存；尚未下载、尚未入库", "SPEND_NOT_CONSUMED", receiptPtr)
			}
		}
	}
	if outcome, settled, err := a.confirmTokenFromSite(ctx, execution, held, job, plan, site, resource, receiptPtr); err != nil || settled {
		return outcome, err
	}
	note := "种子已保存，免费对账未完成"
	if fetchErr != nil {
		note = "获取未完成，需要核对是否已消耗"
	}
	if _, err = a.ledger.Advance(a.now(), site, job.ID, tokenledger.StateRequestUnknown, note); err != nil {
		return scheduler.Outcome{}, err
	}
	if fetchErr != nil {
		return a.finishAcquire(held, job, "token", "获取未完成，令牌状态待对账；尚未下载、尚未入库", "REQUEST_UNKNOWN", nil)
	}
	return a.finishAcquire(held, job, "token", "种子字节已保存，免费是否生效尚未对账；尚未下载、尚未入库", "REQUEST_UNKNOWN", receiptPtr)
}

// bindPageBalance uses the unique total printed on the torrent page when the
// account document has an id but no token field. The number stays bound to
// that account. A known API balance is never replaced, so a disagreement
// still blocks the spend.
func (a *App) bindPageBalance(observation tokenledger.Observation, decision sitefree.Decision) (tokenledger.Observation, error) {
	if observation.Known || observation.AccountID == "" || observation.ErrorClass != tokenledger.ClassBalanceUnknown || decision.Balance == nil {
		return observation, nil
	}
	observation.ReadBase = observation.Generation
	observation.ReadBaseSet = true
	observation.Generation = 0
	observation.Known = true
	observation.Balance = *decision.Balance
	observation.ErrorClass = ""
	observation.Source = "page"
	committed, err := a.ledger.CommitObservation(observation)
	if err != nil {
		return tokenledger.Observation{}, err
	}
	if committed.Known && committed.ErrorClass == "" {
		_ = a.ledger.ClearReadHold(observation.Site)
	}
	return committed, nil
}

// tokenEffectConfirmed reports the site evidence that a token spend took
// effect for this user. That is personal freeleech on the API or in this
// torrent's own row. A global isFreeleech flag, a site-wide 「免费」 label,
// and the saved torrent bytes do not prove the token was consumed.
func tokenEffectConfirmed(decision sitefree.Decision) bool {
	return decision.Personal || decision.RowFree == "personal"
}

// confirmTokenFromSite re-reads the torrent after the token request. When
// the site now shows the free label, the reservation is confirmed and the
// torrent already in hand can be downloaded. A read failure or a page that
// still is not free leaves the reservation for the caller to mark uncertain.
// This read never sends usetoken.
func (a *App) confirmTokenFromSite(ctx context.Context, execution *scheduler.Execution, held taskrepo.Token, job workflow.Job, plan SearchPlan, site, resource string, receipt *torrentfetch.Receipt) (scheduler.Outcome, bool, error) {
	after, err := a.readEvidence(ctx, plan, site, resource)
	if err != nil || !tokenEffectConfirmed(after) {
		return scheduler.Outcome{}, false, nil
	}
	if _, err = a.ledger.Advance(a.now(), site, job.ID, tokenledger.StateConfirmed, "site-personal-free"); err != nil {
		return scheduler.Outcome{}, false, err
	}
	if receipt != nil {
		outcome, err := a.finishConfirmedToken(held, job, "站点已确认免费生效，种子已保存；尚未下载、尚未入库", "", receipt)
		return outcome, true, err
	}
	return a.saveFreeTorrent(ctx, execution, held, job, plan, site, resource)
}

// reconcileInFlightToken runs when this request is already in flight. It
// never sends usetoken again. A site free label confirms the reservation
// and then downloads the torrent without a token; otherwise the request
// stays uncertain.
func (a *App) reconcileInFlightToken(ctx context.Context, execution *scheduler.Execution, held taskrepo.Token, job workflow.Job, plan SearchPlan, site, resource string) (scheduler.Outcome, error) {
	if outcome, settled, err := a.confirmTokenFromSite(ctx, execution, held, job, plan, site, resource, nil); err != nil || settled {
		return outcome, err
	}
	current, found, err := a.ledger.Get(site, job.ID)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	if found && current.State == tokenledger.StateRequesting {
		if _, err = a.ledger.Advance(a.now(), site, job.ID, tokenledger.StateRequestUnknown, "对账后仍未确认免费"); err != nil {
			return scheduler.Outcome{}, err
		}
	}
	return a.finishAcquire(held, job, "token", "令牌请求仍待对账，未再次向站点使用令牌；尚未下载、尚未入库", "REQUEST_UNKNOWN", nil)
}

// saveFreeTorrent stores the torrent with the ordinary download link after
// the site has already shown that this user does not need another token.
func (a *App) saveFreeTorrent(ctx context.Context, execution *scheduler.Execution, held taskrepo.Token, job workflow.Job, plan SearchPlan, site, resource string) (scheduler.Outcome, bool, error) {
	receipt, err := a.fetchTorrent(ctx, execution, plan, site, resource, false)
	if err != nil {
		outcome, finishErr := a.finishAcquire(held, job, "token", "免费已确认，但种子没有保存；未再次使用令牌；尚未下载、尚未入库", "SPEND_CONFIRMED", nil)
		return outcome, true, finishErr
	}
	outcome, finishErr := a.finishConfirmedToken(held, job, "站点已确认免费生效，种子已保存；尚未下载、尚未入库", "", &receipt)
	return outcome, true, finishErr
}

// finishConfirmedToken records a settled token spend as a candidate the
// download lane can start. The album is still not in the library.
func (a *App) finishConfirmedToken(held taskrepo.Token, job workflow.Job, message, class string, receipt *torrentfetch.Receipt) (scheduler.Outcome, error) {
	return a.finishAcquire(held, job, "candidate", message, class, receipt)
}

// confirmSpend asks the registered spend source for a correlated charge
// record for this request. It returns false when there is no source, no
// record, or the source failed: none of those is a proof, and the caller
// keeps the reservation REQUEST_UNKNOWN.
func (a *App) confirmSpend(ctx context.Context, site, requestID string) (tokenledger.SpendProof, bool) {
	if a.spendSource == nil {
		return tokenledger.SpendProof{}, false
	}
	proof, found, err := a.spendSource.Spend(ctx, site, requestID)
	if err != nil || !found {
		return tokenledger.SpendProof{}, false
	}
	return proof, true
}

func (a *App) observeBalance(ctx context.Context, site, cookie string, synthetic bool, base string) (tokenledger.Observation, bool, error) {
	now := a.now()
	if allowed, class, until := a.ledger.ReadAllowed(site, now); !allowed {
		return tokenledger.Observation{Contract: tokenledger.BalanceContract, Site: site, ErrorClass: class, RecheckAtNS: until}, false, nil
	}
	query := trackersearch.Query{Site: site, Cookie: cookie, Synthetic: synthetic, BaseURL: base, SolverURL: a.solverURL()}
	var body []byte
	var status int
	var retryHeader string
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		body, status, retryHeader, err = a.searcher.ReadAccount(ctx, query, url.Values{"action": {"index"}})
		retryable := err != nil || status == 0 || status >= 500
		if !retryable || attempt == 1 {
			break
		}
		if a.Wait != nil {
			if waitErr := a.Wait(ctx, time.Second); waitErr != nil {
				break
			}
		}
	}
	obs := tokenledger.ParseBalance(site, body)
	obs.CookieID = tokenledger.Fingerprint(cookie)
	obs.ObservedAtNS = now.UnixNano()
	switch {
	case err != nil || status == 0 || status >= 500:
		obs.Known = false
		obs.Balance = 0
		obs.AccountID = ""
		obs.ErrorClass = "network"
		obs.RecheckAtNS = now.Add(30 * time.Second).UnixNano()
	case status == 401 || status == 403:
		obs.Known = false
		obs.Balance = 0
		obs.ErrorClass = "auth"
		obs.RecheckAtNS = now.Add(5 * time.Minute).UnixNano()
	case status == 429:
		obs.Known = false
		obs.Balance = 0
		obs.ErrorClass = "rate_limited"
		obs.RecheckAtNS = now.Add(recheckDelay(retryHeader)).UnixNano()
	case status != 200:
		obs.Known = false
		obs.Balance = 0
		obs.ErrorClass = "parse"
		obs.RecheckAtNS = now.Add(30 * time.Second).UnixNano()
	}
	if obs.ErrorClass == "auth" && obs.RecheckAtNS == 0 {
		obs.RecheckAtNS = now.Add(5 * time.Minute).UnixNano()
	}
	// 0.4.31 reads the user profile when the index document has an account
	// id but no token field. A profile failure leaves the balance unknown
	// and does not place a site-wide hold: the next attempt can read again,
	// and no token request has been sent. Auth and rate limits are real
	// site answers and are kept.
	if obs.ErrorClass == tokenledger.ClassBalanceUnknown && obs.AccountID != "" && err == nil && status == http.StatusOK {
		userBody, userStatus, userRetry, userErr := a.searcher.ReadAccount(ctx, query, url.Values{"action": {"user"}, "id": {obs.AccountID}})
		switch {
		case userErr != nil || userStatus == 0 || userStatus >= 500:
		case userStatus == http.StatusUnauthorized || userStatus == http.StatusForbidden:
			obs.Known = false
			obs.Balance = 0
			obs.ErrorClass = "auth"
			obs.RecheckAtNS = now.Add(5 * time.Minute).UnixNano()
		case userStatus == http.StatusTooManyRequests:
			obs.Known = false
			obs.Balance = 0
			obs.ErrorClass = "rate_limited"
			obs.RecheckAtNS = now.Add(recheckDelay(userRetry)).UnixNano()
		case userStatus == http.StatusOK:
			parsed := tokenledger.ParseBalance(site, userBody)
			if parsed.Known && parsed.ErrorClass == "" && (parsed.AccountID == "" || parsed.AccountID == obs.AccountID) {
				obs.Known = true
				obs.Balance = parsed.Balance
				obs.Source = "user." + parsed.Source
				obs.ErrorClass = ""
			}
		}
	}
	// Last resort: the rendered user page. Some sites (Redacted) print the
	// token count only there; the same parser reads "Tokens: N" from it. Only
	// this account's own page is requested, and a failure leaves the balance
	// unknown exactly as before.
	if obs.ErrorClass == tokenledger.ClassBalanceUnknown && obs.AccountID != "" && err == nil && status == http.StatusOK {
		if page, readErr := a.searcher.ReadDetail(ctx, query, "/user.php", url.Values{"id": {obs.AccountID}}); readErr == nil {
			if parsed := tokenledger.ParseBalance(site, page); parsed.Known && parsed.ErrorClass == "" {
				obs.Known = true
				obs.Balance = parsed.Balance
				obs.Source = "page." + parsed.Source
				obs.ErrorClass = ""
			}
		}
	}
	account := obs.AccountID
	if account == "" {
		account = "unbound"
	}
	prior, found, loadErr := a.ledger.Load(site, account)
	if loadErr != nil {
		return tokenledger.Observation{}, true, loadErr
	}
	obs.ReadBaseSet = true
	if found {
		obs.ReadBase = prior.Generation
	}
	committed, err := a.ledger.CommitObservation(obs)
	if err != nil {
		return tokenledger.Observation{}, true, err
	}
	if committed.Generation != prior.Generation {
		if committed.Known && committed.ErrorClass == "" {
			_ = a.ledger.ClearReadHold(site)
		} else if committed.RecheckAtNS > now.UnixNano() {
			switch committed.ErrorClass {
			case "rate_limited", "auth", "network", "parse":
				_ = a.ledger.HoldRead(site, committed.ErrorClass, time.Unix(0, committed.RecheckAtNS))
			}
		}
	}
	return committed, true, nil
}

func recheckDelay(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 60 * time.Second
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value < 1 {
		return 60 * time.Second
	}
	if value > 1800 {
		value = 1800
	}
	return time.Duration(value * float64(time.Second))
}

func (a *App) fetchTorrent(ctx context.Context, execution *scheduler.Execution, plan SearchPlan, site, resource string, useToken bool) (torrentfetch.Receipt, error) {
	phase := "fetch_" + strings.ReplaceAll(site, ".", "_")
	if err := execution.Checkpoint(ctx, phase, 0, site); err != nil {
		return torrentfetch.Receipt{}, err
	}
	request := torrentfetch.Request{
		Site: site, Resource: resource, Cookie: a.cookieFor(plan.RootJobID, site), Synthetic: plan.Synthetic, BaseURL: plan.BaseURL,
		UseToken: useToken, Directory: filepath.Join(a.Dir, "torrents"),
	}
	switch site {
	case "dicmusic":
		request.AuthKey = a.secret(plan.RootJobID + "/dicmusic")
	case "orpheus":
		request.TorrentPass = a.secret(plan.RootJobID + "/orpheus")
	}
	return torrentfetch.Fetch(ctx, request)
}

func (a *App) finishAcquire(held taskrepo.Token, job workflow.Job, review, message, class string, receipt *torrentfetch.Receipt) (scheduler.Outcome, error) {
	complete := workflow.Complete{
		Edge: workflow.EdgeTerminal, NextStage: "needs_review", ReviewKind: review, TaskState: "blocked",
		ErrorClass: class, Message: message, NowNS: a.now().UnixNano(),
	}
	if receipt != nil {
		body, err := json.Marshal(receipt)
		if err != nil {
			return scheduler.Outcome{}, err
		}
		complete.Evidence = []workflow.EvidenceInput{{
			ID: evidenceID(held.ID, held.Fence, receipt.Site), Kind: "api", Source: receipt.Site, Payload: body,
		}}
	}
	if _, err := a.flows.Complete(intentID("acquire", held.ID, held.Fence), held, complete); err != nil {
		return scheduler.Outcome{}, err
	}
	return scheduler.Outcome{State: "blocked", Message: message}, nil
}
