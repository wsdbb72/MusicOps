// Package scheduler keeps independent bounded lanes full without waiting for
// a wave's slowest task. Production handlers are registered by the application;
// this package contains no demo handler or success-producing placeholder.
package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
)

var ErrCancelled = errors.New("cancelled at an execution checkpoint")
var ErrShutdownPending = errors.New("owned execution has not reached shutdown")

type Outcome struct {
	State               string
	Payload             []byte
	ErrorClass, Message string
	Followups           []taskrepo.NewTask
}

// A handler returns only after all of its owned child work has stopped. An
// unclassified error/panic requires reconciliation; it is never blindly retried.
type Handler func(context.Context, *Execution, []byte) (Outcome, error)
type Config struct {
	Limits           map[string]int
	ProgressInterval time.Duration
}

func DefaultConfig() Config {
	return Config{Limits: map[string]int{"identify": 4, "publish": 1, "transcode": 1, "aux": 2}, ProgressInterval: 500 * time.Millisecond}
}

type Health struct {
	Owner    string            `json:"owner"`
	Stopping bool              `json:"stopping"`
	Active   map[string]string `json:"active"`
	Errors   map[string]string `json:"errors"`
}

// WorkflowHooks let a missing executor record that fact on the workflow job.
// Without them, a workflow-owned task is left running instead of reported complete.
type WorkflowHooks struct {
	Block     func(requestID string, token taskrepo.Token, nowNS int64) error
	Interrupt func(requestID string, token taskrepo.Token, nowNS int64, reason string) error
}

type Coordinator struct {
	repo     *taskrepo.Repository
	config   Config
	handlers map[string]Handler
	hooks    WorkflowHooks
	owner    string
	mu       sync.Mutex
	cancel   context.CancelFunc
	ctx      context.Context
	done     chan struct{}
	active   map[string]string
	failures map[string]string
}

func nonce() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic("OS random source unavailable")
	}
	return hex.EncodeToString(value[:])
}
func New(repo *taskrepo.Repository, handlers map[string]Handler, config Config) (*Coordinator, error) {
	if repo == nil || len(config.Limits) != 4 || config.Limits["publish"] != 1 || config.ProgressInterval <= 0 {
		return nil, errors.New("invalid bounded scheduler configuration")
	}
	for _, lane := range []string{"identify", "publish", "transcode", "aux"} {
		if config.Limits[lane] < 1 || config.Limits[lane] > 8 {
			return nil, errors.New("invalid lane capacity")
		}
	}
	copied := map[string]Handler{}
	for name, handler := range handlers {
		if handler == nil {
			return nil, errors.New("nil executor")
		}
		copied[name] = handler
	}
	limits := map[string]int{}
	for lane, value := range config.Limits {
		limits[lane] = value
	}
	config.Limits = limits
	return &Coordinator{repo: repo, handlers: copied, config: config, owner: nonce(), done: make(chan struct{}), active: map[string]string{}, failures: map[string]string{}}, nil
}
func (c *Coordinator) SetWorkflowHooks(hooks WorkflowHooks) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		return errors.New("workflow hooks must be installed before the scheduler starts")
	}
	c.hooks = hooks
	return nil
}

