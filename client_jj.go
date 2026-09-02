package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"forgejo.develop.10.199.64.20.nip.io/zergx/repo-extension/internal/httpx"
)

// jjClient is a thin, status-aware client for the jjlab REST API.
//
// Read endpoints are anonymous; every mutation requires a write token, sent
// as `Authorization: token <token>` (Gitea-style). The token is injected via
// `newJJClient` so the shared httpx helpers stay purpose-neutral.
//
// Existence checks go through the read-only directory scan (`GET /repos`) and
// the per-repo bookmark listing (`GET /branches`); jjlab no longer auto-inits
// missing repos on read paths.
type jjClient struct {
	base  string
	token string
	hc    *http.Client
}

func newJJClient(base, token string) *jjClient {
	return &jjClient{base: base, token: token, hc: &http.Client{Timeout: 30 * time.Second}}
}

func (c *jjClient) call(ctx context.Context, method, path string, body interface{}) (int, map[string]interface{}, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "token "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var v map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, v, nil
}

// repoTree is the read-only snapshot of org → repo → bookmarks (branch names).
type repoTree map[string]map[string][]string

// GetRepoTree lists every org/repo/bookmark. jjlab's `GET /repos` directory
// lists orgs+repos but NOT bookmarks, so this fans out one `GET /branches` per
// repo to reconstruct the branch set (repo counts are small).
func (c *jjClient) GetRepoTree(ctx context.Context) (repoTree, error) {
	status, v, err := c.call(ctx, http.MethodGet, "/api/v1/repos", nil)
	if err != nil {
		return nil, errDownstream("jjlab", err)
	}
	if status != 200 {
		return nil, errDownstream("jjlab", fmt.Errorf("GET /repos: HTTP %d", status))
	}
	tree := repoTree{}
	orgs, _ := v["orgs"].([]interface{})
	for _, oe := range orgs {
		om, ok := oe.(map[string]interface{})
		if !ok {
			continue
		}
		org, _ := om["org"].(string)
		tree[org] = map[string][]string{}
		repos, _ := om["repos"].([]interface{})
		for _, re := range repos {
			rm, ok := re.(map[string]interface{})
			if !ok {
				continue
			}
			repo, _ := rm["repo"].(string)
			bms, err := c.GetBranches(ctx, org, repo)
			if err != nil {
				// A repo that fails its branch listing still surfaces as empty
				// rather than failing the whole directory walk.
				tree[org][repo] = []string{}
				continue
			}
			tree[org][repo] = bms
		}
	}
	return tree, nil
}

// GetBranches lists a repo's branch names.
func (c *jjClient) GetBranches(ctx context.Context, org, repo string) ([]string, error) {
	status, v, err := c.call(ctx, http.MethodGet,
		"/api/v1/repos/"+url.PathEscape(org)+"/"+url.PathEscape(repo)+"/branches", nil)
	if err != nil {
		return nil, errDownstream("jjlab", err)
	}
	if status != 200 {
		return nil, errDownstream("jjlab", fmt.Errorf("GET branches: HTTP %d", status))
	}
	var out []string
	for _, be := range sliceOf(v["branches"]) {
		if bm, ok := be.(map[string]interface{}); ok {
			if n, _ := bm["name"].(string); n != "" {
				out = append(out, n)
			}
		}
	}
	return out, nil
}

func (t repoTree) repoExists(org, repo string) bool {
	repos, ok := t[org]
	if !ok {
		return false
	}
	_, ok = repos[repo]
	return ok
}

func (t repoTree) bookmarkExists(org, repo, bookmark string) bool {
	repos, ok := t[org]
	if !ok {
		return false
	}
	for _, b := range repos[repo] {
		if b == bookmark {
			return true
		}
	}
	return false
}

// EnsureRepo creates org/repo (idempotent: a 409 "already exists" is success).
// The org itself has no first-class REST endpoint — it is created implicitly
// with its first repository.
func (c *jjClient) EnsureRepo(ctx context.Context, org, repo string) error {
	status, v, err := c.call(ctx, http.MethodPost,
		"/api/v1/repos/"+url.PathEscape(org)+"/"+url.PathEscape(repo),
		map[string]interface{}{"default_branch": "main"})
	if err != nil {
		return errDownstream("jjlab", err)
	}
	switch status {
	case 200, 201, 409:
		return nil
	default:
		return errDownstream("jjlab", fmt.Errorf("ensure repo: HTTP %d %s", status, errText(v)))
	}
}

