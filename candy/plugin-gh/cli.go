package plugingh

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/opencharly/plugin-gh/candy/plugin-gh/params"
)

// CliMain — the command:gh standalone surface:
//
//	charly gh <op> [--repo R|--org O] [--number N] [--target issue|pr] [--format json|yaml] [--out FILE]
//	charly gh issues [--org O|--repo R] [--state open|closed|all] [--kind issue|pr|all] [--since TS] [--limit N] [--include-body]
//
// The SAME runOp the verb surface serves — one implementation, two transports.
func CliMain(args []string) int {
	fs := flag.NewFlagSet("gh", flag.ContinueOnError)
	repo := fs.String("repo", "", "the FULL repo slug (owner/name)")
	org := fs.String("org", "", "the org login (issues op: list the whole org)")
	number := fs.Int("number", 0, "the issue OR pull-request number")
	target := fs.String("target", "", "document only: issue|pr (default auto-detect)")
	format := fs.String("format", "", "document/issues: json|yaml (default json)")
	out := fs.String("out", "", "document/issues: write to this path (default: inline result)")
	includeContent := fs.Bool("include-file-content", false, "document only: include each changed file's full content at head (default true; pass =false to disable)")
	state := fs.String("state", "", "issues only: open|closed|all (default open)")
	kind := fs.String("kind", "", "issues only: issue|pr|all (default all)")
	since := fs.String("since", "", "issues only: only items updated at/after this RFC3339 timestamp")
	limit := fs.Int("limit", 0, "issues only: stop after this many items (0 = all)")
	includeBody := fs.Bool("include-body", false, "issues only: include each item's body (default false)")
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: charly gh <op> [--repo OWNER/NAME | --org ORG] [--number N] [--target issue|pr] [--format json|yaml] [--out FILE]")
		fmt.Fprintln(os.Stderr, "  ops: pr_meta pr_files pr_diff pr_commits pr_thread head_sha document issues")
		fmt.Fprintln(os.Stderr, "  gh --self-test   verify the embedded schema + document serialization (offline)")
		return 2
	}
	if args[0] == "--self-test" {
		return selfTest()
	}
	if err := fs.Parse(args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	in := params.GhInput{
		Op: args[0], Repo: *repo, Org: *org, Number: *number, Target: *target,
		Format: *format, Out: *out, State: *state, Kind: *kind, Since: *since, Limit: *limit,
	}
	// include_file_content (document) defaults true; include_body (issues)
	// defaults false; only an explicit flag sets the pointer.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "include-file-content":
			v := *includeContent
			in.IncludeFileContent = &v
		case "include-body":
			v := *includeBody
			in.IncludeBody = &v
		}
	})
	// Per-op required args: the reads need repo+number, the issues list needs
	// exactly one scope (org xor repo).
	if in.Op == "issues" {
		if (in.Org == "") == (in.Repo == "") {
			fmt.Fprintln(os.Stderr, "gh: issues requires exactly one of --org or --repo")
			return 2
		}
	} else if in.Number <= 0 || in.Repo == "" {
		fmt.Fprintln(os.Stderr, "gh: --repo and --number are required")
		return 2
	}
	res, err := runOp(context.Background(), in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gh:", err)
		return 1
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(b))
	return 0
}
