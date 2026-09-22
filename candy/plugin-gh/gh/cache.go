package gh

// cache.go — the gh client's HTTP response cache, on the ONE shared
// spec/cache.Store (R3). No gh-specific cache mechanism: the plugin reuses the
// same content-addressed / revalidating Store the loader and every other cache
// in the tree use.
//
// Two upstream classes, two validity modes:
//
//   - IMMUTABLE resources — a git blob by SHA (the file-content read). The blob
//     SHA IS the content identity, so the entry is keyed by Components{sha} and
//     is valid forever (no TTL, no revalidation). A second read of the same SHA
//     never touches the network.
//
//   - MUTABLE resources — PR/issue meta, files, comments, commits, reviews. The
//     entry carries the upstream ETag as its Validator; the store is revalidated
//     with If-None-Match, and a 304 refreshes the entry's Resolved without a
//     body re-fetch. So the data is re-fetched EXACTLY when it changed upstream,
//     and a repeat read inside the TTL window short-circuits without a request.
//
// A short in-process memo dedupes repeat reads within ONE process (the refs
// GitClient batch-dedupe pattern): a document assembly reads several endpoints
// that can share a URL, and a 16-lane run reads the same PR repeatedly.

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/opencharly/spec/cache"
)

// CacheTTL is how long a MUTABLE response is served without revalidating. Short
// (one minute) because a PR can move at any moment: within the window repeat
// reads are free; past it the ETag revalidation runs and pays only a 304 when
// upstream is unchanged.
const CacheTTL = time.Minute

// cacheEnvName overrides the gh response-cache root (tests isolate to a temp
// dir; operators can point it anywhere), mirroring CHARLY_REPO_CACHE.
const cacheEnvName = "CHARLY_GH_CACHE"

// responseCache is the per-client HTTP response cache: one shared Store plus an
// in-process memo (URL → cached body) so repeat reads in ONE process avoid even
// the ETag round-trip.
type responseCache struct {
	store *cache.Store

	mu   sync.Mutex
	memo map[string]memoEntry
	// hits counts responses served from cache (memo, TTL, or ETag 304) — the
	// provenance flag a caller reports.
	hits int
}

type memoEntry struct {
	body    []byte
	fetched time.Time
}

// newResponseCache opens the response cache: $CHARLY_GH_CACHE if set, else the
// shared cache's `gh-http` store under the charly dir
// (~/.config/charly/cache/gh-http). An inert store (no dir) disables caching
// without error — the cache is an optimization.
func newResponseCache() *responseCache {
	if dir := os.Getenv(cacheEnvName); dir != "" {
		return &responseCache{store: cache.Open(dir), memo: map[string]memoEntry{}}
	}
	return &responseCache{store: cache.OpenNamed("gh-http"), memo: map[string]memoEntry{}}
}

// cachedBody is one cached HTTP response: the raw body + the ETag that
// revalidates it.
type cachedBody struct {
	Body []byte `json:"body"`
	ETag string `json:"etag,omitempty"`
}

// getCached performs a conditional GET of path and returns the raw body,
// serving from cache when valid:
//
//   - in-process memo hit within CacheTTL → body, no request.
//   - persisted entry within CacheTTL → body, no request.
//   - persisted entry past CacheTTL → If-None-Match; 304 → refresh + body;
//     changed → store the new body+ETag.
//   - no entry → GET + store.
//
// immutable keys the entry by its content coordinate (sha) and skips the TTL and
// the revalidation entirely — an immutable resource never changes. The accept
// header selects the media type (the diff/media variants).
func (c *Client) getCached(ctx context.Context, path, accept string, immutable map[string]string) ([]byte, bool, error) {
	if c.cache == nil {
		b, err := c.request(ctx, "GET", path, nil, false, accept)
		return b, false, err
	}
	// The accept header participates in the key: the same path serves a JSON body
	// or a raw diff under different media types.
	key := accept + "\x00" + path + "\x00" + immutableDigest(immutable)
	// Immutable: a pure component read. No TTL, no ETag — the SHA is identity.
	if len(immutable) > 0 {
		if e, ok := c.cache.store.Get(key); ok && e.FreshComponents(immutable) {
			var cb cachedBody
			if e.Decode(&cb) {
				c.cache.recordHit()
				return cb.Body, true, nil
			}
		}
		b, err := c.request(ctx, "GET", path, nil, false, accept)
		if err != nil {
			return nil, false, err
		}
		raw, _ := json.Marshal(cachedBody{Body: b})
		c.cache.store.Put(key, cache.Entry{Value: raw, Components: immutable})
		return b, false, nil
	}

	// Mutable: memo, then TTL, then ETag revalidation.
	if b, ok := c.cache.memoGet(key); ok {
		c.cache.recordHit()
		return b, true, nil
	}
	e, present := c.cache.store.Get(key)
	var prior cachedBody
	if present {
		_ = e.Decode(&prior)
	}
	if present && e.FreshTTL(CacheTTL) && len(prior.Body) > 0 {
		c.cache.recordHit()
		return prior.Body, true, nil
	}

	// Revalidate (or fetch fresh). A 304 means unchanged: keep the body,
	// refresh the resolution time, and memo it.
	b, etag, notModified, err := c.requestConditional(ctx, path, accept, prior.ETag)
	if err != nil {
		return nil, false, err
	}
	if notModified {
		c.cache.store.Put(key, cache.Entry{
			Value:     e.Value,
			Validator: prior.ETag,
		})
		c.cache.memoPut(key, prior.Body)
		c.cache.recordHit()
		return prior.Body, true, nil
	}
	raw, _ := json.Marshal(cachedBody{Body: b, ETag: etag})
	c.cache.store.Put(key, cache.Entry{Value: raw, Validator: etag})
	c.cache.memoPut(key, b)
	return b, false, nil
}

// recordHit notes that a response was served from cache (the provenance flag).
func (r *responseCache) recordHit() {
	r.mu.Lock()
	r.hits++
	r.mu.Unlock()
}

// hitsSince reports how many cache hits have been recorded (the caller diffs it
// across an assembly to report Provenance.Cached).
func (r *responseCache) hitsSince() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits
}

// memoGet returns a memoized body within the TTL.
func (r *responseCache) memoGet(key string) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.memo[key]
	if !ok || time.Since(m.fetched) > CacheTTL {
		return nil, false
	}
	return m.body, true
}

// memoPut records a memoized body (best-effort).
func (r *responseCache) memoPut(key string, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.memo[key] = memoEntry{body: body, fetched: time.Now()}
}

// immutableDigest renders an immutable component set into the cache-key suffix
// (a stable, order-independent digest).
func immutableDigest(components map[string]string) string {
	if len(components) == 0 {
		return ""
	}
	return cache.KeyDigest(components)
}

// invalidate drops every cached response (the `charly cache clear` sweep and the
// test hook).
func (r *responseCache) invalidate() {
	r.mu.Lock()
	r.memo = map[string]memoEntry{}
	r.mu.Unlock()
	_ = r.store.Clear()
}
