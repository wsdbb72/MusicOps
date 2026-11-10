package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/configmigrate"
	"github.com/wsdbb72/MusicOps/native/core/internal/version"
)

const waitTimeoutMessage = "CLI_WAIT_TIMEOUT；任务仍在后台保留"

// printJSON writes v as indented JSON to stdout, matching the Python CLI's
// success output.
func printJSON(v any) {
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Println(string(encoded))
}

// printError writes {"error": ...} to stdout and returns exit code 7,
// matching the Python CLI's own-error path.
func printError(err error) int {
	encoded, _ := json.Marshal(map[string]string{"error": err.Error()})
	fmt.Println(string(encoded))
	return 7
}

// printRawJSON re-emits a core response as indented JSON.
func printRawJSON(raw json.RawMessage) {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		fmt.Println(string(raw))
		return
	}
	printJSON(decoded)
}

type cliFlags struct {
	base        string
	payload     map[string]any
	wait        bool
	waitTimeout float64
	coreBinary  string
	args        []string
}

func parseCLIFlags(cmd string, rest []string) (*cliFlags, error) {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	base := fs.String("base", "", "profile 目录")
	payloadRaw := fs.String("payload", "{}", "JSON 对象")
	wait := fs.Bool("wait", false, "等待任务到达终态")
	waitTimeout := fs.Float64("wait-timeout", 600, "等待任务的秒数；0 表示不限时")
	coreBinary := fs.String("core-binary", "", "核心程序路径")
	// The flag package stops at the first positional argument, but the
	// Python CLI accepts `job <id> --wait`. Split flags from positionals
	// first, keeping each value flag's argument with it.
	valueFlags := map[string]bool{"base": true, "payload": true, "wait-timeout": true, "core-binary": true}
	var flagArgs, positional []string
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flagArgs = append(flagArgs, a)
		name := strings.TrimLeft(a, "-")
		if eq := strings.Index(name, "="); eq >= 0 {
			continue
		}
		if valueFlags[name] && i+1 < len(rest) {
			i++
			flagArgs = append(flagArgs, rest[i])
		}
	}
	if err := fs.Parse(flagArgs); err != nil {
		return nil, err
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(*payloadRaw), &payload); err != nil {
		return nil, fmt.Errorf("CLI_PAYLOAD_MUST_BE_OBJECT")
	}
	if payload == nil {
		return nil, fmt.Errorf("CLI_PAYLOAD_MUST_BE_OBJECT")
	}
	if math.IsNaN(*waitTimeout) || math.IsInf(*waitTimeout, 0) || *waitTimeout < 0 {
		return nil, fmt.Errorf("CLI_INVALID_WAIT_TIMEOUT")
	}
	return &cliFlags{
		base: *base, payload: payload, wait: *wait,
		waitTimeout: *waitTimeout, coreBinary: *coreBinary, args: positional,
	}, nil
}

// runCLI implements the subcommands. Exit codes follow the Python CLI:
// 0 normal, 2 for a terminally failed job state, 3 for a --wait timeout,
// 7 for a CLI-side error. healthcheck is the exception: it reports the
// core's liveness for Docker HEALTHCHECK, so 0 means healthy and 1 means
// not healthy (Docker reserves other codes).
func runCLI(args []string) int {
	cmd := args[0]
	switch cmd {
	case "version":
		printJSON(map[string]string{"version": version.Version})
		return 0
	case "healthcheck":
		return runHealthcheck(args[1:])
	case "status", "jobs", "job", "control", "stop", "migrate", "pause", "resume", "reindex", "gaps", "migration", "fetch", "metadata", "workflow":
		// Parsed below.
	default:
		if unwiredWorker[cmd] {
			return printError(fmt.Errorf("%s 尚未接入，没有执行", cmd))
		}
		if !workerCommand(cmd) {
			return printError(fmt.Errorf("未知命令: %s", cmd))
		}
	}
	flags, err := parseCLIFlags(cmd, args[1:])
	if err != nil {
		return printError(err)
	}
	profile, err := resolveProfile(flags.base)
	if err != nil {
		return printError(err)
	}
	ctx := context.Background()
	if code, handled := runMappedWorker(ctx, cmd, profile, flags); handled {
		return code
	}
	switch cmd {
	case "migrate":
		if len(flags.args) != 0 {
			return printError(errors.New("migrate 不接受位置参数"))
		}
		report, err := configmigrate.MaybeMigrate(profile)
		if err != nil {
			return printError(err)
		}
		printJSON(report)
		return 0
	case "stop":
		if len(flags.args) != 0 {
			return printError(errors.New("stop 不接受位置参数"))
		}
		if err := stopCore(profile); err != nil {
			return printError(err)
		}
		printJSON(map[string]bool{"ok": true})
		return 0
	case "status", "jobs":
		if len(flags.args) != 0 {
			return printError(fmt.Errorf("%s 不接受位置参数", cmd))
		}
		client, err := attachCore(profile, flags.coreBinary)
		if err != nil {
			return printError(err)
		}
		jobs, err := client.getJobs(ctx)
		if err != nil {
			return printError(err)
		}
		printJSON(map[string]any{"ok": true, "jobs": jobs})
		return 0
	case "job":
		if len(flags.args) != 1 {
			return printError(errors.New("job 需要一个任务 ID"))
		}
		id := flags.args[0]
		client, err := attachCore(profile, flags.coreBinary)
		if err != nil {
			return printError(err)
		}
		return runJob(ctx, client, id, flags)
	case "pause", "resume":
		if len(flags.args) != 0 {
			return printError(fmt.Errorf("%s 不接受位置参数", cmd))
		}
		client, err := attachCore(profile, flags.coreBinary)
		if err != nil {
			return printError(err)
		}
		raw, err := client.do(ctx, http.MethodPost, "/v1/control", map[string]any{"action": cmd})
		if err != nil {
			return printError(err)
		}
		printRawJSON(raw)
		return 0
	case "reindex":
		client, err := attachCore(profile, flags.coreBinary)
		if err != nil {
			return printError(err)
		}
		raw, err := client.do(ctx, http.MethodPost, "/v1/library/reindex", map[string]any{})
		if err != nil {
			return printError(err)
		}
		printRawJSON(raw)
		return 0
	case "gaps":
		client, err := attachCore(profile, flags.coreBinary)
		if err != nil {
			return printError(err)
		}
		raw, err := client.do(ctx, http.MethodGet, "/v1/library/gaps", nil)
		if err != nil {
			return printError(err)
		}
		printRawJSON(raw)
		return 0
	case "migration":
		client, err := attachCore(profile, flags.coreBinary)
		if err != nil {
			return printError(err)
		}
		raw, err := client.do(ctx, http.MethodGet, "/v1/migration", nil)
		if err != nil {
			return printError(err)
		}
		printRawJSON(raw)
		return 0
	case "fetch":
		client, err := attachCore(profile, flags.coreBinary)
		if err != nil {
			return printError(err)
		}
		raw, err := client.do(ctx, http.MethodPost, "/v1/playlists/fetch", flags.payload)
		if err != nil {
			return printError(err)
		}
		printRawJSON(raw)
		return 0
	case "metadata":
		client, err := attachCore(profile, flags.coreBinary)
		if err != nil {
			return printError(err)
		}
		raw, err := client.do(ctx, http.MethodPost, "/v1/library/metadata", flags.payload)
		if err != nil {
			return printError(err)
		}
		printRawJSON(raw)
		return 0
	case "workflow":
		client, err := attachCore(profile, flags.coreBinary)
		if err != nil {
			return printError(err)
		}
		raw, err := client.do(ctx, http.MethodPost, "/v1/library/workflow", flags.payload)
		if err != nil {
			return printError(err)
		}
		printRawJSON(raw)
		return 0
	case "control":
		if len(flags.args) != 2 {
			return printError(errors.New("control 需要任务 ID 与动作 pause|resume|cancel|retry"))
		}
		id, action := flags.args[0], flags.args[1]
		switch action {
		case "pause", "resume", "cancel", "retry":
		default:
			return printError(fmt.Errorf("未知动作: %s", action))
		}
		client, err := attachCore(profile, flags.coreBinary)
		if err != nil {
			return printError(err)
		}
		return runControl(ctx, client, id, action, flags)
	default:
		return printError(fmt.Errorf("未知命令: %s", cmd))
	}
}

