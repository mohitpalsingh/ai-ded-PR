package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mohitpalsingh/ai-ded-pr/internal/config"
	"github.com/mohitpalsingh/ai-ded-pr/internal/core/models"
	"github.com/mohitpalsingh/ai-ded-pr/internal/modelsvc"
)

var errNotHealthy = errors.New("model server not healthy")

func keyMsg(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// Dependency jumps move to the referenced changed file and `b` returns to the
// origin, without touching review state or the files on disk.
func TestDependencyJumpAndBack(t *testing.T) {
	m := New(config.Default(), true)
	m.screen = screenReview
	m.viewport.Width = 100
	m.viewport.Height = 30
	m.pr = &models.PullRequest{Files: []models.ChangedFile{
		{Path: "store.go"}, {Path: "svc.go"}, {Path: "ui.go"},
	}}
	m.plan = &models.ReviewPlan{Steps: []models.ReviewStep{
		{Path: "store.go", Order: 1, Status: models.StatusPending, Rationale: "- Intent: a\n- Dependencies: b\n- Verify: c\n- Risk: d"},
		{Path: "svc.go", Order: 2, Status: models.StatusPending, Rationale: "- Intent: a\n- Dependencies: b\n- Verify: c\n- Risk: d",
			DependsOn: []string{"store.go"}},
		{Path: "ui.go", Order: 3, Status: models.StatusPending, Rationale: "- Intent: a\n- Dependencies: b\n- Verify: c\n- Risk: d"},
	}}
	m.current = 1 // svc.go, depends on store.go

	found := ""
	for _, msg := range []tea.KeyMsg{keyMsg("1")} {
		_, _ = m.Update(msg)
		found = m.plan.Steps[m.current].Path
	}
	if found != "store.go" {
		t.Fatalf("after jump: current = %s, want store.go", found)
	}
	if len(m.depStack) != 1 || m.depStack[0] != 1 {
		t.Fatalf("depStack = %v, want [1]", m.depStack)
	}

	_, _ = m.Update(keyMsg("b"))
	if m.plan.Steps[m.current].Path != "svc.go" {
		t.Fatalf("after back: current = %s, want svc.go", m.plan.Steps[m.current].Path)
	}
	if len(m.depStack) != 0 {
		t.Fatalf("depStack should be empty, got %v", m.depStack)
	}
}

// The review screen must not open before the plan is ready: while the model
// is starting we stay on the loading screen, and only planReadyMsg opens it.
func TestReviewOpensOnlyWhenPlanReady(t *testing.T) {
	m := New(config.Default(), false)
	m.pr = &models.PullRequest{Files: []models.ChangedFile{{Path: "a.go", Additions: 2, Deletions: 1}}}
	if cmd := m.beginLoading("starting local Gemma (Metal)"); cmd == nil {
		t.Fatal("beginLoading should return a spinner tick")
	}
	if m.screen != screenLoading {
		t.Fatalf("screen = %v, want screenLoading", m.screen)
	}

	// The model becomes ready: planning kicks off, review must stay closed.
	_, cmd := m.Update(serverReadyMsg{client: modelsvc.NewClient("http://127.0.0.1:1")})
	if cmd == nil {
		t.Fatal("expected a plan command once the model is ready")
	}
	if m.screen != screenLoading {
		t.Fatalf("review opened before the plan was ready (screen=%v)", m.screen)
	}

	// The plan arrives: now the review opens, with content already in place.
	plan := &models.ReviewPlan{Steps: []models.ReviewStep{{
		Path: "a.go", Order: 1, Status: models.StatusPending,
		Rationale: "- Intent: x\n- Dependencies: y\n- Verify: z\n- Risk: w",
	}}}
	_, _ = m.Update(planReadyMsg{plan: plan})
	if m.screen != screenReview {
		t.Fatalf("review did not open on planReadyMsg (screen=%v)", m.screen)
	}
}

// A failed model startup must never strand the user on the loading screen:
// the heuristic plan is used and the review opens.
func TestModelUnavailableStillOpensReview(t *testing.T) {
	m := New(config.Default(), false)
	m.pr = &models.PullRequest{Files: []models.ChangedFile{{Path: "a.go", Additions: 2, Deletions: 1}}}
	m.beginLoading("starting local Gemma (Metal)")

	_, cmd := m.Update(serverReadyMsg{err: errNotHealthy})
	if cmd == nil {
		t.Fatal("expected a fallback plan command when the model is unavailable")
	}
	msg := cmd() // heuristic plan runs offline
	_, _ = m.Update(msg)
	if m.screen != screenReview {
		t.Fatalf("review did not open after fallback (screen=%v)", m.screen)
	}
	if m.plan == nil || len(m.plan.Steps) != 1 {
		t.Fatalf("fallback plan = %+v, want 1 step", m.plan)
	}
}

// The pane layout: file line, blank line, "Rationale:" heading on its own
// line, then the four bullets.
func TestPaneRationaleHeadingLayout(t *testing.T) {
	m := New(config.Default(), true)
	m.screen = screenReview
	m.viewport.Width = 120
	m.viewport.Height = 40
	m.pr = &models.PullRequest{Files: []models.ChangedFile{{Path: "a.go", Additions: 3, Deletions: 1}}}
	m.plan = &models.ReviewPlan{Steps: []models.ReviewStep{{
		Path: "a.go", Order: 1, Status: models.StatusPending,
		Rationale: "- Intent: Gets a key\n- Dependencies: none\n- Verify: run it\n- Risk: nil (low)",
	}}}
	m.current = 0
	m.setDiffContent()
	v := m.viewport.View()
	lines := strings.Split(v, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	v = strings.Join(lines, "\n")
	if !strings.Contains(v, "(+3/-1)\n\nRationale:\n- Intent: Gets a key") {
		t.Fatalf("pane layout wrong:\n%s", v)
	}
}

// Long lines must wrap instead of getting chopped by the viewport, and every
// word must survive the wrap in order (including hard-split long tokens).
func TestWrapTextKeepsAllContent(t *testing.T) {
	long := "Verify: Ensure that the NewDatabricksOperator function correctly initializes both the conn and failureAlert fields with the provided values"
	got := wrapText(""+long+"\n\n- Risk: short", 40)
	for _, ln := range strings.Split(got, "\n") {
		if lipgloss.Width(ln) > 40 {
			t.Fatalf("line wider than 40: %q", ln)
		}
	}
	gotWords := strings.Join(strings.Fields(got), " ")
	wantWords := strings.Join(strings.Fields(long+" - Risk: short"), " ")
	if gotWords != wantWords {
		t.Fatalf("word wrap lost content:\n got: %s\nwant: %s", gotWords, wantWords)
	}

	token := strings.Repeat("abcdefghij", 10) // 100-char single word
	got = wrapText(token, 40)
	for _, ln := range strings.Split(got, "\n") {
		if lipgloss.Width(ln) > 40 {
			t.Fatalf("hard-split line wider than 40: %q", ln)
		}
	}
	if j := strings.Join(strings.Fields(got), ""); j != token {
		t.Fatalf("hard split lost content: %q", j)
	}
}

// The review pane shows jumpable dependencies with their hotkeys.
func TestDiffPaneShowsDependencyLinks(t *testing.T) {
	m := New(config.Default(), true)
	m.screen = screenReview
	m.viewport.Width = 120
	m.viewport.Height = 40
	m.pr = &models.PullRequest{Files: []models.ChangedFile{{Path: "svc.go"}, {Path: "ui.go"}}}
	m.plan = &models.ReviewPlan{Steps: []models.ReviewStep{
		{Path: "svc.go", Order: 1, Status: models.StatusPending, Rationale: "- Intent: a\n- Dependencies: b\n- Verify: c\n- Risk: d"},
		{Path: "ui.go", Order: 2, Status: models.StatusPending, Rationale: "- Intent: a\n- Dependencies: b\n- Verify: c\n- Risk: d", DependsOn: []string{"svc.go"}},
	}}
	m.current = 1
	m.setDiffContent()
	if !strings.Contains(m.viewport.View(), "Deps: [1] svc.go") {
		t.Fatalf("dependency line missing from pane:\n%s", m.viewport.View())
	}
}

// Long dependency lines (three paths + hint) must wrap inside the pane instead
// of being chopped mid-way, and every line must fit the pane width.
func TestDiffPaneWrapsLongDependencyLine(t *testing.T) {
	m := New(config.Default(), true)
	m.screen = screenReview
	m.viewport.Width = 50
	m.viewport.Height = 40
	const file = "internal/core/operator/databricks/databricks_operator.go"
	deps := []string{
		"internal/core/operator/databricks/failure_alert.go",
		"internal/core/operator/operator.go",
		"internal/core/operator/databricks/parameters.go",
	}
	m.pr = &models.PullRequest{Files: []models.ChangedFile{{Path: file, Additions: 37, Deletions: 4}}}
	m.plan = &models.ReviewPlan{Steps: []models.ReviewStep{{
		Path: file, Order: 1, Status: models.StatusPending,
		Rationale: "- Intent: x\n- Dependencies: y\n- Verify: z\n- Risk: w",
		DependsOn: deps,
	}}}
	m.current = 0
	m.setDiffContent()
	v := m.viewport.View()
	for _, ln := range strings.Split(v, "\n") {
		if w := lipgloss.Width(strings.TrimRight(ln, " ")); w > 50 {
			t.Fatalf("pane line wider than 50 (%d): %q", w, ln)
		}
	}
	for _, want := range []string{"failure_alert.go", "parameters.go", "1-3 jump", "b return)"} {
		if !strings.Contains(v, want) {
			t.Fatalf("wrapped Deps line lost %q:\n%s", want, v)
		}
	}
}

// Files without changed dependencies produce a hint, not a jump.
func TestDependencyJumpNoDeps(t *testing.T) {
	m := New(config.Default(), true)
	m.screen = screenReview
	m.viewport.Width = 100
	m.viewport.Height = 30
	m.pr = &models.PullRequest{Files: []models.ChangedFile{{Path: "a.go"}}}
	m.plan = &models.ReviewPlan{Steps: []models.ReviewStep{
		{Path: "a.go", Order: 1, Status: models.StatusPending, Rationale: "- Intent: x\n- Dependencies: y\n- Verify: z\n- Risk: w"},
	}}
	m.current = 0
	_, _ = m.Update(keyMsg("3"))
	if m.current != 0 {
		t.Fatalf("current moved without deps: %d", m.current)
	}
	if m.status == "" {
		t.Fatal("want a status hint when a file has no dependencies")
	}
}
