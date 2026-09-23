package plugingh

import (
	"strings"
	"testing"
)

// TestGhInputRejectsLegacyPr pins the hard-cutover contract for the `pr`→`number`
// rename: the legacy `pr` field is a hard validation error at the verb input,
// not a silently-ignored key. The host validates every authored `gh: {...}`
// input against the served #GhInput (validateAuthoredPluginInput), and a CUE
// STRUCT DEFINITION is closed by default, so an unknown field is rejected. This
// test proves it against the SAME embedded schema the host splices.
func TestGhInputRejectsLegacyPr(t *testing.T) {
	v := newDocumentValidator()
	if v == nil {
		t.Fatal("embedded schema failed to compile — validator unavailable")
	}
	// A valid input (modern field) must pass.
	if err := v.ValidateJSON("#GhInput", []byte(`{"op":"document","repo":"o/r","number":1}`)); err != nil {
		t.Fatalf("a valid #GhInput was rejected: %v", err)
	}
	// The legacy `pr` field must be REJECTED.
	err := v.ValidateJSON("#GhInput", []byte(`{"op":"pr_meta","repo":"o/r","number":1,"pr":2}`))
	if err == nil {
		t.Fatal("legacy `pr` field was ACCEPTED — #GhInput must reject an unknown field")
	}
	if !strings.Contains(err.Error(), "pr") {
		t.Fatalf("rejection should name the offending field, got: %v", err)
	}
}
