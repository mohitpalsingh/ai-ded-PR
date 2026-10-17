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

// Long AI bullets must be preserved in full: the old 160-char caps chopped
// complete Verify sentences mid-way, and because a second canonicalization
// pass stripped the "..." marker the loss was invisible in the UI.
func TestCompleteBulletsSurviveCanonicalization(t *testing.T) {
	longVerify := "Ensure that the monitorOne function correctly retrieves the OperatorParameters from the task history record and passes them to the NewDatabricksOperator constructor. Verify that the failureAlert is propagated correctly and that the OnFailure callback is invoked for both Error and CallbackFailed statuses."
	raw := "- Intent: x\n- Dependencies: task_v3.go\n- Verify: " + longVerify + "\n- Risk: y"
	f := &models.ChangedFile{Path: "internal/scheduler/task_v3.go"}
	got := FormatRationale(raw, f)
	assertCanonical(t, "long verify", got)
	if !strings.Contains(got, "CallbackFailed statuses") {
		t.Fatalf("verify bullet lost its tail:\n%s", got)
	}
	if strings.Contains(got, "...") {
		t.Fatalf("unexpected truncation marker:\n%s", got)
	}
	if again := FormatRationale(got, f); again != got {
		t.Fatalf("canonicalization not idempotent:\nfirst:  %q\nsecond: %q", got, again)
	}
}

// The safety cap still guards against pathological output: it cuts at a word
// boundary and leaves a "..." that survives re-canonicalization.
func TestSafetyCapKeepsVisibleEllipsis(t *testing.T) {
	raw := "- Intent: x\n- Dependencies: y\n- Verify: " + strings.Repeat("word ", 300) + "\n- Risk: z"
	f := &models.ChangedFile{Path: "a.go"}
	got := FormatRationale(raw, f)
	vl := bulletLine(got, "- Verify: ")
	if !strings.HasSuffix(vl, "...") {
		t.Fatalf("safety-capped bullet must end with \"...\": %q", vl)
	}
	if body := strings.TrimSuffix(vl, "..."); !strings.HasSuffix(body, "word") {
		t.Fatalf("cut must land on a word boundary: %q", vl)
	}
	if len(vl) > len("- Verify: ")+600+len("...") {
		t.Fatalf("safety cap too loose: %d chars", len(vl))
	}
	again := FormatRationale(got, f)
	if vl2 := bulletLine(again, "- Verify: "); !strings.HasSuffix(vl2, "...") {
		t.Fatalf("ellipsis lost on re-canonicalization: %q", vl2)
	}
}

func bulletLine(rationale, prefix string) string {
	for _, ln := range strings.Split(rationale, "\n") {
		if strings.HasPrefix(ln, prefix) {
			return ln
		}
	}
	return ""
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

// The plan must come out in a linear reading order: if a step depends on
// another changed file, that file is reviewed first. Bogus, self, duplicate
// and "./"-prefixed paths in depends_on must be tolerated.
func TestPlanOrdersDependenciesFirst(t *testing.T) {
	raw := `{"summary":"s","steps":[
		{"path":"ui.go","rationale":"- Intent: wires the new service into the UI\n- Dependencies: svc.go\n- Verify: click path\n- Risk: mis-wiring (low)","risk":"low","depends_on":["./svc.go","ghost.go","svc.go"]},
		{"path":"svc.go","rationale":"- Intent: adds caching to the service\n- Dependencies: store.go\n- Verify: TTL expiry\n- Risk: stale reads (medium)","risk":"medium","depends_on":["store.go"]},
		{"path":"store.go","rationale":"- Intent: persists entries with a TTL\n- Dependencies: none\n- Verify: eviction\n- Risk: unbounded growth (low)","risk":"low"}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":`+strconv.Quote(raw)+`}}]}`)
	}))
	defer srv.Close()

	pr := &models.PullRequest{Title: "t", Files: []models.ChangedFile{
		{Path: "ui.go", Status: "modified", Additions: 5},
		{Path: "svc.go", Status: "modified", Additions: 10},
		{Path: "store.go", Status: "added", Additions: 30},
	}}
	plan, err := NewPlanner(modelsvc.NewClient(srv.URL), nil).Plan(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 3 {
		t.Fatalf("want 3 steps, got %d", len(plan.Steps))
	}
	pos := map[string]int{}
	for i, s := range plan.Steps {
		pos[s.Path] = i
		assertCanonical(t, s.Path, s.Rationale)
		if s.Order != i+1 {
			t.Fatalf("step %s: order %d, want %d", s.Path, s.Order, i+1)
		}
	}
	// svc depends on store; ui depends on svc -> store < svc < ui
	if !(pos["store.go"] < pos["svc.go"] && pos["svc.go"] < pos["ui.go"]) {
		t.Fatalf("dependency order not respected: %v", pos)
	}
	// deps canonicalized: "./svc.go" -> "svc.go"; "ghost.go" dropped; no dups
	ui := plan.Steps[pos["ui.go"]]
	if len(ui.DependsOn) != 1 || ui.DependsOn[0] != "svc.go" {
		t.Fatalf("ui depends_on = %v, want [svc.go]", ui.DependsOn)
	}
}

