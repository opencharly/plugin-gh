package gh

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

// live_test.go — the LIVE read proof. The unit tests above stub the HTTP
// transport (that is ghkit's own boundary for the decode/pagination LOGIC); they
// cannot prove the real API contract. These tests read a REAL PR through the
// real client.
//
// Per the project rulebook (AGENTS.md): a test that exercises a SERVICE must run
// against the REAL service or SKIP — never a fabricated response. So these tests
// require an EXPLICIT opt-in AND a reachable API, and SKIP otherwise. A skip here
// is the honest outcome ("no live service"), not a pass; the live assertion is
// only claimed when the run reports PASS with the pasted output in the PR body.
//
//	GHKIT_LIVE_REPO  REQUIRED to opt in (e.g. opencharly/plugin-review) — there is
//	                 no hardcoded cross-repo default, so the suite never reaches
//	                 the network unless the operator asks it to.
//	GHKIT_LIVE_PR    the PR number (default 13)
//	GH_TOKEN/GITHUB_TOKEN or an authenticated gh CLI: the credential.
func liveClient(t *testing.T) (*Client, string, int) {
	t.Helper()
	repo := os.Getenv("GHKIT_LIVE_REPO")
	if repo == "" {
		t.Skip("SKIP: set GHKIT_LIVE_REPO to run the live GitHub read tests (no hardcoded default — the suite never reaches the network unasked)")
	}
	pr := 13
	if v := os.Getenv("GHKIT_LIVE_PR"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			pr = n
		}
	}
	c, err := New()
	if err != nil {
		t.Skipf("SKIP: no client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := c.PRMeta(ctx, repo, pr); err != nil {
		t.Skipf("SKIP: live GitHub read of %s#%d failed: %v (set GH_TOKEN for authenticated reads)", repo, pr, err)
	}
	return c, repo, pr
}

func TestLivePRFilesCarryPatches(t *testing.T) {
	c, repo, pr := liveClient(t)
	files, err := c.PRFiles(context.Background(), repo, pr)
	if err != nil {
		t.Fatalf("PRFiles: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("%s#%d has no changed files", repo, pr)
	}
	var withPatch int
	for _, f := range files {
		if f.Patch != "" {
			withPatch++
		}
		if f.Path == "" || f.Status == "" {
			t.Fatalf("a real file row must carry path+status: %+v", f)
		}
	}
	if withPatch == 0 {
		t.Fatal("no file carried a patch — the per-file delivery depends on it")
	}
	t.Logf("LIVE PRFiles %s#%d: %d files, %d with patches", repo, pr, len(files), withPatch)
}

func TestLivePRCommentsCarryIDs(t *testing.T) {
	c, repo, pr := liveClient(t)
	cms, err := c.PRComments(context.Background(), repo, pr)
	if err != nil {
		t.Fatalf("PRComments: %v", err)
	}
	for _, cm := range cms {
		if cm.ID == 0 {
			t.Fatalf("a real comment must carry an id: %+v", cm)
		}
	}
	if len(cms) > 0 {
		// read one back by id — the index->body contract
		got, err := c.PRComment(context.Background(), repo, cms[0].ID)
		if err != nil {
			t.Fatalf("PRComment(%d): %v", cms[0].ID, err)
		}
		if got.Body != cms[0].Body {
			t.Fatalf("PRComment body != list body for id %d", cms[0].ID)
		}
	}
	t.Logf("LIVE PRComments %s#%d: %d comments", repo, pr, len(cms))
}
