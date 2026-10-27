package tui

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mohit/ai-ded-pr/internal/core/models"
)

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	boxStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(1, 2)
	statusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
)

func (m *tuiModel) viewWelcome() string {
	var b strings.Builder
	tag := "ai-ded-pr"
	if m.version != "" && m.version != "dev" {
		tag += " v" + m.version
	}
	b.WriteString(titleStyle.Render(tag) + "  guided code review with local Gemma\n\n")
	b.WriteString("Sources:\n")
	b.WriteString("  g  GitHub pull request (URL)\n")
	b.WriteString("  l  Local git diff (repo path)\n")
	b.WriteString("  d  Download / setup local model\n\n")
	b.WriteString("Review keys: n/p next/prev file, j/k scroll, m done, / ask, r replan, g new PR, q quit\n\n")
	if m.status != "" {
		b.WriteString(statusStyle.Render(m.status) + "\n")
	}
	if m.downloading {
		b.WriteString(fmt.Sprintf("Downloading model: %.1f / %.1f MB\n", float64(m.dlDone)/1e6, float64(m.dlTotal)/1e6))
	} else if _, err := os.Stat(m.cfg.LLMModel); err != nil {
		b.WriteString(errStyle.Render("Model not set up yet: press d to download (~2.5GB) or continue with heuristic review.") + "\n")
	} else {
		b.WriteString(okStyle.Render("Model available.") + "\n")
	}
	return boxStyle.Render(b.String())
}

func (m *tuiModel) viewForm() string {
	var b strings.Builder
	if m.sourceMode == 0 {
		b.WriteString(titleStyle.Render("GitHub PR") + "\n\n")
		b.WriteString("PR URL:\n")
		b.WriteString(m.sourceIn.View() + "\n")
	} else {
		b.WriteString(titleStyle.Render("Local diff") + "\n\n")
		b.WriteString("Repo path:\n")
		b.WriteString(m.sourceIn.View() + "\n\n")
		b.WriteString("Base ref:\n")
		b.WriteString(m.baseIn.View() + "\n\n")
		b.WriteString("Head ref:\n")
		b.WriteString(m.headIn.View() + "\n")
	}
	b.WriteString("\nenter load · esc back · tab switch fields\n")
	if m.status != "" {
		b.WriteString(statusStyle.Render(m.status) + "\n")
	}
	return boxStyle.Render(b.String())
}

func (m *tuiModel) viewLoading() string {
	var b strings.Builder
	tag := "ai-ded-pr"
	if m.version != "" && m.version != "dev" {
		tag += " v" + m.version
	}
	b.WriteString(titleStyle.Render(tag) + "  guided code review with local Gemma\n\n")
	b.WriteString(m.spinner.View() + " " + m.loadingMsg + "\n\n")
	if m.pr != nil {
		b.WriteString(statusStyle.Render(fmt.Sprintf("%d files changed, +%d/-%d", len(m.pr.Files), m.pr.TotalAdd, m.pr.TotalDel)) + "\n")
	}
	if m.downloading {
		b.WriteString(fmt.Sprintf("downloading model: %.1f / %.1f MB\n", float64(m.dlDone)/1e6, float64(m.dlTotal)/1e6))
	} else if m.status != "" {
		b.WriteString(statusStyle.Render(m.status) + "\n")
	}
	b.WriteString("\nThe review opens when the plan (order + rationales) is ready.\n")
	b.WriteString("q quit\n")
	return boxStyle.Render(b.String())
}

