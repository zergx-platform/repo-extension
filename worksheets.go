package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"

	abcprotocol "github.com/abcp-sdk/abc-protocol-go"
)

// Worksheets: durable, human-gated actions published from tool executions.
// The agent stores them (never expiring) and delivers the decision back on
// the `worksheet-decided` call hook; this registry declares which actions
// exist and their fixed behavior on approval. Rejections are acknowledged
// but inert — a passive event tells the owning session's model not to retry.
type WorksheetSpec struct {
	Title func(args map[string]interface{}) (en, zh string)
	Exec  func(ctx context.Context, s *server, session string, args map[string]interface{}) (en, zh string, err error)
}

var worksheetRegistry = map[string]WorksheetSpec{
	"fork-bookmark": {
		Title: func(args map[string]interface{}) (string, string) {
			return fmt.Sprintf("Fork to '%s'", argStr(args, "bookmark")),
				fmt.Sprintf("派生到 '%s'", argStr(args, "bookmark"))
		},
		Exec: execForkRequest,
	},
	"delete-bookmark": {
		Title: func(args map[string]interface{}) (string, string) {
			return fmt.Sprintf("Delete bookmark '%s'", argStr(args, "bookmark")),
				fmt.Sprintf("删除书签 '%s'", argStr(args, "bookmark"))
		},
		Exec: execDeleteBookmark,
	},
}

// newWorksheetID mints an unguessable worksheet id.
func newWorksheetID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("ws-%x", fmt.Sprintf("%p%v", b, slog.Default()))
	}
	return "ws-" + hex.EncodeToString(b)
}

// publishWorksheet lands one worksheet proposal in the session's mailbox.
// The tool returns immediately ("worksheet published"); execution happens
// only after the user approves.
func (s *server) publishWorksheet(ctx context.Context, session, action string, args map[string]interface{}, originCallID string) error {
	spec, ok := worksheetRegistry[action]
	if !ok {
		return fmt.Errorf("unknown worksheet action %q", action)
	}
	en, zh := spec.Title(args)
	ws := map[string]interface{}{
		"worksheet_id": newWorksheetID(),
		"ext_id":       "repo",
		"action":       action,
		"args":         args,
		"title":        lc(ctx, s.ext, session, en, zh),
	}
	if originCallID != "" {
		ws["origin_call_id"] = originCallID
	}
	return s.ext.PublishMailboxEvent(ctx, session, "worksheet_proposed", ws)
}

// onCallHook is the decision entry: the agent calls `worksheet-decided` after
// a human approves or rejects. Execution dedupes by worksheet_id, making
// at-least-once dispatch (retries, reconciler redelivery) exactly-once.
func (s *server) onCallHook(ctx context.Context, hook, sessionName string, args map[string]interface{}) (abcprotocol.HookResponse, error) {
	if hook != "worksheet-decided" {
		return abcprotocol.HookResponse{
			Ok: false,
			Error: &struct {
				Code    abcprotocol.HookResponseErrorCode `json:"code"`
				Message string                            `json:"message"`
			}{Code: abcprotocol.HookResponseErrorCodeNotFound, Message: "no handler for hook " + hook},
		}, nil
	}
	worksheetID := argStr(args, "worksheet_id")
	if worksheetID == "" {
		return hookInvalid("missing worksheet_id"), nil
	}
	action := argStr(args, "action")
	spec, ok := worksheetRegistry[action]
	if !ok {
		return hookInvalid(fmt.Sprintf("unknown worksheet action %q", action)), nil
	}
	decision := argStr(args, "decision")
	target := argStr(args, "session")
	if target == "" {
		target = sessionName
	}

	if decision != "approve" {
		en := fmt.Sprintf("worksheet '%s' was rejected by the user", argStr(args, "title"))
		zh := fmt.Sprintf("工单「%s」已被用户拒绝", argStr(args, "title"))
		_ = s.ext.PublishMailboxEvent(ctx, target, "event", map[string]interface{}{
			"content": lc(ctx, s.ext, target, en, zh),
		})
		return hookOK(), nil
	}

	// Dedup: the first dispatch executes; later redeliveries no-op.
	first, err := s.store.ExecutedWorksheet(ctx, worksheetID, action, target)
	if err != nil {
		return hookInternal("dedup check failed"), nil
	}
	if !first {
		return hookOK(), nil
	}

	wargs, _ := args["args"].(map[string]interface{})
	if wargs == nil {
		wargs = map[string]interface{}{}
	}
	en, zh, err := spec.Exec(ctx, s, target, wargs)
	if err != nil {
		return hookInternal(err.Error()), nil
	}
	_ = s.ext.PublishMailboxEvent(ctx, target, "user_prompt", map[string]interface{}{
		"text": lc(ctx, s.ext, target, en, zh),
	})
	return hookOK(), nil
}

