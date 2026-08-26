package main

import (
	"context"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	abep "abep.dev/sdk"
	natsbus "abep.dev/sdk/nats"
)

// server wires the two faces of repo-extension:
//
//   - tool face (NATS): agent file/git tools forwarding to jj-server, with
//     the (org, repo, bookmark) triple resolved from the injected `_session`
//     via the mapping table;
//   - workspace face: lifecycle events from the agent (durable NATS
//     subscription) eagerly mirrored into jj bookmarks + mapping rows.
type server struct {
	base  string // jj-server base URL
	agent string // agent-ts base URL
	store *Store
	cache *sessCache
	jj    *jjClient
	ag    *agentClient
}

func main() {
	s := &server{
		base:  envOr("RUCODER_REPO_MANAGER_URL", "http://rucoder-repo.temp.svc.cluster.local:80"),
		agent: envOr("RUCODER_AGENT_URL", "http://rucoder-agent.temp.svc.cluster.local:80"),
	}
	s.jj = newJJClient(s.base)
	s.ag = newAgentClient(s.agent)
	s.cache = newSessCache(5 * time.Second)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := OpenStore(ctx, pgConfig())
	if err != nil {
		panic(err)
	}
	s.store = store
	defer store.Close()

	natsURL := envOr("NATS_URL", "nats://nats.develop.svc.cluster.local:4222")

	nbus, err := natsbus.Connect(natsURL)
	if err != nil {
		panic(err)
	}

	if err := abep.Serve(
		nbus,
		abep.Config{
			ID:      "repo",
			Version: "0.3.1",
			Tools:   s.tools(),
			Variables: map[string]abep.VariableSpec{
				"org":      {Scope: "session", Resolve: s.resolveOrg},
				"repo":     {Scope: "session", Resolve: s.resolveRepo},
				"bookmark": {Scope: "session", Resolve: s.resolveBookmark},
			},
			Lifecycle: []string{"created", "forked", "renamed", "deleted"},
			OnLifecycle: func(ctx context.Context, ev abep.LifecycleEvent) error {
				return s.handleLifecycleEvent(ctx, ev.Kind, ev)
			},
		},
		abep.ServeOptions{
			Handler: s.router(),
			Run: func(runCtx context.Context, _ *abep.Extension) {
				go runReconciler(runCtx, s, time.Duration(envInt("RUCODER_RECONCILE_INTERVAL_SECS", 60))*time.Second)
			},
		},
	); err != nil {
		panic(err)
	}
}

func pgConfig() PgConfig {
	return PgConfig{
		Host:     envOr("POSTGRES_HOST", "postgres.develop.svc.cluster.local"),
		Port:     normalizePort(envOr("POSTGRES_PORT", "5432")),
		User:     envOr("POSTGRES_USER", "root"),
		Password: envOr("POSTGRES_PASSWORD", "devpassword"),
		DB:       envOr("POSTGRES_DB_REPOEXT", "rucoder_repoext"),
	}
}

// normalizePort accepts both plain ports ("5432") and k8s link-style values
// ("tcp://10.0.0.1:5432") that pods inherit from the environment.
func normalizePort(v string) string {
	if i := strings.LastIndexByte(v, ':'); i >= 0 {
		candidate := v[i+1:]
		if candidate != "" {
			return candidate
		}
	}
	return v
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envInt(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return d
}
