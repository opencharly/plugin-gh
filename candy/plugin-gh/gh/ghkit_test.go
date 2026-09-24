package gh

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, Token: "test-token", HTTP: srv.Client()}
}

// linkNext writes the Link header GitHub emits iff there IS a next page —
// `rel="next"` pointing at the same query with page=N. Modeling this in the
// stubs is load-bearing: pagination follows rel="next" and ONLY rel="next", so
// a stub that omits it makes its list look single-page (the defect the old
// page-count stubs papered over).
func linkNext(w http.ResponseWriter, r *http.Request, page int) {
	u := *r.URL
	q := u.Query()
	q.Set("page", itoa(page))
	u.RawQuery = q.Encode()
	w.Header().Set("Link", `<http://`+r.Host+u.RequestURI()+`>; rel="next"`)
}

func TestGet_Non2xxSurfacesStatusAndBody(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	err := c.Get(context.Background(), "/repos/x/pulls/1", nil)
	if err == nil {
		t.Fatal("want an error for the 404")
	}
	for _, want := range []string{"404", "Not Found", "/repos/x/pulls/1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must surface %q, got: %v", want, err)
		}
	}
}

func TestPRMeta_DecodesTheWire(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("the auth header must ride every call")
		}
		_, _ = w.Write([]byte(`{"title":"T","state":"open","draft":false,"changed_files":7,"head":{"sha":"abc","ref":"feat"},"base":{"ref":"main"}}`))
	})
	m, err := c.PRMeta(context.Background(), "omacom/omarchy", 10144)
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "T" || m.HeadSHA != "abc" || m.FileCount != 7 || m.Base != "main" {
		t.Errorf("decoded = %+v", m)
	}
}

func TestPRPaths_SpaceSeparated(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"filename":"a.qml"},{"filename":"b.sh"}]`))
	})
	paths, err := c.PRPaths(context.Background(), "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if paths != "a.qml b.sh" {
		t.Errorf("paths = %q, want the space-separated pr-apply contract", paths)
	}
}

func TestResolveToken_PriorityAndHostsFallback(t *testing.T) {
	// 1. GH_TOKEN wins
	os.Setenv("GH_TOKEN", "env-token")
	tok, src, err := resolveToken()
	if err != nil || tok != "env-token" || src != "env:GH_TOKEN" {
		t.Fatalf("GH_TOKEN resolution = %q %q %v", tok, src, err)
	}
	os.Unsetenv("GH_TOKEN")

	// 2. the hosts.yml fallback (save/restore HOME — the env is shared state)
	origHome, hadHome := os.LookupEnv("HOME")
	home := t.TempDir()
	os.Setenv("HOME", home)
	defer func() {
		if hadHome {
			os.Setenv("HOME", origHome)
		} else {
			os.Unsetenv("HOME")
		}
	}()
	dir := filepath.Join(home, ".config", "gh")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	hosts := "github.com:\n    oauth_token: hosts-token\n    user: x\n"
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, src, err = resolveToken()
	if err != nil || tok != "hosts-token" || src != "hosts.yml:github.com" {
		t.Fatalf("hosts.yml resolution = %q %q %v", tok, src, err)
	}

	// 3. nothing anywhere: a REAL error naming every layer, never a silent empty
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	_, _, err = resolveToken()
	if err == nil || !strings.Contains(err.Error(), "gh auth login") {
		t.Errorf("the missing-token error must tell the operator what to do, got: %v", err)
	}
}

func TestNew_BaseURLNeverRedirectsThroughHosts(t *testing.T) {
	os.Unsetenv("GITHUB_API_URL")
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != DefaultBaseURL {
		t.Errorf("base = %q, want the explicit api.github.com (the hosts.yml default-host redirect is the opaque-404 class)", c.BaseURL)
	}
}

// TestPRFiles_PerFilePatchAndPagination pins the two load-bearing properties:
// every file carries its OWN patch (so a caller can deliver one file per
// message), and a >100-file list is paged to completion (a single page would
// silently drop the tail).
func TestPRFiles_PerFilePatchAndPagination(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page == "2" {
			_, _ = w.Write([]byte(`[{"filename":"big.go","status":"modified","additions":50,"deletions":1,"patch":"@@ -1 +1 @@\n-old\n+new"}]`))
			return
		}
		// page 1: exactly 100 rows AND a rel="next" Link (GitHub emits the Link
		// header iff there is a next page) so the pager must continue.
		linkNext(w, r, 2)
		rows := make([]string, 100)
		for i := range rows {
			rows[i] = `{"filename":"f` + itoa(i) + `.go","status":"modified","additions":1,"deletions":0,"patch":"@@ -1 +1 @@\n-a\n+b"}`
		}
		_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
	})
	files, err := c.PRFiles(context.Background(), "o/r", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 101 {
		t.Fatalf("files = %d, want 101 (page 1 of 100 + page 2 of 1 — pagination must follow)", len(files))
	}
	last := files[len(files)-1]
	if last.Path != "big.go" || last.Patch == "" {
		t.Fatalf("the second-page file must carry its own patch: %+v", last)
	}
	if files[0].Patch == "" || files[0].NoPatch {
		t.Fatalf("a file WITH a patch must not be marked binary: %+v", files[0])
	}
}

// TestPRFiles_NoPatchFlag pins the NoPatch signal: a file the API returns
// without patch text is marked NoPatch (not silently "binary"), so a caller
// never reads "no patch" as "no change" NOR assumes it is a binary file.
func TestPRFiles_NoPatchFlag(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"filename":"logo.png","status":"added","additions":0,"deletions":0}]`))
	})
	files, err := c.PRFiles(context.Background(), "o/r", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || !files[0].NoPatch {
		t.Fatalf("a patch-less file must be NoPatch: %+v", files)
	}
}

