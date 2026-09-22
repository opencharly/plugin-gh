package gh

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// document_test.go — the structured issue/PR assembly. Fails without the read:
// the PR document must carry the reviews, the inline review comments, and (when
// requested) each changed file's FULL content at head.

// docServer routes the endpoints the document assembly reads, returning canned
// JSON keyed by path suffix.
func docServer(t *testing.T) *Client {
	t.Helper()
	return cachedTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/issues/7"):
			_, _ = w.Write([]byte(`{"number":7,"title":"T","state":"open","created_at":"c","updated_at":"u","body":"B","html_url":"https://x/7","user":{"login":"alice"},"labels":[{"name":"bug"}],"assignees":[{"login":"bob"}]}`))
		case strings.HasSuffix(p, "/issues/7/comments"):
			_, _ = w.Write([]byte(`[{"id":1,"user":{"login":"carol"},"created_at":"c1","body":"a comment"}]`))
		case strings.HasSuffix(p, "/pulls/7/reviews"):
			_, _ = w.Write([]byte(`[{"id":9,"user":{"login":"dave"},"state":"APPROVED","body":"lgtm","submitted_at":"s1"}]`))
		case strings.HasSuffix(p, "/pulls/7/comments"):
			_, _ = w.Write([]byte(`[{"id":5,"user":{"login":"erin"},"path":"a.go","line":3,"side":"RIGHT","created_at":"c2","body":"nit","in_reply_to_id":4}]`))
		case strings.HasSuffix(p, "/pulls/7/commits"):
			_, _ = w.Write([]byte(`[{"sha":"abc123","commit":{"message":"feat: x\n\nbody","author":{"name":"A"}}}]`))
		case strings.HasSuffix(p, "/pulls/7/files"):
			_, _ = w.Write([]byte(`[{"filename":"a.go","status":"modified","additions":2,"deletions":1,"patch":"@@ -1 +1 @@\n-a\n+b","sha":"blob1"}]`))
		case strings.HasSuffix(p, "/pulls/7"):
			_, _ = w.Write([]byte(`{"title":"T","state":"open","draft":false,"changed_files":1,"head":{"sha":"head1","ref":"feat"},"base":{"ref":"main"}}`))
		case strings.HasSuffix(p, "/git/blobs/blob1"):
			_, _ = w.Write([]byte(`{"content":"cGFja2FnZSBtYWluCg==","encoding":"base64","size":13}`))
		default:
			http.NotFound(w, r)
		}
	})
}

func TestAssembleDocument_PRTier1(t *testing.T) {
	c := docServer(t)
	doc, err := c.AssembleDocument(context.Background(), "o/r", 7, "pr", true)
	if err != nil {
		t.Fatalf("AssembleDocument: %v", err)
	}
	if doc.Kind != "pr" {
		t.Fatalf("kind = %q, want pr", doc.Kind)
	}
	if doc.Number != 7 || doc.Author != "alice" || doc.Body != "B" {
		t.Fatalf("identity block = %+v", doc)
	}
	if len(doc.Comments) != 1 || doc.Comments[0].Author != "carol" {
		t.Fatalf("comments = %+v", doc.Comments)
	}
	if doc.PR == nil {
		t.Fatal("a PR document must carry the pr block")
	}
	if len(doc.PR.Reviews) != 1 || doc.PR.Reviews[0].State != "APPROVED" {
		t.Fatalf("reviews = %+v", doc.PR.Reviews)
	}
	if len(doc.PR.ReviewComments) != 1 || doc.PR.ReviewComments[0].Path != "a.go" {
		t.Fatalf("review comments = %+v", doc.PR.ReviewComments)
	}
	if rc := doc.PR.ReviewComments[0]; rc.InReplyToID == nil || *rc.InReplyToID != 4 {
		t.Fatalf("in_reply_to_id must round-trip, got %+v", rc.InReplyToID)
	}
	if len(doc.PR.Files) != 1 {
		t.Fatalf("files = %+v", doc.PR.Files)
	}
	f := doc.PR.Files[0]
	if f.BlobSHA != "blob1" || f.Content == "" || f.ContentEncoding != "utf-8" {
		t.Fatalf("file content must be fetched + decoded: %+v", f)
	}
	if !strings.Contains(f.Content, "package main") {
		t.Fatalf("decoded content = %q", f.Content)
	}
	if doc.PR.HeadSHA != "head1" {
		t.Fatalf("head_sha = %q", doc.PR.HeadSHA)
	}
}