func (m *tuiModel) viewReview() string {
	if m.pr == nil {
		return "no PR"
	}
	if m.height < 12 {
		m.height = 24
	}

	var helpBox string
	if m.help {
		helpBox = boxStyle.Render(helpText())
		if m.width > 0 && lipgloss.Width(helpBox) > m.width {
			// Terminal narrower than the help box: drop the border and clip.
			helpBox = truncBlock(helpText(), m.width)
		}
		// panes (min 8 rows) + status bar + help box
		if strings.Count(helpBox, "\n")+1+9 > m.height {
			return helpBox // terminal too small for panes + help
		}
	}

	// The panes plus the status bar must never exceed the terminal, otherwise
	// the renderer scrolls the whole view and the diff pane disappears (the
	// bug that made big PRs look unscrollable).
	paneH := m.height - 1 // one line for the status bar
	if helpBox != "" {
		paneH -= strings.Count(helpBox, "\n") + 1
	}
	if paneH < 8 {
		paneH = 8
	}
	leftWidth, rightWidth := m.paneWidths()

	// left: a windowed file list that always keeps the current file visible
	left := m.renderSidebar(leftWidth, paneH)

	// right: diff viewport fills the pane instead of leaving rows unused
	m.viewport.Width = rightWidth
	m.viewport.Height = paneH
	right := m.viewport.View()

	// layout
	leftBox := lipgloss.NewStyle().Width(leftWidth).Height(paneH).Render(left)
	rightBox := lipgloss.NewStyle().Width(rightWidth).Height(paneH).Render(right)
	gutter := lipgloss.NewStyle().Foreground(lipgloss.Color("238")).Render(
		strings.TrimSuffix(strings.Repeat(" │ \n", paneH), "\n"))
	main := lipgloss.JoinHorizontal(lipgloss.Top, leftBox, gutter, rightBox)

	out := main + "\n" + m.statusBar()
	if helpBox != "" {
		out += "\n" + helpBox
	}
	return out
}

// renderSidebar renders the file list with a fixed height. The list scrolls
// around the current file, so a PR with hundreds of files can never push the
// diff pane off-screen; overflow is signalled with ↑/↓ counters.
func (m *tuiModel) renderSidebar(width, height int) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(trunc(fmt.Sprintf("%s#%d", m.pr.Repo, m.pr.Number), width)) + "\n")
	b.WriteString(statusStyle.Render(trunc(m.pr.Title, width)) + "\n\n")

	// Rows reserved below the list: blank, progress, status.
	listH := height - 6
	if listH < 0 {
		listH = 0
	}
	var items []string
	if m.plan != nil {
		for i, st := range m.plan.Steps {
			prefix := "  "
			if i == m.current {
				prefix = "> "
			}
			line := trunc(fmt.Sprintf("%s%s %d %s", prefix, statusIcon(st.Status), st.Order, baseName(st.Path)), width)
			if i == m.current {
				line = lipgloss.NewStyle().Bold(true).Render(line)
			}
			items = append(items, line)
		}
	}
	for _, line := range windowLines(items, m.current, listH) {
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
	done, total := progressCounts(m.plan)
	b.WriteString(statusStyle.Render(fmt.Sprintf("progress: %d/%d", done, total)) + "\n")
	b.WriteString(statusStyle.Render(trunc(m.status, width)))
	return b.String()
}

// truncBlock truncates every line to width columns, for terminals narrower
// than the help box.
func truncBlock(s string, width int) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = trunc(ln, width)
	}
	return strings.Join(lines, "\n")
}

// statusBar renders the footer. It is truncated to the terminal width so a
// narrow window never wraps the footer into an extra row (which would scroll
// the view and clip the panes).
func (m *tuiModel) statusBar() string {
	plain := "n/p file · j/k scroll"
	if m.viewport.TotalLineCount() > m.viewport.Height {
		plain += fmt.Sprintf(" · %d%%", int(m.viewport.ScrollPercent()*100))
	}
	plain += " · m done · s skip · / ask · r replan · ? help · q quit"
	if m.width > 0 && lipgloss.Width(plain) > m.width {
		plain = trunc(plain, m.width)
	}
	return statusStyle.Render(plain)
}

