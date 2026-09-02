package main

import (
	"context"
	"encoding/json"
	abcprotocol "forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go"
	"forgejo.develop.10.199.64.20.nip.io/zergx/repo-extension/internal/env"
	"forgejo.develop.10.199.64.20.nip.io/zergx/repo-extension/internal/naming"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- in-memory fakes ----

// fakeJJ emulates the jjlab REST surface repo-extension depends on.
type fakeJJ struct {
	mu     sync.Mutex
	repos  map[string]map[string][]string // org -> repo -> bookmarks
	anchor map[string]string              // "org/repo/new_bm" -> source it was created from
	server *httptest.Server
}

func newFakeJJ() *fakeJJ {
	f := &fakeJJ{
		repos:  map[string]map[string][]string{},
		anchor: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeTestJSON(w, 405, nil)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		orgs := []interface{}{}
		for org, repos := range f.repos {
			rl := []interface{}{}
			for repo := range repos {
				rl = append(rl, map[string]interface{}{"repo": repo, "default_bookmark": "main"})
			}
			orgs = append(orgs, map[string]interface{}{"org": org, "repos": rl})
		}
		writeTestJSON(w, 200, map[string]interface{}{"orgs": orgs})
	})
	// POST /api/v1/repos/{org}/{repo} — create repo (or 409 if it exists).
	mux.HandleFunc("/api/v1/repos/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/"), "/")
		switch {
		case r.Method == http.MethodPost && len(parts) == 2:
			org, repo := parts[0], parts[1]
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.repos[org] == nil {
				f.repos[org] = map[string][]string{}
			}
			if _, ok := f.repos[org][repo]; !ok {
				f.repos[org][repo] = []string{"main"}
			}
			writeTestJSON(w, 201, map[string]interface{}{"full_name": org + "/" + repo})
		// POST /branches/{bm} {target: src}; DELETE /branches/{bm}.
		case r.Method == http.MethodPost && len(parts) == 4 && parts[2] == "branches":
			var b map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&b)
			org, repo, nb := parts[0], parts[1], parts[3]
			src, _ := b["target"].(string)
			f.mu.Lock()
			defer f.mu.Unlock()
			bms, ok := f.repos[org][repo]
			if !ok {
				writeTestJSON(w, 404, map[string]interface{}{"ok": false, "error": "no repo"})
				return
			}
			found := src == "" // "" resolves to head
			if !found {
				for _, b := range bms {
					if b == src {
						found = true
					}
				}
			}
			if !found {
				writeTestJSON(w, 422, map[string]interface{}{"ok": false, "error": "cannot resolve " + src})
				return
			}
			f.anchor[org+"/"+repo+"/"+nb] = src
			f.repos[org][repo] = append(bms, nb)
			writeTestJSON(w, 200, map[string]interface{}{"ok": true, "sha": "hash"})
		case r.Method == http.MethodDelete && len(parts) == 4 && parts[2] == "branches":
			org, repo, bm := parts[0], parts[1], parts[3]
			f.mu.Lock()
			defer f.mu.Unlock()
			var out []string
			for _, b := range f.repos[org][repo] {
				if b != bm {
					out = append(out, b)
				}
			}
			f.repos[org][repo] = out
			writeTestJSON(w, 204, map[string]interface{}{"ok": true})
		// GET /branches and GET /tree/{rev}.
		case r.Method == http.MethodGet && len(parts) == 3 && parts[2] == "branches":
			org, repo := parts[0], parts[1]
			f.mu.Lock()
			defer f.mu.Unlock()
			bl := []interface{}{}
			for _, b := range f.repos[org][repo] {
				bl = append(bl, map[string]interface{}{"name": b, "sha": "h"})
			}
			writeTestJSON(w, 200, map[string]interface{}{"branches": bl})
		case r.Method == http.MethodGet && len(parts) == 4 && parts[2] == "tree":
			org, repo, rev := parts[0], parts[1], parts[3]
			f.mu.Lock()
			defer f.mu.Unlock()
			if _, ok := f.repos[org][repo]; !ok {
				writeTestJSON(w, 404, map[string]interface{}{"ok": false})
				return
			}
			if rev == "" {
				writeTestJSON(w, 200, map[string]interface{}{"tree": []interface{}{}})
				return
			}
			for _, b := range f.repos[org][repo] {
				if b == rev {
					writeTestJSON(w, 200, map[string]interface{}{"tree": []interface{}{}})
					return
				}
			}
			writeTestJSON(w, 404, map[string]interface{}{"ok": false})
		default:
			writeTestJSON(w, 404, nil)
		}
	})
	f.server = httptest.NewServer(mux)
	return f
}

