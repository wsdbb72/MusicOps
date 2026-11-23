package app

import (
	"fmt"
	"strconv"
	"strings"
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

// reconcilePublishedRoots closes roots that reached publishing without a
// process follow-up, or retained a stale VERIFY_FAILED after the verified
// library record became visible. Only roots already present in the verified
// audio catalogue are touched, and only after a ten-minute grace period. This
// changes bookkeeping; it never copies, deletes or rewrites music.
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
			publicationBoundary := job.Stage == "publishing" || job.Stage == "notifying"
			reconcilableVerifyFailure := job.Stage == "failed" && job.ErrorClass == "VERIFY_FAILED"
			if job.ParentJobID == "" && job.RootID == job.ID && (publicationBoundary || reconcilableVerifyFailure) && job.UpdatedNS <= cutoff && published[job.RootID] {
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

func libraryReconcileFailureSafe(class string) bool {
	switch class {
	case "AUDIO_REJECTED", "AUDIO_MISSING", "LIBRARY_CONFLICT", "NO_AUDIO", "ALREADY_IN_LIBRARY", "INGEST_FAILED":
		return true
	default:
		return false
	}
}

func (a *App) failedRootRecordingIdentity(job workflow.Job) (string, string, bool) {
	if job.ActiveTaskID != "" {
		if raw, found, err := a.tasks.Data(job.ActiveTaskID, "arguments", 0); err == nil && found {
			if plan, decodeErr := decodePlan(raw); decodeErr == nil {
				artist := strings.TrimSpace(plan.ExpectedArtist)
				title := strings.TrimSpace(firstNonEmpty(plan.PlaylistTrack, plan.ExpectedTitle))
				if artist != "" && title != "" {
					return artist, title, true
				}
			}
		}
	}
	label := strings.TrimSpace(job.Label)
	for _, separator := range []string{" - ", " — ", " – "} {
		if at := strings.Index(label, separator); at > 0 {
			artist := strings.TrimSpace(label[:at])
			title := strings.TrimSpace(label[at+len(separator):])
			if artist != "" && title != "" {
				return artist, title, true
			}
			break
		}
	}
	return "", "", false
}

// reconcileLibrarySatisfiedRoots closes acquisition failures when an explicit
// library reindex proves the exact title and credited artist are already on
// disk. It never closes quality upgrades, ambiguous identity failures, token
// debt or review states, and it never invents a download receipt.
func (a *App) reconcileLibrarySatisfiedRoots(now time.Time, log StartupLogger) int {
	cutoff := now.Add(-10 * time.Minute).UnixNano()
	index := a.playlistLibrary()
	var after []byte
	reconciled := 0
	for {
		page, err := a.flows.List(after, 200)
		if err != nil {
			if log != nil {
				log("workflow_reconcile", "library-satisfied scan failed: "+scrub(err.Error()))
			}
			break
		}
		for _, job := range page.Jobs {
			if job.ParentJobID != "" || job.RootID != job.ID || job.Stage != "failed" ||
				job.UpdatedNS > cutoff || !libraryReconcileFailureSafe(job.ErrorClass) {
				continue
			}
			artist, title, ok := a.failedRootRecordingIdentity(job)
			if !ok || !index.hasRecordingIdentity(artist, title) {
				continue
			}
			if _, changed, err := a.flows.ReconcileLibrarySatisfiedRoot(job.ID, cutoff, now.UnixNano()); err != nil {
				if log != nil {
					log("workflow_reconcile", "library-satisfied root "+job.ID+" failed: "+scrub(err.Error()))
				}
			} else if changed {
				reconciled++
			}
		}
		if len(page.Next) == 0 {
			break
		}
		after = page.Next
	}
	index.commitPresenceSnapshots()
	if reconciled > 0 && log != nil {
		log("workflow_reconcile", "closed "+strconv.Itoa(reconciled)+" library-satisfied root(s)")
	}
	return reconciled
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
