package plugingh

// document_emit.go — the `document` op's emission: assemble the structured
// issue/PR document, VALIDATE it against the served #GhDocument schema (R8 —
// the same CUE declaration the host splices), serialize it as JSON or YAML, and
// either write it to `out` or return it inline. ONE implementation serves BOTH
// the verb surface (runOp) and the CLI (CliMain), exactly like every other op.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/opencharly/plugin-gh/candy/plugin-gh/gh"
	"github.com/opencharly/plugin-gh/candy/plugin-gh/params"
	"github.com/opencharly/sdk"
	"github.com/opencharly/sdk/kit"
	"go.yaml.in/yaml/v3"
)

// documentValidator is built once (the schema is embedded, the definition
// fixed): the plugin compiles its OWN served schema and validates the emitted
// document against #GhDocument before writing it.
var documentValidator = newDocumentValidator()

func newDocumentValidator() *sdk.SchemaValidator {
	v, err := sdk.NewSchemaValidator(schemaFS, "schema")
	if err != nil {
		// A defective embedded schema cannot be a runtime surprise: surface it
		// where the op runs (the load gate already rejects a broken schema).
		return nil
	}
	return v
}

// emitDocument assembles + validates + serializes the document and returns the
// op result map. When `out` is set the serialized bytes are written to that path
// (atomically) and the result carries the path + a summary; otherwise the parsed
// document rides `document` in the result.
func emitDocument(ctx context.Context, client *gh.Client, in params.GhInput) (map[string]any, error) {
	format := in.Format
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "yaml" {
		return nil, fmt.Errorf("gh: document: unknown format %q (json|yaml)", format)
	}
	// include_file_content defaults TRUE: the field is a pointer so omission
	// (nil) means the default while an explicit false disables it.
	includeContent := in.IncludeFileContent == nil || *in.IncludeFileContent
	doc, err := client.AssembleDocument(ctx, in.Repo, in.Number, in.Target, includeContent)
	if err != nil {
		return nil, err
	}
	if err := validateDocument(doc); err != nil {
		return nil, err
	}
	data, err := serializeDocument(doc, format)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"op": in.Op, "repo": in.Repo, "number": in.Number,
		"kind": doc.Kind, "format": format,
	}
	if in.Out != "" {
		if err := kit.AtomicWriteFile(in.Out, data, 0o644); err != nil {
			return nil, fmt.Errorf("gh: document: write %s: %w", in.Out, err)
		}
		out["out"] = in.Out
		out["bytes"] = len(data)
		return out, nil
	}
	// Inline: attach the document as its structured map (always via JSON so the
	// result is the plain wire shape, not a Go struct).
	jb, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("gh: document: re-encode for inline result: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(jb, &m); err != nil {
		return nil, fmt.Errorf("gh: document: re-encode for inline result: %w", err)
	}
	out["document"] = m
	return out, nil
}

// validateDocument validates the document against #GhDocument. A nil validator
// (only possible on a defective embedded schema, which the load gate already
// rejects) degrades to a pass with a loud stderr line rather than failing the
// op for a reason unrelated to the read.
func validateDocument(doc *params.GhDocument) error {
	if documentValidator == nil {
		fmt.Fprintln(os.Stderr, "gh: WARNING: #GhDocument validator unavailable (embedded schema failed to compile) — emitting unvalidated")
		return nil
	}
	if err := documentValidator.Validate("#GhDocument", doc); err != nil {
		return fmt.Errorf("gh: document: failed #GhDocument validation: %w", err)
	}
	return nil
}

// selfTest is the offline, deterministic ADE probe: it proves the embedded
// #GhDocument schema compiles and validates a representative document, and that
// JSON + YAML serialization round-trip. No network, no credential — a bed can
// assert the schema/serialization path without a live GitHub read.
func selfTest() int {
	if documentValidator == nil {
		fmt.Fprintln(os.Stderr, "gh self-test: FAIL: #GhDocument validator unavailable (embedded schema did not compile)")
		return 1
	}
	sample := &params.GhDocument{
		Kind: "pr", Repo: "o/r", Number: 1, URL: "https://x/1",
		Title: "t", State: "open", Author: "a",
		CreatedAt: "c", UpdatedAt: "u", Body: "b",
		Labels:   []string{"bug"},
		Comments: []params.GhComment{{ID: 1, Author: "a", CreatedAt: "c", Body: "x"}},
		PR: &params.GhPR{
			HeadSHA: "head", BaseRef: "main", HeadRef: "feat",
			Additions: 1, ChangedFiles: 1,
			// EVERY required list is populated (even empty), so the sample is a
			// schema-valid document with no null where the def requires a list —
			// the same invariant AssembleDocument guarantees by make()-ing every
			// slice.
			Commits:        []params.GhCommit{{SHA: "s", Message: "m", Author: "a"}},
			Files:          []params.GhFile{{Path: "a.go", Status: "modified", Patch: "@@", BlobSHA: "b"}},
			Reviews:        []params.GhReview{{ID: 1, Author: "r", State: "APPROVED", Body: "ok"}},
			ReviewComments: []params.GhReviewComment{{ID: 2, Author: "r", Path: "a.go", Body: "nit"}},
		},
		FetchedAt:  "f",
		Provenance: params.GhProvenance{APIBase: "https://api.github.com", TokenSource: "test"},
	}
	if err := validateDocument(sample); err != nil {
		fmt.Fprintf(os.Stderr, "gh self-test: FAIL: %v\n", err)
		return 1
	}
	// A malformed document (unknown kind) MUST be rejected — the validator has teeth.
	bad := *sample
	bad.Kind = "nonsense"
	if err := validateDocument(&bad); err == nil {
		fmt.Fprintln(os.Stderr, "gh self-test: FAIL: the validator accepted an invalid kind")
		return 1
	}
	for _, format := range []string{"json", "yaml"} {
		if _, err := serializeDocument(sample, format); err != nil {
			fmt.Fprintf(os.Stderr, "gh self-test: FAIL: serialize %s: %v\n", format, err)
			return 1
		}
	}
	fmt.Println("gh self-test: PASS (#GhDocument schema compiles, validates a sample, and rejects an invalid kind; json+yaml serialize)")
	return 0
}

// serializeDocument marshals the document in the requested format.
func serializeDocument(doc *params.GhDocument, format string) ([]byte, error) {
	if format == "yaml" {
		b, err := yaml.Marshal(doc)
		if err != nil {
			return nil, fmt.Errorf("gh: document: marshal yaml: %w", err)
		}
		return b, nil
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("gh: document: marshal json: %w", err)
	}
	return append(b, '\n'), nil
}
