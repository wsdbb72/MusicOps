//go:build windows || darwin

// MusicOps desktop shell (Windows/macOS): Wails window over the Go core.
package main

import (
	"flag"
	"log"
	"os"

	"github.com/wsdbb72/MusicOps/native/core/internal/desktop"
)

func main() {
	profile := flag.String("profile", "", "profile directory (default: resolved automatically)")
	coreBinary := flag.String("core-binary", "", "path to the musicops core binary (default: auto-detect)")
	minimized := flag.Bool("minimized", false, "start minimized to the tray")
	flag.Parse()

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
