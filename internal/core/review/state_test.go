package review

import (
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
