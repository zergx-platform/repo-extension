// Package httpx is the shared JSON-over-HTTP client used by services that
// talk to internal APIs (jjlab, agent, …). It centralizes the contract
// every caller must rely on:
//
//   - one shared client with a bounded timeout (a hung upstream can never
//     stall a tool call indefinitely);
//   - HTTP 404 maps to ErrNotFound so handlers can branch on it;
//   - any other >= 400 status, transport error or malformed body returns a
//     real error — it is never silently decoded into an empty map.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrNotFound maps HTTP 404 responses.
var ErrNotFound = errors.New("not found")

// errBodyLimit caps how much of an error response body is inlined into the
// returned error message.
const errBodyLimit = 2048

// Clients shared by all users of this package.
var (
	// Default is for regular JSON API calls.
	Default = &http.Client{Timeout: 60 * time.Second}
	// Long is for streaming/large payloads (archives, syncs).
	Long = &http.Client{Timeout: 15 * time.Minute}
)

// Get issues GET and decodes a JSON object response.
func Get(ctx context.Context, url string) (map[string]any, error) {
	return DoJSON(ctx, http.MethodGet, url, nil, Default)
}

// Post issues POST with a JSON body and decodes a JSON object response.
func Post(ctx context.Context, url string, body any) (map[string]any, error) {
	return DoJSON(ctx, http.MethodPost, url, body, Default)
}

// Put issues PUT with a JSON body and decodes a JSON object response.
func Put(ctx context.Context, url string, body any) (map[string]any, error) {
	return DoJSON(ctx, http.MethodPut, url, body, Default)
}

// Delete issues DELETE with an optional JSON body and decodes a JSON object
// response.
func Delete(ctx context.Context, url string, body any) (map[string]any, error) {
	return DoJSON(ctx, http.MethodDelete, url, body, Default)
}

// DoJSON performs one JSON request/response round trip on the given client.
func DoJSON(ctx context.Context, method, url string, body any, client *http.Client) (map[string]any, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s %s: %w", method, url, ErrNotFound)
	}
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", method, url, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	var v map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s %s: decode response: %w", method, url, err)
	}
	return v, nil
}
