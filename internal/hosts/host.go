package hosts

import (
	"context"

	"github.com/mohit/ai-ded-pr/internal/core/models"
)

// Ref identifies a PR on a host.
type PRRef struct {
	Owner  string
	Repo   string
	Number int
	URL    string
}

// Host is a source of pull requests / changes.
type Host interface {
	Name() string
	// LoadPR loads a PR by ref.
	LoadPR(ctx context.Context, ref PRRef) (*models.PullRequest, error)
}
