package gh

// document.go — the full issue/PR document reads + assembler. It EXTENDS the
// typed read surface (ghkit.go) with the issue read, the PR reviews + inline
// review comments, and the changed-file CONTENT at head — then assembles the
// structured #GhDocument (schema/gh.cue) in ONE pass from those reads. Every
// read goes through the cached HTTP layer (cache.go), so a document assembled
// twice against an unchanged upstream re-fetches nothing.
//
// SDD: the returned value is the CUE-GENERATED params.GhDocument — the exact
// type #GhDocument generates — never a hand-transcribed schema struct. The
// GitHub API wire shapes are decoded into small local structs (the API shape
// differs from the document shape: user.login → author, etc.).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/opencharly/plugin-gh/candy/plugin-gh/params"
)

// fileContentCap bounds ONE file's full content (a memory/artifact guard, like
// maxBodyBytes for a response). A text file larger than this is recorded with
// truncated + omitted_reason rather than silently emitted huge.
const fileContentCap = 4 << 20 // 4 MiB

// Issue is the issue/PR identity block #GhDocument carries (the fields common to
// an issue and a pull request) — a decode helper for the GitHub issues endpoint.
type Issue struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	State     string   `json:"state"`
	Author    string   `json:"author"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
	Body      string   `json:"body"`
	HTMLURL   string   `json:"html_url"`
	Labels    []string `json:"labels"`
	Assignees []string `json:"assignees"`
}

// Review is one submitted PR review (decode helper).
type Review struct {
	ID          int    `json:"id"`
	Author      string `json:"author"`
	State       string `json:"state"`
	Body        string `json:"body"`
	SubmittedAt string `json:"submitted_at"`
}

// ReviewComment is one inline PR review comment (decode helper).
type ReviewComment struct {
	ID          int    `json:"id"`
	Author      string `json:"author"`
	Path        string `json:"path"`
	Line        int    `json:"line"`
	Side        string `json:"side"`
	CreatedAt   string `json:"created_at"`
	Body        string `json:"body"`
	InReplyToID int    `json:"in_reply_to_id"`
}

// Issue fetches an issue OR pull request's identity block. The same endpoint
// serves both (a PR is an issue with a `pull_request` member), so one read backs
// both document kinds.
func (c *Client) Issue(ctx context.Context, repo string, number int) (*Issue, error) {
	var raw struct {
		Number    int    `json:"number"`
		Title     string `json:"title"`
		State     string `json:"state"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
		Body      string `json:"body"`
		HTMLURL   string `json:"html_url"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Assignees []struct {
			Login string `json:"login"`
		} `json:"assignees"`
	}
	if _, err := c.getCachedJSON(ctx, fmt.Sprintf("/repos/%s/issues/%d", repo, number), nil, &raw); err != nil {
		return nil, err
	}
	labels := make([]string, len(raw.Labels))
	for i, l := range raw.Labels {
		labels[i] = l.Name
	}
	assignees := make([]string, len(raw.Assignees))
	for i, a := range raw.Assignees {
		assignees[i] = a.Login
	}
	author := raw.User.Login
	if author == "" {
		author = "unknown"
	}
	return &Issue{
		Number: raw.Number, Title: raw.Title, State: raw.State, Author: author,
		CreatedAt: raw.CreatedAt, UpdatedAt: raw.UpdatedAt, Body: raw.Body,
		HTMLURL: raw.HTMLURL, Labels: labels, Assignees: assignees,
	}, nil
}

// PRReviews lists every submitted review on a PR, paginated to completion.
func (c *Client) PRReviews(ctx context.Context, repo string, pr int) ([]Review, error) {
	var out []Review
	err := c.getAll(ctx, fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, pr), func(b []byte) (int, error) {
		var page []struct {
			ID   int `json:"id"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
			State       string `json:"state"`
			Body        string `json:"body"`
			SubmittedAt string `json:"submitted_at"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return 0, err
		}
		for _, r := range page {
			author := r.User.Login
			if author == "" {
				author = "unknown"
			}
			out = append(out, Review{ID: r.ID, Author: author, State: r.State, Body: r.Body, SubmittedAt: r.SubmittedAt})
		}
		return len(page), nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PRReviewComments lists every INLINE review comment on a PR, paginated to
// completion (the per-file/hunk comments, distinct from the issue comments).
func (c *Client) PRReviewComments(ctx context.Context, repo string, pr int) ([]ReviewComment, error) {
	var out []ReviewComment
	err := c.getAll(ctx, fmt.Sprintf("/repos/%s/pulls/%d/comments", repo, pr), func(b []byte) (int, error) {
		var page []struct {
			ID   int `json:"id"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
			Path        string `json:"path"`
			Line        int    `json:"line"`
			Side        string `json:"side"`
			CreatedAt   string `json:"created_at"`
			Body        string `json:"body"`
			InReplyToID int    `json:"in_reply_to_id"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return 0, err
		}
		for _, rc := range page {
			author := rc.User.Login
			if author == "" {
				author = "unknown"
			}
			out = append(out, ReviewComment{
				ID: rc.ID, Author: author, Path: rc.Path, Line: rc.Line, Side: rc.Side,
				CreatedAt: rc.CreatedAt, Body: rc.Body, InReplyToID: rc.InReplyToID,
			})
		}
		return len(page), nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// BlobContent fetches a git blob's content by SHA — the IMMUTABLE content read.
// The blob SHA is the content identity, so the read is cached component-keyed
// (SHA) and a repeat read never touches the network.
func (c *Client) BlobContent(ctx context.Context, repo, sha string) ([]byte, error) {
	var raw struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Size     int    `json:"size"`
	}
	if _, err := c.getCachedJSON(ctx, fmt.Sprintf("/repos/%s/git/blobs/%s", repo, sha),
		map[string]string{"repo": repo, "sha": sha}, &raw); err != nil {
		return nil, err
	}
	if raw.Encoding == "base64" {
		// GitHub wraps the base64 in newlines; strip them before decoding.
		clean := strings.ReplaceAll(raw.Content, "\n", "")
		b, err := base64.StdEncoding.DecodeString(clean)
		if err != nil {
			return nil, fmt.Errorf("gh: blob %s: base64 decode: %w", sha, err)
		}
		return b, nil
	}
	return []byte(raw.Content), nil
}

// buildFiles converts the changed-file rows into the document's params.GhFile
// rows, fetching each file's full content at head when includeContent is set.
// The content fetches run through a bounded worker pool (the immutable blob
// reads are independent network calls; a large PR would otherwise serialize).
func (c *Client) buildFiles(ctx context.Context, repo string, files []PRFile, includeContent bool) []params.GhFile {
	out := make([]params.GhFile, len(files))
	for i, f := range files {
		out[i] = params.GhFile{
			Path: f.Path, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions,
			Patch: f.Patch, NoPatch: f.NoPatch, BlobSHA: f.SHA,
		}
	}
	if !includeContent {
		return out
	}
	const workers = 8
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if out[i].BlobSHA == "" {
					continue
				}
				c.fillFileContent(ctx, repo, &out[i])
			}
		}()
	}
	for i := range out {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return out
}

// fillFileContent fetches one file's content into dest, recording is_binary /
// truncated / omitted_reason. A failure is recorded (omitted_reason
// "fetch_failed") rather than propagated: the document still carries the diff.
func (c *Client) fillFileContent(ctx context.Context, repo string, dest *params.GhFile) {
	b, err := c.BlobContent(ctx, repo, dest.BlobSHA)
	if err != nil {
		dest.OmittedReason = "fetch_failed"
		return
	}
	if len(b) > fileContentCap {
		dest.Truncated = true
		dest.OmittedReason = "too_large"
		return
	}
	if isBinary(b) {
		dest.IsBinary = true
		dest.OmittedReason = "binary"
		return
	}
	dest.Content = string(b)
	dest.ContentEncoding = "utf-8"
}

// isBinary reports whether b looks like binary content: invalid UTF-8 OR a NUL
// byte in the first 8 KiB (the git heuristic, bounded).
func isBinary(b []byte) bool {
	head := b
	if len(head) > 8192 {
		head = head[:8192]
	}
	for _, c := range head {
		if c == 0 {
			return true
		}
	}
	return !utf8.Valid(head)
}

// AssembleDocument reads EVERY fact the structured document needs and returns
// the CUE-generated params.GhDocument. target selects issue vs pr; empty
// auto-detects (a number that resolves to a pull request is a PR, else an
// issue). includeContent fetches each changed file's full content at head.
// Every read is required: a failure is a real error (the document must not be
// emitted from a partial read).
func (c *Client) AssembleDocument(ctx context.Context, repo string, number int, target string, includeContent bool) (*params.GhDocument, error) {
	// Snapshot the cache-hit counter so Provenance.Cached reports whether THIS
	// assembly served any response from cache.
	var hitsBefore int
	if c.cache != nil {
		hitsBefore = c.cache.hitsSince()
	}
	issue, err := c.Issue(ctx, repo, number)
	if err != nil {
		return nil, fmt.Errorf("read issue/PR %s#%d: %w", repo, number, err)
	}
	wantPR := target == "pr"
	// Auto-detect: the pulls endpoint 404s for a plain issue — ONLY a 404 means
	// "not a PR"; any other failure (network, 5xx, auth) is REAL and must surface,
	// never silently yield an issue document.
	var prMetaWire *PRMeta
	if target == "" || wantPR {
		m, err := c.PRMeta(ctx, repo, number)
		switch {
		case err == nil:
			prMetaWire = m
		case wantPR:
			return nil, fmt.Errorf("read PR %s#%d: %w", repo, number, err)
		case IsNotFound(err):
			// Confirmed an issue, not a PR: the issue document stands.
		default:
			return nil, fmt.Errorf("probe whether %s#%d is a PR: %w", repo, number, err)
		}
	}

	comments, err := c.PRComments(ctx, repo, number)
	if err != nil {
		return nil, fmt.Errorf("read comment thread: %w", err)
	}
	doc := &params.GhDocument{
		Kind: "issue", Repo: repo, Number: number, URL: issue.HTMLURL,
		Title: issue.Title, State: issue.State, Author: issue.Author,
		CreatedAt: issue.CreatedAt, UpdatedAt: issue.UpdatedAt, Body: issue.Body,
		Labels: issue.Labels, Assignees: issue.Assignees,
		Comments:  make([]params.GhComment, len(comments)),
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		Provenance: params.GhProvenance{
			APIBase:     c.BaseURL,
			TokenSource: c.TokenSource(),
		},
	}
	for i, cm := range comments {
		doc.Comments[i] = params.GhComment{ID: cm.ID, Author: cm.Author, CreatedAt: cm.CreatedAt, Body: cm.Body}
	}
	// finalize stamps the provenance cached flag from the cache-hit delta and
	// returns the doc (the ONE place both return paths share).
	finalize := func() *params.GhDocument {
		if c.cache != nil {
			doc.Provenance.Cached = c.cache.hitsSince() > hitsBefore
		}
		return doc
	}

	if prMetaWire == nil {
		return finalize(), nil
	}
	// PR document: commits, files (+ content), reviews, inline review comments.
	doc.Kind = "pr"
	commits, err := c.PRCommits(ctx, repo, number)
	if err != nil {
		return nil, fmt.Errorf("read commits: %w", err)
	}
	files, err := c.PRFiles(ctx, repo, number)
	if err != nil {
		return nil, fmt.Errorf("read changed files: %w", err)
	}
	reviews, err := c.PRReviews(ctx, repo, number)
	if err != nil {
		return nil, fmt.Errorf("read reviews: %w", err)
	}
	reviewComments, err := c.PRReviewComments(ctx, repo, number)
	if err != nil {
		return nil, fmt.Errorf("read review comments: %w", err)
	}
	docPR := &params.GhPR{
		HeadSHA: prMetaWire.HeadSHA, BaseRef: prMetaWire.Base, HeadRef: prMetaWire.Head,
		Draft: prMetaWire.Draft, Mergeable: prMetaWire.Mergeable,
		Additions: prMetaWire.ChangedSum, ChangedFiles: prMetaWire.FileCount,
		Commits:        make([]params.GhCommit, len(commits)),
		Reviews:        make([]params.GhReview, len(reviews)),
		ReviewComments: make([]params.GhReviewComment, len(reviewComments)),
	}
	for i, cm := range commits {
		docPR.Commits[i] = params.GhCommit{SHA: cm.SHA, Message: cm.Message, Author: cm.Author}
	}
	for i, r := range reviews {
		docPR.Reviews[i] = params.GhReview{ID: r.ID, Author: r.Author, State: r.State, Body: r.Body, SubmittedAt: r.SubmittedAt}
	}
	for i, rc := range reviewComments {
		comment := params.GhReviewComment{
			ID: rc.ID, Author: rc.Author, Path: rc.Path, Line: rc.Line, Side: rc.Side,
			CreatedAt: rc.CreatedAt, Body: rc.Body,
		}
		if rc.InReplyToID != 0 {
			reply := rc.InReplyToID
			comment.InReplyToID = &reply
		}
		docPR.ReviewComments[i] = comment
	}
	docPR.Files = c.buildFiles(ctx, repo, files, includeContent)
	doc.PR = docPR
	return finalize(), nil
}
