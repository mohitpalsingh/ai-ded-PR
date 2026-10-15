package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/mohit/ai-ded-pr/internal/core/diff"
	"github.com/mohit/ai-ded-pr/internal/core/models"
	"github.com/mohit/ai-ded-pr/internal/hosts"
)

type Client struct {
	Token   string
	HTTP    *http.Client
	BaseURL string
}

func New(token string) *Client {
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
		if token == "" {
			token = os.Getenv("GH_TOKEN")
		}
	}
	return &Client{Token: token, HTTP: http.DefaultClient, BaseURL: "https://api.github.com"}
}

func (c *Client) Name() string { return "github" }

type ghUser struct {
	Login string `json:"login"`
}

type ghRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type ghPR struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	User    ghUser `json:"user"`
	Base    ghRef  `json:"base"`
	Head    ghRef  `json:"head"`
	HTMLURL string `json:"html_url"`
}

type ghFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
	Status           string `json:"status"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	Patch            string `json:"patch"`
}

func (c *Client) do(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ai-ded-pr")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("github %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return b, nil
}

func (c *Client) LoadPR(ctx context.Context, ref hosts.PRRef) (*models.PullRequest, error) {
	owner, repo := ref.Owner, ref.Repo
	if owner == "" || repo == "" {
		o, r, n, err := parseRef(ref)
		if err != nil {
			return nil, err
		}
		owner, repo = o, r
		ref.Number = n
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, ref.Number)
	b, err := c.do(ctx, path)
	if err != nil {
		return nil, err
	}
	var pr ghPR
	if err := json.Unmarshal(b, &pr); err != nil {
		return nil, err
	}
	// files
	filesPath := fmt.Sprintf("/repos/%s/%s/pulls/%d/files?per_page=100", owner, repo, ref.Number)
	fb, err := c.do(ctx, filesPath)
	if err != nil {
		return nil, err
	}
	var ghFiles []ghFile
	if err := json.Unmarshal(fb, &ghFiles); err != nil {
		return nil, err
	}
	prm := &models.PullRequest{
		Host:    "github",
		Owner:   owner,
		Repo:    repo,
		Number:  pr.Number,
		Title:   pr.Title,
		Body:    pr.Body,
		Author:  pr.User.Login,
		BaseRef: pr.Base.Ref,
		HeadRef: pr.Head.Ref,
		BaseSHA: pr.Base.SHA,
		HeadSHA: pr.Head.SHA,
		URL:     pr.HTMLURL,
	}
	for _, gf := range ghFiles {
		cf := models.ChangedFile{
			Path:      gf.Filename,
			OldPath:   gf.PreviousFilename,
			Status:    gf.Status,
			Additions: gf.Additions,
			Deletions: gf.Deletions,
			Patch:     gf.Patch,
			Binary:    gf.Patch == "",
			Language:  diff.LanguageFromPath(gf.Filename),
		}
		if gf.Patch != "" {
			cf.Hunks = diff.ParsePatch(gf.Patch)
		}
		prm.Files = append(prm.Files, cf)
		prm.TotalAdd += cf.Additions
		prm.TotalDel += cf.Deletions
	}
	return prm, nil
}

var prURLRe = regexp.MustCompile(`github\.com/([^/]+)/([^/]+)/pull/(\d+)`)

func ParsePRURL(raw string) (owner, repo string, number int, err error) {
	m := prURLRe.FindStringSubmatch(raw)
	if m == nil {
		return "", "", 0, fmt.Errorf("not a github PR url: %s", raw)
	}
	n, _ := strconv.Atoi(m[3])
	return m[1], m[2], n, nil
}

func parseRef(ref hosts.PRRef) (string, string, int, error) {
	if ref.URL != "" {
		o, r, n, err := ParsePRURL(ref.URL)
		if err == nil {
			return o, r, n, nil
		}
	}
	if ref.Number > 0 {
		// owner/repo maybe in URL query? fallback
	}
	return "", "", 0, fmt.Errorf("could not resolve github ref")
}

// MakeRefFromURL builds a PRRef from a URL.
func MakeRefFromURL(raw string) (hosts.PRRef, error) {
	o, r, n, err := ParsePRURL(raw)
	if err != nil {
		return hosts.PRRef{}, err
	}
	return hosts.PRRef{Owner: o, Repo: r, Number: n, URL: raw}, nil
}

// unused guard
var _ = url.Values{}
