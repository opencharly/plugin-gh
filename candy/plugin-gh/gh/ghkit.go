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
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const DefaultBaseURL = "https://api.github.com"

type Client struct {
	BaseURL     string
	Token       string
	tokenSource string
	HTTP        *http.Client
	// cache is the HTTP response cache (the shared spec/cache ArtifactStore). nil only on
	// a Client built directly by a test that wants to bypass caching.
	cache *responseCache
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
		// HTTP carries NO total bound; every path states its OWN bound explicitly (see the
		// read policy below). A total client timeout cannot distinguish "one slow attempt" from
		// "a stall worth retrying", which is exactly what made a transient upstream stall a hard
		// failure of an idempotent read (opencharly/plugin-gh#10).
		HTTP:  &http.Client{},
		cache: newResponseCache(),
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
// The read is served through the response cache (ETag-revalidated), so a repeat
// read of an unchanged resource costs no body fetch.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	_, err := c.getCachedJSON(ctx, path, nil, out)
	return err
}

// Post performs a POST with a JSON body; non-2xx surfaces status + body. A
// marshal failure is a real error (never a silent empty body).
func (c *Client) Post(ctx context.Context, path string, body any, out any) error {
	_, err := c.requestJSON(ctx, http.MethodPost, path, body, out)
	return err
}

// requestJSON is the WRITE/uncached JSON path — Post and any non-GET route — so
// auth, the API-version header, the read bound and the non-2xx status+body
// surfacing live in exactly one place (R3). Cached GETs go through getCachedJSON
// instead (a GET has an ETag to revalidate; a POST does not). A non-nil `out`
// decodes the body; a non-nil `body` is marshalled (a marshal error is returned).
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

// request is the ONE UNCONDITIONAL HTTP path: it sets auth + the API-version
// header, bounds the read, surfaces a non-2xx status + body, and returns the raw
// body. Post and the raw-diff read go through it; a cacheable GET goes through
// getCached (which adds the conditional-header + store logic).
func (c *Client) request(ctx context.Context, method, path string, body io.Reader, hasBody bool, accept string) ([]byte, error) {
	resp, err := c.do(ctx, method, path, body, hasBody, accept, "")
	if err != nil {
		return nil, err
	}
	return readResponse(resp, path)
}

// HTTPError is a non-2xx response, carrying the status so a caller can branch on
// it (e.g. a 404 probing whether a number is a PR) without string-matching the
// message. The message still carries the status + body (the diagnostics contract).
type HTTPError struct {
	Status int
	Path   string
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("gh: %s: HTTP %d: %s", e.Path, e.Status, truncate(e.Body, 300))
}

// IsNotFound reports whether err is a 404 from the GitHub API — the ONE probe
// contract an auto-detect branch needs (a number that is not a PR).
func IsNotFound(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == http.StatusNotFound
}

// readResponse is the ONE bounded-read + non-2xx-surfacing helper every response
// path shares (R3): it reads the body under the memory bound and returns an
// HTTPError for any non-2xx. The caller owns closing resp.Body.
func readResponse(resp *http.Response, path string) ([]byte, error) {
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("gh: %s: read: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{Status: resp.StatusCode, Path: path, Body: string(b)}
	}
	return b, nil
}

