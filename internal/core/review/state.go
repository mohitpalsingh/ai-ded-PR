package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/mohit/ai-ded-pr/internal/core/models"
)

func StatePath(pr *models.PullRequest) string {
	home, _ := os.UserHomeDir()
	key := pr.Host + "-" + pr.Owner + "-" + pr.Repo + "-" + itoa(pr.Number)
	if pr.URL != "" {
		key = safe(pr.URL)
	}
	dir := filepath.Join(home, ".cache", "ai-pr-review", "state")
	os.MkdirAll(dir, 0755)
	return filepath.Join(dir, key+".json")
}

func safe(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, ":", "_")
	return s
}

func itoa(n int) string {
	// simple
	if n == 0 {
		return "local"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

type State struct {
	Done      map[string]bool   `json:"done"`
	Skipped   map[string]bool   `json:"skipped"`
	Notes     map[string]string `json:"notes"`
	Summaries map[string]string `json:"summaries,omitempty"`
}

func LoadState(pr *models.PullRequest) *State {
	st := &State{Done: map[string]bool{}, Skipped: map[string]bool{}, Notes: map[string]string{}}
	b, err := os.ReadFile(StatePath(pr))
	if err != nil {
		return st
	}
	json.Unmarshal(b, st)
	if len(st.Summaries) == 0 && len(st.Notes) > 0 {
		st.Summaries = map[string]string{}
		for k, v := range st.Notes {
			st.Summaries[k] = v
		}
	}
	if st.Done == nil {
		st.Done = map[string]bool{}
	}
	if st.Skipped == nil {
		st.Skipped = map[string]bool{}
	}
	if st.Notes == nil {
		st.Notes = map[string]string{}
	}
	return st
}

func SaveState(pr *models.PullRequest, st *State) {
	b, _ := json.MarshalIndent(st, "", "  ")
	os.WriteFile(StatePath(pr), b, 0644)
}

// Apply restores statuses onto a plan.
func ApplyState(plan *models.ReviewPlan, st *State) {
	for i := range plan.Steps {
		if st.Done[plan.Steps[i].Path] {
			plan.Steps[i].Status = models.StatusDone
		} else if st.Skipped[plan.Steps[i].Path] {
			plan.Steps[i].Status = models.StatusSkipped
		}
	}
}
