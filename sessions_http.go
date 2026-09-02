package main

import (
	"context"
	"encoding/json"
	"forgejo.develop.10.199.64.20.nip.io/zergx/repo-extension/internal/naming"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// ---- session resolution cache (tool path) ----

type sessEntry struct {
	org, repo, bookmark string
	exp                 time.Time
}

type sessCache struct {
	mu  sync.Mutex
	m   map[string]sessEntry
	ttl time.Duration
}

func newSessCache(ttl time.Duration) *sessCache {
	return &sessCache{m: map[string]sessEntry{}, ttl: ttl}
}

func (c *sessCache) get(sid string) (string, string, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[sid]
	if !ok || time.Now().After(e.exp) {
		return "", "", "", false
	}
	return e.org, e.repo, e.bookmark, true
}

func (c *sessCache) put(sid, org, repo, bookmark string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[sid] = sessEntry{org: org, repo: repo, bookmark: bookmark, exp: time.Now().Add(c.ttl)}
}

func (c *sessCache) evict(sid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, sid)
}

// ---- session triple resolution (tool path, strict) ----

// resolveSession maps a session name to its (org, repo, bookmark) triple via
// the mapping table. No fallback, no lazy creation: under the eager
// lifecycle-event model every session has its workspace by the time tools
// run. A miss means the event has not been processed yet (retry shortly) or
// the name is not workspace-derived.
func (s *server) resolveSession(ctx context.Context, sid string) (string, string, string, error) {
	if o, r, b, ok := s.cache.get(sid); ok {
		return o, r, b, nil
	}
	row, err := s.store.GetRowBySession(ctx, sid)
	if err != nil {
		return "", "", "", errDownstream("postgres", err)
	}
	if row == nil {
		if _, _, _, ok := naming.Parse(sid); !ok {
			return "", "", "", errBad("session '%s' does not match org:repo:bookmark naming; cannot resolve workspace", sid)
		}
		return "", "", "", errNotFound("session '%s' workspace is not ready yet (lifecycle event in progress); retry later", sid)
	}
	s.cache.put(sid, row.Org, row.Repo, row.Bookmark)
	return row.Org, row.Repo, row.Bookmark, nil
}

// bindRow records a mapping; a unique conflict means a concurrent path
// already won — converged, not an error.
func (s *server) bindRow(ctx context.Context, org, repo, bookmark, sid string) error {
	if err := s.store.InsertRow(ctx, org, repo, bookmark, sid); err != nil {
		if statusOf(err) == 409 {
			return nil
		}
		return err
	}
	return s.store.InsertManaged(ctx, org, repo)
}

// completeAdopt binds an EXISTING bookmark to a derived session (manual ops
// surface: give an orphan bookmark a session). Returns (sessionName, adopted).
func (s *server) completeAdopt(ctx context.Context, org, repo, bookmark string) (string, bool, error) {
	row, err := s.store.GetRow(ctx, org, repo, bookmark)
	if err != nil {
		return "", false, errDownstream("postgres", err)
	}
	if row != nil {
		return row.SessionName, false, nil
	}
	tree, err := s.jj.GetRepoTree(ctx)
	if err != nil {
		return "", false, err
	}
	if !tree.repoExists(org, repo) {
		return "", false, errNotFound("repository %s/%s does not exist", org, repo)
	}
	if !tree.bookmarkExists(org, repo, bookmark) {
		return "", false, errNotFound("bookmark %s/%s#%s does not exist", org, repo, bookmark)
	}
	name := naming.Session(org, repo, bookmark)
	if err := s.ag.EnsureSession(ctx, name); err != nil {
		return "", false, err
	}
	if err := s.store.InsertRow(ctx, org, repo, bookmark, name); err != nil {
		return "", false, err
	}
	return name, true, nil
}

// ---- HTTP handlers (ops surface; workspace writes are event-driven) ----