// --- the READ retry policy (opencharly/plugin-gh#10) --------------------------------------------
//
// THE POLICY, in one place because it is a policy and not a tuning knob:
//
//   - Only IDEMPOTENT GETs retry. A write (POST today, and any future PUT/PATCH/DELETE) is issued
//     EXACTLY ONCE, with the same single bound as before: a retry there could duplicate a side
//     effect, which is the R4 line this package will not cross by accident.
//   - A read gets `readAttemptTimeout` PER ATTEMPT (not a total), up to `readMaxAttempts`
//     attempts, inside ONE `readTotalBudget` so a caller never waits unboundedly.
//   - Only genuinely transient outcomes retry: a transport error, a timeout, 429 (honouring
//     `Retry-After`), or 502/503/504. A 4xx verdict and a 2xx answer are never retried — a "no"
//     is an answer, not a stall.
//   - Backoff is exponential with FULL JITTER (a random point in [0, min(cap, base*2^n))), so a
//     fleet of readers does not resynchronise onto the same retry instant.
//   - The attempt count is surfaced IN THE ERROR when the read finally fails, so a caller can see
//     that a stall was absorbed by policy rather than by luck. It is deliberately NOT written into
//     provenance: that is a wire-visible contract owned elsewhere, and a diagnostic must not become
//     one by accident.
//
// They are package-level VARS rather than consts so the policy is deterministic under test — the
// same seam pattern the retention engine uses for its host probes. Production never writes them;
// a test that does restores them.
var (
	// readAttemptTimeout bounds ONE read attempt (connect + headers + body).
	readAttemptTimeout = 20 * time.Second
	// readTotalBudget bounds EVERY attempt plus every backoff of one read.
	readTotalBudget = 90 * time.Second
	// readMaxAttempts is the ceiling on attempts per read (1 initial + 3 retries).
	readMaxAttempts = 4
	// readBackoffBase/readBackoffCap bound the exponential backoff window.
	readBackoffBase = 250 * time.Millisecond
	readBackoffCap  = 4 * time.Second
	// writeAttemptTimeout is the SINGLE bound a mutating request keeps — the behavioural contract
	// this change does not touch.
	writeAttemptTimeout = 60 * time.Second
)

// retryableStatus reports whether an HTTP status is a transient stall worth another attempt.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// retryAfterDelay parses a Retry-After header (delta-seconds form) into a backoff, capped by the
// remaining budget; an absent or unparseable header returns 0 (use the jittered backoff).
func retryAfterDelay(h string, remaining time.Duration) time.Duration {
	if h == "" {
		return 0
	}
	secs, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || secs < 0 {
		return 0
	}
	d := time.Duration(secs) * time.Second
	if d > remaining {
		return remaining
	}
	return d
}

// backoffDelay returns the delay before attempt n+1: full jitter inside an exponentially growing
// window, capped, and never longer than the remaining budget.
func backoffDelay(attempt int, remaining time.Duration) time.Duration {
	win := readBackoffBase << (attempt - 1)
	if win > readBackoffCap {
		win = readBackoffCap
	}
	if win > remaining {
		win = remaining
	}
	if win <= 0 {
		return 0
	}
	n, err := crand.Int(crand.Reader, big.NewInt(int64(win)))
	if err != nil {
		return win / 2 // no jitter source: a deterministic midpoint keeps the bound honest
	}
	return time.Duration(n.Int64())
}

// sleepCtx waits for d, or returns early when the context is done. It is a BACKOFF, not a
// synchronisation primitive: nothing here waits for another actor to catch up (R4).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// do issues ONE authenticated request and returns the raw response (the caller owns the body).
// ifNoneMatch adds the conditional header (empty = unconditional). The ONE request constructor
// every path shares (R3), so auth + the API-version header live in exactly one place.
//
// A GET is routed through doGetWithRetry (the bounded, idempotent-read policy above); every other
// method goes through doOnce — ONE attempt, one bound. Making that split HERE, on the method,
// rather than at each call site is what keeps the write path retry-free as the package grows.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader, hasBody bool, accept, ifNoneMatch string) (*http.Response, error) {
	if method == http.MethodGet {
		return c.doGetWithRetry(ctx, path, accept, ifNoneMatch)
	}
	attemptCtx, cancel := context.WithTimeout(ctx, writeAttemptTimeout)
	resp, err := c.doOnce(attemptCtx, method, path, body, hasBody, accept, ifNoneMatch)
	if err != nil {
		cancel()
		return nil, err
	}
	// Read the body INSIDE the attempt, then release the deadline (releaseAttempt). A request
	// context governs the whole request lifetime INCLUDING the body read, so cancelling it while
	// the caller still holds the raw response fails a SUCCESSFUL write with `context canceled` —
	// which is exactly the regression this path shipped for one review round, and the reason the
	// read path reads in-attempt too. One helper, both paths (R3).
	resp, _, rerr := releaseAttempt(resp)
	cancel()
	if rerr != nil {
		return nil, fmt.Errorf("gh: %s: read: %w", path, rerr)
	}
	return resp, nil
}

