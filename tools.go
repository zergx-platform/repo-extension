package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	abcprotocol "forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go"
	"forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go/extension"
	"forgejo.develop.10.199.64.20.nip.io/zergx/go-shared/httpx"
)

// tools returns the repo tools, forwarding each to the jjlab Contents API.
// handlers returns the repo tool handlers, forwarding each to the jjlab
// Contents API. Descriptions/schemas live in manifest.yaml (the single
// declarative protocol source); each handler is bound by tool name.
func (s *server) handlers() map[string]extension.ToolSpec {
	contentsPath := func(o, r, b, path string) string {
		return s.base + "/api/v1/repos/" + url.PathEscape(o) + "/" + url.PathEscape(r) + "/" +
			url.PathEscape(b) + "/contents/" + escPath(path)
	}

	// readFileRaw fetches the file; returns (raw utf8, sha, size) or error.
	readFileRaw := func(ctx context.Context, o, r, b, path string) (string, string, int64, error) {
		v, err := httpx.Get(ctx, contentsPath(o, r, b, path))
		if err != nil {
			return "", "", 0, err
		}
		enc, _ := v["encoding"].(string)
		rawContent, _ := v["content"].(string)
		var text string
		if enc == "base64" {
			raw, derr := base64.StdEncoding.DecodeString(rawContent)
			if derr != nil {
				return "", "", 0, fmt.Errorf("failed to base64-decode file content")
			}
			text = string(raw)
		} else {
			text = rawContent
		}
		sha, _ := v["sha"].(string)
		size := int64(len(text))
		if sz, ok := v["size"].(float64); ok {
			size = int64(sz)
		}
		return text, sha, size, nil
	}

	return map[string]extension.ToolSpec{
		"read": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, b, err := s.sessionBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				path := abcprotocol.ArgString(args, "path")
				if path == "" {
					return extension.ToolResultData{}, fmt.Errorf("missing 'path' argument")
				}
				text, sha, size, err := readFileRaw(ctx, o, r, b, path)
				if err != nil {
					// 404 keeps the agent-friendly wording; any other
					// failure (5xx, timeout, bad body) is a real error so
					// the agent never mistakes an outage for a missing file.
					if errors.Is(err, httpx.ErrNotFound) {
						return extension.ToolResultData{Content: fmt.Sprintf("failed to read file '%s': not found or inaccessible", path)}, nil
					}
					return extension.ToolResultData{}, fmt.Errorf("read '%s': %w", path, err)
				}
				offset := abcprotocol.ArgInt(args, "offset", 1)
				limit := abcprotocol.ArgInt(args, "limit", 0)
				if offset < 1 {
					offset = 1
				}

				lines := strings.Split(text, "\n")
				// strip trailing empty line from split (a trailing \n yields a final "")
				if len(lines) > 0 && lines[len(lines)-1] == "" {
					lines = lines[:len(lines)-1]
				}
				totalLines := int64(len(lines))

				startIdx := offset - 1
				if startIdx > int64(len(lines)) {
					// offset beyond EOF
					return extension.ToolResultData{Content: fmt.Sprintf("file '%s' has %d lines; offset=%d is past the end.", path, totalLines, offset), Data: map[string]interface{}{
						"path": path, "sha": sha, "size": size, "total_lines": totalLines, "truncated": false,
					}}, nil
				}

				endIdx := int64(len(lines))
				truncated := false
				if limit > 0 {
					want := startIdx + limit
					if want < endIdx {
						endIdx = want
						truncated = true
					}
				}

				var sb strings.Builder
				for i := startIdx; i < endIdx; i++ {
					fmt.Fprintf(&sb, "%d: %s\n", i+1, lines[i])
				}
				content := sb.String()
				nextOffset := endIdx + 1
				if truncated {
					fmt.Fprintf(&sb, "\n(file not fully read: showing lines %d-%d of %d; continue with offset=%d)\n", startIdx+1, endIdx, totalLines, nextOffset)
					content = sb.String()
				}

				meta := map[string]interface{}{
					"path":        path,
					"sha":         sha,
					"size":        size,
					"total_lines": totalLines,
					"truncated":   truncated,
				}
				if truncated {
					meta["next_offset"] = nextOffset
				}
				return extension.ToolResultData{Content: content, Data: meta}, nil
			},
		},
		"write": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, b, err := s.sessionBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				path := abcprotocol.ArgString(args, "path")
				if path == "" {
					return extension.ToolResultData{}, fmt.Errorf("missing 'path' argument")
				}
				content := abcprotocol.ArgString(args, "content")
				message := abcprotocol.ArgString(args, "message")
				if message == "" {
					message = "write " + path
				}
				body := map[string]interface{}{
					"content": base64.StdEncoding.EncodeToString([]byte(content)),
					"message": message,
				}
				v, err := httpx.Put(ctx, contentsPath(o, r, b, path), body)
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("failed to write file: %w", err)
				}
				changeID := strVal(v, "change_id")
				if changeID == "" {
					// fall back to commit_id for older backend
					changeID = strVal(v, "commit_id")
				}
				return extension.ToolResultData{Content: fmt.Sprintf("wrote file '%s' (change %s)", path, shortID(changeID)), Data: map[string]interface{}{
					"path": path, "change_id": changeID,
				}}, nil
			},
		},
		"delete": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, b, err := s.sessionBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				path := abcprotocol.ArgString(args, "path")
				if path == "" {
					return extension.ToolResultData{}, fmt.Errorf("missing 'path' argument")
				}
				message := abcprotocol.ArgString(args, "message")
				if message == "" {
					message = "delete " + path
				}
				v, err := httpx.Delete(ctx, contentsPath(o, r, b, path), map[string]interface{}{"message": message})
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("failed to delete file: %w", err)
				}
				changeID := strVal(v, "change_id")
				if changeID == "" {
					changeID = strVal(v, "commit_id")
				}
				return extension.ToolResultData{Content: fmt.Sprintf("deleted file '%s' (change %s)", path, shortID(changeID)), Data: map[string]interface{}{
					"path": path, "change_id": changeID,
				}}, nil
			},
		},
		"edit": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, b, err := s.sessionBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				path := abcprotocol.ArgString(args, "path")
				if path == "" {
					return extension.ToolResultData{}, fmt.Errorf("missing 'path' argument")
				}
				startLine := abcprotocol.ArgInt(args, "start_line", 0)
				endLine := abcprotocol.ArgInt(args, "end_line", 0)
				content := abcprotocol.ArgString(args, "content")
				message := abcprotocol.ArgString(args, "message")
				if message == "" {
					message = "edit " + path
				}

				// 1. read + capture sha (read-before-edit check)
				url := contentsPath(o, r, b, path)
				cur, err := httpx.Get(ctx, url)
				if err != nil {
					if errors.Is(err, httpx.ErrNotFound) {
						return extension.ToolResultData{}, fmt.Errorf("failed to read file '%s': not found or inaccessible", path)
					}
					return extension.ToolResultData{}, fmt.Errorf("read '%s' before edit: %w", path, err)
				}
				sha := strVal(cur, "sha")
				var current string
				if enc, _ := cur["encoding"].(string); enc == "base64" {
					raw, derr := base64.StdEncoding.DecodeString(strVal(cur, "content"))
					if derr != nil {
						return extension.ToolResultData{}, fmt.Errorf("failed to base64-decode file content")
					}
					current = string(raw)
				} else {
					current = strVal(cur, "content")
				}

				// 2. apply line edit
				newContent, err := applyLineEdit(current, startLine, endLine, content)
				if err != nil {
					return extension.ToolResultData{}, err
				}

				// 3. write back with sha CAS
				body := map[string]interface{}{
					"content": base64.StdEncoding.EncodeToString([]byte(newContent)),
					"message": message,
					"sha":     sha,
				}
				v, err := httpx.Put(ctx, url, body)
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("failed to write edited result: %w", err)
				}
				changeID := strVal(v, "change_id")
				if changeID == "" {
					changeID = strVal(v, "commit_id")
				}

				var desc string
				if endLine < startLine {
					desc = fmt.Sprintf("inserted %d line(s) before line %d", startLine, countLines(content))
				} else {
					desc = fmt.Sprintf("replaced lines %d-%d", startLine, endLine)
				}
				return extension.ToolResultData{Content: fmt.Sprintf("edited file '%s': %s (change %s)", path, desc, shortID(changeID)), Data: map[string]interface{}{
					"path": path, "start_line": startLine, "end_line": endLine,
					"old_sha": sha, "change_id": changeID,
				}}, nil
			},
		},
		"ls": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, b, err := s.sessionBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				v, err := httpx.Get(ctx, s.base+"/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/"+url.PathEscape(b)+"/tree")
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("failed to list directory: %w", err)
				}
				entries := toEntries(v)
				dirs := 0
				files := 0
				for _, e := range entries {
					if e.isDir {
						dirs++
					} else {
						files++
					}
				}
				var sb strings.Builder
				fmt.Fprintf(&sb, "branch '%s' has %d entries (%d dirs, %d files):\n", b, len(entries), dirs, files)
				for _, e := range entries {
					if e.isDir {
						fmt.Fprintf(&sb, "  [dir] %s/\n", e.path)
					} else {
						fmt.Fprintf(&sb, "  %s\n", e.path)
					}
				}
				return extension.ToolResultData{Content: sb.String(), Data: map[string]interface{}{"entries": entriesSlice(entries)}}, nil
			},
		},
		"grep": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, b, err := s.sessionBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				pattern := abcprotocol.ArgString(args, "pattern")
				if pattern == "" {
					return extension.ToolResultData{}, fmt.Errorf("missing 'pattern' argument")
				}
				q := url.Values{"pattern": {pattern}}
				v, err := httpx.Get(ctx, s.base+"/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/"+url.PathEscape(b)+"/search?"+q.Encode())
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("search failed: %w", err)
				}
				matches := strSlice(v, "matches")
				if len(matches) == 0 {
					return extension.ToolResultData{Content: fmt.Sprintf("no matches for '%s' in branch '%s'.", b, pattern), Data: map[string]interface{}{"matches": []interface{}{}, "count": 0}}, nil
				}
				var sb strings.Builder
				fmt.Fprintf(&sb, "found %d match(es):\n", len(matches))
				for _, m := range matches {
					fmt.Fprintf(&sb, "  %s\n", m)
				}
				// parse into structured matches
				parsed := make([]interface{}, 0, len(matches))
				for _, m := range matches {
					path, line, text := splitMatch(m)
					parsed = append(parsed, map[string]interface{}{"path": path, "line": line, "text": text})
				}
				return extension.ToolResultData{Content: sb.String(), Data: map[string]interface{}{"matches": parsed, "count": len(matches)}}, nil
			},
		},
		"explore": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				v, err := httpx.Get(ctx, s.base+"/api/v1/repos")
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("failed to browse structure: %w", err)
				}
				orgs := toOrgs(v)
				var sb strings.Builder
				if len(orgs) == 0 {
					return extension.ToolResultData{Content: "no organizations or repositories.", Data: map[string]interface{}{"orgs": []interface{}{}}}, nil
				}
				for _, o := range orgs {
					fmt.Fprintf(&sb, "organization '%s' (%d repo(s)):\n", o.org, len(o.repos))
					for _, r := range o.repos {
						fmt.Fprintf(&sb, "  - %s (branches: %s)\n", r.repo, strings.Join(r.branches, ", "))
					}
				}
				meta := make([]interface{}, 0, len(orgs))
				for _, o := range orgs {
					repos := make([]interface{}, 0, len(o.repos))
					for _, r := range o.repos {
						repos = append(repos, map[string]interface{}{"repo": r.repo, "branches": r.branches})
					}
					meta = append(meta, map[string]interface{}{"org": o.org, "repos": repos})
				}
				return extension.ToolResultData{Content: sb.String(), Data: map[string]interface{}{"orgs": meta}}, nil
			},
		},
		"git-diff": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, _, err := s.sessionBaseXO(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				revA := abcprotocol.ArgString(args, "rev_a")
				revB := abcprotocol.ArgString(args, "rev_b")
				if revA == "" || revB == "" {
					return extension.ToolResultData{}, fmt.Errorf("rev_a and rev_b are required")
				}
				path := abcprotocol.ArgString(args, "path")
				q := url.Values{"rev_a": {revA}, "rev_b": {revB}}
				if path != "" {
					q.Set("path", path)
				}
				v, err := httpx.Get(ctx, s.base+"/api/v1/git-diff/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"?"+q.Encode())
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("failed to get diff: %w", err)
				}
				diff := strVal(v, "diff")
				scope := "tree"
				if path != "" {
					scope = fmt.Sprintf("file '%s'", path)
				}
				if strings.TrimSpace(diff) == "" {
					return extension.ToolResultData{Content: fmt.Sprintf("no diff between '%s' and '%s' (%s).", revA, revB, scope), Data: map[string]interface{}{"path": path, "rev_a": revA, "rev_b": revB}}, nil
				}
				return extension.ToolResultData{Content: fmt.Sprintf("diff (%s) between '%s'..'%s':\n%s", scope, revA, revB, diff), Data: map[string]interface{}{"path": path, "rev_a": revA, "rev_b": revB}}, nil
			},
		},
		"git-rebase": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, b, err := s.sessionBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				source := abcprotocol.ArgString(args, "source")
				if source == "" {
					return extension.ToolResultData{}, fmt.Errorf("missing 'source' argument")
				}
				// The destination is always this session's own branch (b): a tool
				// must never move another branch's bookmark. The divergent commits
				// of `source` are rebased onto `b`, and `b` advances.
				body := map[string]interface{}{"source": source, "dest": b}
				v, err := httpx.Post(ctx, s.base+"/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/"+url.PathEscape(b)+"/rebase", body)
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("rebase failed: %w", err)
				}
				sum, _ := v["rebase"].(map[string]interface{})
				commitID := strVal(sum, "commit_id")
				conflicts := strSlice(sum, "conflicts")
				if len(conflicts) > 0 {
					return extension.ToolResultData{Content: fmt.Sprintf("rebased '%s' onto '%s' with %d conflict(s): %s", source, b, len(conflicts), strings.Join(conflicts, ", ")), Data: map[string]interface{}{"commit_id": commitID, "conflicts": conflicts}}, nil
				}
				return extension.ToolResultData{Content: fmt.Sprintf("rebased '%s' onto '%s' (tip %s).", source, b, shortID(commitID)), Data: map[string]interface{}{"commit_id": commitID, "conflicts": []interface{}{}}}, nil
			},
		},
		"git-resolve": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, b, err := s.sessionBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				path := abcprotocol.ArgString(args, "path")
				if path == "" {
					return extension.ToolResultData{}, fmt.Errorf("missing 'path' argument")
				}
				content := abcprotocol.ArgString(args, "content")
				body := map[string]interface{}{"path": path, "content": content}
				v, err := httpx.Post(ctx, s.base+"/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/"+url.PathEscape(b)+"/resolve", body)
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("resolve failed: %w", err)
				}
				sum, _ := v["resolve"].(map[string]interface{})
				commitID := strVal(sum, "commit_id")
				conflicts := strSlice(sum, "conflicts")
				if len(conflicts) > 0 {
					return extension.ToolResultData{Content: fmt.Sprintf("resolved '%s' (tip %s); remaining conflicts: %s", path, shortID(commitID), strings.Join(conflicts, ", ")), Data: map[string]interface{}{"commit_id": commitID, "conflicts": conflicts}}, nil
				}
				return extension.ToolResultData{Content: fmt.Sprintf("resolved '%s' (tip %s); no remaining conflicts.", path, shortID(commitID)), Data: map[string]interface{}{"commit_id": commitID, "conflicts": []interface{}{}}}, nil
			},
		},
		"git-blame": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, _, err := s.sessionBaseXO(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				rev := abcprotocol.ArgString(args, "rev")
				path := abcprotocol.ArgString(args, "path")
				q := url.Values{"rev": {rev}, "path": {path}}
				v, err := httpx.Get(ctx, s.base+"/api/v1/git-blame/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"?"+q.Encode())
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("failed to get blame: %w", err)
				}
				lines := strSlice(v, "blame")
				var sb strings.Builder
				fmt.Fprintf(&sb, "per-line origin of file '%s':\n", path)
				meta := make([]interface{}, 0, len(lines))
				for _, line := range lines {
					commit, text := splitBlame(line)
					fmt.Fprintf(&sb, "  %s: %s\n", shortID(commit), text)
					meta = append(meta, map[string]interface{}{"commit_id": commit, "content": text})
				}
				return extension.ToolResultData{Content: sb.String(), Data: map[string]interface{}{"lines": meta}}, nil
			},
		},
		"git-log": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, _, err := s.sessionBaseXO(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				limit := abcprotocol.ArgInt(args, "limit", 50)
				q := url.Values{"limit": {fmt.Sprintf("%d", limit)}}
				if rev := abcprotocol.ArgString(args, "rev"); rev != "" {
					q.Set("rev", rev)
				}
				if since := abcprotocol.ArgString(args, "since"); since != "" {
					q.Set("since", since)
				}
				if until := abcprotocol.ArgString(args, "until"); until != "" {
					q.Set("until", until)
				}
				v, err := httpx.Get(ctx, s.base+"/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/log?"+q.Encode())
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("failed to get commit history: %w", err)
				}
				commits := toCommits(v)
				var sb strings.Builder
				if len(commits) == 0 {
					return extension.ToolResultData{Content: "no commits.", Data: map[string]interface{}{"commits": []interface{}{}}}, nil
				}
				fmt.Fprintf(&sb, "latest %d commit(s):\n", len(commits))
				meta := make([]interface{}, 0, len(commits))
				for _, c := range commits {
					msg := c.message
					if msg == "" {
						msg = "(no description)"
					}
					fmt.Fprintf(&sb, "  %s %s（%s）\n", shortID(c.changeID), msg, c.author)
					meta = append(meta, map[string]interface{}{"change_id": c.changeID, "message": c.message, "author": c.author, "timestamp": c.timestamp})
				}
				return extension.ToolResultData{Content: sb.String(), Data: map[string]interface{}{"commits": meta}}, nil
			},
		},
		"git-show": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, _, err := s.sessionBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				rev := abcprotocol.ArgString(args, "rev")
				v, err := httpx.Get(ctx, s.base+"/api/v1/git-show/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/"+url.PathEscape(rev))
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("failed to view change: %w", err)
				}
				patch := strVal(v, "patch")
				if strings.TrimSpace(patch) == "" {
					return extension.ToolResultData{Content: fmt.Sprintf("change '%s' has no content diff.", rev), Data: map[string]interface{}{"rev": rev}}, nil
				}
				return extension.ToolResultData{Content: fmt.Sprintf("changes of '%s':\n%s", rev, patch), Data: map[string]interface{}{"rev": rev, "patch": patch}}, nil
			},
		},
		"git-branches": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, _, err := s.sessionBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				v, err := httpx.Get(ctx, s.base+"/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/bookmarks")
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("failed to list branches: %w", err)
				}
				branches := toBranches(v)
				var sb strings.Builder
				if len(branches) == 0 {
					return extension.ToolResultData{Content: "no branches.", Data: map[string]interface{}{"branches": []interface{}{}}}, nil
				}
				fmt.Fprintf(&sb, "branches (%d):\n", len(branches))
				meta := make([]interface{}, 0, len(branches))
				for _, br := range branches {
					fmt.Fprintf(&sb, "  %s（%s）\n", br.branch, shortID(br.target))
					meta = append(meta, map[string]interface{}{"branch": br.branch, "target": br.target})
				}
				return extension.ToolResultData{Content: sb.String(), Data: map[string]interface{}{"branches": meta}}, nil
			},
		},
	}
}

