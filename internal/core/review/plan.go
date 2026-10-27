package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/mohitpalsingh/ai-ded-pr/internal/core/models"
	"github.com/mohitpalsingh/ai-ded-pr/internal/modelsvc"
)

type Planner struct {
	Client    *modelsvc.Client
	Embedder  *modelsvc.Client
	fileEmbed map[string][]float64
}

func NewPlanner(llm, embed *modelsvc.Client) *Planner { return &Planner{Client: llm, Embedder: embed} }

// HeuristicScore orders files: types/models, core, api/handlers, ui, tests, docs/config, others.
func scorePath(p string) int {
	l := strings.ToLower(p)
	switch {
	case strings.HasSuffix(l, "_test.go") || strings.Contains(l, "test") || strings.Contains(l, "spec"):
		return 60
	case strings.Contains(l, "model") || strings.Contains(l, "type") || strings.Contains(l, "schema") || strings.Contains(l, "domain"):
		return 10
	case strings.Contains(l, "core") || strings.Contains(l, "lib") || strings.Contains(l, "internal"):
		return 20
	case strings.Contains(l, "api") || strings.Contains(l, "handler") || strings.Contains(l, "controller") || strings.Contains(l, "route"):
		return 30
	case strings.Contains(l, "ui") || strings.Contains(l, "view") || strings.Contains(l, "component") || strings.Contains(l, "tui"):
		return 40
	case strings.Contains(l, "config") || strings.Contains(l, "docs") || strings.HasSuffix(l, ".md") || strings.HasSuffix(l, ".json") || strings.HasSuffix(l, ".yaml") || strings.HasSuffix(l, ".yml"):
		return 70
	default:
		return 50
	}
}

func heuristicPlan(pr *models.PullRequest) *models.ReviewPlan {
	files := append([]models.ChangedFile(nil), pr.Files...)
	sort.SliceStable(files, func(i, j int) bool {
		// deleted after? keep added first? simple path score
		si, sj := scorePath(files[i].Path), scorePath(files[j].Path)
		if si != sj {
			return si < sj
		}
		// larger changes first within group? guide reviewers to core then small
		return (files[i].Additions + files[i].Deletions) > (files[j].Additions + files[j].Deletions)
	})

	steps := make([]models.ReviewStep, 0, len(files))
	for i, f := range files {
		focus := []string{}
		if f.Additions+f.Deletions > 200 {
			focus = append(focus, "large change; review in chunks")
		}
		if strings.Contains(strings.ToLower(f.Path), "test") {
			focus = append(focus, "verify tests cover new behavior")
		}
		risk := ""
		if f.Additions+f.Deletions > 400 {
			risk = "high churn"
		}
		steps = append(steps, models.ReviewStep{
			Order:     i + 1,
			Path:      f.Path,
			Rationale: heuristicRationale(f),
			Focus:     focus,
			Risk:      risk,
			Status:    models.StatusPending,
		})
	}
	summary := fmt.Sprintf("PR touches %d files (+%d/-%d). Suggested order groups foundational types first, then logic, then callers, tests and config.", len(pr.Files), pr.TotalAdd, pr.TotalDel)
	// Same dependency treatment as the model path: inferred edges, linear
	// order, and concrete reverse-dependency notes — all offline.
	inferDeps(steps, pr)
	steps = orderSteps(steps)
	steps = pairTestsWithCode(steps)
	annotateReverseDeps(steps)
	for i := range steps {
		steps[i].Order = i + 1
	}
	return &models.ReviewPlan{Summary: summary, Steps: steps}
}

// --- rationale formatting ---------------------------------------------------

// rationaleLabelRe finds section labels such as "Intent:", "Dependencies:",
// "Contracts impacted:", "Verify:" and "Risk:" in model output.
var rationaleLabelRe = regexp.MustCompile(`(?i)(intent|dependencies|dependency|contracts?|verify|verification|checks?|risk)\s*[:：]`)

type rationaleParts struct {
	intent, deps, verify, risk string
}

func labelKind(l string) string {
	switch {
	case strings.HasPrefix(l, "intent"):
		return "intent"
	case strings.HasPrefix(l, "depend"), strings.HasPrefix(l, "contract"):
		return "deps"
	case strings.HasPrefix(l, "verify"), strings.HasPrefix(l, "verification"), strings.HasPrefix(l, "check"):
		return "verify"
	default:
		return "risk"
	}
}

// flattenRationale strips markdown emphasis/labels and leading bullet chars so
// section detection works on plain text.
func flattenRationale(s string) string {
	s = strings.ReplaceAll(s, "**", "")
	s = strings.ReplaceAll(s, "__", "")
	s = strings.ReplaceAll(s, "`", "")
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(ln), "-*•◦# \t"))
	}
	return strings.Join(lines, "\n")
}

func tidySection(s string) string {
	s = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	truncated := strings.HasSuffix(s, "...")
	s = strings.Trim(s, " -:.,;")
	// Keep a truncation marker: re-canonicalizing an already-clipped bullet
	// used to strip its "...", silently hiding that text had been dropped.
	if truncated && s != "" {
		s += "..."
	}
	return s
}

func appendPart(dst *string, s string) {
	if *dst == "" {
		*dst = s
		return
	}
	*dst += " " + s
}

