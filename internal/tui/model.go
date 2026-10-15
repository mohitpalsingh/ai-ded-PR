package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mohit/ai-ded-pr/internal/config"
	"github.com/mohit/ai-ded-pr/internal/core/models"
	"github.com/mohit/ai-ded-pr/internal/core/review"
	"github.com/mohit/ai-ded-pr/internal/hosts/github"
	"github.com/mohit/ai-ded-pr/internal/hosts/localgit"
	"github.com/mohit/ai-ded-pr/internal/modelsvc"
)

type screen int

const (
	screenWelcome screen = iota
	screenForm
	screenLoading
	screenReview
	screenQuery
)

type tuiModel struct {
	cfg    config.Config
	ctx    context.Context
	cancel context.CancelFunc

	noAI        bool
	client      *modelsvc.Client
	server      *modelsvc.Server
	embedClient *modelsvc.Client
	embedServer *modelsvc.Server

	spinner  spinner.Model
	sourceIn textinput.Model
	baseIn   textinput.Model
	headIn   textinput.Model
	queryIn  textinput.Model
	viewport viewport.Model

	screen     screen
	sourceMode int // 0 github 1 local

	pr      *models.PullRequest
	plan    *models.ReviewPlan
	state   *review.State
	planner *review.Planner

	current int
	listOff int
	summary map[string]string

	loadingMsg string
	status     string

	queryResults []string
	queryIdx     int

	width  int
	height int
	help   bool

	downloading bool
	dlDone      int64
	dlTotal     int64
	program     *tea.Program
	autoLoad    bool

	// summary channel tracking not needed
}

type prLoadedMsg struct {
	pr  *models.PullRequest
	err error
}
type planReadyMsg struct {
	plan *models.ReviewPlan
	err  error
}
type summaryMsg struct {
	path    string
	summary string
	fromAI  bool
}
type searchDoneMsg struct {
	query   string
	results []string
	err     error
}
type serverReadyMsg struct {
	client      *modelsvc.Client
	server      *modelsvc.Server
	embedClient *modelsvc.Client
	embedServer *modelsvc.Server
	err         error
}
type embedReadyMsg struct {
	client *modelsvc.Client
	server *modelsvc.Server
	err    error
}

type downloadProgressMsg struct {
	downloaded int64
	total      int64
}
type downloadDoneMsg struct {
	err error
}
type modelMissingMsg struct{}

type Options struct {
	Mode     int
	Source   string
	Base     string
	Head     string
	AutoLoad bool
}

func New(cfg config.Config, noAI bool, opts ...Options) *tuiModel {
	ctx, cancel := context.WithCancel(context.Background())
	m := &tuiModel{
		cfg:     cfg,
		ctx:     ctx,
		cancel:  cancel,
		noAI:    noAI,
		screen:  screenWelcome,
		summary: map[string]string{},
		spinner: spinner.New(),
	}
	m.spinner.Spinner = spinner.Dot
	m.spinner.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
	m.sourceIn = textinput.New()
	m.sourceIn.Placeholder = "https://github.com/owner/repo/pull/123  OR  /path/to/repo"
	m.sourceIn.Width = 60
	m.baseIn = textinput.New()
	m.baseIn.Placeholder = "base (default HEAD~1)"
	m.baseIn.Width = 30
	m.headIn = textinput.New()
	m.headIn.Placeholder = "head (default HEAD)"
	m.headIn.Width = 30
	m.queryIn = textinput.New()
	m.queryIn.Placeholder = "ask about this PR: e.g. which files touch auth?"
	m.queryIn.Width = 60
	m.viewport = viewport.New(80, 20)
	if len(opts) > 0 {
		o := opts[0]
		m.sourceMode = o.Mode
		if o.Source != "" {
			m.sourceIn.SetValue(o.Source)
		}
		if o.Base != "" {
			m.baseIn.SetValue(o.Base)
		}
		if o.Head != "" {
			m.headIn.SetValue(o.Head)
		}
		m.screen = screenForm
		m.sourceIn.Focus()
		m.autoLoad = o.AutoLoad
	}
	return m
}