// releaseAttempt reads a response body into memory and returns the SAME response carrying those
// bytes as an in-memory body, so an attempt's deadline can be released before the caller touches
// it. `maxBodyBytes` still bounds the read (the memory guard is unchanged), and the response keeps
// its StatusCode/Header for the caller's own status handling.
func releaseAttempt(resp *http.Response) (*http.Response, []byte, error) {
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(b))
	return resp, b, err
}

// doGetWithRetry issues a GET under the read policy: per-attempt bound, bounded retries on
// transient outcomes only, full-jitter backoff, one total budget. The body of a SUCCESSFUL attempt
// is read INSIDE the attempt (so the per-attempt deadline can be released and the connection freed
// before the response is handed back), and the response the caller receives carries those bytes as
// an in-memory body — the caller's readResponse works unchanged.
func (c *Client) doGetWithRetry(ctx context.Context, path, accept, ifNoneMatch string) (*http.Response, error) {
	budgetCtx, cancelBudget := context.WithTimeout(ctx, readTotalBudget)
	defer cancelBudget()
	started := time.Now()
	attempts := 0      // the attempts REALLY made — the error must not claim a ceiling
	var lastStatus int // 0 = no response was ever received (a transport-only failure)
	var lastErr error
	for attempt := 1; attempt <= readMaxAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(budgetCtx, readAttemptTimeout)
		attempts++
		resp, err := c.doOnce(attemptCtx, http.MethodGet, path, nil, false, accept, ifNoneMatch)
		if err != nil {
			cancel()
			lastErr = err
			if !sleepCtx(budgetCtx, backoffDelay(attempt, remainingBudget(budgetCtx))) {
				break
			}
			continue
		}
		resp, _, rerr := releaseAttempt(resp)
		cancel()
		if rerr == nil && !retryableStatus(resp.StatusCode) {
			return resp, nil
		}
		lastStatus = resp.StatusCode
		if rerr != nil {
			lastErr = fmt.Errorf("read: %w", rerr)
		} else {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		remaining := remainingBudget(budgetCtx)
		wait := retryAfterDelay(resp.Header.Get("Retry-After"), remaining)
		if wait == 0 {
			wait = backoffDelay(attempt, remaining)
		}
		if !sleepCtx(budgetCtx, wait) {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("context deadline exceeded")
	}
	// The count is the REAL one (a caller-cancelled read that made a single attempt says so), and a
	// transport-only failure omits a status it never received rather than printing `status 0`.
	if lastStatus != 0 {
		return nil, fmt.Errorf("gh: GET %s: %d attempt(s) in %s (status %d): %w",
			path, attempts, time.Since(started).Round(time.Millisecond), lastStatus, lastErr)
	}
	return nil, fmt.Errorf("gh: GET %s: %d attempt(s) in %s: %w",
		path, attempts, time.Since(started).Round(time.Millisecond), lastErr)
}

// remainingBudget is what is left of the read budget, never negative. It reads the budget context's
// own deadline — the wall-clock start time it used to take was dead at every call site.
func remainingBudget(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d > 0 {
			return d
		}
		return 0
	}
	return readTotalBudget
}

// doOnce is ONE request attempt — the body this package always had, unchanged (auth, the
// API-version header, the conditional header, the single caller-visible error wrap). It is the
// ONLY place a write is ever issued.
func (c *Client) doOnce(ctx context.Context, method, path string, body io.Reader, hasBody bool, accept, ifNoneMatch string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	// Set Authorization ONLY when a token exists. GitHub answers 401 "Bad
	// credentials" to an EMPTY `Bearer ` header — turning an ordinarily-anonymous
	// public read into a hard failure — so an unauthenticated client must OMIT
	// the header entirely (verified against api.github.com: omitted → 200,
	// `Bearer ` → 401). The public-repo-read contract depends on this.
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gh: %s: %w", path, err)
	}
	return resp, nil
}

