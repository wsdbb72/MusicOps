package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

const (
	libraryAutomationCheckpointVersion = 1
	libraryAutomationDefaultInterval   = 6 * time.Hour
	libraryAutomationReindexInterval   = 24 * time.Hour
	libraryAutomationFailureRetry      = 5 * time.Minute
	libraryAutomationReindexFileDelay  = 10 * time.Millisecond
)

var libraryAutomationCheckpointKey = []byte("app/library/automation-checkpoint-v1")

// libraryAutomationCheckpoint persists the schedule separately from the
// in-memory report. Without it, every restart queued another full maintenance
// pass five minutes later even when the previous pass had just completed.
type libraryAutomationSchedule struct {
	Version          int    `json:"version"`
	LastAttemptNS    int64  `json:"last_attempt_ns"`
	LastFinishedNS   int64  `json:"last_finished_ns"`
	LastSuccessfulNS int64  `json:"last_successful_ns"`
	LastReindexNS    int64  `json:"last_reindex_ns"`
	LastReindexRoot  string `json:"last_reindex_root,omitempty"`
	RetryAfterNS     int64  `json:"retry_after_ns,omitempty"`
	LastReason       string `json:"last_reason,omitempty"`
	LastStatus       string `json:"last_status,omitempty"`
}

func (a *App) loadLibraryAutomationCheckpoint() (libraryAutomationSchedule, error) {
	checkpoint := libraryAutomationSchedule{Version: libraryAutomationCheckpointVersion}
	if a == nil || a.engine == nil {
		return checkpoint, dataengine.ErrClosed
	}
	found, err := a.loadJSONSetting(libraryAutomationCheckpointKey, &checkpoint)
	if err != nil {
		return libraryAutomationSchedule{}, err
	}
	if !found || checkpoint.Version != libraryAutomationCheckpointVersion {
		return libraryAutomationSchedule{Version: libraryAutomationCheckpointVersion}, nil
	}
	return checkpoint, nil
}

func (a *App) updateLibraryAutomationCheckpoint(mutate func(*libraryAutomationSchedule)) error {
	if a == nil || a.engine == nil || mutate == nil {
		return dataengine.ErrInvalidRequest
	}
	for attempt := 0; attempt < 8; attempt++ {
		var current []byte
		var present bool
		if err := a.engine.View(func(v *dataengine.View) error {
			var err error
			current, present, err = v.Get(libraryAutomationCheckpointKey)
			return err
		}); err != nil {
			return err
		}
		checkpoint := libraryAutomationSchedule{Version: libraryAutomationCheckpointVersion}
		if present {
			if err := json.Unmarshal(current, &checkpoint); err != nil {
				return fmt.Errorf("自动维护检查点损坏: %w", err)
			}
			if checkpoint.Version != libraryAutomationCheckpointVersion {
				checkpoint = libraryAutomationSchedule{Version: libraryAutomationCheckpointVersion}
			}
		}
		mutate(&checkpoint)
		checkpoint.Version = libraryAutomationCheckpointVersion
		encoded, err := json.Marshal(checkpoint)
		if err != nil {
			return err
		}
		conditions := []dataengine.Condition{{Key: libraryAutomationCheckpointKey, Absent: true}}
		if present {
			conditions = []dataengine.Condition{{Key: libraryAutomationCheckpointKey, Value: current}}
		}
		ok, err := a.engine.CompareAndSwap(conditions, []dataengine.Mutation{{Key: libraryAutomationCheckpointKey, Value: encoded}})
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return errors.New("自动维护检查点写入冲突")
}

func (a *App) recordLibraryAutomationStarted(reason string) error {
	now := a.now().UnixNano()
	return a.updateLibraryAutomationCheckpoint(func(checkpoint *libraryAutomationSchedule) {
		checkpoint.LastAttemptNS = now
		checkpoint.LastReason = scrub(reason)
		checkpoint.LastStatus = "running"
		checkpoint.RetryAfterNS = 0
	})
}

func (a *App) recordLibraryAutomationFinished(runErr error) error {
	now := a.now().UnixNano()
	return a.updateLibraryAutomationCheckpoint(func(checkpoint *libraryAutomationSchedule) {
		checkpoint.LastFinishedNS = now
		switch {
		case runErr == nil:
			checkpoint.LastSuccessfulNS = now
			checkpoint.LastStatus = "success"
			checkpoint.RetryAfterNS = 0
		case errors.Is(runErr, context.Canceled):
			checkpoint.LastStatus = "cancelled"
			checkpoint.RetryAfterNS = 0
		default:
			checkpoint.LastStatus = "failed"
			checkpoint.RetryAfterNS = now + int64(libraryAutomationFailureRetry)
		}
	})
}

func (a *App) recordLibraryReindex(root string) error {
	now := a.now().UnixNano()
	return a.updateLibraryAutomationCheckpoint(func(checkpoint *libraryAutomationSchedule) {
		checkpoint.LastReindexNS = now
		checkpoint.LastReindexRoot = filepath.Clean(root)
	})
}

func libraryAutomationNextRun(now time.Time, checkpoint libraryAutomationSchedule, interval time.Duration) time.Time {
	if interval <= 0 {
		interval = libraryAutomationDefaultInterval
	}
	if checkpoint.RetryAfterNS > 0 {
		return time.Unix(0, checkpoint.RetryAfterNS)
	}
	if checkpoint.LastAttemptNS > checkpoint.LastFinishedNS {
		// The prior process stopped before recording a terminal result.
		return now.Add(libraryAutomationInitialDelay)
	}
	if checkpoint.LastFinishedNS > 0 {
		return time.Unix(0, checkpoint.LastFinishedNS).Add(interval)
	}
	return now.Add(libraryAutomationInitialDelay)
}

func libraryAutomationReindexDue(now time.Time, checkpoint libraryAutomationSchedule, root string) bool {
	if checkpoint.LastReindexNS <= 0 || checkpoint.LastReindexRoot == "" {
		return true
	}
	if !sameLibraryRoot(checkpoint.LastReindexRoot, root) {
		return true
	}
	last := time.Unix(0, checkpoint.LastReindexNS)
	return !now.Before(last.Add(libraryAutomationReindexInterval))
}

func sameLibraryRoot(left, right string) bool {
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}