func (m *tuiModel) Init() tea.Cmd {
	if m.noAI {
		return nil
	}
	var cmds []tea.Cmd
	if _, err := os.Stat(m.cfg.LLMModel); err == nil {
		cmds = append(cmds, m.startServerCmd())
	} else {
		cmds = append(cmds, m.downloadModelCmd())
	}
	if _, err := os.Stat(m.cfg.EmbedModel); err == nil {
		cmds = append(cmds, m.startEmbedServerCmd())
	}
	if m.autoLoad {
		if m.sourceIn.Value() != "" || m.sourceMode == 1 {
			cmds = append(cmds, m.loadPRCmd())
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

func (m *tuiModel) startServerCmd() tea.Cmd {
	return func() tea.Msg {
		srv, err := modelsvc.StartServer(m.ctx, "llama-server", m.cfg.LLMModel, 18973, 4096, false)
		if err != nil {
			return serverReadyMsg{err: err}
		}
		client := modelsvc.NewClient(srv.URL)
		// wait healthy
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if client.Healthy(m.ctx) {
				return serverReadyMsg{client: client, server: srv}
			}
			time.Sleep(500 * time.Millisecond)
		}
		srv.Stop()
		return serverReadyMsg{err: fmt.Errorf("model server not healthy")}
	}
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = v.Width
		m.height = v.Height
		m.viewport.Width = max(40, v.Width-40)
		m.viewport.Height = max(10, v.Height-12)
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(v)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(v)
		return m, cmd

	case prLoadedMsg:
		if v.err != nil {
			m.screen = screenForm
			m.status = "load error: " + v.err.Error()
			return m, nil
		}
		m.pr = v.pr
		m.state = review.LoadState(v.pr)
		if m.state.Summaries != nil {
			if m.summary == nil {
				m.summary = map[string]string{}
			}
			for k, v := range m.state.Summaries {
				if strings.Contains(v, "change (") && !strings.Contains(v, "\n") {
					continue
				}
				m.summary[k] = v
			}
		}
		m.loadingMsg = "planning review order..."
		m.screen = screenLoading
		return m, tea.Batch(m.spinner.Tick, m.planCmd())

	case planReadyMsg:
		if v.err != nil {
			m.status = "plan error: " + v.err.Error()
		}
		if v.plan != nil {
			m.plan = v.plan
			review.ApplyState(m.plan, m.state)
		}
		if m.plan == nil && m.pr != nil {
			p, _ := review.NewPlanner(nil, nil).Plan(context.Background(), m.pr)
			m.plan = p
			review.ApplyState(m.plan, m.state)
		}
		m.current = firstPending(m.plan)
		m.screen = screenReview
		m.setDiffContent()
		// load state summaries
		if m.state != nil && m.state.Summaries != nil {
			for k, v := range m.state.Summaries {
				if _, ok := m.summary[k]; !ok {
					m.summary[k] = v
				}
			}
		}
		// Rationale from the plan already provides the guided summary.
		// Only fetch an on-demand deep summary when requested.
		m.setDiffContent()
		return m, nil

	case summaryMsg:
		if v.summary != "" && v.fromAI {
			m.summary[v.path] = v.summary
		}
		if m.stepPath(m.current) == v.path {
			m.setDiffContent()
		}
		m.saveProgress()
		return m, nil

	case searchDoneMsg:
		if v.err != nil {
			m.status = "search error: " + v.err.Error()
		} else {
			m.queryResults = v.results
			if len(v.results) > 0 {
				// jump to first result
				for i, st := range m.plan.Steps {
					if st.Path == v.results[0] {
						m.current = i
						break
					}
				}
				m.status = fmt.Sprintf("found %d relevant files; jumped to %s", len(v.results), v.results[0])
			} else {
				m.status = "no relevant files found"
			}
		}
		m.screen = screenReview
		m.setDiffContent()
		return m, m.fetchSummaryCmd(m.stepPath(m.current))

	case embedReadyMsg:
		if v.err == nil {
			m.embedClient = v.client
			m.embedServer = v.server
			if m.planner != nil {
				m.planner.Embedder = v.client
			}
			m.status = "embeddings ready"
		} else {
			// embeddings optional; don't spam
			_ = v.err
		}
		return m, nil

	case serverReadyMsg:
		if v.err == nil {
			m.client = v.client
			m.server = v.server
			if m.planner == nil {
				m.planner = review.NewPlanner(m.client, m.embedClient)
			} else {
				m.planner.Client = m.client
			}
			m.status = "model ready"
			// If PR already loaded, upgrade heuristic plan with AI + summaries.
			if m.pr != nil && m.plan != nil {
				return m, m.planCmd()
			}
		} else {
			m.status = "model unavailable: " + v.err.Error()
		}
		return m, nil

	case downloadProgressMsg:
		m.dlDone = v.downloaded
		m.dlTotal = v.total
		return m, nil

	case downloadDoneMsg:
		m.downloading = false
		if v.err != nil {
			m.status = "download failed: " + v.err.Error()
			return m, nil
		}
		m.status = "model downloaded; starting..."
		return m, m.startServerCmd()
	}
	return m, nil
}

func (m *tuiModel) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := k.String()

	// global
	if key == "ctrl+c" {
		m.Shutdown()
		return m, tea.Quit
	}
	if key == "q" && m.screen != screenForm && m.screen != screenQuery {
		m.Shutdown()
		return m, tea.Quit
	}

	switch m.screen {
	case screenWelcome:
		switch key {
		case "g":
			m.sourceMode = 0
			m.screen = screenForm
			m.sourceIn.Focus()
		case "l":
			m.sourceMode = 1
			m.screen = screenForm
			m.sourceIn.Focus()
		case "d":
			return m, m.downloadModelCmd()
		case "q":
			m.cancel()
			return m, tea.Quit
		}

	case screenForm:
		switch key {
		case "enter":
			return m, m.loadPRCmd()
		case "esc":
			m.screen = screenWelcome
		case "tab":
			// cycle inputs for local mode
			if m.sourceMode == 1 {
				if m.sourceIn.Focused() {
					m.sourceIn.Blur()
					m.baseIn.Focus()
				} else if m.baseIn.Focused() {
					m.baseIn.Blur()
					m.headIn.Focus()
				} else {
					m.headIn.Blur()
					m.sourceIn.Focus()
				}
			}
		}
		// update inputs
		var cmd tea.Cmd
		if m.sourceIn.Focused() {
			m.sourceIn, cmd = m.sourceIn.Update(k)
		} else if m.baseIn.Focused() {
			m.baseIn, cmd = m.baseIn.Update(k)
		} else if m.headIn.Focused() {
			m.headIn, cmd = m.headIn.Update(k)
		}
		return m, cmd

	case screenLoading:
		// no keys
	case screenReview:
		if key == "?" {
			m.help = !m.help
			return m, nil
		}
		if m.help {
			if key == "esc" {
				m.help = false
			}
			return m, nil
		}
		switch key {
		case "n", "right":
			if m.current < stepCount(m.plan)-1 {
				m.current++
				m.saveProgress()
				m.setDiffContent()
				return m, m.fetchSummaryCmd(m.stepPath(m.current))
			}
		case "p", "left":
			if m.current > 0 {
				m.current--
				m.setDiffContent()
				return m, m.fetchSummaryCmd(m.stepPath(m.current))
			}
		case "m":
			if m.plan != nil && m.current < len(m.plan.Steps) {
				m.plan.Steps[m.current].Status = models.StatusDone
				m.saveProgress()
				m.setDiffContent()
			}
		case "s", "x":
			if m.plan != nil && m.current < len(m.plan.Steps) {
				m.plan.Steps[m.current].Status = models.StatusSkipped
				m.saveProgress()
				m.setDiffContent()
			}
		case "u":
			if m.plan != nil && m.current < len(m.plan.Steps) {
				m.plan.Steps[m.current].Status = models.StatusPending
				m.saveProgress()
				m.setDiffContent()
			}
		case "/":
			m.screen = screenQuery
			m.queryIn.SetValue("")
			m.queryIn.Focus()
			return m, nil
		case "r":
			if m.pr != nil {
				m.loadingMsg = "replanning..."
				m.screen = screenLoading
				if m.planner == nil {
					m.planner = review.NewPlanner(m.client, m.embedClient)
				}
				m.planner.Client = m.client
				return m, m.planCmd()
			}
		case "g":
			m.screen = screenWelcome
		case "up", "k":
			m.viewport.LineUp(1)
		case "down", "j":
			m.viewport.LineDown(1)
		case "pgup":
			m.viewport.ViewUp()
		case "pgdown":
			m.viewport.ViewDown()
		}

	case screenQuery:
		switch key {
		case "enter":
			q := strings.TrimSpace(m.queryIn.Value())
			m.queryIn.Blur()
			if q == "" {
				m.screen = screenReview
				return m, nil
			}
			m.status = "searching..."
			return m, m.searchCmd(q)
		case "esc":
			m.queryIn.Blur()
			m.screen = screenReview
		}
		var cmd tea.Cmd
		m.queryIn, cmd = m.queryIn.Update(k)
		return m, cmd
	}

	return m, nil
}

