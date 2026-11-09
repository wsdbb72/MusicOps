// Download is the qBittorrent download lane: POST /v1/jobs/{id}/download
// starts it, and the download_release executor runs it.
//
// Credentials live only in the app's in-memory secrets map, under
// "<job id>/qbittorrent" as JSON, and are dropped when the execution
// attempt ends (success, failure or cancellation). They are never written
// to disk, never logged and never returned. The durable download plan
// carries no credentials at all, so a retry has to re-supply them through
// POST /v1/jobs/{id}/download.
//
// The chain, and where each step lives:
//
//  1. StartDownload (workflow) completes the blocked acquire task and queues
//     the download task on the same job, moving it to downloading.
//  2. The executor reads the stored torrent receipt from evidence, parses
//     the metainfo, logs in to qBittorrent, and either adds the torrent
//     stopped or resumes the one the client already holds (Query first, so
//     a retry never adds twice). Ownership is re-verified against the live
//     row before any mutation.
//  3. After Start, the executor polls until the client reports the torrent
//     complete (finished state, progress 1, every expected file at
//     progress 1). A poll timeout fails the task with DOWNLOAD_TIMEOUT; the
//     torrent is left in the client as it is — neither this client nor the
//     Python adapter exposes a stop operation — and a retry resumes it.
//  4. Archive moves the finished torrent into <state>/seeds/<task id>
//     through the client (setLocation + setDownloadPath), then verifies the
//     move byte by byte and requires the old directory to be empty. The
//     torrent keeps seeding from the new location: publish verifies the
//     library copies, never the seed files, so seeding is unaffected by
//     publishing.
//  5. Ingest copies each .wav/.flac from the seed directory into incoming/
//     and releases it with audiorelease.Release (Origin "local_import":
//     the file reached incoming/ by the executor copying it, not by a
//     person placing it by hand), collecting publish receipts.
//  6. The executor completes the download task with EdgeAdvance
//     downloading -> validating (the validating stage task carries the
//     receipts in its payload) and then calls RecordRelease on the root
//     job, which moves the root to publishing with the single publish
//     task for these receipts. If RecordRelease fails after the Complete,
//     the executor does NOT fail the job: the validating task still
//     carries the receipts and re-attempts the same RecordRelease
//     idempotently before completing the process job terminally (see
//     finishValidating). The process job never gets its own publish task:
//     evidence ids are global and the stored record embeds the owning
//     job, so a second publish task verifying the same receipts would
//     collide on the release record and on every per-track record and
//     fail with ErrStale.
//
// Free evidence: the acquire executor only fetches a torrent without a
// token after sitefree confirmed it free for this user (personal free, or
// global free on a non-token site), and a token fetch buys freeleech for
// that torrent (the Python provider records free=not nonfree for both
// cases). The receipt's UseToken flag is the durable trace of which branch
// ran, so the handler transcribes it into the plan as FreeConfirmed with
// FreeMethod "site-free" or "token". A receipt that cannot be traced to
// the acquire decision stays unconfirmed and StartDownload refuses it;
// the executor re-checks the same rule through AddStopped, which refuses
// unconfirmed free evidence.
package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/qbittorrent"
	"github.com/wsdbb72/MusicOps/native/core/internal/scheduler"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/torrentfetch"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

// downloadPollTimeout bounds one download execution's poll loop. A real
// album download can take a long time; the bound only stops a runaway
// poll, and a timed-out download is resumable because the torrent stays
// in the client. Tests override these variables.
var downloadPollTimeout = 2 * time.Hour
var downloadPollInterval = 2 * time.Second

// downloadArchiveSettleTimeout bounds waiting for the client to report a
// setLocation / setDownloadPath move.
var downloadArchiveSettleTimeout = 30 * time.Second

// downloadPlan is the durable, secret-free description of one download.
// It is the download task's payload, so it is stored in Pebble: it must
// never contain credentials.
type downloadPlan struct {
	RootJobID      string `json:"root_job_id"`
	JobID          string `json:"job_id"`
	Site           string `json:"site"`
	ResourceID     string `json:"resource_id"`
	InfoHash       string `json:"infohash"`
	ExpectedTitle  string `json:"expected_title"`
	ExpectedArtist string `json:"expected_artist,omitempty"`
	FreeConfirmed  bool   `json:"free_confirmed"`
	FreeMethod     string `json:"free_method"`
	SavePath       string `json:"save_path,omitempty"`
	Category       string `json:"category,omitempty"`
	Tags           string `json:"tags,omitempty"`
}

