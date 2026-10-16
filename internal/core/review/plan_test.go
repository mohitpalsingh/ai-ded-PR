package review

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/mohit/ai-ded-pr/internal/core/models"
	"github.com/mohit/ai-ded-pr/internal/modelsvc"
)

var canonicalPrefixes = []string{"- Intent: ", "- Dependencies: ", "- Verify: ", "- Risk: "}

func assertCanonical(t *testing.T, name, got string) {
	t.Helper()
	lines := strings.Split(got, "\n")
	if len(lines) != 4 {
		t.Fatalf("%s: want 4 lines, got %d: %q", name, len(lines), got)
	}
	for i, want := range canonicalPrefixes {
		if !strings.HasPrefix(lines[i], want) {
			t.Fatalf("%s: line %d = %q, want prefix %q", name, i, lines[i], want)
		}
	}
	if strings.Contains(got, "**") || strings.Contains(got, "`") {
		t.Fatalf("%s: markdown leftovers in %q", name, got)
	}
}

func TestFormatRationaleCanonicalizesModelVariants(t *testing.T) {
	cases := []struct{ name, raw string }{
		{"bold markdown", "*   **Intent:** Modifies the `CompletableFuture` return type.\n*   **Dependencies/Contracts Impacted:** changes the handler contract.\n*   **Verify:** check the async path.\n*   **Risk:** untested futures."},
		{"inline", "Intent: parse list. Dependencies/contracts impacted: parseList callers. Verify: error handling. Risk: bad parsing."},
		{"different labels", "- Code change details: parses experiment names.\n- Impacted Dependencies: parseList callers.\n- Verification Required: handles errors.\n- Risk: incorrect parsing."},
		{"missing sections", "Adds a blacklist check before logging experiments."},
		{"empty", ""},
	}
	f := &models.ChangedFile{Path: "svc/handler.go", Status: "modified", Additions: 4, Deletions: 1}
	for _, tc := range cases {
		assertCanonical(t, tc.name, FormatRationale(tc.raw, f))
	}
	// defaults must mention the file
	got := FormatRationale("", f)
	if !strings.Contains(got, "svc/handler.go") {
		t.Fatalf("default intent should reference the file: %q", got)
	}
}

func TestHeuristicPlanFormatsEveryFile(t *testing.T) {
	pr := &models.PullRequest{Title: "t", Files: []models.ChangedFile{
		{Path: "a.go", Status: "modified", Additions: 10, Deletions: 2},
		{Path: "b.go", Status: "added", Additions: 400, Deletions: 0},
		{Path: "c_test.go", Status: "modified", Additions: 3, Deletions: 3},
	}}
	plan, err := NewPlanner(nil, nil).Plan(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 3 {
		t.Fatalf("want 3 steps, got %d", len(plan.Steps))
	}
	for _, s := range plan.Steps {
		if strings.Count(s.Rationale, "- Intent:") != 1 {
			t.Fatalf("step %s: want exactly one Intent bullet, got %q", s.Path, s.Rationale)
		}
		assertCanonical(t, s.Path, s.Rationale)
	}
}

func TestApplyStateKeepsPersistedRationales(t *testing.T) {
	persisted := "- Intent: old but stable\n- Dependencies: x\n- Verify: y\n- Risk: z"
	plan := &models.ReviewPlan{Steps: []models.ReviewStep{
		{Path: "a.go", Rationale: "- Intent: freshly generated"},
		{Path: "b.go", Rationale: "- Intent: no persisted copy"},
	}}
	st := &State{
		Done:      map[string]bool{},
		Skipped:   map[string]bool{},
		Summaries: map[string]string{"a.go": persisted},
	}
	ApplyState(plan, st)
	if plan.Steps[0].Rationale != persisted {
		t.Fatalf("persisted rationale must win; got %q", plan.Steps[0].Rationale)
	}
	if !strings.Contains(plan.Steps[1].Rationale, "no persisted copy") {
		t.Fatalf("paths without persisted summary must keep plan rationale; got %q", plan.Steps[1].Rationale)
	}
}

// Reproduces what the real Gemma models actually returned: pretty-printed JSON,
// rationale as an array of bullet strings, trailing commas, and a truncated
// tail. The plan must still give every file a canonical 4-bullet rationale.
func TestPlanToleratesModelQuirks(t *testing.T) {
	raw := `{
  "summary": "Adds retry logic and config tuning.",
  "steps": [
    {
      "order": 1,
      "path": "a.go",
      "rationale": [
        "- Intent: Adds retry logic to the client.",
        "- Dependencies: Callers of Do() and the HTTP layer.",
        "- Verify: Backoff behavior on 5xx responses.",
        "- Risk: Duplicate side effects on retries."
      ],
      "focus": ["Do()"],
      "risk": "medium",
    },
    {
      "order": 2,
      "path": "b.go",
      "rationale": "- Intent: retunes config defaults\n- Dependencies: config loader\n- Verify: default values\n- Risk: behavior drift",
      "risk": "low",
    },
    {"order": 3, "path": "c.go", "rationale": "why now"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":`+strconv.Quote(raw)+`}}]}`)
	}))
	defer srv.Close()

	pr := &models.PullRequest{Title: "t", Files: []models.ChangedFile{
		{Path: "a.go", Status: "modified", Additions: 10, Deletions: 2},
		{Path: "b.go", Status: "modified", Additions: 4, Deletions: 1},
		{Path: "c.go", Status: "added", Additions: 50, Deletions: 0},
	}}
	plan, err := NewPlanner(modelsvc.NewClient(srv.URL), nil).Plan(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 3 {
		t.Fatalf("want 3 steps (one per file), got %d", len(plan.Steps))
	}
	for _, s := range plan.Steps {
		assertCanonical(t, s.Path, s.Rationale)
	}
	// the salvaged/array rationale must survive as real content
	var aGo string
	for _, s := range plan.Steps {
		if s.Path == "a.go" {
			aGo = s.Rationale
		}
	}
	if !strings.Contains(aGo, "retry logic") {
		t.Fatalf("a.go should keep the model rationale, got %q", aGo)
	}
	if strings.Contains(aGo, "...") && !strings.Contains(aGo, "retry logic") {
		t.Fatalf("a.go rationale lost content: %q", aGo)
	}
}

