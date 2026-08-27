package main

import (
	"context"
	_ "embed"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	abep "abep.dev/sdk"
	natsbus "abep.dev/sdk/nats"
	"forgejo.develop.10.199.64.20.nip.io/rucoder/go-shared/env"
)

//go:embed manifest.yaml
var manifestYaml []byte

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
	ext   *abep.Extension
}

func main() {
	log := slog.Default().With("svc", "repo-extension")
	s := &server{
		base:  env.Or("RUCODER_REPO_MANAGER_URL", "http://rucoder-repo.temp.svc.cluster.local:80"),
		agent: env.Or("RUCODER_AGENT_URL", "http://rucoder-agent.temp.svc.cluster.local:80"),
	}
	s.jj = newJJClient(s.base)
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

	natsURL := env.Or("NATS_URL", "nats://nats.develop.svc.cluster.local:4222")

	nbus, err := natsbus.Connect(natsURL)
	if err != nil {
		log.Error("nats connect failed", "err", err)
		os.Exit(1)
	}

	manifest, err := abep.ParseManifest(manifestYaml)
	if err != nil {
		log.Error("load manifest failed", "err", err)
		os.Exit(1)
	}

	if err := abep.Serve(
		nbus,
		manifest.Config(
			s.handlers(),
			map[string]abep.VariableSpec{
				"org":      {Resolve: s.resolveOrg},
				"repo":     {Resolve: s.resolveRepo},
				"bookmark": {Resolve: s.resolveBookmark},
			},
			func(ctx context.Context, ev abep.LifecycleEvent) error {
				return s.handleLifecycleEvent(ctx, ev.Kind, ev)
			},
		),
		abep.ServeOptions{
			Handler: s.router(),
			Run: func(runCtx context.Context, ext *abep.Extension) {
				s.ext = ext
				log.Info("listening", "port", env.Or("RUCODER_PORT", "8080"), "nats", natsURL)
				go runReconciler(runCtx, s, time.Duration(env.Int("RUCODER_RECONCILE_INTERVAL_SECS", 60))*time.Second)
			},
		},
	); err != nil {
		log.Error("serve failed", "err", err)
		os.Exit(1)
	}
}

func pgConfig() PgConfig {
	return PgConfig{
		Host:     env.Or("POSTGRES_HOST", "postgres.develop.svc.cluster.local"),
		Port:     env.NormalizePort(env.Or("POSTGRES_PORT", "5432")),
		User:     env.Or("POSTGRES_USER", "root"),
		Password: env.Or("POSTGRES_PASSWORD", "devpassword"),
		DB:       env.Or("POSTGRES_DB_REPOEXT", "rucoder_repoext"),
	}
}