func (c *Coordinator) Start(parent context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		return errors.New("coordinator already started")
	}
	ctx, cancel := context.WithCancel(parent)
	c.cancel = cancel
	c.ctx = ctx
	var workers sync.WaitGroup
	for lane, limit := range c.config.Limits {
		for i := 0; i < limit; i++ {
			workers.Add(1)
			go func(lane string) { defer workers.Done(); c.worker(ctx, lane) }(lane)
		}
	}
	go func() { workers.Wait(); close(c.done) }()
	return nil
}
func (c *Coordinator) Stop(ctx context.Context) error {
	c.mu.Lock()
	cancel := c.cancel
	c.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ErrShutdownPending
	}
}
func (c *Coordinator) Health() Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	active := map[string]string{}
	failures := map[string]string{}
	for key, value := range c.active {
		active[key] = value
	}
	for key, value := range c.failures {
		failures[key] = value
	}
	stopping := c.ctx != nil && c.ctx.Err() != nil
	return Health{Owner: c.owner, Stopping: stopping, Active: active, Errors: failures}
}
func (c *Coordinator) failure(lane string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		delete(c.failures, lane)
	} else {
		c.failures[lane] = fmt.Sprintf("%T", err)
	}
}
func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func (c *Coordinator) worker(ctx context.Context, lane string) {
	for ctx.Err() == nil {
		changed := c.repo.Changes()
		requestID := "claim-" + nonce()
		var result taskrepo.Result
		var err error
		// Retry a storage uncertainty using the same intent ID, not another claim.
		for {
			result, err = c.repo.Claim(requestID, lane, c.owner, time.Now().UnixNano())
			if err == nil {
				c.failure(lane, nil)
				break
			}
			c.failure(lane, err)
			if !wait(ctx, 100*time.Millisecond) {
				return
			}
		}
		if result.Task == nil {
			select {
			case <-ctx.Done():
				return
			case <-changed:
			case <-time.After(time.Second):
			}
			continue
		}
		c.execute(ctx, *result.Task)
	}
}
func (c *Coordinator) execute(ctx context.Context, claimed taskrepo.Task) {
	token := taskrepo.Token{ID: claimed.ID, Owner: claimed.Owner, Fence: claimed.Fence}
	c.mu.Lock()
	hooks := c.hooks
	c.active[claimed.ID] = claimed.Lane
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.active, claimed.ID); c.mu.Unlock() }()
	beginID := "begin-" + nonce()
	for {
		if ctx.Err() != nil {
			current, err := c.repo.Get(token.ID)
			if err == nil && current != nil && current.Owner == token.Owner && current.Fence == token.Fence {
				if current.Execution == "claimed" {
					_, err = c.repo.ReleaseClaim("release-"+nonce(), token, time.Now().UnixNano())
				} else if current.Execution == "started" {
					err = c.interrupt(hooks, token, "启动交接期间退出，等待核对执行记录")
				}
			}
			if err != nil && !errors.Is(err, taskrepo.ErrStale) {
				c.failure(claimed.Lane, err)
			}
			return
		}
		changed := c.repo.Changes()
		current, err := c.repo.Get(claimed.ID)
		if err != nil {
			c.failure(claimed.Lane, err)
			if !wait(ctx, 100*time.Millisecond) {
				continue
			}
			continue
		}
		if current == nil || current.Owner != token.Owner || current.Fence != token.Fence {
			return
		}
		if current.Execution == "started" {
			receipt, queryErr := c.repo.Request(beginID)
			if queryErr == nil && receipt.State == "COMMITTED" {
				break
			}
			if queryErr != nil {
				c.failure(claimed.Lane, queryErr)
				wait(ctx, 100*time.Millisecond)
				continue
			}
			return
		}
		if current.Execution != "claimed" {
			return
		}
		if current.PauseRequested {
			select {
			case <-ctx.Done():
			case <-changed:
			}
			continue
		}
		_, err = c.repo.Begin(beginID, token, time.Now().UnixNano())
		if err == nil {
			break
		}
		if errors.Is(err, taskrepo.ErrStale) {
			beginID = "begin-" + nonce()
			continue
		}
		c.failure(claimed.Lane, err)
		if !wait(ctx, 100*time.Millisecond) {
			continue
		}
	}
	execution := &Execution{repo: c.repo, token: token, interval: c.config.ProgressInterval}
	outcome, err := c.invoke(ctx, execution, claimed)
	if ctx.Err() != nil || (err != nil && !errors.Is(err, ErrCancelled)) {
		if persist := c.interrupt(hooks, token, "执行已退出，等待自动核对外部操作记录"); persist != nil {
			c.failure(claimed.Lane, persist)
		}
		return
	}
	if errors.Is(err, ErrCancelled) {
		outcome.State = "cancelled"
		outcome.ErrorClass = "CANCELLED"
	}
	if outcome.State == "" {
		outcome.State = "completed"
	}
	switch outcome.State {
	case "completed", "failed", "cancelled", "blocked", "recovery_required":
	default:
		if persist := c.interrupt(hooks, token, "执行器返回无效状态，等待核对执行记录"); persist != nil {
			c.failure(claimed.Lane, persist)
		}
		return
	}
	if claimed.WorkflowJobID != "" && outcome.State == "blocked" && outcome.ErrorClass == "UNSUPPORTED_COMMAND" && hooks.Block != nil {
		if persist := hooks.Block("workflow-unregistered-"+nonce(), token, time.Now().UnixNano()); persist != nil && !errors.Is(persist, taskrepo.ErrStale) {
			c.failure(claimed.Lane, persist)
		}
		return
	}
	completion := taskrepo.Completion{State: outcome.State, Payload: outcome.Payload, ErrorClass: outcome.ErrorClass, Message: outcome.Message, NowNS: time.Now().UnixNano()}
	requestID := "finish-" + nonce()
	for {
		_, persist := c.repo.Finish(requestID, token, completion, outcome.Followups)
		if persist == nil || errors.Is(persist, taskrepo.ErrStale) || errors.Is(persist, taskrepo.ErrWorkflowOwned) {
			return
		}
		c.failure(claimed.Lane, persist)
		if !wait(ctx, 100*time.Millisecond) {
			return
		}
	}
}
func (c *Coordinator) interrupt(hooks WorkflowHooks, token taskrepo.Token, reason string) error {
	now := time.Now().UnixNano()
	_, err := c.repo.Interrupt("interrupted-"+nonce(), token, now, reason)
	if errors.Is(err, taskrepo.ErrWorkflowOwned) && hooks.Interrupt != nil {
		return hooks.Interrupt("workflow-interrupt-"+nonce(), token, now, reason)
	}
	return err
}

func (c *Coordinator) invoke(ctx context.Context, e *Execution, task taskrepo.Task) (out Outcome, err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("executor panic; reconcile effects")
		}
	}()
	if err = e.Checkpoint(ctx, "before_execute", 0, ""); err != nil {
		return Outcome{}, err
	}
	handler := c.handlers[task.Command]
	if handler == nil {
		return Outcome{State: "blocked", ErrorClass: "UNSUPPORTED_COMMAND", Message: "执行器尚未注册，任务内容保留"}, nil
	}
	arguments, found, err := c.repo.Data(task.ID, "arguments", 0)
	if err != nil || !found {
		return Outcome{}, errors.New("execution arguments unavailable")
	}
	return handler(ctx, e, arguments)
}
