package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	abep "abep.dev/sdk"
)

// ---- manifest binding ----

// TestManifestBinding asserts the YAML manifest is the single source of
// truth: every declared tool has a bound handler, every handler is declared,
// and descriptions/schemas/variables/lifecycle are carried onto the config.
func TestManifestBinding(t *testing.T) {
	m, err := abep.ParseManifest(manifestYaml)
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if m.ID != "repo" {
		t.Fatalf("manifest id = %q", m.ID)
	}
	if len(m.Tools) != 13 {
		t.Fatalf("manifest tools = %d, want 13", len(m.Tools))
	}
	if len(m.Variables) != 3 {
		t.Fatalf("manifest variables = %d, want 3", len(m.Variables))
	}
	if len(m.Lifecycle) != 4 {
		t.Fatalf("manifest lifecycle = %v, want 4 kinds", m.Lifecycle)
	}

	cfg := m.Config(manifestHandlers(), map[string]abep.VariableSpec{
		"org":      {Resolve: func(context.Context, string) (string, error) { return "acme", nil }},
		"repo":     {Resolve: func(context.Context, string) (string, error) { return "api", nil }},
		"bookmark": {Resolve: func(context.Context, string) (string, error) { return "main", nil }},
	}, nil)

	declared := map[string]bool{}
	for _, tool := range m.Tools {
		if strings.TrimSpace(tool.Description) == "" {
			t.Fatalf("tool %q has empty description", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Fatalf("tool %q has no input_schema", tool.Name)
		}
		declared[tool.Name] = true
		if _, ok := cfg.Tools[tool.Name]; !ok {
			t.Fatalf("tool %q declared but no handler bound", tool.Name)
		}
	}
	for name := range manifestHandlers() {
		if !declared[name] {
			t.Fatalf("handler %q bound but not declared in manifest", name)
		}
	}
	for _, v := range m.Variables {
		if v.Scope != "session" {
			t.Fatalf("variable %q scope = %q, want session", v.Name, v.Scope)
		}
		if _, ok := cfg.Variables[v.Name]; !ok {
			t.Fatalf("variable %q declared but not bound", v.Name)
		}
	}
}

// manifestHandlers returns the handler map without needing a live server.
func manifestHandlers() map[string]abep.ToolSpec {
	return (&server{}).handlers()
}

// ---- wire-level e2e over the inproc transport ----

// startRepoExt serves the extension on the hub and waits for setup.
func startRepoExt(t *testing.T, hub *abep.InprocHub, cfg abep.Config) (*abep.Agent, *abep.Extension) {
	t.Helper()
	ext := abep.NewExtension(abep.NewInprocBus(hub), cfg)
	agent := abep.NewAgent(abep.NewInprocBus(hub))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = ext.Serve(ctx) }()
	time.Sleep(100 * time.Millisecond)
	return agent, ext
}

func TestDiscoverWire(t *testing.T) {
	hub := abep.NewInprocHub()
	m, err := abep.ParseManifest(manifestYaml)
	if err != nil {
		t.Fatal(err)
	}
	agent, ext := startRepoExt(t, hub, m.Config(manifestHandlers(), nil, nil))
	defer ext.Close()
	defer agent.Close()

	manifests, err := agent.Discover(context.Background(), 500)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(manifests) != 1 || manifests[0].ID != "repo" {
		t.Fatalf("discover = %+v", manifests)
	}
	if len(manifests[0].Tools) != 13 {
		t.Fatalf("discover tools = %d, want 13", len(manifests[0].Tools))
	}
	cap := map[string]bool{}
	for _, c := range manifests[0].Capabilities {
		cap[c] = true
	}
	if !cap["tools"] || !cap["prompt"] {
		t.Fatalf("capabilities = %v, want tools+prompt", manifests[0].Capabilities)
	}
}

// TestExploreWire drives the explore tool (no session context required)
// through the abep envelope against a fake jj-server.
func TestExploreWire(t *testing.T) {
	jj := newFakeJJ()
	defer jj.Close()
	// Seed org/repo through the fake's HTTP surface (same paths the explore
	// tool reads from).
	post := func(path string, body map[string]interface{}) {
		b, _ := json.Marshal(body)
		resp, err := http.Post(jj.URL()+path, "application/json", strings.NewReader(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	post("/api/v1/repos/ensure-org", map[string]interface{}{"org": "acme"})
	post("/api/v1/repos/ensure", map[string]interface{}{"org": "acme", "repo": "api"})

	s := &server{base: jj.URL()}

	hub := abep.NewInprocHub()
	m, err := abep.ParseManifest(manifestYaml)
	if err != nil {
		t.Fatal(err)
	}
	agent, ext := startRepoExt(t, hub, m.Config(s.handlers(), nil, nil))
	defer ext.Close()
	defer agent.Close()

	res, err := agent.CallTool(context.Background(), "", "repo", "explore", "x1",
		map[string]interface{}{})
	if err != nil {
		t.Fatalf("explore wire: %v", err)
	}
	if !strings.Contains(res.Content, "acme") || !strings.Contains(res.Content, "api") {
		t.Fatalf("explore content = %q", res.Content)
	}
}
