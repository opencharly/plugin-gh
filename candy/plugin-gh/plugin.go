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
	pb "github.com/opencharly/spec/proto"
)

//go:embed schema/*.cue
var schemaFS embed.FS

const calver = "2026.263.1800"

func NewProvider() pb.ProviderServer { return &provider{} }

func NewMeta() pb.PluginMetaServer {
	return sdk.NewMeta(calver, []sdk.ProvidedCapability{
		{Class: "verb", Word: "gh", InputDef: "#GhInput"},
		{Class: "command", Word: "gh"},
	}, schemaFS)
}

type provider struct{ pb.UnimplementedProviderServer }

func (provider) Invoke(ctx context.Context, req *pb.InvokeRequest) (*pb.InvokeReply, error) {
	switch {
	case req.GetOp() == sdk.OpRun: // command:gh — the standalone CLI
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

	default: // verb:gh — the check surface
		return runVerb(req)
	}
}

// runVerb: the gh: check surface. The op runs; a non-2xx or a failed
// expectation fails the step with the REAL detail (never a bare "failed").
func runVerb(req *pb.InvokeRequest) (*pb.InvokeReply, error) {
	var in params.GhInput
	if len(req.GetParamsJson()) > 0 {
		if err := json.Unmarshal(req.GetParamsJson(), &in); err != nil {
			return nil, fmt.Errorf("gh input decode: %w", err)
		}
	}
	result, err := runOp(context.Background(), in)
	if err != nil {
		b, _ := json.Marshal(map[string]any{"status": "fail", "message": err.Error()})
		return &pb.InvokeReply{ResultJson: b}, nil
	}
	b, _ := json.Marshal(map[string]any{"status": "pass", "result": result})
	return &pb.InvokeReply{ResultJson: b}, nil
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
