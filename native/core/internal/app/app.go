// Package app is the standalone MusicOps process: Pebble, the scheduler, and
// the read-only tracker client. It does not start Python.
package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/audiorelease"
	"github.com/wsdbb72/MusicOps/native/core/internal/configmigrate"
	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/livemigrate"
	"github.com/wsdbb72/MusicOps/native/core/internal/reviewai"
	"github.com/wsdbb72/MusicOps/native/core/internal/roon"
	"github.com/wsdbb72/MusicOps/native/core/internal/scheduler"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/tokenledger"
	"github.com/wsdbb72/MusicOps/native/core/internal/trackersearch"
	"github.com/wsdbb72/MusicOps/native/core/internal/vault"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

// StartupLogger receives sanitized, stage-level progress while the native
// store opens. WAL replay happens synchronously inside Pebble, so the log
// records both sides of that call; if replay stalls, the last line identifies
// the stage without exposing credentials or request bodies.
type StartupLogger func(stage, detail string)

// App owns one native directory and its scheduler. Secrets stay in memory.
type App struct {
	Dir           string
	Backup        string
	password      string
	paused        bool
	engine        *dataengine.Engine
	tasks         *taskrepo.Repository
	flows         *workflow.Repository
	runner        *scheduler.Coordinator
	searcher      *trackersearch.Client
	ledger        *tokenledger.Ledger
	spendSource   tokenledger.SpendSource
	reviews       *reviewai.Cache
	reviewModel   reviewai.Completer
	reviewModelMu sync.RWMutex
	// vault is the profile credential store. Its base is the profile
	// directory: the state directory is <profile>/state/native, so the
	// vault lives two levels up. Keys, cookies and tokens are only ever
	// read through it, never from the settings documents.
	vault *vault.Vault
	now   func() time.Time
	Wait  func(context.Context, time.Duration) error
	// playlistBases replaces a platform origin in tests. Production leaves it nil,
	// so a fetch uses the public host and never a caller-chosen remote server.
	playlistBases map[string]string
	// A large playlist preview and its immediate enqueue reuse one immutable
	// library listing. Per-track file state is still checked on every call.
	playlistEntriesMu     sync.Mutex
	playlistEntriesCache  []audiorelease.Entry
	playlistEntriesCached bool
	playlistEntriesAt     time.Time
	// neteaseBases replaces the NetEase API origin in tests, the same seam as
	// playlistBases. Production leaves it nil.
	neteaseBases map[string]string
	// aiHTTPClient overrides the HTTP client the review model and the AI
	// connection test use. It is nil in production (aiclient.New then
	// uses http.DefaultClient); tests set it to a fake server's client
	// so a TLS test server is trusted. The same seam pattern as
	// searcher.Now and Wait above.
	aiHTTPClient      *http.Client
	mu                sync.Mutex
	duplicateMu       sync.Mutex
	neteaseRecoveryMu sync.Mutex
	secrets           map[string]string
	proxyConfigErr    string
	stopSubs          chan struct{}
	stopReconcile     chan struct{}
	stopAutoReview    chan struct{}
	stopLibraryAuto   chan struct{}
	libraryAutomation *libraryAutomationState
	autoReviewMu      sync.Mutex
	autoReviewRetryAt map[string]time.Time
	// lastTokenRead bounds the background balance reads to one per site per
	// minute. The ledger's read hold only exists for failures; a successful
	// read leaves no trace there, and a 30-second ticker would otherwise read
	// every other sweep.
	reconcileMu   sync.Mutex
	lastTokenRead map[string]time.Time
	// solver is the managed FlareSolverr process. launchSolver builds the
	// child command; tests replace it so the supervisor can run without a
	// real solver binary.
	solver       solverRuntime
	launchSolver func(exe string) *exec.Cmd
	// engines are the bundled services this app started (go-music-dl,
	// FlareSolverr). engineMu guards the map and the start sequence.
	engineMu sync.Mutex
	engines  map[engineName]*engineProcess
	// Roon extension state: one socket, the zone snapshot the page shows, and
	// the token Roon returned so a reconnect does not ask for approval again.
	roonMu     sync.Mutex
	roonClient *roon.Client
	roonState  string
	roonNote   string
	roonCore   roon.Info
	roonZones  []roon.Zone
	roonToken  string
	// roonService is the transport service the subscription used
	// (transport:2 on Roon 2.x, transport:1 on older cores).
	roonService string
	// roonGeneration discards a registration that finished after a newer
	// connect or a disconnect.
	roonGeneration int
	// roonReady closes when the current registration attempt settles.
	roonReady chan struct{}
	// roonStop ends the reconnect loop on Close.
	roonStop chan struct{}
	// migrationFailure is the reason a legacy profile was not imported at
	// startup. It is set once, before the scheduler serves, and reported by
	// GET /v1/migration. An empty value means nothing failed.
	migrationFailure string
}

