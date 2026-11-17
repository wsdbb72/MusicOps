package app

import (
	"strconv"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

func (a *App) recoverOrphanedTasks(currentOwner string, log StartupLogger) {
	tasks, err := a.tasks.RestartRecoverable(currentOwner)
	if err != nil {
		if log != nil {
			log("task_recovery", "scan failed: "+scrub(err.Error()))
		}
		return
	}
	recovered := 0
	for _, task := range tasks {
		if task.State == "recovery_required" {
			if task.Execution != "interrupted" || task.WorkflowJobID == "" || !autoRecoverableCommand(task.Command) {
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

// reconcilePublishedRoots closes pre-0.4.71 roots that reached publishing
// but never received a process follow-up. Only roots already present in the
// verified audio catalogue are touched, and only after a ten-minute grace
// period. This changes bookkeeping; it never copies, deletes or rewrites music.
func (a *App) reconcilePublishedRoots(now time.Time, log StartupLogger) {
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
func autoRecoverableCommand(command string) bool {
	switch command {
	case workflow.CommandSearch, workflow.CommandDownload, workflow.CommandIdentify, workflow.CommandSnapshot, workflow.CommandPublish:
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