// TestAssembleDocument_IssueOmitsPR pins the auto-detect: a number that does not
// resolve to a pull request yields an ISSUE document with no pr block.
func TestAssembleDocument_IssueOmitsPR(t *testing.T) {
	c := cachedTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/issues/3"):
			_, _ = w.Write([]byte(`{"number":3,"title":"I","state":"open","body":"x","html_url":"u","user":{"login":"a"}}`))
		case strings.HasSuffix(p, "/issues/3/comments"):
			_, _ = w.Write([]byte(`[]`))
		case strings.Contains(p, "/pulls/3"):
			http.NotFound(w, r) // not a PR
		default:
			http.NotFound(w, r)
		}
	})
	doc, err := c.AssembleDocument(context.Background(), "o/r", 3, "", true)
	if err != nil {
		t.Fatalf("AssembleDocument: %v", err)
	}
	if doc.Kind != "issue" {
		t.Fatalf("kind = %q, want issue", doc.Kind)
	}
	if doc.PR != nil {
		t.Fatal("an issue document must omit the pr block")
	}
}

// TestAssembleDocument_AutoDetectSurfacesRealError pins finding 6: in auto-detect
// mode a NON-404 failure from the PR probe must surface, never silently yield an
// issue document.
func TestAssembleDocument_AutoDetectSurfacesRealError(t *testing.T) {
	c := cachedTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/3"):
			_, _ = w.Write([]byte(`{"number":3,"title":"I","state":"open","body":"x","html_url":"u","user":{"login":"a"}}`))
		case strings.HasSuffix(r.URL.Path, "/issues/3/comments"):
			_, _ = w.Write([]byte(`[]`))
		case strings.Contains(r.URL.Path, "/pulls/3"):
			w.WriteHeader(http.StatusInternalServerError) // NOT a 404 — a real failure
			_, _ = w.Write([]byte(`{"message":"boom"}`))
		default:
			http.NotFound(w, r)
		}
	})
	if _, err := c.AssembleDocument(context.Background(), "o/r", 3, "", true); err == nil {
		t.Fatal("a non-404 probe failure must surface, not yield an issue document")
	}
}

// TestAssembleDocument_NoContentWhenDisabled pins include_file_content=false: the
// file keeps its diff but fetches no content.
func TestAssembleDocument_NoContentWhenDisabled(t *testing.T) {
	c := docServer(t)
	doc, err := c.AssembleDocument(context.Background(), "o/r", 7, "pr", false)
	if err != nil {
		t.Fatalf("AssembleDocument: %v", err)
	}
	f := doc.PR.Files[0]
	if f.Content != "" || f.NoPatch {
		t.Fatalf("content must be omitted when disabled (and the diff kept): %+v", f)
	}
}

// TestAssembleDocument_ProvenanceCachedOnWarmRead pins the provenance flag: a
// second assembly against the same stub (unchanged upstream) reports Cached=true,
// while the first reports false.
func TestAssembleDocument_ProvenanceCachedOnWarmRead(t *testing.T) {
	c := docServer(t)
	first, err := c.AssembleDocument(context.Background(), "o/r", 7, "pr", false)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.Provenance.Cached {
		t.Fatal("a cold assembly must report Cached=false")
	}
	second, err := c.AssembleDocument(context.Background(), "o/r", 7, "pr", false)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.Provenance.Cached {
		t.Fatal("a warm assembly must report Cached=true (served from the store)")
	}
	// The immutable blob content read also reports a hit on the second pass only
	// when included; here includeContent=false so the content read does not run.
}

// TestAssembleDocument_NoNullLists pins the schema invariant the self-test
// guards: every REQUIRED list in #GhDocument must serialize as a list (never
// null), because the served def rejects null. The assembler make()s every slice,
// so a real document always has [] where a list is required.
func TestAssembleDocument_NoNullLists(t *testing.T) {
	c := docServer(t)
	doc, err := c.AssembleDocument(context.Background(), "o/r", 7, "pr", false)
	if err != nil {
		t.Fatalf("AssembleDocument: %v", err)
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"reviews":null`, `"review_comments":null`, `"files":null`, `"commits":null`, `"comments":null`, `"labels":null`} {
		if strings.Contains(string(b), forbidden) {
			t.Fatalf("a required list serialized as null: %s\n%s", forbidden, b)
		}
	}
}

// TestIsBinary pins the binary heuristic (NUL + invalid UTF-8).
func TestIsBinary(t *testing.T) {
	if isBinary([]byte("hello world")) {
		t.Error("plain text must not be binary")
	}
	if !isBinary([]byte{'a', 0, 'b'}) {
		t.Error("a NUL byte must be binary")
	}
	if !isBinary([]byte{0xff, 0xfe, 0x00}) {
		t.Error("invalid UTF-8 must be binary")
	}
}
