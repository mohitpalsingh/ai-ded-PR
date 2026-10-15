package models

// PullRequest is a normalized PR/diff across hosts.
type PullRequest struct {
	Host     string        `json:"host"`
	Owner    string        `json:"owner"`
	Repo     string        `json:"repo"`
	Number   int           `json:"number"`
	Title    string        `json:"title"`
	Body     string        `json:"body"`
	Author   string        `json:"author"`
	BaseRef  string        `json:"base_ref"`
	HeadRef  string        `json:"head_ref"`
	BaseSHA  string        `json:"base_sha"`
	HeadSHA  string        `json:"head_sha"`
	URL      string        `json:"url"`
	Files    []ChangedFile `json:"files"`
	TotalAdd int           `json:"total_add"`
	TotalDel int           `json:"total_del"`
}

type ChangedFile struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"` // added, modified, deleted, renamed
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch,omitempty"`
	Binary    bool   `json:"binary"`
	Language  string `json:"language,omitempty"`
	Hunks     []Hunk `json:"hunks,omitempty"`
}

type DiffLineType string

const (
	DiffContext DiffLineType = "context"
	DiffAdd     DiffLineType = "add"
	DiffRemove  DiffLineType = "remove"
)

type DiffLine struct {
	Type    DiffLineType `json:"type"`
	OldNum  int          `json:"old_num,omitempty"`
	NewNum  int          `json:"new_num,omitempty"`
	Content string       `json:"content"`
}

type Hunk struct {
	OldStart int        `json:"old_start"`
	OldCount int        `json:"old_count"`
	NewStart int        `json:"new_start"`
	NewCount int        `json:"new_count"`
	Header   string     `json:"header"`
	Lines    []DiffLine `json:"lines"`
}

type ReviewStatus string

const (
	StatusPending ReviewStatus = "pending"
	StatusDone    ReviewStatus = "done"
	StatusSkipped ReviewStatus = "skipped"
	StatusCurrent ReviewStatus = "current"
)

type ReviewStep struct {
	Order      int          `json:"order"`
	Path       string       `json:"path"`
	Rationale  string       `json:"rationale"`
	Focus      []string     `json:"focus,omitempty"`
	Risk       string       `json:"risk,omitempty"`
	EstMinutes int          `json:"est_minutes,omitempty"`
	Status     ReviewStatus `json:"status"`
}

type ReviewPlan struct {
	Summary string       `json:"summary"`
	Steps   []ReviewStep `json:"steps"`
}

type FileNote struct {
	Path    string   `json:"path"`
	Summary string   `json:"summary"`
	Issues  []string `json:"issues,omitempty"`
	Verdict string   `json:"verdict,omitempty"`
}