func (m *tuiModel) startEmbedServerCmd() tea.Cmd {
	return func() tea.Msg {
		srv, err := modelsvc.StartEmbeddingServer(m.ctx, "llama-server", m.cfg.EmbedModel, 18974, 2048)
		if err != nil {
			return embedReadyMsg{err: err}
		}
		client := modelsvc.NewClient(srv.URL)
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if client.Healthy(m.ctx) {
				return embedReadyMsg{client: client, server: srv}
			}
			time.Sleep(500 * time.Millisecond)
		}
		srv.Stop()
		return embedReadyMsg{err: fmt.Errorf("embedding server not healthy")}
	}
}

func (m *tuiModel) loadPRCmd() tea.Cmd {
	mode := m.sourceMode
	src := strings.TrimSpace(m.sourceIn.Value())
	base := strings.TrimSpace(m.baseIn.Value())
	head := strings.TrimSpace(m.headIn.Value())
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 120*time.Second)
		defer cancel()
		if mode == 0 {
			ref, err := github.MakeRefFromURL(src)
			if err != nil {
				return prLoadedMsg{err: err}
			}
			c := github.New("")
			pr, err := c.LoadPR(ctx, ref)
			return prLoadedMsg{pr: pr, err: err}
		}
		ad := localgit.New(src, base, head)
		pr, err := ad.Load(ctx)
		return prLoadedMsg{pr: pr, err: err}
	}
}

