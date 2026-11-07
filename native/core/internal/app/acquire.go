package app

import (
	"context"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/scheduler"
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
	plan, err := decodePlan(payload)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	held := execution.Token()
	task, err := a.tasks.Get(held.ID)
	if err != nil || task == nil {
		return scheduler.Outcome{}, err
	}
	job, err := a.flows.Job(task.WorkflowJobID)
	if err != nil || job == nil {
		return scheduler.Outcome{}, err
	}
	defer a.forget(plan.RootJobID)
	if job.Stage == "fallback" || job.Stage == "downloading" {
		return a.finishAcquire(held, *job, "candidate", "流媒体或音频获取尚未接入；种子保存不等于已下载、尚未入库", "", nil)
	}
	if job.Stage != "requesting" {
		return a.finishAcquire(held, *job, "candidate", "当前阶段不能获取；尚未下载、尚未入库", "", nil)
	}
	site, resource, ok := strings.Cut(plan.SelectedCandidateID, ":")
	if !ok || site == "" || site != plan.SelectedSite || resource == "" {
		return a.finishAcquire(held, *job, "candidate", "没有唯一候选，不能获取；尚未下载、尚未入库", "", nil)
	}
	if a.secret(plan.RootJobID) == "" {
		return a.finishAcquire(held, *job, "candidate", "没有可用的登录状态，不能获取；尚未下载、尚未入库", "", nil)
	}
	if plan.TokenCost > 0 {
		return a.acquireWithToken(ctx, execution, held, *job, plan, site, resource)
	}
	if !plan.FreeConfirmed {
		return a.finishAcquire(held, *job, "candidate", "没有可确认的免费或令牌授权；尚未下载、尚未入库", "", nil)
	}
	receipt, err := a.fetchTorrent(ctx, execution, plan, site, resource, false)
	if err != nil {
		return a.finishAcquire(held, *job, "candidate", "种子响应不是有效元信息；尚未下载、尚未入库", "PARSE_ERROR", nil)
	}
	return a.finishAcquire(held, *job, "candidate", "种子字节已保存，专辑尚未下载、尚未入库", "", &receipt)
}

func (a *App) acquireWithToken(ctx context.Context, execution *scheduler.Execution, held taskrepo.Token, job workflow.Job, plan SearchPlan, site, resource string) (scheduler.Outcome, error) {
	if site != "dicmusic" && site != "orpheus" {
		return a.finishAcquire(held, job, "token", "该站不使用这条令牌账本；尚未下载、尚未入库", "", nil)
	}
	observation, _, err := a.observeBalance(ctx, site, a.secret(plan.RootJobID), plan.Synthetic, plan.BaseURL)
	if err != nil {
		return a.finishAcquire(held, job, "token", "余额读取失败，未发令牌请求；尚未下载、尚未入库", observation.ErrorClass, nil)
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
	quote := tokenledger.Quote{
		Site: site, AccountID: observation.AccountID, CookieID: tokenledger.Fingerprint(a.secret(plan.RootJobID)),
		ResourceID: resource, RequestID: job.ID, Cost: plan.TokenCost,
		SafetyMargin: plan.SafetyMargin, BudgetRef: job.BatchID, Scene: job.Scene,
		IdentityConfirmed: plan.Identity.IdentityConfirmed, StreamingEnabled: plan.StreamingEnabled,
	}
	reserved, err := a.ledger.ReserveObserved(a.now(), quote, observation.AccountID)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	if reserved.State != tokenledger.StateReserved {
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
	return a.finishAcquire(held, job, "token", "种子字节已保存，免费是否生效尚未对账；尚未下载、尚未入库", "REQUEST_UNKNOWN", &receipt)
}

func (a *App) observeBalance(ctx context.Context, site, cookie string, synthetic bool, base string) (tokenledger.Observation, bool, error) {
	now := a.now()
	if allowed, class, until := a.ledger.ReadAllowed(site, now); !allowed {
		return tokenledger.Observation{Contract: tokenledger.BalanceContract, Site: site, ErrorClass: class, RecheckAtNS: until}, false, nil
	}
	query := trackersearch.Query{Site: site, Cookie: cookie, Synthetic: synthetic, BaseURL: base}
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
		Site: site, Resource: resource, Cookie: a.secret(plan.RootJobID), Synthetic: plan.Synthetic, BaseURL: plan.BaseURL,
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
			ID: evidenceID(held.ID, receipt.Site), Kind: "api", Source: receipt.Site, Payload: body,
		}}
	}
	if _, err := a.flows.Complete(intentID("acquire", held.ID, held.Fence), held, complete); err != nil {
		return scheduler.Outcome{}, err
	}
	return scheduler.Outcome{State: "blocked", Message: message}, nil
}