// nextLink returns the rel="next" URL from a Link header, or "" when there is no
// next page. GitHub's Link header is the AUTHORITATIVE page-count signal (the
// `rel="last"` page number is not stable under concurrent writes, and a short
// page is not a reliable terminator for endpoints whose page size varies), so
// pagination follows rel="next" until it is absent.
func nextLink(linkHeader string) string {
	for _, part := range strings.Split(linkHeader, ",") {
		seg := strings.TrimSpace(part)
		if !strings.Contains(seg, `rel="next"`) {
			continue
		}
		lt := strings.Index(seg, "<")
		gt := strings.Index(seg, ">")
		if lt == -1 || gt == -1 || gt < lt {
			continue
		}
		return seg[lt+1 : gt]
	}
	return ""
}

// relPath strips the API base from an absolute Link URL so it rides the same
// BaseURL+path construction every request uses (and the same cache key).
func (c *Client) relPath(abs string) string {
	return strings.TrimPrefix(abs, c.BaseURL)
}

// requestConditional issues a GET with an optional If-None-Match and returns the
// body, the response ETag, the rel="next" Link target ("" at the last page), and
// whether upstream answered 304 Not Modified (the caller then serves its cached
// body). A non-2xx/304 surfaces status + body.
func (c *Client) requestConditional(ctx context.Context, path, accept, ifNoneMatch string) (body []byte, etag, next string, notModified bool, err error) {
	resp, err := c.do(ctx, http.MethodGet, path, nil, false, accept, ifNoneMatch)
	if err != nil {
		return nil, "", "", false, err
	}
	if resp.StatusCode == http.StatusNotModified {
		_ = resp.Body.Close()
		return nil, resp.Header.Get("ETag"), c.relPath(nextLink(resp.Header.Get("Link"))), true, nil
	}
	b, err := readResponse(resp, path)
	if err != nil {
		return nil, "", "", false, err
	}
	return b, resp.Header.Get("ETag"), c.relPath(nextLink(resp.Header.Get("Link"))), false, nil
}

// getCachedJSON is the cached GET-and-decode: it serves the body from the
// response cache (immutable when components are given, ETag-revalidated
// otherwise) and decodes it. The `cached` return is true when no network body
// was fetched.
func (c *Client) getCachedJSON(ctx context.Context, path string, immutable map[string]string, out any) (cached bool, err error) {
	b, _, fromCache, err := c.getCached(ctx, path, "application/vnd.github+json", immutable)
	if err != nil {
		return false, err
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return false, fmt.Errorf("gh: %s: decode: %w", path, err)
		}
	}
	return fromCache, nil
}

// pageDecode decodes ONE page body and reports how many rows it added plus
// whether the caller has all it needs (done=true stops the walk — the exact
// signal for a bounded listing, so no page is fetched that a client-side filter
// would then discard).
type pageDecode func(b []byte) (added int, done bool, err error)