// windowLines returns at most height lines centred on current, with one-line
// "+N more" markers at whichever ends are cut off. When the list fits it is
// returned untouched.
func windowLines(lines []string, current, height int) []string {
	n := len(lines)
	if height <= 0 || n == 0 {
		return nil
	}
	if n <= height {
		return lines
	}
	if height == 1 {
		return []string{lines[clampInt(current, 0, n-1)]}
	}
	// Two markers are ideal, but when the window sits at an edge only one is
	// needed — reclaim the spare row for another file.
	topReserve := 2
	if height < 3 {
		topReserve = height - 1
	}
	for reserve := topReserve; reserve >= 1; reserve-- {
		inner := height - reserve
		if inner < 1 {
			inner = 1
		}
		start := clampInt(current-inner/2, 0, n-inner)
		end := start + inner
		top, bottom := start > 0, end < n
		need := 0
		if top {
			need++
		}
		if bottom {
			need++
		}
		if need != reserve {
			continue
		}
		out := make([]string, 0, height)
		if top {
			out = append(out, statusStyle.Render(fmt.Sprintf("  ↑ %d more", start)))
		}
		out = append(out, lines[start:end]...)
		if bottom {
			out = append(out, statusStyle.Render(fmt.Sprintf("  ↓ %d more", n-end)))
		}
		return out
	}
	return lines[n-height:]
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (m *tuiModel) viewQuery() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Ask about this PR") + "\n\n")
	b.WriteString(m.queryIn.View() + "\n\n")
	if len(m.queryResults) > 0 {
		b.WriteString("Relevant files:\n")
		for _, r := range m.queryResults {
			b.WriteString("  - " + r + "\n")
		}
	}
	b.WriteString("\nenter ask · esc back\n")
	if m.status != "" {
		b.WriteString(statusStyle.Render(m.status) + "\n")
	}
	return boxStyle.Render(b.String())
}

// paneWidths returns the sidebar and diff-pane widths for the current
// terminal size; both the layout and the viewport wrapping use the same math.
// The two panes plus their separator must always fit inside the terminal, or
// the renderer wraps a line and scrolls the whole view (which is how the diff
// pane used to disappear on narrow windows).
func (m *tuiModel) paneWidths() (int, int) {
	width := m.width
	if width <= 0 {
		width = 100
	}
	leftWidth := 34
	switch {
	case width < 70:
		leftWidth = width / 3
	case width < 90:
		leftWidth = 30
	}
	if leftWidth < 16 {
		leftWidth = 16
	}
	rightWidth := width - leftWidth - 5 // 3 for the gutter, 2 for the right margin
	if rightWidth < 20 {
		rightWidth = 20
		if rightWidth > width-leftWidth-2 {
			leftWidth = width - rightWidth - 2
			if leftWidth < 8 {
				leftWidth = 8
				rightWidth = width - leftWidth - 2
			}
		}
	}
	if rightWidth < 8 {
		rightWidth = 8
	}
	return leftWidth, rightWidth
}

func progressCounts(p *models.ReviewPlan) (int, int) {
	if p == nil {
		return 0, 0
	}
	total := len(p.Steps)
	done := 0
	for _, s := range p.Steps {
		if s.Status == models.StatusDone || s.Status == models.StatusSkipped {
			done++
		}
	}
	return done, total
}

func baseName(p string) string {
	i := len(p) - 1
	for i >= 0 && p[i] != '/' {
		i--
	}
	return p[i+1:]
}

// trunc shortens s to at most n display columns, appending an ellipsis. It
// counts rendered width (not bytes) so wide runes and ANSI-free unicode names
// cannot overflow the pane.
func trunc(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= n {
		return s
	}
	limit := n - 1
	if limit < 1 {
		limit = 1
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if w+rw > limit {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + "…"
}

func helpText() string {
	return `Keys:
n / p   next / previous file
j / k   scroll diff
m       mark current file done
s       skip current file
u       unmark
1-9     jump to a changed dependency of this file
b       return after a dependency jump
/       ask changed files by natural language
r       regenerate review plan
g       new PR / source
?       toggle help
q       quit`
}

// os.Stat import shim
func init() {}
