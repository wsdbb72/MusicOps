package workflow

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/searchdecision"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
)

// ReconcileQueuedSuperseded cancels queued workflow tasks that no longer
// belong to their job's current stage. Playlist re-enqueue, recovery and old
// retry chains can leave such rows behind; letting the scheduler claim them
// creates a misleading "task ownership or state changed" review even though
// the replacement task is already correct. This is bookkeeping only and only
// runs before scheduler workers start.
func (r *Repository) ReconcileQueuedSuperseded(requestPrefix string, nowNS int64) (int, error) {
	if r == nil || requestPrefix == "" || nowNS <= 0 {
		return 0, dataengine.ErrInvalidRequest
	}
	reconciled := 0
	for _, lane := range []string{"identify", "publish", "transcode", "aux", "download"} {
		var after []byte
		for {
			page, err := r.tasks.List(lane, "queued", after, 200)
			if err != nil {
				return reconciled, err
			}
			candidates := make([]taskrepo.Task, 0)
			if err := r.engine.View(func(v *dataengine.View) error {
				for _, task := range page.Tasks {
					if task.WorkflowJobID == "" {
						continue
					}
					live, _, readErr := readJob(v, task.WorkflowJobID)
					if readErr != nil {
						return readErr
					}
					if live != nil && queuedTaskStillCurrent(v, *live, &task) {
						continue
					}
					candidates = append(candidates, task)
				}
				return nil
			}); err != nil {
				return reconciled, err
			}
			// Most queued rows are current. Only the small superseded subset
			// needs a read-write CAS, so a large healthy backlog no longer
			// serializes hundreds of no-op commits during startup.
			for _, task := range candidates {
				requestID := requestPrefix + "-" + digestBytes([]byte(task.ID + "/" + strconv.FormatUint(task.Revision, 10)))[:32]
				result, err := r.discardQueuedSuperseded(requestID, task, nowNS)
				if errors.Is(err, ErrStale) {
					continue
				}
				if err != nil {
					return reconciled, err
				}
				if result.Changed {
					reconciled++
				}
			}
			if len(page.Next) == 0 {
				break
			}
			after = page.Next
		}
	}
	return reconciled, nil
}

func (r *Repository) discardQueuedSuperseded(requestID string, task taskrepo.Task, nowNS int64) (Result, error) {
	extra, _ := json.Marshal(struct{ JobID, Action string }{task.WorkflowJobID, "superseded"})
	result, err := r.tasks.ControlOwned(requestID, task.ID, "cancel", nowNS, extra, func(v *dataengine.View, before, _ *taskrepo.Task, _ []taskrepo.Task) ([]dataengine.Condition, []dataengine.Mutation, json.RawMessage, error) {
		if before == nil || before.State != "queued" || before.WorkflowJobID != task.WorkflowJobID || before.Revision != task.Revision {
			return nil, nil, nil, ErrStale
		}
		live, _, err := readJob(v, before.WorkflowJobID)
		if err != nil {
			return nil, nil, nil, err
		}
		if live != nil && queuedTaskStillCurrent(v, *live, before) {
			return nil, nil, nil, ErrStale
		}
		attached, err := json.Marshal(Result{Job: live})
		return nil, nil, attached, err
	})
	return decode(result, err)
}

func queuedTaskStillCurrent(v *dataengine.View, job Job, task *taskrepo.Task) bool {
	if task == nil || job.CancelRequested || terminalStage(job.Stage) || job.ActiveTaskID != task.ID {
		return false
	}
	stage, ok := queuedTaskStage(v, job, task)
	return ok && stage == job.Stage
}

