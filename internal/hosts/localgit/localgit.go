package localgit

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mohit/ai-ded-pr/internal/core/diff"
	"github.com/mohit/ai-ded-pr/internal/core/models"
	"github.com/mohit/ai-ded-pr/internal/hosts"
)

type Adapter struct {
	RepoPath string
	Base     string
	Head     string
}

func New(repoPath, base, head string) *Adapter {
	if base == "" {
		base = "HEAD~1"
	}
	if head == "" {
		head = "HEAD"
	}
	return &Adapter{RepoPath: repoPath, Base: base, Head: head}
}

func (a *Adapter) Name() string { return "local" }

func (a *Adapter) Load(ctx context.Context) (*models.PullRequest, error) {
	// Normalize to an absolute path so persisted review state is keyed per repo.
	if abs, err := filepath.Abs(a.RepoPath); err == nil {
		a.RepoPath = abs
	}
	// validate repo
	if _, err := exec.CommandContext(ctx, "git", "-C", a.RepoPath, "rev-parse", "--is-inside-work-tree").Output(); err != nil {
		return nil, fmt.Errorf("not a git repo: %s", a.RepoPath)
	}
	// resolve SHAs
	baseSHA, _ := gitOut(ctx, a.RepoPath, "rev-parse", a.Base)
	headSHA, _ := gitOut(ctx, a.RepoPath, "rev-parse", a.Head)
	title := fmt.Sprintf("Local diff %s..%s", short(a.Base), short(a.Head))
	// diff
	out, err := exec.CommandContext(ctx, "git", "-C", a.RepoPath, "diff", "--no-color", "--unified=3", a.Base+"..."+a.Head).Output()
	if err != nil {
		out2, err2 := exec.CommandContext(ctx, "git", "-C", a.RepoPath, "diff", "--no-color", "--unified=3", a.Base, a.Head).Output()
		if err2 == nil {
			out = out2
		} else {
			// try working tree diff as last resort
			out3, _ := exec.CommandContext(ctx, "git", "-C", a.RepoPath, "diff", "--no-color", "--unified=3", "HEAD").Output()
			out = out3
		}
	}
	files := diff.ParseUnifiedDiff(string(out))
	// If no changes between refs, try working tree diff
	if len(files) == 0 {
		out2, _ := exec.CommandContext(ctx, "git", "-C", a.RepoPath, "diff", "--no-color", "--unified=3", "HEAD").Output()
		files = diff.ParseUnifiedDiff(string(out2))
		if len(files) > 0 {
			headSHA, _ = gitOut(ctx, a.RepoPath, "rev-parse", "HEAD")
			a.Head = "WORKTREE"
		}
	}
	pr := &models.PullRequest{
		Host:    "local",
		Owner:   "",
		Repo:    filepath.Base(a.RepoPath),
		Number:  0,
		Title:   title,
		Body:    fmt.Sprintf("Local review for %s", a.RepoPath),
		Author:  "local",
		BaseRef: a.Base,
		HeadRef: a.Head,
		BaseSHA: strings.TrimSpace(baseSHA),
		HeadSHA: strings.TrimSpace(headSHA),
		URL:     a.RepoPath,
		Files:   files,
	}
	for _, f := range files {
		pr.TotalAdd += f.Additions
		pr.TotalDel += f.Deletions
	}
	return pr, nil
}

func (a *Adapter) LoadPR(ctx context.Context, ref hosts.PRRef) (*models.PullRequest, error) {
	return a.Load(ctx)
}

func gitOut(ctx context.Context, repo string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	b, err := cmd.Output()
	return string(b), err
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