func splitRationale(raw string) rationaleParts {
	var out rationaleParts
	text := flattenRationale(raw)
	locs := rationaleLabelRe.FindAllStringSubmatchIndex(text, -1)
	if len(locs) == 0 {
		out.intent = tidySection(text)
		return out
	}
	if locs[0][0] > 0 {
		out.intent = tidySection(text[:locs[0][0]])
	}
	assign := func(kind, content string) {
		content = tidySection(content)
		if content == "" {
			return
		}
		switch kind {
		case "intent":
			appendPart(&out.intent, content)
		case "deps":
			appendPart(&out.deps, content)
		case "verify":
			appendPart(&out.verify, content)
		case "risk":
			appendPart(&out.risk, content)
		}
	}
	for i, m := range locs {
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		assign(labelKind(strings.ToLower(text[m[2]:m[3]])), text[m[1]:end])
	}
	return out
}

func clipWords(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	if i := strings.LastIndex(cut, " "); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "..."
}

func defaultIntent(f *models.ChangedFile) string {
	status := f.Status
	if status == "" {
		status = "modified"
	}
	s := fmt.Sprintf("modifies %s (%s, +%d/-%d)", f.Path, status, f.Additions, f.Deletions)
	if f.Language != "" {
		s += " " + f.Language
	}
	return s
}

func defaultRisk(f *models.ChangedFile) string {
	churn := f.Additions + f.Deletions
	switch {
	case churn >= 400:
		return "high churn; review in chunks and watch for regressions"
	case churn >= 100:
		return "medium churn; check edge cases in the changed hunks"
	default:
		return "low churn; quick correctness pass"
	}
}

// pairTestsWithCode moves a test step that depends on exactly one changed file
// directly after that file (stem + Test/IT/Spec/_test), so the reviewer checks
// the implementation and its test back to back. Tests that cannot be paired
// keep their position — no gratuitous reordering.
func pairTestsWithCode(steps []models.ReviewStep) []models.ReviewStep {
	if len(steps) < 2 {
		return steps
	}
	out := make([]models.ReviewStep, len(steps))
	copy(out, steps)
	for i := 0; i < len(out); i++ {
		t := out[i]
		// The primary dependency is the one the test covers by name; other
		// edges (e.g. a shared service) don't block pairing.
		dep := ""
		for _, d := range t.DependsOn {
			if coversTest(t.Path, d) {
				dep = d
				break
			}
		}
		if dep == "" {
			continue
		}
		di := -1
		for j := range out {
			if out[j].Path == dep {
				di = j
				break
			}
		}
		if di < 0 || di > i {
			continue
		}
		at := di + 1
		for at < i && coversTest(out[at].Path, dep) {
			at++
		}
		if at == i {
			continue // already directly after the file it covers
		}
		moved := out[i]
		copy(out[at+1:i+1], out[at:i])
		out[at] = moved
	}
	return out
}

// isTestFile reports whether a code file looks like a test module.
func isTestFile(p string) bool {
	stem, ok := codeStem(p)
	if !ok {
		return false
	}
	switch {
	case strings.HasSuffix(stem, "Test"), strings.HasSuffix(stem, "Tests"),
		strings.HasSuffix(stem, "IT"), strings.HasSuffix(stem, "Spec"),
		strings.HasSuffix(stem, "_test"), strings.HasSuffix(stem, "_spec"),
		strings.HasPrefix(stem, "test_"):
		return true
	}
	return false
}

// coversTest reports whether testPath is a test file for the code file depPath
// (e.g. ".../OrderServiceTest.java" covers ".../OrderService.java").
func coversTest(testPath, depPath string) bool {
	if !isTestFile(testPath) {
		return false
	}
	ts, ok1 := codeStem(testPath)
	ds, ok2 := codeStem(depPath)
	if !ok1 || !ok2 {
		return false
	}
	for _, suf := range []string{"Test", "Tests", "IT", "Spec"} {
		if ts == ds+suf {
			return true
		}
	}
	return ts == ds+"_test" || ts == ds+"_spec" || ts == "test_"+ds
}

