package desktop

import (
	"encoding/base64"
	"testing"

	"github.com/wailsapp/wails/v2/pkg/menu"
)

func TestBuildTrayMenu(t *testing.T) {
	var calls []string
	actions := TrayActions{
		Show:          func() { calls = append(calls, "show") },
		OpenInBrowser: func() { calls = append(calls, "browser") },
		CheckUpdates:  func() { calls = append(calls, "updates") },
		Quit:          func() { calls = append(calls, "quit") },
	}
	tray := BuildTrayMenu(actions)
	if tray.Label != "MusicOps" {
		t.Fatalf("label = %q", tray.Label)
	}
	if tray.Menu == nil {
		t.Fatal("menu is nil")
	}
	// Four actions plus one separator.
	if len(tray.Menu.Items) != 5 {
		t.Fatalf("menu has %d items, want 5", len(tray.Menu.Items))
	}
	wantLabels := []string{"显示窗口", "在浏览器中打开", "检查更新", "", "退出"}
	for i, want := range wantLabels {
		if tray.Menu.Items[i].Label != want {
			t.Fatalf("item %d label = %q, want %q", i, tray.Menu.Items[i].Label, want)
		}
	}
	if tray.Menu.Items[3].Type != menu.SeparatorType {
		t.Fatalf("item 3 type = %v, want a separator", tray.Menu.Items[3].Type)
	}
	// The icon is the embedded brand PNG.
	raw, err := base64.StdEncoding.DecodeString(tray.Image)
	if err != nil {
		t.Fatalf("icon is not base64: %v", err)
	}
	if len(raw) < 8 || string(raw[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatal("icon is not a PNG")
	}
	// Every action callback fires.
	for _, item := range tray.Menu.Items {
		if item.Click != nil {
			item.Click(&menu.CallbackData{MenuItem: item})
		}
	}
	if len(calls) != 4 {
		t.Fatalf("callbacks fired %v, want 4 calls", calls)
	}
	for i, want := range []string{"show", "browser", "updates", "quit"} {
		if calls[i] != want {
			t.Fatalf("call %d = %q, want %q", i, calls[i], want)
		}
	}
}

func TestBuildTrayMenuNilActionsDoNotPanic(t *testing.T) {
	tray := BuildTrayMenu(TrayActions{})
	for _, item := range tray.Menu.Items {
		if item.Click != nil {
			item.Click(&menu.CallbackData{MenuItem: item})
		}
	}
}