// getAll pages a list endpoint to completion by following the Link header's
// rel="next" — the AUTHORITATIVE page-count signal. GitHub emits the Link header
// iff there IS a next page (verified against api.github.com: a single-page result
// carries no Link header at all), so "no rel=next" is the exact terminator and
// the ONLY pagination mechanism. No page-count heuristic, no fallback: a server
// that strips Link headers is a broken boundary that must fail loudly, not a
// condition to silently paper over (R4). The walk also stops the instant the
// callback reports done (a bounded listing never fetches pages it will discard).
// Each page rides the ETag-revalidated cache under its OWN URL, so a repeat
// listing of an unchanged page set costs one 304 per page and ZERO body
// re-fetches — and a warm in-process memo replays the whole set with no requests.
func (c *Client) getAll(ctx context.Context, path string, decode pageDecode) error {
	// A hard page ceiling (200 pages = 20,000 rows) so a pathological list can
	// never page forever; every real PR/org listing is far under it.
	const perPage = 100
	const maxPages = 200
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	next := fmt.Sprintf("%s%sper_page=%d", path, sep, perPage)
	for page := 1; page <= maxPages && next != ""; page++ {
		reqPath := next
		b, nextPage, _, err := c.getCached(ctx, reqPath, "application/vnd.github+json", nil)
		if err != nil {
			return err
		}
		_, done, err := decode(b)
		if err != nil {
			return fmt.Errorf("gh: %s: decode: %w", reqPath, err)
		}
		if done {
			return nil
		}
		next = nextPage
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
	Title     string `json:"title"`
	State     string `json:"state"`
	Draft     bool   `json:"draft"`
	Mergeable *bool  `json:"mergeable"`
	HeadSHA   string `json:"head_sha"`
	Base      string `json:"base"`
	Head      string `json:"head"`
	FileCount int    `json:"file_count"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
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
	// SHA is the git blob SHA of the file at the head commit — the immutable
	// coordinate the full content is fetched (and cached) by.
	SHA string `json:"sha"`
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
		Additions  int    `json:"additions"`
		Deletions  int    `json:"deletions"`
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
	m := &PRMeta{Title: raw.Title, State: raw.State, Draft: raw.Draft, Mergeable: raw.Mergeable, HeadSHA: raw.Head.SHA, Base: raw.Base.Ref, Head: raw.Head.Ref, FileCount: raw.ChangedNum, Additions: raw.Additions, Deletions: raw.Deletions}
	return m, nil
}

// PRFiles returns EVERY changed file with its full patch, paginated across the
// API's 100-per-page cap. Each element carries Patch (the file's own diff), so
// a caller can deliver one file per message instead of one monolithic diff —
// the shape that keeps a reasoning model from spiralling on a multi-file diff.
func (c *Client) PRFiles(ctx context.Context, repo string, pr int) ([]PRFile, error) {
	var files []PRFile
	err := c.getAll(ctx, fmt.Sprintf("/repos/%s/pulls/%d/files", repo, pr), func(b []byte) (int, bool, error) {
		var page []struct {
			Filename  string `json:"filename"`
			Status    string `json:"status"`
			Additions int    `json:"additions"`
			Deletions int    `json:"deletions"`
			Patch     string `json:"patch"`
			SHA       string `json:"sha"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return 0, false, err
		}
		for _, f := range page {
			files = append(files, PRFile{
				Path: f.Filename, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions,
				Patch: f.Patch, SHA: f.SHA, NoPatch: f.Patch == "",
			})
		}
		return len(page), false, nil
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
	err := c.getAll(ctx, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, pr), func(b []byte) (int, bool, error) {
		var page []struct {
			ID   int `json:"id"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
			CreatedAt string `json:"created_at"`
			Body      string `json:"body"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return 0, false, err
		}
		for _, cm := range page {
			author := cm.User.Login
			if author == "" {
				author = "unknown"
			}
			out = append(out, PRComment{ID: cm.ID, Author: author, CreatedAt: cm.CreatedAt, Body: cm.Body})
		}
		return len(page), false, nil
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
	b, _, _, err := c.getCached(ctx, fmt.Sprintf("/repos/%s/pulls/%d", repo, pr), "application/vnd.github.diff", nil)
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
	err := c.getAll(ctx, fmt.Sprintf("/repos/%s/pulls/%d/commits", repo, pr), func(b []byte) (int, bool, error) {
		var page []commitWire
		if err := json.Unmarshal(b, &page); err != nil {
			return 0, false, err
		}
		raw = append(raw, page...)
		return len(page), false, nil
	})
	if err != nil {
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
	err := c.getAll(ctx, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, pr), func(b []byte) (int, bool, error) {
		var page []struct {
			Body string `json:"body"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return 0, false, err
		}
		comments = append(comments, page...)
		return len(page), false, nil
	})
	if err != nil {
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
