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

// maxBodyBytes bounds ONE response body (a memory guard, not a content policy).
// 64 MiB so even a very large PR diff is read whole — a truncated diff would
// silently blind a caller. ONE value, shared by every request path.
const maxBodyBytes = 64 << 20

// Get performs a GET and decodes the JSON body; non-2xx surfaces status + body.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	_, err := c.requestJSON(ctx, http.MethodGet, path, nil, out)
	return err
}

// Post performs a POST with a JSON body; non-2xx surfaces status + body. A
// marshal failure is a real error (never a silent empty body).
func (c *Client) Post(ctx context.Context, path string, body any, out any) error {
	_, err := c.requestJSON(ctx, http.MethodPost, path, body, out)
	return err
}

// requestJSON is the ONE JSON request path: Get/Post and every typed read route
// through it, so auth, the API-version header, the read bound and the non-2xx
// status+body surfacing live in exactly one place (R3). A non-nil `out` decodes
// the body; a non-nil `body` is marshalled (a marshal error is returned).
func (c *Client) requestJSON(ctx context.Context, method, path string, body any, out any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("gh: %s: encode request body: %w", path, err)
		}
		rdr = strings.NewReader(string(b))
	}
	b, err := c.request(ctx, method, path, rdr, body != nil, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return nil, fmt.Errorf("gh: %s: decode: %w", path, err)
		}
	}
	return b, nil
}

// request is the ONE HTTP path: requestJSON (Get/Post), getAll and PRDiff all
// route through it. It sets auth + the API-version header, bounds the read, and
// surfaces a non-2xx status + body, and returns the raw body for a caller that
// needs no JSON decode (the diff media type).
func (c *Client) request(ctx context.Context, method, path string, body io.Reader, hasBody bool, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gh: %s: %w", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("gh: %s: read: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gh: %s: HTTP %d: %s", path, resp.StatusCode, truncate(string(b), 300))
	}
	return b, nil
}

// getAll pages a list endpoint to completion. GitHub caps `per_page` at 100, so
// a list longer than one page MUST be followed (a single page silently drops
// the tail — the very class the review gate's thread fix removed); it pages by
// page-count and stops on the first SHORT page (the same page-count contract
// the API itself exposes, without parsing the Link header). Each page is
// decoded by the caller's callback, which returns the rows added.
func (c *Client) getAll(ctx context.Context, path string, decode func([]byte) (int, error)) error {
	// A hard page ceiling (200 pages = 20,000 rows) so a pathological list can
	// never page forever; every real PR is far under it.
	const maxPages = 200
	for page := 1; page <= maxPages; page++ {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		full := fmt.Sprintf("%s%sper_page=100&page=%d", path, sep, page)
		b, err := c.request(ctx, http.MethodGet, full, nil, false, "application/vnd.github+json")
		if err != nil {
			return err
		}
		added, err := decode(b)
		if err != nil {
			return fmt.Errorf("gh: %s: decode: %w", full, err)
		}
		if added < 100 {
			return nil
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
	// Status is the file's change kind: added | removed | modified | renamed |
	// copied | changed | unchanged.
	Status string `json:"status"`
	// Patch is the file's unified-diff hunk text. Empty when the API returned no
	// patch — which happens for a BINARY file, a rename/copy with no content
	// change, or a file too large to render. `NoPatch` records exactly that, so a
	// caller never has to infer "binary" from an empty string.
	Patch string `json:"patch"`
	// NoPatch is true when the API returned no patch text for this file. It is
	// NOT a synonym for "binary": check Status and the additions/deletions to
	// decide why. A rename-only change has NoPatch==true and is not binary.
	NoPatch bool `json:"-"`
}

// PRComment is one issue-comment with its id (a caller reads a comment BY ID so
// a long thread can be delivered one comment per message).
type PRComment struct {
	ID        int    `json:"id"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
	Body      string `json:"body"`
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

// PRFiles returns EVERY changed file with its full patch, paginated across the
// API's 100-per-page cap. Each element carries Patch (the file's own diff), so
// a caller can deliver one file per message instead of one monolithic diff —
// the shape that keeps a reasoning model from spiralling on a multi-file diff.
func (c *Client) PRFiles(ctx context.Context, repo string, pr int) ([]PRFile, error) {
	var files []PRFile
	err := c.getAll(ctx, fmt.Sprintf("/repos/%s/pulls/%d/files", repo, pr), func(b []byte) (int, error) {
		var page []struct {
			Filename  string `json:"filename"`
			Status    string `json:"status"`
			Additions int    `json:"additions"`
			Deletions int    `json:"deletions"`
			Patch     string `json:"patch"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return 0, err
		}
		for _, f := range page {
			files = append(files, PRFile{
				Path: f.Filename, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions,
				Patch: f.Patch, NoPatch: f.Patch == "",
			})
		}
		return len(page), nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// PRComments lists EVERY issue comment (id + author + date + body), paginated to
// completion. A caller builds a compact index from this and then reads a body by
// id via PRComment, so a long thread is delivered one comment per message.
func (c *Client) PRComments(ctx context.Context, repo string, pr int) ([]PRComment, error) {
	var out []PRComment
	err := c.getAll(ctx, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, pr), func(b []byte) (int, error) {
		var page []struct {
			ID   int `json:"id"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
			CreatedAt string `json:"created_at"`
			Body      string `json:"body"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return 0, err
		}
		for _, cm := range page {
			author := cm.User.Login
			if author == "" {
				author = "unknown"
			}
			out = append(out, PRComment{ID: cm.ID, Author: author, CreatedAt: cm.CreatedAt, Body: cm.Body})
		}
		return len(page), nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PRComment fetches ONE comment by id (the read path that pairs with the thread
// index: a caller reads a comment as its own message, bounded individually).
func (c *Client) PRComment(ctx context.Context, repo string, id int) (*PRComment, error) {
	var raw struct {
		ID   int `json:"id"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		CreatedAt string `json:"created_at"`
		Body      string `json:"body"`
	}
	if err := c.Get(ctx, fmt.Sprintf("/repos/%s/issues/comments/%d", repo, id), &raw); err != nil {
		return nil, err
	}
	author := raw.User.Login
	if author == "" {
		author = "unknown"
	}
	return &PRComment{ID: raw.ID, Author: author, CreatedAt: raw.CreatedAt, Body: raw.Body}, nil
}

// PostComment posts ONE issue comment to a PR. Non-2xx surfaces the status +
// body via request(), so a failed post is never silent.
func (c *Client) PostComment(ctx context.Context, repo string, pr int, body string) error {
	return c.Post(ctx, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, pr), map[string]string{"body": body}, nil)
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

// PRDiff returns the WHOLE PR diff (every file in one text). Callers that must
// deliver the change one file at a time should prefer PRFiles (per-file Patch);
// PRDiff is retained for callers that genuinely want the unified whole.
func (c *Client) PRDiff(ctx context.Context, repo string, pr int) (string, error) {
	b, err := c.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, pr), nil, false, "application/vnd.github.diff")
	if err != nil {
		return "", err
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