// Open creates the native store and starts the scheduler. Torrent bytes are
// not treated as an imported album.
func Open(dir, backup, password string) (*App, error) {
	return open(dir, backup, password, nil)
}

// OpenWithLogger is Open with bounded stage diagnostics for the desktop and
// container entrypoint. The callback must not log the password or payloads.
func OpenWithLogger(dir, backup, password string, logger StartupLogger) (*App, error) {
	return open(dir, backup, password, logger)
}

func open(dir, backup, password string, logger StartupLogger) (*App, error) {
	progress := func(stage, detail string) {
		if logger != nil {
			logger(stage, detail)
		}
	}
	progress("arguments", "validating state and backup directories")
	if len(password) < 16 || dir == "" || backup == "" {
		return nil, errors.New("state directory, backup directory and a password of at least 16 characters are required")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	profile := filepath.Dir(filepath.Dir(absDir))
	legacyDB := filepath.Join(profile, "state", "musicops.sqlite")
	// A broken legacy profile is input to the new build, not a reason to
	// refuse to start: the old SQLite file is only ever read, and a failure
	// here used to leave a desktop install unstartable with no way in
	// (P0-4). The failure is reported on GET /v1/migration and the next
	// start retries; nothing is deleted or overwritten.
	var migrationFailure string
	progress("legacy_config", "checking the optional legacy profile")
	if _, statErr := os.Stat(legacyDB); statErr == nil {
		// Config migration opens the native store itself, so it has to
		// finish before this process keeps the store open.
		if _, err = configmigrate.MaybeMigrate(profile); err != nil {
			migrationFailure = "旧版配置迁移失败：" + scrub(err.Error())
			progress("legacy_config", migrationFailure)
		}
	}
	progress("native_store_open", "opening Pebble; WAL replay may be in progress")
	engine, err := dataengine.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("open native Pebble store: %w", err)
	}
	progress("native_store_ready", "Pebble opened and schema checked")
	app := &App{Dir: dir, Backup: backup, password: password, engine: engine, tasks: taskrepo.New(engine), searcher: &trackersearch.Client{}, now: time.Now, secrets: map[string]string{}, autoReviewRetryAt: map[string]time.Time{}, libraryAutomation: &libraryAutomationState{}}
	progress("outbound_proxy", "loading saved proxy before the scheduler starts")
	if proxyErr := app.restoreOutboundProxy(); proxyErr != nil {
		progress("outbound_proxy", "saved proxy could not be applied; local services remain available")
	}
	progress("vault", "opening the encrypted credential vault")
	app.vault, err = vault.Open(filepath.Dir(filepath.Dir(absDir)))
	if err != nil {
		engine.Close()
		return nil, fmt.Errorf("open credential vault: %w", err)
	}
	app.ledger = tokenledger.New(engine)
	// The spend source is a file the operator writes (spend-proofs.jsonl
	// under the state directory). No file means no proofs and no behaviour
	// change; a real online ledger would replace this field with a source
	// that reads its own format.
	app.spendSource = tokenledger.NewFileSpendSource(filepath.Join(dir, "spend-proofs.jsonl"))
	app.reviews = reviewai.NewCache(2)
	// Wire the production review model from the saved AI settings, if any.
	// A missing or disabled configuration leaves reviewModel nil and the
	// search executor keeps its local-only classification.
	app.refreshReviewModel()
	app.flows = workflow.New(engine, app.tasks)
	app.loadPipelinePause()
	if _, statErr := os.Stat(legacyDB); statErr == nil {
		progress("legacy_migration", "importing the legacy profile into the native store")
		if backupErr := app.backupBeforeLiveMigration(); backupErr != nil {
			migrationFailure = "迁移前备份失败，旧库未导入：" + scrub(backupErr.Error())
		} else if _, err = livemigrate.Activate(engine, profile, app.flows, app.ledger, app.now()); err != nil {
			migrationFailure = "旧库导入失败，旧库与音乐文件未改动：" + scrub(err.Error())
		}
	}
	app.migrationFailure = migrationFailure
	runnerConfig := scheduler.DefaultConfig()
	// A recovered panic keeps its first frame in the task row; the whole
	// stack goes to panic.log, redacted the same way.
	runnerConfig.OnPanic = app.recordPanic
	// The global pause must stop every lane before it claims more work. A
	// handler-level wait alone still showed queued jobs as running workers.
	runnerConfig.BeforeClaim = app.waitIfPaused
	progress("scheduler_init", "creating bounded workflow queues")
	runner, err := scheduler.New(app.tasks, app.handlers(), runnerConfig)
	if err != nil {
		engine.Close()
		return nil, fmt.Errorf("create scheduler: %w", err)
	}
	if err = runner.SetWorkflowHooks(scheduler.WorkflowHooks{
		Block: func(requestID string, token taskrepo.Token, nowNS int64) error {
			_, err := app.flows.BlockUnregistered(requestID, token, nowNS)
			return err
		},
		Interrupt: func(requestID string, token taskrepo.Token, nowNS int64, reason string) error {
			if _, err := app.flows.Interrupt(requestID, token, nowNS, reason); err != nil {
				return err
			}
			if task, _ := app.tasks.Get(token.ID); task != nil && task.CancelRequested {
				// The process is gone and the user already requested cancellation.
				// There is no executor left to acknowledge it, so commit the final
				// cancelled edge instead of leaving recovery_required forever.
				_, _ = app.flows.Control("cancel-interrupted-"+token.ID+"-"+token.Owner, task.WorkflowJobID, "cancel", nowNS)
			}
			return nil
		},
		Cancel: func(requestID string, token taskrepo.Token, nowNS int64) error {
			// A cancel that lands before the executor started: commit it as
			// the job's own terminal state instead of leaving the job behind.
			_, err := app.flows.Complete(requestID, token, workflow.Complete{
				Edge: workflow.EdgeTerminal, NextStage: "cancelled", TaskState: "cancelled",
				Message: "已取消；未开始的阶段不会继续", NowNS: nowNS,
			})
			return err
		},
	}); err != nil {
		engine.Close()
		return nil, fmt.Errorf("install workflow hooks: %w", err)
	}
	app.normalizeLegacyRetrySchedules(progress)
	// Recovery must finish before workers can claim. Starting both at once let
	// the scheduler execute an old queued retry while startup recovery was
	// changing the same workflow, producing hundreds of false
	// RECOVERY_REQUIRED reviews. Reconciling superseded queued tasks before
	// Start also prevents them from being claimed at all.
	progress("task_recovery", "recovering orphaned tasks before starting scheduler workers")
	app.recoverOrphanedTasks(runner.Owner(), progress)
	app.reconcileSupersededQueuedTasks(progress)
	progress("scheduler_start", "starting scheduler workers")
	if err = runner.Start(context.Background()); err != nil {
		engine.Close()
		return nil, fmt.Errorf("start scheduler: %w", err)
	}
	app.runner = runner
	go app.reconcilePublishedRoots(time.Now(), progress)
	app.stopSubs = make(chan struct{})
	go app.runSubscriptions(app.stopSubs)
	// Token reconciliation runs with the app: an unanswered spend must stop
	// blocking its site on evidence, not only on a 30-minute deadline.
	app.stopReconcile = make(chan struct{})
	go app.runTokenReconciler(app.stopReconcile)
	// Safe review items are retried by the core itself. Token holds remain
	// excluded by aiReviewEligible and still require an explicit FL decision.
	app.stopAutoReview = make(chan struct{})
	go app.runAutoReview(app.stopAutoReview)
	app.stopLibraryAuto = make(chan struct{})
	go app.runLibraryAutomationScheduler(app.stopLibraryAuto)
	// A managed solver is started once, in the background: a slow browser
	// start must not delay the UI, and the settings page reports the state.
	go app.autoStartSolver()
	// MusicOps is a Roon extension: with a stored token it rejoins on its own,
	// which is also what keeps it visible in Roon's Extensions list.
	app.startRoonSupervisor()
	progress("ready", "native store and scheduler are ready")
	return app, nil
}