// Even when the model emits no depends_on at all, symbol references in the
// diffs must produce jump edges and force dependencies to be reviewed first.
func TestPlanInfersDepsFromDiffSymbols(t *testing.T) {
	const svc = "src/main/java/com/x/ApolloConfigService.java"
	const test = "src/test/java/com/x/ApolloConfigServiceTest.java"
	raw := `{"summary":"s","steps":[` +
		`{"path":"` + test + `","rationale":"- Intent: covers the new lookup path\n- Dependencies: none\n- Verify: run the test\n- Risk: flaky assertion (low)","risk":"low"},` +
		`{"path":"` + svc + `","rationale":"- Intent: adds a TTL lookup\n- Dependencies: none\n- Verify: TTL expiry\n- Risk: stale reads (medium)","risk":"medium"}` +
		`]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":`+strconv.Quote(raw)+`}}]}`)
	}))
	defer srv.Close()

	pr := &models.PullRequest{Title: "t", Files: []models.ChangedFile{
		{Path: svc, Status: "modified", Additions: 6, Patch: "@@ -10,3 +10,5 @@\n     private final Map<String, String> cache;\n+    String get(String key) { return cache.get(key); }\n"},
		{Path: test, Status: "modified", Additions: 4, Patch: "@@ -5,2 +5,4 @@\n+        ApolloConfigService svc = new ApolloConfigService();\n+        assertEquals(\"v\", svc.get(\"k\"));\n"},
	}}
	plan, err := NewPlanner(modelsvc.NewClient(srv.URL), nil).Plan(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, s := range plan.Steps {
		pos[s.Path] = i
	}
	if pos[svc] != 0 || pos[test] != 1 {
		t.Fatalf("dependency order not enforced by inference: %v", pos)
	}
	testStep := plan.Steps[pos[test]]
	if len(testStep.DependsOn) != 1 || testStep.DependsOn[0] != svc {
		t.Fatalf("test depends_on = %v, want [%s]", testStep.DependsOn, svc)
	}
	// The service is load-bearing for the test: its Dependencies bullet must
	// say so concretely (reverse-dep annotation replaced the filler).
	if !strings.Contains(plan.Steps[pos[svc]].Rationale, "ApolloConfigServiceTest.java") {
		t.Fatalf("reverse-dep note missing: %q", plan.Steps[pos[svc]].Rationale)
	}
}

