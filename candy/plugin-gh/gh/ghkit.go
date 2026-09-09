// Package ghkit — the CANONICAL GitHub REST client for opencharly plugins.
//
// One implementation of the GitHub read surface (R3): every plugin that needs
// PR data imports THIS package instead of hand-rolling `exec.Command("gh")`
// subprocess calls (RCA 2026.252.2233: the hand-rolled calls swallowed gh's
// stderr, depended on the ambient gh binary + its hosts.yml resolution order,
// and broke opaquely inside executor subprocesses where the operator env does
// not reach).
//
// Token resolution (explicit, testable, first match wins):
//  1. GH_TOKEN / GITHUB_TOKEN env
//  2. the gh CLI's hosts.yml (github.com → oauth_token) — the shared local
//     auth store, so an operator's existing `gh auth login` keeps working
//
// The API base is ALWAYS https://api.github.com unless GITHUB_API_URL
// overrides it — never hosts.yml's "default host", which is a silent,
// undeclared redirect and the source of opaque 404s.
//
// Every non-2xx surfaces the HTTP status AND the response body — a gh call
// that fails as "gh meta failed" with no detail is a defect (RCA 2026.252.2233).
package gh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const DefaultBaseURL = "https://api.github.com"

type Client struct {
	BaseURL     string
	Token       string
	tokenSource string
	HTTP        *http.Client
}

// TokenSource reports where the token came from (the evidence packet names it).
func (c *Client) TokenSource() string { return c.tokenSource }

// New resolves the token + the API base from the documented layers.
func New() (*Client, error) {
	token, src, tokenErr := resolveToken()
	if tokenErr != nil {
		// a MISSING token is not a construction error: public-repo reads work
		// unauthenticated (rate-limited) — the auth failure surfaces AT THE CALL
		// with the HTTP status + body, never as a speculative abort.
		src = "none (public/unauthenticated reads only)"
		token = ""
	}
	base := os.Getenv("GITHUB_API_URL")
	if base == "" {
		base = DefaultBaseURL
	}
	return &Client{
		BaseURL:     strings.TrimRight(base, "/"),
		Token:       token,
		tokenSource: src,
		HTTP:        &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// tokenSource is per-CLIENT (concurrent clients never share state — the
// curLedger lesson, RCA 2026.252.2210).

func resolveToken() (token string, source string, err error) {
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := os.Getenv(k); v != "" {
			return strings.TrimSpace(v), "env:" + k, nil
		}
	}
	return hostsToken()
}

// hostsToken parses the gh CLI's hosts.yml — ONLY the auth token, never the
// default-host redirect (the RCA 2026.252.2233 404 class).
func hostsToken() (string, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("gh: no GH_TOKEN/GITHUB_TOKEN and no HOME for hosts.yml: %w", err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".config", "gh", "hosts.yml"))
	if err != nil {
		return "", "", fmt.Errorf("gh: no GH_TOKEN/GITHUB_TOKEN and no ~/.config/gh/hosts.yml — run 'gh auth login' or set GH_TOKEN: %w", err)
	}
	// minimal indentation-scoped parse: hosts: → github.com: → oauth_token:
	// both real layouts: `hosts: > github.com:` (modern) and a top-level `github.com:`.
	// Only the oauth_token is read - never the default host (the opaque-404 class).
	var inCom bool
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "github.com:":
			inCom = true
		case strings.HasPrefix(t, "oauth_token:"):
			if inCom {
				tok := strings.TrimSpace(strings.TrimPrefix(t, "oauth_token:"))
				tok = strings.Trim(tok, "'")
				if tok != "" {
					return tok, "hosts.yml:github.com", nil
				}
			}
		case len(line) > 0 && line[0] != ' ' && line[0] != '-' && !strings.HasPrefix(t, "#") && !strings.HasPrefix(t, "github.com:"):
			inCom = false
		}
	}
	return "", "", fmt.Errorf("gh: no token found (GH_TOKEN/GITHUB_TOKEN unset, hosts.yml has no github.com oauth_token)")
}

