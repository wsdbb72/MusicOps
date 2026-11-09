package desktop

import (
	_ "embed"
	"encoding/base64"

	"github.com/wailsapp/wails/v2/pkg/menu"
)

//go:embed assets/musicops.png
var trayIconPNG []byte

// TrayActions are the four tray menu operations. The shell wires them to
// the Wails runtime (show window, open in browser, check for updates,
// quit); tests supply fakes.
type TrayActions struct {
	Show          func()
	OpenInBrowser func()
	CheckUpdates  func()
	Quit          func()
}

// BuildTrayMenu builds the system tray menu: show window / open in browser
// / check for updates / quit, with the brand icon.
//
// Honest limitation (verified against the wails v2.17.1 module source):
// Wails v2 exposes no public API to install a tray menu from Go —
// options.App has no tray field, pkg/runtime has no tray function, and the
// frontend dispatcher has no tray system call; menu.TrayMenu exists but
// its only consumer, the internal menumanager, has no public caller. So
// this menu is fully built data (labels, icon, callbacks) ready to hand
// to a tray implementation, but v2.17.1 cannot show it. Installing it
// needs a tray library or a Wails version that exposes tray creation; see
// the Phase 4 report.
func BuildTrayMenu(actions TrayActions) *menu.TrayMenu {
	m := menu.NewMenu()
	m.AddText("显示窗口", nil, func(*menu.CallbackData) {
		if actions.Show != nil {
			actions.Show()
		}
	})
	m.AddText("在浏览器中打开", nil, func(*menu.CallbackData) {
		if actions.OpenInBrowser != nil {
			actions.OpenInBrowser()
		}
	})
	m.AddText("检查更新", nil, func(*menu.CallbackData) {
		if actions.CheckUpdates != nil {
			actions.CheckUpdates()
		}
	})
	m.AddSeparator()
	m.AddText("退出", nil, func(*menu.CallbackData) {
		if actions.Quit != nil {
			actions.Quit()
		}
	})
	return &menu.TrayMenu{
		Label:   "MusicOps",
		Image:   base64.StdEncoding.EncodeToString(trayIconPNG),
		Tooltip: "MusicOps",
		Menu:    m,
	}
}
