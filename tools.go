package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	extensionsdk "forgejo.develop.10.199.64.20.nip.io/rucoder/extension-sdk-go"
)

// tools returns the repo tools, forwarding each to the jj-server Contents API.
// Each tool returns natural-language `content` (fed to the model) plus a
// `metadata` map carrying only stable values (change_id etc).
func (s *server) tools() map[string]extensionsdk.ToolSpec {
	schema := extensionsdk.Schema
	str := func(string) map[string]interface{} { return extensionsdk.StrProp() }
	intProp := extensionsdk.IntProp

	contentsPath := func(o, r, b, path string) string {
		return s.base + "/api/v1/repos/" + url.PathEscape(o) + "/" + url.PathEscape(r) + "/" +
			url.PathEscape(b) + "/contents/" + escPath(path)
	}

	// readFileRaw fetches the file; returns (raw utf8, sha, size) or error.
	readFileRaw := func(ctx context.Context, o, r, b, path string) (string, string, int64, error) {
		v, err := get(ctx, contentsPath(o, r, b, path))
		if err != nil {
			return "", "", 0, err
		}
		enc, _ := v["encoding"].(string)
		rawContent, _ := v["content"].(string)
		var text string
		if enc == "base64" {
			raw, derr := base64.StdEncoding.DecodeString(rawContent)
			if derr != nil {
				return "", "", 0, fmt.Errorf("文件内容 base64 解码失败")
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

	return map[string]extensionsdk.ToolSpec{
		"read": {
			Description: "读取仓库中的文件，返回带行号(1-based)的内容。可用 offset/limit 分段读取大文件。",
			InputSchema: schema(map[string]interface{}{
				"path":   str("string"),
				"offset": intProp(),
				"limit":  intProp(),
			}, "path"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, map[string]interface{}, error) {
				o, r, b, err := s.sessionBase(ctx, args)
				if err != nil {
					return "", nil, err
				}
				path := extensionsdk.ArgString(args, "path")
				if path == "" {
					return "", nil, fmt.Errorf("缺少 'path' 参数")
				}
				text, sha, size, err := readFileRaw(ctx, o, r, b, path)
				if err != nil {
					return fmt.Sprintf("读取文件 '%s' 失败：文件不存在或无法访问", path), nil, nil
				}
				offset := extensionsdk.ArgInt(args, "offset", 1)
				limit := extensionsdk.ArgInt(args, "limit", 0)
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
					return fmt.Sprintf("文件 '%s' 共 %d 行，offset=%d 已超出文件末尾。", path, totalLines, offset), map[string]interface{}{
						"path": path, "sha": sha, "size": size, "total_lines": totalLines, "truncated": false,
					}, nil
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
					fmt.Fprintf(&sb, "\n（文件未读完：当前显示第 %d-%d 行，共 %d 行。请用 offset=%d 继续读取后续内容）\n", startIdx+1, endIdx, totalLines, nextOffset)
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
				return content, meta, nil
			},
		},
		"write": {
			Description: "在仓库中创建或覆盖一个文件（会自动提交为一次变更）。",
			InputSchema: schema(map[string]interface{}{"path": str("string"), "content": str("string"), "message": str("string")}, "path", "content"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, map[string]interface{}, error) {
				o, r, b, err := s.sessionBase(ctx, args)
				if err != nil {
					return "", nil, err
				}
				path := extensionsdk.ArgString(args, "path")
				if path == "" {
					return "", nil, fmt.Errorf("缺少 'path' 参数")
				}
				content := extensionsdk.ArgString(args, "content")
				message := extensionsdk.ArgString(args, "message")
				if message == "" {
					message = "write " + path
				}
				body := map[string]interface{}{
					"content": base64.StdEncoding.EncodeToString([]byte(content)),
					"message": message,
				}
				v, err := put(ctx, contentsPath(o, r, b, path), body)
				if err != nil {
					return "", nil, fmt.Errorf("写入文件失败：%w", err)
				}
				changeID := strVal(v, "change_id")
				if changeID == "" {
					// fall back to commit_id for older backend
					changeID = strVal(v, "commit_id")
				}
				return fmt.Sprintf("已写入文件 '%s'（变更 %s）", path, shortID(changeID)), map[string]interface{}{
					"path": path, "change_id": changeID,
				}, nil
			},
		},
		"delete": {
			Description: "删除仓库中的一个文件（会自动提交为一次变更）。",
			InputSchema: schema(map[string]interface{}{"path": str("string"), "message": str("string")}, "path"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, map[string]interface{}, error) {
				o, r, b, err := s.sessionBase(ctx, args)
				if err != nil {
					return "", nil, err
				}
				path := extensionsdk.ArgString(args, "path")
				if path == "" {
					return "", nil, fmt.Errorf("缺少 'path' 参数")
				}
				message := extensionsdk.ArgString(args, "message")
				if message == "" {
					message = "delete " + path
				}
				v, err := delBody(ctx, contentsPath(o, r, b, path), map[string]interface{}{"message": message})
				if err != nil {
					return "", nil, fmt.Errorf("删除文件失败：%w", err)
				}
				changeID := strVal(v, "change_id")
				if changeID == "" {
					changeID = strVal(v, "commit_id")
				}
				return fmt.Sprintf("已删除文件 '%s'（变更 %s）", path, shortID(changeID)), map[string]interface{}{
					"path": path, "change_id": changeID,
				}, nil
			},
		},
		"edit": {
			Description: "按行号替换或插入文件内容。先读取校验当前文件，应用行编辑，再写回（带 sha 校验，防止覆盖他人修改）。",
			InputSchema: schema(map[string]interface{}{
				"path":       str("string"),
				"start_line": intProp(),
				"end_line":   intProp(),
				"content":    str("string"),
				"message":    str("string"),
			}, "path", "start_line", "end_line", "content"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, map[string]interface{}, error) {
				o, r, b, err := s.sessionBase(ctx, args)
				if err != nil {
					return "", nil, err
				}
				path := extensionsdk.ArgString(args, "path")
				if path == "" {
					return "", nil, fmt.Errorf("缺少 'path' 参数")
				}
				startLine := extensionsdk.ArgInt(args, "start_line", 0)
				endLine := extensionsdk.ArgInt(args, "end_line", 0)
				content := extensionsdk.ArgString(args, "content")
				message := extensionsdk.ArgString(args, "message")
				if message == "" {
					message = "edit " + path
				}

				// 1. read + capture sha (read-before-edit 校验)
				url := contentsPath(o, r, b, path)
				cur, err := get(ctx, url)
				if err != nil {
					return "", nil, fmt.Errorf("读取文件 '%s' 失败：文件不存在或无法访问", path)
				}
				sha := strVal(cur, "sha")
				var current string
				if enc, _ := cur["encoding"].(string); enc == "base64" {
					raw, derr := base64.StdEncoding.DecodeString(strVal(cur, "content"))
					if derr != nil {
						return "", nil, fmt.Errorf("文件内容 base64 解码失败")
					}
					current = string(raw)
				} else {
					current = strVal(cur, "content")
				}

				// 2. apply line edit
				newContent, err := applyLineEdit(current, startLine, endLine, content)
				if err != nil {
					return "", nil, err
				}

				// 3. write back with sha CAS
				body := map[string]interface{}{
					"content": base64.StdEncoding.EncodeToString([]byte(newContent)),
					"message": message,
					"sha":     sha,
				}
				v, err := put(ctx, url, body)
				if err != nil {
					return "", nil, fmt.Errorf("写入编辑结果失败：%w", err)
				}
				changeID := strVal(v, "change_id")
				if changeID == "" {
					changeID = strVal(v, "commit_id")
				}

				var desc string
				if endLine < startLine {
					desc = fmt.Sprintf("在第 %d 行前插入了 %d 行", startLine, countLines(content))
				} else {
					desc = fmt.Sprintf("替换了第 %d-%d 行", startLine, endLine)
				}
				return fmt.Sprintf("已编辑文件 '%s'：%s（变更 %s）", path, desc, shortID(changeID)), map[string]interface{}{
					"path": path, "start_line": startLine, "end_line": endLine,
					"old_sha": sha, "change_id": changeID,
				}, nil
			},
		},
		"ls": {
			Description: "列出仓库树中的文件与目录。",
			InputSchema: schema(map[string]interface{}{}),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, map[string]interface{}, error) {
				o, r, b, err := s.sessionBase(ctx, args)
				if err != nil {
					return "", nil, err
				}
				v, err := get(ctx, s.base+"/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/"+url.PathEscape(b)+"/tree")
				if err != nil {
					return "", nil, fmt.Errorf("列出目录失败：%w", err)
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
				fmt.Fprintf(&sb, "当前分支 '%s' 共 %d 项（%d 个目录、%d 个文件）：\n", b, len(entries), dirs, files)
				for _, e := range entries {
					if e.isDir {
						fmt.Fprintf(&sb, "  [目录] %s/\n", e.path)
					} else {
						fmt.Fprintf(&sb, "  %s\n", e.path)
					}
				}
				return sb.String(), map[string]interface{}{"entries": entriesSlice(entries)}, nil
			},
		},
		"grep": {
			Description: "用正则表达式搜索文件内容。",
			InputSchema: schema(map[string]interface{}{"pattern": str("string")}, "pattern"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, map[string]interface{}, error) {
				o, r, b, err := s.sessionBase(ctx, args)
				if err != nil {
					return "", nil, err
				}
				pattern := extensionsdk.ArgString(args, "pattern")
				if pattern == "" {
					return "", nil, fmt.Errorf("缺少 'pattern' 参数")
				}
				q := url.Values{"pattern": {pattern}}
				v, err := get(ctx, s.base+"/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/"+url.PathEscape(b)+"/search?"+q.Encode())
				if err != nil {
					return "", nil, fmt.Errorf("搜索失败：%w", err)
				}
				matches := strSlice(v, "matches")
				if len(matches) == 0 {
					return fmt.Sprintf("在 '%s' 分支没有找到匹配 '%s' 的内容。", b, pattern), map[string]interface{}{"matches": []interface{}{}, "count": 0}, nil
				}
				var sb strings.Builder
				fmt.Fprintf(&sb, "找到 %d 处匹配：\n", len(matches))
				for _, m := range matches {
					fmt.Fprintf(&sb, "  %s\n", m)
				}
				// parse into structured matches
				parsed := make([]interface{}, 0, len(matches))
				for _, m := range matches {
					path, line, text := splitMatch(m)
					parsed = append(parsed, map[string]interface{}{"path": path, "line": line, "text": text})
				}
				return sb.String(), map[string]interface{}{"matches": parsed, "count": len(matches)}, nil
			},
		},
		"explore": {
			Description: "浏览项目的组织结构（org/repo/分支）。",
			InputSchema: schema(map[string]interface{}{"org": str("string"), "repo": str("string")}),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, map[string]interface{}, error) {
				v, err := get(ctx, s.base+"/api/v1/repos")
				if err != nil {
					return "", nil, fmt.Errorf("浏览结构失败：%w", err)
				}
				orgs := toOrgs(v)
				var sb strings.Builder
				if len(orgs) == 0 {
					return "当前没有任何组织或仓库。", map[string]interface{}{"orgs": []interface{}{}}, nil
				}
				for _, o := range orgs {
					fmt.Fprintf(&sb, "组织 '%s'（%d 个仓库）：\n", o.org, len(o.repos))
					for _, r := range o.repos {
						fmt.Fprintf(&sb, "  - %s（分支：%s）\n", r.repo, strings.Join(r.branches, ", "))
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
				return sb.String(), map[string]interface{}{"orgs": meta}, nil
			},
		},
		"git-diff": {
			Description: "比较两个版本之间的文件差异。",
			InputSchema: schema(map[string]interface{}{"rev_a": str("string"), "rev_b": str("string"), "path": str("string")}, "rev_a", "rev_b", "path"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, map[string]interface{}, error) {
				o, r, _, err := s.sessionBase(ctx, args)
				if err != nil {
					return "", nil, err
				}
				revA := extensionsdk.ArgString(args, "rev_a")
				revB := extensionsdk.ArgString(args, "rev_b")
				path := extensionsdk.ArgString(args, "path")
				q := url.Values{"rev_a": {revA}, "rev_b": {revB}, "path": {path}}
				v, err := get(ctx, s.base+"/api/v1/git-diff/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"?"+q.Encode())
				if err != nil {
					return "", nil, fmt.Errorf("获取差异失败：%w", err)
				}
				diff := strVal(v, "diff")
				if strings.TrimSpace(diff) == "" {
					return fmt.Sprintf("文件 '%s' 在 '%s' 和 '%s' 之间没有差异。", path, revA, revB), map[string]interface{}{"path": path, "rev_a": revA, "rev_b": revB}, nil
				}
				return fmt.Sprintf("文件 '%s' 在 '%s'..'%s' 的差异：\n%s", path, revA, revB, diff), map[string]interface{}{"path": path, "rev_a": revA, "rev_b": revB}, nil
			},
		},
		"git-blame": {
			Description: "标注文件每一行是由哪个变更引入的。",
			InputSchema: schema(map[string]interface{}{"rev": str("string"), "path": str("string")}, "rev", "path"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, map[string]interface{}, error) {
				o, r, _, err := s.sessionBase(ctx, args)
				if err != nil {
					return "", nil, err
				}
				rev := extensionsdk.ArgString(args, "rev")
				path := extensionsdk.ArgString(args, "path")
				q := url.Values{"rev": {rev}, "path": {path}}
				v, err := get(ctx, s.base+"/api/v1/git-blame/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"?"+q.Encode())
				if err != nil {
					return "", nil, fmt.Errorf("获取 blame 失败：%w", err)
				}
				lines := strSlice(v, "blame")
				var sb strings.Builder
				fmt.Fprintf(&sb, "文件 '%s' 的逐行来源：\n", path)
				meta := make([]interface{}, 0, len(lines))
				for _, line := range lines {
					commit, text := splitBlame(line)
					fmt.Fprintf(&sb, "  %s: %s\n", shortID(commit), text)
					meta = append(meta, map[string]interface{}{"commit_id": commit, "content": text})
				}
				return sb.String(), map[string]interface{}{"lines": meta}, nil
			},
		},
		"git-log": {
			Description: "查看提交历史。",
			InputSchema: schema(map[string]interface{}{"limit": intProp()}),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, map[string]interface{}, error) {
				o, r, _, err := s.sessionBase(ctx, args)
				if err != nil {
					return "", nil, err
				}
				limit := extensionsdk.ArgInt(args, "limit", 50)
				q := url.Values{"limit": {fmt.Sprintf("%d", limit)}}
				v, err := get(ctx, s.base+"/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/log?"+q.Encode())
				if err != nil {
					return "", nil, fmt.Errorf("获取提交历史失败：%w", err)
				}
				commits := toCommits(v)
				var sb strings.Builder
				if len(commits) == 0 {
					return "当前没有提交记录。", map[string]interface{}{"commits": []interface{}{}}, nil
				}
				fmt.Fprintf(&sb, "最近的 %d 条提交：\n", len(commits))
				meta := make([]interface{}, 0, len(commits))
				for _, c := range commits {
					msg := c.message
					if msg == "" {
						msg = "(无描述)"
					}
					fmt.Fprintf(&sb, "  %s %s（%s）\n", shortID(c.changeID), msg, c.author)
					meta = append(meta, map[string]interface{}{"change_id": c.changeID, "message": c.message, "author": c.author, "timestamp": c.timestamp})
				}
				return sb.String(), map[string]interface{}{"commits": meta}, nil
			},
		},
		"git-show": {
			Description: "查看某个变更改动了什么。",
			InputSchema: schema(map[string]interface{}{"rev": str("string")}, "rev"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, map[string]interface{}, error) {
				o, r, _, err := s.sessionBase(ctx, args)
				if err != nil {
					return "", nil, err
				}
				rev := extensionsdk.ArgString(args, "rev")
				v, err := get(ctx, s.base+"/api/v1/git-show/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/"+url.PathEscape(rev))
				if err != nil {
					return "", nil, fmt.Errorf("查看变更失败：%w", err)
				}
				patch := strVal(v, "patch")
				if strings.TrimSpace(patch) == "" {
					return fmt.Sprintf("变更 '%s' 没有内容差异。", rev), map[string]interface{}{"rev": rev}, nil
				}
				return fmt.Sprintf("变更 '%s' 的改动：\n%s", rev, patch), map[string]interface{}{"rev": rev, "patch": patch}, nil
			},
		},
		"git-branches": {
			Description: "列出所有分支（bookmarks）。",
			InputSchema: schema(map[string]interface{}{}),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, map[string]interface{}, error) {
				o, r, _, err := s.sessionBase(ctx, args)
				if err != nil {
					return "", nil, err
				}
				v, err := get(ctx, s.base+"/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/bookmarks")
				if err != nil {
					return "", nil, fmt.Errorf("列出分支失败：%w", err)
				}
				branches := toBranches(v)
				var sb strings.Builder
				if len(branches) == 0 {
					return "当前没有分支。", map[string]interface{}{"branches": []interface{}{}}, nil
				}
				fmt.Fprintf(&sb, "分支列表（%d 个）：\n", len(branches))
				meta := make([]interface{}, 0, len(branches))
				for _, br := range branches {
					fmt.Fprintf(&sb, "  %s（%s）\n", br.branch, shortID(br.target))
					meta = append(meta, map[string]interface{}{"branch": br.branch, "target": br.target})
				}
				return sb.String(), map[string]interface{}{"branches": meta}, nil
			},
		},
	}
}

// sessionBase resolves the (org, repo, bookmark) triple for a tool call.
// Priority: `_session` (agent-injected session name, resolved via the
// mapping table with lazy adoption) → legacy `_org`/`_repo`/`_branch` args.
func (s *server) sessionBase(ctx context.Context, args map[string]interface{}) (string, string, string, error) {
	if sid := extensionsdk.ArgString(args, "_session"); sid != "" {
		return s.resolveSession(ctx, sid)
	}
	o, r, b := extensionsdk.ArgString(args, "_org"), extensionsdk.ArgString(args, "_repo"), extensionsdk.ArgString(args, "_branch")
	if o == "" || r == "" {
		return "", "", "", fmt.Errorf("缺少会话上下文（_session 或 _org/_repo）")
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
		return "", fmt.Errorf("start_line 必须 >= 1")
	}
	if startLine > int64(total)+1 {
		return "", fmt.Errorf("start_line=%d 超出范围（文件共 %d 行）", startLine, total)
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
		return "", fmt.Errorf("end_line=%d 超出范围（文件共 %d 行）", endLine, total)
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