// router builds the chi router (shared by main and tests).
func (s *server) router() http.Handler {
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/health", s.health)
		r.Get("/session-map", s.getSessionMap)
		r.Get("/repos", s.listRepos)
		r.Get("/repos/{org}/{repo}/bookmarks", s.listBookmarks)
		r.Post("/repos/{org}/{repo}/bookmarks/{bm}/session", s.ensureSession)
	})
	return r
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": "repo-extension"})
}

// listRepos: GET /repos — jj tree annotated with managed flag and session binding.
func (s *server) listRepos(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tree, err := s.jj.GetRepoTree(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	rows, err := s.store.ListRows(ctx)
	if err != nil {
		writeErr(w, errDownstream("postgres", err))
		return
	}
	managed, err := s.store.ListManaged(ctx)
	if err != nil {
		writeErr(w, errDownstream("postgres", err))
		return
	}
	mSet := map[string]bool{}
	for _, m := range managed {
		mSet[m.Org+"/"+m.Repo] = true
	}
	bound := map[string]string{}
	for _, row := range rows {
		bound[row.Org+"/"+row.Repo+"/"+row.Bookmark] = row.SessionName
	}

	orgs := []interface{}{}
	for org, repos := range tree {
		rl := []interface{}{}
		for repo, bms := range repos {
			bl := []interface{}{}
			for _, bm := range bms {
				var sn interface{}
				if v, ok := bound[org+"/"+repo+"/"+bm]; ok {
					sn = v
				}
				bl = append(bl, map[string]interface{}{"branch": bm, "session_name": sn})
			}
			rl = append(rl, map[string]interface{}{"repo": repo, "managed": mSet[org+"/"+repo], "bookmarks": bl})
		}
		orgs = append(orgs, map[string]interface{}{"org": org, "repos": rl})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"orgs": orgs})
}

// listBookmarks: GET /repos/{org}/{repo}/bookmarks
func (s *server) listBookmarks(w http.ResponseWriter, r *http.Request) {
	org, repo := chi.URLParam(r, "org"), chi.URLParam(r, "repo")
	ctx := r.Context()
	tree, err := s.jj.GetRepoTree(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !tree.repoExists(org, repo) {
		writeErr(w, errNotFound("repository %s/%s does not exist", org, repo))
		return
	}
	rows, err := s.store.ListRowsForRepo(ctx, org, repo)
	if err != nil {
		writeErr(w, errDownstream("postgres", err))
		return
	}
	bound := map[string]string{}
	for _, row := range rows {
		bound[row.Bookmark] = row.SessionName
	}
	out := []interface{}{}
	for _, bm := range tree[org][repo] {
		var sn interface{}
		if v, ok := bound[bm]; ok {
			sn = v
		}
		out = append(out, map[string]interface{}{"branch": bm, "session_name": sn})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"bookmarks": out})
}

// ensureSession: POST /repos/{org}/{repo}/bookmarks/{bm}/session — bind an
// orphan bookmark to a (created) session. Idempotent.
func (s *server) ensureSession(w http.ResponseWriter, r *http.Request) {
	org, repo, bm := chi.URLParam(r, "org"), chi.URLParam(r, "repo"), chi.URLParam(r, "bm")
	if !naming.ValidComponent(bm) {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"ok": false, "error": "invalid bookmark name; cannot derive session"})
		return
	}
	name, adopted, err := s.completeAdopt(r.Context(), org, repo, bm)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true, "session_name": name, "adopted": adopted,
	})
}

// getSessionMap: GET /session-map?session=NAME — reverse lookup.
func (s *server) getSessionMap(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("session")
	if sid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "session is required"})
		return
	}
	row, err := s.store.GetRowBySession(r.Context(), sid)
	if err != nil {
		writeErr(w, errDownstream("postgres", err))
		return
	}
	if row == nil {
		writeErr(w, errNotFound("session %s has no mapping row", sid))
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"org": row.Org, "repo": row.Repo, "bookmark": row.Bookmark,
	})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	writeJSON(w, statusOf(err), map[string]interface{}{"ok": false, "error": err.Error()})
}
