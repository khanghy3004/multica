# Per-user terminal token usage — design

Date: 2026-06-12

## Goal

Track tokens consumed by interactive `claude` terminal sessions, attributed to
the user who opened the terminal, and surface a per-user table on the Usage
(Dashboard) page.

## Token source

Terminal `claude` writes a transcript JSONL with per-turn
`usage{input_tokens, output_tokens, cache_creation_input_tokens,
cache_read_input_tokens}` to `~/.claude/projects/<cwd-slug>/<session-id>.jsonl`.
The daemon spawns claude with `--session-id <uuid>` so the filename is
deterministic; it locates the file by globbing `~/.claude/projects/*/<uuid>.jsonl`
(no need to replicate claude's cwd-slug algorithm).

## Data flow

1. **Backend `TerminalWebSocket`** adds the authenticated `user_id` to the
   `terminal:open` payload — the daemon otherwise has no idea who opened the
   session.
2. **Daemon `handleOpen`** generates a session UUID, spawns
   `claude --session-id <uuid> --settings <…>`, and records
   `{sessionID, userID, workspaceID, uuid}`.
3. **Daemon usage reader** — on a 30s ticker and on session close — parses the
   transcript, sums **cumulative** per-model totals, and POSTs them to the
   backend.
4. **Backend** upserts into `terminal_session_usage` (PK `session_id, model`)
   with REPLACE semantics (store cumulative totals, not deltas). Idempotent: a
   daemon restart re-reads the whole transcript → identical totals → no
   double-count.
5. **Read endpoint** `GET /api/dashboard/usage/terminal-by-user?days=&tz=`
   aggregates `SUM` across sessions grouped by user, returns
   `[{user_id, name, input_tokens, output_tokens, cache_read_tokens,
   cache_write_tokens, total_tokens}]`.
6. **Frontend** adds a "Terminal usage (by user)" table to `DashboardPage`
   (ActorAvatar + name + token totals over the selected period). Reuses the
   existing `formatTokens` helper and period selector.

## Schema — migration `119_terminal_session_usage`

```sql
CREATE TABLE terminal_session_usage (
    session_id          UUID        NOT NULL,
    workspace_id        UUID        NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    user_id             UUID        NOT NULL REFERENCES "user"(id) ON DELETE CASCADE,
    model               TEXT        NOT NULL,
    input_tokens        BIGINT      NOT NULL DEFAULT 0,
    output_tokens       BIGINT      NOT NULL DEFAULT 0,
    cache_read_tokens   BIGINT      NOT NULL DEFAULT 0,
    cache_write_tokens  BIGINT      NOT NULL DEFAULT 0,
    started_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, model)
);
CREATE INDEX idx_terminal_session_usage_ws_user
    ON terminal_session_usage (workspace_id, user_id);
```

## Endpoints

- `POST /api/workspaces/{id}/terminal/usage` (daemon/user-token authed):
  body `{session_id, user_id, model, input_tokens, output_tokens,
  cache_read_tokens, cache_write_tokens}` → UPSERT REPLACE.
- `GET /api/dashboard/usage/terminal-by-user?days=&tz=` → per-user aggregate.

## Design choices

- **Cumulative per-session + REPLACE upsert**, not deltas — restart-safe and
  idempotent.
- Table shows by-user totals over the selected range only — no per-user charts,
  no cost (YAGNI). Tokens: input / output / cache / total.
- Web-only, matching the terminal feature.

## Out of scope

Cost/pricing, per-day terminal charts, desktop.

## Verification

- Go: transcript parser sums a sample JSONL; `POST` upsert is idempotent under
  repeated cumulative reports; by-user aggregation sums across sessions.
- TS: the dashboard table renders per-user rows from a mocked query.