// backupBeforeLiveMigration copies the current store into the backup
// directory before a legacy import writes into it. A first successful import
// already leaves both the receipt and this backup, so later starts skip the
// copy: the legacy database is never removed, and copying the whole store on
// every start would be a real cost.
func (a *App) backupBeforeLiveMigration() error {
	if _, found, err := livemigrate.Read(a.engine); err != nil || found {
		return err
	}
	if err := os.MkdirAll(a.Backup, 0o700); err != nil {
		return err
	}
	destination := filepath.Join(a.Backup, "pre-migration-"+a.now().UTC().Format("20060102-150405.000000000"))
	return a.engine.Checkpoint(destination)
}

// handlers are the executors this process runs. publish_album only
// verifies an already-imported local library record: saving a torrent or a
// fixture must not be registered as an audio download.
func (a *App) handlers() map[string]scheduler.Handler {
	return map[string]scheduler.Handler{
		workflow.CommandIdentify: a.whenPipelineRunning(a.identify),
		workflow.CommandSnapshot: a.whenPipelineRunning(a.identify),
		workflow.CommandSearch:   a.whenPipelineRunning(a.search),
		workflow.CommandProcess:  a.whenPipelineRunning(a.acquire),
		workflow.CommandDownload: a.whenPipelineRunning(a.download),
		workflow.CommandPublish:  a.whenPipelineRunning(a.publish),
		transcodeCommand:         a.whenPipelineRunning(a.transcode),
	}
}

