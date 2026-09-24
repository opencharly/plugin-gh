package gh

// issues.go — the `issues` list op: a repo's or an ORGANIZATION's issues AND
// pull requests as a compact index, WITHOUT a per-item fetch. This is the
// efficient discovery surface: ONE paginated request to /orgs/{org}/issues
// returns every issue+PR across the whole org (the endpoint defaults to
// filter=assigned, so filter=all is required), and each row already carries the
// PR marker, the state, the update time and (optionally) the body — enough for a
// caller to build a review queue and then read only the items it cares about via
// the `document` op.
//
// Why the org endpoint over search: /search/issues is capped at 10 requests/min
// unauthenticated and 30/min authenticated and its result window is shallow,
// while /orgs/{org}/issues rides the CORE limit (60/hr unauth, 5000/hr auth) and
// pages deterministically via the Link header. A repo-scoped list uses
// /repos/{owner}/{repo}/issues — the SAME row shape and the SAME code path.
//
// Caching: every page is ETag-revalidated (cache.go), so a repeat listing of an
// unchanged org costs one 304 per page and no body re-fetch; a warm in-process
// memo replays the whole page set with no request at all.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/opencharly/plugin-gh/candy/plugin-gh/params"
)

// issueRow is one row of a GitHub issues listing (the /issues endpoints serve
// issues AND pull requests; a PR row carries a `pull_request` member).
type issueRow struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	HTMLURL   string `json:"html_url"`
	Body      string `json:"body"`
	Comments  int    `json:"comments"`
	// Repository is the STRUCTURED repo object the /issues endpoints return
	// (org listings span repos) — full_name is "owner/name".
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	// PullRequest is non-nil iff this row is a pull request. The issues endpoint
	// carries ONLY url/html_url/diff_url/patch_url/merged_at here — the head/base
	// refs and SHA are NOT on this endpoint (they live on /pulls), so the index
	// does not claim them.
	PullRequest *struct {
		MergedAt *string `json:"merged_at"`
	} `json:"pull_request"`
	// Draft is the top-level issue field the endpoint returns for a PR row.
	Draft bool `json:"draft"`
}

// ListIssues lists issues AND pull requests for an org (org != "") or a repo
// (repo != ""), paginated to completion via the Link header and cached per page.
// state defaults to open; kind/limit/since filter the result; includeBody adds
// each item's body. The returned index is scope-labelled and provenance-stamped.
//
// Exactly one of org/repo is required — the two scopes share this ONE
// implementation (R3); only the request path differs.
func (c *Client) ListIssues(ctx context.Context, org, repo, state, kind, since string, limit int, includeBody bool) (*params.GhIssueIndex, error) {
	if (org == "") == (repo == "") {
		return nil, fmt.Errorf("gh: issues: exactly one of org or repo is required")
	}
	if state == "" {
		state = "open"
	}
	if kind == "" {
		kind = "all"
	}
	var (
		scope string
		path  string
	)
	if org != "" {
		scope = "org:" + org
		// filter=all is REQUIRED: the endpoint defaults to `assigned` (only what
		// the authenticated user is assigned), which would silently under-list.
		path = fmt.Sprintf("/orgs/%s/issues?filter=all&state=%s", org, state)
	} else {
		scope = "repo:" + repo
		path = fmt.Sprintf("/repos/%s/issues?state=%s", repo, state)
	}
	if since != "" {
		path += "&since=" + since + "&direction=desc&sort=updated"
	}

	var hitsBefore int
	if c.cache != nil {
		hitsBefore = c.cache.hitsSince()
	}
	idx := &params.GhIssueIndex{
		Scope: scope, State: state, Kind: kind,
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		Provenance: params.GhProvenance{
			APIBase:     c.BaseURL,
			TokenSource: c.TokenSource(),
		},
	}
	err := c.getAll(ctx, path, func(b []byte) (int, bool, error) {
		var page []issueRow
		if err := json.Unmarshal(b, &page); err != nil {
			return 0, false, err
		}
		for _, r := range page {
			isPR := r.PullRequest != nil
			if kind == "issue" && isPR {
				continue
			}
			if kind == "pr" && !isPR {
				continue
			}
			if limit > 0 && len(idx.Items) >= limit {
				// Bounded listing satisfied: stop the walk exactly here, so no
				// further page is fetched for rows that would be discarded.
				return len(page), true, nil
			}
			idx.Items = append(idx.Items, c.toIssueRef(r, isPR, includeBody))
		}
		return len(page), false, nil
	})
	if err != nil {
		return nil, err
	}
	idx.Count = len(idx.Items)
	if c.cache != nil {
		idx.Provenance.Cached = c.cache.hitsSince() > hitsBefore
	}
	return idx, nil
}

// toIssueRef converts one API row into a #GhIssueRef, deriving the per-repo
// fields from repository_url (an org listing spans repos).
func (c *Client) toIssueRef(r issueRow, isPR, includeBody bool) params.GhIssueRef {
	ref := params.GhIssueRef{
		Kind: "issue", Repo: r.Repository.FullName, Number: r.Number,
		Title: r.Title, State: r.State, Author: orUnknown(r.User.Login),
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, URL: r.HTMLURL,
		CommentCount: r.Comments,
		Labels:       make([]string, len(r.Labels)),
	}
	for i, l := range r.Labels {
		ref.Labels[i] = l.Name
	}
	if isPR {
		ref.Kind = "pr"
		pr := &params.GhIssueRefPR{Draft: r.Draft}
		if r.PullRequest.MergedAt != nil {
			pr.MergedAt = *r.PullRequest.MergedAt
		}
		ref.PR = pr
	}
	if includeBody {
		ref.Body = r.Body
	}
	return ref
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
