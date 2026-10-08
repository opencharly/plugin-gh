package gh

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestReadRetryPolicy pins the WHOLE policy of opencharly/plugin-gh#10, and the invariant that
// matters most is the third one: a mutating request is attempted EXACTLY ONCE. That is the R4 line
// — a retry on a write can duplicate a side effect — and it is pinned here rather than trusted to a
// comment, so no future "simplify the retry" can cross it silently.
//
// The bounds are shortened for the test (they are package-level vars for exactly this reason) so
// each case runs in milliseconds instead of a real 20 s attempt bound. The SHAPE under test is the
// production one: same code path, same decision points.
func TestReadRetryPolicy(t *testing.T) {
	origAttempt, origBudget, origBase, origWrite := readAttemptTimeout, readTotalBudget, readBackoffBase, writeAttemptTimeout
	defer func() {
		readAttemptTimeout, readTotalBudget, readBackoffBase, writeAttemptTimeout = origAttempt, origBudget, origBase, origWrite
	}()
	readAttemptTimeout = 60 * time.Millisecond
	readTotalBudget = 3 * time.Second
	readBackoffBase = time.Millisecond
	// The write bound is shortened ONLY so the "a stalled write is not retried" case can fail in
	// milliseconds instead of sixty seconds; production keeps its single 60 s bound untouched.
	writeAttemptTimeout = 60 * time.Millisecond

	// stalling builds a server that answers EVERY request with `status`, after sleeping
	// `stallFor` for the first `stall` of them (so `stall=1` + a sleep longer than the attempt bound
	// is "stalls once, then recovers"; `stall=0` is "the same answer every time"). It counts every
	// request it receives, which is how each invariant below is measured.
	//
	// The count is the MEASUREMENT, so the handler must always answer with the status it was asked
	// for: an early `return` before WriteHeader would answer 200 and make a "persistent failure"
	// case silently pass (this test's first draft did exactly that).
	stalling := func(stallFor time.Duration, status int, stall int32) (*httptest.Server, *int32) {
		var hits int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if n := atomic.AddInt32(&hits, 1); n <= stall {
				time.Sleep(stallFor)
			}
			if status == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "1")
			}
			w.WriteHeader(status)
			if status >= 200 && status < 300 {
				_, _ = w.Write([]byte(`{"ok":true}`))
			}
		}))
		t.Cleanup(srv.Close)
		return srv, &hits
	}
	newClient := func(url string) *Client { return &Client{BaseURL: url, HTTP: &http.Client{}} }

	t.Run("a transient stall is ABSORBED: the read recovers and the stall is visible", func(t *testing.T) {
		srv, hits := stalling(readAttemptTimeout*3, http.StatusOK, 1)
		c := newClient(srv.URL)
		var out map[string]any
		if err := c.Get(context.Background(), "/x", &out); err != nil {
			t.Fatalf("a transient stall must not fail an idempotent read: %v", err)
		}
		if got := atomic.LoadInt32(hits); got < 2 {
			t.Fatalf("the server saw %d request(s); the stalled attempt must have been retried", got)
		}
	})

	t.Run("a stalled WRITE is attempted EXACTLY ONCE — the read policy never reaches it", func(t *testing.T) {
		// The write stalls PAST its own bound, so it FAILS exactly as a transient read failure
		// would — and it must still be issued only once. A retry here can duplicate a side effect:
		// that is the R4 line this whole PR exists to keep on the right side of.
		srv, hits := stalling(readAttemptTimeout*3, http.StatusOK, 1)
		c := newClient(srv.URL)
		var out map[string]any
		err := c.Post(context.Background(), "/x", map[string]string{"a": "b"}, &out)
		if err == nil {
			t.Fatal("the stalled write reported success")
		}
		if got := atomic.LoadInt32(hits); got != 1 {
			t.Fatalf("the server saw %d request(s) for ONE Post; a retry on a mutating path can "+
				"duplicate a side effect (R4) — this is the line the read retry must never cross", got)
		}
	})

	t.Run("a persistent transient failure stops after exactly readMaxAttempts and says so", func(t *testing.T) {
		srv, hits := stalling(0, http.StatusServiceUnavailable, 0)
		c := newClient(srv.URL)
		// The internal GET path is used here so the retry decision itself is what fails, rather
		// than a decode of the empty body a 503 carries through the cached-read wrapper.
		_, err := c.request(context.Background(), http.MethodGet, "/x", nil, false, "application/vnd.github+json")
		if err == nil {
			t.Fatal("a persistent 503 reported success")
		}
		if !strings.Contains(err.Error(), "4 attempt(s)") {
			t.Errorf("the failure must name the attempt count so a stall is distinguishable from a "+
				"verdict, got: %v", err)
		}
		if got := atomic.LoadInt32(hits); got != int32(readMaxAttempts) {
			t.Errorf("the server saw %d request(s), want exactly %d", got, readMaxAttempts)
		}
	})

	t.Run("a 404 is a verdict, not a stall: attempted exactly once", func(t *testing.T) {
		srv, hits := stalling(0, http.StatusNotFound, 0)
		c := newClient(srv.URL)
		_, err := c.request(context.Background(), http.MethodGet, "/x", nil, false, "application/vnd.github+json")
		if err == nil {
			t.Fatal("a 404 reported success")
		}
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != http.StatusNotFound {
			t.Fatalf("a 404 must surface as an HTTPError carrying its status, got: %v", err)
		}
		if got := atomic.LoadInt32(hits); got != 1 {
			t.Errorf("the server saw %d request(s) for a 404; a verdict is an answer, not a stall", got)
		}
	})

	t.Run("500 is a verdict on this policy, not a stall: attempted exactly once", func(t *testing.T) {
		// The approved retry list is 429 + 502/503/504. A 500 is deliberately NOT on it, and this
		// case pins that boundary so the list cannot quietly grow: widening it is a policy change,
		// not a refactor.
		srv, hits := stalling(0, http.StatusInternalServerError, 0)
		c := newClient(srv.URL)
		var out map[string]any
		if err := c.Get(context.Background(), "/x", &out); err == nil {
			t.Fatal("a 500 reported success")
		}
		if got := atomic.LoadInt32(hits); got != 1 {
			t.Errorf("the server saw %d request(s) for a 500; it is not on the approved retry list", got)
		}
	})

	t.Run("429 honours Retry-After and then succeeds", func(t *testing.T) {
		var hits int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := atomic.AddInt32(&hits, 1)
			if n == 1 {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer srv.Close()
		c := newClient(srv.URL)
		var out map[string]any
		started := time.Now()
		if err := c.Get(context.Background(), "/x", &out); err != nil {
			t.Fatalf("a 429-then-200 read must succeed: %v", err)
		}
		if elapsed := time.Since(started); elapsed < time.Second {
			t.Errorf("the retry waited %s; it must honour the server's Retry-After (1s): a reader that "+
				"hammers through a rate-limit is how a transient 429 becomes a longer ban", elapsed)
		}
		if got := atomic.LoadInt32(&hits); got != 2 {
			t.Errorf("the server saw %d request(s), want 2 (the 429, then the success)", got)
		}
	})

	t.Run("cancelling the caller's context stops the backoff", func(t *testing.T) {
		srv, hits := stalling(0, http.StatusInternalServerError, 99)
		c := newClient(srv.URL)
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		var out map[string]any
		if err := c.Get(ctx, "/x", &out); err == nil {
			t.Fatal("a cancelled read reported success")
		}
		if got := atomic.LoadInt32(hits); got >= int32(readMaxAttempts) {
			t.Errorf("the server saw %d request(s) after the caller cancelled; the backoff must be "+
				"cancellable, never a blind sleep", got)
		}
	})
}