func queuedTaskStage(v *dataengine.View, job Job, task *taskrepo.Task) (string, bool) {
	switch task.Command {
	case cmdSearch:
		return "searching", true
	case cmdDownload:
		return "downloading", true
	case cmdIdentify:
		return "identifying", true
	case cmdSnapshot:
		return "snapshotting", true
	case cmdPrepare:
		return "preparing", true
	case cmdRepair:
		return "metadata", true
	case cmdPublish:
		if task.ID == job.ID && job.Stage == "publishing" {
			return "publishing", true
		}
		if stage, found := strings.CutPrefix(task.ID, "stage:"+job.ID+":"); found && stage == "publishing" {
			return stage, true
		}
		return "", false
	case cmdProcess:
		if task.ID == job.ID && (job.Stage == "requesting" || job.Stage == "fallback") {
			return job.Stage, true
		}
		if stage, found := strings.CutPrefix(task.ID, "stage:"+job.ID+":"); found && stages[stage] {
			return stage, true
		}
		if job.ParentJobID == "" {
			return "", false
		}
		parent, _, err := readJob(v, job.ParentJobID)
		if err != nil || parent == nil {
			return "", false
		}
		if parent.SearchAction == searchdecision.ActionFallback {
			return "fallback", true
		}
		return "requesting", true
	default:
		return "", false
	}
}

var errClaimedTaskCurrent = errors.New("claimed workflow task is still current")

// ReconcileClaimedSuperseded runs between Claim and Begin. It prevents both
// stale retry rows and concurrent workers from entering a workflow that has
// already moved to another task. A current claim is left untouched; a
// superseded claim is safely cancelled because no executor has started yet.

// ClaimedBeginDomain is the atomic half of the pre-execution gate. The
// scheduler passes it to taskrepo.BeginOwned, which verifies the claimed task
// and this returned job condition in one CAS. A job revision that changes
// after the advisory gate therefore cannot start a superseded executor.
func (r *Repository) ClaimedBeginDomain(task taskrepo.Task) taskrepo.Domain {
	return func(v *dataengine.View, before, _ *taskrepo.Task, _ []taskrepo.Task) ([]dataengine.Condition, []dataengine.Mutation, json.RawMessage, error) {
		if before == nil || before.WorkflowJobID == "" || before.ID != task.ID || before.Owner != task.Owner ||
			before.Fence != task.Fence || before.Revision != task.Revision || before.Execution != "claimed" {
			return nil, nil, nil, ErrStale
		}
		live, raw, err := readJob(v, before.WorkflowJobID)
		if err != nil || live == nil {
			if err != nil {
				return nil, nil, nil, err
			}
			return nil, nil, nil, ErrNotFound
		}
		if !queuedTaskStillCurrent(v, *live, before) {
			return nil, nil, nil, ErrStale
		}
		attached, err := json.Marshal(Result{Job: live})
		return []dataengine.Condition{{Key: jobKey(live.ID), Value: raw}}, nil, attached, err
	}
}

func (r *Repository) ReconcileClaimedSuperseded(requestPrefix string, task taskrepo.Task, nowNS int64) (bool, error) {
	if r == nil || requestPrefix == "" || task.ID == "" || nowNS <= 0 {
		return false, dataengine.ErrInvalidRequest
	}
	if task.WorkflowJobID == "" {
		return false, nil
	}
	requestID := requestPrefix + "-" + digestBytes([]byte(task.ID + "/" + strconv.FormatUint(task.Revision, 10)))[:32]
	extra, _ := json.Marshal(struct{ JobID, Action string }{task.WorkflowJobID, "superseded_claim"})
	result, err := r.tasks.ControlOwned(requestID, task.ID, "cancel", nowNS, extra, func(v *dataengine.View, before, _ *taskrepo.Task, _ []taskrepo.Task) ([]dataengine.Condition, []dataengine.Mutation, json.RawMessage, error) {
		if before == nil || before.WorkflowJobID != task.WorkflowJobID || (before.State != "queued" && before.State != "running") || before.Execution != "claimed" || before.Owner != task.Owner || before.Fence != task.Fence || before.Revision != task.Revision {
			return nil, nil, nil, ErrStale
		}
		live, _, err := readJob(v, before.WorkflowJobID)
		if err != nil {
			return nil, nil, nil, err
		}
		if live != nil && queuedTaskStillCurrent(v, *live, before) {

			return nil, nil, nil, errClaimedTaskCurrent
		}
		attached, err := json.Marshal(Result{Job: live})
		return nil, nil, attached, err
	})
	if errors.Is(err, errClaimedTaskCurrent) {
		return false, nil
	}
	if errors.Is(err, ErrStale) {
		// The claim moved or disappeared while being validated. Never execute
		// the old snapshot; let the scheduler obtain a fresh row.
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return result.Changed, nil
}
