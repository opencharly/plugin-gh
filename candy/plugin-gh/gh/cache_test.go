package gh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// cache_test.go — the response cache's two validity modes. Each test FAILS
// without its behavior: the mutable ETag revalidation and the immutable
// component cache.

// cachedTestClient builds a Client over a stub server WITH a fresh temp cache
// (CHARLY_GH_CACHE isolated per test).
func cachedTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	t.Setenv(cacheEnvName, t.TempDir())
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, Token: "t", HTTP: srv.Client()}
	c.cache = newResponseCache()
	return c
}

// TestMutable_ETagRevalidation pins the mutable contract: an unchanged upstream
// (304) serves the cached body WITHOUT a re-fetch, and a CHANGED body is picked
// up.
func TestMutable_ETagRevalidation(t *testing.T) {
	var requests atomic.Int32
	body := "v1"
	etag := `"e1"`
	c := cachedTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = w.Write([]byte(`"` + body + `"`))
	})

	var got string
	if err := c.Get(context.Background(), "/x", &got); err != nil || got != "v1" {
		t.Fatalf("first read = %q err=%v", got, err)
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("first read made %d requests, want 1", n)
	}
	// Second read within TTL: memo/store hit, no request.
	if err := c.Get(context.Background(), "/x", &got); err != nil || got != "v1" {
		t.Fatalf("second read = %q err=%v", got, err)
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("a warm read must not hit the network, got %d requests", n)
	}
	// Expire the TTL: the next read revalidates with If-None-Match -> 304 -> body.
	c.cache.memo = map[string]memoEntry{}
	storeKey := "application/vnd.github+json\x00/x\x00"
	e, _ := c.cache.store.Get(storeKey)
	e.Resolved = time.Now().Add(-2 * CacheTTL)
	c.cache.store.PutEntry(storeKey, e)
	if err := c.Get(context.Background(), "/x", &got); err != nil || got != "v1" {
		t.Fatalf("revalidated read = %q err=%v", got, err)
	}
	if n := requests.Load(); n != 2 {
		t.Fatalf("revalidation must issue exactly one conditional request, got %d", n)
	}

	// Upstream changes: the next expired read fetches the NEW body (a new ETag).
	body = "v2"
	etag = `"e2"`
	c.cache.memo = map[string]memoEntry{}
	e, _ = c.cache.store.Get(storeKey)
	e.Resolved = time.Now().Add(-2 * CacheTTL)
	c.cache.store.PutEntry(storeKey, e)
	if err := c.Get(context.Background(), "/x", &got); err != nil || got != "v2" {
		t.Fatalf("after upstream change = %q err=%v (want v2)", got, err)
	}
}

// TestImmutable_CachedByComponents pins the immutable contract: two reads of the
// same blob SHA make ONE request (the SHA is the identity).
func TestImmutable_CachedByComponents(t *testing.T) {
	var requests atomic.Int32
	c := cachedTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("ETag", `"blob"`)
		_, _ = w.Write([]byte(`{"content":"aGk=","encoding":"base64","size":2}`))
	})

	for range 3 {
		b, err := c.BlobContent(context.Background(), "o/r", "sha123")
		if err != nil || string(b) != "hi" {
			t.Fatalf("BlobContent = %q err=%v", b, err)
		}
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("an immutable blob read must fetch ONCE, got %d requests", n)
	}
}

// TestHTTPError_IsNotFound pins the typed-status contract the PR auto-detect
// relies on: a 404 is identified without string-matching, and any other non-2xx
// is NOT a not-found.
func TestHTTPError_IsNotFound(t *testing.T) {
	c := cachedTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	err := c.Get(context.Background(), "/repos/o/r/pulls/1", nil)
	if !IsNotFound(err) {
		t.Fatalf("a 404 must satisfy IsNotFound, got %v", err)
	}
	c2 := cachedTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	})
	err2 := c2.Get(context.Background(), "/x", nil)
	if IsNotFound(err2) {
		t.Fatalf("a 500 must NOT satisfy IsNotFound, got %v", err2)
	}
}

// TestGetCached_DecodesIntoOut is a guard that the cached path still decodes.
func TestGetCached_DecodesIntoOut(t *testing.T) {
	c := cachedTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"title":"T"}`))
	})
	var out struct {
		Title string `json:"title"`
	}
	if err := c.Get(context.Background(), "/p", &out); err != nil || out.Title != "T" {
		t.Fatalf("cached decode = %+v err=%v", out, err)
	}
}

// TestResponseCache_ConcurrentSameURLNoDeadlock pins the CONCURRENCY safety of a
// cold-read stampede on one key: every goroutine must complete and observe the
// body (the store's atomic publish + the memo's mutex mean no torn read and no
// deadlock). It deliberately does NOT assert a request count — concurrent cold
// reads legitimately race past the memo and each fetch; single-flight is the
// Store.Fill contract (used by the loader), not the mutable-ETag read path.
func TestResponseCache_ConcurrentSameURLNoDeadlock(t *testing.T) {
	var requests atomic.Int32
	c := cachedTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		time.Sleep(2 * time.Millisecond)
		_, _ = w.Write([]byte(`{"n":1}`))
	})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out map[string]any
			if err := c.Get(context.Background(), "/same", &out); err != nil {
				t.Errorf("concurrent read: %v", err)
			}
		}()
	}
	wg.Wait()
	if requests.Load() < 1 {
		t.Fatal("at least one request must be made")
	}
}