// execForkRequest: bookmark+row FIRST (base_rev anchoring beats the lifecycle
// event and the reconciler), then the session fork (chat pinned at fork_tip),
// then quest injection (reminder event + quest prompt wake the sub-agent).
// Every step is idempotent so retries converge on base_rev.
func execForkRequest(ctx context.Context, s *server, session string, args map[string]interface{}) (string, string, error) {
	org, repo, parentBM, ok := parseSession(session)
	if !ok {
		return "", "", errBad("session %q does not match org:repo:bookmark naming", session)
	}
	newBM := argStr(args, "bookmark")
	if newBM == "" {
		return "", "", errBad("fork-bookmark: missing 'bookmark'")
	}
	quest := argStr(args, "quest")
	if quest == "" {
		return "", "", errBad("fork-bookmark: missing 'quest'")
	}
	baseRev := argStr(args, "base_rev")
	forkTip := argStr(args, "fork_tip")
	newSession := namingSession(org, repo, newBM)

	if err := s.ensureForkedAt(ctx, org, repo, newBM, newSession, baseRev, parentBM); err != nil {
		return "", "", err
	}
	if err := s.ag.ForkSession(ctx, session, newSession, forkTip, "build"); err != nil {
		return "", "", err
	}

	en := fmt.Sprintf("You are forked from bookmark '%s'. After completing your quest, use the mr-create tool to send a merge request back to bookmark '%s'.", parentBM, parentBM)
	zh := fmt.Sprintf("你从书签「%s」派生而来。完成任务后，使用 mr-create 工具向书签「%s」发起合并请求。", parentBM, parentBM)
	_ = s.ext.PublishMailboxEvent(ctx, newSession, "event", map[string]interface{}{
		"content": lc(ctx, s.ext, newSession, en, zh),
	})
	_ = s.ext.PublishMailboxEvent(ctx, newSession, "user_prompt", map[string]interface{}{
		"text": quest,
	})

	return fmt.Sprintf("fork '%s' created; sub-agent started on the quest", newBM),
		fmt.Sprintf("已创建派生「%s」；子代理已开始执行任务", newBM), nil
}

// ensureForkedAt anchors the new bookmark at baseRev (call-time sha) when
// available, else at the parent bookmark — and binds the mapping row.
// Row-first ordering makes the async lifecycle 'forked' event and the
// reconciler backfill both no-op (they early-return on the existing row).
func (s *server) ensureForkedAt(ctx context.Context, org, repo, newBM, newSession, baseRev, parentBM string) error {
	if row, err := s.store.GetRowBySession(ctx, newSession); err != nil {
		return errDownstream("postgres", err)
	} else if row != nil {
		return nil // already bound (retry path)
	}
	if err := s.jj.EnsureRepo(ctx, org, repo); err != nil {
		return err
	}
	anchor := baseRev
	if anchor == "" {
		anchor = parentBM
	}
	if err := s.jj.EnsureBookmark(ctx, org, repo, anchor, newBM); err != nil {
		return err
	}
	return s.bindRow(ctx, org, repo, newBM, newSession)
}

// execDeleteBookmark: withdraw the bookmark AND its session. Deleting the
// agent session fires the lifecycle chain; the direct ensureDeleted call
// makes bookmark+row cleanup immediate and idempotent.
func execDeleteBookmark(ctx context.Context, s *server, session string, args map[string]interface{}) (string, string, error) {
	org, repo, ownBM, ok := parseSession(session)
	if !ok {
		return "", "", errBad("session %q does not match org:repo:bookmark naming", session)
	}
	bm := argStr(args, "bookmark")
	if bm == "" {
		return "", "", errBad("delete-bookmark: missing 'bookmark'")
	}
	if bm == ownBM {
		return "", "", errBad("delete-bookmark: refusing to delete this session's own bookmark")
	}
	if bm == "main" {
		return "", "", errBad("delete-bookmark: refusing to delete the default bookmark 'main'")
	}
	target := argStr(args, "session")
	if target == "" {
		target = namingSession(org, repo, bm)
	}

	if err := s.ag.DeleteSession(ctx, target); err != nil {
		return "", "", err
	}
	if row, err := s.store.GetRowBySession(ctx, target); err != nil {
		return "", "", errDownstream("postgres", err)
	} else if row != nil {
		if err := s.ensureDeleted(ctx, target); err != nil {
			return "", "", err
		}
	} else {
		// No mapping row: orphan bookmark cleanup only.
		if err := s.jj.DeleteBookmark(ctx, org, repo, bm); err != nil {
			return "", "", err
		}
	}

	return fmt.Sprintf("bookmark '%s' and its session deleted", bm),
		fmt.Sprintf("书签「%s」及其会话已删除", bm), nil
}

func argStr(args map[string]interface{}, key string) string {
	if args == nil {
		return ""
	}
	s, _ := args[key].(string)
	return s
}

func argInt(args map[string]interface{}, key string) int64 {
	if args == nil {
		return 0
	}
	switch v := args[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

func hookOK() abcprotocol.HookResponse {
	return abcprotocol.HookResponse{Ok: true}
}

func hookInvalid(msg string) abcprotocol.HookResponse {
	return abcprotocol.HookResponse{
		Ok: false,
		Error: &struct {
			Code    abcprotocol.HookResponseErrorCode `json:"code"`
			Message string                            `json:"message"`
		}{Code: abcprotocol.HookResponseErrorCodeInvalidArgument, Message: msg},
	}
}

func hookInternal(msg string) abcprotocol.HookResponse {
	slog.Warn("worksheet exec failed", "err", msg)
	return abcprotocol.HookResponse{
		Ok: false,
		Error: &struct {
			Code    abcprotocol.HookResponseErrorCode `json:"code"`
			Message string                            `json:"message"`
		}{Code: abcprotocol.HookResponseErrorCodeInternal, Message: msg},
	}
}
