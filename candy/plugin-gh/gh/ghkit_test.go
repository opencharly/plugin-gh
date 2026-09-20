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
		// page 1: exactly 100 rows so the pager must continue
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
	if files[0].Patch == "" || files[0].Binary {
		t.Fatalf("a file WITH a patch must not be marked binary: %+v", files[0])
	}
}

// TestPRFiles_BinaryHasNoPatch pins the binary signal: a file the API returns
// without a patch is marked Binary, so a caller never reads "no patch" as "no
// change".
func TestPRFiles_BinaryHasNoPatch(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"filename":"logo.png","status":"added","additions":0,"deletions":0}]`))
	})
	files, err := c.PRFiles(context.Background(), "o/r", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || !files[0].Binary {
		t.Fatalf("a patch-less file must be Binary: %+v", files)
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
