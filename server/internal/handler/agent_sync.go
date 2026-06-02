package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// reconcileSubagents matches a daemon-reported subagent snapshot against
// agent rows keyed on (runtime_id, source_path) and returns the set of
// pending writes the daemon should perform on its next heartbeat.
//
// Last-write-wins by mtime:
//   - file mtime newer than DB source_mtime → pull (DB ← file)
//   - DB updated_at newer than source_mtime → push (file ← DB)
//   - both newer → file wins iff file.Mtime > row.UpdatedAt (tiebreak)
//   - row in DB but not in snapshot → archive
//   - row appears with archived_at set → upsert un-archives (handled by SQL)
//
// Returns the deletes list always empty in v1: archive-on-missing only
// flips the DB flag — files that vanished are already gone from disk.
// A future "Multica-side archive should also wipe the file" flow will
// populate deletes; the wire path is reserved.
func (h *Handler) reconcileSubagents(
	ctx context.Context,
	runtimeID pgtype.UUID,
	provider string,
	snapshot []protocol.DaemonHeartbeatSubagentRow,
) ([]protocol.DaemonHeartbeatPendingSubagentWrite, []protocol.DaemonHeartbeatPendingSubagentDelete, error) {
	sourceKind := provider + "_subagent"

	rt, err := h.Queries.GetAgentRuntime(ctx, runtimeID)
	if err != nil {
		return nil, nil, err
	}

	existing, err := h.Queries.ListSyncedSubagentsByRuntime(ctx, db.ListSyncedSubagentsByRuntimeParams{
		RuntimeID:  runtimeID,
		SourceKind: strToText(sourceKind),
	})
	if err != nil {
		return nil, nil, err
	}
	byPath := make(map[string]db.Agent, len(existing))
	for _, a := range existing {
		byPath[a.SourcePath.String] = a
	}

	var writes []protocol.DaemonHeartbeatPendingSubagentWrite
	seen := make(map[string]struct{}, len(snapshot))

	for _, row := range snapshot {
		seen[row.Path] = struct{}{}
		fileMtime := time.Unix(row.MtimeUnix, 0).UTC()
		current, ok := byPath[row.Path]
		switch {
		case !ok:
			// New file → insert. After insert, pin source_mtime to the
			// row's updated_at so the next heartbeat does NOT see
			// updated_at > source_mtime and fire a spurious push that
			// would overwrite the on-disk file with whatever the row
			// happens to hold. Only a genuine UI edit (which advances
			// updated_at past source_mtime later) should ever trigger
			// the push path.
			inserted, err := h.Queries.UpsertSyncedSubagent(ctx, buildUpsertParams(rt, row, sourceKind, fileMtime))
			if err != nil {
				return nil, nil, err
			}
			if err := h.Queries.UpdateSubagentSyncMtime(ctx, db.UpdateSubagentSyncMtimeParams{
				ID:          inserted.ID,
				SourceMtime: inserted.UpdatedAt,
			}); err != nil {
				return nil, nil, err
			}
		case fileWins(fileMtime, current):
			updated, err := h.Queries.UpsertSyncedSubagent(ctx, buildUpsertParams(rt, row, sourceKind, fileMtime))
			if err != nil {
				return nil, nil, err
			}
			if err := h.Queries.UpdateSubagentSyncMtime(ctx, db.UpdateSubagentSyncMtimeParams{
				ID:          updated.ID,
				SourceMtime: updated.UpdatedAt,
			}); err != nil {
				return nil, nil, err
			}
		case dbWins(current):
			writes = append(writes, pendingWriteFromAgent(current))
			// Pin source_mtime to the current updated_at so the next
			// heartbeat does NOT re-emit the same push. Without this,
			// updated_at > source_mtime stays permanently true (the
			// daemon writes the file with mtime = updated_at.Unix(),
			// losing microsecond precision, so the file mtime can
			// never catch up to updated_at on its own) and the push
			// branch fires forever — wasted bandwidth, repeated disk
			// writes, and a never-true `in_sync` invariant.
			if err := h.Queries.UpdateSubagentSyncMtime(ctx, db.UpdateSubagentSyncMtimeParams{
				ID:          current.ID,
				SourceMtime: current.UpdatedAt,
			}); err != nil {
				return nil, nil, err
			}
		default:
			// Equal → no-op.
		}
	}

	for path, a := range byPath {
		if _, present := seen[path]; present {
			continue
		}
		_, err := h.Queries.ArchiveOrphanSubagent(ctx, db.ArchiveOrphanSubagentParams{
			ID:         a.ID,
			ArchivedBy: pgtype.UUID{}, // system actor — no user id
		})
		if err != nil {
			return nil, nil, err
		}
	}

	return writes, nil, nil
}

