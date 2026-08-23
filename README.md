# repo-extension

Go extension server (NATS tool layer) replacing the Rust `repo-tools`
service. It forwards agent file/git tools to the jj-server Contents API and
keeps every coding session's jj workspace in a strict **1:1 with its
bookmark**, maintained eagerly via the agent's lifecycle trigger hooks.

## Model: lifecycle events (eager, no lazy creation)

The agent owns session lifecycle and, after every committed action
(create/fork/rename/delete), publishes a best-effort event to the durable
JetStream stream `RCODER_NOTIFY`:

| Subject | Payload |
| --- | --- |
| `notify.lifecycle.session.created` | `{event, session_name}` |
| `notify.lifecycle.session.forked` | `{event, session_name, parent}` |
| `notify.lifecycle.session.renamed` | `{event, from, to}` |
| `notify.lifecycle.session.deleted` | `{event, session_name}` |

repo-extension consumes them via a **durable consumer with manual acks**
(events emitted while it is down are delivered on restart). Every step is
idempotent, so at-least-once redeliveries converge:

- `created` → jj ensure org/repo + bookmark from `main` (head fallback) + row
- `forked` → bookmark from the **parent's bookmark** (true workspace
  inheritance at fork time; the parent is materialized first when unmapped)
- `renamed` → new bookmark at the old one's position + row update + old
  bookmark removed
- `deleted` → bookmark + row removed

Non-derived session names (not `org:repo:bookmark`) are discarded with a log.

Session names derive strictly: `session_name = org:repo:bookmark`, each
component matching `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$` and never containing
`:` (reserved as the separator; bijection) nor `..`/trailing `.`/`.lock`
(jj/git ref rules — inner dots ARE allowed, `my.repo` is fine). `:` needs no
URL encoding and is never embedded in NATS subjects or KV keys (the agent
hashes the name into a safe token there; payloads carry the real name).
The tool path is **strict**: an unmapped session resolves to
a "workspace not ready" error — no lazy creation, no fallbacks.

## Consistency

Lifecycle events are the fast path; a background reconciler (default 60s) is
the correctness backstop. All rules idempotent, row-level only — the
reconciler never destroys agent sessions or jj state:

- row + bookmark gone → unmap (a later lifecycle event re-creates it);
- row + session gone → unmap; the bookmark becomes a legal orphan, adoptable
  via the ops endpoint;
- session with a derived name but no row (lost event — publish failure or
  downtime beyond stream retention) → workspace backfilled anchored at
  `main`. Fork anchoring precision needs the original event, which is why
  the notify stream keeps 1-day retention: it makes retention a tuning
  knob, not a correctness pillar;
- orphan bookmark (no row) → legal long-term state, log only.

## NATS tools (unchanged protocol)

`tool.call.{name}` → `tool.result.{call_id}` via
`forgejo.develop.10.199.64.20.nip.io/rucoder/extension-sdk-go`; results
>256 KiB offload to the `RCODER_TOOL` object store.

Tools: `read`, `write`, `edit`, `delete`, `ls`, `grep`, `explore`,
`git-diff`, `git-blame`, `git-log`, `git-show`, `git-branches`.

Context resolution priority: `_session` (agent-injected) → legacy
`_org`/`_repo`/`_branch` args.

## Ops surface (HTTP, chi)

Read-only inspection plus one manual adoption helper. Workspace writes are
event-driven; there are no workspace-write endpoints.

| Endpoint | Meaning |
| --- | --- |
| `GET /api/v1/health` | probe |
| `GET /api/v1/repos` | jj tree with `managed`/`session_name` annotations |
| `GET /api/v1/repos/{o}/{r}/bookmarks` | bookmarks with session binding |
| `GET /api/v1/session-map?session=NAME` | reverse lookup |
| `POST /api/v1/repos/{o}/{r}/bookmarks/{bm}/session` | bind an orphan bookmark to a (created) session, idempotent |

## jj-server requirement

`POST /api/v1/repos/bookmark-from` must accept `source_rev` resolving
bookmarks, commit-id prefixes, change-id prefixes, and `""` (head).

## Config

| Env | Default |
| --- | ------- |
| `RUCODER_REPO_MANAGER_URL` | `http://rucoder-repo.temp.svc.cluster.local:80` |
| `RUCODER_AGENT_URL` | `http://rucoder-agent.temp.svc.cluster.local:80` |
| `NATS_URL` | `nats://nats.develop.svc.cluster.local:4222` |
| `POSTGRES_HOST/PORT/USER/PASSWORD` | dev cluster postgres |
| `POSTGRES_DB_REPOEXT` | `rucoder_repoext` (private DB; never the agent's) |
| `RUCODER_RECONCILE_INTERVAL_SECS` | `60` |
| `RUCODER_PORT` | `8080` |

## Tests

```bash
go test ./...                       # unit (naming, lifecycle handlers w/ fakes)
REPOEXT_TEST_PG=1 go test ./...     # + PG-backed contract & reconcile
```
