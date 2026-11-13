package app

// Managed FlareSolverr: when the setting asks for it and an executable is
// configured, this process starts the solver once, waits for it to answer,
// and stops it when the app closes. It never adopts a solver it did not
// start: killing a process that belongs to the user would be a surprise, and
// a solver already listening on the port is indistinguishable from one that
// the user is debugging.
//
// Auto-start is Windows-only in practice: the executable field is validated
// as an absolute .exe path, the same rule the legacy wizard used. On Linux
// and in the container the solver is started by the entrypoint, and the
// settings point at that instance.

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	solverProcessStarting = "starting"
	solverProcessReady    = "ready"
	solverProcessFailed   = "failed"
)

// solverRuntime reports the process this app owns. A nil cmd with an empty
// state means nothing was started.
type solverRuntime struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	state   string
	detail  string
	version string
}

// solverRuntimeState is a copy safe to serialise.
type solverRuntimeState struct {
	Managed bool   `json:"managed"`
	State   string `json:"state"`
	Detail  string `json:"detail,omitempty"`
	Version string `json:"version,omitempty"`
}

func (a *App) solverRuntimeState() solverRuntimeState {
	a.solver.mu.Lock()
	defer a.solver.mu.Unlock()
	return solverRuntimeState{
		Managed: a.solver.cmd != nil,
		State:   a.solver.state,
		Detail:  a.solver.detail,
		Version: a.solver.version,
	}
}

func (a *App) setSolverState(state, detail string) {
	a.solver.mu.Lock()
	a.solver.state = state
	a.solver.detail = detail
	a.solver.mu.Unlock()
}

// solverCommand builds the child process. Tests replace the factory so the
// supervisor can be exercised without a real solver binary.
func (a *App) solverCommand(exe string) *exec.Cmd {
	if a.launchSolver != nil {
		return a.launchSolver(exe)
	}
	return exec.Command(exe)
}

// startManagedSolver starts the configured solver and waits for it to answer.
// It reports whether this call started a process; a solver that already
// answers is left alone.
func (a *App) startManagedSolver(settings solverSettings) (bool, error) {
	exe := strings.TrimSpace(settings.Executable)
	if exe == "" {
		return false, errors.New("没有配置 FlareSolverr 可执行程序")
	}
	if sessions, version, ok := probeSolver(settings.BaseURL, time.Duration(settings.Timeout+5)*time.Second); ok {
		a.setSolverState(solverProcessReady, "已在运行（未由本程序启动）")
		a.solver.mu.Lock()
		a.solver.version = version
		a.solver.mu.Unlock()
		_ = sessions
		return false, nil
	}
	if !settings.AutoStart && !settings.Managed {
		return false, errors.New("未启用自动启动")
	}
	cmd := a.solverCommand(exe)
	if cmd == nil {
		return false, errors.New("FlareSolverr 可执行程序无法创建进程")
	}
	prepareSolverCommand(cmd)
	a.setSolverState(solverProcessStarting, "已启动，等待浏览器服务就绪")
	if err := cmd.Start(); err != nil {
		a.setSolverState(solverProcessFailed, "启动失败："+err.Error())
		return false, err
	}
	a.solver.mu.Lock()
	a.solver.cmd = cmd
	a.solver.mu.Unlock()
	// Reap the child so a stopped solver does not linger as a zombie.
	go func() { _ = cmd.Wait() }()

	deadline := time.Now().Add(time.Duration(settings.Timeout+30) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if _, version, ok := probeSolver(settings.BaseURL, 3*time.Second); ok {
			a.solver.mu.Lock()
			a.solver.state = solverProcessReady
			a.solver.detail = "由本程序启动"
			a.solver.version = version
			a.solver.mu.Unlock()
			return true, nil
		}
	}
	a.setSolverState(solverProcessFailed, "启动后仍未响应")
	return true, errors.New("FlareSolverr 启动后仍未响应")
}

// stopManagedSolver stops only a process this app started.
func (a *App) stopManagedSolver() {
	a.solver.mu.Lock()
	cmd := a.solver.cmd
	a.solver.cmd = nil
	a.solver.state = ""
	a.solver.detail = ""
	a.solver.version = ""
	a.solver.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

// autoStartSolver runs at startup: it is the only place that starts a process
// without a user action, and it does nothing unless the settings ask for it.
func (a *App) autoStartSolver() {
	settings, err := a.loadSolverSettings()
	if err != nil || !settings.Enabled || !settings.AutoStart {
		return
	}
	if settings.Managed {
		// The bundled service is started through the engine supervisor: it
		// picks a free loopback port and hands out the service token every
		// request must carry. The legacy path below only knows a fixed
		// address and an explicit executable, which a bundle does not have.
		a.setSolverState(solverProcessStarting, "正在启动内置 FlareSolverr")
		base, _, startErr := a.ensureEngine(engineSolver)
		if startErr != nil {
			a.setSolverState(solverProcessFailed, "内置 FlareSolverr 启动失败："+startErr.Error())
			return
		}
		a.setSolverState(solverProcessReady, "内置 FlareSolverr 已由本程序启动")
		if _, version, ok := probeSolver(base, 10*time.Second); ok {
			a.solver.mu.Lock()
			a.solver.version = version
			a.solver.mu.Unlock()
		}
		return
	}
	_, _ = a.startManagedSolver(settings)
}

// probeSolver asks a solver for its session list. It is the same check the
// connection test performs, so "ready" means the settings page would also say
// the solver is healthy.
func probeSolver(baseURL string, timeout time.Duration) (int, string, bool) {
	if strings.TrimSpace(baseURL) == "" {
		return 0, "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return solverSessions(ctx, baseURL, "")
}
