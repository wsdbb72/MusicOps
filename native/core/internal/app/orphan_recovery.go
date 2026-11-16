package app

import (
	"strconv"
	"time"

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
