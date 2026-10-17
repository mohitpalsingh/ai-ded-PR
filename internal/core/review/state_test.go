package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mohit/ai-ded-pr/internal/core/models"
)

func TestStatePathUniquePerRepoAndDiff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base := models.PullRequest{Host: "local", Repo: "app", URL: "/tmp/repo-x", HeadSHA: "aaaa1111bbbb"}
	other := base
	other.URL = "/tmp/repo-y"
	if StatePath(&base) == StatePath(&other) {
		t.Fatal("different repos must not share a state path")
	}
	newHead := base
	newHead.HeadSHA = "cccc2222dddd"
	if StatePath(&base) == StatePath(&newHead) {
		t.Fatal("different diffs must not share a state path")
	}
	// same repo and same diff must be stable
	same := base
	if StatePath(&base) != StatePath(&same) {
		t.Fatal("state path must be stable for the same repo/diff")
	}
	if _, err := os.Stat(filepath.Dir(StatePath(&base))); err != nil {
		t.Fatalf("state dir should have been created: %v", err)
	}
}

// Pre-v2 state files may contain summaries silently clipped by the old
// 160-char rationale cap: they must be dropped once so summaries regenerate,
// while review progress is preserved.
func TestLegacyStateDropsStaleSummaries(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	pr := &models.PullRequest{Owner: "o", Repo: "r", HeadSHA: "0123456789abcdef"}

	legacy := map[string]any{
		"done":      map[string]bool{"a.go": true},
		"skipped":   map[string]bool{},
		"notes":     map[string]string{"a.go": "- Intent: old note alias"},
		"summaries": map[string]string{"a.go": "- Intent: clipped text ending mid"},
	}
	b, _ := json.Marshal(legacy)
	if err := os.WriteFile(StatePath(pr), b, 0644); err != nil {
		t.Fatal(err)
	}
	st := LoadState(pr)
	if len(st.Summaries) != 0 {
		t.Fatalf("legacy summaries must be dropped so they regenerate, got %v", st.Summaries)
	}
	if !st.Done["a.go"] {
		t.Fatalf("review progress must be preserved: %v", st.Done)
	}

	// New saves are versioned and round-trip summaries untouched.
	st.Summaries = map[string]string{"a.go": "- Intent: complete and correct"}
	SaveState(pr, st)
	got := LoadState(pr)
	if got.Version != stateVersion {
		t.Fatalf("saved state version = %d, want %d", got.Version, stateVersion)
	}
	if got.Summaries["a.go"] != "- Intent: complete and correct" {
		t.Fatalf("v2 summaries must survive reload, got %v", got.Summaries)
	}
	if !got.Done["a.go"] {
		t.Fatalf("progress must survive reload: %v", got.Done)
	}
}
