package plugingh

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/opencharly/plugin-gh/candy/plugin-gh/params"
	"github.com/opencharly/sdk/kit"
	pb "github.com/opencharly/spec/proto"
)

// TestInvokeDiscriminatesOnClass pins the dispatch contract that the framework
// actually uses, and that a GetOp()-keyed switch gets wrong. BOTH the command
// and the verb surface are dispatched with ops.OpRun, so only req.Class can
// tell them apart:
//
//   - a `gh:` check step arrives as Class="verb", Op="run", params_json = the
//     full marshalled spec.Op with the authored input nested under
//     "plugin_input";
//   - a compiled-in `charly gh …` command arrives as Class="command",
//     Op="run", params_json = {"args": [...]}.
//
// Before the fix a `GetOp() == sdk.OpRun` switch routed EVERY verb call into
// CliMain with empty args → `gh: exit 2`, so the entire declarative verb
// surface was dead. This test fails on the pre-fix switch and passes on the
// class discriminator, WITHOUT touching the network: the verb arm is exercised
// with an op whose input decode is observable and whose failure is a clean
// {status:"fail"} (never a panic / exit).
func TestInvokeDiscriminatesOnClass(t *testing.T) {
	prov := NewProvider()

	// A verb envelope carrying a bogus op: proves the params_json is decoded as
	// the spec.Op envelope (plugin_input) and reaches runOp — NOT CliMain. The
	// pre-fix code returned the transport error `gh: exit 2` (CliMain with empty
	// args); the fixed code returns a structured {status:"fail"} naming the op.
	verbOp := map[string]any{
		"plugin":       "gh",
		"plugin_input": map[string]any{"op": "no_such_op", "repo": "o/r", "number": 1},
	}
	verbBody, _ := json.Marshal(verbOp)
	rep, err := prov.Invoke(context.Background(), &pb.InvokeRequest{
		Class: "verb", Reserved: "gh", Op: "run", ParamsJson: verbBody,
	})
	if err != nil {
		t.Fatalf("verb dispatch returned a transport error %v (the class switch routed it to the CLI arm)", err)
	}
	var vr struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if e := json.Unmarshal(rep.GetResultJson(), &vr); e != nil {
		t.Fatalf("verb reply is not a {status,message}: %v (%s)", e, rep.GetResultJson())
	}
	if vr.Status != "fail" || !strings.Contains(vr.Message, "no_such_op") {
		t.Fatalf("verb arm did not decode plugin_input into runOp: %+v", vr)
	}

	// A command envelope with no args: the command arm still runs (usage → exit
	// 2), proving the discriminator did not break the CLI placement.
	cmdBody, _ := json.Marshal(map[string]any{"args": []string{}})
	if _, err := prov.Invoke(context.Background(), &pb.InvokeRequest{
		Class: "command", Reserved: "gh", Op: "run", ParamsJson: cmdBody,
	}); err == nil {
		t.Fatal("command arm with no args should fail (usage → non-zero exit), got nil error")
	}

	// An unsupported command op is rejected explicitly, not silently CLI'd.
	if _, err := prov.Invoke(context.Background(), &pb.InvokeRequest{
		Class: "command", Reserved: "gh", Op: "not-run",
	}); err == nil || !strings.Contains(err.Error(), "unsupported command op") {
		t.Fatalf("unsupported command op should be rejected naming the op, got: %v", err)
	}
}

// TestDecodeGhInputFromOpEnvelope proves the envelope decode the fix relies on:
// the authored `gh: {...}` fields ride spec.Op.plugin_input and decode into
// params.GhInput (the same kit.DecodeInput path every sibling out-of-process
// verb uses). It is the decode half of TestInvokeDiscriminatesOnClass, with no
// network and no client.
func TestDecodeGhInputFromOpEnvelope(t *testing.T) {
	var op struct {
		PluginInput map[string]any `json:"plugin_input"`
	}
	raw := []byte(`{"plugin":"gh","plugin_input":{"op":"document","repo":"o/r","number":7,"target":"issue","format":"yaml","include_file_content":false}}`)
	if err := json.Unmarshal(raw, &op); err != nil {
		t.Fatal(err)
	}
	var in params.GhInput
	kit.DecodeInput(op.PluginInput, &in)
	if in.Op != "document" || in.Repo != "o/r" || in.Number != 7 {
		t.Fatalf("core fields did not decode: %+v", in)
	}
	if in.Target != "issue" || in.Format != "yaml" {
		t.Fatalf("document fields did not decode: %+v", in)
	}
	if in.IncludeFileContent == nil || *in.IncludeFileContent {
		t.Fatalf("include_file_content pointer did not decode false: %+v", in.IncludeFileContent)
	}
}
