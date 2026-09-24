package gh

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// TestListIssues_OrgScope pins the org listing: ONE paginated call to
// /orgs/{org}/issues (with the REQUIRED filter=all — the endpoint defaults to
// filter=assigned, which under-lists), every issue AND PR returned, the
// pull_request rows carrying kind=pr + the cheap PR signals, and the repo
// derived from the STRUCTURED repository.full_name (not parsed from a URL).
func TestListIssues_OrgScope(t *testing.T) {
	var gotPath string
	c := cachedTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.URL.Query().Get("filter") != "all" {
			t.Errorf("org listing must send filter=all (default assigned under-lists), got %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`[
			{"number":1,"title":"an issue","state":"open","created_at":"c","updated_at":"u","html_url":"h","comments":3,
			 "repository":{"full_name":"opencharly/charly"},"user":{"login":"alice"},"labels":[{"name":"bug"}]},
			{"number":2,"title":"a PR","state":"open","created_at":"c","updated_at":"u","html_url":"h","comments":0,
			 "repository":{"full_name":"opencharly/plugin-gh"},"user":{"login":"bob"},"draft":true,
			 "pull_request":{"url":"https://api.github.com/repos/opencharly/plugin-gh/pulls/2","merged_at":null}}
		]`))
	})
	idx, err := c.ListIssues(context.Background(), "opencharly", "", "", "", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/orgs/opencharly/issues" {
		t.Fatalf("org path = %q", gotPath)
	}
	if idx.Scope != "org:opencharly" || idx.Count != 2 {
		t.Fatalf("scope/count = %q/%d", idx.Scope, idx.Count)
	}
	issue, pr := idx.Items[0], idx.Items[1]
	if issue.Kind != "issue" || issue.Repo != "opencharly/charly" || issue.CommentCount != 3 || issue.Author != "alice" {
		t.Fatalf("issue row = %+v", issue)
	}
	if len(issue.Labels) != 1 || issue.Labels[0] != "bug" {
		t.Fatalf("labels = %v", issue.Labels)
	}
	if pr.Kind != "pr" || pr.PR == nil || !pr.PR.Draft || pr.PR.MergedAt != "" {
		t.Fatalf("pr row = %+v (pr=%+v)", pr, pr.PR)
	}
	if issue.PR != nil {
		t.Fatalf("an issue row must not carry a pr block: %+v", issue.PR)
	}
}

// TestListIssues_RepoScopeAndKindFilter pins the repo-scoped path (the SAME
// implementation, a different request path), the kind client-side filter, and
// the repo-slug fallback: the REAL repo endpoint has NO `repository` object
// (verified against api.github.com), so the row's repo comes from the requested
// slug — not from a fabricated `repository.full_name`.
func TestListIssues_RepoScopeAndKindFilter(t *testing.T) {
	var gotPath string
	// The real /repos/{owner}/{repo}/issues row shape: no `repository` object.
	body := `[
		{"number":1},
		{"number":2,"pull_request":{"merged_at":null}}
	]`
	c := cachedTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(body))
	})
	idx, err := c.ListIssues(context.Background(), "", "o/r", "", "pr", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/repos/o/r/issues" {
		t.Fatalf("repo path = %q", gotPath)
	}
	if idx.Scope != "repo:o/r" || idx.Kind != "pr" {
		t.Fatalf("scope/kind = %q/%q", idx.Scope, idx.Kind)
	}
	if idx.Count != 1 || idx.Items[0].Kind != "pr" || idx.Items[0].Number != 2 {
		t.Fatalf("kind=pr must keep only the PR row: %+v", idx.Items)
	}
	if idx.Items[0].Repo != "o/r" {
		t.Fatalf("a repo listing must fill the row repo from the requested slug, got %q", idx.Items[0].Repo)
	}
}

// TestListIssues_RequiresExactlyOneScope pins the input contract: neither, or
// both, org and repo is a hard error — never a silent default scope.
func TestListIssues_RequiresExactlyOneScope(t *testing.T) {
	c := cachedTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request may be sent for an invalid scope")
	})
	if _, err := c.ListIssues(context.Background(), "", "", "", "", "", 0, false); err == nil {
		t.Fatal("an empty scope must error")
	}
	if _, err := c.ListIssues(context.Background(), "o", "r", "", "", "", 0, false); err == nil {
		t.Fatal("org AND repo together must error")
	}
}

// TestListIssues_LinkPaginationAndCacheWarm pins the two efficiency properties:
// a >100-item org listing is walked via the Link header to completion, and a
// second listing of an unchanged upstream is served ENTIRELY from cache (no new
// request) — the point of per-page ETag caching.
func TestListIssues_LinkPaginationAndCacheWarm(t *testing.T) {
	var requests atomic.Int32
	c := cachedTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		page := r.URL.Query().Get("page")
		if page == "2" {
			_, _ = w.Write([]byte(`[{"number":101,"title":"last","repository":{"full_name":"o/r"}}]`))
			return
		}
		linkNext(w, r, 2)
		var rows []string
		for i := 1; i <= 100; i++ {
			rows = append(rows, fmt.Sprintf(`{"number":%d,"title":"t","repository":{"full_name":"o/r"}}`, i))
		}
		_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
	})
	idx, err := c.ListIssues(context.Background(), "o", "", "", "", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Count != 101 || idx.Items[100].Number != 101 {
		t.Fatalf("pagination incomplete: count=%d last=%+v", idx.Count, idx.Items[len(idx.Items)-1])
	}
	if n := requests.Load(); n != 2 {
		t.Fatalf("cold listing = %d requests, want 2 (one per page)", n)
	}
	if idx.Provenance.Cached {
		t.Fatal("a cold listing must not report cached")
	}

	// Warm: same URLs, unchanged upstream (ETag matches) → the memo replays both
	// pages with ZERO new requests.
	before := requests.Load()
	idx2, err := c.ListIssues(context.Background(), "o", "", "", "", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if n := requests.Load(); n != before {
		t.Fatalf("a warm listing made %d new requests, want 0", n-before)
	}
	if !idx2.Provenance.Cached {
		t.Fatal("a warm listing must report cached")
	}
}

// TestListIssues_LimitStopsEarly pins the bounded-listing efficiency: limit=1
// stops after ONE page (the callback reports done), so pages of rows that would
// be discarded are never fetched.
func TestListIssues_LimitStopsEarly(t *testing.T) {
	var requests atomic.Int32
	c := cachedTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		linkNext(w, r, 2) // there IS a next page...
		var rows []string
		for i := 1; i <= 100; i++ {
			rows = append(rows, fmt.Sprintf(`{"number":%d,"repository":{"full_name":"o/r"}}`, i))
		}
		_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
	})
	idx, err := c.ListIssues(context.Background(), "o", "", "", "", "", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Count != 1 {
		t.Fatalf("limit=1 must stop at 1 item, got %d", idx.Count)
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("limit=1 must fetch exactly ONE page, got %d", n)
	}
}

// TestListIssues_LimitOnPageBoundaryStops pins the EXACT boundary case: a limit
// that is an exact multiple of per_page (here limit=100 with a full 100-row
// page) must STILL stop after that page — the done signal is page-level, so the
// next page is never fetched-and-discarded. A row-loop-only check fails this.
func TestListIssues_LimitOnPageBoundaryStops(t *testing.T) {
	var requests atomic.Int32
	c := cachedTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		linkNext(w, r, 2)
		var rows []string
		for i := 1; i <= 100; i++ {
			rows = append(rows, fmt.Sprintf(`{"number":%d,"repository":{"full_name":"o/r"}}`, i))
		}
		_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
	})
	idx, err := c.ListIssues(context.Background(), "o", "", "", "", "", 100, false)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Count != 100 {
		t.Fatalf("limit=100 must yield exactly 100 items, got %d", idx.Count)
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("limit=100 on a full page must STOP after page 1, got %d requests (page 2 fetched then discarded)", n)
	}
}

// TestGetAll_NoLinkHeaderIsTerminal pins the exact terminator: with NO Link
// header on page 1 the walk stops after one page even when that page is a FULL
// 100 rows — GitHub emits rel="next" iff there is a next page, so absence is the
// authoritative end. There is no page-count fallback (R4).
func TestGetAll_NoLinkHeaderIsTerminal(t *testing.T) {
	var requests atomic.Int32
	c := cachedTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		var rows []string
		for i := 1; i <= 100; i++ {
			rows = append(rows, fmt.Sprintf(`{"number":%d,"repository":{"full_name":"o/r"}}`, i))
		}
		_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
	})
	idx, err := c.ListIssues(context.Background(), "o", "", "", "", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Count != 100 {
		t.Fatalf("count = %d, want 100 (full page, no Link ⇒ terminal)", idx.Count)
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("a full page with NO Link header must stop after ONE request, got %d", n)
	}
}

// TestNextLink_Parsing pins the Link-header parser on the real GitHub shape,
// including the rel="last" arm that must be ignored for following.
func TestNextLink_Parsing(t *testing.T) {
	h := `<https://api.github.com/organizations/1/issues?page=2>; rel="next", <https://api.github.com/organizations/1/issues?page=789>; rel="last"`
	if got := nextLink(h); got != "https://api.github.com/organizations/1/issues?page=2" {
		t.Fatalf("nextLink = %q", got)
	}
	if got := nextLink(`<https://api.github.com/x?page=9>; rel="last"`); got != "" {
		t.Fatalf("a last-only header must yield no next, got %q", got)
	}
	if got := nextLink(""); got != "" {
		t.Fatalf("an empty header must yield no next, got %q", got)
	}
}