// annotateReverseDeps upgrades generic "Dependencies" filler with concrete
// information now that dependency edges are known: which changed files rely on
// this one. The reviewer learns whether a file is load-bearing before reading it.
func annotateReverseDeps(steps []models.ReviewStep) {
	rev := map[string][]string{}
	for _, s := range steps {
		for _, d := range s.DependsOn {
			rev[d] = append(rev[d], s.Path)
		}
	}
	for i := range steps {
		names := rev[steps[i].Path]
		if len(names) == 0 {
			continue
		}
		// name up to three dependents; the rest becomes a count
		var short []string
		for _, n := range names {
			if len(short) == 3 {
				break
			}
			short = append(short, baseName(n))
		}
		txt := strings.Join(short, ", ")
		if len(names) > len(short) {
			txt += fmt.Sprintf(" (+%d more)", len(names)-len(short))
		}
		replacement := fmt.Sprintf("changed file(s) depending on this one: %s", txt)
		lines := strings.Split(steps[i].Rationale, "\n")
		for j, ln := range lines {
			if !strings.HasPrefix(ln, "- Dependencies: ") {
				continue
			}
			if strings.Contains(ln, "check callers, tests and configuration") {
				lines[j] = "- Dependencies: " + replacement
			}
			break
		}
		steps[i].Rationale = strings.Join(lines, "\n")
	}
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func buildRationale(p rationaleParts, f *models.ChangedFile) string {
	if p.intent == "" {
		p.intent = defaultIntent(f)
	}
	if p.deps == "" {
		p.deps = "check callers, tests and configuration that reference this file"
	}
	if p.verify == "" {
		p.verify = "walk each hunk for correctness, edge cases and consistency with surrounding code"
	}
	if p.risk == "" {
		p.risk = defaultRisk(f)
	}
	return strings.Join([]string{
		// Caps are a safety net against pathological model output only; the
		// old 160-char caps chopped complete sentences mid-way (and a second
		// canonicalization pass stripped the "..." marker, hiding the loss).
		"- Intent: " + clipWords(p.intent, 480),
		"- Dependencies: " + clipWords(p.deps, 480),
		"- Verify: " + clipWords(p.verify, 600),
		"- Risk: " + clipWords(p.risk, 480),
	}, "\n")
}

// FormatRationale normalizes any summary/rationale text into the canonical
// four-bullet review format so every file reads consistently.
func FormatRationale(raw string, f *models.ChangedFile) string {
	if f == nil {
		f = &models.ChangedFile{}
	}
	if strings.TrimSpace(raw) == "" {
		return buildRationale(rationaleParts{}, f)
	}
	return buildRationale(splitRationale(raw), f)
}

func heuristicRationale(f models.ChangedFile) string {
	return buildRationale(rationaleParts{}, &f)
}

func (p *Planner) Plan(ctx context.Context, pr *models.PullRequest) (*models.ReviewPlan, error) {
	if p.Client == nil {
		plan := heuristicPlan(pr)
		p.ensureFileEmbeddings(ctx, pr)
		plan.Steps = reorderStepsByCluster(plan.Steps, semanticClusters(pr, p.fileEmbed))
		return plan, nil
	}
	// Build compact file list. Always keep every file in the prompt: hunk
	// headers are shed first so the model never silently loses files.
	type fsummary struct {
		Path   string   `json:"path"`
		Status string   `json:"status"`
		Add    int      `json:"add"`
		Del    int      `json:"del"`
		Diff   string   `json:"diff,omitempty"`
		Hunks  []string `json:"hunks,omitempty"`
	}
	var files []fsummary
	for _, f := range pr.Files {
		var hs []string
		for _, h := range f.Hunks {
			if h.Header != "" {
				hs = append(hs, h.Header)
			}
			if len(hs) >= 6 {
				break
			}
		}
		files = append(files, fsummary{Path: f.Path, Status: f.Status, Add: f.Additions, Del: f.Deletions, Hunks: hs})
	}
	// Give the model the actual changed lines (bounded): rationales built from
	// file names alone are speculation, which is useless to a reviewer. The
	// per-file digest budget degrades gracefully; the file list stays complete.
	diffBudget := 8000
	for i := range pr.Files {
		d := diffDigest(pr.Files[i].Patch, 800)
		if d == "" || len(d) > diffBudget {
			continue
		}
		diffBudget -= len(d)
		files[i].Diff = d
	}
	p.ensureFileEmbeddings(ctx, pr)
	clusters := semanticClusters(pr, p.fileEmbed)
	fb, _ := json.Marshal(files)
	if len(fb) > 12000 {
		for i := range files {
			files[i].Hunks = nil
		}
		fb, _ = json.Marshal(files)
	}
	if len(fb) > 12000 {
		fb = append(fb[:12000], []byte("...")...)
	}
	clusterText := ""
	if len(clusters) > 0 {
		cb, _ := json.Marshal(clusters)
		clusterText = "\nSemantic clusters (files likely related and best reviewed together): " + string(cb)
	}
	prompt := fmt.Sprintf(`You are an expert code reviewer. Produce a guided review plan that minimizes the reviewer's time.

PR Title: %s
PR Body: %s
Files (JSON): %s%s

Return ONLY valid JSON with this shape:
{"summary":"2-3 sentence overview: what the PR does and where the risk concentrates","steps":[{"order":1,"path":"...","rationale":"- Intent: ...\n- Dependencies: ...\n- Verify: ...\n- Risk: ...","focus":["specific check"],"risk":"low|medium|high","depends_on":["exact-path-of-another-changed-file"],"est_minutes":2}]}

Rationale rules — exactly four bullet lines, these exact prefixes, each under 18 words. Base every bullet on the diff lines provided (each file has "diff": changed lines, + added, - removed); never speculate:
- "- Intent: " the behavior delta: what the code does after this change that it did not do before. Name concrete symbols/values from the diff. Never start with "Modifies"/"Updates"/"Adds" + the file name; never restate the path; never say "likely"/"possibly"/"probably".
- "- Dependencies: " first the exact changed paths this file imports or calls (as seen in the diff), then external contracts/services it touches.
- "- Verify: " one or two concrete checks: a specific edge case, input, symbol, or call site to inspect. Never generic advice like "check correctness".
- "- Risk: " the most plausible failure mode and its severity (low/medium/high).

Ordering rules:
- Steps must be in the exact linear order a senior reviewer should read them: shared types/interfaces/config consumed by others first, then core logic in call order (callee before caller), then entrypoints/APIs, then tests, then docs/resources.
- If this file imports, calls, or relies on behavior in another changed file, list that exact path in depends_on and make sure it appears earlier in the steps.
- Keep tightly related files adjacent (a test right after the code it tests); do not interleave unrelated groups.
- Include exactly one step for every file listed above: the list has %d files, so output exactly %d steps; never omit, merge or repeat a file (each path appears exactly once).
- depends_on lists only exact paths from the file list above; empty array otherwise.`, pr.Title, truncate(pr.Body, 1500), string(fb), clusterText, len(pr.Files), len(pr.Files))
	if os.Getenv("AI_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "PLAN prompt=%d bytes files=%d\n", len(prompt), len(files))
	}

	out, err := p.Client.Chat(ctx, "You only output valid JSON, no markdown.", prompt)
	if err != nil {
		return heuristicPlan(pr), nil
	}
	out = stripCodeFence(out)
	if os.Getenv("AI_DEBUG") != "" {
		fmt.Fprintln(os.Stderr, "RAW JSON: "+out)
	}
	wp, parseErr := parsePlan(out)
	if parseErr != nil {
		// Don't give up: the retry below asks for a strict JSON plan, and any
		// file still missing gets a heuristic summary instead of nothing.
		if os.Getenv("AI_DEBUG") != "" {
			fmt.Fprintln(os.Stderr, "PARSE ERR (will retry): "+parseErr.Error())
		}
		wp = planWire{}
	}
	if os.Getenv("AI_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "PARSED steps=%d summary=%q\n", len(wp.Steps), wp.Summary)
	}
	// validate paths and fill missing
	byPath := map[string]*models.ChangedFile{}
	for i := range pr.Files {
		byPath[pr.Files[i].Path] = &pr.Files[i]
	}
	seen := map[string]bool{}
	steps := wireToSteps(wp.Steps, byPath, seen)
	// If the model omitted files (truncated output, schema drift), retry once
	// with a compact prompt so every file still gets an AI-authored rationale.
	if len(seen) < len(pr.Files) && p.Client != nil {
		var missing []fsummary
		for _, f := range pr.Files {
			if !seen[f.Path] {
				missing = append(missing, fsummary{Path: f.Path, Status: f.Status, Add: f.Additions, Del: f.Deletions, Diff: diffDigest(f.Patch, 600)})
			}
		}
		if os.Getenv("AI_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "FOLLOW-UP for %d file(s)\n", len(missing))
		}
		mb, _ := json.Marshal(missing)
		followUp := fmt.Sprintf(`You are an expert code reviewer producing a guided review plan.
Return ONLY valid JSON: {"steps":[{"path":"...","rationale":"- Intent: ...\n- Dependencies: ...\n- Verify: ...\n- Risk: ...","focus":["specific check"],"risk":"low|medium|high","depends_on":["exact-path-of-another-changed-file"]}]}
Rules:
- Include exactly one step for every file below: %d file(s) means %d steps total.
- rationale must be exactly four bullet lines with prefixes "- Intent: ", "- Dependencies: ", "- Verify: ", "- Risk: ", each under 18 words; Intent states the behavior delta, never restates the path.
- depends_on lists only exact paths from the list below (empty array otherwise).
- Output MUST start with { and end with }: no prose, no markdown, no code fences.
Files (JSON): %s`, len(missing), len(missing), string(mb))
		if out2, err2 := p.Client.Chat(ctx, "You only output valid JSON, no markdown.", followUp); err2 == nil {
			if wp2, err3 := parsePlan(out2); err3 == nil {
				steps = append(steps, wireToSteps(wp2.Steps, byPath, seen)...)
			}
		}
	}
	// append any missing files
	for _, f := range pr.Files {
		if !seen[f.Path] {
			steps = append(steps, models.ReviewStep{Path: f.Path, Rationale: heuristicRationale(f), Risk: "medium", Status: models.StatusPending})
		}
	}
	// normalize every rationale so all files read in the same 4-bullet shape
	for i := range steps {
		if f := byPath[steps[i].Path]; f != nil {
			steps[i].Rationale = FormatRationale(steps[i].Rationale, f)
		}
	}
	// Make the order linear to read: dependencies first, model order otherwise.
	inferDeps(steps, pr) // deterministic symbol-based edges on top of the model's
	steps = orderSteps(steps)
	steps = pairTestsWithCode(steps)
	annotateReverseDeps(steps)
	for i := range steps {
		steps[i].Order = i + 1
	}
	if strings.TrimSpace(string(wp.Summary)) == "" {
		wp.Summary = flexString(heuristicPlan(pr).Summary)
	}
	plan := models.ReviewPlan{Summary: string(wp.Summary), Steps: steps}
	if len(plan.Steps) == 0 {
		return heuristicPlan(pr), nil
	}
	return &plan, nil
}

// diffDigest condenses a patch to the changed lines only (+/-), keeping hunk
// headers for orientation and skipping context lines, capped near maxChars so
// the model sees what actually changed without blowing the prompt budget.
func diffDigest(patch string, maxChars int) string {
	if strings.TrimSpace(patch) == "" {
		return ""
	}
	var b strings.Builder
	for _, ln := range strings.Split(patch, "\n") {
		var keep string
		switch {
		case strings.HasPrefix(ln, "@@"):
			keep = ln
		case strings.HasPrefix(ln, "+++"), strings.HasPrefix(ln, "---"), strings.HasPrefix(ln, "diff "), strings.HasPrefix(ln, "index "):
			continue
		case strings.HasPrefix(ln, "+"), strings.HasPrefix(ln, "-"):
			keep = ln
		default:
			continue
		}
		if b.Len()+len(keep)+1 > maxChars {
			b.WriteString("\n...(truncated)")
			break
		}
		b.WriteString(keep)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// orderSteps makes the reading order consistent: when a step depends on
// another changed file, that file comes first. Stable otherwise — the model's
// order is preserved except where a dependency forces a move (cycles keep
// their original relative order).
func orderSteps(steps []models.ReviewStep) []models.ReviewStep {
	if len(steps) < 2 {
		return steps
	}
	idx := map[string]int{}
	for i, s := range steps {
		idx[s.Path] = i
	}
	indeg := make([]int, len(steps))
	dependents := make([][]int, len(steps))
	for i, s := range steps {
		for _, d := range s.DependsOn {
			j, ok := idx[d]
			if !ok || j == i {
				continue
			}
			indeg[i]++
			dependents[j] = append(dependents[j], i)
		}
	}
	var ready []int
	for i := range steps {
		if indeg[i] == 0 {
			ready = append(ready, i)
		}
	}
	out := make([]models.ReviewStep, 0, len(steps))
	done := make([]bool, len(steps))
	for len(ready) > 0 {
		// always take the lowest original index → stable result
		k := 0
		for x := 1; x < len(ready); x++ {
			if ready[x] < ready[k] {
				k = x
			}
		}
		i := ready[k]
		ready = append(ready[:k], ready[k+1:]...)
		done[i] = true
		out = append(out, steps[i])
		for _, j := range dependents[i] {
			indeg[j]--
			if indeg[j] == 0 {
				ready = append(ready, j)
			}
		}
	}
	for i := range steps {
		if !done[i] {
			out = append(out, steps[i])
		}
	}
	return out
}

// planWire mirrors the review-plan JSON while tolerating alternate field names
// and shapes (arrays of bullet strings, sibling intent/verify/risk keys, and
// trailing commas) that smaller models tend to emit.
type planWire struct {
	Summary flexString `json:"summary"`
	Steps   []stepWire `json:"steps"`
}

type stepWire struct {
	Order      int           `json:"order"`
	Path       string        `json:"path"`
	Rationale  flexRationale `json:"rationale"`
	Intent     flexString    `json:"intent"`
	Deps       flexString    `json:"dependencies"`
	DepsAlt    flexString    `json:"dependencies_contracts"`
	Contracts  flexString    `json:"contracts"`
	Verify     flexString    `json:"verify"`
	RiskText   flexString    `json:"risk"`
	Focus      []string      `json:"focus"`
	DependsOn  flexPaths     `json:"depends_on"`
	EstMinutes int           `json:"est_minutes"`
}

// flexPaths accepts a JSON array of strings (preferred), a single string
// ("a.go, b.go"), or an array of objects with a "path" key.
type flexPaths []string

func (fp *flexPaths) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	var raw []json.RawMessage
	if b[0] == '[' {
		if err := json.Unmarshal(b, &raw); err != nil {
			return err
		}
	} else {
		raw = []json.RawMessage{b}
	}
	var out []string
	for _, r := range raw {
		var s string
		if json.Unmarshal(r, &s) == nil {
			for _, part := range strings.Split(s, ",") {
				if part = strings.TrimSpace(part); part != "" {
					out = append(out, part)
				}
			}
			continue
		}
		var obj struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(r, &obj) == nil && obj.Path != "" {
			out = append(out, obj.Path)
		}
	}
	*fp = out
	return nil
}

// flexRationale accepts the rationale as a string, an array of strings, or an
// object with intent/dependencies/verify/risk keys (models differ), and
// normalizes objects into the canonical bullet layout.
type flexRationale string

func (f *flexRationale) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '{' {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(b, &obj); err != nil {
			return err
		}
		pick := func(keys ...string) string {
			for _, k := range keys {
				for kk, v := range obj {
					if !strings.EqualFold(kk, k) {
						continue
					}
					var s string
					if json.Unmarshal(v, &s) == nil {
						return strings.TrimSpace(s)
					}
					var arr []string
					if json.Unmarshal(v, &arr) == nil {
						return strings.TrimSpace(strings.Join(arr, " "))
					}
				}
			}
			return ""
		}
		var lines []string
		if v := pick("intent"); v != "" {
			lines = append(lines, "- Intent: "+v)
		}
		if v := pick("dependencies", "dependencies_contracts", "contracts", "deps"); v != "" {
			lines = append(lines, "- Dependencies: "+v)
		}
		if v := pick("verify", "verification"); v != "" {
			lines = append(lines, "- Verify: "+v)
		}
		if v := pick("risk"); v != "" {
			lines = append(lines, "- Risk: "+v)
		}
		if len(lines) == 0 {
			return fmt.Errorf("flexRationale: object without recognizable keys")
		}
		*f = flexRationale(strings.Join(lines, "\n"))
		return nil
	}
	var fs flexString
	if err := fs.UnmarshalJSON(b); err != nil {
		return err
	}
	*f = flexRationale(fs)
	return nil
}

// flexString accepts a JSON string or an array of strings/values.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	switch b[0] {
	case '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	case '[':
		var parts []string
		var raws []json.RawMessage
		if err := json.Unmarshal(b, &raws); err != nil {
			return err
		}
		for _, raw := range raws {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				parts = append(parts, s)
				continue
			}
			var n float64
			if json.Unmarshal(raw, &n) == nil {
				parts = append(parts, fmt.Sprintf("%g", n))
				continue
			}
			parts = append(parts, string(raw))
		}
		*f = flexString(strings.Join(parts, "\n"))
		return nil
	default:
		var n float64
		if err := json.Unmarshal(b, &n); err == nil {
			*f = flexString(fmt.Sprintf("%g", n))
			return nil
		}
		return fmt.Errorf("flexString: unsupported JSON %q", string(b))
	}
}