// EnsureBookmark creates a bookmark at `src` (a snapshot: bookmark / sha /
// empty for head), unless it already exists (idempotent forward step).
func (c *jjClient) EnsureBookmark(ctx context.Context, org, repo, src, bookmark string) error {
	tree, err := c.GetRepoTree(ctx)
	if err != nil {
		return err
	}
	if tree.bookmarkExists(org, repo, bookmark) {
		return nil
	}
	if !tree.repoExists(org, repo) {
		return errNotFound("repository %s/%s does not exist", org, repo)
	}
	status, v, err := c.call(ctx, http.MethodPost,
		"/api/v1/repos/"+url.PathEscape(org)+"/"+url.PathEscape(repo)+"/branches/"+url.PathEscape(bookmark),
		map[string]interface{}{"target": src})
	if err != nil {
		return errDownstream("jjlab", err)
	}
	if status != 200 && status != 201 {
		return errDownstream("jjlab", fmt.Errorf("create bookmark %q: HTTP %d %s", src, status, errText(v)))
	}
	return nil
}

// DeleteBookmark removes the bookmark if it exists (idempotent).
func (c *jjClient) DeleteBookmark(ctx context.Context, org, repo, bookmark string) error {
	tree, err := c.GetRepoTree(ctx)
	if err != nil {
		return err
	}
	if !tree.bookmarkExists(org, repo, bookmark) {
		return nil
	}
	status, v, err := c.call(ctx, http.MethodDelete,
		"/api/v1/repos/"+url.PathEscape(org)+"/"+url.PathEscape(repo)+"/branches/"+url.PathEscape(bookmark), nil)
	if err != nil {
		return errDownstream("jjlab", err)
	}
	if status != 200 && status != 204 {
		return errDownstream("jjlab", fmt.Errorf("delete bookmark: HTTP %d %s", status, errText(v)))
	}
	return nil
}

// CanResolve reports whether the rev (bookmark / sha / tag / change-id)
// resolves in the repo. Read-only via the tree-at-rev endpoint.
func (c *jjClient) CanResolve(ctx context.Context, org, repo, rev string) (bool, error) {
	status, _, err := c.call(ctx, http.MethodGet,
		"/api/v1/repos/"+url.PathEscape(org)+"/"+url.PathEscape(repo)+"/tree/"+url.PathEscape(rev), nil)
	if err != nil {
		return false, errDownstream("jjlab", err)
	}
	switch status {
	case 200:
		return true, nil
	case 404:
		return false, nil
	default:
		return false, errDownstream("jjlab", fmt.Errorf("resolve %q: HTTP %d", rev, status))
	}
}

func sliceOf(v interface{}) []interface{} {
	if arr, ok := v.([]interface{}); ok {
		return arr
	}
	return nil
}

// get/post/put/delete are the token-authenticated JSON round-trips used by the
// tool handlers. They inherit the shared httpx contract (404 → ErrNotFound),
// while injecting the write token on every request.
func (c *jjClient) get(ctx context.Context, path string) (map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	return c.doJSON(req)
}

func (c *jjClient) post(ctx context.Context, path string, body interface{}) (map[string]interface{}, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.doJSON(req)
}

func (c *jjClient) put(ctx context.Context, path string, body interface{}) (map[string]interface{}, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.doJSON(req)
}

func (c *jjClient) delete(ctx context.Context, path string, body interface{}) (map[string]interface{}, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.doJSON(req)
}

func (c *jjClient) doJSON(req *http.Request) (map[string]interface{}, error) {
	if c.token != "" {
		req.Header.Set("Authorization", "token "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, httpx.ErrNotFound)
	}
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	var v map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s %s: decode response: %w", req.Method, req.URL.Path, err)
	}
	return v, nil
}

func errText(v map[string]interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v["error"].(string); ok {
		return s
	}
	if s, ok := v["message"].(string); ok {
		return s
	}
	return ""
}