# Claude Code Subagent Sync — Design

**Date:** 2026-06-01
**Status:** Draft
**Author:** sw-cc2@itrvn.com

## 1. Problem

The user maintains Claude Code subagent definitions as Markdown files under `~/.claude/agents/*.md` on their local machine. Each file contains a YAML frontmatter block (`name`, `description`, `tools`, `model`) followed by a system-prompt body. Today these files are invisible to Multica: the user cannot list, edit, or assign Multica issues to them.

Multica already has a full agent CRUD stack (Go handler, sqlc queries, React detail page, issue assignee picker), a `claude` provider in `server/pkg/agent/claude.go` that shells out to the local `claude` CLI, and a runtime-local-skills discovery flow that scans daemon-side folders. The gap is **bidirectional sync** between Multica's `agent` table and the user's on-disk subagent files, so a subagent can be treated as a first-class Multica agent and receive issue assignments.

## 2. Goals

- Each `.md` file under a runtime's `~/.claude/agents/` is represented by exactly one row in the `agent` table belonging to the runtime's workspace.
- Editing the agent in the Multica UI rewrites the on-disk file on the daemon machine.
- Editing the on-disk file (in the editor of the user's choice) updates the Multica agent row on next daemon heartbeat.
- Issues can be assigned to a synced subagent with no changes to the existing assignment, task queue, or activity surfaces.
- Conflict resolution: last-write-wins by mtime (matches the existing local-skills pattern; no UI conflict resolution needed in v1).

## 3. Non-Goals

- Codex / Cursor / other-provider subagents. The schema reserves space (`source_kind` column) but only `claude_subagent` ships in v1.
- Multi-machine same-user replication. Each daemon owns its own `~/.claude/agents` directory; a user with two laptops gets two independent agent sets.
- Conflict resolution UI (diff viewer, lock-on-edit, manual merge). Last-write-wins is accepted.
- Cross-workspace sharing of synced agents. They follow existing `agent.visibility` (workspace / private).

## 4. Architecture Overview

```
~/.claude/agents/*.md         ──watch──▶  daemon (local_subagents.go)
        ▲                                       │
        │ atomic write                          │ heartbeat report
        │                                       ▼
   daemon push                          server (agent_sync.go)
        ▲                                       │
        │ pending_subagent_write                │ upsert / archive
        │                                       ▼
   server (agent_sync.go)  ◀────update──── agent table
                                                ▲
                                                │ standard UpdateAgent
                                                │
                                          React UI (existing)
```

Three responsibilities split cleanly across layers:

- **Daemon** owns the filesystem. It enumerates `~/.claude/agents/*.md` on heartbeat, parses frontmatter, writes files atomically when the server pushes content, and never touches the DB directly.
- **Server** owns reconciliation. It compares the daemon's reported list against the `agent` rows for the runtime, decides who wins per mtime, enqueues push-down writes, and archives missing files.
- **Frontend** treats synced agents as ordinary agents. A small badge marks them; otherwise the assignment / detail / list flow is unchanged.

## 5. Data Model

### 5.1 Migration

New file: `server/migrations/NNN_claude_subagent_sync.sql` (NNN = next available).

```sql
ALTER TABLE agent
  ADD COLUMN source_path  TEXT,
  ADD COLUMN source_mtime TIMESTAMPTZ,
  ADD COLUMN source_kind  TEXT;

-- Dedup guarantee: one row per (runtime, file path).
-- Partial index so pure Multica-managed agents (source_path IS NULL) are
-- unaffected.
CREATE UNIQUE INDEX agent_source_path_runtime
  ON agent (runtime_id, source_path)
  WHERE source_path IS NOT NULL;

-- Lookup index for the reconciler: "give me every synced agent for this
-- runtime".
CREATE INDEX agent_runtime_source_kind
  ON agent (runtime_id, source_kind)
  WHERE source_kind IS NOT NULL;
```

Down migration drops the indexes and columns.

### 5.2 Column semantics

| Column         | Type          | Meaning                                                      |
|----------------|---------------|--------------------------------------------------------------|
| `source_path`  | `TEXT NULL`   | Absolute path on the daemon machine. `NULL` = pure Multica-managed (existing behaviour). |
| `source_mtime` | `TIMESTAMPTZ` | Filesystem mtime the daemon reported on the last successful sync. Used as the conflict tiebreaker. |
| `source_kind`  | `TEXT NULL`   | Discriminator: `"claude_subagent"` in v1. Future: `"codex_subagent"`, etc. `NULL` for pure Multica agents. |

### 5.3 Frontmatter ↔ DB mapping

| YAML key        | DB column                                                |
|-----------------|----------------------------------------------------------|
| `name`          | `agent.name`                                             |
| `description`   | `agent.description`                                      |
| `model`         | `agent.model`                                            |
| `tools` (list)  | `agent.custom_args` — written as `--allowed-tools=<csv>` |
| (body)          | `agent.instructions`                                     |

Any frontmatter key Multica does not recognise is preserved verbatim in `agent.runtime_config.subagent_extra` (JSONB). This keeps round-trips lossless when the upstream Claude Code adds new frontmatter keys we have not modelled yet.

### 5.4 Slug ↔ filename

The slug (filename without `.md`) is the stable identifier. It must be unique per runtime. The Multica UI disables renaming for synced agents because renaming the file is the user's job — Multica only mirrors what is on disk.

## 6. Daemon Implementation

### 6.1 New file: `server/internal/daemon/local_subagents.go`

Mirrors the shape of `local_skills.go`.

```go
type LocalSubagent struct {
    Path        string    // absolute path
    Slug        string    // filename without .md
    Name        string    // frontmatter `name`, defaults to Slug
    Description string
    Model       string
    Tools       []string
    Body        string    // post-frontmatter text
    Extra       map[string]any // unrecognised frontmatter keys
    Mtime       time.Time
}

// Provider-gated: only "claude" returns a non-empty list today.
func listLocalSubagents(provider string) ([]LocalSubagent, bool, error)

// Atomic temp+rename write. After write, sets file mtime explicitly to
// the value the server provided so a daemon push does not loop back as
// a "newer file" on the next heartbeat.
func writeLocalSubagent(path string, body string, fm Frontmatter, mtime time.Time) error

// Deletes a subagent file when the server tells the daemon to remove it
// (Multica-side archive).
func deleteLocalSubagent(path string) error
```

Frontmatter parser: reuse `gopkg.in/yaml.v3` (already a transitive dep via sqlc). Reject malformed files with a logged warning and skip; do not block the sync of the rest.

### 6.2 Heartbeat extensions

`internal/daemon/daemon.go`:

- On every heartbeat, if the runtime's provider is `claude`, attach the latest `listLocalSubagents` snapshot to the report. Snapshot is small (`<10` files typically); always send so the server can reconcile without a separate request round-trip.
- Heartbeat response carries `pending_subagent_writes []SubagentWrite` and `pending_subagent_deletes []string` (paths). Daemon executes each, then reports the outcome on the next heartbeat. Failure (e.g. permission denied) is logged and the row is re-enqueued.

### 6.3 Watcher

Out of scope for v1. The heartbeat snapshot is the only signal. The user accepts up to one heartbeat interval (~5s) of lag between file edit and Multica reflecting it. A watcher (fsnotify) can be added later without schema changes.

## 7. Server Implementation

### 7.1 New file: `server/internal/handler/agent_sync.go`

Public endpoints:

- `POST /api/runtimes/{id}/subagents/sync` — manual full-resync trigger. Owner/admin only.

Internal:

- `ReconcileSubagents(ctx, runtimeID, snapshot []LocalSubagent)` — called from the heartbeat handler whenever a snapshot is attached.

Reconciler pseudocode:

```
for each file F in snapshot:
    row = SELECT FROM agent WHERE runtime_id = R AND source_path = F.Path
    if row is null:
        INSERT new agent (workspace_id=R.workspace_id, runtime_id=R.id,
                          source_path=F.Path, source_kind='claude_subagent',
                          source_mtime=F.Mtime, name/desc/model/instructions
                          from F)
    else if F.Mtime > row.source_mtime AND F.Mtime > row.updated_at:
        UPDATE agent SET name/desc/model/instructions=F..., source_mtime=F.Mtime
    else if row.updated_at > F.Mtime:
        enqueue SubagentWrite{path=F.Path, body=row.instructions,
                              frontmatter from row, mtime=row.updated_at}
    else:
        no-op

for each row R where source_path is set AND R.source_path not in snapshot:
    if R.archived_at is null:
        ARCHIVE R (set archived_at=now(), archived_by=system user)
```

The `else if row.updated_at > F.Mtime` branch is the push-down path. After the daemon confirms the write, it reports the new mtime, which the server stores in `source_mtime` so the next heartbeat does not re-trigger.

### 7.2 Hook into existing `UpdateAgent`

`server/internal/handler/agent.go` — `UpdateAgent` handler: after the DB write succeeds, if `agent.source_path` is non-null, enqueue a `SubagentWrite` for the next heartbeat to that runtime. The existing optimistic-update / WS-broadcast paths are unchanged.

### 7.3 sqlc queries

Add to `server/pkg/db/queries/agent.sql`:

```sql
-- name: UpsertSyncedSubagent :one
INSERT INTO agent (
    workspace_id, runtime_id, name, description, instructions,
    runtime_mode, runtime_config, visibility, max_concurrent_tasks,
    owner_id, model, source_path, source_mtime, source_kind, custom_args
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
ON CONFLICT (runtime_id, source_path) WHERE source_path IS NOT NULL
DO UPDATE SET
    name         = EXCLUDED.name,
    description  = EXCLUDED.description,
    instructions = EXCLUDED.instructions,
    model        = EXCLUDED.model,
    custom_args  = EXCLUDED.custom_args,
    source_mtime = EXCLUDED.source_mtime,
    archived_at  = NULL,   -- restore on re-appearance
    archived_by  = NULL,
    updated_at   = now()
RETURNING *;

-- name: ListSyncedSubagentsByRuntime :many
SELECT * FROM agent
WHERE runtime_id = $1
  AND source_path IS NOT NULL
  AND source_kind = 'claude_subagent';

-- name: ArchiveOrphanSubagent :one
UPDATE agent SET archived_at = now(), archived_by = $2, updated_at = now()
WHERE id = $1 AND source_path IS NOT NULL AND archived_at IS NULL
RETURNING *;

-- name: UpdateSubagentSyncMtime :exec
UPDATE agent SET source_mtime = $2 WHERE id = $1;
```

Run `make sqlc` after editing.

## 8. Frontend Changes

### 8.1 Type extension

`packages/core/types/agent.ts` — extend `Agent`:

```ts
export interface Agent {
  // ...existing fields
  source_path?: string;       // absent when not synced
  source_kind?: "claude_subagent";
  source_mtime?: string;      // ISO timestamp
}
```

Schema parser in `packages/core/api/schema.ts` updated to accept the three new optional fields with safe fallbacks per the **API Response Compatibility** rules in CLAUDE.md.

### 8.2 UI surfaces

- `packages/views/agents/components/agent-detail-page.tsx`:
  - When `source_path` is set, render a `Badge` near the title: `Synced from ~/.claude/agents/<slug>.md`.
  - Disable the name field (slug = filename, renaming would require a file move outside scope).
  - Keep `instructions`, `model`, `description`, `tools` editable — those round-trip cleanly.
- `packages/views/agents/components/agents-page.tsx`:
  - New filter chip: `Claude Code subagents` (predicate: `source_kind === 'claude_subagent'`).
  - Existing assignee pickers untouched; subagents appear because they are ordinary agent rows.
- Runtime detail page (existing): add a small `Subagents (N)` link that scrolls to the filtered agents list. No new page.

### 8.3 No changes required

- Issue assignee picker.
- Task queue, presence, activity sparkline.
- Workspace dashboard usage charts.

## 9. Task Execution

`server/pkg/agent/claude.go` is **unchanged** in v1. The synced subagent looks identical to a Multica-native claude agent because:

- `instructions` is the system prompt body (already passed via `--append-system-prompt`).
- `custom_args` carries the tool whitelist (already forwarded to `claude` CLI).
- `model` and `thinking_level` already flow through.

If a future Claude Code CLI exposes a `--agent <slug>` form that consumes `~/.claude/agents/<slug>.md` directly, the implementation plan will gate switching to it on CLI version detection (the `version.go` pattern already used for other providers). v1 ships the system-prompt approach because it works against any current `claude` CLI.

## 10. Conflict Resolution

Last-write-wins by mtime:

- Server's view of "DB time" is `agent.updated_at`.
- Daemon's view of "file time" is the filesystem mtime.
- `source_mtime` is the daemon-reported mtime at the moment the row and the file were last known to be equal.

Decision matrix when the reconciler runs:

| `F.Mtime` vs `row.source_mtime` | `row.updated_at` vs `row.source_mtime` | Action            |
|---------------------------------|----------------------------------------|-------------------|
| equal                           | equal                                  | no-op             |
| newer                           | equal                                  | pull (DB ← file)  |
| equal                           | newer                                  | push (file ← DB)  |
| newer                           | newer                                  | **conflict → file wins** (`F.Mtime > row.updated_at`) or **DB wins** (otherwise). Tiebreak by latest. Loser's edits silently dropped. |

The double-newer case is the only data-loss path. It is documented as accepted in §3 (Non-Goals) and matched by the existing local-skills behaviour. A future iteration can add a "conflict detected, please resolve" inbox notification without breaking the schema.

## 11. Security

- `~/.claude/agents` may contain prompts that reference local file paths or secrets in plain text. Existing `agent.instructions` is workspace-visible to any member who can see the agent. Document this in the import confirmation modal: **"Subagent instructions become visible to every workspace member with access to this agent."**
- The daemon writes only inside `~/.claude/agents/` after validating the path is a direct child of that directory (no `..`, no symlink traversal). Reject any server-pushed path that fails this check and log a security warning.
- Frontmatter `tools` is an allow-list, not a deny-list. When converting to `--allowed-tools`, escape commas / shell metacharacters; treat the source as untrusted on the server even though it originated locally — the user may share a runtime with teammates whose machines wrote those files.
- The endpoint `POST /api/runtimes/{id}/subagents/sync` is owner/admin only (`requireRuntimeOwnerOrAdmin` middleware). Other members can read synced agents but cannot trigger a re-scan.

## 12. Testing

### Go

- `server/internal/daemon/local_subagents_test.go` — frontmatter round-trip (parse → write → re-parse identity), malformed YAML skipped with log, atomic write does not leak temp files on simulated crash, mtime preservation after `writeLocalSubagent`.
- `server/internal/handler/agent_sync_test.go` — every cell of the §10 matrix (no-op / pull / push / both-newer-file-wins / both-newer-db-wins), file-gone archives the row, file-reappear un-archives, multiple runtimes with same slug do not collide.
- `server/internal/handler/agent_test.go` — `UpdateAgent` on a row with `source_path` enqueues a write; on a row without it, no enqueue.

### TypeScript

- `packages/core/api/schema.test.ts` — agent response with `source_path` parses; without it still parses; malformed `source_mtime` falls back rather than throwing (per CLAUDE.md compatibility rules).
- `packages/views/agents/components/agent-detail-page.test.tsx` — synced badge renders only when `source_path` is set; name input is disabled when synced.
- `packages/views/agents/components/agents-page.test.tsx` — subagent filter chip narrows the list.

### Manual

- Create a file `~/.claude/agents/test-sync.md` → appears in Multica within ~5s.
- Edit in Multica → file body changes on disk, mtime advances.
- Edit on disk → Multica reflects within ~5s.
- Delete file → agent row archived; restoring file un-archives.
- Assign an issue to a synced subagent → task runs through `claude` provider end-to-end.

## 13. Migration / Rollout

- Migration is additive: existing `agent` rows get `NULL` in the three new columns and behave exactly as before. No data backfill needed.
- Feature gate: ship behind a workspace setting `subagent_sync_enabled` (default `false` for a release, then flip to `true`). Lets us catch frontmatter-parser edge cases on a small population before opening it up.
- Older desktop installs that have not learnt the three new agent fields parse-fallback them to undefined; the UI gracefully degrades (no badge, no disabled name field). This satisfies the **API Response Compatibility** rules in CLAUDE.md.

## 14. Open Questions Deferred to Implementation

- Exact `claude` CLI flag form (`--append-system-prompt` body vs `--agent <slug>` reference) — picked at implementation time based on the installed CLI version on the developer's machine.
- Whether to materialise unrecognised frontmatter into a visible "advanced" UI panel or keep it as opaque JSON in `runtime_config`. Default: opaque in v1.
- Watcher (fsnotify) for sub-heartbeat-latency updates. Documented as a future iteration.

---

## Appendix A — Affected Files

```
server/migrations/NNN_claude_subagent_sync.sql                          (new)
server/pkg/db/queries/agent.sql                                          (edit)
server/internal/daemon/local_subagents.go                                (new)
server/internal/daemon/local_subagents_test.go                           (new)
server/internal/daemon/daemon.go                                         (edit: heartbeat extension)
server/internal/daemon/client.go                                         (edit: report shape)
server/internal/handler/agent_sync.go                                    (new)
server/internal/handler/agent_sync_test.go                               (new)
server/internal/handler/agent.go                                         (edit: UpdateAgent enqueue)
server/internal/handler/router.go                                        (edit: register endpoint)
packages/core/types/agent.ts                                             (edit)
packages/core/api/schema.ts                                              (edit)
packages/views/agents/components/agent-detail-page.tsx                   (edit)
packages/views/agents/components/agent-detail-page.test.tsx              (edit)
packages/views/agents/components/agents-page.tsx                         (edit)
packages/views/agents/components/agents-page.test.tsx                    (edit)
```
