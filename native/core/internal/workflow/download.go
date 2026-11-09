// StartDownload moves a blocked acquire job into downloading and queues the
// download_release task that runs the qBittorrent download.
//
// Only a process job in needs_review with ReviewKind "candidate" and a
// blocked task may start: that is the job the acquire executor left behind
// after saving a torrent receipt, and the receipt must still be stored as
// evidence for the site/resource the plan names. A streaming-fallback job
// also ends in needs_review/candidate, but it has no receipt, so the
// evidence check refuses it instead of letting the executor fail later on
// a torrent that was never saved.
//
// The blocked task completes and the download task queues in one atomic
// commit through taskrepo.ResolveOwned, exactly like Review. The download
// task belongs to the same job (WorkflowJobID is the original job id); no
// new job is created. The plan payload must never contain credentials: the
// qBittorrent login lives only in the app's in-memory secrets and is handed
// to the executor through that map, never through this payload.
//
// FreeConfirmed must be true: a download task only runs on confirmed free
// evidence. The app derives it from the torrent receipt (see
// app/download.go): the acquire executor only fetches a torrent without a
// token after sitefree confirmed it free for this user, and a token fetch
// buys freeleech for that torrent, so either branch is confirmed free.
// Anything the handler cannot trace to the acquire decision stays
// unconfirmed and is refused here, before any task is queued.
package workflow

import (
	"encoding/json"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
)

// DownloadInput is one download start. Plan is the durable download plan
// (site, resource, infohash, expected titles, qBittorrent save path /
// category / tags); it must never contain credentials.
type DownloadInput struct {
	Plan          []byte
	Site          string
	ResourceID    string
	InfoHash      string
	FreeConfirmed bool
}

// DownloadTaskID is the download task for one process job.
func DownloadTaskID(jobID string) string { return "download:" + jobID }

func validInfoHash(hash string) bool {
	if len(hash) != 40 && len(hash) != 64 {
		return false
	}
	for _, r := range hash {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'f' {
			continue
		}
		return false
	}
	return true
}

func validResourceID(id string) bool {
	if id == "" || id[0] == '0' {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// storedReceipt reports whether the job holds a torrent receipt for this
// site/resource: the acquire executor stores the receipt (site, resource_id,
// metainfo_sha256, path) as evidence when it saves the torrent bytes. A
// receipt whose payload does not parse is skipped, not treated as a match.
func (r *Repository) storedReceipt(jobID, site, resource string) (bool, error) {
	var after []byte
	for {
		page, err := r.EvidenceFor(jobID, after, 200)
		if err != nil {
			return false, err
		}
		for _, item := range page.Evidence {
			payload, found, err := r.EvidenceData(item.ID)
			if err != nil {
				return false, err
			}
			if !found {
				return false, ErrIntegrity
			}
			var receipt struct {
				Site       string `json:"site"`
				ResourceID string `json:"resource_id"`
				MetaSHA256 string `json:"metainfo_sha256"`
			}
			if json.Unmarshal(payload, &receipt) != nil {
				continue
			}
			if receipt.Site == site && receipt.ResourceID == resource && receipt.MetaSHA256 != "" {
				return true, nil
			}
		}
		if len(page.Next) == 0 {
			return false, nil
		}
		after = page.Next
	}
}

// StartDownload atomically completes the blocked acquire task, queues the
// download_release task on the aux lane, and moves the job to downloading
// with ActiveTaskID pointing at the new task. Calling it again while the
// job is already downloading replays the current job instead of queuing a
// second download task.
func (r *Repository) StartDownload(requestID, jobID string, in DownloadInput, nowNS int64) (Result, error) {
	if !validID(jobID) || requestID == "" || len(requestID) > 128 || nowNS <= 0 {
		return Result{}, dataengine.ErrInvalidRequest
	}
	if len(in.Plan) == 0 || len(in.Plan) > taskrepo.PayloadLimit || !json.Valid(in.Plan) {
		return Result{}, dataengine.ErrInvalidRequest
	}
	if !sourcePattern.MatchString(in.Site) || !validResourceID(in.ResourceID) || !validInfoHash(in.InfoHash) || !in.FreeConfirmed {
		return Result{}, dataengine.ErrInvalidRequest
	}
	job, task, err := r.reviewTarget(jobID)
	if err != nil {
		return Result{}, err
	}
	if job == nil {
		return Result{}, ErrNotFound
	}
	if task == nil {
		return Result{}, ErrStale
	}
	if job.CancelRequested || task.CancelRequested {
		return Result{}, ErrStale
	}
	if job.Paused {
		return Result{}, ErrStale
	}
	downloadID := DownloadTaskID(jobID)
	if job.Stage == "downloading" {
		if job.ActiveTaskID == downloadID && task.ID == downloadID && task.Command == cmdDownload {
			return Result{Job: job, Replayed: true}, nil
		}
		return Result{}, ErrStale
	}
	if job.Stage != "needs_review" || task.State != "blocked" || job.ReviewKind != "candidate" || job.TaskType != "album" || task.Command != cmdProcess {
		return Result{}, ErrStale
	}
	receipt, err := r.storedReceipt(jobID, in.Site, in.ResourceID)
	if err != nil {
		return Result{}, err
	}
	if !receipt {
		return Result{}, ErrStale
	}
	followup := taskrepo.NewTask{Spec: taskrepo.Spec{ID: downloadID, Command: cmdDownload, Lane: "aux", Priority: 1, CreatedNS: nowNS}, Payload: in.Plan}
	end := taskrepo.Completion{State: "completed", NowNS: nowNS, Message: "已确认下载，等待 qBittorrent"}
	extra, _ := json.Marshal(struct{ JobID, Action string }{jobID, "start-download"})
	result, err := r.tasks.ResolveOwned(requestID, task.ID, end, []taskrepo.NewTask{followup}, map[string]string{downloadID: jobID}, extra, func(v *dataengine.View, _, after *taskrepo.Task, children []taskrepo.Task) ([]dataengine.Condition, []dataengine.Mutation, json.RawMessage, error) {
		live, raw, err := readJob(v, jobID)
		if err != nil || live == nil {
			if err != nil {
				return nil, nil, nil, err
			}
			return nil, nil, nil, ErrNotFound
		}
		if live.Revision != job.Revision || live.Stage != "needs_review" || live.ActiveTaskID != task.ID {
			return nil, nil, nil, errRetry
		}
		if after == nil || len(children) != 1 || children[0].ID != downloadID || children[0].WorkflowJobID != jobID {
			return nil, nil, nil, ErrStale
		}
		next := *live
		next.Stage = "downloading"
		next.ActiveTaskID = downloadID
		next.ReviewKind = ""
		next.ErrorClass = ""
		next.Message = "正在下载（qBittorrent），尚未入库"
		updated, err := bump(next, nowNS, true)
		if err != nil {
			return nil, nil, nil, err
		}
		condition, writes, err := putJob(raw, updated)
		if err != nil {
			return nil, nil, nil, err
		}
		attached, err := json.Marshal(Result{Job: &updated})
		return []dataengine.Condition{condition}, writes, attached, err
	})
	return decode(result, err)
}
