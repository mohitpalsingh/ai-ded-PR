package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mohitpalsingh/ai-ded-pr/internal/config"
	"github.com/mohitpalsingh/ai-ded-pr/internal/core/models"
)

// bigPatch builds a one-file patch with adds removed lines and adds added
// lines. The final added line carries a marker so tests can prove the tail of
// the diff is actually reachable.
func bigPatch(adds int) string {
	var b strings.Builder
	b.WriteString("diff --git a/internal/service/pipeline.go b/internal/service/pipeline.go\n")
	b.WriteString("index 1111111..2222222 100644\n")
	b.WriteString("--- a/internal/service/pipeline.go\n")
	b.WriteString("+++ b/internal/service/pipeline.go\n")
	fmt.Fprintf(&b, "@@ -1,%d +1,%d @@\n", adds+2, adds+2)
	b.WriteString(" package service\n\n")
	for i := 0; i < adds; i++ {
		fmt.Fprintf(&b, "-func handler%d(x int) int { return x + %d }\n", i, i)
	}
	for i := 0; i < adds; i++ {
		marker := ""
		if i == adds-1 {
			marker = " // EOF-MARKER"
		}
		fmt.Fprintf(&b, "+func handler%d(x int) int { return x*2 + %d }%s\n", i, i, marker)
	}
	return b.String()
}

const largePatchPath = "internal/service/pipeline.go"

// newLargeModel builds the review screen for a PR with `files` files where the
// first one alone has a 2*adds-line diff.
func newLargeModel(files, adds, width, height int) *tuiModel {
	m := New(config.Default(), true)
	m.screen = screenReview
	m.pr = &models.PullRequest{Repo: "large-pr-repo", Title: "Large PR", Files: []models.ChangedFile{{
		Path:      largePatchPath,
		Status:    "modified",
		Additions: adds,
		Deletions: adds,
		Patch:     bigPatch(adds),
	}}}
	m.plan = &models.ReviewPlan{Steps: []models.ReviewStep{{
		Path: largePatchPath, Order: 1, Status: models.StatusPending,
		Rationale: "- Intent: rewrites the pipeline\n- Dependencies: none\n- Verify: every handler\n- Risk: high",
	}}}
	for i := 1; i < files; i++ {
		path := fmt.Sprintf("pkg/util/util%d.go", i)
		m.pr.Files = append(m.pr.Files, models.ChangedFile{Path: path, Additions: 1, Deletions: 1,
			Patch: fmt.Sprintf("diff --git a/%[1]s b/%[1]s\n--- a/%[1]s\n+++ b/%[1]s\n@@ -1 +1 @@\n-func U%d() int { return 1 }\n+func U%d() int { return 2 }\n", path, i, i)})
		m.plan.Steps = append(m.plan.Steps, models.ReviewStep{
			Path: path, Order: i + 1, Status: models.StatusPending,
			Rationale: "- Intent: bumps a helper\n- Dependencies: none\n- Verify: callers\n- Risk: low",
		})
	}
	m.current = 0
	m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return m
}

// The reported failure: a 1k+ LOC PR with many files pushed the diff pane off
// screen, so the current file's changes could not be seen or scrolled at all.
func TestLargePRKeepsDiffPaneOnScreen(t *testing.T) {
	m := newLargeModel(121, 1200, 120, 40)
	view := m.View()

	if got := len(strings.Split(view, "\n")); got > 40 {
		t.Fatalf("view is %d lines for a 40-row terminal (panes overflow):\n%s", got, view)
	}
	for _, want := range []string{
		"--- diff ---",
		"@@ -1,1202 +1,1202 @@",
		"-func handler0(x int) int { return x + 0 }",
		"↓ ", // the sidebar tells the reader the rest of the list is below
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("view lost %q:\n%s", want, view)
		}
	}
}

