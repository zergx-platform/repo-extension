package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"

	abcprotocol "forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go"
	"forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go/extension"
	"forgejo.develop.10.199.64.20.nip.io/zergx/go-shared/httpx"
)

// handlers binds each repo tool to its implementation, forwarding to the
// jjlab REST surface. Descriptions/schemas live in manifest.yaml (the single
// declarative protocol source); each handler is bound by tool name.
func (s *server) handlers() map[string]extension.ToolSpec {
	// contentsPath addresses a file at the session bookmark via the
	// Gitea-style `?ref=` snapshot query.
	contentsPath := func(o, r, b, path string) string {
		return "/api/v1/repos/" + url.PathEscape(o) + "/" + url.PathEscape(r) +
			"/contents/" + escPath(path) + "?ref=" + url.QueryEscape(b)
	}

	// readFileRaw fetches a file and returns (raw utf8, sha, size) or error.
	// jjlab responds with `encoding: base64` + base64 `content` (Gitea shape),
	// never with a plain-text body.
	readFileRaw := func(ctx context.Context, o, r, b, path string) (string, string, int64, error) {
		v, err := s.jj.get(ctx, contentsPath(o, r, b, path))
		if err != nil {
			return "", "", 0, err
		}
		rawContent, _ := v["content"].(string)
		raw, derr := base64.StdEncoding.DecodeString(rawContent)
		if derr != nil {
			return "", "", 0, fmt.Errorf("failed to base64-decode file content")
		}
		text := string(raw)
		sha, _ := v["sha"].(string)
		size := int64(len(text))
		if sz, ok := v["size"].(float64); ok {
			size = int64(sz)
		}
		return text, sha, size, nil
	}

	// fileBody builds the Gitea-style write body: base64 content + branch.
	fileBody := func(b, content, message string) map[string]interface{} {
		return map[string]interface{}{
			"content_base64": base64.StdEncoding.EncodeToString([]byte(content)),
			"branch":         b,
			"message":        message,
		}
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
				if len(lines) > 0 && lines[len(lines)-1] == "" {
					lines = lines[:len(lines)-1]
				}
				totalLines := int64(len(lines))

				startIdx := offset - 1
				if startIdx > int64(len(lines)) {
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
					"path": path, "sha": sha, "size": size, "total_lines": totalLines, "truncated": truncated,
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
				v, err := s.jj.put(ctx, contentsPath(o, r, b, path), fileBody(b, content, message))
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("failed to write file: %w", err)
				}
				changeID := strVal(v, "change_id")
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
				v, err := s.jj.delete(ctx, contentsPath(o, r, b, path), map[string]interface{}{"branch": b, "message": message})
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("failed to delete file: %w", err)
				}
				changeID := strVal(v, "change_id")
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

				text, sha, _, err := readFileRaw(ctx, o, r, b, path)
				if err != nil {
					if errors.Is(err, httpx.ErrNotFound) {
						return extension.ToolResultData{}, fmt.Errorf("failed to read file '%s': not found or inaccessible", path)
					}
					return extension.ToolResultData{}, fmt.Errorf("read '%s' before edit: %w", path, err)
				}

				newContent, err := applyLineEdit(text, startLine, endLine, content)
				if err != nil {
					return extension.ToolResultData{}, err
				}

				v, err := s.jj.put(ctx, contentsPath(o, r, b, path), fileBody(b, newContent, message))
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("failed to write edited result: %w", err)
				}
				changeID := strVal(v, "change_id")

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
				v, err := s.jj.get(ctx, "/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/tree/"+url.PathEscape(b))
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("failed to list directory: %w", err)
				}
				entries := toEntries(v)
				dirs, files := 0, 0
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
				q := url.Values{"pattern": {pattern}, "ref": {b}}
				v, err := s.jj.get(ctx, "/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/search?"+q.Encode())
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
				tree, err := s.jj.GetRepoTree(ctx)
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("failed to browse structure: %w", err)
				}
				orgArg := abcprotocol.ArgString(args, "org")
				repoArg := abcprotocol.ArgString(args, "repo")

				var sb strings.Builder
				meta := []interface{}{}
				for org, repos := range tree {
					if orgArg != "" && org != orgArg {
						continue
					}
					fmt.Fprintf(&sb, "organization '%s' (%d repo(s)):\n", org, len(repos))
					rmeta := []interface{}{}
					for repo, bms := range repos {
						if repoArg != "" && repo != repoArg {
							continue
						}
						fmt.Fprintf(&sb, "  - %s (branches: %s)\n", repo, strings.Join(bms, ", "))
						rmeta = append(rmeta, map[string]interface{}{"repo": repo, "branches": bms})
					}
					meta = append(meta, map[string]interface{}{"org": org, "repos": rmeta})
				}
				if len(meta) == 0 {
					return extension.ToolResultData{Content: "no organizations or repositories.", Data: map[string]interface{}{"orgs": []interface{}{}}}, nil
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
				q := url.Values{"base": {revA}, "head": {revB}}
				v, err := s.jj.get(ctx, "/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/compare?"+q.Encode())
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("failed to get diff: %w", err)
				}
				diff := strVal(v, "diff")
				scope := "tree"
				if path != "" {
					diff = diffForPath(diff, path)
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
				body := map[string]interface{}{"source": source, "dest": b}
				v, err := s.jj.post(ctx, "/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/rebase", body)
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
				message := "resolve " + path
				v, err := s.jj.put(ctx, contentsPath(o, r, b, path), fileBody(b, content, message))
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("resolve failed: %w", err)
				}
				commitID := strVal(v, "sha")
				changeID := strVal(v, "change_id")
				return extension.ToolResultData{Content: fmt.Sprintf("resolved '%s' (tip %s, change %s).", path, shortID(commitID), shortID(changeID)), Data: map[string]interface{}{"commit_id": commitID, "change_id": changeID, "conflicts": []interface{}{}}}, nil
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
				if rev == "" {
					rev = "main"
				}
				if path == "" {
					return extension.ToolResultData{}, fmt.Errorf("missing 'path' argument")
				}
				q := url.Values{"rev": {rev}}
				v, err := s.jj.get(ctx, "/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/annotate/"+escPath(path)+"?"+q.Encode())
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("failed to get blame: %w", err)
				}
				anns := arraySlice(v["annotations"])
				var sb strings.Builder
				fmt.Fprintf(&sb, "per-line origin of file '%s':\n", path)
				meta := make([]interface{}, 0, len(anns))
				for _, a := range anns {
					m, ok := a.(map[string]interface{})
					if !ok {
						continue
					}
					commit := strFrom(m, "commit_id")
					change := strFrom(m, "change_id")
					content, _ := m["content"].(string)
					// Show the change-id (the semantic unit) when available.
					owner := change
					if owner == "" {
						owner = commit
					}
					fmt.Fprintf(&sb, "  %s: %s", shortID(owner), content)
					if !strings.HasSuffix(content, "\n") {
						fmt.Fprintln(&sb)
					}
					meta = append(meta, map[string]interface{}{"commit_id": commit, "change_id": change, "content": content})
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
				q := url.Values{"limit": {fmt.Sprintf("%d", limit)}, "page": {"1"}}
				if rev := abcprotocol.ArgString(args, "rev"); rev != "" {
					q.Set("sha", rev)
				} else {
					if b := abcprotocol.ArgString(args, "_branch"); b != "" {
						q.Set("sha", b)
					}
				}
				if since := abcprotocol.ArgString(args, "since"); since != "" {
					q.Set("since", since)
				}
				if until := abcprotocol.ArgString(args, "until"); until != "" {
					q.Set("until", until)
				}
				v, err := s.jj.get(ctx, "/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/commits?"+q.Encode())
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
					meta = append(meta, map[string]interface{}{"change_id": c.changeID, "message": c.message, "author": c.author})
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
				if rev == "" {
					return extension.ToolResultData{}, fmt.Errorf("missing 'rev' argument")
				}
				v, err := s.jj.get(ctx, "/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/commits/"+url.PathEscape(rev)+"/diff")
				if err != nil {
					return extension.ToolResultData{}, fmt.Errorf("failed to view change: %w", err)
				}
				patch := strVal(v, "diff")
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
				v, err := s.jj.get(ctx, "/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/branches")
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

// sessionBaseXO is sessionBase with an explicit `org`/`repo`/`branch` override
// (read-only cross-repo access); the bookmark still defaults to the workspace.
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

func arraySlice(v interface{}) []interface{} {
	if arr, ok := v.([]interface{}); ok {
		return arr
	}
	return nil
}

// splitMatch splits "path:line:text" into (path, line, text); line/text are
// empty when the backend omits them.
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

// diffForPath filters a unified diff down to the entries for a single path.
// The backend's `compare` returns a whole-tree patch; Gitea has no path filter
// on compare, so the tool narrows client-side.
func diffForPath(diff, path string) string {
	header := "a/" + path + " b/" + path
	var out []string
	for _, block := range strings.Split(diff, "\ndiff --git ") {
		trimmed := block
		if i := strings.Index(trimmed, "\n"); i >= 0 {
			if trimmed[:i] == header {
				out = append(out, "diff --git "+trimmed)
			}
		}
	}
	return strings.Join(out, "\n")
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
		if k, _ := m["kind"].(string); k == "tree" {
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

type commitInfo struct {
	changeID string
	message  string
	author   string
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
			changeID: strFrom(m, "change_id"),
			message:  strFrom(m, "description"),
			author:   strFrom(m, "author"),
		})
	}
	return out
}

type branchInfo struct {
	branch string
	target string
}

func toBranches(v map[string]interface{}) []branchInfo {
	arr, ok := v["branches"].([]interface{})
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
			branch: strFrom(m, "name"),
			target: strFrom(m, "sha"),
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
// original editor tools. start_line is 1-based; end_line < start_line means
// insert before start_line.
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