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
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
)

var ErrCancelled = errors.New("cancelled at an execution checkpoint")

// secretShape matches the credential query keys an upstream error string can
// quote. The task store never holds the secret itself; only the shape.
var secretShape = regexp.MustCompile(`(?i)(authkey|torrent_pass|cookie|password|usetoken)=[^&\s"']*`)

// panicError keeps a recovered panic together with the stack that produced it.
type panicError struct {
	value string
	stack string
}

func (p *panicError) Error() string { return "执行器崩溃: " + p.value }

// frame is the first line of the panic stack that names a function, which is
// enough to locate the executor without storing a whole trace in health.
func (p *panicError) frame() string {
	lines := strings.Split(p.stack, "\n")
	for i, line := range lines {
		// A Go stack prints the function on one line and "<file>.go:<line>"
		// on the next. The first file line after the panic belongs to the
		// executor that panicked.
		if i == 0 || !strings.Contains(line, ".go:") {
			continue
		}
		name := strings.TrimSpace(lines[i-1])
		if strings.HasPrefix(name, "panic(") || strings.HasPrefix(name, "runtime.") {
			continue
		}
		return clip(strings.TrimSpace(name), 160) + " @ " + clip(strings.TrimSpace(line), 160)
	}
	return ""
}
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
	return Config{Limits: map[string]int{"identify": 4, "publish": 1, "transcode": 1, "aux": 2, "download": 3}, ProgressInterval: 500 * time.Millisecond}
}

// schedulerLanes is the exact set of queue lanes the coordinator runs. The
// download lane is separate from aux on purpose: a multi-hour torrent download
// must not consume one of the two aux slots the identify/acquire stages need
// for short site reads (D6).
var schedulerLanes = []string{"identify", "publish", "transcode", "aux", "download"}

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
	// Cancel commits a cancelled workflow-owned task. The generic Finish
	// refuses workflow tasks (ErrWorkflowOwned), so without this hook a
	// cancel that lands before the executor runs — at the before_execute
	// checkpoint — left the job in "downloading" (or whatever stage) with
	// no live executor and no way out.
	Cancel func(requestID string, token taskrepo.Token, nowNS int64) error
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
	if repo == nil || len(config.Limits) != len(schedulerLanes) || config.Limits["publish"] != 1 || config.ProgressInterval <= 0 {
		return nil, errors.New("invalid bounded scheduler configuration")
	}
	for _, lane := range schedulerLanes {
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
		c.failures[lane] = failureDetail(err)
	}
}

// failureDetail is what the health view keeps for one lane. Storing only the
// error type ("%T") told an operator that something failed but never what,
// which made "执行已退出" unactionable (P1-3). The text is scrubbed and
// bounded, and a panic keeps the first frame of its stack.
func failureDetail(err error) string {
	if err == nil {
		return ""
	}
	var panicked *panicError
	if errors.As(err, &panicked) {
		return clip(detailText(panicked.value)+" "+panicked.frame(), 400)
	}
	return clip(detailText(err.Error()), 400)
}

// detailText removes the credential shapes an executor error may quote and
// collapses whitespace so a message stays one line in the task list.
func detailText(value string) string {
	value = secretShape.ReplaceAllString(value, "$1=redacted")
	value = strings.Join(strings.Fields(value), " ")
	return value
}

func clip(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "…"
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
		reason := "执行已退出，等待自动核对外部操作记录"
		if err != nil && !errors.Is(err, ErrCancelled) {
			// Keep an ordinary executor error with the task: the reason is
			// the only place an operator sees why the run stopped. A panic
			// value stays out of the task message (it can quote a secret);
			// it is recorded in health with its frame instead.
			var panicked *panicError
			if errors.As(err, &panicked) {
				// The panic value stays out of the task message because it
				// can quote a secret; its first stack frame is safe and is
				// what locates the failing executor.
				reason = "执行器崩溃，等待核对外部影响：" + panicked.frame()
			} else {
				reason = "执行器返回错误，等待核对外部影响：" + detailText(err.Error())
			}
			c.failure(claimed.Lane, err)
		}
		if persist := c.interrupt(hooks, token, reason); persist != nil {
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
		if errors.Is(persist, taskrepo.ErrWorkflowOwned) && outcome.State == "cancelled" && hooks.Cancel != nil {
			// The task belongs to a workflow job: the cancellation has to be
			// committed by the workflow layer, which the app hooks in.
			if cancelErr := hooks.Cancel("workflow-cancel-"+nonce(), token, time.Now().UnixNano()); cancelErr != nil && !errors.Is(cancelErr, taskrepo.ErrStale) {
				c.failure(claimed.Lane, cancelErr)
			}
			return
		}
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
		if recovered := recover(); recovered != nil {
			err = &panicError{value: fmt.Sprint(recovered), stack: string(debug.Stack())}
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
