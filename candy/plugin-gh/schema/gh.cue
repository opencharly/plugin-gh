// gh.cue — the plugin-gh verb surface's own schema (SDD single source).
// Self-contained: no package clause, no base references.

#GhInput: {
	op:   "pr_meta" | "pr_files" | "pr_diff" | "pr_commits" | "pr_thread" | "head_sha"
	repo: string       // the FULL slug (owner/name) — e.g. omacom/omarchy
	pr:   int & >0
}