// sessionBase resolves the (org, repo, bookmark) triple for a tool call.
// Priority: the first-class `session_name` envelope field (resolved via the
// mapping table) → legacy `_org`/`_repo`/`_branch` args.
func (s *server) sessionBase(ctx context.Context, args map[string]interface{}, sessionName string) (string, string, string, error) {
	if sessionName != "" {
		return s.resolveSession(ctx, sessionName)
	}
	o, r, b := abcprotocol.ArgString(args, "_org"), abcprotocol.ArgString(args, "_repo"), abcprotocol.ArgString(args, "_branch")
	if o == "" || r == "" {
		return "", "", "", fmt.Errorf("missing session context (session_name or _org/_repo)")
	}
	return o, r, b, nil
}

// sessionBaseXO is sessionBase with an explicit argument `org`/`repo` capable
// of overriding the session workspace (read-only cross-repo access). The
// bookmark still defaults to the workspace's branch when not passed.
func (s *server) sessionBaseXO(ctx context.Context, args map[string]interface{}, sessionName string) (string, string, string, error) {
	o, r, b, err := s.sessionBase(ctx, args, sessionName)
	if err != nil {
		return "", "", "", err
	}
	if arg := abcprotocol.ArgString(args, "org"); arg != "" {
		o = arg
	}
	if arg := abcprotocol.ArgString(args, "repo"); arg != "" {
		r = arg
	}
	if arg := abcprotocol.ArgString(args, "branch"); arg != "" {
		b = arg
	} else if arg := abcprotocol.ArgString(args, "bookmark"); arg != "" {
		b = arg
	}
	return o, r, b, nil
}