// whenPipelineRunning closes the small race between claiming a task and a
// pause arriving just before its executor starts. Existing work is allowed to
// reach its next safe checkpoint; the executor itself starts only while the
// global pipeline is running.
func (a *App) whenPipelineRunning(handler scheduler.Handler) scheduler.Handler {
	return func(ctx context.Context, execution *scheduler.Execution, payload []byte) (scheduler.Outcome, error) {
		if err := a.waitIfPaused(ctx); err != nil {
			return scheduler.Outcome{}, err
		}
		return handler(ctx, execution, payload)
	}
}

// Close stops workers and closes Pebble. It does not delete imported rows.
func (a *App) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	stop := a.stopSubs
	stopReconcile := a.stopReconcile
	stopAutoReview := a.stopAutoReview
	stopLibraryAuto := a.stopLibraryAuto
	a.stopSubs = nil
	a.stopReconcile = nil
	a.stopAutoReview = nil
	a.stopLibraryAuto = nil
	a.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if stopReconcile != nil {
		close(stopReconcile)
	}
	if stopAutoReview != nil {
		close(stopAutoReview)
	}
	if stopLibraryAuto != nil {
		close(stopLibraryAuto)
	}
	if a.libraryAutomation != nil {
		a.libraryAutomation.stop()
		waitCtx, cancelWait := context.WithTimeout(context.Background(), 15*time.Second)
		a.libraryAutomation.wait(waitCtx)
		cancelWait()
	}
	a.stopManagedSolver()
	a.stopEngines()
	a.closeRoon()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := a.runner.Stop(ctx)
	closeErr := a.engine.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (a *App) remember(jobID, cookie string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cookie == "" {
		delete(a.secrets, jobID)
		return
	}
	a.secrets[jobID] = cookie
}

func (a *App) secret(jobID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.secrets[jobID]
}

func (a *App) rememberDownload(jobID, site, value string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := jobID + "/" + site
	if value == "" {
		delete(a.secrets, key)
		return
	}
	a.secrets[key] = value
}

func (a *App) forget(jobID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.secrets, jobID)
	delete(a.secrets, jobID+"/dicmusic")
	delete(a.secrets, jobID+"/orpheus")
}
