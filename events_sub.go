package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// Lifecycle trigger hooks: the agent publishes session lifecycle events to
// the durable `notify.lifecycle.session.>` subjects (JetStream, stream
// RCODER_NOTIFY); this subscriber eagerly mirrors them into jj workspaces.
// A durable consumer with manual acks means events emitted while
// repo-extension is down are delivered on restart.

const (
	lifecycleSubject = "notify.lifecycle.session.>"
	lifecycleDurable = "repo-extension-lifecycle"
)

// runLifecycleSubscriber consumes lifecycle events until ctx is done,
// reconnecting with a backoff on connection loss.
func runLifecycleSubscriber(ctx context.Context, s *server, natsURL string) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		err := consumeLifecycle(ctx, s, natsURL)
		if ctx.Err() != nil {
			return
		}
		fmt.Printf("[repo-extension] lifecycle subscriber stopped: %v — reconnecting in 5s\n", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func consumeLifecycle(ctx context.Context, s *server, natsURL string) error {
	nc, err := nats.Connect(natsURL,
		nats.MaxReconnects(-1), nats.ReconnectWait(2*time.Second))
	if err != nil {
		return err
	}
	defer nc.Close()

	js, err := nc.JetStream(nats.PublishAsyncMaxPending(256))
	if err != nil {
		return err
	}

	_, err = js.Subscribe(lifecycleSubject, func(msg *nats.Msg) {
		handleLifecycleMsg(ctx, s, msg)
	}, nats.Durable(lifecycleDurable), nats.ManualAck(), nats.DeliverAll())
	if err != nil {
		return err
	}

	fmt.Printf("[repo-extension] lifecycle subscriber ready (%s)\n", lifecycleSubject)
	<-ctx.Done()
	nc.Close()
	return nc.LastError()
}

// lifecycleEvent is the payload shape the agent publishes on
// `notify.lifecycle.session.{kind}`.
type lifecycleEvent struct {
	Event       string `json:"event"`
	SessionName string `json:"session_name"`
	Parent      string `json:"parent"`
	From        string `json:"from"`
	To          string `json:"to"`
}

func handleLifecycleMsg(ctx context.Context, s *server, msg *nats.Msg) {
	var env lifecycleEvent
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		fmt.Printf("[repo-extension] lifecycle: bad payload %q: %v — discarding\n", string(msg.Data), err)
		_ = msg.Term()
		return
	}

	err := s.handleLifecycleEvent(ctx, env.Event, env)
	switch {
	case err == nil:
		_ = msg.Ack()
	case isPermanent(err):
		// Can never succeed (e.g. name not workspace-derived) — drop.
		fmt.Printf("[repo-extension] lifecycle %s: %v — discarding\n", env.Event, err)
		_ = msg.Term()
	default:
		// Transient (jj/PG down) — retry with backoff via redelivery.
		fmt.Printf("[repo-extension] lifecycle %s: %v — redelivering\n", env.Event, err)
		_ = msg.NakWithDelay(5 * time.Second)
	}
}

// handleLifecycleEvent mirrors one agent lifecycle event into the workspace
// layer. Every step is idempotent: redeliveries (at-least-once) converge.
func (s *server) handleLifecycleEvent(ctx context.Context, event string, env lifecycleEvent) error {
	switch event {
	case "created":
		org, repo, bm, ok := parseSessionName(env.SessionName)
		if !ok {
			return errBad("session %q 不符合 org:repo:bookmark 命名 — 忽略", env.SessionName)
		}
		return s.ensureCreated(ctx, org, repo, bm, env.SessionName)
	case "forked":
		org, repo, bm, ok := parseSessionName(env.SessionName)
		if !ok {
			return errBad("session %q 不符合 org:repo:bookmark 命名 — 忽略", env.SessionName)
		}
		return s.ensureForked(ctx, org, repo, bm, env.SessionName, env.Parent)
	case "renamed":
		return s.ensureRenamed(ctx, env.From, env.To)
	case "deleted":
		return s.ensureDeleted(ctx, env.SessionName)
	default:
		return errBad("unknown lifecycle event %q — ignoring", event)
	}
}

// ensureCreated: repo + bookmark from `main` (head fallback) + mapping row.
func (s *server) ensureCreated(ctx context.Context, org, repo, bm, sid string) error {
	if row, err := s.store.GetRowBySession(ctx, sid); err != nil {
		return errDownstream("postgres", err)
	} else if row != nil {
		return nil // already mirrored
	}
	if err := s.jj.EnsureOrg(ctx, org); err != nil {
		return err
	}
	if err := s.jj.EnsureRepo(ctx, org, repo); err != nil {
		return err
	}
	if err := s.ensureBookmarkAnchored(ctx, org, repo, bm); err != nil {
		return err
	}
	return s.bindRow(ctx, org, repo, bm, sid)
}