func (f *fakeJJ) Close()      { f.server.Close() }
func (f *fakeJJ) URL() string { return f.server.URL }
func (f *fakeJJ) hasRepo(org, repo string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.repos[org][repo]
	return ok
}
func (f *fakeJJ) hasBM(org, repo, bm string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.repos[org][repo] {
		if b == bm {
			return true
		}
	}
	return false
}
func (f *fakeJJ) bmAnchor(org, repo, bm string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.anchor[org+"/"+repo+"/"+bm]
}

// fakeAgent emulates the agent-ts session REST surface.
type fakeAgent struct {
	mu       sync.Mutex
	sessions map[string]bool
	server   *httptest.Server
}

func newFakeAgent() *fakeAgent {
	a := &fakeAgent{sessions: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			a.mu.Lock()
			defer a.mu.Unlock()
			list := []interface{}{}
			for name := range a.sessions {
				list = append(list, map[string]interface{}{"name": name})
			}
			writeTestJSON(w, 200, map[string]interface{}{"sessions": list})
			return
		}
		var b map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&b)
		name, _ := b["name"].(string)
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.sessions[name] {
			writeTestJSON(w, 409, map[string]interface{}{"ok": false, "error": "Session already exists"})
			return
		}
		a.sessions[name] = true
		writeTestJSON(w, 200, map[string]interface{}{"ok": true, "session_name": name})
	})
	a.server = httptest.NewServer(mux)
	return a
}

func (a *fakeAgent) Close()      { a.server.Close() }
func (a *fakeAgent) URL() string { return a.server.URL }
func (a *fakeAgent) has(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[name]
}

func writeTestJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ---- test harness ----