func (m *tuiModel) planCmd() tea.Cmd {
	pr := m.pr
	client := m.client
	return func() tea.Msg {
		planner := review.NewPlanner(client, m.embedClient)
		plan, err := planner.Plan(context.Background(), pr)
		return planReadyMsg{plan: plan, err: err}
	}
}

func (m *tuiModel) fetchSummaryCmd(path string) tea.Cmd {
	if path == "" {
		return nil
	}
	if cached, ok := m.summary[path]; ok && cached != "" {
		return func() tea.Msg { return summaryMsg{path: path, summary: cached, fromAI: true} }
	}
	pr := m.pr
	client := m.client
	return func() tea.Msg {
		// find file
		var cf *models.ChangedFile
		for i := range pr.Files {
			if pr.Files[i].Path == path {
				cf = &pr.Files[i]
				break
			}
		}
		if cf == nil {
			return summaryMsg{path: path, summary: ""}
		}
		if client == nil {
			return summaryMsg{path: path, summary: "", fromAI: false}
		}
		planner := review.NewPlanner(client, m.embedClient)
		s := planner.FileSummary(context.Background(), cf)
		fromAI := client != nil && !strings.HasPrefix(strings.TrimSpace(s), "ERROR ")
		return summaryMsg{path: path, summary: s, fromAI: fromAI}
	}
}

func (m *tuiModel) searchCmd(q string) tea.Cmd {
	pr := m.pr
	client := m.client
	return func() tea.Msg {
		planner := review.NewPlanner(client, m.embedClient)
		res := planner.Search(context.Background(), pr, q)
		return searchDoneMsg{query: q, results: res}
	}
}

// placeholders to satisfy compile; replaced in View? We'll adjust.
func pr2stepsPlaceholder() []int { return nil }

func (m *tuiModel) downloadModelCmd() tea.Cmd {
	m.downloading = true
	m.dlDone = 0
	m.dlTotal = 0
	go func() {
		if err := config.EnsureDir(m.cfg); err != nil {
			if m.program != nil {
				m.program.Send(downloadDoneMsg{err: err})
			}
			return
		}
		err := modelsvc.Download(m.ctx, m.cfg.LLMModelURL, m.cfg.LLMModel, func(d, t int64) {
			m.dlDone = d
			m.dlTotal = t
			if m.program != nil {
				m.program.Send(downloadProgressMsg{downloaded: d, total: t})
			}
		})
		// optional: download embedding model for semantic aid (best effort)
		if err == nil {
			if _, e := os.Stat(m.cfg.EmbedModel); e != nil {
				_ = modelsvc.Download(m.ctx, m.cfg.EmbedModelURL, m.cfg.EmbedModel, nil)
			}
		}
		if m.program != nil {
			m.program.Send(downloadDoneMsg{err: err})
		}
	}()
	return nil
}

func (m *tuiModel) saveProgress() {
	if m.plan == nil || m.pr == nil {
		return
	}
	st := m.state
	if st == nil {
		st = review.LoadState(m.pr)
	}
	st.Done = map[string]bool{}
	st.Skipped = map[string]bool{}
	st.Summaries = map[string]string{}
	for k, v := range m.summary {
		if strings.TrimSpace(v) != "" && strings.Contains(v, "\n") {
			st.Summaries[k] = v
		}
	}
	for _, s := range m.plan.Steps {
		if s.Status == models.StatusDone {
			st.Done[s.Path] = true
		}
		if s.Status == models.StatusSkipped {
			st.Skipped[s.Path] = true
		}
	}
	review.SaveState(m.pr, st)
	m.state = st
}

