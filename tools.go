package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	extensionsdk "forgejo.develop.10.199.64.20.nip.io/rucoder/extension-sdk-go"
)

// tools returns the 14 repo tools, forwarding each to repo-manager HTTP.
func (s *server) tools() map[string]extensionsdk.ToolSpec {
	ctxBase := func(args map[string]interface{}) (o, r, b string) {
		return esc(strArg(args, "_org")), esc(strArg(args, "_repo")), esc(strArg(args, "_branch"))
	}
	schema := func(props map[string]interface{}, req ...string) map[string]interface{} {
		return map[string]interface{}{"type": "object", "properties": props, "required": req}
	}
	str := func(t string) map[string]interface{} { return map[string]interface{}{"type": t} }
	intProp := func() map[string]interface{} { return map[string]interface{}{"type": "integer"} }

	return map[string]extensionsdk.ToolSpec{
		"read": {
			Description: "Read a file from the current repo, returning its content.",
			InputSchema: schema(map[string]interface{}{"path": str("string"), "offset": intProp(), "limit": intProp()}, "path"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, error) {
				o, r, b := ctxBase(args)
				path := strArg(args, "path")
				if path == "" {
					return "", fmt.Errorf("read: missing 'path'")
				}
				q := url.Values{"org": {o}, "repo": {r}, "branch": {b}, "path": {esc(path)}}
				v, err := get(ctx, qurl(s.base, "/api/v1/fs/read", q))
				if err != nil {
					return "", fmt.Errorf("read failed: %w", err)
				}
				return strVal(v, "content"), nil
			},
		},
		"write": {
			Description: "Write (create or overwrite) a file in the current repo.",
			InputSchema: schema(map[string]interface{}{"path": str("string"), "content": str("string")}, "path", "content"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, error) {
				o, r, b := ctxBase(args)
				path := strArg(args, "path")
				if path == "" {
					return "", fmt.Errorf("write: missing 'path'")
				}
				v, err := put(ctx, s.base+"/api/v1/fs/write", map[string]interface{}{
					"org": strArg(args, "_org"), "repo": strArg(args, "_repo"), "branch": strArg(args, "_branch"),
					"path": path, "content": strArg(args, "content"),
				})
				if err != nil {
					return "", fmt.Errorf("write failed: %w", err)
				}
				_ = o
				_ = r
				_ = b
				cid := strVal(v, "change_id")
				if cid != "" {
					return fmt.Sprintf("Wrote file '%s'. (change_id=%s)", path, cid), nil
				}
				return fmt.Sprintf("Wrote file '%s'.", path), nil
			},
		},
		"edit": {
			Description: "Replace or insert lines in a file by line numbers.",
			InputSchema: schema(map[string]interface{}{"path": str("string"), "start_line": intProp(), "end_line": intProp(), "content": str("string")}, "path", "start_line", "end_line", "content"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, error) {
				path := strArg(args, "path")
				if path == "" {
					return "", fmt.Errorf("edit: missing 'path'")
				}
				_, err := post(ctx, s.base+"/api/v1/fs/edit", map[string]interface{}{
					"org": strArg(args, "_org"), "repo": strArg(args, "_repo"), "branch": strArg(args, "_branch"),
					"path": path, "start_line": intArg(args, "start_line", 0), "end_line": intArg(args, "end_line", 0), "content": strArg(args, "content"),
				})
				if err != nil {
					return "", fmt.Errorf("edit failed: %w", err)
				}
				return fmt.Sprintf("Edited file '%s'.", path), nil
			},
		},
		"delete": {
			Description: "Delete a file from the current repo.",
			InputSchema: schema(map[string]interface{}{"path": str("string")}, "path"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, error) {
				o, r, b := ctxBase(args)
				path := strArg(args, "path")
				if path == "" {
					return "", fmt.Errorf("delete: missing 'path'")
				}
				q := url.Values{"org": {o}, "repo": {r}, "branch": {b}, "path": {esc(path)}}
				_, err := del(ctx, qurl(s.base, "/api/v1/fs/delete", q))
				if err != nil {
					return "", fmt.Errorf("delete failed: %w", err)
				}
				return fmt.Sprintf("Deleted file '%s'.", path), nil
			},
		},
		"grep": {
			Description: "Search file contents using a regular expression.",
			InputSchema: schema(map[string]interface{}{"pattern": str("string"), "path": str("string"), "limit": intProp()}, "pattern"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, error) {
				o, r, b := ctxBase(args)
				pattern := strArg(args, "pattern")
				if pattern == "" {
					return "", fmt.Errorf("grep: missing 'pattern'")
				}
				q := url.Values{"org": {o}, "repo": {r}, "branch": {b}, "pattern": {esc(pattern)}, "limit": {fmt.Sprintf("%d", intArg(args, "limit", 200))}}
				if sub := strArg(args, "path"); sub != "" {
					q.Set("path", esc(sub))
				}
				v, err := get(ctx, qurl(s.base, "/api/v1/fs/grep", q))
				if err != nil {
					return "", fmt.Errorf("grep failed: %w", err)
				}
				return joinStrSlice(v, "matches", "\n"), nil
			},
		},
		"glob": {
			Description: "Find files by glob pattern (e.g. **/*.rs).",
			InputSchema: schema(map[string]interface{}{"pattern": str("string"), "limit": intProp()}, "pattern"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, error) {
				o, r, b := ctxBase(args)
				pattern := strArg(args, "pattern")
				if pattern == "" {
					return "", fmt.Errorf("glob: missing 'pattern'")
				}
				q := url.Values{"org": {o}, "repo": {r}, "branch": {b}, "pattern": {esc(pattern)}, "limit": {fmt.Sprintf("%d", intArg(args, "limit", 200))}}
				v, err := get(ctx, qurl(s.base, "/api/v1/fs/glob", q))
				if err != nil {
					return "", fmt.Errorf("glob failed: %w", err)
				}
				return joinStrSlice(v, "files", "\n"), nil
			},
		},
		"ls": {
			Description: "List files in the current repo as a tree.",
			InputSchema: schema(map[string]interface{}{"path": str("string"), "depth": intProp()}),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, error) {
				o, r, b := ctxBase(args)
				q := url.Values{"org": {o}, "repo": {r}, "branch": {b}, "depth": {fmt.Sprintf("%d", intArg(args, "depth", 5))}}
				if path := strArg(args, "path"); path != "" {
					q.Set("path", esc(path))
				}
				v, err := get(ctx, qurl(s.base, "/api/v1/fs/list", q))
				if err != nil {
					return "", fmt.Errorf("ls failed: %w", err)
				}
				return joinPathSlice(v), nil
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
				q := url.Values{"rev_a": {strArg(args, "rev_a")}, "rev_b": {strArg(args, "rev_b")}, "path": {esc(strArg(args, "path"))}}
				v, err := get(ctx, qurl(s.base, "/api/v1/git-diff/"+o+"/"+r, q))
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
				q := url.Values{"rev": {strArg(args, "rev")}, "path": {esc(strArg(args, "path"))}}
				v, err := get(ctx, qurl(s.base, "/api/v1/git-blame/"+o+"/"+r, q))
				if err != nil {
					return "", fmt.Errorf("git-blame failed: %w", err)
				}
				return joinStrSlice(v, "blame", "\n"), nil
			},
		},
		"git-log": {
			Description: "Query commit history using a jj revset.",
			InputSchema: schema(map[string]interface{}{"revset": str("string"), "limit": intProp()}),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, error) {
				o, r, _ := ctxBase(args)
				q := url.Values{"limit": {fmt.Sprintf("%d", intArg(args, "limit", 50))}}
				v, err := get(ctx, qurl(s.base, "/api/v1/repos/"+o+"/"+r+"/log", q))
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
				v, err := get(ctx, s.base+"/api/v1/git-show/"+o+"/"+r+"/"+strArg(args, "rev"))
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
				v, err := get(ctx, s.base+"/api/v1/repos/"+o+"/"+r+"/bookmarks")
				if err != nil {
					return "", fmt.Errorf("git-branches failed: %w", err)
				}
				return pretty(v["bookmarks"]), nil
			},
		},
		"git-restore": {
			Description: "Restore a single file to a previous revision.",
			InputSchema: schema(map[string]interface{}{"rev": str("string"), "path": str("string")}, "rev", "path"),
			Execute: func(ctx context.Context, args map[string]interface{}, callID string) (string, error) {
				o, r, b := ctxBase(args)
				q := url.Values{"org": {o}, "repo": {r}, "branch": {b}, "rev": {strArg(args, "rev")}, "path": {esc(strArg(args, "path"))}}
				_, err := get(ctx, qurl(s.base, "/api/v1/git-restore", q))
				if err != nil {
					return "", fmt.Errorf("git-restore failed: %w", err)
				}
				return "Restored file.", nil
			},
		},
	}
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
		return join(out, sep)
	}
	return ""
}

func joinPathSlice(v map[string]interface{}) string {
	if entries, ok := v["entries"].([]interface{}); ok {
		out := make([]string, 0, len(entries))
		for _, e := range entries {
			if m, ok := e.(map[string]interface{}); ok {
				if s, ok := m["path"].(string); ok {
					out = append(out, s)
				}
			}
		}
		return join(out, "\n")
	}
	return ""
}

func join(a []string, sep string) string {
	r := ""
	for i, s := range a {
		if i > 0 {
			r += sep
		}
		r += s
	}
	return r
}

func pretty(v interface{}) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