var trailingCommaRe = regexp.MustCompile(`,\s*([}\]])`)

// sanitizeJSON removes trailing commas before closing braces/brackets.
func sanitizeJSON(s string) string {
	return trailingCommaRe.ReplaceAllString(s, "$1")
}

// trimToJSON cuts leading/trailing prose around the outermost JSON object.
func trimToJSON(s string) string {
	i := strings.Index(s, "{")
	j := strings.LastIndex(s, "}")
	if i >= 0 && j > i {
		return s[i : j+1]
	}
	return s
}

// unmarshalPlan tries the payload as-is plus common truncation repairs.
func unmarshalPlan(s string, wp *planWire) error {
	s = strings.TrimSpace(s)
	variants := []string{s, trimToJSON(s), sanitizeJSON(s), sanitizeJSON(trimToJSON(s))}
	var candidates []string
	seen := map[string]bool{}
	for _, v := range variants {
		for _, c := range []string{v, v + "}", v + "]}", v + "}]}", v + ",\"summary\":\"\"}]}"} {
			if !seen[c] {
				seen[c] = true
				candidates = append(candidates, c)
			}
		}
	}
	var err error
	for _, c := range candidates {
		if err = json.Unmarshal([]byte(c), wp); err == nil {
			return nil
		}
	}
	return err
}

