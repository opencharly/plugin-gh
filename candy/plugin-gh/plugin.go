// Package plugingh — the canonical GitHub surface for opencharly plugins.
//
// Provides verb:gh (the check-plan surface: `gh: {op: ..., repo: ..., number: ...}`)
// and command:gh (`charly gh <op> ...`, the standalone CLI). The actual GitHub
// work lives in gh/ghkit — the ONE client every opencharly plugin imports
// (R3); this plugin wires it into the charly verb/command registry.
//
// SDD: the CUE schema (schema/gh.cue) is the single source for #GhInput AND
// #GhDocument. Every authored input is validated at load against the served
// schema, and the `document` op validates the marshalled document against
// #GhDocument BEFORE writing it (R8) — the same declaration, two uses.
package plugingh

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"

	"github.com/opencharly/plugin-gh/candy/plugin-gh/gh"
	"github.com/opencharly/plugin-gh/candy/plugin-gh/params"
	"github.com/opencharly/sdk"
	"github.com/opencharly/sdk/kit"
	pb "github.com/opencharly/spec/proto"
	"github.com/opencharly/spec/spec"
)

//go:embed schema/*.cue
var schemaFS embed.FS

const calver = "2026.265.2200"

func NewProvider() pb.ProviderServer { return &provider{} }

func NewMeta() pb.PluginMetaServer {
	return sdk.NewMeta(calver, []sdk.ProvidedCapability{
		{Class: "verb", Word: "gh", InputDef: "#GhInput"},
		{Class: "command", Word: "gh"},
	}, schemaFS)
}

type provider struct{ pb.UnimplementedProviderServer }

func (provider) Invoke(ctx context.Context, req *pb.InvokeRequest) (*pb.InvokeReply, error) {
	// Discriminate on the CAPABILITY CLASS, never the op selector: BOTH the
	// command and the verb surface are dispatched with ops.OpRun (the generic
	// runtime selector — see charly/provider_checkenv.go invokeVerbProvider and
	// candy/plugin-agentteams/plugin.go invokeCommand). A `GetOp() == OpRun`
	// switch therefore cannot tell `charly gh …` (command) from a `gh:` check
	// step (verb), and routed EVERY out-of-process verb call into CliMain with
	// empty args → `gh: exit 2`. Class is the one reliable discriminator; an
	// out-of-tree command never reaches here anyway (charly fork/execs it via
	// syscall.Exec — provider_command_external.go), so the command arm is the
	// compiled-in in-proc placement.
	switch req.GetClass() {
	case "command":
		if req.GetOp() != sdk.OpRun {
			return nil, fmt.Errorf("gh: unsupported command op %q", req.GetOp())
		}
		var in struct {
			Args []string `json:"args"`
		}
		if len(req.GetParamsJson()) > 0 {
			_ = json.Unmarshal(req.GetParamsJson(), &in)
		}
		code := CliMain(in.Args)
		if code != 0 {
			return nil, fmt.Errorf("gh: exit %d", code)
		}
		return &pb.InvokeReply{}, nil

	default: // verb:gh — the check surface (dispatched with ops.OpRun)
		return runVerb(req)
	}
}

// runVerb: the gh: check surface. The framework hands a plugin VERB the FULL
// #Op envelope as params_json (plugin_input nested under `plugin_input`) — the
// SAME shape every sibling out-of-process verb decodes (candy/plugin-bpf/
// provider.go, candy/plugin-appium). The op runs; a non-2xx transport error and
// a failed EXPECTATION are both folded through the shared verdict pipeline
// (sdk.VerbVerdict, R3 — one matcher implementation), so authored
// exit_status/stdout/stderr on the step are evaluated rather than a bare pass.
func runVerb(req *pb.InvokeRequest) (*pb.InvokeReply, error) {
	var op spec.Op
	if len(req.GetParamsJson()) > 0 {
		if err := json.Unmarshal(req.GetParamsJson(), &op); err != nil {
			return sdk.ResultJSON("fail", "gh: decode op: "+err.Error())
		}
	}
	var in params.GhInput
	kit.DecodeInput(op.PluginInput, &in)
	result, err := runOp(context.Background(), in)
	var out string
	if err == nil {
		b, merr := json.Marshal(result)
		if merr != nil {
			err = fmt.Errorf("gh: marshal result: %w", merr)
		} else {
			out = string(b)
		}
	}
	return sdk.VerbVerdict("gh", in.Op, out, err, &op, false)
}

// runOp dispatches one gh op against ghkit (the single implementation).
func runOp(ctx context.Context, in params.GhInput) (map[string]any, error) {
	client, err := gh.New()
	if err != nil {
		return nil, err
	}
	out := map[string]any{"op": in.Op, "repo": in.Repo, "number": in.Number}
	switch in.Op {
	case "pr_meta":
		m, err := client.PRMeta(ctx, in.Repo, in.Number)
		if err != nil {
			return nil, err
		}
		out["title"], out["state"], out["draft"], out["head_sha"] = m.Title, m.State, m.Draft, m.HeadSHA
		out["base"], out["head"], out["file_count"] = m.Base, m.Head, m.FileCount
	case "pr_files":
		paths, err := client.PRPaths(ctx, in.Repo, in.Number)
		if err != nil {
			return nil, err
		}
		out["files"] = paths
	case "pr_diff":
		d, err := client.PRDiff(ctx, in.Repo, in.Number)
		if err != nil {
			return nil, err
		}
		out["diff"] = d
	case "pr_commits":
		cs, err := client.PRCommits(ctx, in.Repo, in.Number)
		if err != nil {
			return nil, err
		}
		out["commits"] = cs
	case "pr_thread":
		th, err := client.PRThread(ctx, in.Repo, in.Number)
		if err != nil {
			return nil, err
		}
		out["body"], out["comments"] = th.Body, th.Comments
	case "head_sha":
		s, err := client.HeadSHA(ctx, in.Repo, in.Number)
		if err != nil {
			return nil, err
		}
		out["head_sha"] = s
	case "document":
		return emitDocument(ctx, client, in)
	default:
		return nil, fmt.Errorf("gh: unknown op %q", in.Op)
	}
	return out, nil
}
