package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go/agent"
	"forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go/extension"
	"forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go/manifest"
	"forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go/natsrun"
	natsbus "forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go/transport/nats"
)

// ---- manifest binding ----

// TestManifestBinding asserts the YAML manifest is the single source of
// truth: every declared tool has a bound handler, every handler is declared,
// and descriptions/schemas/variables/lifecycle are carried onto the config.
func TestManifestBinding(t *testing.T) {
	m, err := manifest.ParseManifest(manifestYaml)
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if m.ID != "repo" {
		t.Fatalf("manifest id = %q", m.ID)
	}
	if len(m.Tools) != 14 {
		t.Fatalf("manifest tools = %d, want 14", len(m.Tools))
	}
	if len(m.Variables) != 3 {
		t.Fatalf("manifest variables = %d, want 3", len(m.Variables))
	}
	if len(m.Lifecycle) != 4 {
		t.Fatalf("manifest lifecycle = %v, want 4 kinds", m.Lifecycle)
	}

	cfg := m.BuildConfig(manifest.Bindings{Handlers: manifestHandlers(), Variables: map[string]extension.VariableSpec{
		"org":      {Resolve: func(context.Context, string) (string, error) { return "acme", nil }},
		"repo":     {Resolve: func(context.Context, string) (string, error) { return "api", nil }},
		"bookmark": {Resolve: func(context.Context, string) (string, error) { return "main", nil }},
	}})

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
func manifestHandlers() map[string]extension.ToolSpec {
	return (&server{jj: newJJClient("http://unused", "t")}).handlers()
}

// ---- wire-level e2e over the inproc transport ----

// startRepoExt serves the extension on the hub and waits for setup.
func startRepoExt(t *testing.T, cfg extension.Config) (*agent.Agent, *extension.Extension) {
	t.Helper()
	srv, err := natsrun.Start(natsrun.Config{Storage: natsrun.Memory})
	if err != nil {
		t.Fatalf("start nats: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	extBus, err := natsbus.Connect(srv.URL())
	if err != nil {
		t.Fatalf("ext bus: %v", err)
	}
	agentBus, err := natsbus.Connect(srv.URL())
	if err != nil {
		t.Fatalf("agent bus: %v", err)
	}
	ext := extension.New(extBus, cfg)
	ag := agent.New(agentBus)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = extBus.Close(); _ = agentBus.Close() })
	go func() { _ = ext.Serve(ctx) }()
	time.Sleep(300 * time.Millisecond)
	return ag, ext
}

func TestDiscoverWire(t *testing.T) {
	m, err := manifest.ParseManifest(manifestYaml)
	if err != nil {
		t.Fatal(err)
	}
	agent, ext := startRepoExt(t, m.BuildConfig(manifest.Bindings{Handlers: manifestHandlers()}))
	defer ext.Close()
	defer agent.Close()

	manifests, err := agent.Discover(context.Background(), 500)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(manifests) != 1 || manifests[0].Id != "repo" {
		t.Fatalf("discover = %+v", manifests)
	}
	if len(*manifests[0].Tools) != 14 {
		t.Fatalf("discover tools = %d, want 14", len(*manifests[0].Tools))
	}
}

// TestExploreWire drives the explore tool (no session context required)
// through the abep envelope against a fake jjlab.
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
	post("/api/v1/repos/acme/api", map[string]interface{}{"default_branch": "main"})

	s := &server{base: jj.URL(), jj: newJJClient(jj.URL(), "t")}

	m, err := manifest.ParseManifest(manifestYaml)
	if err != nil {
		t.Fatal(err)
	}
	agent, ext := startRepoExt(t, m.BuildConfig(manifest.Bindings{Handlers: s.handlers()}))
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
