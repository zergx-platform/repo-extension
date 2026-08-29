package main

import (
	"context"
	"errors"
	"forgejo.develop.10.199.64.20.nip.io/zergx/go-shared/httpx"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDoJSONNotFoundMapsToErrNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no such repo"}`))
	}))
	defer srv.Close()

	if _, err := httpx.Get(context.Background(), srv.URL); !errors.Is(err, httpx.ErrNotFound) {
		t.Fatalf("get 404: err = %v, want httpx.ErrNotFound", err)
	}
	if _, err := httpx.Delete(context.Background(), srv.URL, map[string]interface{}{"message": "x"}); !errors.Is(err, httpx.ErrNotFound) {
		t.Fatalf("delBody 404: err = %v, want httpx.ErrNotFound", err)
	}
}

func TestDoJSONServerErrorIncludesStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	v, err := httpx.Get(context.Background(), srv.URL)
	if err == nil {
		t.Fatalf("get 500: err = nil, v = %v; want error", v)
	}
	if !strings.Contains(err.Error(), "HTTP 500") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("get 500: err = %q, want status and body snippet", err)
	}
}

func TestDoJSONMalformedBodyIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer srv.Close()

	if _, err := httpx.Get(context.Background(), srv.URL); err == nil {
		t.Fatal("get non-json 200: err = nil, want decode error")
	}
}

func TestDoJSONEmptyBodyOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	v, err := httpx.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("get empty 200: err = %v", err)
	}
	if len(v) != 0 {
		t.Fatalf("get empty 200: v = %v, want empty map", v)
	}
}

// TestReadTool404Vs500 pins the read tool error semantics: 404 stays a
// friendly success-style message, any other backend failure surfaces as a
// real tool error (so an outage is never mistaken for a missing file).
func TestReadTool404Vs500(t *testing.T) {
	mode := "404"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/o/r/main/contents/f.txt", func(w http.ResponseWriter, _ *http.Request) {
		switch mode {
		case "500":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"db down"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no such file"}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := &server{base: srv.URL}
	h := s.handlers()["read"]
	exec := func() (string, error) {
		res, err := h.Execute(context.Background(),
			map[string]interface{}{"path": "f.txt", "_org": "o", "_repo": "r", "_branch": "main"},
			"", "")
		return res.Content, err
	}

	out, err := exec()
	if err != nil {
		t.Fatalf("read 404: err = %v, want nil", err)
	}
	if !strings.Contains(out, "not found or inaccessible") {
		t.Fatalf("read 404: out = %q", out)
	}

	mode = "500"
	_, err = exec()
	if err == nil {
		t.Fatal("read 500: err = nil, want real error")
	}
}
