package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strings"

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
// 7 for a CLI-side error.
func runCLI(args []string) int {
	cmd := args[0]
	switch cmd {
	case "version":
		printJSON(map[string]string{"version": version.Version})
		return 0
	case "status", "jobs", "job", "control", "stop", "migrate":
		// Parsed below.
	default:
		return printError(fmt.Errorf("未知命令: %s", cmd))
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
