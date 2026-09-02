package main

import (
	"context"
	_ "embed"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	abcprotocol "forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go"
	"forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go/extension"
	"forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go/manifest"
	natsbus "forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go/transport/nats"
	"forgejo.develop.10.199.64.20.nip.io/zergx/repo-extension/internal/env"
)

//go:embed manifest.yaml
var manifestYaml []byte

// server wires the two faces of repo-extension:
//
//   - tool face (NATS): agent file/git tools forwarding to jjlab, with
//     the (org, repo, bookmark) triple resolved from the injected `_session`
//     via the mapping table;
//   - workspace face: lifecycle events from the agent (durable NATS
//     subscription) eagerly mirrored into jj bookmarks + mapping rows.
type server struct {
	base  string // jjlab base URL
	agent string // agent-ts base URL
	store *Store
	cache *sessCache
	jj    *jjClient
	ag    *agentClient
	ext   *extension.Extension
}

func main() {
	log := slog.Default().With("svc", "repo-extension")
	s := &server{
		base:  env.Or("ZERGX_REPO_MANAGER_URL", "http://jjlab.zergx.svc.cluster.local:80"),
		agent: env.Or("ZERGX_AGENT_URL", "http://agent.zergx.svc.cluster.local:80"),
	}
	s.jj = newJJClient(s.base, env.Or("JJLAB_TOKEN", env.Or("ZERGX_JJLAB_TOKEN", "devtoken")))
	s.ag = newAgentClient(s.agent)
	s.cache = newSessCache(5 * time.Second)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := OpenStore(ctx, pgConfig())
	if err != nil {
		log.Error("pg connect failed", "err", err)
		os.Exit(1)
	}
	s.store = store
	defer store.Close()

	natsURL := env.Or("NATS_URL", "nats://nats.zergx.svc.cluster.local:4222")

	nbus, err := natsbus.Connect(natsURL)
	if err != nil {
		log.Error("nats connect failed", "err", err)
		os.Exit(1)
	}

	m, err := manifest.ParseManifest(manifestYaml)
	if err != nil {
		log.Error("load manifest failed", "err", err)
		os.Exit(1)
	}

	if err := extension.Serve(
		extension.New(nbus, m.BuildConfig(manifest.Bindings{
			Handlers: s.handlers(),
			Variables: map[string]extension.VariableSpec{
				"org":      {Resolve: s.resolveOrg},
				"repo":     {Resolve: s.resolveRepo},
				"bookmark": {Resolve: s.resolveBookmark},
			},
			OnLifecycle: func(ctx context.Context, ev abcprotocol.LifecycleEvent) error {
				return s.handleLifecycleEvent(ctx, string(ev.Kind), ev)
			},
		})),
		extension.ServeOptions{
			Handler: s.router(),
			Run: func(runCtx context.Context, ext *extension.Extension) {
				s.ext = ext
				log.Info("listening", "port", env.Or("ZERGX_PORT", "8080"), "nats", natsURL)
				go runReconciler(runCtx, s, time.Duration(env.Int("ZERGX_RECONCILE_INTERVAL_SECS", 60))*time.Second)
			},
		},
	); err != nil {
		log.Error("serve failed", "err", err)
		os.Exit(1)
	}
}

func pgConfig() PgConfig {
	return PgConfig{
		Host:     env.Or("POSTGRES_HOST", "postgres.zergx.svc.cluster.local"),
		Port:     env.NormalizePort(env.Or("POSTGRES_PORT", "5432")),
		User:     env.Or("POSTGRES_USER", "root"),
		Password: env.Or("POSTGRES_PASSWORD", "devpassword"),
		DB:       env.Or("POSTGRES_DB_REPOEXT", "zergx_repoext"),
	}
}
