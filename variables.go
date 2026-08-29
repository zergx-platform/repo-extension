package main

import (
	"context"

	"forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go/extension"
)

// resolveOrg/resolveRepo/resolveBookmark are the authoritative lazy resolvers
// for the session variables `vars.repo.org` / `vars.repo.repo` /
// `vars.repo.bookmark`. They read the extension's own mapping table (the
// single source of truth); the KV copy is a projection written on lifecycle
// events, and these resolvers serve as the KV-miss fallback.

func (s *server) resolveOrg(ctx context.Context, sessionName string) (string, error) {
	o, _, _, err := s.resolveSession(ctx, sessionName)
	return o, err
}

func (s *server) resolveRepo(ctx context.Context, sessionName string) (string, error) {
	_, r, _, err := s.resolveSession(ctx, sessionName)
	return r, err
}

func (s *server) resolveBookmark(ctx context.Context, sessionName string) (string, error) {
	_, _, b, err := s.resolveSession(ctx, sessionName)
	return b, err
}

// publishSessionVars projects the session's org/repo/bookmark into the shared
// KV (vars.repo.{token}.*) after the authoritative mapping changed. Called by
// the lifecycle handler; the reconciler additionally rebuilds projections.
func (s *server) publishSessionVars(ctx context.Context, ext *extension.Extension, sessionName string) {
	o, r, b, err := s.resolveSession(ctx, sessionName)
	if err != nil {
		return
	}
	_ = ext.SetSessionVariable(ctx, sessionName, "org", o)
	_ = ext.SetSessionVariable(ctx, sessionName, "repo", r)
	_ = ext.SetSessionVariable(ctx, sessionName, "bookmark", b)
}

// clearSessionVars removes a session's projected variables on deletion.
func (s *server) clearSessionVars(ctx context.Context, ext *extension.Extension, sessionName string) {
	_ = ext.DeleteSessionVariables(ctx, sessionName)
}