func escPath(p string) string {
	p = strings.TrimPrefix(p, "/")
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func strVal(v map[string]interface{}, k string) string {
	if s, ok := v[k].(string); ok {
		return s
	}
	return ""
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

func strSlice(v map[string]interface{}, k string) []string {
	if arr, ok := v[k].([]interface{}); ok {
		out := make([]string, 0, len(arr))
		for _, e := range arr {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// splitBlame splits "commitId content" into (commitId, content).
func splitBlame(line string) (string, string) {
	i := strings.IndexByte(line, ' ')
	if i == -1 {
		return line, ""
	}
	return line[:i], line[i+1:]
}

// splitMatch splits "path:line:text" into (path, line, text). Line numbers may
// be absent for some backends, in which case line=="".
func splitMatch(m string) (string, string, string) {
	i := strings.IndexByte(m, ':')
	if i == -1 {
		return m, "", ""
	}
	path := m[:i]
	rest := m[i+1:]
	j := strings.IndexByte(rest, ':')
	if j == -1 {
		return path, "", rest
	}
	return path, rest[:j], rest[j+1:]
}

type entry struct {
	path  string
	isDir bool
	size  int64
}

func toEntries(v map[string]interface{}) []entry {
	arr, ok := v["tree"].([]interface{})
	if !ok {
		return nil
	}
	out := make([]entry, 0, len(arr))
	for _, e := range arr {
		m, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		p, _ := m["path"].(string)
		isDir := false
		if t, _ := m["type"].(string); t == "tree" {
			isDir = true
		}
		var size int64
		if s, ok := m["size"].(float64); ok {
			size = int64(s)
		}
		out = append(out, entry{path: p, isDir: isDir, size: size})
	}
	return out
}

func entriesSlice(entries []entry) []interface{} {
	out := make([]interface{}, 0, len(entries))
	for _, e := range entries {
		typ := "blob"
		if e.isDir {
			typ = "tree"
		}
		out = append(out, map[string]interface{}{"path": e.path, "type": typ, "size": e.size})
	}
	return out
}

type orgInfo struct {
	org   string
	repos []repoInfo
}

type repoInfo struct {
	repo     string
	branches []string
}

func toOrgs(v map[string]interface{}) []orgInfo {
	arr, ok := v["orgs"].([]interface{})
	if !ok {
		return nil
	}
	out := make([]orgInfo, 0, len(arr))
	for _, e := range arr {
		om, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		org, _ := om["org"].(string)
		oi := orgInfo{org: org}
		if repos, ok := om["repos"].([]interface{}); ok {
			for _, re := range repos {
				rm, ok := re.(map[string]interface{})
				if !ok {
					continue
				}
				repo, _ := rm["repo"].(string)
				ri := repoInfo{repo: repo}
				if bms, ok := rm["bookmarks"].([]interface{}); ok {
					for _, bme := range bms {
						if bm, ok := bme.(map[string]interface{}); ok {
							if b, ok := bm["branch"].(string); ok {
								ri.branches = append(ri.branches, b)
							}
						}
					}
				}
				oi.repos = append(oi.repos, ri)
			}
		}
		out = append(out, oi)
	}
	return out
}

type commitInfo struct {
	changeID  string
	message   string
	author    string
	timestamp string
}

func toCommits(v map[string]interface{}) []commitInfo {
	arr, ok := v["commits"].([]interface{})
	if !ok {
		return nil
	}
	out := make([]commitInfo, 0, len(arr))
	for _, e := range arr {
		m, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		out = append(out, commitInfo{
			changeID:  strFrom(m, "change_id"),
			message:   strFrom(m, "message"),
			author:    strFrom(m, "author"),
			timestamp: strFrom(m, "timestamp"),
		})
	}
	return out
}

type branchInfo struct {
	branch string
	target string
}

func toBranches(v map[string]interface{}) []branchInfo {
	arr, ok := v["bookmarks"].([]interface{})
	if !ok {
		return nil
	}
	out := make([]branchInfo, 0, len(arr))
	for _, e := range arr {
		m, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		out = append(out, branchInfo{
			branch: strFrom(m, "branch"),
			target: strFrom(m, "target"),
		})
	}
	return out
}

func strFrom(m map[string]interface{}, k string) string {
	if s, ok := m[k].(string); ok {
		return s
	}
	return ""
}

// applyLineEdit applies a line-based insert/replace to `current`, mirroring the
// original Rust edit tool semantics. start_line is 1-based; end_line <
// start_line means insert before start_line.
func applyLineEdit(current string, startLine, endLine int64, content string) (string, error) {
	endsNewline := strings.HasSuffix(current, "\n")
	body := strings.TrimSuffix(current, "\n")
	var lines []string
	if body == "" {
		lines = nil
	} else {
		lines = strings.Split(body, "\n")
	}
	total := len(lines)
	if startLine <= 0 {
		return "", fmt.Errorf("start_line must be >= 1")
	}
	if startLine > int64(total)+1 {
		return "", fmt.Errorf("start_line=%d out of range (file has %d lines)", startLine, total)
	}
	if endLine < startLine {
		insertAt := int(startLine - 1)
		if insertAt > total {
			insertAt = total
		}
		var result []string
		result = append(result, lines[:insertAt]...)
		if content != "" {
			result = append(result, strings.Split(content, "\n")...)
		}
		result = append(result, lines[insertAt:]...)
		joined := strings.Join(result, "\n")
		if endsNewline {
			joined += "\n"
		}
		return joined, nil
	}
	if endLine > int64(total)+1 {
		return "", fmt.Errorf("end_line=%d out of range (file has %d lines)", endLine, total)
	}
	s := int(startLine - 1)
	e := int(endLine)
	var result []string
	result = append(result, lines[:s]...)
	if content != "" {
		result = append(result, strings.Split(content, "\n")...)
	}
	result = append(result, lines[e:]...)
	joined := strings.Join(result, "\n")
	if endsNewline {
		joined += "\n"
	}
	return joined, nil
}

// helper to silence unused import if needed
var _ = json.MarshalIndent
