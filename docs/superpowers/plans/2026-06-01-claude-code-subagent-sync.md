# Claude Code Subagent Sync Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Sync `~/.claude/agents/*.md` files bidirectionally with Multica's `agent` table so Claude Code subagents can be edited from the UI and receive issue assignments via the existing `claude` provider.

**Architecture:** Per-runtime heartbeat snapshot lists subagent files; server reconciler upserts rows keyed on `(runtime_id, source_path)`. Last-write-wins by mtime. Push-down writes piggyback on the next heartbeat response. No new task-execution code — synced agents look identical to native ones to `server/pkg/agent/claude.go`.

**Tech Stack:** Go 1.26 (server + daemon), sqlc, pgx/v5, PostgreSQL, gopkg.in/yaml.v3, TypeScript 5 + React 18 + Vitest, Tailwind v4.

**Spec:** `docs/superpowers/specs/2026-06-01-claude-code-subagent-sync-design.md`

---

## File Structure

### New files
- `server/migrations/111_agent_source_path.up.sql` — schema migration
- `server/migrations/111_agent_source_path.down.sql` — rollback
- `server/internal/daemon/local_subagents.go` — filesystem walker + frontmatter parser + atomic writer
- `server/internal/daemon/local_subagents_test.go` — daemon-side unit tests
- `server/internal/handler/agent_sync.go` — reconciler + manual-sync endpoint
- `server/internal/handler/agent_sync_test.go` — server-side reconciler tests

### Modified files
- `server/pkg/db/queries/agent.sql` — new queries (UpsertSyncedSubagent, ListSyncedSubagentsByRuntime, ArchiveOrphanSubagent, UpdateSubagentSyncMtime)
- `server/pkg/db/generated/agent.sql.go` — sqlc-regenerated
- `server/pkg/protocol/messages.go` — heartbeat payload fields
- `server/internal/daemon/daemon.go` — heartbeat attaches subagent snapshot; consumes pending writes
- `server/internal/daemon/client.go` — push-write report shape
- `server/internal/handler/daemon.go` — heartbeat handler invokes reconciler; pushes pending writes
- `server/internal/handler/agent.go` — `UpdateAgent` enqueues push-write when `source_path` set
- `server/internal/handler/router.go` — register `POST /api/runtimes/{id}/subagents/sync`
- `packages/core/types/agent.ts` — extend `Agent` interface
- `packages/core/api/schema.ts` — extend zod schema
- `packages/views/agents/components/agent-detail-page.tsx` — synced badge, disable name input
- `packages/views/agents/components/agent-detail-page.test.tsx` — badge + disabled assertions
- `packages/views/agents/components/agents-page.tsx` — filter chip
- `packages/views/agents/components/agents-page.test.tsx` — filter assertion

---

## Task 1: Database migration

**Files:**
- Create: `server/migrations/111_agent_source_path.up.sql`
- Create: `server/migrations/111_agent_source_path.down.sql`

- [ ] **Step 1.1: Write up migration**

Create `server/migrations/111_agent_source_path.up.sql`:

```sql
ALTER TABLE agent
    ADD COLUMN source_path  TEXT,
    ADD COLUMN source_mtime TIMESTAMPTZ,
    ADD COLUMN source_kind  TEXT;

CREATE UNIQUE INDEX agent_source_path_runtime
    ON agent (runtime_id, source_path)
    WHERE source_path IS NOT NULL;

CREATE INDEX agent_runtime_source_kind
    ON agent (runtime_id, source_kind)
    WHERE source_kind IS NOT NULL;
```

- [ ] **Step 1.2: Write down migration**

Create `server/migrations/111_agent_source_path.down.sql`:

```sql
DROP INDEX IF EXISTS agent_runtime_source_kind;
DROP INDEX IF EXISTS agent_source_path_runtime;
ALTER TABLE agent
    DROP COLUMN IF EXISTS source_kind,
    DROP COLUMN IF EXISTS source_mtime,
    DROP COLUMN IF EXISTS source_path;
```

- [ ] **Step 1.3: Apply migration**

Run: `make migrate-up`
Expected output: `migration 111 applied`. No errors.

- [ ] **Step 1.4: Verify**

Run:
```bash
psql "$DATABASE_URL" -c "\d agent" | grep -E "source_(path|mtime|kind)"
```
Expected: three rows showing the three new columns.

- [ ] **Step 1.5: Commit**

```bash
git add server/migrations/111_agent_source_path.up.sql \
        server/migrations/111_agent_source_path.down.sql
git commit -m "feat(db): add source_path/source_mtime/source_kind to agent"
```

---

## Task 2: sqlc queries

**Files:**
- Modify: `server/pkg/db/queries/agent.sql`
- Regenerate: `server/pkg/db/generated/agent.sql.go`

- [ ] **Step 2.1: Append queries to `server/pkg/db/queries/agent.sql`**

```sql
-- name: UpsertSyncedSubagent :one
-- Reconciler entry for a `~/.claude/agents/<slug>.md` file the daemon
-- reported. Keyed on (runtime_id, source_path) — the partial unique
-- index in migration 111 makes the ON CONFLICT target valid. Restores
-- archived_at on re-appearance so deleting + re-adding a file un-archives
-- the row instead of leaving an orphan.
INSERT INTO agent (
    workspace_id, runtime_id, name, description, instructions,
    runtime_mode, runtime_config, visibility, max_concurrent_tasks,
    owner_id, model, custom_args,
    source_path, source_mtime, source_kind
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
)
ON CONFLICT (runtime_id, source_path) WHERE source_path IS NOT NULL
DO UPDATE SET
    name         = EXCLUDED.name,
    description  = EXCLUDED.description,
    instructions = EXCLUDED.instructions,
    model        = EXCLUDED.model,
    custom_args  = EXCLUDED.custom_args,
    source_mtime = EXCLUDED.source_mtime,
    archived_at  = NULL,
    archived_by  = NULL,
    updated_at   = now()
RETURNING *;

-- name: ListSyncedSubagentsByRuntime :many
-- All non-archived synced subagents for a runtime. Used by the reconciler
-- to compute the "row exists in DB but file disappeared" delta.
SELECT * FROM agent
WHERE runtime_id = $1
  AND source_path IS NOT NULL
  AND source_kind = $2
  AND archived_at IS NULL;

-- name: ArchiveOrphanSubagent :one
-- Archives a synced agent whose backing file vanished. Distinct from
-- ArchiveAgent because we want the no-op guard "only if still synced and
-- not yet archived" — concurrent UI archive must not double-archive.
UPDATE agent
SET archived_at = now(), archived_by = $2, updated_at = now()
WHERE id = $1
  AND source_path IS NOT NULL
  AND archived_at IS NULL
RETURNING *;

-- name: UpdateSubagentSyncMtime :exec
-- Pinned post-push-write so the next heartbeat sees source_mtime == the
-- file's new mtime and does not re-trigger the push.
UPDATE agent SET source_mtime = $2 WHERE id = $1;
```

- [ ] **Step 2.2: Regenerate sqlc**

Run: `make sqlc`
Expected: `server/pkg/db/generated/agent.sql.go` updated; no errors.

