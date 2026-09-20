package gh

import (
	"context"
	"os"
	"testing"
	"time"
)

// live_test.go — the LIVE read proof. The unit tests above stub the HTTP
// transport (that is ghkit's own boundary for decode/pagination logic); they
// cannot prove the real API contract. This test reads a REAL public PR through
// the real client and SKIPS when no token/API is reachable — never a fabricated
// response.
//
//	GH_TOKEN / GITHUB_TOKEN: the live credential (public reads also work
//	                         unauthenticated but rate-limit hard).
//	GHKIT_LIVE_REPO default opencharly/plugin-review
//	GHKIT_LIVE_PR   default 13
func liveClient(t *testing.T) (*Client, string, int) {
	t.Helper()
	c, err := New()
	if err != nil {
		t.Skipf("SKIP: no client: %v", err)
	}
	repo := os.Getenv("GHKIT_LIVE_REPO")
	if repo == "" {
		repo = "opencharly/plugin-review"
	}
	pr := 13
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
