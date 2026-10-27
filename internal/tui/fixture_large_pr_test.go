package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mohitpalsingh/ai-ded-pr/internal/config"
	"github.com/mohitpalsingh/ai-ded-pr/internal/core/models"
	"github.com/mohitpalsingh/ai-ded-pr/internal/core/review"
	"github.com/mohitpalsingh/ai-ded-pr/internal/hosts/localgit"
)

// TestLargePRFixtureRepo is the end-to-end regression for the reported case:
// a real git repository with a 1,200-line rewrite in one file (~2,400 diff
// lines) plus 120 other changed files. The review must open with the diff pane
// on screen, and the very last line of the big diff must be reachable.
//
// The shell equivalent (for manual TUI testing) is
// scripts/make-large-pr-fixture.sh.
func TestLargePRFixtureRepo(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	const loc = 1200
	const others = 120
	pipeline := func(head bool) string {
		var b strings.Builder
		b.WriteString("package service\n\n")
		for i := 0; i < loc; i++ {
			switch {
			case !head:
				fmt.Fprintf(&b, "func handler%d(x int) int { return x + %d }\n", i, i)
			case i == loc-1:
				fmt.Fprintf(&b, "func handler%d(x int) int { return x*2 + %d } // EOF-MARKER\n", i, i)
			default:
				fmt.Fprintf(&b, "func handler%d(x int) int { return x*2 + %d } // changed\n", i, i)
			}
		}
		return b.String()
	}

	run("init", "-q", "-b", "main")
	run("config", "user.email", "fixture@example.com")
	run("config", "user.name", "fixture")
	run("config", "commit.gpgsign", "false")

	write("internal/service/pipeline.go", pipeline(false))
	for i := 0; i < others; i++ {
		write(fmt.Sprintf("pkg/util/util%d.go", i), fmt.Sprintf("package util\n\nfunc Util%d() int { return %d }\n", i, i))
	}
	run("add", "-A")
	run("commit", "-qm", "base")
	run("branch", "base")

	write("internal/service/pipeline.go", pipeline(true))
	for i := 0; i < others; i++ {
		write(fmt.Sprintf("pkg/util/util%d.go", i), fmt.Sprintf("package util\n\nfunc Util%d() int { return %d }\n", i, i+1))
	}
	run("add", "-A")
	run("commit", "-qm", "large change")
	run("branch", "large-pr")
	run("checkout", "-q", "large-pr")

	pr, err := localgit.New(dir, "base", "large-pr").Load(context.Background())
	if err != nil {
		t.Fatalf("load fixture repo: %v", err)
	}
	if got, want := len(pr.Files), others+1; got != want {
		t.Fatalf("fixture files = %d, want %d", got, want)
	}
	var big *models.ChangedFile
	for i := range pr.Files {
		if pr.Files[i].Path == largePatchPath {
			big = &pr.Files[i]
		}
	}
	if big == nil {
		t.Fatalf("fixture is missing %s", largePatchPath)
	}
	if big.Additions != loc || big.Deletions != loc {
		t.Fatalf("big file stats = +%d/-%d, want +%d/-%d", big.Additions, big.Deletions, loc, loc)
	}
	if diffLines := strings.Count(big.Patch, "\n"); diffLines < 2*loc {
		t.Fatalf("fixture diff has %d lines, want at least %d", diffLines, 2*loc)
	}

	plan, err := review.NewPlanner(nil, nil).Plan(context.Background(), pr)
	if err != nil {
		t.Fatalf("heuristic plan: %v", err)
	}
	m := New(config.Default(), true)
	m.screen = screenReview
	m.pr = pr
	m.plan = plan
	m.current = 0
	for i, s := range plan.Steps {
		if s.Path == largePatchPath {
			m.current = i
		}
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	view := m.View()
	if lines := len(strings.Split(view, "\n")); lines > 40 {
		t.Fatalf("view overflows a 40-row terminal (%d lines)", lines)
	}
	if !strings.Contains(view, "--- diff ---") || !strings.Contains(view, "-func handler0(x int) int { return x + 0 }") {
		t.Fatalf("diff pane is not visible for the large PR:\n%s", view)
	}

	for i := 0; i < m.viewport.TotalLineCount()+10; i++ {
		m.Update(keyMsg("j"))
	}
	if !m.viewport.AtBottom() {
		t.Fatalf("viewport did not reach the bottom: offset=%d lines=%d", m.viewport.YOffset, m.viewport.TotalLineCount())
	}
	if tail := m.viewport.View(); !strings.Contains(tail, "EOF-MARKER") {
		t.Fatalf("end of the big diff is unreachable:\n%s", tail)
	}
}
