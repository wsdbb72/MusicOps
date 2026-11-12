package app

import (
	"bytes"
	"strings"
	"testing"
)

// The stylesheet is a design system, not a pile of patches: it has to declare
// its tokens once, theme them for a dark scheme, and keep the focus ring and
// the reduced-motion opt-out. These guards fail when a colour or a breakpoint
// gets hardcoded into a component again.
func TestDesignSystemDeclaresTokensAndSchemes(t *testing.T) {
	for _, token := range []string{
		"--font-ui:", "--t-md:", "--s-4:", "--r-lg:", "--accent:", "--surface:", "--fg-muted:", "--danger:", "--ring:",
	} {
		if !strings.Contains(appCSS, token) {
			t.Fatal("missing token", token)
		}
	}
	for _, rule := range []string{
		"@media (prefers-color-scheme:dark)",
		"prefers-reduced-motion:reduce",
		":focus-visible",
		".skip",
		".app-bar",
		".card-link",
		"max-width:720px",
		"min-width:67.5rem",
	} {
		if !strings.Contains(appCSS, rule) {
			t.Fatal("missing rule", rule)
		}
	}
	// A dark scheme that never overrides the surface tokens is not a scheme.
	dark := appCSS[strings.Index(appCSS, "@media (prefers-color-scheme:dark)"):]
	end := strings.Index(dark, "\n}\n")
	if end < 0 || !strings.Contains(dark[:end], "--bg:") || !strings.Contains(dark[:end], "--fg:") {
		t.Fatal("the dark scheme does not redefine the base surfaces")
	}
	if open, closed := strings.Count(appCSS, "{"), strings.Count(appCSS, "}"); open != closed {
		t.Fatalf("unbalanced blocks: %d open, %d closed", open, closed)
	}
}

// The shell carries the landmarks the stylesheet and the keyboard rely on:
// a skip link to #main, one h1 for the view title, and the app bar holding the
// navigation. The detail actions are real groups, not paragraphs.
func TestShellHasLandmarksAndGroupedActions(t *testing.T) {
	for _, want := range []string{
		`<a class="skip" href="#main">`,
		`<main id="main" tabindex="-1">`,
		`<div class="view-head"><h1>`,
		`<header class="app-bar">`,
		`<nav id="nav" aria-label="功能">`,
		`<div id="detail-actions">`,
		`<div id="torrent-actions" hidden>`,
		`aria-live="polite"`,
		`<meta name="color-scheme" content="light dark">`,
	} {
		if !strings.Contains(page, want) {
			t.Fatal("missing in shell:", want)
		}
	}
	if strings.Count(page, "<h1") != 1 {
		t.Fatal("the view title must stay the only h1")
	}
	// The job card is the only card-wide click target, and it is marked as
	// such: the old rule styled every button inside a card as bare text, which
	// silently flattened the primary actions in the settings cards.
	if !strings.Contains(appJS, `class="card-link" data-job=`) {
		t.Fatal("the job card is not marked as a card link")
	}
	if bytes.Contains([]byte(appCSS), []byte("article button{")) {
		t.Fatal("a blanket rule still flattens buttons inside cards")
	}
}

// The torrent controls were rendered but never shown: a job in downloading had
// no way to pause or remove the torrent from the page. The detail panel now
// reveals them for the stages that own one.
func TestTorrentControlsFollowTheJobStage(t *testing.T) {
	for _, want := range []string{
		`document.getElementById("torrent-actions").hidden = !(job.stage === "downloading" || job.stage === "failed")`,
		`/download/control`,
		`JSON.stringify({action: action})`,
		`torrent-pause`, `torrent-resume`, `torrent-recheck`, `torrent-delete`,
	} {
		if !strings.Contains(appJS, want) {
			t.Fatal("missing control wiring:", want)
		}
	}
	if strings.Contains(appJS, "deleteFiles") {
		t.Fatal("the page must never ask for the files to be deleted")
	}
}
