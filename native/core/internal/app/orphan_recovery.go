package app

import (
	"fmt"
	"strconv"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func (a *App) recoverOrphanedTasks(currentOwner string, log StartupLogger) {
	if log != nil {
		log("task_recovery", "scanning stale task owners and interrupted attempts")
	}
	tasks, err := a.tasks.RestartRecoverable(currentOwner)
	if err != nil {
		if log != nil {
			log("task_recovery", "scan failed: "+scrub(err.Error()))
		}
		return
	}
	if log != nil {
		log("task_recovery", fmt.Sprintf("classified %d recoverable task(s)", len(tasks)))
	}
	recovered := 0
	for index, task := range tasks {
		if log != nil && index > 0 && index%25 == 0 {
			log("task_recovery", fmt.Sprintf("processed %d/%d recoverable task(s)", index, len(tasks)))
		}
		if task.State == "recovery_required" {
			if task.Execution != "interrupted" || task.WorkflowJobID == "" {
				continue
			}
			if task.CancelRequested {
				if _, err = a.flows.Control("orphan-cancel-"+task.ID+"-"+strconv.FormatUint(task.Fence, 10), task.WorkflowJobID, "cancel", a.now().UnixNano()); err == nil {
					recovered++
				} else if log != nil {
					log("task_recovery", "cancel "+task.ID+" failed: "+scrub(err.Error()))
				}
				continue
			}
			if !autoRecoverableCommand(task.Command) {
				continue
			}
			if task.Command == workflow.CommandDownload && !a.restoreDownloadCredentials(task.WorkflowJobID) {
				if log != nil {
					log("task_recovery", "download "+task.ID+" left interrupted: no saved qBittorrent login")
				}
				continue
			}
			if _, err = a.flows.RecoverInterrupted("orphan-recover-"+task.ID+"-"+strconv.FormatUint(task.Fence, 10), task.WorkflowJobID, time.Now().UnixNano()); err == nil {
				recovered++
			} else if log != nil {
				log("task_recovery", "recover "+task.ID+" failed: "+scrub(err.Error()))
			}
			continue
		}
		if task.Owner == "" {
			continue
		}
		token := taskrepo.Token{ID: task.ID, Owner: task.Owner, Fence: task.Fence}
		now := a.now().UnixNano()
		if task.Execution == "claimed" {
			if _, err = a.tasks.ReleaseClaim("orphan-claim-"+task.ID+"-"+task.Owner, token, now); err == nil {
				recovered++
			} else if log != nil {
				log("task_recovery", "release "+task.ID+" failed: "+scrub(err.Error()))
			}
			continue
		}
		if task.Execution != "started" || task.WorkflowJobID == "" || !autoRecoverableCommand(task.Command) {
			continue
		}
		if task.Command == workflow.CommandDownload && !a.restoreDownloadCredentials(task.WorkflowJobID) {
			if log != nil {
				log("task_recovery", "download "+task.ID+" left interrupted: no saved qBittorrent login")
			}
			continue
		}
		if _, err = a.flows.Interrupt("orphan-interrupt-"+task.ID+"-"+task.Owner, token, now, "旧进程已退出，正在自动核对并恢复"); err != nil {
			if log != nil {
				log("task_recovery", "interrupt "+task.ID+" failed: "+scrub(err.Error()))
			}
			continue
		}
		if _, err = a.flows.RecoverInterrupted("orphan-recover-"+task.ID+"-"+task.Owner, task.WorkflowJobID, time.Now().UnixNano()); err == nil {
			recovered++
		} else if log != nil {
			log("task_recovery", "recover "+task.ID+" failed: "+scrub(err.Error()))
		}
	}
	if recovered > 0 && log != nil {
		log("task_recovery", "automatically recovered "+strconv.Itoa(recovered)+" task(s)")
	}
}

// reconcileSupersededQueuedTasks performs the workflow-side cleanup after
// orphan recovery and before any scheduler worker can claim a task. Old retry
// chains and re-enqueued jobs can leave queued rows whose job now points at a
// different task or stage; executing one produces a false RECOVERY_REQUIRED
// review. These rows never crossed Begin, so cancelling the task is side-effect
// free and does not change the current workflow job.
func (a *App) reconcileSupersededQueuedTasks(log StartupLogger) {
	prefix := "startup-queued-" + strconv.FormatInt(a.now().UnixNano(), 10)
	count, err := a.flows.ReconcileQueuedSuperseded(prefix, a.now().UnixNano())
	if err != nil && log != nil {
		log("task_recovery", "queued reconciliation failed: "+scrub(err.Error()))
	}
	if count > 0 && log != nil {
		log("task_recovery", "cancelled "+strconv.Itoa(count)+" superseded queued task(s)")
	}
}

// normalizeLegacyRetrySchedules moves 0.4.90 job-only retry deadlines onto
// queued task records. Those tasks used to sleep inside a worker after they
// were claimed; persisting the deadline lets the new claimer skip them until
// due and continue with unrelated work.
func (a *App) normalizeLegacyRetrySchedules(log StartupLogger) {
	if log != nil {
		log("task_recovery", "normalizing legacy retry schedules")
	}
	var after []byte
	updated := 0
	for {
		page, err := a.tasks.List("identify", "queued", after, 200)
		if err != nil {
			if log != nil {
				log("task_recovery", "legacy retry scan failed: "+scrub(err.Error()))
			}
			return
		}
		for _, task := range page.Tasks {
			if task.Command != workflow.CommandSearch || task.NotBeforeNS != 0 || task.WorkflowJobID == "" {
				continue
			}
			job, jobErr := a.flows.Job(task.WorkflowJobID)
			if jobErr != nil || job == nil || job.Stage != "searching" || job.SearchRetryAfterSec <= 0 {
				continue
			}
			due := job.UpdatedNS + int64(job.SearchRetryAfterSec)*int64(time.Second)
			if _, err = a.tasks.Schedule("legacy-schedule-"+task.ID+"-"+strconv.FormatUint(task.Revision, 10), task.ID, due, a.now().UnixNano()); err == nil {
				updated++
			}
		}
		if len(page.Next) == 0 {
			break
		}
		after = page.Next
	}
	if updated > 0 && log != nil {
		log("task_recovery", "normalized "+strconv.Itoa(updated)+" legacy retry deadline(s)")
	}
}

// reconcilePublishedRoots closes pre-0.4.71 roots that reached publishing
// but never received a process follow-up. Only roots already present in the
// verified audio catalogue are touched, and only after a ten-minute grace
// period. This changes bookkeeping; it never copies, deletes or rewrites music.
func (a *App) reconcilePublishedRoots(stop <-chan struct{}, now time.Time, log StartupLogger) {
	if stopped(stop) {
		return
	}
	published, err := audiorelease.Albums(a.engine)
	if err != nil {
		if log != nil {
			log("workflow_reconcile", "audio catalogue unavailable: "+scrub(err.Error()))
		}
		return
	}
	cutoff := now.Add(-10 * time.Minute).UnixNano()
	candidates := map[string]int64{}
	var after []byte
	for {
		page, err := a.flows.List(after, 200)
		if err != nil {
			if log != nil {
				log("workflow_reconcile", "job scan failed: "+scrub(err.Error()))
			}
			return
		}
		for _, job := range page.Jobs {
			if job.ParentJobID == "" && job.RootID == job.ID && (job.Stage == "publishing" || job.Stage == "notifying") && job.UpdatedNS <= cutoff && published[job.RootID] {
				candidates[job.ID] = job.UpdatedNS
			}
		}
		if len(page.Next) == 0 {
			break
		}
		after = page.Next
	}
	reconciled := 0
	for root := range candidates {
		if job, changed, err := a.flows.ReconcilePublishedRoot(root, cutoff, now.UnixNano()); err != nil {
			if log != nil {
				log("workflow_reconcile", "root "+root+" failed: "+scrub(err.Error()))
			}
		} else if changed && job != nil {
			reconciled++
		}
	}
	if reconciled > 0 && log != nil {
		log("workflow_reconcile", "closed "+strconv.Itoa(reconciled)+" published root(s)")
	}
}

func stopped(stop <-chan struct{}) bool {
	if stop == nil {
		return false
	}
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

func autoRecoverableCommand(command string) bool {
	// Process is included so the workflow layer can decide on its narrow safe
	// subset. RecoverInterrupted still refuses tracker acquire stages that may
	// have spent a token; only NetEase playlist fallbacks are replayed.
	switch command {
	case workflow.CommandSearch, workflow.CommandDownload, workflow.CommandIdentify, workflow.CommandSnapshot, workflow.CommandPublish, workflow.CommandProcess:
		return true
	default:
		return false
	}
}

func (a *App) restoreDownloadCredentials(jobID string) bool {
	effective, resolved := a.effectiveDownloadRequest(downloadRequest{})
	if !resolved {
		return false
	}
	origin, creds, _, _, ok := a.validDownloadSettings(effective)
	if !ok {
		return false
	}
	_, err := a.rememberQBittorrent(jobID, qbittorrentCredentials{BaseURL: origin, Username: creds.Username, Password: creds.Password})
	return err == nil
}
