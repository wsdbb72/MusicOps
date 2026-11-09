// MusicOps application. This process is the Go runtime: it does not start Python.
//
// With no subcommand (or a leading dash, i.e. the serve flags) it runs the
// core server, exactly as before. With a subcommand as the first argument
// it behaves as the MusicOps CLI (see cli.go): version, status, jobs, job,
// control, stop and migrate.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/app"
)

func main() {
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		os.Exit(runCLI(os.Args[1:]))
	}
	serve()
}

func serve() {
	dir := flag.String("state-dir", "", "dedicated MusicOps native data directory")
	backup := flag.String("backup-dir", "", "dedicated backup directory")
	listen := flag.String("listen", "127.0.0.1:0", "local application listen address")
	parentPipe := flag.Bool("parent-pipe", false, "stop when the owning parent's stdin closes")
	flag.Parse()
	password := os.Getenv("MUSICOPS_APP_PASSWORD")
	if *dir == "" || *backup == "" || len(password) < 16 {
		fmt.Fprintln(os.Stderr, "--state-dir, --backup-dir and MUSICOPS_APP_PASSWORD (16+ characters) are required")
		os.Exit(2)
	}
	if err := os.MkdirAll(*backup, 0700); err != nil {
		fmt.Fprintln(os.Stderr, "backup directory unavailable")
		os.Exit(1)
	}
	application, err := app.Open(*dir, *backup, password)
	if err != nil {
		fmt.Fprintln(os.Stderr, "application failed to open the native store")
		os.Exit(1)
	}
	listener, err := net.Listen("tcp4", *listen)
	if err != nil {
		_ = application.Close()
		fmt.Fprintln(os.Stderr, "listen failed")
		os.Exit(1)
	}
	server := &http.Server{Handler: application.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *parentPipe {
		go func() { _, _ = io.Copy(io.Discard, os.Stdin); stop() }()
	}
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	if err = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"ready": true, "url": "http://" + listener.Addr().String(), "pid": os.Getpid(),
		"engine": "pebble", "application": "musicops", "python": false, "data_schema": 2,
	}); err != nil {
		_ = server.Close()
		_ = application.Close()
		os.Exit(1)
	}
	select {
	case err = <-finished:
		if err == http.ErrServerClosed {
			err = nil
		}
	case <-ctx.Done():
		err = server.Shutdown(context.Background())
		serveErr := <-finished
		if err == nil && serveErr != http.ErrServerClosed {
			err = serveErr
		}
	}
	closeErr := application.Close()
	if err != nil || closeErr != nil {
		fmt.Fprintln(os.Stderr, "application stopped with an error")
		os.Exit(1)
	}
}