// salvageSteps extracts intact step objects from a model response, even when
// the surrounding JSON is truncated or malformed. Braces inside strings and
// nested objects are handled by tracking depth, not by regex.
func salvageSteps(s string) []stepWire {
	var out []stepWire
	var stack []int
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			// skip string contents (handling escapes) so braces inside don't count
			i++
			for i < len(s) {
				if s[i] == '\\' {
					i += 2
					continue
				}
				if s[i] == '"' {
					break
				}
				i++
			}
		case '{':
			stack = append(stack, i)
		case '}':
			if len(stack) == 0 {
				continue
			}
			start := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			obj := s[start : i+1]
			if !strings.Contains(obj, `"path"`) {
				continue
			}
			for _, candidate := range []string{obj, sanitizeJSON(obj)} {
				var w stepWire
				if json.Unmarshal([]byte(candidate), &w) == nil && w.Path != "" {
					out = append(out, w)
					break
				}
			}
		}
	}
	return out
}

// parsePlan decodes a model response, salvaging intact steps from truncated output.
func parsePlan(out string) (planWire, error) {
	var wp planWire
	err := unmarshalPlan(out, &wp)
	if err != nil {
		if ws := salvageSteps(out); len(ws) > 0 {
			return planWire{Steps: ws}, nil
		}
	}
	return wp, err
}