// The sidebar must follow the current file instead of always rendering the
// first N files, and must stay bounded no matter how many files exist.
func TestLargePRSidebarFollowsCurrentFile(t *testing.T) {
	m := newLargeModel(121, 40, 100, 30)
	m.current = 100
	m.setDiffContent()
	view := m.View()

	if got := len(strings.Split(view, "\n")); got > 30 {
		t.Fatalf("view is %d lines for a 30-row terminal:\n%s", got, view)
	}
	for _, want := range []string{"↑ ", "↓ ", "> • 101 util100.go"} {
		if !strings.Contains(view, want) {
			t.Fatalf("sidebar did not window around file 101, missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "  1 pipeline.go") {
		t.Fatalf("sidebar still renders from step 1 while the current file is 101:\n%s", view)
	}
}

// A single 1,200-line rewrite is ~2,400 diff lines; the old 1,500-line cap cut
// off the tail of the file. The whole diff must render and the last line must
// be reachable with j.
func TestLargeDiffScrollsToLastLine(t *testing.T) {
	m := newLargeModel(1, 1200, 120, 40)

	if got, want := m.viewport.TotalLineCount() > 2*1200, true; got != want {
		t.Fatalf("pane renders %d lines; a 1,200-line rewrite must not be truncated", m.viewport.TotalLineCount())
	}
	for i := 0; i < m.viewport.TotalLineCount()+10; i++ {
		m.Update(keyMsg("j"))
	}
	pane := m.viewport.View()
	if !strings.Contains(pane, "EOF-MARKER") {
		t.Fatalf("last diff line is unreachable; pane tail:\n%s", pane)
	}
	if !m.viewport.AtBottom() {
		t.Fatalf("viewport not at bottom after scrolling: offset=%d", m.viewport.YOffset)
	}
	if view := m.View(); !strings.Contains(view, "100%") {
		t.Fatalf("footer does not show the scroll position:\n%s", view)
	}
}

// The safety cap must never touch a normal 1k-LOC review; only pathological
// files are cut, and the cut is visible.
func TestCapDiffOnlyCutsPathologicalFiles(t *testing.T) {
	realistic := strings.Repeat("+func handler(x int) int { return x }\n", 2400)
	if got := capDiff(realistic); strings.Contains(got, "not shown") {
		t.Fatalf("2,400-line diff was truncated:\n%s", got[len(got)-120:])
	}
	huge := strings.Repeat("+func handler(x int) int { return x }\n", maxDiffLines+10)
	got := capDiff(huge)
	if !strings.Contains(got, "10 more lines not shown") {
		t.Fatalf("pathological diff was silently cut:\n%s", got[len(got)-160:])
	}
}

// Marking a file done used to re-render the pane from the top, losing the
// reader's place in a long diff.
func TestMarkDoneKeepsScrollPosition(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := newLargeModel(1, 400, 120, 30)
	for i := 0; i < 120; i++ {
		m.Update(keyMsg("j"))
	}
	before := m.viewport.YOffset
	if before == 0 {
		t.Fatal("setup: viewport did not scroll")
	}
	m.Update(keyMsg("m"))
	if m.viewport.YOffset != before {
		t.Fatalf("marking done moved the pane: %d -> %d", before, m.viewport.YOffset)
	}
}

// The footer must not wrap into an extra line on a narrow terminal (that extra
// row is what scrolls the whole view and clips the panes), and no rendered
// line may be wider than the terminal.
func TestFooterFitsNarrowTerminal(t *testing.T) {
	for _, tc := range []struct{ width, height int }{
		{40, 20}, {60, 20}, {80, 24}, {100, 30}, {120, 40}, {200, 50},
	} {
		m := newLargeModel(20, 40, tc.width, tc.height)
		for _, help := range []bool{false, true} {
			m.help = help
			view := m.View()
			lines := strings.Split(view, "\n")
			if len(lines) > tc.height {
				t.Fatalf("%dx%d (help=%v): view is %d lines:\n%s", tc.width, tc.height, help, len(lines), view)
			}
			for _, ln := range lines {
				if w := lipgloss.Width(ln); w > tc.width {
					t.Fatalf("%dx%d (help=%v): line wider than terminal (%d): %q", tc.width, tc.height, help, w, ln)
				}
			}
		}
	}
}

// Diff lines are hard-wrapped, never reflowed: indentation, tabs and runs of
// spaces in code must survive, and no characters may be dropped.
func TestWrapCodeLinePreservesContent(t *testing.T) {
	line := "\tif x == 1 &&  y == 2 { // a deliberately long comment that has to wrap somewhere"
	chunks := wrapCodeLine(line, 32)
	if len(chunks) < 2 {
		t.Fatalf("expected the line to wrap, got %d chunk(s)", len(chunks))
	}
	for _, c := range chunks {
		if w := lipgloss.Width(c); w > 32 {
			t.Fatalf("chunk wider than 32 (%d): %q", w, c)
		}
		if strings.Contains(c, "\t") {
			t.Fatalf("tab survived into rendered output: %q", c)
		}
	}
	if got, want := strings.Join(chunks, ""), expandTabs(line, 4); got != want {
		t.Fatalf("wrap lost content:\n got: %q\nwant: %q", got, want)
	}
}

// windowLines is the sidebar's bound: never more rows than asked for, always
// centred on the current item, with explicit overflow markers.
func TestWindowLines(t *testing.T) {
	items := make([]string, 100)
	for i := range items {
		items[i] = fmt.Sprintf("file-%d", i)
	}
	cases := []struct {
		current, height int
		wantContains    string
	}{
		{0, 10, "file-0"},
		{50, 10, "file-50"},
		{99, 10, "file-99"},
	}
	for _, c := range cases {
		got := windowLines(items, c.current, c.height)
		if len(got) > c.height {
			t.Fatalf("current=%d height=%d: got %d lines", c.current, c.height, len(got))
		}
		joined := strings.Join(got, "\n")
		if !strings.Contains(joined, c.wantContains) {
			t.Fatalf("current=%d height=%d: window does not contain %q:\n%s", c.current, c.height, c.wantContains, joined)
		}
	}
	if got := windowLines(items[:5], 0, 10); len(got) != 5 {
		t.Fatalf("short list should not be padded, got %d", len(got))
	}
	// Tiny panes must still be bounded (2 rows can hold one file + one marker).
	for _, h := range []int{1, 2, 3, 4, 5} {
		for _, cur := range []int{0, 50, 99} {
			if got := windowLines(items, cur, h); len(got) > h {
				t.Fatalf("height=%d current=%d: got %d lines:\n%s", h, cur, len(got), strings.Join(got, "\n"))
			}
		}
	}
}
