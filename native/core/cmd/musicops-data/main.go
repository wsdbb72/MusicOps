// MusicOps' bundled, parent-owned local data engine.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
	"github.com/wsdbb72/MusicOps/native/core/internal/rpc"
)

var version = "development"

func main() {
	directory := flag.String("state-dir", "", "dedicated MusicOps native data directory")
	showVersion := flag.Bool("version", false, "print the build version")
	backups := flag.String("backup-dir", "", "dedicated checkpoint directory")
	parentPipe := flag.Bool("parent-pipe", false, "stop when the owning parent's stdin closes")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if *directory == "" {
		fmt.Fprintln(os.Stderr, "--state-dir is required; do not pass an existing library or SQLite directory")
		os.Exit(2)
	}
	token := os.Getenv("MUSICOPS_DATA_TOKEN")
	if len(token) < 32 || *backups == "" {
		fmt.Fprintln(os.Stderr, "private authentication and --backup-dir are required")
		os.Exit(2)
	}
	engine, err := dataengine.Open(*directory)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	err = run(engine, token, *backups, *parentPipe)
	closeErr := engine.Close()
	if err != nil || closeErr != nil {
		fmt.Fprintln(os.Stderr, "data service stopped with an error")
		os.Exit(1)
	}

}

func run(engine *dataengine.Engine, token, backups string, parentPipe bool) error {
	handler, err := rpc.New(engine, token, backups)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if parentPipe {
		go func() { _, _ = io.Copy(io.Discard, os.Stdin); stop() }()
	}
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	// stdout is a private pipe to the owner. Authentication never appears here.
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"ready": true,
		"url": "http://" + listener.Addr().String(), "pid": os.Getpid(),
		"engine": "pebble", "data_schema": dataengine.SchemaVersion, "version": version}); err != nil {
		_ = server.Close()
		return err
	}
	select {
	case err := <-finished:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case <-ctx.Done():
		// Drain accepted writes before closing the database. Do not interrupt a
		// synced write and then claim that it was never committed.
		if err := server.Shutdown(context.Background()); err != nil {
			return err
		}
		err := <-finished
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}
