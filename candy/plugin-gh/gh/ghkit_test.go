package gh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
