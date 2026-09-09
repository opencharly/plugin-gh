package plugingh

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/opencharly/plugin-gh/candy/plugin-gh/params"
)

// CliMain — the command:gh standalone surface: `charly gh <op> --repo R --pr N`.
// The SAME runOp the verb surface serves — one implementation, two transports.
func CliMain(args []string) int {
	fs := flag.NewFlagSet("gh", flag.ContinueOnError)
	repo := fs.String("repo", "", "the FULL repo slug (owner/name)")
	pr := fs.Int("pr", 0, "the PR number")
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: charly gh <op> --repo OWNER/NAME --pr N   (ops: pr_meta pr_files pr_diff pr_commits pr_thread head_sha)")
		return 2
	}
	if err := fs.Parse(args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	in := params.GhInput{Op: args[0], Repo: *repo, Pr: *pr}
	if in.Pr <= 0 || in.Repo == "" {
		fmt.Fprintln(os.Stderr, "gh: --repo and --pr are required")
		return 2
	}
	out, err := runOp(context.Background(), in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gh:", err)
		return 1
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	return 0
}