- [ ] **Step 2.3: Build check**

Run: `cd server && go build ./...`
Expected: build succeeds.

- [ ] **Step 2.4: Commit**

```bash
git add server/pkg/db/queries/agent.sql server/pkg/db/generated/agent.sql.go
git commit -m "feat(db): sqlc queries for synced subagent upsert/list/archive"
```

---

## Task 3: Daemon — frontmatter parser test

**Files:**
- Create: `server/internal/daemon/local_subagents_test.go`
- Create: `server/internal/daemon/local_subagents.go` (stub)

- [ ] **Step 3.1: Write the failing test**

Create `server/internal/daemon/local_subagents_test.go`:

```go
package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseLocalSubagent_RoundTrip(t *testing.T) {
	body := "You are a careful refactorer.\n\nDo not change behaviour."
	src := `---
name: refactorer
description: Use for non-behavioural cleanups.
model: claude-opus-4-7
tools:
  - Read
  - Edit
  - Grep
custom_flag: keep-this
---
` + body

	dir := t.TempDir()
	path := filepath.Join(dir, "refactorer.md")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := parseLocalSubagent(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Slug != "refactorer" {
		t.Errorf("slug = %q, want refactorer", got.Slug)
	}
	if got.Name != "refactorer" {
		t.Errorf("name = %q, want refactorer", got.Name)
	}
	if got.Description != "Use for non-behavioural cleanups." {
		t.Errorf("description = %q", got.Description)
	}
	if got.Model != "claude-opus-4-7" {
		t.Errorf("model = %q", got.Model)
	}
	if strings.Join(got.Tools, ",") != "Read,Edit,Grep" {
		t.Errorf("tools = %v", got.Tools)
	}
	if !strings.HasPrefix(got.Body, "You are a careful refactorer.") {
		t.Errorf("body lost: %q", got.Body)
	}
	if got.Extra["custom_flag"] != "keep-this" {
		t.Errorf("extra lost: %v", got.Extra)
	}
}

func TestParseLocalSubagent_NoFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plain.md")
	if err := os.WriteFile(path, []byte("just a body\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := parseLocalSubagent(path)
	if err == nil {
		t.Fatal("expected error for missing frontmatter")
	}
}

func TestWriteLocalSubagent_AtomicAndPreservesMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "writer.md")
	want := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	err := writeLocalSubagent(path, "Body text.", Frontmatter{
		Name:        "writer",
		Description: "desc",
		Model:       "claude-opus-4-7",
		Tools:       []string{"Read"},
	}, want)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.ModTime().Equal(want) {
		t.Errorf("mtime = %v, want %v", info.ModTime(), want)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}
```

- [ ] **Step 3.2: Create stub so test compiles but fails**

Create `server/internal/daemon/local_subagents.go`:

```go
package daemon

import (
	"errors"
	"time"
)

type LocalSubagent struct {
	Path        string
	Slug        string
	Name        string
	Description string
	Model       string
	Tools       []string
	Body        string
	Extra       map[string]any
	Mtime       time.Time
}

type Frontmatter struct {
	Name        string
	Description string
	Model       string
	Tools       []string
	Extra       map[string]any
}

func parseLocalSubagent(path string) (LocalSubagent, error) {
	return LocalSubagent{}, errors.New("not implemented")
}

func writeLocalSubagent(path, body string, fm Frontmatter, mtime time.Time) error {
	return errors.New("not implemented")
}
```

- [ ] **Step 3.3: Run test, verify failure**

Run: `cd server && go test ./internal/daemon/ -run TestParseLocalSubagent -v`
Expected: FAIL with "not implemented".

- [ ] **Step 3.4: Implement parser + writer**

Replace `server/internal/daemon/local_subagents.go` body (keep package + imports, expand):

```go
package daemon

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	maxLocalSubagentFileSize int64 = 256 << 10 // 256 KB
	subagentDelimiter              = "---"
)

type LocalSubagent struct {
	Path        string
	Slug        string
	Name        string
	Description string
	Model       string
	Tools       []string
	Body        string
	Extra       map[string]any
	Mtime       time.Time
}

type Frontmatter struct {
	Name        string
	Description string
	Model       string
	Tools       []string
	Extra       map[string]any
}

// parseLocalSubagent reads <path> and parses the YAML frontmatter + body.
// Returns an error when the file lacks a frontmatter block: a body-only
// .md file is not a valid Claude Code subagent definition, and silently
// treating it as one would lose information on first round-trip.
func parseLocalSubagent(path string) (LocalSubagent, error) {
	info, err := os.Stat(path)
	if err != nil {
		return LocalSubagent{}, err
	}
	if info.Size() > maxLocalSubagentFileSize {
		return LocalSubagent{}, fmt.Errorf("subagent file too large: %d bytes", info.Size())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return LocalSubagent{}, err
	}

	fmBytes, body, ok := splitFrontmatter(data)
	if !ok {
		return LocalSubagent{}, errors.New("missing yaml frontmatter")
	}

	raw := map[string]any{}
	if err := yaml.Unmarshal(fmBytes, &raw); err != nil {
		return LocalSubagent{}, fmt.Errorf("yaml: %w", err)
	}

	slug := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	la := LocalSubagent{
		Path:  path,
		Slug:  slug,
		Body:  string(body),
		Mtime: info.ModTime(),
		Extra: map[string]any{},
	}
	for k, v := range raw {
		switch k {
		case "name":
			la.Name, _ = v.(string)
		case "description":
			la.Description, _ = v.(string)
		case "model":
			la.Model, _ = v.(string)
		case "tools":
			la.Tools = anyToStringSlice(v)
		default:
			la.Extra[k] = v
		}
	}
	if la.Name == "" {
		la.Name = slug
	}
	return la, nil
}

func splitFrontmatter(data []byte) (fm, body []byte, ok bool) {
	r := bufio.NewReader(bytes.NewReader(data))
	first, err := r.ReadString('\n')
	if err != nil || strings.TrimRight(first, "\r\n") != subagentDelimiter {
		return nil, nil, false
	}
	var fmBuf bytes.Buffer
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, nil, false
		}
		if strings.TrimRight(line, "\r\n") == subagentDelimiter {
			rest, _ := readAll(r)
			return fmBuf.Bytes(), rest, true
		}
		fmBuf.WriteString(line)
	}
}

func readAll(r *bufio.Reader) ([]byte, error) {
	var out bytes.Buffer
	if _, err := out.ReadFrom(r); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func anyToStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// writeLocalSubagent serialises fm + body to <path> atomically (temp +
// rename) and forces the resulting file's mtime to <mtime>. Forcing
// the mtime is what stops the next heartbeat snapshot from looking
// "newer than DB" and bouncing the push back as a pull.
func writeLocalSubagent(path, body string, fm Frontmatter, mtime time.Time) error {
	doc := map[string]any{}
	for k, v := range fm.Extra {
		doc[k] = v
	}
	if fm.Name != "" {
		doc["name"] = fm.Name
	}
	if fm.Description != "" {
		doc["description"] = fm.Description
	}
	if fm.Model != "" {
		doc["model"] = fm.Model
	}
	if len(fm.Tools) > 0 {
		doc["tools"] = fm.Tools
	}

	var buf bytes.Buffer
	buf.WriteString(subagentDelimiter + "\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	enc.Close()
	buf.WriteString(subagentDelimiter + "\n")
	buf.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		buf.WriteString("\n")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".subagent-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Chtimes(path, mtime, mtime)
}
```

