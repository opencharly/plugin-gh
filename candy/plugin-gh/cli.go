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
//	charly gh <op> --repo R --number N [--target issue|pr] [--format json|yaml] [--out FILE]
//
// The SAME runOp the verb surface serves — one implementation, two transports.
func CliMain(args []string) int {
	fs := flag.NewFlagSet("gh", flag.ContinueOnError)
	repo := fs.String("repo", "", "the FULL repo slug (owner/name)")
	number := fs.Int("number", 0, "the issue OR pull-request number")
	target := fs.String("target", "", "document only: issue|pr (default auto-detect)")
	format := fs.String("format", "", "document only: json|yaml (default json)")
	out := fs.String("out", "", "document only: write to this path (default: inline result)")
	includeContent := fs.Bool("include-file-content", false, "document only: include each changed file's full content at head (default true; pass =false to disable)")
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: charly gh <op> --repo OWNER/NAME --number N [--target issue|pr] [--format json|yaml] [--out FILE]")
		fmt.Fprintln(os.Stderr, "  ops: pr_meta pr_files pr_diff pr_commits pr_thread head_sha document")
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
	in := params.GhInput{Op: args[0], Repo: *repo, Number: *number, Target: *target, Format: *format, Out: *out}
	if in.Number <= 0 || in.Repo == "" {
		fmt.Fprintln(os.Stderr, "gh: --repo and --number are required")
		return 2
	}
	// include_file_content defaults true; a flag explicitly visited overrides.
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "include-file-content" {
			v := *includeContent
			in.IncludeFileContent = &v
		}
	})
	res, err := runOp(context.Background(), in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gh:", err)
		return 1
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(b))
	return 0
}
