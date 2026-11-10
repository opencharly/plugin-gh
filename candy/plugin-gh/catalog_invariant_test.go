package plugingh

// catalog_invariant_test.go pins the invariant the jetkvm and cua siblings keep
// and this plugin needs for the same reason: every op the SCHEMA advertises must
// be reachable — routed by runOp — and every op runOp routes must be in the
// schema. Without it an op can sit in the catalog as a phantom (advertised but
// unimplemented): the schema validates the authored `gh: {...}`, and the step
// then fails only at dispatch with `unknown op`.
//
// The tests read the REAL sources — the SERVED enum (through the CUE API,
// schema_served_test.go) and plugin.go's runOp switch — rather than restating
// either, so they cannot drift alongside the code they guard.

import (
	"os"
	"regexp"
	"sort"
	"testing"
)

// schemaOps returns the sorted op names the SERVED #GhInput.op enum accepts. It
// reads the schema through the CUE API rather than its text: the enum is now
// §10.1 S3 (a vocabulary table plus a derived word list), and a text parse of
// the union would find ZERO ops and leave every invariant below passing
// vacuously.
func schemaOps(t *testing.T) []string {
	t.Helper()
	return schemaWordList(t, servedSchema(t), "#GhInput.op")
}

// routedOps returns the op names runOp routes. It reads the switch's
// `case "…":` arms out of plugin.go, scoped to the runOp function so no other
// switch in the file is mistaken for dispatch.
func routedOps(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("plugin.go")
	if err != nil {
		t.Fatalf("read plugin: %v", err)
	}
	fn := regexp.MustCompile(`(?s)func runOp\(.*?\n\}`).FindString(string(b))
	if fn == "" {
		t.Fatal("could not locate runOp in plugin.go")
	}
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`case "([a-z][a-z0-9_]*)":`).FindAllStringSubmatch(fn, -1) {
		seen[m[1]] = true
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// TestCatalogHasNoPhantomOps asserts every schema op is routed — no op is
// advertised that the plugin cannot act on.
func TestCatalogHasNoPhantomOps(t *testing.T) {
	reachable := map[string]bool{}
	for _, m := range routedOps(t) {
		reachable[m] = true
	}
	if len(reachable) == 0 {
		t.Fatal("the dispatch surface is empty — the invariant would pass over nothing")
	}
	for _, m := range schemaOps(t) {
		if !reachable[m] {
			t.Errorf("schema op %q is a PHANTOM: runOp does not route it — add an implementation or remove it from the schema", m)
		}
	}
}

// TestDispatchHasNoPhantomOpArms is the other direction: an arm for a word the
// schema no longer accepts is dead code, and it hides a rename that only half
// landed.
func TestDispatchHasNoPhantomOpArms(t *testing.T) {
	advertised := map[string]bool{}
	for _, m := range schemaOps(t) {
		advertised[m] = true
	}
	for _, m := range routedOps(t) {
		if !advertised[m] {
			t.Errorf("runOp routes %q, which the served #GhInput.op enum does NOT accept — the arm is unreachable", m)
		}
	}
}

// TestEverySchemaOpIsRouted is the two-way set equality in one assertion, so a
// reader sees the whole invariant at once.
func TestEverySchemaOpIsRouted(t *testing.T) {
	schema := schemaOps(t)
	routed := routedOps(t)
	if len(schema) != len(routed) {
		t.Fatalf("served enum has %d ops but runOp routes %d — the catalog and the dispatch have drifted", len(schema), len(routed))
	}
	for i := range schema {
		if schema[i] != routed[i] {
			t.Errorf("served op %q has no matching routed arm (routed=%v)", schema[i], routed)
			break
		}
	}
}
