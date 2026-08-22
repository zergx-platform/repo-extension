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
func (s *server) tools() map[string]extensionsdk.ToolSpec {
	schema := func(props map[string]interface{}, req ...string) map[string]interface{} {
		return map[string]interface{}{"type": "object", "properties": props, "required": req}
	}
	str := func(t string) map[string]interface{} { return map[string]interface{}{"type": t} }
	intProp := func() map[string]interface{} { return map[string]interface{}{"type": "integer"} }

	// contentsPath builds /api/v1/repos/{org}/{repo}/{branch}/contents/{path}.
	contentsPath := func(o, r, b, path string) string {
		return s.base + "/api/v1/repos/" + url.PathEscape(o) + "/" + url.PathEscape(r) + "/" +
			url.PathEscape(b) + "/contents/" + escPath(path)
	}

	return map[string]extensionsdk.ToolSpec{
		"read": {
			Description: "Read a file from the current repo, returning its content (UTF-8).",
			InputSchema: schema(map[string]interface{}{"path": str("string")}, "path"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, error) {
				o, r, b := ctxBase(args)
				path := strArg(args, "path")
				if path == "" {
					return "", fmt.Errorf("read: missing 'path'")
				}
				v, err := get(ctx, contentsPath(o, r, b, path))
				if err != nil {
					return "", fmt.Errorf("read failed: %w", err)
				}
				content, ok := v["content"].(string)
				if !ok {
					return "", fmt.Errorf("read: unexpected response")
				}
				if enc, _ := v["encoding"].(string); enc == "base64" {
					raw, err := base64.StdEncoding.DecodeString(content)
					if err != nil {
						return "", fmt.Errorf("read: bad base64: %w", err)
					}
					return string(raw), nil
				}
				return content, nil
			},
		},
		"write": {
			Description: "Write (create or overwrite) a file in the current repo.",
			InputSchema: schema(map[string]interface{}{"path": str("string"), "content": str("string"), "message": str("string"), "sha": str("string")}, "path", "content"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, error) {
				o, r, b := ctxBase(args)
				path := strArg(args, "path")
				if path == "" {
					return "", fmt.Errorf("write: missing 'path'")
				}
				content := strArg(args, "content")
				message := strArg(args, "message")
				if message == "" {
					message = "write " + path
				}
				body := map[string]interface{}{
					"content": base64.StdEncoding.EncodeToString([]byte(content)),
					"message": message,
				}
				if sha := strArg(args, "sha"); sha != "" {
					body["sha"] = sha
				}
				v, err := put(ctx, contentsPath(o, r, b, path), body)
				if err != nil {
					return "", fmt.Errorf("write failed: %w", err)
				}
				if cid := strVal(v, "commit_id"); cid != "" {
					return fmt.Sprintf("Wrote file '%s'. (commit_id=%s)", path, cid), nil
				}
				return fmt.Sprintf("Wrote file '%s'.", path), nil
			},
		},
		"delete": {
			Description: "Delete a file from the current repo.",
			InputSchema: schema(map[string]interface{}{"path": str("string"), "message": str("string")}, "path"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, error) {
				o, r, b := ctxBase(args)
				path := strArg(args, "path")
				if path == "" {
					return "", fmt.Errorf("delete: missing 'path'")
				}
				message := strArg(args, "message")
				if message == "" {
					message = "delete " + path
				}
				_, err := delBody(ctx, contentsPath(o, r, b, path), map[string]interface{}{"message": message})
				if err != nil {
					return "", fmt.Errorf("delete failed: %w", err)
				}
				return fmt.Sprintf("Deleted file '%s'.", path), nil
			},
		},
		"ls": {
			Description: "List files in the current repo as a tree.",
			InputSchema: schema(map[string]interface{}{}),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, error) {
				o, r, b := ctxBase(args)
				v, err := get(ctx, s.base+"/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/"+url.PathEscape(b)+"/tree")
				if err != nil {
					return "", fmt.Errorf("ls failed: %w", err)
				}
				return joinTreeSlice(v), nil
			},
		},
		"grep": {
			Description: "Search file contents using a regular expression.",
			InputSchema: schema(map[string]interface{}{"pattern": str("string")}, "pattern"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, error) {
				o, r, b := ctxBase(args)
				pattern := strArg(args, "pattern")
				if pattern == "" {
					return "", fmt.Errorf("grep: missing 'pattern'")
				}
				q := url.Values{"pattern": {pattern}}
				v, err := get(ctx, s.base+"/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/"+url.PathEscape(b)+"/search?"+q.Encode())
				if err != nil {
					return "", fmt.Errorf("grep failed: %w", err)
				}
				return joinStrSlice(v, "matches", "\n"), nil
			},
		},
		"explore": {
			Description: "Explore the ReCoder project structure (orgs/repos/bookmarks).",
			InputSchema: schema(map[string]interface{}{"org": str("string"), "repo": str("string")}),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, error) {
				v, err := get(ctx, s.base+"/api/v1/repos")
				if err != nil {
					return "", fmt.Errorf("explore failed: %w", err)
				}
				return pretty(v["orgs"]), nil
			},
		},
		"git-diff": {
			Description: "Diff a file between two revisions.",
			InputSchema: schema(map[string]interface{}{"rev_a": str("string"), "rev_b": str("string"), "path": str("string")}, "rev_a", "rev_b", "path"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, error) {
				o, r, _ := ctxBase(args)
				q := url.Values{"rev_a": {strArg(args, "rev_a")}, "rev_b": {strArg(args, "rev_b")}, "path": {strArg(args, "path")}}
				v, err := get(ctx, s.base+"/api/v1/git-diff/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"?"+q.Encode())
				if err != nil {
					return "", fmt.Errorf("git-diff failed: %w", err)
				}
				return strVal(v, "diff"), nil
			},
		},
		"git-blame": {
			Description: "Annotate each line of a file with its introducing commit.",
			InputSchema: schema(map[string]interface{}{"rev": str("string"), "path": str("string")}, "rev", "path"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, error) {
				o, r, _ := ctxBase(args)
				q := url.Values{"rev": {strArg(args, "rev")}, "path": {strArg(args, "path")}}
				v, err := get(ctx, s.base+"/api/v1/git-blame/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"?"+q.Encode())
				if err != nil {
					return "", fmt.Errorf("git-blame failed: %w", err)
				}
				return joinStrSlice(v, "blame", "\n"), nil
			},
		},
		"git-log": {
			Description: "Query commit history.",
			InputSchema: schema(map[string]interface{}{"limit": intProp()}),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, error) {
				o, r, _ := ctxBase(args)
				q := url.Values{"limit": {fmt.Sprintf("%d", intArg(args, "limit", 50))}}
				v, err := get(ctx, s.base+"/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/log?"+q.Encode())
				if err != nil {
					return "", fmt.Errorf("git-log failed: %w", err)
				}
				return pretty(v["commits"]), nil
			},
		},
		"git-show": {
			Description: "Show what a commit changed.",
			InputSchema: schema(map[string]interface{}{"rev": str("string")}, "rev"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, error) {
				o, r, _ := ctxBase(args)
				v, err := get(ctx, s.base+"/api/v1/git-show/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/"+url.PathEscape(strArg(args, "rev")))
				if err != nil {
					return "", fmt.Errorf("git-show failed: %w", err)
				}
				return strVal(v, "patch"), nil
			},
		},
		"git-branches": {
			Description: "List all bookmarks (branches).",
			InputSchema: schema(map[string]interface{}{}),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, error) {
				o, r, _ := ctxBase(args)
				v, err := get(ctx, s.base+"/api/v1/repos/"+url.PathEscape(o)+"/"+url.PathEscape(r)+"/bookmarks")
				if err != nil {
					return "", fmt.Errorf("git-branches failed: %w", err)
				}
				return pretty(v["bookmarks"]), nil
			},
		},
	}
}

// ctxBase returns the raw org/repo/branch from args (not escaped; escaping is
// done at path construction).
func ctxBase(args map[string]interface{}) (o, r, b string) {
	return strArg(args, "_org"), strArg(args, "_repo"), strArg(args, "_branch")
}

// escPath escapes a file path for a URL path segment, keeping slashes.
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

func joinStrSlice(v map[string]interface{}, k, sep string) string {
	if arr, ok := v[k].([]interface{}); ok {
		out := make([]string, 0, len(arr))
		for _, e := range arr {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return strings.Join(out, sep)
	}
	return ""
}

// joinTreeSlice renders the /tree response as one path per line.
func joinTreeSlice(v map[string]interface{}) string {
	if entries, ok := v["tree"].([]interface{}); ok {
		out := make([]string, 0, len(entries))
		for _, e := range entries {
			if m, ok := e.(map[string]interface{}); ok {
				if s, ok := m["path"].(string); ok {
					prefix := ""
					if typ, _ := m["type"].(string); typ == "tree" {
						prefix = "d "
					}
					out = append(out, prefix+s)
				}
			}
		}
		return strings.Join(out, "\n")
	}
	return ""
}

func pretty(v interface{}) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