// Get performs a GET and decodes the JSON body; non-2xx surfaces status + body.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("gh: %s: %w", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("gh: %s: read: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gh: %s: HTTP %d: %s", path, resp.StatusCode, truncate(string(b), 300))
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("gh: %s: decode: %w", path, err)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ── the typed read surface (the ops the eval lane's agents consume) ──────

type PRMeta struct {
	Title      string `json:"title"`
	State      string `json:"state"`
	Draft      bool   `json:"draft"`
	Mergeable  *bool  `json:"mergeable"`
	HeadSHA    string `json:"head_sha"`
	Base       string `json:"base"`
	Head       string `json:"head"`
	FileCount  int    `json:"file_count"`
	ChangedSum int    `json:"changed_lines"`
}

type PRFile struct {
	Path      string `json:"filename"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

type PRCommit struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Author  string `json:"author"`
}

type PRThread struct {
	Body     string   `json:"body"`
	Comments []string `json:"comments"`
}

func (c *Client) PRMeta(ctx context.Context, repo string, pr int) (*PRMeta, error) {
	var raw struct {
		Title      string `json:"title"`
		State      string `json:"state"`
		Draft      bool   `json:"draft"`
		Mergeable  *bool  `json:"mergeable"`
		ChangedNum int    `json:"changed_files"`
		Head       struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := c.Get(ctx, fmt.Sprintf("/repos/%s/pulls/%d", repo, pr), &raw); err != nil {
		return nil, err
	}
	m := &PRMeta{Title: raw.Title, State: raw.State, Draft: raw.Draft, Mergeable: raw.Mergeable, HeadSHA: raw.Head.SHA, Base: raw.Base.Ref, Head: raw.Head.Ref, FileCount: raw.ChangedNum}
	return m, nil
}

func (c *Client) PRFiles(ctx context.Context, repo string, pr int) ([]PRFile, error) {
	var files []PRFile
	if err := c.Get(ctx, fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100", repo, pr), &files); err != nil {
		return nil, err
	}
	return files, nil
}

// PRPaths returns the changed paths space-separated (the pr-apply contract).
func (c *Client) PRPaths(ctx context.Context, repo string, pr int) (string, error) {
	files, err := c.PRFiles(ctx, repo, pr)
	if err != nil {
		return "", err
	}
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	return strings.Join(paths, " "), nil
}

func (c *Client) PRDiff(ctx context.Context, repo string, pr int) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/pulls/%d", c.BaseURL, repo, pr), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github.diff")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("gh: diff: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("gh: diff: HTTP %d: %s", resp.StatusCode, truncate(string(b), 300))
	}
	return string(b), nil
}

type commitWire struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Name string `json:"name"`
		} `json:"author"`
	} `json:"commit"`
}

func (c *Client) PRCommits(ctx context.Context, repo string, pr int) ([]PRCommit, error) {
	var raw []commitWire
	if err := c.Get(ctx, fmt.Sprintf("/repos/%s/pulls/%d/commits?per_page=100", repo, pr), &raw); err != nil {
		return nil, err
	}
	out := make([]PRCommit, len(raw))
	for i, r := range raw {
		out[i] = PRCommit{SHA: r.SHA, Message: truncate(strings.SplitN(r.Commit.Message, "\n", 2)[0], 120), Author: r.Commit.Author.Name}
	}
	return out, nil
}

func (c *Client) PRThread(ctx context.Context, repo string, pr int) (*PRThread, error) {
	var body struct {
		Body string `json:"body"`
	}
	if err := c.Get(ctx, fmt.Sprintf("/repos/%s/issues/%d", repo, pr), &body); err != nil {
		return nil, err
	}
	var comments []struct {
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := c.Get(ctx, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repo, pr), &comments); err != nil {
		return nil, err
	}
	out := make([]string, len(comments))
	for i, c := range comments {
		out[i] = truncate("@"+c.User.Login+": "+strings.SplitN(c.Body, "\n", 2)[0], 300)
	}
	return &PRThread{Body: body.Body, Comments: out}, nil
}

// HeadSHA — the single head-sha lookup (the freshness + pr-apply contract).
func (c *Client) HeadSHA(ctx context.Context, repo string, pr int) (string, error) {
	var raw struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := c.Get(ctx, fmt.Sprintf("/repos/%s/pulls/%d", repo, pr), &raw); err != nil {
		return "", err
	}
	return raw.Head.SHA, nil
}
