//go:build windows || darwin

// Thin Wails shell for the desktop app: window, single instance, tray menu
// data, update check, and supervisor lifecycle. All behaviour lives in the
// pure-Go desktop package or the Go core; this file only wires them to
// Wails (wails.Run, no CLI, no Node, no frontend build).
//
// The window renders the core's own WebUI through the reverse proxy, so
// the desktop and Docker interfaces are the same pages and the same API
// by construction.
package desktop

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/wsdbb72/MusicOps/native/core/internal/configmigrate"
	"github.com/wsdbb72/MusicOps/native/core/internal/updater"
	"github.com/wsdbb72/MusicOps/native/core/internal/version"
)

// Options configures one desktop run.
type Options struct {
	Profile    string // "" means ResolveProfile
	CoreBinary string // "" means auto-detect
	Minimized  bool   // start minimized to the tray
	Logger     *log.Logger
}

// Run resolves the profile, runs any config migration, starts the core
// supervisor, and runs the Wails window until it closes. The supervisor is
// stopped on shutdown (and again on return; Stop is idempotent).
func Run(opts Options) error {
	logger := opts.Logger
	if logger == nil {
		logger = log.New(os.Stderr, "musicops-desktop: ", log.LstdFlags)
	}
	profile := opts.Profile
	if profile == "" {
		resolved, err := ResolveProfile("")
		if err != nil {
			return fmt.Errorf("cannot resolve profile: %w", err)
		}
		profile = resolved
	}
	// Linux-only no-op on Windows/macOS; kept for the shared flow.
	LinuxDefaultKeyFile()

	if _, err := configmigrate.MaybeMigrate(profile); err != nil {
		return fmt.Errorf("profile migration failed: %w", err)
	}

	sup := New(profile, opts.CoreBinary)
	if err := sup.Start(context.Background()); err != nil {
		return fmt.Errorf("cannot start the core: %w", err)
	}
	handle := sup.Handle()
	if handle == nil {
		_ = sup.Stop()
		return fmt.Errorf("core started but no handle is available")
	}
	proxy := NewProxy(handle.URL, handle.Password)

	var appCtx context.Context
	app := &options.App{
		Title:            "MusicOps",
		Width:            1280,
		Height:           800,
		MinWidth:         900,
		MinHeight:        600,
		BackgroundColour: options.NewRGB(0xf7, 0xf4, 0xef),
		// Assets is nil, so every GET falls through to Handler and every
		// non-GET request goes to Handler too (assetserver.Options docs).
		AssetServer: &assetserver.Options{Handler: proxy},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "musicops-desktop",
			OnSecondInstanceLaunch: func(data options.SecondInstanceData) {
				if appCtx != nil {
					wailsruntime.WindowShow(appCtx)
				}
			},
		},
		OnStartup: func(ctx context.Context) {
			appCtx = ctx
			// Build the tray menu (labels, brand icon, callbacks). Wails
			// v2.17.1 exposes no API to install it — see BuildTrayMenu.
			_ = BuildTrayMenu(TrayActions{
				Show: func() { wailsruntime.WindowShow(ctx) },
				OpenInBrowser: func() {
					wailsruntime.BrowserOpenURL(ctx, handle.URL)
				},
				CheckUpdates: func() { checkForUpdates(ctx, logger) },
				Quit:         func() { wailsruntime.Quit(ctx) },
			})
		},
		OnShutdown: func(ctx context.Context) {
			_ = sup.Stop()
		},
	}
	if opts.Minimized {
		app.WindowStartState = options.Minimised
	}
	if err := wails.Run(app); err != nil {
		_ = sup.Stop()
		return err
	}
	_ = sup.Stop()
	return nil
}

// checkForUpdates runs one manual update check: Check, ask the user, then
// Stage (with progress in the log), Verify, Launch (waiting for this
// process), and quit so the installer can take over.
func checkForUpdates(ctx context.Context, logger *log.Logger) {
	u := &updater.Updater{
		HTTP:      &http.Client{Timeout: 30 * time.Second},
		Directory: filepath.Join(UserDataRoot(), "MusicOps", "updates"),
		Current:   version.Version,
		// System and Machine are left empty: the updater resolves them
		// from runtime ("win32"/"darwin", "amd64"/"arm64").
		TrustedHosts: []string{"api.github.com", "github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com"},
	}
	rel, err := u.Check(ctx)
	if err != nil {
		logger.Printf("update check failed: %v", err)
		_, _ = wailsruntime.MessageDialog(ctx, wailsruntime.MessageDialogOptions{
			Type:    wailsruntime.ErrorDialog,
			Title:   "MusicOps 更新",
			Message: fmt.Sprintf("检查更新失败：%v", err),
			Buttons: []string{"确定"},
		})
		return
	}
	if rel == nil {
		_, _ = wailsruntime.MessageDialog(ctx, wailsruntime.MessageDialogOptions{
			Type:    wailsruntime.InfoDialog,
			Title:   "MusicOps 更新",
			Message: "已是最新版本。",
			Buttons: []string{"确定"},
		})
		return
	}
	answer, err := wailsruntime.MessageDialog(ctx, wailsruntime.MessageDialogOptions{
		Type:          wailsruntime.QuestionDialog,
		Title:         "MusicOps 更新",
		Message:       fmt.Sprintf("发现新版本 %s，是否下载并安装？", rel.Version),
		Buttons:       []string{"下载并安装", "稍后"},
		DefaultButton: "下载并安装",
		CancelButton:  "稍后",
	})
	if err != nil || answer != "下载并安装" {
		return
	}
	staged, err := u.Stage(ctx, rel, func(percent int) {
		logger.Printf("downloading update %s: %d%%", rel.Version, percent)
	})
	if err != nil {
		logger.Printf("update download failed: %v", err)
		_, _ = wailsruntime.MessageDialog(ctx, wailsruntime.MessageDialogOptions{
			Type:    wailsruntime.ErrorDialog,
			Title:   "MusicOps 更新",
			Message: fmt.Sprintf("下载更新失败：%v", err),
			Buttons: []string{"确定"},
		})
		return
	}
	if err := updater.Verify(staged); err != nil {
		logger.Printf("update verification failed: %v", err)
		_, _ = wailsruntime.MessageDialog(ctx, wailsruntime.MessageDialogOptions{
			Type:    wailsruntime.ErrorDialog,
			Title:   "MusicOps 更新",
			Message: fmt.Sprintf("更新校验失败：%v", err),
			Buttons: []string{"确定"},
		})
		return
	}
	if err := u.Launch(staged, os.Getpid(), ""); err != nil {
		logger.Printf("update launch failed: %v", err)
		_, _ = wailsruntime.MessageDialog(ctx, wailsruntime.MessageDialogOptions{
			Type:    wailsruntime.ErrorDialog,
			Title:   "MusicOps 更新",
			Message: fmt.Sprintf("启动安装器失败：%v", err),
			Buttons: []string{"确定"},
		})
		return
	}
	wailsruntime.Quit(ctx)
}
