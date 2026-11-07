package scheduler

import (
	"context"
	"sync"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
)

type Execution struct {
	repo      *taskrepo.Repository
	token     taskrepo.Token
	mu        sync.Mutex
	sequence  uint64
	last      taskrepo.Point
	lastWrite time.Time
	interval  time.Duration
}

func (e *Execution) Token() taskrepo.Token { return e.token }

// Checkpoint is called at safe boundaries, before starting the next external
// action. Waiting for resume holds no database snapshot or transaction open.
func (e *Execution) Checkpoint(ctx context.Context, phase string, percent float64, message string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		changed := e.repo.Changes()
		current, err := e.repo.Get(e.token.ID)
		if err != nil {
			return err
		}
		if current == nil || current.Owner != e.token.Owner || current.Fence != e.token.Fence || current.Execution != "started" {
			return taskrepo.ErrStale
		}
		if current.CancelRequested {
			return ErrCancelled
		}
		contentChanged := phase != e.last.Phase || percent != e.last.Percent || message != e.last.Message
		write := e.sequence == 0 || phase != e.last.Phase || contentChanged && time.Since(e.lastWrite) >= e.interval
		if current.PauseRequested && current.State != "paused" {
			write = true
		}
		if write {
			e.sequence++
			point := taskrepo.Point{Sequence: e.sequence, Phase: phase, Percent: percent, Message: message, NowNS: time.Now().UnixNano()}
			reply, err := e.repo.Progress(e.token, point)
			if err != nil {
				return err
			}
			e.last = point
			e.lastWrite = time.Now()
			if reply.Decision == "cancel" {
				return ErrCancelled
			}
			current = &reply.Task
		}
		if !current.PauseRequested {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
