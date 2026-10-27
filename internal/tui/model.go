package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
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

	shutdownOnce sync.Once

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

	current  int
	listOff  int
	depStack []int
	summary  map[string]string
	// shownPath is the file currently rendered in the diff pane; it lets
	// setDiffContent keep the scroll position when only the status changes.
	shownPath string

	loadingMsg string
	status     string

	queryResults []string
	queryIdx     int

	version string

	width  int
	height int
	help   bool

	downloading bool
	dlDone      int64
	dlTotal     int64
	program     *tea.Program
	autoLoad    bool
}

type prLoadedMsg struct {
	pr  *models.PullRequest
	err error
}
type planReadyMsg struct {
	plan   *models.ReviewPlan
	err    error
	fromAI bool
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
	Version  string
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
		m.version = o.Version
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
	var cmds []tea.Cmd
	if !m.noAI {
		if _, err := os.Stat(m.cfg.LLMModel); err == nil {
			cmds = append(cmds, m.startServerCmd())
		} else {
			cmds = append(cmds, m.downloadModelCmd())
		}
		if _, err := os.Stat(m.cfg.EmbedModel); err == nil {
			cmds = append(cmds, m.startEmbedServerCmd())
		}
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
		srv, err := modelsvc.StartServer(m.ctx, "llama-server", m.cfg.LLMModel, 18973, 8192, false)
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
		_, rw := m.paneWidths()
		m.viewport.Width = rw
		// viewReview refines this when the help box is open; the diff pane
		// otherwise fills the terminal minus the status bar.
		m.viewport.Height = max(8, v.Height-1)
		// re-wrap the pane for the new width
		if m.screen == screenReview {
			m.setDiffContent()
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(v)

	case spinner.TickMsg:
		// Animate only while the loading screen is up; leaving it lets the
		// tick loop drain, so no timer keeps firing for the app's lifetime.
		if m.screen != screenLoading {
			return m, nil
		}
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
		m.plan = nil // a plan from an earlier source must never leak through
		m.state = review.LoadState(v.pr)
		m.depStack = nil
		if m.state.Summaries != nil {
			if m.summary == nil {
				m.summary = map[string]string{}
			}
			for path, raw := range m.state.Summaries {
				// old single-line heuristic placeholders are not summaries
				if strings.Contains(raw, "change (") && !strings.Contains(raw, "\n") {
					continue
				}
				var cf *models.ChangedFile
				for i := range v.pr.Files {
					if v.pr.Files[i].Path == path {
						cf = &v.pr.Files[i]
						break
					}
				}
				// normalize old state entries to the canonical 4-bullet shape
				m.summary[path] = review.FormatRationale(raw, cf)
			}
		}
		// The review screen opens only once the plan is ready. While the model
		// warms up (or downloads) we stay on the loading screen — the user
		// never sees an unordered, placeholder review reshuffle under them.
		switch {
		case m.downloading:
			return m, m.beginLoading("downloading local model (one-time, ~2.5 GB)")
		case m.noAI || m.client != nil:
			return m, tea.Batch(
				m.beginLoading("planning review — ordering files & writing rationales"),
				m.planCmd(),
			)
		default:
			return m, m.beginLoading("starting local Gemma (Metal)")
		}

	case planReadyMsg:
		if v.err != nil {
			m.status = "plan error: " + v.err.Error()
		}
		if v.plan != nil {
			m.plan = v.plan
			m.depStack = nil
			review.ApplyState(m.plan, m.state)
		}
		if m.plan == nil && m.pr != nil {
			p, _ := review.NewPlanner(nil, nil).Plan(context.Background(), m.pr)
			m.plan = p
			review.ApplyState(m.plan, m.state)
		}
		if v.fromAI && m.plan != nil {
			// Persist the AI summary for every file exactly once. Summaries
			// already persisted (loaded above into m.summary) always win, so
			// replans and later sessions never change them.
			for i := range m.plan.Steps {
				s := m.plan.Steps[i]
				if strings.TrimSpace(s.Rationale) == "" {
					continue
				}
				if old, ok := m.summary[s.Path]; ok && strings.Contains(old, "- Intent:") {
					continue
				}
				m.summary[s.Path] = s.Rationale
			}
			m.saveProgress()
		}
		m.current = firstPending(m.plan)
		m.screen = screenReview
		m.setDiffContent()
		return m, nil

	case searchDoneMsg:
		if v.err != nil {
			m.status = "ask error: " + v.err.Error()
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
		return m, nil

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
			// The source is loaded but the plan isn't: plan now, stay on the
			// loading screen until planReadyMsg opens the review.
			if m.pr != nil && m.plan == nil && m.screen == screenLoading {
				m.loadingMsg = "planning review — ordering files & writing rationales"
				return m, m.planCmd()
			}
		} else {
			m.status = "model unavailable: " + v.err.Error()
			// Never strand the user on the loading screen: fall back to the
			// heuristic plan (no model) so the review still opens.
			if m.pr != nil && m.plan == nil && m.screen == screenLoading {
				m.loadingMsg = "model unavailable — using heuristic review order"
				return m, m.planCmd()
			}
		}
		return m, nil

	case downloadProgressMsg:
		m.dlDone = v.downloaded
		m.dlTotal = v.total
		if v.total > 0 {
			m.status = fmt.Sprintf("downloading model... %.0f%%", 100*float64(v.downloaded)/float64(v.total))
		} else {
			m.status = "downloading model..."
		}
		return m, nil

	case downloadDoneMsg:
		m.downloading = false
		if v.err != nil {
			m.status = "download failed: " + v.err.Error()
			// Unblock a waiting review with the heuristic plan.
			if m.pr != nil && m.plan == nil && m.screen == screenLoading {
				m.loadingMsg = "model download failed — using heuristic review order"
				return m, m.planCmd()
			}
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
				m.setDiffContent()
				return m, nil
			}
		case "p", "left":
			if m.current > 0 {
				m.current--
				m.setDiffContent()
				return m, nil
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
		case "1", "2", "3", "4", "5", "6", "7", "8", "9":
			m.jumpToDep(int(key[0] - '1'))
		case "b", "backspace":
			m.jumpBack()
		case "/":
			m.screen = screenQuery
			m.queryIn.SetValue("")
			m.queryIn.Focus()
			return m, nil
		case "r":
			if m.pr != nil {
				if m.planner == nil {
					m.planner = review.NewPlanner(m.client, m.embedClient)
				}
				m.planner.Client = m.client
				return m, tea.Batch(
					m.beginLoading("replanning — ordering files & writing rationales"),
					m.planCmd(),
				)
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
			m.status = "asking..."
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
	ctx := m.ctx
	return func() tea.Msg {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		planner := review.NewPlanner(client, m.embedClient)
		plan, err := planner.Plan(pctx, pr)
		return planReadyMsg{plan: plan, err: err, fromAI: client != nil}
	}
}

func (m *tuiModel) searchCmd(q string) tea.Cmd {
	pr := m.pr
	client := m.client
	ctx := m.ctx
	return func() tea.Msg {
		sctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		planner := review.NewPlanner(client, m.embedClient)
		res := planner.Search(sctx, pr, q)
		return searchDoneMsg{query: q, results: res}
	}
}

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
		if strings.Contains(v, "- Intent:") {
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
	m.shutdownOnce.Do(func() {
		m.cancel()
		if m.server != nil {
			m.server.Stop()
		}
		if m.embedServer != nil {
			m.embedServer.Stop()
		}
	})
}
func (m *tuiModel) SetProgram(p *tea.Program) { m.program = p }

// beginLoading switches to the loading screen and returns a spinner tick so
// the indicator animates. Every long operation (model start, planning,
// replanning) funnels through here; the review screen is entered only when
// the plan is ready, never with placeholder content.
func (m *tuiModel) beginLoading(msg string) tea.Cmd {
	m.loadingMsg = msg
	m.screen = screenLoading
	return m.spinner.Tick
}

func (m *tuiModel) stepPath(i int) string {
	if m.plan == nil || i >= len(m.plan.Steps) || i < 0 {
		return ""
	}
	return m.plan.Steps[i].Path
}

func (m *tuiModel) setDiffContent() {
	if m.pr == nil || m.plan == nil || m.current >= len(m.plan.Steps) {
		m.viewport.SetContent("")
		m.shownPath = ""
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
	fmt.Fprintf(&b, "%s  %s  (+%d/-%d)\n\n", statusIcon(m.plan.Steps[m.current].Status), path, safeAdd(cf), safeDel(cf))
	// One canonical block per file: heading, then the summary (persisted when
	// available, otherwise the plan's rationale) rendered exactly once.
	summary := m.summary[path]
	if summary == "" {
		summary = m.plan.Steps[m.current].Rationale
	}
	b.WriteString("Rationale:\n")
	if summary != "" {
		b.WriteString(summary + "\n")
	}
	if len(m.plan.Steps[m.current].Focus) > 0 || len(m.plan.Steps[m.current].DependsOn) > 0 {
		b.WriteString("\n")
	}
	if len(m.plan.Steps[m.current].Focus) > 0 {
		fmt.Fprintf(&b, "Focus: %s\n", strings.Join(m.plan.Steps[m.current].Focus, "; "))
	}
	if deps := m.plan.Steps[m.current].DependsOn; len(deps) > 0 {
		var parts []string
		for i, d := range deps {
			if i >= 9 {
				break
			}
			parts = append(parts, fmt.Sprintf("[%d] %s", i+1, d))
		}
		fmt.Fprintf(&b, "Deps: %s  (1-%d jump · b return)\n", strings.Join(parts, "  "), len(parts))
	}
	// Wrap prose to the pane width before it reaches the viewport; the
	// viewport clips long lines rather than wrapping them, which used to chop
	// rationale bullets, dependency links and long diff lines mid-way.
	// Wrapping plain text first keeps it ANSI-safe: coloring is applied only
	// after the diff is wrapped.
	w := m.viewport.Width
	content := wrapText(b.String(), w)
	content += "\n--- diff ---\n"
	if cf != nil {
		// Diff lines are hard-wrapped exactly at the pane width: no word
		// reflow, so indentation and spacing in code survive the render.
		content += colorizeDiff(capDiff(cf.Patch), w)
	} else {
		content += "(no diff)"
	}
	m.viewport.SetContent(content)
	// Clamp the offset after the content (or the window) changed size.
	m.viewport.SetYOffset(m.viewport.YOffset)
	// Switching files starts at the top; marking done/skipping or resizing
	// keeps the reader where they were in a long diff.
	if path != m.shownPath {
		m.shownPath = path
		m.viewport.GotoTop()
	}
}

// wrapText wraps each line of plain text to at most limit columns at word
// boundaries, preserving existing newlines. Words wider than the limit are
// hard-split, so nothing is ever chopped. Call before adding ANSI styling.
func wrapText(s string, limit int) string {
	if limit < 8 {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = wrapLine(line, limit)
	}
	return strings.Join(lines, "\n")
}

// wrapLine wraps one line; continuation lines are indented two spaces past
// the original indent so the wrap is visible.
func wrapLine(line string, limit int) string {
	if lipgloss.Width(line) <= limit {
		return line
	}
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" {
		return line
	}
	indent := line[:len(line)-len(trimmed)]
	cont := indent + "  "
	if lipgloss.Width(cont)+10 > limit {
		cont = indent
	}
	indentW, contW := lipgloss.Width(indent), lipgloss.Width(cont)
	// Flatten: words that cannot fit on a fresh line get hard-split.
	maxWord := limit - contW
	if maxWord < 1 {
		maxWord = 1
	}
	var words []string
	for _, w := range strings.Fields(trimmed) {
		for lipgloss.Width(w) > maxWord {
			head, tail := splitAtWidth(w, maxWord)
			if head == "" {
				break
			}
			words = append(words, head)
			w = tail
		}
		if w != "" {
			words = append(words, w)
		}
	}
	var out strings.Builder
	cur := indent
	curW := indentW
	first := true
	for _, w := range words {
		wW := lipgloss.Width(w)
		if curW > indentW && curW+1+wW > limit {
			if !first {
				out.WriteByte('\n')
			}
			out.WriteString(cur)
			first = false
			cur, curW = cont, contW
		} else if curW > indentW {
			cur += " "
			curW++
		}
		cur += w
		curW += wW
	}
	if !first {
		out.WriteByte('\n')
	}
	out.WriteString(cur)
	return out.String()
}

// splitAtWidth splits s at a rune boundary where the visible width first
// reaches limit. Cuts prefer a path separator so long file paths wrap at
// directory boundaries instead of mid-name.
func splitAtWidth(s string, limit int) (string, string) {
	if limit <= 0 {
		return "", s
	}
	used := 0
	cut := -1
	for i, ch := range s {
		cw := lipgloss.Width(string(ch))
		if used+cw > limit {
			cut = i
			break
		}
		used += cw
	}
	if cut < 0 {
		return s, ""
	}
	if j := strings.LastIndexByte(s[:cut], '/'); j >= 0 {
		return s[:j+1], s[j+1:]
	}
	return s[:cut], s[cut:]
}

// maxDiffLines bounds how much of a pathological patch we render, keeping the
// viewport fast and memory flat on absurd files. It must stay well above real
// review sizes: a 1,200-line rewrite is already ~2,400 diff lines, and the old
// 1,500-line cap silently cut the tail of exactly those files (the "large PR
// isn't scrollable" bug).
const maxDiffLines = 20000

// capDiff truncates a patch only past the safety bound, and says so visibly.
func capDiff(patch string) string {
	total := strings.Count(patch, "\n")
	if total <= maxDiffLines {
		return patch
	}
	idx := 0
	for i := 0; i < maxDiffLines; i++ {
		j := strings.IndexByte(patch[idx:], '\n')
		if j < 0 {
			idx = len(patch)
			break
		}
		idx += j + 1
	}
	return patch[:idx] + fmt.Sprintf("\n... (%d more lines not shown — file too large to render)\n", total-maxDiffLines)
}

// jumpToDep moves to the n-th changed dependency of the current file
// (0-based). The previous position is remembered so `b` can return after
// verifying the dependency — no extra model calls, no state rewrites.
func (m *tuiModel) jumpToDep(n int) {
	if m.plan == nil || m.current >= len(m.plan.Steps) {
		return
	}
	deps := m.plan.Steps[m.current].DependsOn
	if len(deps) == 0 {
		m.status = "this file has no changed dependencies to jump to"
		return
	}
	if n >= len(deps) {
		return
	}
	target := -1
	for i := range m.plan.Steps {
		if m.plan.Steps[i].Path == deps[n] {
			target = i
			break
		}
	}
	if target < 0 || target == m.current {
		return
	}
	m.depStack = append(m.depStack, m.current)
	if len(m.depStack) > 20 {
		m.depStack = m.depStack[len(m.depStack)-20:]
	}
	m.current = target
	m.status = fmt.Sprintf("→ dependency %d: %s  (b to return)", n+1, deps[n])
	m.setDiffContent()
}

// jumpBack returns to the file a dependency jump came from.
func (m *tuiModel) jumpBack() {
	if m.plan == nil || len(m.depStack) == 0 {
		m.status = "nothing to return to (1-9 jumps to a changed dependency)"
		return
	}
	m.current = m.depStack[len(m.depStack)-1]
	m.depStack = m.depStack[:len(m.depStack)-1]
	if m.current >= len(m.plan.Steps) {
		m.current = 0
	}
	m.status = "← back to " + m.plan.Steps[m.current].Path
	m.setDiffContent()
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

// wrapCodeLine hard-wraps one diff line at the pane width without reflowing
// it: every character is preserved in order, so code indentation, runs of
// spaces and punctuation survive the render. Tabs are expanded first because
// a terminal would otherwise blow past the measured width.
func wrapCodeLine(line string, limit int) []string {
	line = expandTabs(line, 4)
	if limit < 8 || lipgloss.Width(line) <= limit {
		return []string{line}
	}
	out := make([]string, 0, lipgloss.Width(line)/limit+1)
	for lipgloss.Width(line) > limit {
		head, tail := splitAtWidth(line, limit)
		if head == "" {
			break
		}
		out = append(out, head)
		line = tail
	}
	return append(out, line)
}

// expandTabs turns tabs into spaces using a fixed tab stop, so display width
// matches what the viewport measures.
func expandTabs(s string, tab int) string {
	if tab <= 0 || !strings.ContainsRune(s, '\t') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	col := 0
	for _, r := range s {
		if r == '\t' {
			n := tab - col%tab
			b.WriteString(strings.Repeat(" ", n))
			col += n
			continue
		}
		b.WriteRune(r)
		col += lipgloss.Width(string(r))
	}
	return b.String()
}

// colorizeDiff wraps and colors each diff line. Wrapping happens per line
// before styling so every continuation row keeps the line's color and no
// content is lost to the viewport's horizontal clip.
func colorizeDiff(patch string, width int) string {
	lines := strings.Split(patch, "\n")
	var b strings.Builder
	green := lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	cyan := lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	gray := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	for _, l := range lines {
		var style *lipgloss.Style
		switch {
		case strings.HasPrefix(l, "@@"):
			style = &cyan
		case strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++"):
			style = &green
		case strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "---"):
			style = &red
		case strings.HasPrefix(l, "diff "), strings.HasPrefix(l, "index "), strings.HasPrefix(l, "---"), strings.HasPrefix(l, "+++"):
			style = &gray
		}
		chunks := wrapCodeLine(l, width)
		for i, chunk := range chunks {
			if i > 0 {
				b.WriteByte('\n')
			}
			if style != nil {
				b.WriteString(style.Render(chunk))
			} else {
				b.WriteString(chunk)
			}
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
