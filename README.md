# plugin-gh

The canonical GitHub surface for opencharly plugins — one implementation of the
gh read ops, served as a charly plugin (the `verb:gh` check surface + the
`command:gh` standalone CLI) and importable as a Go package (`gh/ghkit`) by any
plugin that needs PR data.

## Why

Every plugin that hand-rolls `exec.Command("gh", ...)` inherits the same defect
set (RCA 2026.252.2233): gh's stderr is swallowed (an error surfaces as a bare
`gh meta failed`), the auth + host resolution ride the ambient `gh hosts.yml`
(and its default-host redirect — a silent, undeclared target change), and the
calls break opaquely inside executor subprocesses where the operator's shell env
does not reach.

`gh/ghkit` replaces all of it:

- **Explicit token resolution** — `GH_TOKEN` → `GITHUB_TOKEN` → the gh
  `hosts.yml` auth (`github.com` → `oauth_token`), each source named.
- **A pinned API base** — `https://api.github.com` (override:
  `GITHUB_API_URL`); the hosts.yml default-host redirect is never followed.
- **Real diagnostics** — every non-2xx surfaces the HTTP status AND the
  response body; a call that fails with no detail is a defect.
- **Typed ops** — pr_meta, pr_files (+ the space-separated paths for the
  pr-apply seam), pr_diff, pr_commits, pr_thread, head_sha, **document**, and
  **issues**.
- **Link-header pagination** — every list is followed by the Link header's
  `rel="next"` (the ONE mechanism; GitHub emits it iff there is a next page, so
  its absence is the exact terminator). There is no page-count guesswork.
- **A shared response cache** — every read goes through the ONE
  `github.com/opencharly/spec/cache` **ArtifactStore** (an OCI Image Layout,
  R3). Mutable reads (PR/issue meta,
  files, comments, commits, reviews) are ETag-revalidated: unchanged upstream
  costs a 304, never a body re-fetch. Immutable reads (a git blob by SHA) are
  content-addressed: a repeat read never touches the network at all. This is not
  a gh-specific cache — it is the same cache mechanism the loader and the rest
  of the tree use.

## Surface

| Class | Word | Shape |
|---|---|---|
| verb | `gh` | `gh: {op: pr_meta\|pr_files\|pr_diff\|pr_commits\|pr_thread\|head_sha\|document, repo: OWNER/NAME, number: N, target?: issue\|pr, format?: json\|yaml, out?: PATH, include_file_content?: bool}` |
| verb | `gh` | `gh: {op: issues, repo?: OWNER/NAME, org?: LOGIN, state?: open\|closed\|all, kind?: issue\|pr\|all, since?: TS, limit?: N, include_body?: bool, format?: json\|yaml, out?: PATH}` |
| command | `gh` | `charly gh <op> [--repo OWNER/NAME \| --org ORG] [--number N] [--target …] [--format …] [--out …] [--state …] [--kind …] [--since …] [--limit N] [--include-body]` |

### The `document` op

`gh: {op: document, repo: …, number: …, out: pr.json}` (or
`charly gh document --repo … --number … --format yaml --out pr.yaml`) assembles a
whole issue OR pull request into ONE structured artifact — the full body, EVERY
issue comment, EVERY review and inline review comment, the commits, and every
changed file's unified patch PLUS its complete content at the head commit. The
document is validated against the served `#GhDocument` CUE def before it is
written.

- `target` — `issue` | `pr`; omitted auto-detects (a pull request number yields
  a PR document, anything else an issue).
- `format` — `json` (default) or `yaml`.
- `out` — write the serialized document to this path; omitted returns it inline
  in the verb result.
- `include_file_content` — fetch each changed file's full content at head
  (default **true**; pass `=false` to keep only the diffs). A binary or oversized
  file is recorded with `is_binary`/`truncated` + `omitted_reason` rather than
  silently dropped.

### The `issues` op

`gh: {op: issues, org: opencharly}` (whole org) or
`gh: {op: issues, repo: opencharly/plugin-gh}` (one repo) lists the issues AND
pull requests as a compact `#GhIssueIndex` — the efficient discovery surface to
pair with `document`: list cheaply, then fetch only the items you care about.

- **Org-wide in ONE call** — `/orgs/{org}/issues?filter=all` returns every open
  issue+PR across the whole organization. (The endpoint defaults to
  `filter=assigned`, which under-lists; the op always sends `filter=all`.) It
  requires **authentication** — with no credential the op reports a
  host-visible **skip**, never a fabricated listing.
- **Repo-scoped** — `/repos/{owner}/{repo}/issues`, public (no credential).
- `state` — `open` (default) | `closed` | `all`.
- `kind` — `all` (default) | `issue` | `pr` (GitHub has no server-side
  issue-only filter here; applied client-side off the `pull_request` marker).
- `since` — only items updated at/after an RFC3339 timestamp (incremental).
- `limit` — stop after N items (the walk stops exactly, no discarded page).
- `include_body` — include each item's body (default false; the index is a
  compact listing).

Every row carries `kind`, `repo`, `number`, `title`, `state`, `author`, the
timestamps, `url`, `labels`, `comment_count`, and — for a PR — `draft` and
`merged_at` (the only PR signals the issues endpoints carry; the head/base refs
and SHA live on `/pulls`, so use the `document` op for those). The whole index is
cached: a repeat listing of an unchanged org/repo is served from the per-page
ETag cache with no body re-fetch (`provenance.cached=true`).

The Go client: `github.com/opencharly/plugin-gh/candy/plugin-gh/gh`.

## Verify

```
cd candy/plugin-gh && go test ./...      # hermetic unit + cache + document + issues tests
charly gh --self-test                    # offline schema/serialization probe
charly check run gh-issues-repo          # R10: repo-scoped issues + cache-warm (no credential)
charly check run gh-issues-org           # R10: org-wide issues (skips cleanly without a token)
```

The live GitHub read tests opt in via `GHKIT_LIVE_REPO` (+ a token) and SKIP
cleanly otherwise (R7a — never a fabricated response).

*Assisted-by: opencode ollama-cloud/deepseek-v4.1-flash (fully tested and validated)*
