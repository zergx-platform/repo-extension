package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// Raw JSON-over-HTTP helpers used by the tool forwarding layer (tools.go).

func strArg(args map[string]interface{}, k string) string {
	if v, ok := args[k].(string); ok {
		return v
	}
	return ""
}

func intArg(args map[string]interface{}, k string, def int64) int64 {
	if v, ok := args[k].(float64); ok {
		return int64(v)
	}
	return def
}

func get(ctx context.Context, url string) (map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var v map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func post(ctx context.Context, url string, body interface{}) (map[string]interface{}, error) {
	return send(ctx, http.MethodPost, url, body)
}

func put(ctx context.Context, url string, body interface{}) (map[string]interface{}, error) {
	return send(ctx, http.MethodPut, url, body)
}

func send(ctx context.Context, method, url string, body interface{}) (map[string]interface{}, error) {
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var v map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func del(ctx context.Context, url string) (map[string]interface{}, error) {
	return delBody(ctx, url, nil)
}

// delBody sends a DELETE request with an optional JSON body (Contents API).
func delBody(ctx context.Context, url string, body interface{}) (map[string]interface{}, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var v map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return v, nil
}