// Model output naming a dependency by fully-qualified class name must map onto
// the changed file path (and only that file).
func TestLookupFileAcceptsFQN(t *testing.T) {
	byPath := map[string]*models.ChangedFile{
		"ap-comms-dds-server/src/main/java/com/hotstar/ap/comms/service/impl/ApolloConfigService.java":       {Path: "ap-comms-dds-server/src/main/java/com/hotstar/ap/comms/service/impl/ApolloConfigService.java"},
		"ap-comms-dds-server/src/main/java/com/hotstar/ap/comms/service/handler/CommsCollectionHandler.java": {Path: "ap-comms-dds-server/src/main/java/com/hotstar/ap/comms/service/handler/CommsCollectionHandler.java"},
	}
	for _, q := range []string{
		"com.hotstar.ap.comms.service.impl.ApolloConfigService",
		"com.hotstar.ap.comms.service.impl.ApolloConfigService.java",
		"ApolloConfigService",
		"impl/ApolloConfigService.java",
		"./ap-comms-dds-server/src/main/java/com/hotstar/ap/comms/service/impl/ApolloConfigService.java",
	} {
		f := lookupFile(byPath, q)
		if f == nil || !strings.HasSuffix(f.Path, "ApolloConfigService.java") {
			t.Fatalf("lookupFile(%q) = %+v, want ApolloConfigService.java", q, f)
		}
	}
	if f := lookupFile(byPath, "com.hotstar.ap.comms.service.impl.GhostService"); f != nil {
		t.Fatalf("lookupFile(ghost) = %+v, want nil", f)
	}
}

// A test that covers a changed class is placed directly after that class —
// even when the test also depends on other changed files — while unpaired
// tests keep their position.
func TestPairTestsWithCode(t *testing.T) {
	steps := []models.ReviewStep{
		{Path: "src/main/java/Shared.java"},
		{Path: "src/main/java/S.java"},
		{Path: "src/main/java/Other.java"},
		{Path: "src/test/java/STest.java", DependsOn: []string{"src/main/java/Shared.java", "src/main/java/S.java"}},
		{Path: "src/test/java/UnpairedTest.java", DependsOn: []string{"src/main/java/Ghost.java"}},
		{Path: "docs/readme.md"},
	}
	out := pairTestsWithCode(steps)
	var got []string
	for _, s := range out {
		got = append(got, s.Path)
	}
	want := "src/main/java/Shared.java,src/main/java/S.java,src/test/java/STest.java,src/main/java/Other.java,src/test/java/UnpairedTest.java,docs/readme.md"
	if strings.Join(got, ",") != want {
		t.Fatalf("order = %s, want %s", strings.Join(got, ","), want)
	}
}

// depends_on may arrive as a comma string or as objects; both parse.
func TestFlexPathsTolerance(t *testing.T) {
	raw := `{"steps":[{"path":"a.go","rationale":"- Intent: x - Dependencies: y - Verify: z - Risk: w","depends_on":"b.go, ./c.go"},{"path":"b.go","rationale":"- Intent: x - Dependencies: y - Verify: z - Risk: w","depends_on":[{"path":"a.go"}]}]}`
	wp, err := parsePlan(raw)
	if err != nil {
		t.Fatalf("parsePlan: %v", err)
	}
	if len(wp.Steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(wp.Steps))
	}
	var got []string
	for _, s := range wp.Steps {
		if s.Path == "a.go" {
			got = s.DependsOn
		}
	}
	if len(got) != 2 || got[0] != "b.go" || got[1] != "./c.go" {
		t.Fatalf("string depends_on parsed as %v", got)
	}
}

// orderSteps must never drop or duplicate steps, even with cycles.
func TestOrderStepsCycleKeepsEverything(t *testing.T) {
	steps := []models.ReviewStep{
		{Path: "a.go", DependsOn: []string{"b.go"}},
		{Path: "b.go", DependsOn: []string{"a.go"}},
		{Path: "c.go"},
	}
	out := orderSteps(steps)
	if len(out) != 3 {
		t.Fatalf("want 3 steps, got %d", len(out))
	}
	seen := map[string]bool{}
	for _, s := range out {
		if seen[s.Path] {
			t.Fatalf("duplicate step %s", s.Path)
		}
		seen[s.Path] = true
	}
}