// lookupFile resolves a model-provided path to a changed file, tolerating
// "./" prefixes and unambiguous suffix matches.
func lookupFile(byPath map[string]*models.ChangedFile, p string) *models.ChangedFile {
	if f := byPath[p]; f != nil {
		return f
	}
	q := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(p), "./"), "/")
	if f := byPath[q]; f != nil {
		return f
	}
	if q == "" {
		return nil
	}
	var found *models.ChangedFile
	for path, f := range byPath {
		if strings.HasSuffix(path, "/"+q) {
			if found != nil {
				return nil // ambiguous
			}
			found = f
		}
	}
	if found != nil {
		return found
	}
	// Models often name dependencies by class or FQN instead of path:
	// "com.a.b.OrderService", "com.a.b.OrderService.java" or
	// bare "OrderService". Match these against extension-stripped
	// path suffixes.
	if !strings.Contains(q, "/") {
		cand := q
		if strings.Contains(cand, ".") {
			// drop a trailing code extension if present ("...C.java")
			if i := strings.LastIndexByte(cand, '.'); i > 0 && knownCodeExt(cand[i+1:]) {
				cand = cand[:i]
			}
			cand = strings.ReplaceAll(cand, ".", "/")
		}
		var found *models.ChangedFile
		for path, f := range byPath {
			stem := path
			if i := strings.LastIndexByte(stem, '.'); i > strings.LastIndexByte(stem, '/') {
				stem = stem[:i]
			}
			if stem == cand || strings.HasSuffix(stem, "/"+cand) {
				if found != nil {
					return nil // ambiguous
				}
				found = f
			}
		}
		return found
	}
	return nil
}

// knownCodeExt reports whether ext is a source-file extension whose basename
// (minus extension) is a plausible symbol name the model may cite.
func knownCodeExt(ext string) bool {
	switch ext {
	case "java", "go", "kt", "kts", "ts", "tsx", "js", "jsx", "py", "rb",
		"rs", "cs", "swift", "c", "cc", "cpp", "h", "hpp", "scala", "m", "mm":
		return true
	}
	return false
}

// codeStem returns the symbol name a code filename stands for (e.g.
// "OrderService" for ".../OrderService.java").
func codeStem(p string) (string, bool) {
	base := p
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.LastIndexByte(base, '.'); i > 0 && knownCodeExt(base[i+1:]) {
		return base[:i], true
	}
	return "", false
}

