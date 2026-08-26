package main

import (
	"context"
	"fmt"
	"time"
)

// runReconciler converges drift between jj-server bookmarks, the mapping
// table, and agent sessions. It is the correctness backstop for the
// best-effort lifecycle events — every rule is idempotent:
//
//   - row + bookmark gone (jj-side delete): drop the row;
//   - row + session gone (agent-side delete): drop the row; the bookmark
//     becomes a legal orphan, adoptable by a future same-name session;
//   - session (derived name) without a row (lost lifecycle event — publish
//     failure or downtime beyond stream retention): backfill the workspace
//     anchored at `main` (fork anchoring precision needs the original event,
//     which is why stream retention stays at 1 day);
//   - orphan bookmark (no row): legal long-term state, log only.
func runReconciler(ctx context.Context, s *server, interval time.Duration) {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := reconcileOnce(ctx, s); err != nil {
				log.Error("reconcile failed", "err", err)
			}
		}
	}
}

func reconcileOnce(ctx context.Context, s *server) error {
	sessions, err := s.ag.ListSessions(ctx)
	if err != nil {
		return err
	}
	if err := convergeDrift(ctx, s, sessions); err != nil {
		return err
	}
	return backfillWorkspaces(ctx, s, sessions)
}

// convergeDrift enforces row-level consistency for managed repos. It only
// ever deletes mapping rows — never agent sessions or jj state.
func convergeDrift(ctx context.Context, s *server, sessions map[string]bool) error {
	managed, err := s.store.ListManaged(ctx)
	if err != nil {
		return fmt.Errorf("list managed: %w", err)
	}
	if len(managed) == 0 {
		return nil
	}
	tree, err := s.jj.GetRepoTree(ctx)
	if err != nil {
		return err
	}

	for _, m := range managed {
		rows, err := s.store.ListRowsForRepo(ctx, m.Org, m.Repo)
		if err != nil {
			return fmt.Errorf("list rows %s/%s: %w", m.Org, m.Repo, err)
		}
		bms := map[string]bool{}
		if repos, ok := tree[m.Org]; ok {
			for _, b := range repos[m.Repo] {
				bms[b] = true
			}
		}
		bound := map[string]bool{}
		for _, row := range rows {
			bound[row.Bookmark] = true
			switch {
			case !bms[row.Bookmark]:
				log.Warn("reconcile: bookmark gone — unmapping session",
					"org", row.Org, "repo", row.Repo, "bookmark", row.Bookmark, "session", row.SessionName)
				if err := s.store.DeleteRow(ctx, row.Org, row.Repo, row.Bookmark); err != nil {
					return errDownstream("postgres", err)
				}
				s.cache.evict(row.SessionName)
			case !sessions[row.SessionName]:
				// Session gone: the bookmark becomes a legal orphan (work
				// preserved; adoptable by a same-name session later).
				log.Warn("reconcile: session gone — unmapping (bookmark becomes orphan)",
					"session", row.SessionName, "org", row.Org, "repo", row.Repo, "bookmark", row.Bookmark)
				if err := s.store.DeleteRow(ctx, row.Org, row.Repo, row.Bookmark); err != nil {
					return errDownstream("postgres", err)
				}
				s.cache.evict(row.SessionName)
			}
		}
		for bm := range bms {
			if !bound[bm] {
				log.Info("reconcile: orphan bookmark (adoptable via ops endpoint)",
					"org", m.Org, "repo", m.Repo, "bookmark", bm)
			}
		}
	}
	return nil
}

// backfillWorkspaces heals sessions whose lifecycle event never arrived
// (best-effort publish, or downtime beyond stream retention): any agent
// session with a derived name and no mapping row gets its workspace created
// now. This rule makes stream retention a tuning knob rather than a
// correctness pillar; fork anchoring precision beyond `main` requires the
// original event, which retention (1 day) preserves for realistic outages.
func backfillWorkspaces(ctx context.Context, s *server, sessions map[string]bool) error {
	rows, err := s.store.ListRows(ctx)
	if err != nil {
		return errDownstream("postgres", err)
	}
	mapped := map[string]bool{}
	for _, r := range rows {
		mapped[r.SessionName] = true
	}
	for name := range sessions {
		if mapped[name] {
			continue
		}
		org, repo, bm, ok := parseSessionName(name)
		if !ok {
			continue // non-workspace session (e.g. "hi") — not our contract
		}
		log.Info("reconcile: session has no workspace — backfilling", "session", name)
		if err := s.ensureCreated(ctx, org, repo, bm, name); err != nil {
			if isPermanent(err) {
				continue
			}
			return err
		}
	}
	return nil
}
