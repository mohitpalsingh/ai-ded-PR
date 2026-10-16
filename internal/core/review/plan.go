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

	"github.com/mohit/ai-ded-pr/internal/core/models"
	"github.com/mohit/ai-ded-pr/internal/modelsvc"
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
	return strings.Trim(s, " -:.,;")
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
		"- Intent: " + clipWords(p.intent, 200),
		"- Dependencies: " + clipWords(p.deps, 160),
		"- Verify: " + clipWords(p.verify, 160),
		"- Risk: " + clipWords(p.risk, 160),
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
	prompt := fmt.Sprintf(`You are an expert code reviewer. Given a PR, produce a guided review plan.

PR Title: %s
PR Body: %s
Files (JSON): %s%s

Return ONLY valid JSON with this shape:
{"summary":"2-3 sentence overview of what the PR does and its risk","steps":[{"order":1,"path":"...","rationale":"- Intent: ...\n- Dependencies: ...\n- Verify: ...\n- Risk: ...","focus":["specific check"],"risk":"low|medium|high","est_minutes":2}]}
Rules:
- Rationale must be exactly four bullet lines, in this order and with these exact prefixes: "- Intent: ", "- Dependencies: ", "- Verify: ", "- Risk: ".
- Rationale must reference concrete code changes (added/removed behavior, symbols, paths). Keep every bullet under 20 words. No generic placeholders like "why now".
- Keep semantic clusters together.
- Order files to minimize cognitive load: foundational types/models -> core logic -> callers/API/UI -> tests -> config/docs.
- Path must be one of the changed files.
- Include exactly one step for every file listed above; never omit a file.`, pr.Title, truncate(pr.Body, 1500), string(fb), clusterText)
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
				missing = append(missing, fsummary{Path: f.Path, Status: f.Status, Add: f.Additions, Del: f.Deletions})
			}
		}
		mb, _ := json.Marshal(missing)
		followUp := fmt.Sprintf(`You are an expert code reviewer producing a guided review plan.
Return ONLY valid JSON: {"steps":[{"path":"...","rationale":"- Intent: ...\n- Dependencies: ...\n- Verify: ...\n- Risk: ...","focus":["specific check"],"risk":"low|medium|high"}]}
Rules:
- Include exactly one step for every file below.
- rationale must be exactly four bullet lines with prefixes "- Intent: ", "- Dependencies: ", "- Verify: ", "- Risk: ", each under 20 words.
- Output MUST start with { and end with }: no prose, no markdown, no code fences.
Files (JSON): %s`, string(mb))
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
	for i := range steps {
		steps[i].Order = i + 1
	}
	if strings.TrimSpace(string(wp.Summary)) == "" {
		wp.Summary = flexString(heuristicPlan(pr).Summary)
	}
	steps = reorderStepsByCluster(steps, clusters)
	plan := models.ReviewPlan{Summary: string(wp.Summary), Steps: steps}
	if len(plan.Steps) == 0 {
		return heuristicPlan(pr), nil
	}
	return &plan, nil
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
	EstMinutes int           `json:"est_minutes"`
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
	return found
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
		steps = append(steps, models.ReviewStep{
			Path:       f.Path,
			Rationale:  buildRationale(parts, f),
			Focus:      compactFocus(w.Focus),
			Risk:       level,
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