// testStore connects to a PG instance gated by REPOEXT_TEST_PG=1 and resets
// the schema for isolation.
func testStore(t *testing.T) *Store {
	t.Helper()
	if os.Getenv("REPOEXT_TEST_PG") != "1" {
		t.Skip("set REPOEXT_TEST_PG=1 (and optional REPOEXT_TEST_PG_HOST/DB) to run PG-backed tests")
	}
	cfg := PgConfig{
		Host:     env.Or("REPOEXT_TEST_PG_HOST", "postgres.zergx.svc.cluster.local"),
		Port:     env.NormalizePort(env.Or("REPOEXT_TEST_PG_PORT", "5432")),
		User:     env.Or("REPOEXT_TEST_PG_USER", "root"),
		Password: env.Or("REPOEXT_TEST_PG_PASSWORD", "devpassword"),
		DB:       env.Or("REPOEXT_TEST_PG_DB", "zergx_repoext_test"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := OpenStore(ctx, cfg)
	if err != nil {
		t.Skipf("pg unavailable: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `
		TRUNCATE session_repos; TRUNCATE managed_repos;`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

func newTestServer(t *testing.T, jj *fakeJJ, ag *fakeAgent, store *Store) *server {
	t.Helper()
	return &server{
		base:  jj.URL(),
		agent: ag.URL(),
		store: store,
		cache: newSessCache(50 * time.Millisecond),
		jj:    newJJClient(jj.URL(), "test-token"),
		ag:    newAgentClient(ag.URL()),
	}
}

func doReq(t *testing.T, h http.Handler, method, path string, body interface{}) (int, map[string]interface{}) {
	t.Helper()
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var v map[string]interface{}
	_ = json.NewDecoder(rec.Body).Decode(&v)
	return rec.Code, v
}

func (s *server) emit(t *testing.T, ctx context.Context, event string, env abcprotocol.LifecycleEvent) error {
	t.Helper()
	return s.handleLifecycleEvent(ctx, event, env)
}

// ---- lifecycle event tests ----

func TestLifecycleCreated(t *testing.T) {
	jj, ag := newFakeJJ(), newFakeAgent()
	defer jj.Close()
	defer ag.Close()
	s := newTestServer(t, jj, ag, testStore(t))
	ctx := context.Background()

	if err := s.emit(t, ctx, "created", abcprotocol.LifecycleEvent{Kind: "created", SessionName: "acme:api:main"}); err != nil {
		t.Fatal(err)
	}
	if !jj.hasRepo("acme", "api") || !jj.hasBM("acme", "api", "main") {
		t.Fatal("created did not materialize repo/bookmark")
	}
	if row, _ := s.store.GetRow(ctx, "acme", "api", "main"); row == nil {
		t.Fatal("mapping row missing")
	}
	// tool path resolves immediately (eager)
	o, r, b, err := s.resolveSession(ctx, "acme:api:main")
	if err != nil || o != "acme" || r != "api" || b != "main" {
		t.Fatalf("resolveSession = %q %q %q %v", o, r, b, err)
	}
	// idempotent: redelivery is a no-op
	if err := s.emit(t, ctx, "created", abcprotocol.LifecycleEvent{Kind: "created", SessionName: "acme:api:main"}); err != nil {
		t.Fatal(err)
	}
	// non-derived names are dropped (permanent)
	if err := s.emit(t, ctx, "created", abcprotocol.LifecycleEvent{Kind: "created", SessionName: "hi"}); !isPermanent(err) {
		t.Fatalf("non-derived create should be permanent, got %v", err)
	}
}

func TestLifecycleForkedInheritsParentBookmark(t *testing.T) {
	jj, ag := newFakeJJ(), newFakeAgent()
	defer jj.Close()
	defer ag.Close()
	s := newTestServer(t, jj, ag, testStore(t))
	ctx := context.Background()

	mustEmit := func(ev abcprotocol.LifecycleEvent) {
		event := string(ev.Kind)
		if err := s.emit(t, ctx, event, ev); err != nil {
			t.Fatalf("%s: %v", event, err)
		}
	}
	mustEmit(abcprotocol.LifecycleEvent{Kind: "created", SessionName: "acme:api:main"})
	mustEmit(abcprotocol.LifecycleEvent{Kind: "created", SessionName: "acme:api:dev"})
	mustEmit(abcprotocol.LifecycleEvent{Kind: "forked", SessionName: "acme:api:feat", Parent: strptr("acme:api:dev")})

	if !jj.hasBM("acme", "api", "feat") {
		t.Fatal("fork bookmark missing")
	}
	if got := jj.bmAnchor("acme", "api", "feat"); got != "dev" {
		t.Fatalf("fork anchored at %q, want parent bookmark %q", got, "dev")
	}
	if row, _ := s.store.GetRow(ctx, "acme", "api", "feat"); row == nil || row.SessionName != "acme:api:feat" {
		t.Fatalf("fork row missing: %+v", row)
	}
	// idempotent redelivery
	mustEmit(abcprotocol.LifecycleEvent{Kind: "forked", SessionName: "acme:api:feat", Parent: strptr("acme:api:dev")})
	// fork from unmapped parent materializes the parent first
	mustEmit(abcprotocol.LifecycleEvent{Kind: "forked", SessionName: "acme:api:f2", Parent: strptr("acme:api:legacy")})
	if !jj.hasBM("acme", "api", "legacy") {
		t.Fatal("unmapped parent should be materialized first")
	}
	// cross-repo fork is rejected (permanent)
	if err := s.emit(t, ctx, "forked", abcprotocol.LifecycleEvent{Kind: "forked", SessionName: "other:api:x", Parent: strptr("acme:api:dev")}); !isPermanent(err) {
		t.Fatalf("cross-repo fork should be permanent, got %v", err)
	}
}

func TestLifecycleRenamedDual(t *testing.T) {
	jj, ag := newFakeJJ(), newFakeAgent()
	defer jj.Close()
	defer ag.Close()
	s := newTestServer(t, jj, ag, testStore(t))
	ctx := context.Background()

	s.emit(t, ctx, "created", abcprotocol.LifecycleEvent{Kind: "created", SessionName: "acme:api:old"})
	if err := s.emit(t, ctx, "renamed", abcprotocol.LifecycleEvent{Kind: "renamed", From: strptr("acme:api:old"), To: strptr("acme:api:new")}); err != nil {
		t.Fatal(err)
	}
	if jj.hasBM("acme", "api", "old") || !jj.hasBM("acme", "api", "new") {
		t.Fatal("bookmark rename not dual")
	}
	if got := jj.bmAnchor("acme", "api", "new"); got != "old" {
		t.Fatalf("renamed bookmark should anchor at old, got %q", got)
	}
	if row, _ := s.store.GetRow(ctx, "acme", "api", "new"); row == nil || row.SessionName != "acme:api:new" {
		t.Fatalf("row not renamed: %+v", row)
	}
	if row, _ := s.store.GetRow(ctx, "acme", "api", "old"); row != nil {
		t.Fatal("old row must be gone")
	}
	// rename of never-materialized session = plain create
	if err := s.emit(t, ctx, "renamed", abcprotocol.LifecycleEvent{Kind: "renamed", From: strptr("acme:api:ghost"), To: strptr("acme:api:revived")}); err != nil {
		t.Fatal(err)
	}
	if !jj.hasBM("acme", "api", "revived") {
		t.Fatal("rename-as-create failed")
	}
}

func TestLifecycleDeleted(t *testing.T) {
	jj, ag := newFakeJJ(), newFakeAgent()
	defer jj.Close()
	defer ag.Close()
	s := newTestServer(t, jj, ag, testStore(t))
	ctx := context.Background()

	s.emit(t, ctx, "created", abcprotocol.LifecycleEvent{Kind: "created", SessionName: "acme:api:tmp"})
	if err := s.emit(t, ctx, "deleted", abcprotocol.LifecycleEvent{Kind: "deleted", SessionName: "acme:api:tmp"}); err != nil {
		t.Fatal(err)
	}
	if jj.hasBM("acme", "api", "tmp") {
		t.Fatal("bookmark survived delete")
	}
	if row, _ := s.store.GetRow(ctx, "acme", "api", "tmp"); row != nil {
		t.Fatal("row survived delete")
	}
	// deleting a never-materialized session is a no-op success
	if err := s.emit(t, ctx, "deleted", abcprotocol.LifecycleEvent{Kind: "deleted", SessionName: "acme:api:never"}); err != nil {
		t.Fatal(err)
	}
}

func TestResolveSessionStrict(t *testing.T) {
	jj, ag := newFakeJJ(), newFakeAgent()
	defer jj.Close()
	defer ag.Close()
	s := newTestServer(t, jj, ag, testStore(t))
	ctx := context.Background()

	s.emit(t, ctx, "created", abcprotocol.LifecycleEvent{Kind: "created", SessionName: "acme:api:main"})

	// unmapped but well-formed → "not ready" (permanent-ish info), no creation
	if _, _, _, err := s.resolveSession(ctx, "acme:api:pending"); err == nil {
		t.Fatal("unmapped session should fail (no lazy creation)")
	}
	if !jj.hasBM("acme", "api", "pending") {
		// sanity: nothing was created
		t.Log("ok: no lazy creation")
	} else {
		t.Fatal("resolveSession must not create bookmarks")
	}
	// non-derived name → hard error
	if _, _, _, err := s.resolveSession(ctx, "hi"); err == nil {
		t.Fatal("non-derived name should fail")
	}
	// legacy args still work
	o, r, b, err := s.sessionBase(ctx, map[string]interface{}{"_org": "x", "_repo": "y", "_branch": "z"}, "")
	if err != nil || o != "x" || r != "y" || b != "z" {
		t.Fatalf("legacy base = %q %q %q %v", o, r, b, err)
	}
	if _, _, _, err = s.sessionBase(ctx, map[string]interface{}{}, ""); err == nil {
		t.Fatal("expected error without session context")
	}
}

// ---- ops-surface endpoint tests ----

func TestLazyAdoptEndpoint(t *testing.T) {
	jj, ag := newFakeJJ(), newFakeAgent()
	defer jj.Close()
	defer ag.Close()
	s := newTestServer(t, jj, ag, testStore(t))
	h := s.router()

	// orphan bookmark created directly in jj
	jj.mu.Lock()
	jj.repos["acme"] = map[string][]string{"api": {"main", "orphan-bm"}}
	jj.mu.Unlock()

	code, v := doReq(t, h, "POST", "/api/v1/repos/acme/api/bookmarks/orphan-bm/session", nil)
	if code != 200 || v["adopted"] != true {
		t.Fatalf("adopt = %d %v", code, v)
	}
	if !ag.has("acme:api:orphan-bm") {
		t.Fatal("adopt did not create session")
	}
	code, v = doReq(t, h, "POST", "/api/v1/repos/acme/api/bookmarks/orphan-bm/session", nil)
	if code != 200 || v["adopted"] != false {
		t.Fatalf("re-adopt = %d %v", code, v)
	}
	if code, _ = doReq(t, h, "POST", "/api/v1/repos/acme/api/bookmarks/ghost/session", nil); code != 404 {
		t.Fatalf("ghost adopt = %d, want 404", code)
	}
}

func TestGetSessionMap(t *testing.T) {
	jj, ag := newFakeJJ(), newFakeAgent()
	defer jj.Close()
	defer ag.Close()
	s := newTestServer(t, jj, ag, testStore(t))
	h := s.router()
	ctx := context.Background()

	s.emit(t, ctx, "created", abcprotocol.LifecycleEvent{Kind: "created", SessionName: "acme:api:main"})
	code, v := doReq(t, h, "GET", "/api/v1/session-map?session=acme:api:main", nil)
	if code != 200 || v["bookmark"] != "main" {
		t.Fatalf("session-map = %d %v", code, v)
	}
	if code, _ = doReq(t, h, "GET", "/api/v1/session-map?session=missing", nil); code != 404 {
		t.Fatalf("missing map = %d", code)
	}
}

func TestListReposAnnotates(t *testing.T) {
	jj, ag := newFakeJJ(), newFakeAgent()
	defer jj.Close()
	defer ag.Close()
	s := newTestServer(t, jj, ag, testStore(t))
	h := s.router()

	s.emit(t, context.Background(), "created", abcprotocol.LifecycleEvent{Kind: "created", SessionName: "acme:api:main"})
	code, v := doReq(t, h, "GET", "/api/v1/repos", nil)
	if code != 200 {
		t.Fatalf("list = %d", code)
	}
	b, _ := json.Marshal(v["orgs"])
	if !strings.Contains(string(b), `"managed":true`) || !strings.Contains(string(b), "acme:api:main") {
		t.Fatalf("annotation missing: %s", b)
	}
}

// ---- reconcile tests ----

func TestReconcileConvergesDrift(t *testing.T) {
	jj, ag := newFakeJJ(), newFakeAgent()
	defer jj.Close()
	defer ag.Close()
	s := newTestServer(t, jj, ag, testStore(t))
	ctx := context.Background()

	s.emit(t, ctx, "created", abcprotocol.LifecycleEvent{Kind: "created", SessionName: "acme:api:main"})

	// drift 1: bookmark deleted directly in jj → row removed
	jj.mu.Lock()
	jj.repos["acme"]["api"] = []string{}
	jj.mu.Unlock()
	if err := reconcileOnce(ctx, s); err != nil {
		t.Fatal(err)
	}
	if row, _ := s.store.GetRow(ctx, "acme", "api", "main"); row != nil {
		t.Fatal("row survived bookmark deletion")
	}

	// drift 2: session deleted directly in agent → row removed, bookmark orphaned
	s.emit(t, ctx, "created", abcprotocol.LifecycleEvent{Kind: "created", SessionName: "acme:api:bm2"})
	ag.mu.Lock()
	delete(ag.sessions, "acme:api:bm2")
	ag.mu.Unlock()
	if err := reconcileOnce(ctx, s); err != nil {
		t.Fatal(err)
	}
	if row, _ := s.store.GetRow(ctx, "acme", "api", "bm2"); row != nil {
		t.Fatal("row survived session deletion")
	}
	if !jj.hasBM("acme", "api", "bm2") {
		t.Fatal("bookmark must survive as orphan (work preserved)")
	}
}

func TestReconcileBackfillsLostEvents(t *testing.T) {
	jj, ag := newFakeJJ(), newFakeAgent()
	defer jj.Close()
	defer ag.Close()
	s := newTestServer(t, jj, ag, testStore(t))
	ctx := context.Background()

	// sessions created in the agent while every lifecycle event was lost
	// (publish failure / downtime beyond retention) — no rows, no bookmarks
	ag.mu.Lock()
	ag.sessions["acme:api:main"] = true
	ag.sessions["acme:api:feat"] = true
	ag.sessions["hi"] = true // non-derived: outside the 1:1 contract
	ag.mu.Unlock()

	if err := reconcileOnce(ctx, s); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"acme:api:main", "acme:api:feat"} {
		org, repo, bm, _ := naming.Parse(name)
		if row, _ := s.store.GetRow(ctx, org, repo, bm); row == nil || row.SessionName != name {
			t.Fatalf("backfill missing row for %s", name)
		}
		if !jj.hasBM(org, repo, bm) {
			t.Fatalf("backfill missing bookmark for %s", name)
		}
	}
	// non-derived session must NOT get a workspace
	if row, _ := s.store.GetRowBySession(ctx, "hi"); row != nil {
		t.Fatal("non-derived session must not be backfilled")
	}
	// idempotent: a second cycle changes nothing
	if err := reconcileOnce(ctx, s); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileBackfillAndUnmapDoNotFight(t *testing.T) {
	jj, ag := newFakeJJ(), newFakeAgent()
	defer jj.Close()
	defer ag.Close()
	s := newTestServer(t, jj, ag, testStore(t))
	ctx := context.Background()

	// row exists, session gone (drift) — unmap must win over backfill paths
	s.emit(t, ctx, "created", abcprotocol.LifecycleEvent{Kind: "created", SessionName: "acme:api:x"})
	ag.mu.Lock()
	delete(ag.sessions, "acme:api:x")
	ag.mu.Unlock()
	if err := reconcileOnce(ctx, s); err != nil {
		t.Fatal(err)
	}
	if row, _ := s.store.GetRow(ctx, "acme", "api", "x"); row != nil {
		t.Fatal("row should be unmapped (session gone)")
	}
	if !jj.hasBM("acme", "api", "x") {
		t.Fatal("bookmark should remain as orphan")
	}
}

func strptr(s string) *string { return &s }
