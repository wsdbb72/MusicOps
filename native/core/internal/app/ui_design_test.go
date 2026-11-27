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
	} {
		if !strings.Contains(appCSS, rule) {
			t.Fatal("missing rule", rule)
		}
	}
	if strings.Contains(appCSS, "grid-template-columns:15.5rem minmax(0,1fr)") {
		t.Fatal("wide screens must keep the top navigation")
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
		`<div class="view-head">`, `id="view-subtitle"`, `id="view-live"`, `class="app-bar-inner"`,
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

// Library maintenance supports page selection and an explicit whole-library
// mode. Both actions must call real backend jobs rather than pretending to
// select tens of thousands of paths in the browser.
func TestLibrarySelectionSupportsPageAndWholeLibrary(t *testing.T) {
	for _, want := range []string{
		`id="library-select-page"`, `id="library-select-all"`, `id="library-clear-selection"`,
		`function setLibrarySelectionAll(selected)`, `librarySelectAll = false`,
		`/v1/library/upgrade/whole`, `/v1/library/expand/whole`, `/v1/library/scrape/whole`,
		`id="run-library-upgrade-now"`, `id="run-library-expand-now"`, `id="auto-maintenance-status"`,
		`running ? 3000 : 10000`,
	} {
		if !strings.Contains(appJS, want) {
			t.Fatal("missing library selection behavior:", want)
		}
	}
	if !strings.Contains(appCSS, ".selection-toolbar") || !strings.Contains(appCSS, ".maintenance-status.running") {
		t.Fatal("library selection and live run state need visible styles")
	}
}

// The torrent controls were rendered but never shown: a job in downloading had
// no way to pause or remove the torrent from the page. The detail panel now
// reveals them for the stages that own one.
func TestTorrentControlsFollowTheJobStage(t *testing.T) {
	for _, want := range []string{
		`document.getElementById("torrent-actions").hidden = !(state.stage === "downloading" || state.stage === "failed")`,
		`/download/control`,
		`var downloading = currentActionStage === "downloading"`,
		`encodeURIComponent(currentActionJobID) + "/review"`,
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

// Token reconciliation is a successful hold, not an AI submission failure.
// The batch and single-item views must distinguish automatic progress from
// balance reconciliation and streaming fallback.
func TestTokenReviewUIReportsHeldAndAutomaticOutcomes(t *testing.T) {
	for _, want := range []string{
		`result.held || result.stage === "needs_review"`,
		`等待状态对账`,
		`已自动切换流媒体回退，未重复使用令牌。`,
		`没有可安全执行的后续路径，已自动结束。`,
	} {
		if !strings.Contains(appJS, want) {
			t.Fatal("missing token review feedback:", want)
		}
	}
}

func TestTaskQueueExposesBatchSelectionAndRetryControls(t *testing.T) {
	for _, want := range []string{
		`html += batchBar();`,
		`data-batch-select="all">全选已显示`,
		`data-batch-select="retryable">选择可处理项`,
		`data-batch-select="none" class="quiet">清空`,
		`data-batch-action="resume">批量继续`,
		`data-batch-action="retry">批量重试`,
		`function setBatchSelection(mode)`,
		`job.stage === "needs_review" || job.stage === "failed"`,
		`class="task-select"><input type="checkbox" data-batch=`,
		`id="review-ai-all"`,
		`data-task-filter="`,
		`function taskFilterRows(rows)`,
		`function pipelineStages()`,
		`if (activeCount <= 0) return;`,
	} {
		if !strings.Contains(appJS, want) {
			t.Fatal("missing queue batch behavior:", want)
		}
	}
	if !strings.Contains(appCSS, `.batch button[data-batch-action=resume]`) || !strings.Contains(appCSS, `.batch button[data-batch-select]`) {
		t.Fatal("batch controls need visible action styles")
	}
	if !strings.Contains(appCSS, `.task-item.is-active`) || !strings.Contains(appCSS, `.pipeline-track`) {
		t.Fatal("the live task list and pipeline need visible states")
	}
}

func TestTaskComposerStaysCollapsedAndAIReviewSettlesNoMatch(t *testing.T) {
	for _, want := range []string{
		`id="task-composer"`,
		`(composerOpen ? ' open' : '')`,
		`result.code === "AI_NO_CONFIRMED_MATCH"`,
		`body:JSON.stringify({action:"skip"})`,
		`AI 没有确认安全候选，已自动结束本轮`,
		`已载入 `,
		`data-task-filter=`,
	} {
		if !strings.Contains(appJS, want) {
			t.Fatal("missing automatic review or task composer behavior:", want)
		}
	}
}

func TestWholeLibraryScrapeNamesEveryMetadataScope(t *testing.T) {
	for _, want := range []string{
		`id="run-library-scrape-now">一键全库刮削（标签·歌手·专辑·封面·歌词）`,
		`id="scrape-whole-library">全库补齐标签·封面·歌词`, `id="apply-artist-folders" class="primary">一键补全歌手目录`,
		`/v1/library/scrape/whole`,
	} {
		if !strings.Contains(appJS, want) {
			t.Fatal("missing whole-library scrape scope:", want)
		}
	}
}
