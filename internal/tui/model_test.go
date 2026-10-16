package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mohit/ai-ded-pr/internal/config"
	"github.com/mohit/ai-ded-pr/internal/core/models"
)

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
