package plugingh

// schema_served_test.go is the schema-LOAD gate, and the §10.1 S3 gate.
//
// `charly box validate` does NOT compile a served schema: a dangling reference
// inside a served def is green at plugin load and only surfaces when the host
// validates an authored `gh: {...}` against it (opencharly/spec#301). So this
// file composes the plugin's OWN Describe reply and compiles the body it serves,
// with the same CUE library the runtime links — the served body, not the file on
// disk, is the surface the host splices onto the base.
//
// It pins the S3 half of D-PAT-3 as well: every closed set of words is a
// vocabulary table, its word list is DERIVED from that table, and the enum a
// document meets is `or()` of the derived list — so a word cannot be added to the
// table and forgotten by the list, nor accepted by the enum while the table it
// claims to derive from lacks it.

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
	pb "github.com/opencharly/spec/proto"
)

// vocabHarness LIFTS the hidden vocabulary tables and their derived word lists
// into one public definition. An `_`-prefixed label is hidden by CUE and cannot
// be looked up from outside the compilation unit, so the served body alone
// exposes only the enums; this fragment adds no semantics, it re-exports the
// tables the schema already carries so the test can read them instead of
// restating their words.
const vocabHarness = `
#TestVocabulary: {
	tables: {
		op:     _op
		target: _target
		format: _format
		state:  _state
		kind:   _kind
	}
	words: {
		op:     _opWord
		target: _targetWord
		format: _formatWord
		state:  _stateWord
		kind:   _kindWord
	}
}
`

// vocabProbe pairs a vocabulary table (named as in vocabHarness) with ONE
// authored document that uses a word from it, so the enum is exercised through
// the same def the host validates against.
type vocabProbe struct {
	name string
	use  func(word string) string
}

// documentDoc renders a complete #GhDocument (every required field present) with
// the document kind set to the probed word.
func documentDoc(kind string) string {
	return fmt.Sprintf(`{#GhDocument: {kind: %q, repo: "o/r", number: 1, url: "u", title: "t", state: "open", author: "a", created_at: "c", updated_at: "u", body: "b", labels: [], comments: [], fetched_at: "f", provenance: {api_base: "x", token_source: "y", cached: false}}}`, kind)
}

// vocabProbes covers every vocabulary table this schema carries, and every field
// wired to it. A new table belongs in vocabHarness and here.
var vocabProbes = []vocabProbe{
	{"op", func(w string) string {
		return fmt.Sprintf(`{#GhInput: {op: %q}}`, w)
	}},
	{"target", func(w string) string {
		return fmt.Sprintf(`{#GhInput: {op: "document", target: %q}}`, w)
	}},
	{"target", func(w string) string {
		return documentDoc(w)
	}},
	{"format", func(w string) string {
		return fmt.Sprintf(`{#GhInput: {op: "document", format: %q}}`, w)
	}},
	{"state", func(w string) string {
		return fmt.Sprintf(`{#GhInput: {op: "issues", state: %q}}`, w)
	}},
	{"kind", func(w string) string {
		return fmt.Sprintf(`{#GhInput: {op: "issues", kind: %q}}`, w)
	}},
}

// servedSchema returns the plugin's served CUE body COMPILED, with the test's
// table re-export appended. NewMeta's Describe already compiles the concatenated
// body (sdk.BuildCapabilities), so a failure here means the schema the host
// receives does not stand on its own.
func servedSchema(t *testing.T) cue.Value {
	t.Helper()
	caps, err := NewMeta().Describe(context.Background(), &pb.Empty{})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	body := caps.GetSchemaCue()
	if body == "" {
		t.Fatal("Describe served an EMPTY CUE body")
	}
	v := cuecontext.New().CompileString(body + vocabHarness)
	if err := v.Err(); err != nil {
		t.Fatalf("served schema does not compile: %v", err)
	}
	return v
}

