package review

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
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

func descriptiveRationale(f models.ChangedFile) string {
	status := f.Status
	if status == "" {
		status = "change"
	}
	lang := f.Language
	if lang == "" {
		lang = "source"
	}
	return fmt.Sprintf("%s modifies %s (%s, +%d/-%d lines). Review it at this point because its hunks "+
		"introduce behavior that dependent files and tests build on; verify the diff for correctness, "+
		"edge cases, and consistency with the surrounding module.", f.Path, status, lang, f.Additions, f.Deletions)
}

func heuristicRationale(f models.ChangedFile) string {
	l := strings.ToLower(f.Path)
	if strings.Contains(l, "model") || strings.Contains(l, "type") {
		return "Foundational types: review first to understand new shapes."
	}
	if strings.Contains(l, "test") {
		return "Tests: verify expected behavior and edge cases."
	}
	if strings.Contains(l, "config") || strings.HasSuffix(l, ".md") {
		return "Config/docs: low-risk, review last."
	}
	return fmt.Sprintf("Changed %d lines; review with dependent callers in mind.", f.Additions+f.Deletions)
}

func (p *Planner) Plan(ctx context.Context, pr *models.PullRequest) (*models.ReviewPlan, error) {
	if p.Client == nil {
		plan := heuristicPlan(pr)
		p.ensureFileEmbeddings(ctx, pr)
		plan.Steps = reorderStepsByCluster(plan.Steps, semanticClusters(pr, p.fileEmbed))
		return plan, nil
	}
	// Build compact file list
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
		}
		files = append(files, fsummary{Path: f.Path, Status: f.Status, Add: f.Additions, Del: f.Deletions, Hunks: hs})
	}
	p.ensureFileEmbeddings(ctx, pr)
	clusters := semanticClusters(pr, p.fileEmbed)
	fb, _ := json.Marshal(files)
	if len(fb) > 7000 {
		fb = append(fb[:7000], []byte("...")...)
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
{"summary":"2-3 sentence overview of what the PR does and its risk","steps":[{"order":1,"path":"...","rationale":"a concrete 2-3 sentence explanation of what changed in this file, why it must be reviewed at this point, and what a reviewer should verify","focus":["specific check"],"risk":"low|medium|high","est_minutes":2}]}
Rules:
- Rationale must reference concrete code changes (added/removed behavior, symbols, paths) and be at least 25 words. No generic placeholders like "why now".
- Keep semantic clusters together.
- Order files to minimize cognitive load: foundational types/models -> core logic -> callers/API/UI -> tests -> config/docs.
- Path must be one of the changed files.`, pr.Title, truncate(pr.Body, 1500), string(fb), clusterText)

	out, err := p.Client.Chat(ctx, "You only output valid JSON, no markdown.", prompt)
	if err != nil {
		return heuristicPlan(pr), nil
	}
	out = stripCodeFence(out)
	if os.Getenv("AI_DEBUG") != "" {
		fmt.Fprintln(os.Stderr, "RAW JSON: "+out)
	}
	var plan models.ReviewPlan
	parseErr := json.Unmarshal([]byte(out), &plan)
	if parseErr != nil {
		for _, suf := range []string{"}", "}"} {
			if err := json.Unmarshal([]byte(strings.TrimSpace(out)+suf), &plan); err == nil {
				parseErr = nil
				break
			}
		}
	}
	if parseErr != nil {
		if os.Getenv("AI_DEBUG") != "" {
			fmt.Fprintln(os.Stderr, "PARSE ERR: "+parseErr.Error())
		}
		return heuristicPlan(pr), nil
	}
	if os.Getenv("AI_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "PARSED steps=%d summary=%q\n", len(plan.Steps), plan.Summary)
	}
	// validate paths and fill missing
	known := map[string]bool{}
	for _, f := range pr.Files {
		known[f.Path] = true
	}
	// ensure all steps map to known files; drop unknown, fill status
	steps := []models.ReviewStep{}
	seen := map[string]bool{}
	for _, s := range plan.Steps {
		if !known[s.Path] || seen[s.Path] {
			continue
		}
		seen[s.Path] = true
		if s.Status == "" {
			s.Status = models.StatusPending
		}
		r := strings.TrimSpace(s.Rationale)
		if r == "" || strings.EqualFold(r, "why now") {
			var f *models.ChangedFile
			for i := range pr.Files {
				if pr.Files[i].Path == s.Path {
					f = &pr.Files[i]
					break
				}
			}
			if f != nil {
				if p.Client != nil {
					sum := p.FileSummary(ctx, f)
					if sum != "" {
						s.Rationale = sum
					} else {
						s.Rationale = descriptiveRationale(*f)
					}
				} else {
					s.Rationale = descriptiveRationale(*f)
				}
			}
		}
		if strings.TrimSpace(s.Rationale) == "" {
			s.Rationale = heuristicRationale(models.ChangedFile{Path: s.Path})
		}
		rk := strings.ToLower(strings.TrimSpace(s.Risk))
		if rk != "low" && rk != "medium" && rk != "high" {
			s.Risk = "medium"
		} else {
			s.Risk = rk
		}
		steps = append(steps, s)
	}
	// append any missing files
	for _, f := range pr.Files {
		if !seen[f.Path] {
			rat := heuristicRationale(f)
			if p.Client != nil {
				if sum := p.FileSummary(ctx, &f); strings.TrimSpace(sum) != "" {
					rat = sum
				}
			}
			risk := "medium"
			steps = append(steps, models.ReviewStep{Path: f.Path, Rationale: rat, Risk: risk, Status: models.StatusPending})
		}
	}
	for i := range steps {
		steps[i].Order = i + 1
	}
	if plan.Summary == "" {
		plan.Summary = heuristicPlan(pr).Summary
	}
	steps = reorderStepsByCluster(steps, clusters)
	plan.Steps = steps
	if len(plan.Steps) == 0 {
		return heuristicPlan(pr), nil
	}
	return &plan, nil
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

func cleanModelOutput(s string) string {
	s = strings.TrimSpace(s)
	// drop common conversational filler lines
	badPrefixes := []string{"okay, here", "okay here", "here is", "here's", "here are", "let me know", "i hope", "please let", "concise summary", "review bullet"}
	lines := strings.Split(s, "\n")
	var keep []string
	for _, ln := range lines {
		l := strings.ToLower(strings.TrimSpace(ln))
		drop := false
		for _, bp := range badPrefixes {
			if strings.HasPrefix(l, bp) || strings.Contains(l, bp) {
				drop = true
				break
			}
		}
		if strings.Contains(l, "bullet") || strings.Contains(l, "let me know") {
			drop = true
		}
		if drop {
			continue
		}
		keep = append(keep, ln)
	}
	s = strings.TrimSpace(strings.Join(keep, "\n"))
	// remove trailing follow-up sentences
	if i := strings.Index(strings.ToLower(s), "let me know"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	// cap to 6 lines to keep it scannable
	ls := strings.Split(s, "\n")
	if len(ls) > 6 {
		ls = ls[:6]
	}
	return strings.TrimSpace(strings.Join(ls, "\n"))
}

// FileSummary asks LLM for a concise summary of one changed file.
func (p *Planner) FileSummary(ctx context.Context, f *models.ChangedFile) string {
	if p.Client == nil {
		return fmt.Sprintf("%s: %s change (+%d/-%d).", f.Path, f.Status, f.Additions, f.Deletions)
	}
	patch := f.Patch
	if len(patch) > 6000 {
		patch = patch[:6000] + "\n...truncated..."
	}
	prompt := fmt.Sprintf(`Summarize this code change for a reviewer in exactly 4 short bullets. No intro, no outro, no headings. Bullets:
- Intent: what changed
- Dependencies/contracts impacted
- Verify: what reviewer must check
- Risk: key risk
File: %s
DIFF (truncated):
%s`, f.Path, patch)
	out, err := p.Client.Chat(ctx, "You are an expert code reviewer. Output only concise review bullets, no intro/outro.", prompt)
	if err != nil {
		return fmt.Sprintf("%s: %s change (+%d/-%d).", f.Path, f.Status, f.Additions, f.Deletions)
	}
	return cleanModelOutput(out)
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
		// try fallback
		return nil
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