// inferDeps adds dependency edges that are certain from the diffs themselves:
// when a file's patch mentions another changed code file's symbol, that edge is
// real regardless of what the model said (or forgot). Tests point at the class
// under test, callers at the service they call — exactly the jumps a reviewer
// needs, at zero model cost.
func inferDeps(steps []models.ReviewStep, pr *models.PullRequest) {
	if pr == nil || len(steps) < 2 {
		return
	}
	patch := make(map[string]string, len(pr.Files))
	for _, f := range pr.Files {
		patch[f.Path] = f.Patch
	}
	type cand struct{ path, stem string }
	var cands []cand
	for _, s := range steps {
		if stem, ok := codeStem(s.Path); ok && len(stem) >= 4 {
			cands = append(cands, cand{s.Path, stem})
		}
	}
	for i := range steps {
		text := patch[steps[i].Path]
		if text == "" {
			continue
		}
		content := diffContentLines(text)
		have := make(map[string]bool, len(steps[i].DependsOn))
		for _, d := range steps[i].DependsOn {
			have[d] = true
		}
		for _, c := range cands {
			if c.path == steps[i].Path || have[c.path] {
				continue
			}
			if containsWord(content, c.stem) {
				steps[i].DependsOn = append(steps[i].DependsOn, c.path)
				have[c.path] = true
			}
		}
	}
}