func (m *tuiModel) Shutdown() {
	m.cancel()
	if m.server != nil {
		m.server.Stop()
	}
	if m.embedServer != nil {
		m.embedServer.Stop()
	}
}
func (m *tuiModel) SetProgram(p *tea.Program) { m.program = p }

func (m *tuiModel) stepPath(i int) string {
	if m.plan == nil || i >= len(m.plan.Steps) || i < 0 {
		return ""
	}
	return m.plan.Steps[i].Path
}

func (m *tuiModel) setDiffContent() {
	if m.pr == nil || m.plan == nil || m.current >= len(m.plan.Steps) {
		m.viewport.SetContent("")
		return
	}
	path := m.plan.Steps[m.current].Path
	var cf *models.ChangedFile
	for i := range m.pr.Files {
		if m.pr.Files[i].Path == path {
			cf = &m.pr.Files[i]
			break
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s  (+%d/-%d)\n", statusIcon(m.plan.Steps[m.current].Status), path, safeAdd(cf), safeDel(cf))
	fmt.Fprintf(&b, "Rationale: %s\n", m.plan.Steps[m.current].Rationale)
	if len(m.plan.Steps[m.current].Focus) > 0 {
		fmt.Fprintf(&b, "Focus: %s\n", strings.Join(m.plan.Steps[m.current].Focus, "; "))
	}
	if m.plan.Steps[m.current].Risk != "" {
		fmt.Fprintf(&b, "Risk: %s\n", m.plan.Steps[m.current].Risk)
	}
	b.WriteString("\n--- summary ---\n")
	if s, ok := m.summary[path]; ok && s != "" {
		b.WriteString(s)
	} else {
		// Fall back to the plan rationale so the review is useful immediately.
		if m.plan != nil && m.current < len(m.plan.Steps) {
			b.WriteString(m.plan.Steps[m.current].Rationale)
			if len(m.plan.Steps[m.current].Focus) > 0 {
				b.WriteString("\nFocus: " + strings.Join(m.plan.Steps[m.current].Focus, "; "))
			}
		} else {
			b.WriteString("(loading...)")
		}
	}
	b.WriteString("\n\n--- diff ---\n")
	if cf != nil {
		b.WriteString(colorizeDiff(cf.Patch))
	} else {
		b.WriteString("(no diff)")
	}
	m.viewport.SetContent(b.String())
	m.viewport.GotoTop()
}

func safeAdd(cf *models.ChangedFile) int {
	if cf == nil {
		return 0
	}
	return cf.Additions
}
func safeDel(cf *models.ChangedFile) int {
	if cf == nil {
		return 0
	}
	return cf.Deletions
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func stepCount(p *models.ReviewPlan) int {
	if p == nil {
		return 0
	}
	return len(p.Steps)
}

func firstPending(p *models.ReviewPlan) int {
	if p == nil || len(p.Steps) == 0 {
		return 0
	}
	for i, s := range p.Steps {
		if s.Status == models.StatusPending {
			return i
		}
	}
	return 0
}

func statusIcon(s models.ReviewStatus) string {
	switch s {
	case models.StatusDone:
		return "✓"
	case models.StatusSkipped:
		return "⊘"
	case models.StatusCurrent:
		return "→"
	default:
		return "•"
	}
}

func colorizeDiff(patch string) string {
	lines := strings.Split(patch, "\n")
	var b strings.Builder
	green := lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	cyan := lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	gray := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "@@"):
			b.WriteString(cyan.Render(l))
		case strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++"):
			b.WriteString(green.Render(l))
		case strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "---"):
			b.WriteString(red.Render(l))
		case strings.HasPrefix(l, "diff "), strings.HasPrefix(l, "index "), strings.HasPrefix(l, "---"), strings.HasPrefix(l, "+++"):
			b.WriteString(gray.Render(l))
		default:
			b.WriteString(l)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// View
func (m *tuiModel) View() string {
	switch m.screen {
	case screenWelcome:
		return m.viewWelcome()
	case screenForm:
		return m.viewForm()
	case screenLoading:
		return m.viewLoading()
	case screenReview:
		return m.viewReview()
	case screenQuery:
		return m.viewQuery()
	}
	return ""
}