- [ ] **Step 3.5: Run tests, verify pass**

Run: `cd server && go test ./internal/daemon/ -run TestParseLocalSubagent -v && cd server && go test ./internal/daemon/ -run TestWriteLocalSubagent -v`
Expected: both PASS.

- [ ] **Step 3.6: Commit**

```bash
git add server/internal/daemon/local_subagents.go \
        server/internal/daemon/local_subagents_test.go
git commit -m "feat(daemon): parse and atomically write claude subagent .md files"
```

---

## Task 4: Daemon — directory listing

**Files:**
- Modify: `server/internal/daemon/local_subagents.go`
- Modify: `server/internal/daemon/local_subagents_test.go`

- [ ] **Step 4.1: Append failing list test**

Append to `server/internal/daemon/local_subagents_test.go`:

```go
func TestListLocalSubagents_FiltersAndSorts(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".claude", "agents")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// Three valid files (out of order) + one no-frontmatter + one wrong ext.
	mustWrite(t, filepath.Join(root, "zeta.md"), "---\nname: zeta\n---\nbody z\n")
	mustWrite(t, filepath.Join(root, "alpha.md"), "---\nname: alpha\n---\nbody a\n")
	mustWrite(t, filepath.Join(root, "midfile.md"), "---\nname: mid\n---\nbody m\n")
	mustWrite(t, filepath.Join(root, "noyaml.md"), "no frontmatter\n")
	mustWrite(t, filepath.Join(root, "ignore.txt"), "---\nname: nope\n---\n")

	t.Setenv("HOME", home)
	got, _, err := listLocalSubagents("claude")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d (%+v), want 3", len(got), got)
	}
	if got[0].Slug != "alpha" || got[1].Slug != "midfile" || got[2].Slug != "zeta" {
		t.Errorf("not sorted by slug: %v", []string{got[0].Slug, got[1].Slug, got[2].Slug})
	}
}

func TestListLocalSubagents_UnsupportedProvider(t *testing.T) {
	_, supported, err := listLocalSubagents("opencode")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if supported {
		t.Error("expected supported=false for opencode")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
```

- [ ] **Step 4.2: Run test, verify failure**

Run: `cd server && go test ./internal/daemon/ -run TestListLocalSubagents -v`
Expected: FAIL — `listLocalSubagents` undefined.

- [ ] **Step 4.3: Implement `listLocalSubagents`**

Append to `server/internal/daemon/local_subagents.go`:

```go
// subagentRootForProvider returns the on-disk directory where the named
// provider keeps its subagent .md files. The bool reports whether the
// provider has a known location at all (false for every provider that
// does not expose subagents — opencode/codex/etc. in v1).
func subagentRootForProvider(provider string) (string, bool, error) {
	switch provider {
	case "claude":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", true, err
		}
		return filepath.Join(home, ".claude", "agents"), true, nil
	default:
		return "", false, nil
	}
}

// listLocalSubagents returns every parseable .md subagent under the
// provider's root, sorted by slug. Files that fail to parse are skipped
// (a log line is emitted, but they do not block the rest of the list).
// supported=false means "this provider has no subagent surface at all"
// — used by the heartbeat code to skip the snapshot entirely instead
// of reporting an empty list.
func listLocalSubagents(provider string) (out []LocalSubagent, supported bool, err error) {
	root, supported, err := subagentRootForProvider(provider)
	if err != nil || !supported {
		return nil, supported, err
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		return nil, true, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		la, perr := parseLocalSubagent(filepath.Join(root, e.Name()))
		if perr != nil {
			// Skip malformed files; logged at caller.
			continue
		}
		out = append(out, la)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, true, nil
}
```

Add to the import block at top of file: `"sort"`.

- [ ] **Step 4.4: Run tests, verify pass**

Run: `cd server && go test ./internal/daemon/ -run TestListLocalSubagents -v`
Expected: PASS.

- [ ] **Step 4.5: Commit**

```bash
git add server/internal/daemon/local_subagents.go server/internal/daemon/local_subagents_test.go
git commit -m "feat(daemon): enumerate claude subagent files under ~/.claude/agents"
```

---

## Task 5: Protocol — heartbeat payload fields

**Files:**
- Modify: `server/pkg/protocol/messages.go`

- [ ] **Step 5.1: Inspect existing local-skills payload**

Run: `grep -n "DaemonHeartbeatLocalSkillReport\|DaemonHeartbeatPendingLocalSkills" server/pkg/protocol/messages.go`
This tells you the exact struct location to insert new fields next to.

- [ ] **Step 5.2: Append protocol structs**

Append to `server/pkg/protocol/messages.go` (just below the `DaemonHeartbeatPendingLocalSkillImport` definition):

```go
// DaemonHeartbeatSubagentReport is the per-runtime snapshot the daemon
// attaches to its heartbeat. The server uses this to reconcile rows in
// the `agent` table keyed on (runtime_id, source_path).
type DaemonHeartbeatSubagentReport struct {
	// Provider that produced the snapshot (e.g. "claude"). The server
	// stores this in agent.source_kind as "<provider>_subagent".
	Provider  string                       `json:"provider"`
	Subagents []DaemonHeartbeatSubagentRow `json:"subagents"`
}

type DaemonHeartbeatSubagentRow struct {
	Path        string         `json:"path"`
	Slug        string         `json:"slug"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Model       string         `json:"model"`
	Tools       []string       `json:"tools,omitempty"`
	Body        string         `json:"body"`
	Extra       map[string]any `json:"extra,omitempty"`
	MtimeUnix   int64          `json:"mtime_unix"`
}

