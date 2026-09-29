# AGENTS.md — plugin-gh

Standalone plugin repo for the canonical GitHub read surface: the declarative
`verb:gh` check verb (`pr_meta`, `pr_files`, `pr_diff`, `pr_commits`,
`pr_thread`, `head_sha`, `document`, `issues`) and the `charly gh` CLI
(`command:gh`), plus the importable `gh/ghkit` Go client. The Go module lives at
the REPO ROOT (module `github.com/opencharly/plugin-gh` — the tag-trap layout its
Go consumers require by proxy tag); the candy lives at `candy/plugin-gh/`. The
root `charly.yml` declares `discover: candy` and the two R10 witness beds
(`gh-issues-repo`, `gh-issues-org`).

Canonical files:

- `candy/plugin-gh/charly.yml` — the `plugin-gh:` candy entity (`plugin:` block,
  `plan:` checks).
- `candy/plugin-gh/plugin.go`, `cli.go`, `document_emit.go`, `issues_emit.go` —
  the provider, the CLI, and the document/issue-index emitters.
- `candy/plugin-gh/gh/` — the `ghkit` client: `ghkit.go` (token/API-base/diag),
  `cache.go` (the shared `spec/cache` ArtifactStore), `document.go`,
  `issues.go`.
- `candy/plugin-gh/schema/gh.cue` + `params/cue_types_gen.go`.
- `candy/plugin-gh/cmd/serve/main.go` — the out-of-process serve shim.
- `charly.yml` — the root project manifest + the R10 witness beds.
- `.github/workflows/ci.yml` + `.github/workflows/tag-on-merge.yml`.
- `README.md` — user overview only; never agent guidance.

## Load these skills first (R0)

- `/charly-internals:plugin` — the plugin authoring reference: the `plugin:`
  block, the unified Provider model, the per-plugin CUE-schema contract,
  placement. Load before touching the provider or schema.
- `/charly-check:check` — the check-verb surface and the disposable R10 beds this
  repo's `gh-issues-*` entities drive.
- `/charly-internals:git-workflow` — before any git/PR action.

There is no dedicated `/charly-*:gh` owning skill for `verb:gh`/`command:gh` —
this repo's candy carries no `skill:` entity. The gap is recorded against
`opencharly/opencharly#291`; when one is authored, add it here.

## Build / validate / test

- `go build ./...` at the repo root — compile the root module (the module is the
  repo root, so this covers `gh/`, the candy, and `cmd/serve`).
- `go test ./...` at the repo root — the hermetic unit + cache + document +
  issues tests.
- `charly gh --self-test` — the offline schema/serialization probe.
- `charly check run gh-issues-repo` — R10: repo-scoped issues + cache-warm (no
  credential needed).
- `charly check run gh-issues-org` — R10: org-wide issues (skips cleanly without
  a token; never a fabricated listing).
- The merge gate is the **org-wide** `charly/pr-validator` (required check
  `validate / validate`, defined in `opencharly/.github`); this repo has **no**
  per-repo candy gate.

## Modify this repo

- Edit the `plugin-gh:` candy entity, the Go source, and `schema/gh.cue`
  **together** — the schema is the single source for the generated params.
- Keep every GitHub read on `gh/ghkit`: explicit token resolution, the pinned
  `api.github.com` base, real non-2xx diagnostics, Link-header pagination, and
  the shared `spec/cache` ArtifactStore. Never reintroduce a hand-rolled
  `exec.Command("gh")`.
- The live GitHub read tests opt in via `GHKIT_LIVE_REPO` (+ a token) and SKIP
  cleanly otherwise — never a fabricated response.

## Landing

Every change lands through a pull request gated by the org-required
`charly/pr-validator`. The landing mechanics — the `feat/` branch, the PR-only
rule, `CHANGELOG/` history, and the tag-on-merge CalVer — are owned by
`/charly-internals:git-workflow` and the umbrella `AGENTS.md` /
`charly/AGENTS.md`; this signpost points at them and does not restate them.