// ensureForked: bookmark from the parent's bookmark (true workspace
// inheritance at fork time) + mapping row. The parent is materialized first
// when its own event was missed (e.g. pre-event sessions).
func (s *server) ensureForked(ctx context.Context, org, repo, bm, sid, parentSid string) error {
	if row, err := s.store.GetRowBySession(ctx, sid); err != nil {
		return errDownstream("postgres", err)
	} else if row != nil {
		return nil
	}

	// Resolve the parent's bookmark; materialize the parent when unmapped
	// (legacy or missed event) so the fork anchors at real work.
	var parentBM string
	if pOrg, pRepo, pBM, ok := parseSessionName(parentSid); ok {
		if pOrg != org || pRepo != repo {
			return errBad("fork 跨仓库（%s → %s/%s）— 不支持", parentSid, org, repo)
		}
		parentBM = pBM
		if prow, err := s.store.GetRow(ctx, org, repo, pBM); err != nil {
			return errDownstream("postgres", err)
		} else if prow == nil {
			if err := s.ensureCreated(ctx, org, repo, pBM, parentSid); err != nil {
				return err
			}
		}
	} else {
		parentBM = "main" // non-derived parent: anchor at main
	}

	if err := s.jj.EnsureBookmark(ctx, org, repo, parentBM, bm); err != nil {
		return err
	}
	return s.bindRow(ctx, org, repo, bm, sid)
}

// ensureRenamed: dual rename — new bookmark at the old one's position, row
// update, old bookmark removed.
func (s *server) ensureRenamed(ctx context.Context, fromSid, toSid string) error {
	fromOrg, fromRepo, fromBM, ok := parseSessionName(fromSid)
	if !ok {
		return errBad("session %q 不符合命名 — 忽略 rename", fromSid)
	}
	toOrg, toRepo, toBM, ok := parseSessionName(toSid)
	if !ok {
		return errBad("session %q 不符合命名 — 忽略 rename", toSid)
	}
	if fromOrg != toOrg || fromRepo != toRepo {
		return errBad("rename 跨仓库（%s → %s）— 不支持", fromSid, toSid)
	}

	row, err := s.store.GetRow(ctx, fromOrg, fromRepo, fromBM)
	if err != nil {
		return errDownstream("postgres", err)
	}
	if row == nil {
		// Old name never had a workspace; treat as a plain create.
		return s.ensureCreated(ctx, toOrg, toRepo, toBM, toSid)
	}
	if err := s.jj.EnsureBookmark(ctx, fromOrg, fromRepo, fromBM, toBM); err != nil {
		return err
	}
	if err := s.store.RenameRow(ctx, fromOrg, fromRepo, fromBM, toBM, toSid); err != nil {
		if statusOf(err) == 409 {
			return nil // concurrent rename already won
		}
		return errDownstream("postgres", err)
	}
	s.cache.evict(fromSid)
	return s.jj.DeleteBookmark(ctx, fromOrg, fromRepo, fromBM)
}

// ensureDeleted: bookmark + mapping row removed (bookmark-first order: a
// crash mid-way leaves an adoptable orphan, never a dangling row).
func (s *server) ensureDeleted(ctx context.Context, sid string) error {
	org, repo, bm, ok := parseSessionName(sid)
	if !ok {
		return errBad("session %q 不符合命名 — 忽略 delete", sid)
	}
	row, err := s.store.GetRowBySession(ctx, sid)
	if err != nil {
		return errDownstream("postgres", err)
	}
	if row == nil {
		return nil
	}
	if err := s.jj.DeleteBookmark(ctx, org, repo, bm); err != nil {
		return err
	}
	if err := s.store.DeleteRow(ctx, org, repo, bm); err != nil {
		return errDownstream("postgres", err)
	}
	s.cache.evict(sid)
	return nil
}

// ensureBookmarkAnchored creates bookmark at `main`, falling back to the
// repo head when `main` itself does not exist (fresh repo bootstrap order).
func (s *server) ensureBookmarkAnchored(ctx context.Context, org, repo, bm string) error {
	anchor := "main"
	if ok, err := s.jj.CanResolve(ctx, org, repo, anchor); err != nil {
		return err
	} else if !ok {
		anchor = "" // jj resolves "" as the repo head
	}
	return s.jj.EnsureBookmark(ctx, org, repo, anchor, bm)
}