// schemaWordList returns the sorted alternatives of a disjunction at path — for
// an S3 enum, exactly the words its derived list feeds.
func schemaWordList(t *testing.T, v cue.Value, path string) []string {
	t.Helper()
	val := v.LookupPath(cue.ParsePath(path))
	if err := val.Err(); err != nil {
		t.Fatalf("%s does not resolve in the served schema: %v", path, err)
	}
	_, alts := val.Expr()
	if len(alts) == 0 {
		t.Fatalf("%s has no alternatives — it is not a word list", path)
	}
	words := make([]string, 0, len(alts))
	for _, alt := range alts {
		w, err := alt.String()
		if err != nil {
			t.Fatalf("%s has a non-string alternative %v: %v", path, alt, err)
		}
		words = append(words, w)
	}
	sort.Strings(words)
	return words
}

// TestServedSchemaResolvesItsDefs — every definition the plugin's contract names
// must still resolve in the served body.
func TestServedSchemaResolvesItsDefs(t *testing.T) {
	v := servedSchema(t)
	for _, path := range []string{
		"#GhInput", "#GhDocument", "#GhComment", "#GhPR", "#GhFile",
		"#GhProvenance", "#GhIssueIndex", "#GhIssueRef", "#GhIssueRefPR",
	} {
		if err := v.LookupPath(cue.ParsePath(path)).Err(); err != nil {
			t.Errorf("served schema: %s does not resolve: %v", path, err)
		}
	}
}

// TestServedCatalogCoversTheWholeEnum guards the READER and the served catalog in
// one step: the op enum's first and last entries must be present, so neither a
// truncated read nor a table whose tail never reaches the enum can exempt the
// catalog's tail.
func TestServedCatalogCoversTheWholeEnum(t *testing.T) {
	got := schemaWordList(t, servedSchema(t), "#GhInput.op")
	has := func(name string) bool {
		for _, m := range got {
			if m == name {
				return true
			}
		}
		return false
	}
	if !has("pr_meta") {
		t.Errorf("served enum is missing its HEAD (pr_meta) — got %d ops: %v", len(got), got)
	}
	if !has("issues") {
		t.Errorf("served enum is missing its TAIL (issues) — the derivation is losing rows; got %d ops: %v", len(got), got)
	}
	if len(got) < 8 {
		t.Errorf("read only %d ops — the served #GhInput.op derivation looks truncated", len(got))
	}
}

// TestS3VocabularyTablesDriveTheirEnums proves, for every vocabulary table, that
// (a) the derived word list IS the table's row set, and (b) each word is accepted
// where an author uses it while a non-word is refused. (b) is what a table that
// is merely DECLARED — never wired into the enum — fails.
func TestS3VocabularyTablesDriveTheirEnums(t *testing.T) {
	v := servedSchema(t)
	ctx := v.Context()
	var lifted struct {
		Tables map[string]map[string]any
		Words  map[string][]string
	}
	if err := v.LookupPath(cue.ParsePath("#TestVocabulary")).Decode(&lifted); err != nil {
		t.Fatalf("lift the vocabulary tables: %v", err)
	}
	for _, p := range vocabProbes {
		rows, ok := lifted.Tables[p.name]
		if !ok {
			t.Errorf("no vocabulary table %q in the served schema", p.name)
			continue
		}
		if len(rows) == 0 {
			t.Errorf("vocabulary table %q has no rows", p.name)
			continue
		}
		words, ok := lifted.Words[p.name]
		if !ok {
			t.Errorf("no derived word list for %q in the served schema", p.name)
			continue
		}
		keys := make([]string, 0, len(rows))
		for w := range rows {
			keys = append(keys, w)
		}
		sort.Strings(keys)
		sort.Strings(words)
		if fmt.Sprint(keys) != fmt.Sprint(words) {
			t.Errorf("%q's derived list %v is not its table's row set %v", p.name, words, keys)
		}
		for _, w := range keys {
			if err := v.Unify(ctx.CompileString(p.use(w))).Err(); err != nil {
				t.Errorf("%q word %q is refused where an author uses it: %v", p.name, w, err)
			}
		}
		if err := v.Unify(ctx.CompileString(p.use("zzz-not-a-word"))).Err(); err == nil {
			t.Errorf("%q accepts a non-word — the enum is not the table", p.name)
		}
	}
}