// runHealthcheck probes the core's unauthenticated /healthz endpoint and
// reports healthy (exit 0) or not (exit 1). It takes no profile: Docker's
// HEALTHCHECK runs it against the listening address only. Flags: --addr
// (default 127.0.0.1:8080), --timeout seconds (default 5). A healthy core
// answers 200 with ok:true, application:musicops and python:false.
func runHealthcheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", "127.0.0.1:8080", "core listen address")
	timeout := fs.Float64("timeout", 5, "probe timeout in seconds")
	if err := fs.Parse(args); err != nil {
		return printError(err)
	}
	if math.IsNaN(*timeout) || math.IsInf(*timeout, 0) || *timeout < 0 {
		return printError(fmt.Errorf("CLI_INVALID_TIMEOUT"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeout*float64(time.Second)))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+*addr+"/healthz", nil)
	if err != nil {
		printJSON(map[string]any{"ok": false, "error": err.Error()})
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		printJSON(map[string]any{"ok": false, "error": err.Error()})
		return 1
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		printJSON(map[string]any{"ok": false, "status": resp.StatusCode, "error": "invalid health response"})
		return 1
	}
	healthy := resp.StatusCode == http.StatusOK && body["ok"] == true &&
		body["application"] == "musicops" && body["python"] == false
	printJSON(map[string]any{"ok": healthy, "status": resp.StatusCode, "body": body})
	if healthy {
		return 0
	}
	return 1
}

// runJob fetches one job, optionally waiting for it to settle, then prints
// the final response. The exit code follows the job's terminal state.
func runJob(ctx context.Context, client *coreClient, id string, flags *cliFlags) int {
	raw, err := client.getJob(ctx, id)
	if err != nil {
		return printError(err)
	}
	if flags.wait && activeState(extractState(raw)) {
		return waitAndPrint(ctx, client, id, flags, raw)
	}
	printRawJSON(raw)
	return exitCodeForState(extractState(raw))
}

// runControl posts a control action, optionally waiting for the job to
// settle, then prints the final response.
func runControl(ctx context.Context, client *coreClient, id, action string, flags *cliFlags) int {
	raw, err := client.control(ctx, id, action)
	if err != nil {
		return printError(err)
	}
	if flags.wait {
		return waitAndPrint(ctx, client, id, flags, raw)
	}
	printRawJSON(raw)
	return exitCodeForState(extractState(raw))
}

// waitAndPrint polls until the job settles. On timeout it prints the
// timeout object and returns 3; the job stays running in the background.
func waitAndPrint(ctx context.Context, client *coreClient, id string, flags *cliFlags, first json.RawMessage) int {
	raw, state, timedOut, err := client.waitSettled(ctx, id, flags.waitTimeout)
	if err != nil {
		return printError(err)
	}
	if timedOut {
		printJSON(map[string]string{"task_id": id, "state": state, "error": waitTimeoutMessage})
		return 3
	}
	if raw == nil {
		raw = first
	}
	printRawJSON(raw)
	return exitCodeForState(extractState(raw))
}
