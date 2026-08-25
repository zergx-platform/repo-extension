package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// jjClient is a thin, status-aware client for the jj-server REST API.
//
// Caveat encoded here: jj-server's repo `open` auto-initializes missing repos,
// so any read on a repo path may create it. Existence checks therefore go
// through the read-only `GET /api/v1/repos` directory listing.
type jjClient struct {
	base string
	hc   *http.Client
}

func newJJClient(base string) *jjClient {
	return &jjClient{base: base, hc: &http.Client{Timeout: 30 * time.Second}}
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
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var v map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, v, nil
}

// repoTree is the read-only snapshot of org → repo → bookmarks.
type repoTree map[string]map[string][]string

// GetRepoTree lists every org/repo/bookmark via the read-only directory scan.
func (c *jjClient) GetRepoTree(ctx context.Context) (repoTree, error) {
	status, v, err := c.call(ctx, http.MethodGet, "/api/v1/repos", nil)
	if err != nil {
		return nil, errDownstream("jj-server", err)
	}
	if status != 200 {
		return nil, errDownstream("jj-server", fmt.Errorf("GET /repos: HTTP %d", status))
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
			var bms []string
			bmlist, _ := rm["bookmarks"].([]interface{})
			for _, be := range bmlist {
				if bm, ok := be.(map[string]interface{}); ok {
					if b, _ := bm["branch"].(string); b != "" {
						bms = append(bms, b)
					}
				}
			}
			tree[org][repo] = bms
		}
	}
	return tree, nil
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
	bms, ok := repos[repo]
	if !ok {
		return false
	}
	for _, b := range bms {
		if b == bookmark {
			return true
		}
	}
	return false
}

// EnsureOrg creates the org directory + registry row (idempotent).
func (c *jjClient) EnsureOrg(ctx context.Context, org string) error {
	status, v, err := c.call(ctx, http.MethodPost, "/api/v1/repos/ensure-org", map[string]interface{}{"org": org})
	if err != nil {
		return errDownstream("jj-server", err)
	}
	if status != 200 {
		return errDownstream("jj-server", fmt.Errorf("ensure-org: HTTP %d %s", status, errText(v)))
	}
	return nil
}

// EnsureRepo opens-or-initializes org/repo (idempotent; init creates `main`).
func (c *jjClient) EnsureRepo(ctx context.Context, org, repo string) error {
	status, v, err := c.call(ctx, http.MethodPost, "/api/v1/repos/ensure", map[string]interface{}{"org": org, "repo": repo})
	if err != nil {
		return errDownstream("jj-server", err)
	}
	if status != 200 {
		return errDownstream("jj-server", fmt.Errorf("ensure: HTTP %d %s", status, errText(v)))
	}
	return nil
}

// EnsureBookmark creates bookmark pointing at src, unless it already exists
// (idempotent forward step). src may be a bookmark name OR a change/commit
// id — jj-server's full resolver handles both (lineage anchoring).
func (c *jjClient) EnsureBookmark(ctx context.Context, org, repo, src, bookmark string) error {
	tree, err := c.GetRepoTree(ctx)
	if err != nil {
		return err
	}
	if tree.bookmarkExists(org, repo, bookmark) {
		return nil
	}
	if !tree.repoExists(org, repo) {
		return errNotFound("仓库 %s/%s 不存在", org, repo)
	}
	status, v, err := c.call(ctx, http.MethodPost,
		"/api/v1/repos/"+url.PathEscape(org)+"/"+url.PathEscape(repo)+"/bookmarks",
		map[string]interface{}{"rev": src, "branch": bookmark})
	if err != nil {
		return errDownstream("jj-server", err)
	}
	if status != 200 {
		return errDownstream("jj-server", fmt.Errorf("create bookmark %q: HTTP %d %s", src, status, errText(v)))
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
		"/api/v1/repos/"+url.PathEscape(org)+"/"+url.PathEscape(repo)+"/"+url.PathEscape(bookmark), nil)
	if err != nil {
		return errDownstream("jj-server", err)
	}
	if status != 200 {
		return errDownstream("jj-server", fmt.Errorf("delete bookmark: HTTP %d %s", status, errText(v)))
	}
	return nil
}

// CanResolve reports whether rev (bookmark / commit-id / change-id) resolves
// in the repo. Read-only via the tree-at-rev endpoint. The repo must already
// exist — the endpoint auto-inits missing repos.
func (c *jjClient) CanResolve(ctx context.Context, org, repo, rev string) (bool, error) {
	status, _, err := c.call(ctx, http.MethodGet,
		"/api/v1/repos/"+url.PathEscape(org)+"/"+url.PathEscape(repo)+"/tree/"+url.PathEscape(rev), nil)
	if err != nil {
		return false, errDownstream("jj-server", err)
	}
	switch status {
	case 200:
		return true, nil
	case 404:
		return false, nil
	default:
		return false, errDownstream("jj-server", fmt.Errorf("resolve %q: HTTP %d", rev, status))
	}
}

func errText(v map[string]interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v["error"].(string); ok {
		return s
	}
	return ""
}