// qbittorrentCredentials is the in-memory-only qBittorrent login for one
// download attempt. It is stored JSON-encoded under "<job id>/qbittorrent"
// in the app's secrets map and dropped when the attempt ends. Generation
// counts the remember calls for the job: an executor only drops the
// credentials of its own attempt, because a retry can re-supply fresh
// credentials while the previous attempt is still unwinding, and the stale
// forget must not delete those.
type qbittorrentCredentials struct {
	BaseURL    string `json:"base_url"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	Generation uint64 `json:"generation,omitempty"`
}

func (a *App) rememberQBittorrent(jobID string, creds qbittorrentCredentials) (uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if raw, ok := a.secrets[jobID+"/qbittorrent"]; ok {
		var previous qbittorrentCredentials
		if err := json.Unmarshal([]byte(raw), &previous); err == nil {
			creds.Generation = previous.Generation + 1
		}
	}
	encoded, err := json.Marshal(creds)
	if err != nil {
		return 0, err
	}
	a.secrets[jobID+"/qbittorrent"] = string(encoded)
	return creds.Generation, nil
}

func (a *App) qbittorrentSecret(jobID string) (qbittorrentCredentials, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	raw, ok := a.secrets[jobID+"/qbittorrent"]
	if !ok {
		return qbittorrentCredentials{}, false
	}
	var creds qbittorrentCredentials
	if err := json.Unmarshal([]byte(raw), &creds); err != nil || creds.BaseURL == "" || creds.Username == "" || creds.Password == "" {
		return qbittorrentCredentials{}, false
	}
	return creds, true
}

// forgetQBittorrent drops the stored login only when it is still the
// generation the caller read: a newer remember (a retry that re-supplied
// credentials) leaves the fresh login alone.
func (a *App) forgetQBittorrent(jobID string, generation uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	raw, ok := a.secrets[jobID+"/qbittorrent"]
	if !ok {
		return
	}
	var stored qbittorrentCredentials
	if err := json.Unmarshal([]byte(raw), &stored); err != nil || stored.Generation != generation {
		return
	}
	delete(a.secrets, jobID+"/qbittorrent")
}

// downloadRecordID derives the qBittorrent-side task id for one process
// job. Record ids allow only [A-Za-z0-9_-]{1,120} while workflow job ids
// contain colons, so the id is the job id with other characters folded to
// "-" plus a short hash of the original: two jobs that fold alike still
// get different ids, and the same job always gets the same id, so a retry
// resumes the same client torrent instead of adding a second one.
func downloadRecordID(jobID string) string {
	sum := sha256.Sum256([]byte("download-record/" + jobID))
	suffix := hex.EncodeToString(sum[:4])
	var b strings.Builder
	for _, r := range jobID {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	base := b.String()
	if len(base) > 100 {
		base = base[:100]
	}
	return base + "-" + suffix
}

// downloadFailure is a download step that failed for a known, reportable
// reason. The class is what the job records; the message is what the
// operator sees.
type downloadFailure struct {
	Class   string
	Message string
}

func (e *downloadFailure) Error() string { return e.Class + ": " + e.Message }

func downloadError(class, message string) *downloadFailure {
	return &downloadFailure{Class: class, Message: message}
}

// torrentReceipt reads the stored torrent receipt for one job. When site or
// resource is set it must match; otherwise the first stored receipt wins.
// The receipt is the acquire executor's evidence, so a job that never saved
// a torrent (for example a streaming-fallback job) has none.
func (a *App) torrentReceipt(jobID, site, resource string) (torrentfetch.Receipt, error) {
	inspection, err := a.flows.Inspect(jobID)
	if err != nil {
		return torrentfetch.Receipt{}, err
	}
	for _, item := range inspection.Evidence {
		payload, found, err := a.flows.EvidenceData(item.ID)
		if err != nil || !found {
			continue
		}
		var receipt torrentfetch.Receipt
		if err := json.Unmarshal(payload, &receipt); err != nil || receipt.Site == "" || receipt.MetaSHA256 == "" || receipt.Path == "" {
			continue
		}
		if site != "" && receipt.Site != site {
			continue
		}
		if resource != "" && receipt.ResourceID != resource {
			continue
		}
		return receipt, nil
	}
	return torrentfetch.Receipt{}, errors.New("no saved torrent receipt for this job")
}

// readTorrentBytes reads the .torrent file a receipt points at. The path
// comes from our own evidence, but it is still refused when it escapes the
// state directory, and the bytes are capped at the fetch limit.
func (a *App) readTorrentBytes(receipt torrentfetch.Receipt) ([]byte, error) {
	path, err := resolveInDir(a.Dir, receipt.Path)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, 8<<20+1))
}

// downloadJob starts a qBittorrent download for one blocked acquire job.
//
// The request body carries the qBittorrent login (base_url, username,
// password) plus optional save_path, category and tags. The login is
// stored in memory only, under "<job id>/qbittorrent", and is dropped when
// the download attempt ends; it is never written to disk, logged or
// returned. The durable plan built here carries no credentials.
//
//   - already downloading: the call is idempotent and returns the current
//     job instead of queuing a second download task.
//   - failed download task: a call with fresh credentials re-supplies them
//     and retries the download task through the review retry path, so a
//     credential failure is recoverable without a second endpoint.
//   - anything else: the job must be a needs_review/candidate acquire job
//     with a saved torrent receipt, or the call is refused with 409.
//
// downloadRequest is one POST /v1/jobs/{id}/download body. The login is
// memory-only; the rest is stored in the durable download plan.
type downloadRequest struct {
	BaseURL  string `json:"base_url"`
	Username string `json:"username"`
	Password string `json:"password"`
	SavePath string `json:"save_path"`
	Category string `json:"category"`
	Tags     string `json:"tags"`
}

func (a *App) downloadJob(w http.ResponseWriter, r *http.Request) {
	var body downloadRequest
	if err := decode(r, &body); err != nil || strings.ContainsAny(body.BaseURL+body.Username+body.Password+body.SavePath+body.Category+body.Tags, "\r\n") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	id := r.PathValue("id")
	job, err := a.flows.Job(id)
	if err != nil || job == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "code": "NOT_FOUND"})
		return
	}
	if job.Stage == "downloading" && job.ActiveTaskID == workflow.DownloadTaskID(id) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": publicJob(*job), "stage": job.Stage, "download_task_id": job.ActiveTaskID, "replayed": true})
		return
	}
	if job.Stage == "failed" {
		a.downloadRetry(w, id, *job, body)
		return
	}
	if job.Stage != "needs_review" || job.ReviewKind != "candidate" || job.TaskType != "album" {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "WORKFLOW_REJECTED", "error": "job stage " + job.Stage + " cannot start a download"})
		return
	}
	origin, creds, category, tags, ok := a.validDownloadSettings(body)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	plan, err := a.buildDownloadPlan(*job, body.SavePath, category, tags)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "WORKFLOW_REJECTED", "error": "no saved torrent receipt for this job"})
		return
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "DOWNLOAD_UNAVAILABLE"})
		return
	}
	gen, err := a.rememberQBittorrent(id, qbittorrentCredentials{BaseURL: origin, Username: creds.Username, Password: creds.Password})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "DOWNLOAD_UNAVAILABLE"})
		return
	}
	nowNS := time.Now().UnixNano()
	result, err := a.flows.StartDownload("download-"+id+"-"+strconv.FormatInt(nowNS, 10), id, workflow.DownloadInput{
		Plan: encoded, Site: plan.Site, ResourceID: plan.ResourceID, InfoHash: plan.InfoHash, FreeConfirmed: plan.FreeConfirmed,
	}, nowNS)
	if err != nil {
		a.forgetQBittorrent(id, gen)
		status, code := http.StatusInternalServerError, "JOBS_UNAVAILABLE"
		switch {
		case errors.Is(err, workflow.ErrNotFound):
			status, code = http.StatusNotFound, "NOT_FOUND"
		case errors.Is(err, dataengine.ErrInvalidRequest):
			status, code = http.StatusBadRequest, "INVALID_REQUEST"
		case errors.Is(err, workflow.ErrStale):
			status, code = http.StatusConflict, "WORKFLOW_REJECTED"
		}
		writeJSON(w, status, map[string]any{"ok": false, "code": code, "error": err.Error()})
		return
	}
	stage := "downloading"
	if result.Job != nil {
		stage = result.Job.Stage
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "job": publicJob(*result.Job), "stage": stage, "download_task_id": workflow.DownloadTaskID(id)})
}

// downloadRetry re-supplies qBittorrent credentials for a failed download
// task and retries it. Only a failed job whose active task is the failed
// download task may take this path; anything else is refused, and a bare
// review retry without fresh credentials still fails honestly with
// MISSING_CREDENTIALS in the executor.
func (a *App) downloadRetry(w http.ResponseWriter, id string, job workflow.Job, body downloadRequest) {
	task, err := a.tasks.Get(job.ActiveTaskID)
	if err != nil || task == nil || task.Command != workflow.CommandDownload || task.State != "failed" {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "code": "WORKFLOW_REJECTED", "error": "job stage failed cannot start a download"})
		return
	}
	origin, creds, _, _, ok := a.validDownloadSettings(body)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "code": "INVALID_REQUEST"})
		return
	}
	gen, err := a.rememberQBittorrent(id, qbittorrentCredentials{BaseURL: origin, Username: creds.Username, Password: creds.Password})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "code": "DOWNLOAD_UNAVAILABLE"})
		return
	}
	nowNS := time.Now().UnixNano()
	result, err := a.flows.Review("download-retry-"+id+"-"+strconv.FormatInt(nowNS, 10), id, workflow.ReviewDecision{Action: "retry"}, nowNS)
	if err != nil {
		a.forgetQBittorrent(id, gen)
		status, code := http.StatusInternalServerError, "JOBS_UNAVAILABLE"
		switch {
		case errors.Is(err, workflow.ErrNotFound):
			status, code = http.StatusNotFound, "NOT_FOUND"
		case errors.Is(err, dataengine.ErrInvalidRequest):
			status, code = http.StatusBadRequest, "INVALID_REQUEST"
		case errors.Is(err, workflow.ErrStale):
			status, code = http.StatusConflict, "WORKFLOW_REJECTED"
		}
		writeJSON(w, status, map[string]any{"ok": false, "code": code, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": publicJob(*result.Job), "stage": result.Job.Stage, "download_task_id": workflow.DownloadTaskID(id)})
}

// validDownloadSettings validates the qBittorrent settings from a download
// request: a clean origin (plain HTTP only for loopback/private hosts), a
// non-empty login, an absolute save path when one is given, and defaults
// for category and tags. It returns the cleaned origin, not the raw input.
func (a *App) validDownloadSettings(body downloadRequest) (string, qbittorrentCredentials, string, string, bool) {
	origin, err := qbittorrent.ValidateBaseURL(body.BaseURL)
	if err != nil || body.Username == "" || body.Password == "" {
		return "", qbittorrentCredentials{}, "", "", false
	}
	if body.SavePath != "" && !filepath.IsAbs(body.SavePath) {
		return "", qbittorrentCredentials{}, "", "", false
	}
	category := body.Category
	if category == "" {
		category = qbittorrent.DefaultCategory
	}
	tags := body.Tags
	if tags == "" {
		tags = qbittorrent.ProjectTag
	}
	for _, tag := range strings.Split(tags, ",") {
		if strings.TrimSpace(tag) == "" {
			return "", qbittorrentCredentials{}, "", "", false
		}
	}
	return origin, qbittorrentCredentials{BaseURL: origin, Username: body.Username, Password: body.Password}, category, tags, true
}

// buildDownloadPlan builds the durable, secret-free download plan for one
// acquire job from the stored search plan and the stored torrent receipt.
// The free determination is transcribed from the receipt: the acquire
// executor only fetches without a token after sitefree confirmed the
// torrent free for this user, and a token fetch buys freeleech for that
// torrent, so either branch is confirmed free (see the package comment).
// A job with no receipt, or a receipt whose bytes are gone, has no plan.
func (a *App) buildDownloadPlan(job workflow.Job, savePath, category, tags string) (downloadPlan, error) {
	var plan SearchPlan
	inspection, err := a.flows.Inspect(job.ID)
	if err != nil {
		return downloadPlan{}, err
	}
	if inspection.Task != nil {
		if raw, found, err := a.tasks.Data(inspection.Task.ID, "arguments", 0); err == nil && found {
			_ = json.Unmarshal(raw, &plan)
		}
	}
	site, resource := plan.SelectedSite, ""
	if _, res, ok := strings.Cut(plan.SelectedCandidateID, ":"); ok {
		resource = res
	}
	receipt, err := a.torrentReceipt(job.ID, site, resource)
	if err != nil {
		return downloadPlan{}, err
	}
	if _, err := a.readTorrentBytes(receipt); err != nil {
		return downloadPlan{}, err
	}
	freeMethod := "site-free"
	if receipt.UseToken {
		freeMethod = "token"
	}
	return downloadPlan{
		RootJobID: job.RootID, JobID: job.ID, Site: receipt.Site, ResourceID: receipt.ResourceID,
		InfoHash: receipt.InfoHash, ExpectedTitle: plan.ExpectedTitle, ExpectedArtist: plan.ExpectedArtist,
		FreeConfirmed: true, FreeMethod: freeMethod,
		SavePath: savePath, Category: category, Tags: tags,
	}, nil
}

// failDownload fails the download task and moves the job to "failed" in one
// atomic workflow step, like failPublish. The job stays visible as failed
// and can be retried: from review (which needs fresh credentials through
// POST /v1/jobs/{id}/download) or by re-posting the download with new
// credentials.
func (a *App) failDownload(execution *scheduler.Execution, class, message string) (scheduler.Outcome, error) {
	token := execution.Token()
	if _, err := a.flows.Complete(intentID("download", token.ID, token.Fence), token, workflow.Complete{
		Edge: workflow.EdgeTerminal, NextStage: "failed", TaskState: "failed",
		ErrorClass: class, Message: message, NowNS: a.now().UnixNano(),
	}); err != nil {
		return scheduler.Outcome{}, err
	}
	return scheduler.Outcome{State: "failed", ErrorClass: class, Message: message}, nil
}

// failDownloadStep is failDownload for the three-value step helpers
// (ensureTorrent, startIfStopped, pollDownload): the outcome, done=true
// and the error, so the caller returns the outcome instead of running on.
func (a *App) failDownloadStep(execution *scheduler.Execution, class, message string) (scheduler.Outcome, bool, error) {
	outcome, err := a.failDownload(execution, class, message)
	return outcome, true, err
}

// download runs one download_release task: add-or-resume in qBittorrent,
// start, poll to completion, archive into the seeding directory, ingest
// the audio, and hand the job to publishing. It fails the task (through
// failDownload) instead of inventing a result whenever the client, the
// receipt or the archive disagrees with the plan.
func (a *App) download(ctx context.Context, execution *scheduler.Execution, payload []byte) (scheduler.Outcome, error) {
	if err := a.cancelled(ctx, execution); err != nil {
		return scheduler.Outcome{}, err
	}
	var plan downloadPlan
	if err := json.Unmarshal(payload, &plan); err != nil || plan.RootJobID == "" || plan.JobID == "" || plan.Site == "" || plan.ResourceID == "" || plan.InfoHash == "" || !plan.FreeConfirmed {
		return a.failDownload(execution, "INVALID_PLAN", "下载计划无效，无法开始下载")
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
	if job.Stage != "downloading" {
		// The job already moved past downloading (a retry after the first
		// attempt already advanced it, or an operator moved it). No
		// workflow edge completes this task without moving the job, so
		// return an error and let the coordinator interrupt the job for
		// review instead of inventing a transition.
		return scheduler.Outcome{}, errors.New("download task ran while the job is in stage " + job.Stage)
	}
	receipt, err := a.torrentReceipt(plan.JobID, plan.Site, plan.ResourceID)
	if err != nil {
		return a.failDownload(execution, "RECEIPT_MISSING", "种子收据已不在证据中，无法下载")
	}
	torrent, err := a.readTorrentBytes(receipt)
	if err != nil {
		return a.failDownload(execution, "RECEIPT_MISSING", "种子文件不可读")
	}
	meta, err := qbittorrent.ParseMetainfo(torrent)
	if err != nil {
		return a.failDownload(execution, "RECEIPT_MISMATCH", "种子文件不是有效元信息")
	}
	if meta.MetainfoSHA256 != receipt.MetaSHA256 || (!strings.EqualFold(meta.InfoHashV1, plan.InfoHash) && !strings.EqualFold(meta.InfoHashV2, plan.InfoHash)) {
		return a.failDownload(execution, "RECEIPT_MISMATCH", "种子内容与收据不一致")
	}
	creds, ok := a.qbittorrentSecret(plan.JobID)
	if !ok {
		return a.failDownload(execution, "MISSING_CREDENTIALS", "没有可用的 qBittorrent 凭据，请重新提交下载请求")
	}
	// The credential lives in memory only for this one execution attempt
	// and is dropped on every exit below (success, failure or
	// cancellation), but only when no newer remember replaced it: a retry
	// that re-supplied credentials while this attempt was unwinding must
	// keep its own login. A retry re-supplies it through
	// POST /v1/jobs/{id}/download.
	defer a.forgetQBittorrent(plan.JobID, creds.Generation)
	client, err := qbittorrent.NewClient(qbittorrent.Config{BaseURL: creds.BaseURL, Username: creds.Username, Password: creds.Password})
	if err != nil {
		return a.failDownload(execution, "INVALID_QBITTORRENT", "qBittorrent 服务地址无效")
	}
	if err := client.Login(ctx); err != nil {
		class := "QBITTORENT_UNAVAILABLE"
		if errors.Is(err, qbittorrent.ErrAuthFailed) {
			class = "AUTH_FAILED"
		}
		return a.failDownload(execution, class, "qBittorrent 登录失败")
	}
	taskID := downloadRecordID(plan.JobID)
	sourceRoot := plan.SavePath
	if sourceRoot == "" {
		sourceRoot = filepath.Join(a.Dir, "downloads")
	}
	rec, err := qbittorrent.NewRecord(taskID, sourceRoot, plan.Category)
	if err != nil {
		return a.failDownload(execution, "INVALID_PLAN", "下载任务标识无效")
	}
	if plan.Tags != "" {
		rec.TaskTag = plan.Tags
	}
	rec.InfoHashV1 = meta.InfoHashV1
	rec.InfoHashV2 = meta.InfoHashV2
	if meta.InfoHashV1 != "" {
		rec.ClientID = meta.InfoHashV1
	} else {
		rec.ClientID = meta.InfoHashV2[:40]
	}
	rec.MetainfoSHA256 = meta.MetainfoSHA256
	rec.Files = meta.Files
	// The creation record for this task is the deterministic task id, the
	// save path, the tag and the category: a torrent the client already
	// holds is only resumed after every ownership field is re-verified
	// against the live row below, so a rebuilt record may honestly claim
	// the creation intent it re-verifies. The record itself is not
	// persisted: everything in it is rebuilt from the torrent bytes and
	// the plan on every attempt, and the start-attempt budget resets with
	// it (a retry gets a fresh three attempts).
	rec.CreationIntent = true
	evidence := qbittorrent.FreeEvidence{Confirmed: plan.FreeConfirmed, InfoHash: plan.InfoHash, Free: qbittorrent.FreePtr(true)}
	if err := execution.Checkpoint(ctx, "download_add", 0, "正在确认 qBittorrent 任务"); err != nil {
		return scheduler.Outcome{}, err
	}
	if outcome, done, err := a.ensureTorrent(ctx, execution, client, &rec, torrent, evidence); err != nil {
		return scheduler.Outcome{}, err
	} else if done {
		return outcome, nil
	}
	if outcome, done, err := a.pollDownload(ctx, execution, client, &rec, evidence); err != nil {
		return scheduler.Outcome{}, err
	} else if done {
		return outcome, nil
	}
	seedDir := filepath.Join(a.Dir, "seeds", taskID)
	archived, err := a.archiveDownload(ctx, client, &rec, meta, seedDir)
	if err != nil {
		if df, ok := err.(*downloadFailure); ok {
			return a.failDownload(execution, df.Class, df.Message)
		}
		return a.failDownload(execution, "ARCHIVE_FAILED", "归档失败")
	}
	receipts, err := a.ingestDownload(plan, meta, seedDir)
	if err != nil {
		if df, ok := err.(*downloadFailure); ok {
			return a.failDownload(execution, df.Class, df.Message)
		}
		return a.failDownload(execution, "INGEST_FAILED", "入库失败")
	}
	return a.finishDownload(execution, held, plan, receipts, archived)
}

// ensureTorrent makes sure the torrent exists in the client, stopped or
// already running. It adds it stopped when the client does not hold it, and
// resumes the existing one (after re-verifying ownership) when it does, so
// a retry never adds the same torrent twice. It returns done=true with a
// failDownload outcome when the step cannot proceed.
func (a *App) ensureTorrent(ctx context.Context, execution *scheduler.Execution, client *qbittorrent.Client, rec *qbittorrent.Record, torrent []byte, evidence qbittorrent.FreeEvidence) (scheduler.Outcome, bool, error) {
	row, err := client.Query(ctx, *rec)
	if err != nil {
		return a.failDownloadStep(execution, "QBITTORENT_UNAVAILABLE", "qBittorrent 查询失败")
	}
	if row == nil {
		if _, err := client.AddStopped(ctx, rec, torrent, evidence); err != nil {
			if errors.Is(err, qbittorrent.ErrAlreadyExists) {
				// Another attempt added it between the Query and the Add:
				// fall through to the resume path below.
			} else if errors.Is(err, qbittorrent.ErrFreeEvidenceMismatch) {
				return a.failDownloadStep(execution, "FREE_EVIDENCE_MISMATCH", "免费证据与种子不一致，无法下载")
			} else {
				return a.failDownloadStep(execution, "ADD_FAILED", "qBittorrent 加种失败")
			}
		}
		// AddStopped already reconciled the add; re-read the live row so
		// the start decision below is made on fresh client state.
	}
	return a.startIfStopped(ctx, execution, client, rec, evidence)
}

// startIfStopped starts the torrent when the client holds it stopped, and
// leaves it alone when it is already downloading, waiting or finished. The
// ownership fields are re-verified against the live row before any start:
// a torrent the client already holds is only resumed after that check, and
// a row that never appeared after an add fails as ADD_UNKNOWN instead of
// being started blind.
func (a *App) startIfStopped(ctx context.Context, execution *scheduler.Execution, client *qbittorrent.Client, rec *qbittorrent.Record, evidence qbittorrent.FreeEvidence) (scheduler.Outcome, bool, error) {
	row, err := client.Query(ctx, *rec)
	if err != nil {
		return a.failDownloadStep(execution, "QBITTORENT_UNAVAILABLE", "qBittorrent 查询失败")
	}
	if row == nil {
		return a.failDownloadStep(execution, "ADD_UNKNOWN", "qBittorrent 加种未确认，请重试")
	}
	if reason := client.CheckOwnership(row, *rec, false); reason != "" {
		return a.failDownloadStep(execution, "OWNERSHIP_FAILED", "qBittorrent 已有该种子，但归属校验未通过："+reason)
	}
	if qbittorrent.ClassifyState(row.State, row.Progress) == qbittorrent.OutcomeStopped {
		// StartDownload refuses a record that is not in the stopped state,
		// and the record is rebuilt on every attempt, so mark it stopped
		// from the live row before starting.
		rec.State = qbittorrent.StateStopped
		if err := client.StartDownload(ctx, rec, evidence); err != nil {
			if errors.Is(err, qbittorrent.ErrFreeEvidenceMismatch) {
				return a.failDownloadStep(execution, "FREE_EVIDENCE_MISMATCH", "免费证据与种子不一致，无法下载")
			}
			var ownErr *qbittorrent.OwnershipError
			if errors.As(err, &ownErr) {
				return a.failDownloadStep(execution, "OWNERSHIP_FAILED", "qBittorrent 归属校验未通过："+ownErr.Reason)
			}
			return a.failDownloadStep(execution, "START_FAILED", "qBittorrent 开始下载失败")
		}
	}
	return scheduler.Outcome{}, false, nil
}

// pollDownload polls the client until the torrent is complete (finished
// state, progress 1, every expected file at progress 1), reporting progress
// at each checkpoint. A stopped torrent fails with DOWNLOAD_STOPPED: the
// client stopped it (or the operator did), and a retry resumes it. A
// blocked torrent fails with DOWNLOAD_BLOCKED. A poll timeout fails with
// DOWNLOAD_TIMEOUT and leaves the torrent in the client as it is, for a
// retry to resume.
func (a *App) pollDownload(ctx context.Context, execution *scheduler.Execution, client *qbittorrent.Client, rec *qbittorrent.Record, evidence qbittorrent.FreeEvidence) (scheduler.Outcome, bool, error) {
	deadline := time.Now().Add(downloadPollTimeout)
	for {
		if time.Now().After(deadline) {
			return a.failDownloadStep(execution, "DOWNLOAD_TIMEOUT", "下载轮询超时，种子仍在 qBittorrent 中，可重试续传")
		}
		rows, err := client.Info(ctx, rec.ClientID)
		if err != nil {
			return a.failDownloadStep(execution, "QBITTORENT_UNAVAILABLE", "qBittorrent 查询失败")
		}
		progress := 0.0
		if len(rows) == 1 {
			progress = rows[0].Progress * 100
		}
		if err := execution.Checkpoint(ctx, "download_poll", progress, "正在下载"); err != nil {
			return scheduler.Outcome{}, false, err
		}
		result, err := client.Poll(ctx, rec, nil)
		if err != nil {
			var ownErr *qbittorrent.OwnershipError
			if errors.As(err, &ownErr) {
				return a.failDownloadStep(execution, "OWNERSHIP_FAILED", "qBittorrent 归属校验未通过："+ownErr.Reason)
			}
			return a.failDownloadStep(execution, "QBITTORENT_UNAVAILABLE", "qBittorrent 查询失败")
		}
		switch result.Outcome {
		case qbittorrent.OutcomeComplete:
			return scheduler.Outcome{}, false, nil
		case qbittorrent.OutcomeStopped:
			return a.failDownloadStep(execution, "DOWNLOAD_STOPPED", "qBittorrent 已停止该任务，请重试继续")
		case qbittorrent.OutcomeBlocked:
			return a.failDownloadStep(execution, "DOWNLOAD_BLOCKED", "qBittorrent 报告错误或文件缺失")
		default:
			if err := a.wait(ctx, downloadPollInterval); err != nil {
				return scheduler.Outcome{}, false, err
			}
		}
	}
}

// finishDownload records the download: the download task completes with
// EdgeAdvance downloading -> validating (the validating stage task carries
// the publish payload), and the root job is recorded for publishing with
// the ingest receipts. If RecordRelease fails after the Complete, the
// executor does not fail the job: the download task is already completed
// and the validating task still carries the same receipts, so it
// completes the chain (see finishValidating), and the root can also be
// published again through POST /v1/audio/release.
func (a *App) finishDownload(execution *scheduler.Execution, held taskrepo.Token, plan downloadPlan, receipts []publishReceipt, archived []archiveFile) (scheduler.Outcome, error) {
	receiptsJSON, err := json.Marshal(receipts)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	payload := publishPayload{
		RootID:    plan.RootJobID,
		RequestID: publishRequestID(plan.RootJobID, "receipts", hex.EncodeToString(receiptsJSON)[:16]),
		Receipts:  receipts,
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	archiveJSON, err := json.Marshal(archiveReceipt{TaskID: downloadRecordID(plan.JobID), InfoHash: plan.InfoHash, SeedDir: filepath.Join(a.Dir, "seeds", downloadRecordID(plan.JobID)), Files: archived})
	if err != nil {
		return scheduler.Outcome{}, err
	}
	if _, err := a.flows.Complete(intentID("download", held.ID, held.Fence), held, workflow.Complete{
		Edge: workflow.EdgeAdvance, NextStage: "validating", TaskState: "completed",
		Message:  "下载完成，已归档做种，音频已入库，等待发布校验",
		Result:   payloadJSON,
		Evidence: []workflow.EvidenceInput{{ID: evidenceID(held.ID, held.Fence, "qbittorrent"), Kind: "catalog", Source: "qbittorrent", SubjectID: plan.RootJobID, Payload: archiveJSON}},
		NowNS:    a.now().UnixNano(),
	}); err != nil {
		return scheduler.Outcome{}, err
	}
	// A failure here must not interrupt the job: the download task already
	// completed, and the validating task carries the same receipts and
	// re-attempts this same RecordRelease idempotently (see
	// finishValidating), so the chain still completes.
	_, _ = a.flows.RecordRelease(workflow.RecordReleaseInput{
		JobID: plan.RootJobID, RequestID: "record-" + payload.RequestID,
		Evidence: workflow.EvidenceInput{ID: "pub-" + payload.RequestID, Kind: "catalog", Source: "local-library", SubjectID: plan.RootJobID, TracklistVerified: true, Payload: payloadJSON},
		Payload:  payloadJSON, NowNS: time.Now().UnixNano(),
	})
	return scheduler.Outcome{State: "completed", Message: "下载完成，已归档做种，音频已入库"}, nil
}

// archiveReceipt is the durable record of a finished download's move into
// the seeding directory. It is stored as evidence on the download
// completion, so the move can be audited without re-reading the client.
type archiveReceipt struct {
	TaskID   string        `json:"task_id"`
	InfoHash string        `json:"infohash"`
	SeedDir  string        `json:"seed_dir"`
	Files    []archiveFile `json:"files"`
}

// archiveFile is one moved file with the hash it had before the move.
type archiveFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// archiveDownload moves a finished torrent into the seeding directory
// (<state>/seeds/<task id>, the Go form of Python's seed_root) through the
// client, following Python's archive_completed: complete files only,
// setLocation to the seed directory, setDownloadPath in sync, then byte
// verification and an empty old directory. The torrent keeps seeding from
// the new location. A re-run over an already-archived torrent verifies the
// files where they are instead of moving them again.
func (a *App) archiveDownload(ctx context.Context, client *qbittorrent.Client, rec *qbittorrent.Record, meta qbittorrent.Metainfo, seedDir string) ([]archiveFile, error) {
	oldDir := rec.SourcePath
	expected := make([]qbittorrent.MetainfoFile, 0, len(meta.Files))
	for _, f := range meta.Files {
		if !f.Padding {
			expected = append(expected, f)
		}
	}
	if len(expected) == 0 {
		return nil, downloadError("ARCHIVE_FAILED", "种子内没有可归档的文件")
	}
	// The seed directory itself must not be created here: the client moves
	// the torrent into it, and a pre-created target directory makes the
	// move fail on filesystems that refuse renaming onto an existing
	// directory. Only its parent is created, so the rename has somewhere
	// to land.
	if err := os.MkdirAll(filepath.Dir(seedDir), 0o700); err != nil {
		return nil, err
	}
	archived := make([]archiveFile, 0, len(expected))
	if oldDir != seedDir {
		for _, f := range expected {
			oldPath, err := archiveJoin(oldDir, f.Path)
			if err != nil {
				return nil, downloadError("ARCHIVE_FAILED", "种子文件路径越界："+f.Path)
			}
			info, err := os.Lstat(oldPath)
			if err != nil || !info.Mode().IsRegular() {
				return nil, downloadError("ARCHIVE_FAILED", "原始做种文件缺失："+f.Path)
			}
			digest, err := hashFile(oldPath)
			if err != nil {
				return nil, err
			}
			target, err := archiveJoin(seedDir, f.Path)
			if err != nil {
				return nil, downloadError("ARCHIVE_FAILED", "种子文件路径越界："+f.Path)
			}
			if _, err := os.Lstat(target); err == nil {
				return nil, downloadError("ARCHIVE_FAILED", "做种目标已存在，不能覆盖："+f.Path)
			}
			archived = append(archived, archiveFile{Path: f.Path, SHA256: digest, Size: info.Size()})
		}
		if err := client.SetLocation(ctx, rec.ClientID, seedDir); err != nil {
			return nil, downloadError("ARCHIVE_FAILED", "客户端未接受做种目录迁移")
		}
		if err := a.waitLocation(ctx, client, rec, seedDir); err != nil {
			return nil, err
		}
		if err := client.SetDownloadPath(ctx, rec.ClientID, filepath.Join(seedDir, ".incomplete")); err != nil {
			return nil, downloadError("ARCHIVE_FAILED", "客户端临时路径尚未同步")
		}
		if err := a.waitDownloadPath(ctx, client, rec, seedDir); err != nil {
			return nil, err
		}
		for _, f := range archived {
			target, err := archiveJoin(seedDir, f.Path)
			if err != nil {
				return nil, downloadError("ARCHIVE_FAILED", "种子文件路径越界："+f.Path)
			}
			digest, err := hashFile(target)
			if err != nil || digest != f.SHA256 {
				return nil, downloadError("ARCHIVE_FAILED", "迁移后原始文件字节校验未通过："+f.Path)
			}
			oldPath, err := archiveJoin(oldDir, f.Path)
			if err != nil {
				return nil, downloadError("ARCHIVE_FAILED", "种子文件路径越界："+f.Path)
			}
			if _, err := os.Lstat(oldPath); err == nil {
				return nil, downloadError("ARCHIVE_FAILED", "客户端迁移后旧目录仍有文件："+f.Path)
			}
		}
		if leftovers, err := remainingFiles(oldDir); err != nil {
			return nil, err
		} else if leftovers > 0 {
			return nil, downloadError("ARCHIVE_FAILED", "客户端迁移后旧目录仍有文件")
		}
	} else {
		for _, f := range expected {
			target, err := archiveJoin(seedDir, f.Path)
			if err != nil {
				return nil, downloadError("ARCHIVE_FAILED", "种子文件路径越界："+f.Path)
			}
			info, err := os.Lstat(target)
			if err != nil || !info.Mode().IsRegular() {
				return nil, downloadError("ARCHIVE_FAILED", "做种文件缺失："+f.Path)
			}
			digest, err := hashFile(target)
			if err != nil {
				return nil, err
			}
			archived = append(archived, archiveFile{Path: f.Path, SHA256: digest, Size: info.Size()})
		}
	}
	rec.SourcePath = seedDir
	return archived, nil
}

// archiveJoin joins a torrent-relative path (validated by ParseMetainfo:
// no "..", no absolute paths) under a directory and refuses anything that
// escapes it.
func archiveJoin(dir, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", downloadError("ARCHIVE_FAILED", "种子文件路径无效")
	}
	joined := filepath.Join(dir, filepath.FromSlash(rel))
	root, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	if rel2, err := filepath.Rel(root, abs); err != nil || rel2 == ".." || strings.HasPrefix(rel2, ".."+string(filepath.Separator)) {
		return "", downloadError("ARCHIVE_FAILED", "种子文件路径越界")
	}
	return joined, nil
}

// remainingFiles counts the regular files left under a directory.
func remainingFiles(dir string) (int, error) {
	count := 0
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode().IsRegular() {
			count++
		}
		return nil
	})
	return count, err
}

// waitLocation waits until the client reports the torrent's save path as
// the seed directory (or the settle timeout runs out). A "moving" state
// keeps waiting; anything else that never settles fails honestly.
func (a *App) waitLocation(ctx context.Context, client *qbittorrent.Client, rec *qbittorrent.Record, seedDir string) error {
	deadline := time.Now().Add(downloadArchiveSettleTimeout)
	for {
		rows, err := client.Info(ctx, rec.ClientID)
		if err != nil {
			return downloadError("ARCHIVE_FAILED", "qBittorrent 查询失败")
		}
		if len(rows) == 1 && samePath(rows[0].SavePath, seedDir) {
			return nil
		}
		if time.Now().After(deadline) {
			return downloadError("ARCHIVE_FAILED", "客户端未确认迁移")
		}
		if err := a.wait(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
}

// waitDownloadPath waits until the client reports the torrent's incomplete
// directory under the seed directory.
func (a *App) waitDownloadPath(ctx context.Context, client *qbittorrent.Client, rec *qbittorrent.Record, seedDir string) error {
	deadline := time.Now().Add(downloadArchiveSettleTimeout)
	for {
		rows, err := client.Info(ctx, rec.ClientID)
		if err != nil {
			return downloadError("ARCHIVE_FAILED", "qBittorrent 查询失败")
		}
		if len(rows) == 1 && rows[0].DownloadPath != "" && insideDir(seedDir, rows[0].DownloadPath) {
			return nil
		}
		if time.Now().After(deadline) {
			return downloadError("ARCHIVE_FAILED", "客户端临时路径尚未同步")
		}
		if err := a.wait(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
}

// samePath compares two paths after cleaning them. The client reports the
// path it was given, so a lexical comparison is enough here; ownership
// checks elsewhere use the resolved comparison.
func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

// ingestDownload copies each .wav/.flac from the seed directory into
// incoming/ and releases it with audiorelease.Release, collecting one
// publish receipt per track. Non-audio files in the torrent (cue sheets,
// covers, logs) are skipped, not ingested. Track numbers follow the audio
// files in torrent order; titles come from the file stems; artist and
// album come from the confirmed search expectation, falling back to
// "Unknown Artist" and the torrent root name when the search carried none
// (the audio itself is verified either way; the fallback is a library
// convention, not a confirmed identity claim).
func (a *App) ingestDownload(plan downloadPlan, meta qbittorrent.Metainfo, seedDir string) ([]publishReceipt, error) {
	incoming, library := filepath.Join(a.Dir, "incoming"), filepath.Join(a.Dir, "library-audio")
	if err := os.MkdirAll(incoming, 0o700); err != nil || os.MkdirAll(library, 0o700) != nil {
		return nil, downloadError("INGEST_FAILED", "入库目录不可用")
	}
	artist := strings.TrimSpace(plan.ExpectedArtist)
	if artist == "" {
		artist = "Unknown Artist"
	}
	album := strings.TrimSpace(plan.ExpectedTitle)
	if album == "" {
		album = meta.Name
	}
	artist, album = truncateName(artist), truncateName(album)
	short := downloadRecordID(plan.JobID)
	if len(short) > 12 {
		short = short[:12]
	}
	receipts := []publishReceipt{}
	track := 0
	for _, f := range meta.Files {
		if f.Padding {
			continue
		}
		ext := strings.ToLower(filepath.Ext(f.Path))
		if ext != ".wav" && ext != ".flac" {
			continue
		}
		track++
		title := truncateName(strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path)))
		if title == "" {
			title = fmt.Sprintf("Track %d", track)
		}
		source, err := archiveJoin(seedDir, f.Path)
		if err != nil {
			return nil, downloadError("INGEST_FAILED", "种子文件路径越界："+f.Path)
		}
		incomingName := fmt.Sprintf("dl-%s-%02d-%s", short, track, filepath.Base(f.Path))
		if err := copyIncoming(source, filepath.Join(incoming, incomingName)); err != nil {
			if df, ok := err.(*downloadFailure); ok {
				return nil, df
			}
			return nil, err
		}
		result, err := audiorelease.Release(context.Background(), a.engine, audiorelease.Request{
			RequestID: audioRequestID(plan.RootJobID, track), RootID: plan.RootJobID,
			Artist: artist, Album: album, Title: title, Track: track,
			SourceName: incomingName, Origin: "local_import",
			Incoming: incoming, Library: library, Decoder: audiorelease.SystemDecoder(),
		})
		if err != nil {
			class := "INGEST_FAILED"
			switch {
			case errors.Is(err, audiorelease.ErrMissing):
				class = "AUDIO_MISSING"
			case errors.Is(err, audiorelease.ErrRejected):
				class = "AUDIO_REJECTED"
			case errors.Is(err, audiorelease.ErrTool):
				class = "DECODER_MISSING"
			case errors.Is(err, audiorelease.ErrConflict):
				class = "LIBRARY_CONFLICT"
			}
			return nil, downloadError(class, "入库失败："+filepath.Base(f.Path))
		}
		receipts = append(receipts, publishReceipt{
			TrackID: strconv.Itoa(track), RequestID: result.RequestID, SHA256: result.SHA256,
			Format: result.Format, Origin: result.Origin, SourceName: result.SourceName,
			DecodeState: result.DecodeState,
		})
	}
	if len(receipts) == 0 {
		return nil, downloadError("NO_AUDIO", "种子内没有可入库的音频文件")
	}
	return receipts, nil
}

// audioRequestID derives the deterministic release request id for one
// (root, track). It must stay identical to the derivation in releaseAudio
// (http.go): a manual POST /v1/audio/release for the same track replays
// the same record instead of ingesting twice.
func audioRequestID(root string, track int) string {
	sum := sha256.Sum256([]byte("album-audio/" + root + "/track/" + strconv.Itoa(track)))
	return "audio-" + hex.EncodeToString(sum[:16])
}

// truncateName caps a library name at the 80 characters Release accepts.
// Longer names are cut, not rejected: the audio is verified either way,
// and a failed ingest over a long title would block the whole download.
func truncateName(name string) string {
	if len(name) > 80 {
		return name[:80]
	}
	return name
}

// copyIncoming copies a seed file into incoming/ (or reuses the copy when
// the same bytes are already there, so a re-run does not ingest twice). A
// different file under the same name is a conflict, never an overwrite.
func copyIncoming(source, dest string) error {
	body, err := os.ReadFile(source)
	if err != nil {
		return downloadError("AUDIO_MISSING", "做种文件不可读")
	}
	if existing, err := os.ReadFile(dest); err == nil {
		if bytes.Equal(existing, body) {
			return nil
		}
		return downloadError("INGEST_FAILED", "incoming 文件冲突："+filepath.Base(dest))
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	temp := dest + ".tmp"
	if err := os.WriteFile(temp, body, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temp, dest); err != nil {
		os.Remove(temp)
		return err
	}
	return nil
}

// finishValidating runs the validating stage task the download executor
// queued when it advanced the job from downloading to validating. The task
// payload is the publish payload the download executor built from the
// ingested receipts (see finishDownload). This task is the second half of
// the download chain, and it is also the recovery path: if the download
// executor crashed after its Complete but before RecordRelease, this task
// still carries the receipts and completes the chain.
//
// It does NOT advance the process job to publishing. There is exactly one
// publish task for these receipts, the root job's, created by RecordRelease:
// evidence ids are global and the stored record embeds the owning job, so a
// second publish task verifying the same receipts would collide on the
// release record ("pub-<request id>") and on every per-track record
// ("publish-<track request id>") and fail with ErrStale. The process job's
// own work (download and ingest) is done here, so it completes terminally;
// the publish verification is tracked on the root job, which RecordRelease
// moved to publishing.
//
// RecordRelease is called here with the same request id the download
// executor used, so it is idempotent: when the executor already recorded
// the release, this call replays it; when the executor crashed before it,
// this call records it now and no manual POST /v1/audio/release is needed.
func (a *App) finishValidating(execution *scheduler.Execution, held taskrepo.Token, job workflow.Job, payload []byte) (scheduler.Outcome, error) {
	var receipt publishPayload
	if err := json.Unmarshal(payload, &receipt); err != nil || receipt.RootID == "" || len(receipt.Receipts) == 0 {
		return a.finishAcquire(held, job, "candidate", "验证阶段缺少入库收据，尚未发布", "VALIDATION_INCOMPLETE", nil)
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return scheduler.Outcome{}, err
	}
	message := "音频已入库，发布校验由根任务进行"
	if _, err := a.flows.RecordRelease(workflow.RecordReleaseInput{
		JobID: receipt.RootID, RequestID: "record-" + receipt.RequestID,
		Evidence: workflow.EvidenceInput{ID: "pub-" + receipt.RequestID, Kind: "catalog", Source: "local-library", SubjectID: receipt.RootID, TracklistVerified: true, Payload: encoded},
		Payload:  encoded, NowNS: time.Now().UnixNano(),
	}); err != nil {
		// The download and ingest are done either way; only the root
		// publish record failed (the root is paused or cancelled, or the
		// store refused it). Say so in the completion message instead of
		// failing a finished download: the operator can re-record the
		// release through POST /v1/audio/release.
		message = "音频已入库，但根任务未能进入发布：" + err.Error()
	}
	if _, err := a.flows.Complete(intentID("validating", held.ID, held.Fence), held, workflow.Complete{
		Edge: workflow.EdgeTerminal, NextStage: "completed", TaskState: "completed",
		Message: message, Result: encoded, NowNS: a.now().UnixNano(),
	}); err != nil {
		return scheduler.Outcome{}, err
	}
	return scheduler.Outcome{State: "completed", Message: message}, nil
}