// fileWins reports whether the file's mtime should overwrite the DB row.
// Pull case: mtime newer than the last synced mtime AND newer than
// updated_at (so a same-tick UI edit isn't clobbered by a stale file).
func fileWins(fileMtime time.Time, row db.Agent) bool {
	if !fileMtime.After(row.SourceMtime.Time) {
		return false
	}
	if !fileMtime.After(row.UpdatedAt.Time) {
		return false
	}
	return true
}

// dbWins reports whether the DB row has unsaved-to-disk changes.
func dbWins(row db.Agent) bool {
	return row.UpdatedAt.Time.After(row.SourceMtime.Time)
}

func buildUpsertParams(rt db.AgentRuntime, row protocol.DaemonHeartbeatSubagentRow, sourceKind string, mtime time.Time) db.UpsertSyncedSubagentParams {
	// custom_args is stored as JSONB array of literal CLI args (the
	// daemon appends each element verbatim to the `claude` invocation).
	// Convert the frontmatter's `tools: [Read, Edit, ...]` list into a
	// single `--allowed-tools=<csv>` flag so Claude Code actually
	// enforces the whitelist — without this step the raw list elements
	// hit the CLI as positional args and get ignored, leaving the
	// agent with default-everything-allowed.
	var argsJSON []byte
	if len(row.Tools) > 0 {
		flag := "--allowed-tools=" + strings.Join(row.Tools, ",")
		argsJSON, _ = json.Marshal([]string{flag})
	} else {
		argsJSON = []byte("null")
	}
	return db.UpsertSyncedSubagentParams{
		WorkspaceID:        rt.WorkspaceID,
		RuntimeID:          rt.ID,
		Name:               truncate(row.Name, 255),
		Description:        truncate(row.Description, 255),
		Instructions:       row.Body,
		RuntimeMode:        rt.RuntimeMode,
		RuntimeConfig:      []byte("{}"),
		Visibility:         "workspace",
		MaxConcurrentTasks: 1,
		OwnerID:            rt.OwnerID,
		Model:              strToText(row.Model),
		CustomArgs:         argsJSON,
		SourcePath:         strToText(row.Path),
		SourceMtime:        pgtype.Timestamptz{Time: mtime, Valid: true},
		SourceKind:         strToText(sourceKind),
	}
}

func pendingWriteFromAgent(a db.Agent) protocol.DaemonHeartbeatPendingSubagentWrite {
	var tools []string
	if len(a.CustomArgs) > 0 && string(a.CustomArgs) != "null" {
		var args []string
		if err := json.Unmarshal(a.CustomArgs, &args); err == nil {
			for _, arg := range args {
				if strings.HasPrefix(arg, "--allowed-tools=") {
					csv := strings.TrimPrefix(arg, "--allowed-tools=")
					for _, t := range strings.Split(csv, ",") {
						t = strings.TrimSpace(t)
						if t != "" {
							tools = append(tools, t)
						}
					}
				}
			}
		}
	}
	return protocol.DaemonHeartbeatPendingSubagentWrite{
		ID:          uuidToString(a.ID),
		Path:        a.SourcePath.String,
		Name:        a.Name,
		Description: a.Description,
		Model:       a.Model.String,
		Tools:       tools,
		Body:        a.Instructions,
		MtimeUnix:   a.UpdatedAt.Time.Unix(),
	}
}

// PostRuntimeSubagentsSync is the manual-refresh trigger. v1 simply
// accepts the request — every heartbeat already carries a snapshot — so
// the endpoint exists for forward-compat (when we add snapshot debouncing
// daemon-side). Owner/admin only.
func (h *Handler) PostRuntimeSubagentsSync(w http.ResponseWriter, r *http.Request) {
	runtimeID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "runtimeId"), "runtime_id")
	if !ok {
		return
	}
	rt, err := h.Queries.GetAgentRuntime(r.Context(), runtimeID)
	if err != nil {
		writeError(w, http.StatusNotFound, "runtime not found")
		return
	}
	member, ok := h.requireWorkspaceMember(w, r, uuidToString(rt.WorkspaceID), "runtime not found")
	if !ok {
		return
	}
	if !canEditRuntime(member, rt) {
		writeError(w, http.StatusForbidden, "owner or admin only")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// logSubagentReconcileError centralises the warning the heartbeat handler
// emits when reconcile fails so the heartbeat itself can keep flowing.
func logSubagentReconcileError(runtimeID string, err error) {
	if err == nil || strings.Contains(err.Error(), "context canceled") {
		return
	}
	slog.Warn("subagent reconcile failed", "runtime_id", runtimeID, "err", err)
}

// truncate clips a string to <max> runes. Used to defensively cap the
// daemon-reported name/description before they hit the agent_*_length
// check constraints. Claude Code subagent frontmatter routinely carries
// descriptions well past 255 chars (the schema's existing cap for
// human-typed multica agents) — silently truncating beats refusing to
// import the row at all.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
