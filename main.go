package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	extensionsdk "forgejo.develop.10.199.64.20.nip.io/rucoder/extension-sdk-go"
)

// server holds the repo-manager HTTP base for forwarding.
type server struct {
	base string
}

// esc mirrors the original `esc` closure (slash/colon → underscore).
func esc(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '/', ':':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

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
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return nil, err
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

func qurl(base string, path string, q url.Values) string {
	return base + path + "?" + q.Encode()
}

func main() {
	s := &server{base: envOr("RUCODER_REPO_MANAGER_URL", "http://rucoder-repo-manager.develop.svc.cluster.local:80")}
	ext, err := extensionsdk.Register(extensionsdk.Config{
		ID:      "repo-extension",
		Version: "0.1.0",
		NATSURL: envOr("NATS_URL", "nats://nats.develop.svc.cluster.local:4222"),
		Tools:   s.tools(),
	})
	if err != nil {
		panic(err)
	}
	defer ext.Close()
	println("[repo-extension] registered")
	select {}
}

func envOr(k, d string) string {
	if v := getenv(k); v != "" {
		return v
	}
	return d
}
