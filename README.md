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
  pr-apply seam), pr_diff, pr_commits, pr_thread, head_sha.

## Surface

| Class | Word | Shape |
|---|---|---|
| verb | `gh` | `gh: {op: pr_meta|pr_files|pr_diff|pr_commits|pr_thread|head_sha, repo: OWNER/NAME, pr: N}` |
| command | `gh` | `charly gh <op> --repo OWNER/NAME --pr N` |

The Go client: `github.com/opencharly/plugin-gh/candy/plugin-gh/gh`.

## Verify

```
cd candy/plugin-gh && go test ./...
```

*Assisted-by: pi openrouter/deepseek/deepseek-v4-flash-0731 (fully tested and validated)*