// diffContentLines joins a patch's lines, dropping file headers so a file's own
// name in "+++ b/..." cannot be mistaken for a symbol reference.
func diffContentLines(patch string) string {
	var b strings.Builder
	for _, ln := range strings.Split(patch, "\n") {
		if strings.HasPrefix(ln, "diff ") || strings.HasPrefix(ln, "index ") ||
			strings.HasPrefix(ln, "+++ ") || strings.HasPrefix(ln, "--- ") ||
			strings.HasPrefix(ln, "+++b/") {
			continue
		}
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	return b.String()
}

// containsWord reports whether text contains word delimited by non-identifier
// bytes, so "OrderService" does not match inside
// "OrderServiceHelper".
func containsWord(text, word string) bool {
	if word == "" {
		return false
	}
	for i := 0; i+len(word) <= len(text); {
		j := strings.Index(text[i:], word)
		if j < 0 {
			return false
		}
		j += i
		before := j == 0 || !isIdentByte(text[j-1])
		afterIdx := j + len(word)
		after := afterIdx >= len(text) || !isIdentByte(text[afterIdx])
		if before && after {
			return true
		}
		i = j + len(word)
	}
	return false
}

func isIdentByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// wireToSteps maps parsed model steps onto known changed files, tolerating
// alternate field names and formatting every rationale canonically.
func wireToSteps(ws []stepWire, byPath map[string]*models.ChangedFile, seen map[string]bool) []models.ReviewStep {
	var steps []models.ReviewStep
	for _, w := range ws {
		f := lookupFile(byPath, w.Path)
		if f == nil || seen[f.Path] {
			continue
		}
		seen[f.Path] = true
		// tolerate models that emit sibling keys instead of the 4-bullet rationale
		parts := splitRationale(string(w.Rationale))
		if parts.intent == "" {
			parts.intent = tidySection(string(w.Intent))
		}
		if parts.deps == "" {
			parts.deps = tidySection(firstNonEmpty(string(w.Deps), string(w.DepsAlt), string(w.Contracts)))
		}
		if parts.verify == "" {
			parts.verify = tidySection(string(w.Verify))
		}
		riskText := tidySection(string(w.RiskText))
		level := "medium"
		switch strings.ToLower(riskText) {
		case "low", "medium", "high":
			level = strings.ToLower(riskText)
		default:
			if parts.risk == "" {
				parts.risk = riskText
			}
		}
		if isNoise(parts.intent) {
			parts.intent = ""
		}
		if isNoise(parts.deps) {
			parts.deps = ""
		}
		if isNoise(parts.verify) {
			parts.verify = ""
		}
		if isNoise(parts.risk) {
			parts.risk = ""
		}
		// canonicalize changed-file dependencies: exact known paths only,
		// de-duplicated, never self-referencing
		var depends []string
		depSeen := map[string]bool{}
		for _, d := range w.DependsOn {
			df := lookupFile(byPath, d)
			if df == nil || df.Path == f.Path || depSeen[df.Path] {
				continue
			}
			depSeen[df.Path] = true
			depends = append(depends, df.Path)
		}
		steps = append(steps, models.ReviewStep{
			Path:       f.Path,
			Rationale:  buildRationale(parts, f),
			Focus:      compactFocus(w.Focus),
			Risk:       level,
			DependsOn:  depends,
			EstMinutes: w.EstMinutes,
			Status:     models.StatusPending,
		})
	}
	return steps
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func isNoise(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "none", "n/a", "na", "nil", "null", "unknown", "why now", "not applicable", "no dependencies":
		return true
	}
	return false
}

func compactFocus(in []string) []string {
	var out []string
	for _, s := range in {
		s = tidySection(s)
		if s == "" || isNoise(s) {
			continue
		}
		out = append(out, clipWords(s, 120))
		if len(out) >= 3 {
			break
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		// remove first line
		i := strings.Index(s, "\n")
		if i >= 0 {
			s = s[i+1:]
		}
		j := strings.LastIndex(s, "```")
		if j >= 0 {
			s = s[:j]
		}
	}
	return strings.TrimSpace(s)
}

// SearchRoutes asks LLM which changed files are relevant to query.
func (p *Planner) Search(ctx context.Context, pr *models.PullRequest, query string) []string {
	// Semantic search first (aids guided review).
	if p.Embedder != nil {
		p.ensureFileEmbeddings(ctx, pr)
		if p.fileEmbed != nil {
			vecs, err := p.Embedder.Embed(ctx, []string{query})
			if err == nil && len(vecs) == 1 {
				type item struct {
					path  string
					score float64
				}
				var items []item
				for path, vec := range p.fileEmbed {
					items = append(items, item{path: path, score: cosine(vecs[0], vec)})
				}
				// sort
				for i := 1; i < len(items); i++ {
					for j := i; j > 0 && items[j-1].score < items[j].score; j-- {
						items[j-1], items[j] = items[j], items[j-1]
					}
				}
				var res []string
				k := 8
				if len(items) < k {
					k = len(items)
				}
				for i := 0; i < k; i++ {
					if items[i].score >= 0.25 {
						res = append(res, items[i].path)
					}
				}
				if len(res) > 0 {
					return res
				}
			}
		}
	}
	if p.Client == nil {
		// keyword fallback
		q := strings.ToLower(query)
		var res []string
		for _, f := range pr.Files {
			if strings.Contains(strings.ToLower(f.Path), q) || strings.Contains(strings.ToLower(f.Patch), q) {
				res = append(res, f.Path)
			}
		}
		return res
	}
	var paths []string
	for _, f := range pr.Files {
		paths = append(paths, f.Path)
	}
	pb, _ := json.Marshal(paths)
	prompt := fmt.Sprintf(`Given a PR and a reviewer question, return JSON array of file paths (from the list) most relevant. Question: %s
Files: %s
Return only a JSON array of strings.`, query, string(pb))
	out, _ := p.Client.Chat(ctx, "You only output JSON.", prompt)
	out = stripCodeFence(out)
	var res []string
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		// try repair truncated
		if err2 := json.Unmarshal([]byte(strings.TrimSpace(out)+"]"), &res); err2 != nil {
			// keyword fallback
			q := strings.ToLower(query)
			for _, f := range pr.Files {
				if strings.Contains(strings.ToLower(f.Path), q) || strings.Contains(strings.ToLower(f.Patch), q) {
					res = append(res, f.Path)
				}
			}
			return res
		}
	}
	if len(res) == 0 {
		q := strings.ToLower(query)
		for _, f := range pr.Files {
			if strings.Contains(strings.ToLower(f.Path), q) || strings.Contains(strings.ToLower(f.Patch), q) {
				res = append(res, f.Path)
			}
		}
	}
	return res
}

// --- semantic helpers -------------------------------------------------------

func (p *Planner) ensureFileEmbeddings(ctx context.Context, pr *models.PullRequest) {
	if p.Embedder == nil || p.fileEmbed != nil || pr == nil {
		return
	}
	p.fileEmbed = map[string][]float64{}
	var paths []string
	var texts []string
	for _, f := range pr.Files {
		patch := f.Patch
		if len(patch) > 1500 {
			patch = patch[:1500]
		}
		paths = append(paths, f.Path)
		texts = append(texts, f.Path+"\n"+patch)
	}
	if len(texts) == 0 {
		return
	}
	vecs, err := p.Embedder.Embed(ctx, texts)
	if err != nil || len(vecs) != len(paths) {
		p.fileEmbed = nil
		return
	}
	for i, path := range paths {
		p.fileEmbed[path] = vecs[i]
	}
}

func cosine(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func semanticClusters(pr *models.PullRequest, embeds map[string][]float64) [][]string {
	if len(embeds) < 2 {
		return nil
	}
	paths := []string{}
	for _, f := range pr.Files {
		paths = append(paths, f.Path)
	}
	// union-find
	parent := map[string]string{}
	for _, p := range paths {
		parent[p] = p
	}
	var find func(string) string
	find = func(x string) string {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b string) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}
	for i := 0; i < len(paths); i++ {
		for j := i + 1; j < len(paths); j++ {
			sim := cosine(embeds[paths[i]], embeds[paths[j]])
			if sim >= 0.60 {
				union(paths[i], paths[j])
			}
		}
	}
	groups := map[string][]string{}
	for _, p := range paths {
		groups[find(p)] = append(groups[find(p)], p)
	}
	var out [][]string
	for _, g := range groups {
		if len(g) > 1 {
			out = append(out, g)
		}
	}
	return out
}

func reorderStepsByCluster(steps []models.ReviewStep, clusters [][]string) []models.ReviewStep {
	if len(clusters) == 0 {
		return steps
	}
	groupID := map[string]int{}
	for i, g := range clusters {
		for _, p := range g {
			groupID[p] = i + 1
		}
	}
	// stable: group files together but keep original relative order within group
	type keyed struct {
		step models.ReviewStep
		gid  int
		idx  int
	}
	arr := make([]keyed, len(steps))
	for i, s := range steps {
		g := groupID[s.Path]
		if g == 0 {
			g = 1000 + i // unknown files last-ish but preserve
		}
		arr[i] = keyed{step: s, gid: g, idx: i}
	}
	arr2 := append([]keyed(nil), arr...)
	// insertion sort
	for i := 1; i < len(arr2); i++ {
		key := arr2[i]
		j := i - 1
		for j >= 0 && arr2[j].gid > key.gid {
			arr2[j+1] = arr2[j]
			j--
		}
		arr2[j+1] = key
	}
	out := make([]models.ReviewStep, len(steps))
	for i, k := range arr2 {
		out[i] = k.step
	}
	for i := range out {
		out[i].Order = i + 1
	}
	return out
}