// TestPRComment_ById pins the single-comment read (the pair to the thread index).
func TestPRComment_ById(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/issues/comments/42") {
			t.Errorf("must fetch the single comment by id, got %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":42,"user":{"login":"bot"},"created_at":"2026-01-01T00:00:00Z","body":"finding x"}`))
	})
	cm, err := c.PRComment(context.Background(), "o/r", 42)
	if err != nil {
		t.Fatal(err)
	}
	if cm.ID != 42 || cm.Author != "bot" || cm.Body != "finding x" {
		t.Fatalf("decoded = %+v", cm)
	}
}

// TestPostComment_SendsBodyAndSurfacesFailure pins the POST path: the body
// rides the request, and a non-2xx is an error with the status.
func TestPostComment_SendsBodyAndSurfacesFailure(t *testing.T) {
	var got string
	ok := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("must POST, got %s", r.Method)
		}
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		_, _ = w.Write([]byte(`{"id":1}`))
	})
	if err := ok.PostComment(context.Background(), "o/r", 3, "hello"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"body":"hello"`) {
		t.Fatalf("posted body = %q", got)
	}
	bad := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"no"}`))
	})
	if err := bad.PostComment(context.Background(), "o/r", 3, "x"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a failed post must surface the status, got: %v", err)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// TestPost_UnmarshalableBodyIsAnError pins the marshal-error path: a body that
// cannot be encoded is a real error AND no request is sent — never a silent
// empty POST with a nil return.
func TestPost_UnmarshalableBodyIsAnError(t *testing.T) {
	var called bool
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{}`))
	})
	// a channel cannot be marshalled by encoding/json
	err := c.Post(context.Background(), "/repos/o/r/issues/1/comments", map[string]any{"body": make(chan int)}, nil)
	if err == nil {
		t.Fatal("an unmarshalable body must return an error")
	}
	if !strings.Contains(err.Error(), "encode request body") {
		t.Fatalf("the error must name the encode failure, got: %v", err)
	}
	if called {
		t.Fatal("no request may be sent when the body fails to encode")
	}
}

// TestPRComments_IndexWithIds pins the list-with-ids read: every comment's id
// rides the index (so a caller can fetch a body by id) and a >100-comment
// thread is paged complete.
func TestPRComments_IndexWithIds(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`[{"id":101,"user":{"login":"b"},"created_at":"t","body":"last"}]`))
			return
		}
		linkNext(w, r, 2)
		rows := make([]string, 100)
		for i := range rows {
			rows[i] = `{"id":` + itoa(i+1) + `,"user":{"login":"a"},"created_at":"t","body":"c"}`
		}
		_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
	})
	cms, err := c.PRComments(context.Background(), "o/r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(cms) != 101 {
		t.Fatalf("comments = %d, want 101 (paged complete)", len(cms))
	}
	if cms[100].ID != 101 || cms[100].Body != "last" {
		t.Fatalf("the last comment must carry its id + body: %+v", cms[100])
	}
}

// TestDo_EmptyTokenOmitsAuthorization pins the anonymous-read contract: an empty
// token must NOT emit `Authorization: Bearer ` — GitHub answers 401 "Bad
// credentials" to that header, turning a public read into a hard failure. With a
// token the header is present; without one it is absent entirely.
func TestDo_EmptyTokenOmitsAuthorization(t *testing.T) {
	var sawAuth string
	var hadAuth bool
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		_, hadAuth = r.Header["Authorization"]
		_, _ = w.Write([]byte(`[]`))
	})
	c.Token = ""
	if _, _, _, err := c.getCached(context.Background(), "/x", "application/vnd.github+json", nil); err != nil {
		t.Fatal(err)
	}
	if hadAuth {
		t.Fatalf("an empty token must OMIT the Authorization header, got %q", sawAuth)
	}
	c.Token = "tok"
	if _, _, _, err := c.getCached(context.Background(), "/y", "application/vnd.github+json", nil); err != nil {
		t.Fatal(err)
	}
	if sawAuth != "Bearer tok" {
		t.Fatalf("a token must be sent, got %q", sawAuth)
	}
}