// If the model answers in prose (no JSON at all), Plan must retry with the
// strict compact prompt and still return AI-authored rationales for every file.
func TestPlanRetriesAfterProseResponse(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		var content string
		if calls == 1 {
			content = "Here is the review plan:\n\n1. a.go - Intent: change A\n2. b.go - Intent: change B\n3. c.go - Intent: change C"
		} else {
			content = `{"steps":[
				{"path":"a.go","rationale":"- Intent: RETRY A\n- Dependencies: da\n- Verify: va\n- Risk: ra","risk":"low"},
				{"path":"b.go","rationale":"- Intent: RETRY B\n- Dependencies: db\n- Verify: vb\n- Risk: rb","risk":"low"},
				{"path":"c.go","rationale":"- Intent: RETRY C\n- Dependencies: dc\n- Verify: vc\n- Risk: rc","risk":"low"}
			]}`
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":`+strconv.Quote(content)+`}}]}`)
	}))
	defer srv.Close()

	pr := &models.PullRequest{Title: "t", Files: []models.ChangedFile{
		{Path: "a.go", Status: "modified", Additions: 10, Deletions: 2},
		{Path: "b.go", Status: "modified", Additions: 4, Deletions: 1},
		{Path: "c.go", Status: "added", Additions: 50, Deletions: 0},
	}}
	plan, err := NewPlanner(modelsvc.NewClient(srv.URL), nil).Plan(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("want a retry call after prose output, got %d call(s)", calls)
	}
	if len(plan.Steps) != 3 {
		t.Fatalf("want 3 steps, got %d", len(plan.Steps))
	}
	for _, s := range plan.Steps {
		assertCanonical(t, s.Path, s.Rationale)
		if !strings.Contains(s.Rationale, "RETRY") {
			t.Fatalf("%s: want AI-authored retry rationale, got %q", s.Path, s.Rationale)
		}
	}
}

// Models sometimes nest the four bullets under a rationale object instead of a
// flat string; every variant must still become canonical bullets.
func TestParseRationaleAsObject(t *testing.T) {
	raw := `{"steps":[{"path":"./a.go","rationale":{"intent":"adds cache","dependencies":"Redis","verify":"TTL math","risk":"stale reads"}}]}`
	wp, err := parsePlan(raw)
	if err != nil {
		t.Fatalf("parsePlan failed: %v", err)
	}
	if len(wp.Steps) != 1 {
		t.Fatalf("want 1 step, got %d", len(wp.Steps))
	}
	pr := &models.PullRequest{Files: []models.ChangedFile{{Path: "a.go", Status: "modified", Additions: 1}}}
	byPath := map[string]*models.ChangedFile{"a.go": &pr.Files[0]}
	steps := wireToSteps(wp.Steps, byPath, map[string]bool{})
	if len(steps) != 1 {
		t.Fatalf("want 1 mapped step (with ./ prefix tolerated), got %d", len(steps))
	}
	assertCanonical(t, "object rationale", steps[0].Rationale)
	if !strings.Contains(steps[0].Rationale, "adds cache") || !strings.Contains(steps[0].Rationale, "TTL math") {
		t.Fatalf("object values lost: %q", steps[0].Rationale)
	}
}

// Truncated output with nested objects: complete steps must still be salvaged.
func TestSalvageHandlesNestedObjects(t *testing.T) {
	raw := `{"summary":"x","steps":[` +
		`{"path":"a.go","rationale":{"intent":"one","verify":"two","risk":"three"},"focus":["f"],"risk":"low"},` +
		`{"path":"b.go","rationale":"- Intent: trunc"`
	ws := salvageSteps(raw)
	if len(ws) != 1 || ws[0].Path != "a.go" {
		t.Fatalf("want 1 salvaged step for a.go, got %+v", ws)
	}
}
