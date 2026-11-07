// Package app is the standalone MusicOps process: Pebble, the scheduler, and
// the read-only tracker client. It does not start Python.
package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/reviewai"
	"github.com/wsdbb72/MusicOps/native/core/internal/scheduler"
	"github.com/wsdbb72/MusicOps/native/core/internal/taskrepo"
	"github.com/wsdbb72/MusicOps/native/core/internal/tokenledger"
	"github.com/wsdbb72/MusicOps/native/core/internal/trackersearch"
	"github.com/wsdbb72/MusicOps/native/core/internal/workflow"
)

// App owns one native directory and its scheduler. Secrets stay in memory.
type App struct {
	Dir         string
	Backup      string
	password    string
	engine      *dataengine.Engine
	tasks       *taskrepo.Repository
	flows       *workflow.Repository
	runner      *scheduler.Coordinator
	searcher    *trackersearch.Client
	ledger      *tokenledger.Ledger
	reviews     *reviewai.Cache
	reviewModel reviewai.Completer
	now         func() time.Time
	Wait        func(context.Context, time.Duration) error
	mu          sync.Mutex
	secrets     map[string]string
}

// Open creates the native store and starts the scheduler. Library publication
// stays unregistered. Torrent bytes are not treated as an imported album.
func Open(dir, backup, password string) (*App, error) {
	if len(password) < 16 || dir == "" || backup == "" {
		return nil, errors.New("state directory, backup directory and a password of at least 16 characters are required")
	}
	engine, err := dataengine.Open(dir)
	if err != nil {
		return nil, err
	}
	app := &App{Dir: dir, Backup: backup, password: password, engine: engine, tasks: taskrepo.New(engine), searcher: &trackersearch.Client{}, now: time.Now, secrets: map[string]string{}}
	app.ledger = tokenledger.New(engine)
	app.reviews = reviewai.NewCache(2)
	app.flows = workflow.New(engine, app.tasks)
	runner, err := scheduler.New(app.tasks, app.handlers(), scheduler.DefaultConfig())
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
	}); err != nil {
		engine.Close()
		return nil, err
	}
	if err = runner.Start(context.Background()); err != nil {
		engine.Close()
		return nil, err
	}
	app.runner = runner
	return app, nil
}

// handlers are the executors this process runs. publish_album stays absent:
// saving a torrent or a fixture must not be registered as an audio download.
func (a *App) handlers() map[string]scheduler.Handler {
	return map[string]scheduler.Handler{
		workflow.CommandIdentify: a.identify,
		workflow.CommandSnapshot: a.identify,
		workflow.CommandSearch:   a.search,
		workflow.CommandProcess:  a.acquire,
	}
}

// Close stops workers and closes Pebble. It does not delete imported rows.
func (a *App) Close() error {
	if a == nil {
		return nil
	}
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
