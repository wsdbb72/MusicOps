//go:build windows || darwin

// MusicOps desktop shell (Windows/macOS): Wails window over the Go core.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/wsdbb72/MusicOps/native/core/internal/desktop"
	"github.com/wsdbb72/MusicOps/native/core/internal/version"
)

func main() {
	profile := flag.String("profile", "", "profile directory (default: resolved automatically)")
	coreBinary := flag.String("core-binary", "", "path to the musicops core binary (default: auto-detect)")
	minimized := flag.Bool("minimized", false, "start minimized to the tray")
	versionFlag := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	// --version prints before any window is created, so the build
	// scripts and the releasegate can verify the packaged shell's
	// version the same way they verify the core's.
	if *versionFlag {
		raw, _ := json.Marshal(map[string]string{"version": version.Version})
		fmt.Println(string(raw))
		return
	}

	logger := log.New(os.Stderr, "musicops-desktop: ", log.LstdFlags)
	if err := desktop.Run(desktop.Options{
		Profile:    *profile,
		CoreBinary: *coreBinary,
		Minimized:  *minimized,
		Logger:     logger,
	}); err != nil {
		logger.Printf("fatal: %v", err)
		os.Exit(1)
	}
}
