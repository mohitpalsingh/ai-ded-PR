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
	leftWidth := 34
	if m.width < 90 {
		leftWidth = 30
	}
	rightWidth := m.width - leftWidth - 4
	if rightWidth < 40 {
		rightWidth = 40
	}
	if m.height < 12 {
		m.height = 24
	}
	// left list
	var left strings.Builder
	left.WriteString(titleStyle.Render(fmt.Sprintf("%s#%d", m.pr.Repo, m.pr.Number)) + "\n")
	left.WriteString(statusStyle.Render(trunc(m.pr.Title, 36)) + "\n\n")
	if m.plan != nil {
		for i, st := range m.plan.Steps {
			prefix := "  "
			if i == m.current {
				prefix = "> "
			}
			icon := statusIcon(st.Status)
			line := fmt.Sprintf("%s%s %d %s", prefix, icon, st.Order, baseName(st.Path))
			if i == m.current {
				line = lipgloss.NewStyle().Bold(true).Render(line)
			}
			left.WriteString(line + "\n")
		}
	}
	left.WriteString("\n")
	done, total := progressCounts(m.plan)
	left.WriteString(statusStyle.Render(fmt.Sprintf("progress: %d/%d", done, total)) + "\n")
	left.WriteString(statusStyle.Render(m.status) + "\n")

	// right: diff viewport
	right := m.viewport.View()

	// layout
	leftBox := lipgloss.NewStyle().Width(leftWidth).Height(m.height - 3).Render(left.String())
	rightBox := lipgloss.NewStyle().Width(rightWidth).Height(m.height - 3).Render(right)
	main := lipgloss.JoinHorizontal(lipgloss.Top, leftBox, "  ", rightBox)

	bar := statusStyle.Render("n/p file · j/k scroll · m done · s skip · / ask · r replan · ? help · q quit")
	out := main + "\n" + bar
	if m.help {
		out += "\n" + boxStyle.Render(helpText())
	}
	return out
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

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
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
