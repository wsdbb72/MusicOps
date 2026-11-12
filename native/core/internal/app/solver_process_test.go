package app

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// solverStub is a FlareSolverr stand-in: it answers the one call the probe
// makes and nothing else.
func solverStub(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "sessions": []any{}, "version": "3.4.0"})
	}))
	t.Cleanup(server.Close)
	return server
}

// A solver the user already started is reused, never adopted: this app must
// not kill a process it did not spawn.
func TestManagedSolverReusesARunningInstance(t *testing.T) {
	application := newApp(t)
	server := solverStub(t)
	application.launchSolver = func(string) *exec.Cmd {
		t.Error("a solver that already answers was started again")
		return nil
	}
	started, err := application.startManagedSolver(solverSettings{
		Managed: true, Enabled: true, AutoStart: true, BaseURL: server.URL, Timeout: 10, Executable: "C:/fake/flaresolverr.exe",
	})
	if err != nil || started {
		t.Fatal(started, err)
	}
	state := application.solverRuntimeState()
	if state.State != solverProcessReady || state.Managed || !strings.Contains(state.Detail, "未由本程序启动") {
		t.Fatal(state)
	}
	// Stopping must leave the user's process alone.
	application.stopManagedSolver()
	if sessions, _, ok := probeSolver(server.URL, 2*time.Second); !ok || sessions != 0 {
		t.Fatal("stopping the app took down a solver it did not start")
	}
}

// Without an executable there is nothing to manage, and the error says so.
func TestManagedSolverNeedsAnExecutable(t *testing.T) {
	application := newApp(t)
	if _, err := application.startManagedSolver(solverSettings{Enabled: true, AutoStart: true, BaseURL: "http://127.0.0.1:1", Timeout: 10}); err == nil {
		t.Fatal("a managed solver started without a program")
	}
}

// The managed path starts the process, waits for it to answer, and stops it on
// close. The child is this test binary in helper mode, so no solver install is
// needed to prove the lifecycle.
func TestManagedSolverStartsAndStopsTheProcess(t *testing.T) {
	address := freeLoopbackAddress(t)
	application := newApp(t)
	application.launchSolver = func(string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=TestSolverHelperProcess", "-test.v=false")
		cmd.Env = append(os.Environ(), "MUSICOPS_SOLVER_HELPER_ADDR="+address)
		return cmd
	}
	started, err := application.startManagedSolver(solverSettings{
		Managed: true, Enabled: true, AutoStart: true, BaseURL: "http://" + address, Timeout: 10, Executable: "C:/fake/flaresolverr.exe",
	})
	if err != nil || !started {
		t.Fatal(started, err)
	}
	state := application.solverRuntimeState()
	if state.State != solverProcessReady || !state.Managed || state.Version != "helper" {
		t.Fatal(state)
	}
	application.stopManagedSolver()
	// The port must stop answering once the app closes.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := probeSolver("http://"+address, 300*time.Millisecond); !ok {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the managed solver was still answering after stop")
}

// The settings response carries the runtime state, so the page can say whether
// the managed process is up instead of guessing.
func TestSolverSettingsReportTheRuntime(t *testing.T) {
	application := newApp(t)
	application.setSolverState(solverProcessFailed, "启动后仍未响应")
	recorder := getAuth(t, application, "/v1/settings/solver")
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code)
	}
	var decoded struct {
		Runtime solverRuntimeState `json:"runtime"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Runtime.State != solverProcessFailed || !strings.Contains(decoded.Runtime.Detail, "未响应") {
		t.Fatal(decoded.Runtime)
	}
}

// TestSolverHelperProcess is not a test: it is the child the supervisor test
// starts. Without the address it is skipped, so a normal run does nothing.
func TestSolverHelperProcess(t *testing.T) {
	address := os.Getenv("MUSICOPS_SOLVER_HELPER_ADDR")
	if address == "" {
		t.Skip("helper process only")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "sessions": []any{}, "version": "helper"})
	})
	_ = http.ListenAndServe(address, mux)
}

// freeLoopbackAddress reserves a port and releases it, so the child can bind
// the same one. A test that races another process for a port is flaky by
// construction; the window here is a few milliseconds.
func freeLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(strings.Split(address, ":")[1])
	if err != nil || port <= 0 {
		t.Fatal(address)
	}
	return address
}