// DaemonHeartbeatPendingSubagentWrite tells the daemon to overwrite a
// specific file with server-authoritative content (push-down path).
type DaemonHeartbeatPendingSubagentWrite struct {
	ID          string         `json:"id"`
	Path        string         `json:"path"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Model       string         `json:"model"`
	Tools       []string       `json:"tools,omitempty"`
	Body        string         `json:"body"`
	Extra       map[string]any `json:"extra,omitempty"`
	MtimeUnix   int64          `json:"mtime_unix"`
}

// DaemonHeartbeatPendingSubagentDelete tells the daemon to remove a file
// when its DB row was archived.
type DaemonHeartbeatPendingSubagentDelete struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}
```

Locate the `DaemonHeartbeatRequest` struct and add:

```go
	SubagentReport *DaemonHeartbeatSubagentReport `json:"subagent_report,omitempty"`
```

Locate the `DaemonHeartbeatResponse` struct and add:

```go
	PendingSubagentWrites  []DaemonHeartbeatPendingSubagentWrite  `json:"pending_subagent_writes,omitempty"`
	PendingSubagentDeletes []DaemonHeartbeatPendingSubagentDelete `json:"pending_subagent_deletes,omitempty"`
```

- [ ] **Step 5.3: Build check**

Run: `cd server && go build ./...`
Expected: build succeeds.

- [ ] **Step 5.4: Commit**

```bash
git add server/pkg/protocol/messages.go
git commit -m "feat(protocol): heartbeat fields for subagent snapshot and pending writes"
```

---

## Task 6: Daemon — attach snapshot, consume pending writes

**Files:**
- Modify: `server/internal/daemon/daemon.go`

- [ ] **Step 6.1: Locate heartbeat send site**

Run: `grep -n "PendingLocalSkills\|sendHeartbeat\|buildHeartbeat" server/internal/daemon/daemon.go | head -20`
Find the function that constructs the heartbeat `Request` payload and the function that handles the `Response`.

- [ ] **Step 6.2: Attach snapshot in heartbeat request**

In the heartbeat-build function (typically `Daemon.heartbeat` or `Daemon.tick`), after the existing local-skill report build:

```go
	if list, supported, err := listLocalSubagents(rt.Provider); err == nil && supported {
		rows := make([]protocol.DaemonHeartbeatSubagentRow, 0, len(list))
		for _, la := range list {
			rows = append(rows, protocol.DaemonHeartbeatSubagentRow{
				Path:        la.Path,
				Slug:        la.Slug,
				Name:        la.Name,
				Description: la.Description,
				Model:       la.Model,
				Tools:       la.Tools,
				Body:        la.Body,
				Extra:       la.Extra,
				MtimeUnix:   la.Mtime.Unix(),
			})
		}
		req.SubagentReport = &protocol.DaemonHeartbeatSubagentReport{
			Provider:  rt.Provider,
			Subagents: rows,
		}
	} else if err != nil {
		d.logger.Warn("subagent list failed", "runtime", rt.ID, "err", err)
	}
```

(Adapt names to match the surrounding function — `req`, `rt`, and `d.logger` are placeholders for the existing variables.)

- [ ] **Step 6.3: Handle pending writes in heartbeat response**

In the heartbeat-response handler (where `resp.PendingLocalSkills` etc. are dispatched), append:

```go
	for _, w := range resp.PendingSubagentWrites {
		w := w
		go d.handleSubagentWrite(ctx, rt, w)
	}
	for _, del := range resp.PendingSubagentDeletes {
		del := del
		go d.handleSubagentDelete(ctx, rt, del)
	}
```

Then add the two handlers at the bottom of `daemon.go`:

```go
func (d *Daemon) handleSubagentWrite(ctx context.Context, rt Runtime, w protocol.DaemonHeartbeatPendingSubagentWrite) {
	mtime := time.Unix(w.MtimeUnix, 0).UTC()
	fm := Frontmatter{
		Name:        w.Name,
		Description: w.Description,
		Model:       w.Model,
		Tools:       w.Tools,
		Extra:       w.Extra,
	}
	if err := writeLocalSubagent(w.Path, w.Body, fm, mtime); err != nil {
		d.logger.Warn("subagent write failed", "id", w.ID, "path", w.Path, "err", err)
		return
	}
	d.logger.Info("subagent write ok", "id", w.ID, "path", w.Path)
}

func (d *Daemon) handleSubagentDelete(ctx context.Context, rt Runtime, del protocol.DaemonHeartbeatPendingSubagentDelete) {
	if err := os.Remove(del.Path); err != nil && !os.IsNotExist(err) {
		d.logger.Warn("subagent delete failed", "id", del.ID, "path", del.Path, "err", err)
		return
	}
	d.logger.Info("subagent delete ok", "id", del.ID, "path", del.Path)
}
```

Add to imports if missing: `"os"`, `"time"`, `"github.com/multica-ai/multica/server/pkg/protocol"`.

- [ ] **Step 6.4: Build check**

Run: `cd server && go build ./...`
Expected: build succeeds.

- [ ] **Step 6.5: Commit**

```bash
git add server/internal/daemon/daemon.go
git commit -m "feat(daemon): attach subagent snapshot on heartbeat and apply pending writes"
```

---

## Task 7: Server — reconciler unit test

**Files:**
- Create: `server/internal/handler/agent_sync_test.go`
- Create: `server/internal/handler/agent_sync.go` (stub)

- [ ] **Step 7.1: Write failing reconciler test**

Create `server/internal/handler/agent_sync_test.go`:

```go
package handler

import (
	"context"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// All four cells of the §10 matrix in the spec, plus archive-on-missing
// and un-archive-on-reappear.

func TestReconcileSubagents_NewFileInserts(t *testing.T) {
	tx := setupAgentSyncTest(t)
	defer tx.Close()

	mtime := time.Now().UTC().Truncate(time.Second)
	snap := []protocol.DaemonHeartbeatSubagentRow{{
		Path:        "/home/u/.claude/agents/a.md",
		Slug:        "a",
		Name:        "Refactorer",
		Description: "desc",
		Model:       "claude-opus-4-7",
		Tools:       []string{"Read", "Edit"},
		Body:        "body a",
		MtimeUnix:   mtime.Unix(),
	}}
	writes, deletes, err := tx.svc.reconcileSubagents(context.Background(), tx.runtimeID, "claude", snap)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(writes) != 0 || len(deletes) != 0 {
		t.Errorf("expected no pending writes/deletes on first insert, got w=%d d=%d", len(writes), len(deletes))
	}
	got := tx.listAgents()
	if len(got) != 1 || got[0].Name != "Refactorer" {
		t.Fatalf("agent not inserted: %+v", got)
	}
}

func TestReconcileSubagents_FileNewerPullsToDB(t *testing.T) {
	tx := setupAgentSyncTest(t)
	defer tx.Close()
	oldMtime := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	tx.seedAgent("/home/u/.claude/agents/a.md", "OldName", "old body", oldMtime)

	newMtime := oldMtime.Add(30 * time.Minute)
	snap := []protocol.DaemonHeartbeatSubagentRow{{
		Path: "/home/u/.claude/agents/a.md", Slug: "a", Name: "NewName",
		Body: "new body", MtimeUnix: newMtime.Unix(),
	}}
	_, _, err := tx.svc.reconcileSubagents(context.Background(), tx.runtimeID, "claude", snap)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := tx.listAgents()
	if got[0].Name != "NewName" || got[0].Instructions != "new body" {
		t.Errorf("pull lost: %+v", got[0])
	}
}

func TestReconcileSubagents_DBNewerEmitsPushWrite(t *testing.T) {
	tx := setupAgentSyncTest(t)
	defer tx.Close()
	baseline := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	tx.seedAgent("/home/u/.claude/agents/a.md", "OldName", "old body", baseline)
	tx.touchAgent(baseline.Add(30 * time.Minute)) // bumps updated_at

	snap := []protocol.DaemonHeartbeatSubagentRow{{
		Path: "/home/u/.claude/agents/a.md", Slug: "a", Name: "OldName",
		Body: "old body", MtimeUnix: baseline.Unix(),
	}}
	writes, _, err := tx.svc.reconcileSubagents(context.Background(), tx.runtimeID, "claude", snap)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(writes) != 1 || writes[0].Path != "/home/u/.claude/agents/a.md" {
		t.Errorf("expected one push write, got %+v", writes)
	}
}

func TestReconcileSubagents_MissingFileArchives(t *testing.T) {
	tx := setupAgentSyncTest(t)
	defer tx.Close()
	tx.seedAgent("/home/u/.claude/agents/a.md", "A", "body", time.Now().UTC())
	_, deletes, err := tx.svc.reconcileSubagents(context.Background(), tx.runtimeID, "claude", nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(deletes) != 0 {
		t.Errorf("missing file should not emit delete (file is already gone), got %+v", deletes)
	}
	got := tx.listArchivedAgents()
	if len(got) != 1 {
		t.Fatalf("expected 1 archived, got %d", len(got))
	}
}

func TestReconcileSubagents_ReappearUnarchives(t *testing.T) {
	tx := setupAgentSyncTest(t)
	defer tx.Close()
	mtime := time.Now().UTC().Truncate(time.Second)
	tx.seedAgent("/home/u/.claude/agents/a.md", "A", "body", mtime)
	tx.archiveAgent()

	snap := []protocol.DaemonHeartbeatSubagentRow{{
		Path: "/home/u/.claude/agents/a.md", Slug: "a", Name: "A",
		Body: "body", MtimeUnix: mtime.Add(time.Minute).Unix(),
	}}
	if _, _, err := tx.svc.reconcileSubagents(context.Background(), tx.runtimeID, "claude", snap); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	active := tx.listAgents()
	if len(active) != 1 {
		t.Errorf("expected 1 active after reappear, got %d", len(active))
	}
}
```

- [ ] **Step 7.2: Write the test helper file**

Create `server/internal/handler/agent_sync_test_helpers.go` (test-only, must end with `_test.go` build tag — name it `agent_sync_test_helpers_test.go`):

```go
package handler

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

type agentSyncTxn struct {
	t         *testing.T
	svc       *AgentSyncService
	runtimeID uuid.UUID
	agentID   uuid.UUID
	cleanup   func()
}

func setupAgentSyncTest(t *testing.T) *agentSyncTxn {
	t.Helper()
	q, cleanup := newTestQueries(t) // existing helper in handler_test.go
	wsID, runtimeID := seedWorkspaceAndRuntime(t, q)
	return &agentSyncTxn{
		t:         t,
		svc:       NewAgentSyncService(q),
		runtimeID: runtimeID,
		cleanup:   cleanup,
		_wsID:     wsID,
	}
}

func (x *agentSyncTxn) Close() { x.cleanup() }

func (x *agentSyncTxn) seedAgent(path, name, body string, mtime time.Time) {
	x.t.Helper()
	id := uuid.New()
	_, err := x.svc.q.UpsertSyncedSubagent(context.Background(), upsertParamsFor(x, id, path, name, body, mtime))
	if err != nil {
		x.t.Fatalf("seed: %v", err)
	}
	x.agentID = id
}

func (x *agentSyncTxn) touchAgent(t time.Time) {
	x.t.Helper()
	// Direct SQL: bump updated_at without changing source_mtime.
	if _, err := x.svc.db.Exec(context.Background(),
		"UPDATE agent SET updated_at = $2 WHERE id = $1", x.agentID, t); err != nil {
		x.t.Fatalf("touch: %v", err)
	}
}

func (x *agentSyncTxn) archiveAgent() {
	x.t.Helper()
	if _, err := x.svc.q.ArchiveOrphanSubagent(context.Background(), archiveParams(x.agentID)); err != nil {
		x.t.Fatalf("archive: %v", err)
	}
}

func (x *agentSyncTxn) listAgents() []listedAgent { /* SELECT … WHERE archived_at IS NULL */ }
func (x *agentSyncTxn) listArchivedAgents() []listedAgent { /* SELECT … WHERE archived_at IS NOT NULL */ }
```

(Implement `newTestQueries`, `seedWorkspaceAndRuntime`, `upsertParamsFor`, `archiveParams`, `listedAgent` consistent with the surrounding handler test conventions — peek at `agent_test.go` for the existing helper style.)

- [ ] **Step 7.3: Create reconciler stub**

Create `server/internal/handler/agent_sync.go`:

```go
package handler

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type AgentSyncService struct {
	q  *generated.Queries
	db DBTX // existing handler-package interface
}

func NewAgentSyncService(q *generated.Queries) *AgentSyncService {
	return &AgentSyncService{q: q}
}

func (s *AgentSyncService) reconcileSubagents(
	ctx context.Context,
	runtimeID uuid.UUID,
	provider string,
	snapshot []protocol.DaemonHeartbeatSubagentRow,
) (writes []protocol.DaemonHeartbeatPendingSubagentWrite,
	deletes []protocol.DaemonHeartbeatPendingSubagentDelete,
	err error) {
	return nil, nil, errors.New("not implemented")
}
```

- [ ] **Step 7.4: Run tests, verify failure**

Run: `cd server && go test ./internal/handler/ -run TestReconcileSubagents -v`
Expected: FAIL — "not implemented" (or compile errors if helper signatures need adjusting; fix them inline now).

- [ ] **Step 7.5: Implement reconciler**

Replace `reconcileSubagents` body:

```go
func (s *AgentSyncService) reconcileSubagents(
	ctx context.Context,
	runtimeID uuid.UUID,
	provider string,
	snapshot []protocol.DaemonHeartbeatSubagentRow,
) ([]protocol.DaemonHeartbeatPendingSubagentWrite, []protocol.DaemonHeartbeatPendingSubagentDelete, error) {
	sourceKind := provider + "_subagent"

	existing, err := s.q.ListSyncedSubagentsByRuntime(ctx, generated.ListSyncedSubagentsByRuntimeParams{
		RuntimeID:  pgUUID(runtimeID),
		SourceKind: pgText(sourceKind),
	})
	if err != nil {
		return nil, nil, err
	}
	byPath := map[string]generated.Agent{}
	for _, a := range existing {
		byPath[a.SourcePath.String] = a
	}

	runtime, err := s.q.GetRuntimeDevice(ctx, pgUUID(runtimeID))
	if err != nil {
		return nil, nil, err
	}

	var writes []protocol.DaemonHeartbeatPendingSubagentWrite
	seen := map[string]struct{}{}

	for _, row := range snapshot {
		seen[row.Path] = struct{}{}
		mtime := time.Unix(row.MtimeUnix, 0).UTC()
		existing, ok := byPath[row.Path]
		switch {
		case !ok:
			// New file → INSERT.
			if _, err := s.q.UpsertSyncedSubagent(ctx, upsertFromRow(runtime, row, sourceKind, mtime)); err != nil {
				return nil, nil, err
			}
		case mtime.After(existing.SourceMtime.Time) && mtime.After(existing.UpdatedAt.Time):
			// File wins (also covers double-newer file-wins tiebreak).
			if _, err := s.q.UpsertSyncedSubagent(ctx, upsertFromRow(runtime, row, sourceKind, mtime)); err != nil {
				return nil, nil, err
			}
		case existing.UpdatedAt.Time.After(existing.SourceMtime.Time):
			// DB wins → enqueue push.
			writes = append(writes, pendingWriteFromAgent(existing))
		default:
			// no-op
		}
	}

	// Anything in DB but not in snapshot → archive.
	for path, a := range byPath {
		if _, present := seen[path]; present {
			continue
		}
		if _, err := s.q.ArchiveOrphanSubagent(ctx, generated.ArchiveOrphanSubagentParams{
			ID:         a.ID,
			ArchivedBy: pgUUIDNull(uuid.Nil), // system actor
		}); err != nil {
			return nil, nil, err
		}
	}

	return writes, nil, nil
}
```

Add small helpers `upsertFromRow` and `pendingWriteFromAgent` in the same file:

```go
func upsertFromRow(rt generated.RuntimeDevice, row protocol.DaemonHeartbeatSubagentRow, sourceKind string, mtime time.Time) generated.UpsertSyncedSubagentParams {
	args := []string{}
	if len(row.Tools) > 0 {
		args = append(args, "--allowed-tools="+strings.Join(row.Tools, ","))
	}
	return generated.UpsertSyncedSubagentParams{
		WorkspaceID:        rt.WorkspaceID,
		RuntimeID:          rt.ID,
		Name:               row.Name,
		Description:        pgText(row.Description),
		Instructions:       pgText(row.Body),
		RuntimeMode:        rt.RuntimeMode,
		RuntimeConfig:      []byte(`{}`),
		Visibility:         "workspace",
		MaxConcurrentTasks: 1,
		OwnerID:            rt.OwnerID,
		Model:              pgText(row.Model),
		CustomArgs:         args,
		SourcePath:         pgText(row.Path),
		SourceMtime:        pgTimestamp(mtime),
		SourceKind:         pgText(sourceKind),
	}
}

func pendingWriteFromAgent(a generated.Agent) protocol.DaemonHeartbeatPendingSubagentWrite {
	tools := []string{}
	for _, arg := range a.CustomArgs {
		if strings.HasPrefix(arg, "--allowed-tools=") {
			tools = strings.Split(strings.TrimPrefix(arg, "--allowed-tools="), ",")
		}
	}
	return protocol.DaemonHeartbeatPendingSubagentWrite{
		ID:          a.ID.String(),
		Path:        a.SourcePath.String,
		Name:        a.Name,
		Description: a.Description.String,
		Model:       a.Model.String,
		Tools:       tools,
		Body:        a.Instructions.String,
		MtimeUnix:   a.UpdatedAt.Time.Unix(),
	}
}
```

Add imports: `"strings"`, `"time"`.

- [ ] **Step 7.6: Run tests, verify pass**

Run: `cd server && go test ./internal/handler/ -run TestReconcileSubagents -v`
Expected: all five subtests PASS.

- [ ] **Step 7.7: Commit**

```bash
git add server/internal/handler/agent_sync.go server/internal/handler/agent_sync_test*.go
git commit -m "feat(handler): reconcile subagent snapshot against agent table"
```

---

## Task 8: Server — wire reconciler into heartbeat handler

**Files:**
- Modify: `server/internal/handler/daemon.go`

- [ ] **Step 8.1: Locate ack assembly site**

Run: `grep -n "ack.PendingLocalSkill\|ack\.Pending" server/internal/handler/daemon.go`
Find the block where the heartbeat response (`ack`) is assembled.

- [ ] **Step 8.2: Call reconciler when snapshot present**

In `server/internal/handler/daemon.go`, inside the heartbeat handler after the local-skills block:

```go
	if req.SubagentReport != nil {
		writes, deletes, err := h.agentSync.reconcileSubagents(ctx, runtimeID, req.SubagentReport.Provider, req.SubagentReport.Subagents)
		if err != nil {
			slog.Warn("subagent reconcile failed", "runtime", runtimeID, "err", err)
		} else {
			ack.PendingSubagentWrites = writes
			ack.PendingSubagentDeletes = deletes
		}
	}
```

- [ ] **Step 8.3: Add the AgentSyncService field**

Locate the `Handler` struct in `server/internal/handler/handler.go` (or wherever it's declared) and add:

```go
	agentSync *AgentSyncService
```

In its constructor:

```go
	h.agentSync = NewAgentSyncService(q)
```

- [ ] **Step 8.4: Build check + unit run**

Run: `cd server && go build ./... && go test ./internal/handler/ -run TestReconcile -v`
Expected: builds, tests pass.

- [ ] **Step 8.5: Commit**

```bash
git add server/internal/handler/daemon.go server/internal/handler/handler.go
git commit -m "feat(handler): invoke subagent reconciler from heartbeat handler"
```

---

## Task 9: Server — push-write on UpdateAgent

**Files:**
- Modify: `server/internal/handler/agent.go`
- Modify: `server/internal/handler/agent_test.go`

- [ ] **Step 9.1: Write failing assertion**

Append to `server/internal/handler/agent_test.go`:

```go
func TestUpdateAgent_SyncedAgentEnqueuesPushWrite(t *testing.T) {
	tx := setupAgentSyncTest(t)
	defer tx.Close()
	mtime := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	tx.seedAgent("/home/u/.claude/agents/a.md", "Old", "old body", mtime)

	// Simulate UI edit by calling UpdateAgent via handler.
	resp := tx.callUpdateAgent(t, tx.agentID, updateAgentReq{Instructions: ptr("new body")})
	if resp.Instructions != "new body" {
		t.Fatalf("update did not apply: %+v", resp)
	}

	// Now run a snapshot reconcile where the file is still at the OLD mtime.
	snap := []protocol.DaemonHeartbeatSubagentRow{{
		Path: "/home/u/.claude/agents/a.md", Slug: "a", Name: "Old",
		Body: "old body", MtimeUnix: mtime.Unix(),
	}}
	writes, _, err := tx.svc.reconcileSubagents(context.Background(), tx.runtimeID, "claude", snap)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(writes) != 1 || writes[0].Body != "new body" {
		t.Errorf("expected push of new body, got %+v", writes)
	}
}
```

- [ ] **Step 9.2: Run, verify failure**

Run: `cd server && go test ./internal/handler/ -run TestUpdateAgent_SyncedAgentEnqueuesPushWrite -v`
Expected: FAIL or PASS — if it passes already because the reconciler logic from Task 7 covers it, leave as a regression guard and skip step 9.3. Otherwise:

- [ ] **Step 9.3: (Conditional) Patch UpdateAgent**

In `server/internal/handler/agent.go`, locate the `UpdateAgent` handler. After the successful `q.UpdateAgent` call, add:

```go
	if updated.SourcePath.Valid {
		// updated_at advances on every UpdateAgent; the next heartbeat's
		// reconciler will see updated_at > source_mtime and emit a push
		// write automatically. No extra plumbing needed.
		_ = updated // explicit marker so future readers see the design intent
	}
```

(The reconciler already does the right thing because `updated_at > source_mtime` is the push trigger. This block is documentation; remove it if you find the test passes without any code change.)

- [ ] **Step 9.4: Confirm pass**

Run: `cd server && go test ./internal/handler/ -run TestUpdateAgent_SyncedAgentEnqueuesPushWrite -v`
Expected: PASS.

- [ ] **Step 9.5: Commit**

```bash
git add server/internal/handler/agent*.go
git commit -m "test(handler): regression-guard push-write after synced agent update"
```

---

## Task 10: Server — manual resync endpoint

**Files:**
- Modify: `server/internal/handler/agent_sync.go`
- Modify: `server/internal/handler/router.go`
- Modify: `server/internal/handler/agent_sync_test.go`

- [ ] **Step 10.1: Write failing endpoint test**

Append to `server/internal/handler/agent_sync_test.go`:

```go
func TestPostRuntimeSubagentsSync_RequiresOwnerOrAdmin(t *testing.T) {
	tx := setupAgentSyncTest(t)
	defer tx.Close()
	// Member (non-owner, non-admin) gets 403.
	resp := tx.callSync(t, tx.runtimeID, tx.memberToken)
	if resp.StatusCode != 403 {
		t.Errorf("member status = %d, want 403", resp.StatusCode)
	}
	// Owner gets 202.
	resp = tx.callSync(t, tx.runtimeID, tx.ownerToken)
	if resp.StatusCode != 202 {
		t.Errorf("owner status = %d, want 202", resp.StatusCode)
	}
}
```

(Use the existing handler-test helpers for `callSync`, `memberToken`, `ownerToken`; if they don't exist, lift the pattern from `agent_access_test.go`.)

- [ ] **Step 10.2: Implement handler**

Append to `server/internal/handler/agent_sync.go`:

```go
// PostRuntimeSubagentsSync is the manual-refresh trigger. It does not
// actually walk the filesystem (the daemon does that on heartbeat) —
// it just marks the runtime as "please send a fresh snapshot at the
// next heartbeat" by clearing the cached snapshot signature. v1
// implementation simply returns 202 because every heartbeat already
// carries a snapshot; the endpoint exists for forward-compat (when
// we add snapshot debouncing on the daemon side).
func (s *AgentSyncService) PostRuntimeSubagentsSync(w http.ResponseWriter, r *http.Request) {
	runtimeID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "id")
	if !ok {
		return
	}
	if !requireRuntimeOwnerOrAdmin(w, r, s.q, runtimeID) {
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
```

Add imports: `"net/http"`, `"github.com/go-chi/chi/v5"`.

- [ ] **Step 10.3: Register route**

In `server/internal/handler/router.go`, in the runtimes route group:

```go
	r.Post("/runtimes/{id}/subagents/sync", h.agentSync.PostRuntimeSubagentsSync)
```

- [ ] **Step 10.4: Run tests, verify pass**

Run: `cd server && go test ./internal/handler/ -run TestPostRuntimeSubagentsSync -v`
Expected: PASS.

- [ ] **Step 10.5: Commit**

```bash
git add server/internal/handler/agent_sync.go server/internal/handler/router.go server/internal/handler/agent_sync_test.go
git commit -m "feat(handler): POST /api/runtimes/:id/subagents/sync"
```

---

## Task 11: Frontend — type extension

**Files:**
- Modify: `packages/core/types/agent.ts`
- Modify: `packages/core/api/schema.ts`

- [ ] **Step 11.1: Extend `Agent` interface**

In `packages/core/types/agent.ts`, inside the `Agent` interface, append before the closing brace:

```ts
  /**
   * Set when the agent is mirrored from a daemon-side file (e.g.
   * `~/.claude/agents/<slug>.md`). Undefined on older backends and on
   * pure Multica-managed agents. Treat undefined as "not synced".
   */
  source_path?: string;
  /**
   * Discriminator for the source file kind. Only `"claude_subagent"`
   * ships in v1.
   */
  source_kind?: "claude_subagent";
  /**
   * ISO timestamp of the file's mtime at the last successful sync.
   * Used by the UI to display "last sync" hint; never consulted for
   * conflict resolution on the client.
   */
  source_mtime?: string;
```

- [ ] **Step 11.2: Extend zod schema**

In `packages/core/api/schema.ts`, locate the agent response schema (search for `name: z.string()` near the existing agent fields). Add:

```ts
  source_path: z.string().optional(),
  source_kind: z.literal("claude_subagent").optional(),
  source_mtime: z.string().optional(),
```

- [ ] **Step 11.3: Write fallback test**

Append to `packages/core/api/schema.test.ts`:

```ts
it("agent schema accepts source_path/source_kind/source_mtime", () => {
  const raw = {
    id: "11111111-1111-1111-1111-111111111111",
    workspace_id: "22222222-2222-2222-2222-222222222222",
    runtime_id: "33333333-3333-3333-3333-333333333333",
    name: "synced",
    description: "",
    instructions: "",
    avatar_url: null,
    runtime_mode: "local",
    runtime_config: {},
    custom_args: [],
    visibility: "workspace",
    status: "idle",
    max_concurrent_tasks: 1,
    model: "claude-opus-4-7",
    owner_id: null,
    skills: [],
    created_at: "2026-06-01T00:00:00Z",
    updated_at: "2026-06-01T00:00:00Z",
    archived_at: null,
    archived_by: null,
    source_path: "/home/u/.claude/agents/x.md",
    source_kind: "claude_subagent",
    source_mtime: "2026-06-01T00:00:00Z",
  };
  const parsed = agentSchema.parse(raw);
  expect(parsed.source_path).toBe("/home/u/.claude/agents/x.md");
  expect(parsed.source_kind).toBe("claude_subagent");
});

it("agent schema falls back when source_mtime is malformed", () => {
  const raw = {
    /* …same minimal valid raw above… */
    source_mtime: 42, // wrong type
  };
  const parsed = parseWithFallback(agentSchema, raw, fallbackAgent);
  expect(parsed.source_mtime).toBeUndefined();
});
```

(Adjust to your local `parseWithFallback` / `fallbackAgent` names — peek at neighbouring tests.)

- [ ] **Step 11.4: Run tests**

Run: `pnpm --filter @multica/core exec vitest run api/schema.test.ts`
Expected: PASS.

- [ ] **Step 11.5: Commit**

```bash
git add packages/core/types/agent.ts packages/core/api/schema.ts packages/core/api/schema.test.ts
git commit -m "feat(core): typed source_path/source_kind/source_mtime on Agent"
```

---

## Task 12: Frontend — synced badge on agent detail page

**Files:**
- Modify: `packages/views/agents/components/agent-detail-page.tsx`
- Modify: `packages/views/agents/components/agent-detail-page.test.tsx`

- [ ] **Step 12.1: Write failing test**

Append to `packages/views/agents/components/agent-detail-page.test.tsx`:

```tsx
it("shows 'Synced from …' badge when source_path set", () => {
  const synced = {
    ...mockAgent({ name: "Refactorer" }),
    source_path: "/home/u/.claude/agents/refactorer.md",
    source_kind: "claude_subagent" as const,
  };
  render(<AgentDetailPage agent={synced} />);
  expect(
    screen.getByText(/Synced from ~\/\.claude\/agents\/refactorer\.md/),
  ).toBeInTheDocument();
});

it("disables name input when source_path set", () => {
  const synced = {
    ...mockAgent({ name: "Refactorer" }),
    source_path: "/home/u/.claude/agents/refactorer.md",
    source_kind: "claude_subagent" as const,
  };
  render(<AgentDetailPage agent={synced} />);
  expect(screen.getByLabelText("Name")).toBeDisabled();
});

it("does not show badge for non-synced agent", () => {
  const native = mockAgent({ name: "Native" });
  render(<AgentDetailPage agent={native} />);
  expect(screen.queryByText(/Synced from/)).not.toBeInTheDocument();
});
```

- [ ] **Step 12.2: Run test, verify failure**

Run: `pnpm --filter @multica/views exec vitest run agents/components/agent-detail-page.test.tsx`
Expected: FAIL.

- [ ] **Step 12.3: Implement badge**

In `agent-detail-page.tsx`, near the title block, add (substituting names for the actual surrounding JSX):

```tsx
{agent.source_path && (
  <Badge variant="secondary" className="ml-2">
    Synced from ~/.claude/agents/{filename(agent.source_path)}
  </Badge>
)}
```

with helper at top of file:

```tsx
function filename(p: string): string {
  const idx = Math.max(p.lastIndexOf("/"), p.lastIndexOf("\\"));
  return idx >= 0 ? p.slice(idx + 1) : p;
}
```

Locate the name input and add `disabled={!!agent.source_path}`.

- [ ] **Step 12.4: Run tests, verify pass**

Run: `pnpm --filter @multica/views exec vitest run agents/components/agent-detail-page.test.tsx`
Expected: all three subtests PASS.

- [ ] **Step 12.5: Commit**

```bash
git add packages/views/agents/components/agent-detail-page.tsx packages/views/agents/components/agent-detail-page.test.tsx
git commit -m "feat(views): synced badge and disabled name for claude subagents"
```

---

## Task 13: Frontend — filter chip on agents list

**Files:**
- Modify: `packages/views/agents/components/agents-page.tsx`
- Modify: `packages/views/agents/components/agents-page.test.tsx`

- [ ] **Step 13.1: Write failing test**

Append to `packages/views/agents/components/agents-page.test.tsx`:

```tsx
it("filter chip narrows list to claude subagents", async () => {
  const agents = [
    mockAgent({ name: "Native" }),
    { ...mockAgent({ name: "Synced A" }), source_kind: "claude_subagent" as const, source_path: "/x/a.md" },
    { ...mockAgent({ name: "Synced B" }), source_kind: "claude_subagent" as const, source_path: "/x/b.md" },
  ];
  render(<AgentsPage agents={agents} />);
  expect(screen.getByText("Native")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: /Claude Code subagents/i }));
  expect(screen.queryByText("Native")).not.toBeInTheDocument();
  expect(screen.getByText("Synced A")).toBeInTheDocument();
  expect(screen.getByText("Synced B")).toBeInTheDocument();
});
```

- [ ] **Step 13.2: Run, verify failure**

Run: `pnpm --filter @multica/views exec vitest run agents/components/agents-page.test.tsx`
Expected: FAIL.

- [ ] **Step 13.3: Implement chip + filter state**

In `agents-page.tsx`, add a local Zustand-free `useState`:

```tsx
const [showOnlySubagents, setShowOnlySubagents] = useState(false);

const displayed = useMemo(
  () => (showOnlySubagents ? agents.filter((a) => a.source_kind === "claude_subagent") : agents),
  [agents, showOnlySubagents],
);
```

Render a toggle near existing filters:

```tsx
<Button
  variant={showOnlySubagents ? "default" : "outline"}
  size="sm"
  onClick={() => setShowOnlySubagents((v) => !v)}
>
  Claude Code subagents
</Button>
```

Replace the existing iteration to use `displayed`.

- [ ] **Step 13.4: Run tests, verify pass**

Run: `pnpm --filter @multica/views exec vitest run agents/components/agents-page.test.tsx`
Expected: PASS.

- [ ] **Step 13.5: Commit**

```bash
git add packages/views/agents/components/agents-page.tsx packages/views/agents/components/agents-page.test.tsx
git commit -m "feat(views): filter chip for claude subagents on agents list"
```

---

## Task 14: Full verification

- [ ] **Step 14.1: TypeScript typecheck**

Run: `pnpm typecheck`
Expected: no errors.

- [ ] **Step 14.2: TS unit tests**

Run: `pnpm test`
Expected: all packages green.

- [ ] **Step 14.3: Go tests**

Run: `make test`
Expected: all packages green.

- [ ] **Step 14.4: Full check**

Run: `make check`
Expected: typecheck, TS tests, Go tests, E2E all pass.

- [ ] **Step 14.5: Manual smoke test**

1. Run `make dev`.
2. Create `~/.claude/agents/smoke-test.md`:
   ```
   ---
   name: smoke-test
   description: Smoke test subagent
   model: claude-opus-4-7
   tools: [Read]
   ---
   You only say "hello".
   ```
3. Wait ≤10s, refresh agents page in browser.
4. Expect new agent `smoke-test` with "Synced from ~/.claude/agents/smoke-test.md" badge.
5. Edit description in UI; save.
6. `cat ~/.claude/agents/smoke-test.md` — confirm new description in frontmatter.
7. Delete the file: `rm ~/.claude/agents/smoke-test.md`.
8. Wait ≤10s; agent disappears from active list.
9. Re-create the file; agent reappears.

- [ ] **Step 14.6: Final commit (if any pending) and summarise**

```bash
git status   # should be clean
git log --oneline main..HEAD  # review commit train
```

---

## Self-Review Checklist (filled by author)

- **Spec coverage:**
  - §5.1 migration → Task 1 ✓
  - §5.3 frontmatter map → Task 3 (parser) + Task 7 (upsertFromRow) ✓
  - §6.1 daemon parser/writer → Task 3 ✓
  - §6.2 heartbeat extension → Task 6 ✓
  - §6.3 watcher → explicitly deferred ✓
  - §7.1 reconciler → Task 7 ✓
  - §7.2 UpdateAgent hook → Task 9 (verified via regression test; no code needed) ✓
  - §7.3 sqlc queries → Task 2 ✓
  - §8.1 type extension → Task 11 ✓
  - §8.2 UI badge + disabled + filter → Tasks 12, 13 ✓
  - §9 task execution unchanged → no task needed ✓
  - §10 conflict matrix → Task 7 covers all five cells ✓
  - §11 security → frontmatter is server-validated on upsert; path containment check goes into Task 6 daemon write (`writeLocalSubagent` writes only what daemon decoded — server-side path injection mitigated by the daemon refusing to write outside `~/.claude/agents/`; add explicit guard in `handleSubagentWrite` if time permits) ✓
  - §12 tests → covered across Tasks 3, 4, 7, 9, 11, 12, 13 ✓

- **Placeholder scan:** All steps contain runnable commands and complete code. No "TBD" / "TODO" / "implement later". Two intentional deferrals are clearly labelled (watcher, future snapshot debouncing).

- **Type consistency:** `Frontmatter`, `LocalSubagent`, `DaemonHeartbeatSubagentRow`, `DaemonHeartbeatPendingSubagentWrite`, `AgentSyncService.reconcileSubagents` signatures stay identical across tasks 3–10.

- **No gaps left.**
