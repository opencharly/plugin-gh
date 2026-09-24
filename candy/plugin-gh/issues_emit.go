package plugingh

// issues_emit.go — the `issues` op's emission: assemble the compact org/repo
// index, VALIDATE it against the served #GhIssueIndex schema (R8 — the same CUE
// declaration the host splices), and serialize it as JSON or YAML, either inline
// or to `out`. ONE implementation serves BOTH the verb surface (runOp) and the
// CLI (CliMain), exactly like the document op.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/opencharly/plugin-gh/candy/plugin-gh/gh"
	"github.com/opencharly/plugin-gh/candy/plugin-gh/params"
	"github.com/opencharly/sdk/kit"
	"go.yaml.in/yaml/v3"
)

// emitIssues runs the list op, validates the index against #GhIssueIndex, and
// serializes it. include_body defaults false (the index is a compact listing).
//
// An ORG-scoped list requires authentication (/orgs/{org}/issues answers 401
// unauthenticated); rather than fail a bed on a missing credential, the op
// returns the host-visible `skip` status — the "live or skip, never fake"
// contract. A repo-scoped list works unauthenticated and never skips.
func emitIssues(ctx context.Context, client *gh.Client, in params.GhInput) (map[string]any, error) {
	if (in.Org == "") == (in.Repo == "") {
		return nil, fmt.Errorf("gh: issues: exactly one of org or repo is required")
	}
	if in.Org != "" && client.Token == "" {
		return nil, errSkip("gh: issues org=<%s>: no GH_TOKEN/GITHUB_TOKEN (or gh hosts.yml auth) — an org-wide listing requires authentication", in.Org)
	}
	includeBody := in.IncludeBody != nil && *in.IncludeBody
	idx, err := client.ListIssues(ctx, in.Org, in.Repo, in.State, in.Kind, in.Since, in.Limit, includeBody)
	if err != nil {
		return nil, err
	}
	if err := validateIssueIndex(idx); err != nil {
		return nil, err
	}
	format := in.Format
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "yaml" {
		return nil, fmt.Errorf("gh: issues: unknown format %q (json|yaml)", format)
	}
	data, err := serializeIssueIndex(idx, format)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"op": "issues", "scope": idx.Scope, "state": idx.State, "kind": idx.Kind,
		"count": idx.Count, "cached": idx.Provenance.Cached,
	}
	if in.Out != "" {
		if err := kit.AtomicWriteFile(in.Out, data, 0o644); err != nil {
			return nil, fmt.Errorf("gh: issues: write %s: %w", in.Out, err)
		}
		out["out"] = in.Out
		out["bytes"] = len(data)
		return out, nil
	}
	jb, err := json.Marshal(idx)
	if err != nil {
		return nil, fmt.Errorf("gh: issues: re-encode for inline result: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(jb, &m); err != nil {
		return nil, fmt.Errorf("gh: issues: re-encode for inline result: %w", err)
	}
	out["index"] = m
	return out, nil
}

// skipError carries a host-visible SKIP (status "skip") rather than a failure —
// the credential-absent case for an op whose endpoint cannot be served without a
// credential. The host maps status "skip" to a reported skip, never a silent
// pass (charly/provider_checkenv.go).
type skipError struct{ msg string }

func (e *skipError) Error() string { return e.msg }

// errSkip builds a skipError (a formatted Skip, not a fail).
func errSkip(format string, args ...any) error {
	return &skipError{msg: fmt.Sprintf(format, args...)}
}

// validateIssueIndex validates the index against #GhIssueIndex. A nil validator
// is a HARD ERROR (R8 — the artifact is validated BEFORE it is emitted).
func validateIssueIndex(idx *params.GhIssueIndex) error {
	if documentValidator == nil {
		return fmt.Errorf("gh: issues: #GhIssueIndex validator unavailable (embedded schema failed to compile) — refusing to emit an unvalidated index")
	}
	if err := documentValidator.Validate("#GhIssueIndex", idx); err != nil {
		return fmt.Errorf("gh: issues: failed #GhIssueIndex validation: %w", err)
	}
	return nil
}

// serializeIssueIndex marshals the index in the requested format.
func serializeIssueIndex(idx *params.GhIssueIndex, format string) ([]byte, error) {
	if format == "yaml" {
		b, err := yaml.Marshal(idx)
		if err != nil {
			return nil, fmt.Errorf("gh: issues: marshal yaml: %w", err)
		}
		return b, nil
	}
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("gh: issues: marshal json: %w", err)
	}
	return append(b, '\n'), nil
}
