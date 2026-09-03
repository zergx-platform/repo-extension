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

// agentClient talks to the agent-ts session REST API. The workspace layer
// only ever READS session state (lineage archaeology) plus one write —
// creating a session when adopting an orphan bookmark. Session lifecycle
// (create/fork/rename/delete) belongs to callers of the agent; workspaces
// follow lazily. Conflict (409) responses mean "already exists" and are
// treated as idempotent success.
type agentClient struct {
	base string
	hc   *http.Client
}

func newAgentClient(base string) *agentClient {
	return &agentClient{base: base, hc: &http.Client{Timeout: 30 * time.Second}}
}

func (c *agentClient) call(ctx context.Context, method, path string, body interface{}) (int, map[string]interface{}, error) {
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

func sessionsPath(name string) string {
	return "/api/v1/sessions/" + url.PathEscape(name)
}

// EnsureSession creates the session; 409 (already exists) is success.
func (c *agentClient) EnsureSession(ctx context.Context, name string) error {
	status, v, err := c.call(ctx, http.MethodPost, "/api/v1/sessions", map[string]interface{}{"name": name})
	if err != nil {
		return errDownstream("agent", err)
	}
	switch status {
	case 200, 409:
		return nil
	default:
		return errDownstream("agent", fmt.Errorf("create session: HTTP %d %s", status, errText(v)))
	}
}

// ListSessions returns every session name.
func (c *agentClient) ListSessions(ctx context.Context) (map[string]bool, error) {
	status, v, err := c.call(ctx, http.MethodGet, "/api/v1/sessions", nil)
	if err != nil {
		return nil, errDownstream("agent", err)
	}
	if status != 200 {
		return nil, errDownstream("agent", fmt.Errorf("list sessions: HTTP %d", status))
	}
	out := map[string]bool{}
	if list, ok := v["sessions"].([]interface{}); ok {
		for _, e := range list {
			if m, ok := e.(map[string]interface{}); ok {
				if n, _ := m["name"].(string); n != "" {
					out[n] = true
				}
			}
		}
	}
	return out, nil
}

// GetSession returns the session row (nil when absent); the tip_id field
// feeds fork-point pinning.
func (c *agentClient) GetSession(ctx context.Context, name string) (map[string]interface{}, error) {
	status, v, err := c.call(ctx, http.MethodGet, sessionsPath(name), nil)
	if err != nil {
		return nil, errDownstream("agent", err)
	}
	switch status {
	case 200:
		if s, ok := v["session"].(map[string]interface{}); ok {
			return s, nil
		}
		return nil, errDownstream("agent", fmt.Errorf("get session: malformed response"))
	case 404:
		return nil, nil
	default:
		return nil, errDownstream("agent", fmt.Errorf("get session: HTTP %d %s", status, errText(v)))
	}
}

// ForkSession forks parentSID into a new session named `name`. The optional
// messageID pins the fork to a specific message on the parent's chain (empty
// forks from the current tip). 409 (already exists) is idempotent success.
func (c *agentClient) ForkSession(ctx context.Context, parentSID, name, messageID string) error {
	body := map[string]interface{}{"name": name}
	if messageID != "" {
		body["message_id"] = messageID
	}
	status, v, err := c.call(ctx, http.MethodPost, sessionsPath(parentSID)+"/fork", body)
	if err != nil {
		return errDownstream("agent", err)
	}
	switch status {
	case 200, 409:
		return nil
	default:
		return errDownstream("agent", fmt.Errorf("fork session: HTTP %d %s", status, errText(v)))
	}
}

// DeleteSession removes the session (which also fires the lifecycle chain).
// 404 is idempotent success.
func (c *agentClient) DeleteSession(ctx context.Context, name string) error {
	status, v, err := c.call(ctx, http.MethodDelete, sessionsPath(name), nil)
	if err != nil {
		return errDownstream("agent", err)
	}
	switch status {
	case 200, 404:
		return nil
	default:
		return errDownstream("agent", fmt.Errorf("delete session: HTTP %d %s", status, errText(v)))
	}
}
