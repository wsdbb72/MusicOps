// Package app is the standalone MusicOps process: Pebble, the scheduler, and
// the read-only tracker client. It does not start Python.
package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/configmigrate"
	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/livemigrate"
	"github.com/wsdbb72/MusicOps/native/core/internal/reviewai"
	"github.com/wsdbb72/MusicOps/native/core/internal/scheduler"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/tokenledger"
	"github.com/wsdbb72/MusicOps/native/core/internal/trackersearch"
	"github.com/wsdbb72/MusicOps/native/core/internal/vault"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

// App owns one native directory and its scheduler. Secrets stay in memory.
type App struct {
	Dir         string
	Backup      string
	password    string
	paused      bool
	engine      *dataengine.Engine
	tasks       *taskrepo.Repository
	flows       *workflow.Repository
	runner      *scheduler.Coordinator
	searcher    *trackersearch.Client
	ledger      *tokenledger.Ledger
	spendSource tokenledger.SpendSource
	reviews     *reviewai.Cache
	reviewModel reviewai.Completer
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
	// neteaseBases replaces the NetEase API origin in tests, the same seam as
	// playlistBases. Production leaves it nil.
	neteaseBases map[string]string
	// aiHTTPClient overrides the HTTP client the review model and the AI
	// connection test use. It is nil in production (aiclient.New then
	// uses http.DefaultClient); tests set it to a fake server's client
	// so a TLS test server is trusted. The same seam pattern as
	// searcher.Now and Wait above.
	aiHTTPClient *http.Client
	mu           sync.Mutex
	secrets      map[string]string
	stopSubs      chan struct{}
	stopReconcile chan struct{}
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
	// migrationFailure is the reason a legacy profile was not imported at
	// startup. It is set once, before the scheduler serves, and reported by
	// GET /v1/migration. An empty value means nothing failed.
	migrationFailure string
}

// Open creates the native store and starts the scheduler. Torrent bytes are
// not treated as an imported album.
func Open(dir, backup, password string) (*App, error) {
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
	if _, statErr := os.Stat(legacyDB); statErr == nil {
		// Config migration opens the native store itself, so it has to
		// finish before this process keeps the store open.
		if _, err = configmigrate.MaybeMigrate(profile); err != nil {
			migrationFailure = "旧版配置迁移失败：" + scrub(err.Error())
		}
	}
	engine, err := dataengine.Open(dir)
	if err != nil {
		return nil, err
	}
	app := &App{Dir: dir, Backup: backup, password: password, engine: engine, tasks: taskrepo.New(engine), searcher: &trackersearch.Client{}, now: time.Now, secrets: map[string]string{}}
	app.vault, err = vault.Open(filepath.Dir(filepath.Dir(absDir)))
	if err != nil {
		engine.Close()
		return nil, err
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
	runner, err := scheduler.New(app.tasks, app.handlers(), runnerConfig)
	if err != nil {
		engine.Close()
		return nil, err
	}
	if err = runner.SetWorkflowHooks(scheduler.WorkflowHooks{
		Block: func(requestID string, token taskrepo.Token, nowNS int64) error {
			_, err := app.flows.BlockUnregistered(requestID, token, nowNS)
			return err
		},
		Interrupt: func(requestID string, token taskrepo.Token, nowNS int64, reason string) error {
			_, err := app.flows.Interrupt(requestID, token, nowNS, reason)
			return err
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
		return nil, err
	}
	if err = runner.Start(context.Background()); err != nil {
		engine.Close()
		return nil, err
	}
	app.runner = runner
	app.stopSubs = make(chan struct{})
	go app.runSubscriptions(app.stopSubs)
	// Token reconciliation runs with the app: an unanswered spend must stop
	// blocking its site on evidence, not only on a 30-minute deadline.
	app.stopReconcile = make(chan struct{})
	go app.runTokenReconciler(app.stopReconcile)
	// A managed solver is started once, in the background: a slow browser
	// start must not delay the UI, and the settings page reports the state.
	go app.autoStartSolver()
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
		workflow.CommandIdentify: a.identify,
		workflow.CommandSnapshot: a.identify,
		workflow.CommandSearch:   a.search,
		workflow.CommandProcess:  a.acquire,
		workflow.CommandDownload: a.download,
		workflow.CommandPublish:  a.publish,
		transcodeCommand:         a.transcode,
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
	a.stopSubs = nil
	a.stopReconcile = nil
	a.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if stopReconcile != nil {
		close(stopReconcile)
	}
	a.stopManagedSolver()
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
