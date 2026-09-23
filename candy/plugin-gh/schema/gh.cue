// gh.cue — the plugin-gh verb surface's own schema (SDD single source).
// Self-contained: no package clause, no base references.
//
// Two defs are served over Describe and spliced onto charly's base:
//   - #GhInput  — the authored `gh: {...}` verb input (host-validated).
//   - #GhDocument — the STRUCTURED issue/PR document the `document` op emits;
//     the plugin ALSO compiles this def internally (sdk.NewSchemaValidator) and
//     validates the marshalled document against it BEFORE writing (R8), so the
//     output contract is the same declaration the docs publish.

// #GhInput — the verb input. `op` selects the read; the document op adds the
// target/format/out/include_file_content knobs.
#GhInput: {
	// op — the read to run. The first six are the original PR reads; `document`
	// assembles the full structured issue/PR artifact.
	op: "pr_meta" | "pr_files" | "pr_diff" | "pr_commits" | "pr_thread" | "head_sha" | "document" @go(Op)
	// repo — the FULL slug (owner/name) — e.g. omacom/omarchy
	repo: string @go(Repo)
	// number — the issue OR pull-request number (renamed from `pr`: the document
	// op addresses issues too, and `pr` mis-described an issue read).
	number: int & >0 @go(Number,type=int)
	// target — document only: which artifact to assemble. Omitted auto-detects
	// (a pull request number yields a PR document; anything else an issue).
	target?: "issue" | "pr" @go(Target)
	// format — document only: the serialization of the emitted file (default json).
	format?: "json" | "yaml" @go(Format)
	// out — document only: write the serialized document to this path. Omitted
	// returns the document inline in the verb result.
	out?: string @go(Out)
	// include_file_content — document only: fetch each changed file's COMPLETE
	// content at the head commit (via the git-blobs endpoint), not just its diff.
	// A pointer so omission (nil) means the DEFAULT (true) while an explicit false
	// disables it; a binary or oversized file is recorded with omitted_reason.
	include_file_content?: bool @go(IncludeFileContent,type=*bool)
}

// #GhDocument — the structured issue/PR artifact. EVERY comment surface is
// present: the issue/PR body, the issue comments, and (for a PR) the reviews and
// the inline review comments. PR documents additionally carry the commits and
// every changed file (patch + optional full content at head).
#GhDocument: {
	kind:   "issue" | "pr" @go(Kind)
	repo:   string          @go(Repo)
	number: int             @go(Number,type=int)
	url:    string          @go(URL)
	title:  string          @go(Title)
	state:  string          @go(State)
	author: string          @go(Author)
	// created_at / updated_at / fetched_at are RFC3339 timestamps.
	created_at: string   @go(CreatedAt)
	updated_at: string   @go(UpdatedAt)
	body:       string   @go(Body)
	labels:     [...string] @go(Labels)
	assignees?: [...string] @go(Assignees)
	comments: [...#GhComment] @go(Comments)
	// pr is present iff kind == "pr" (a pointer so an issue document omits it).
	pr?: #GhPR @go(PR,type=*GhPR)
	// fetched_at is when the plugin assembled this document.
	fetched_at: string         @go(FetchedAt)
	provenance: #GhProvenance @go(Provenance)
}

// #GhComment — one issue/PR (timeline) comment.
#GhComment: {
	id:         int    @go(ID,type=int)
	author:     string @go(Author)
	created_at: string @go(CreatedAt)
	body:       string @go(Body)
}

// #GhPR — the pull-request-specific block.
#GhPR: {
	head_sha:      string @go(HeadSHA)
	base_ref:      string @go(BaseRef)
	head_ref:      string @go(HeadRef)
	draft:         bool   @go(Draft)
	mergeable?:    bool   @go(Mergeable,type=*bool)
	additions:     int    @go(Additions,type=int)
	deletions:     int    @go(Deletions,type=int)
	changed_files: int    @go(ChangedFiles,type=int)
	commits:         [...#GhCommit]        @go(Commits)
	files:           [...#GhFile]          @go(Files)
	reviews:         [...#GhReview]        @go(Reviews)
	review_comments: [...#GhReviewComment] @go(ReviewComments)
}

// #GhCommit — one commit on the PR.
#GhCommit: {
	sha:     string @go(SHA)
	message: string @go(Message)
	author:  string @go(Author)
	date:    string @go(Date)
}

// #GhReview — one submitted PR review (APPROVED / CHANGES_REQUESTED / COMMENTED /
// DISMISSED), with its summary body.
#GhReview: {
	id:           int    @go(ID,type=int)
	author:       string @go(Author)
	state:        string @go(State)
	body:         string @go(Body)
	submitted_at: string @go(SubmittedAt)
}

// #GhReviewComment — one INLINE review comment (anchored to a file + line).
#GhReviewComment: {
	id:         int    @go(ID,type=int)
	author:     string @go(Author)
	path:       string @go(Path)
	line?:      int    @go(Line,type=int)
	side?:      string @go(Side)
	created_at: string @go(CreatedAt)
	body:       string @go(Body)
	// in_reply_to_id is set when this comment replies to another review comment.
	in_reply_to_id?: int @go(InReplyToID,type=*int)
}

// #GhFile — one changed file: its diff (patch) plus, when include_file_content
// is on, its complete content at the head commit.
#GhFile: {
	path:      string @go(Path)
	status:    string @go(Status)
	additions: int    @go(Additions,type=int)
	deletions: int    @go(Deletions,type=int)
	// patch is the unified-diff hunk text (empty when GitHub returned none).
	patch:    string @go(Patch)
	no_patch: bool   @go(NoPatch)
	// blob_sha is the git blob SHA of the file at the head commit — the immutable
	// coordinate the content is fetched (and cached) by.
	blob_sha?: string @go(BlobSHA)
	// content is the file's full text at head (present iff is_binary is false and
	// the file was not omitted).
	content?: string @go(Content)
	// content_encoding is "utf-8" for the decoded text content.
	content_encoding?: string @go(ContentEncoding)
	// is_binary marks a file whose content is not text (content omitted).
	is_binary?: bool @go(IsBinary)
	// truncated marks a text file whose content exceeded the size cap.
	truncated?: bool @go(Truncated)
	// omitted_reason names WHY content is absent: "binary" | "too_large" | "fetch_failed".
	omitted_reason?: string @go(OmittedReason)
}

// #GhProvenance — how the document was produced (the evidence header).
#GhProvenance: {
	api_base:     string @go(APIBase)
	token_source: string @go(TokenSource)
	// cached is true when the document assembly served at least one response
	// from the local cache without re-fetching the body.
	cached: bool @go(Cached)
}
