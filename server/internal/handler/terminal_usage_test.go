package handler

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/google/uuid"
)

// TestTerminalSessionUsageUpsertAndAggregate covers the two invariants the
// terminal-usage feature depends on:
//   - REPLACE-upsert: re-reporting cumulative totals for the same
//     (session, model) overwrites, never adds (idempotent under daemon re-reads).
//   - by-user aggregation sums across a user's sessions.
func TestTerminalSessionUsageUpsertAndAggregate(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("no database")
	}
	ctx := context.Background()
	q := testHandler.Queries

	userID := createWorkspaceMemberUser(t, "Terminal Usage", "terminal-usage@example.test")
	wsUUID := parseUUID(testWorkspaceID)
	userUUID := parseUUID(userID)

	sessionA := uuid.NewString()
	sessionB := uuid.NewString()
	t.Cleanup(func() {
		testPool.Exec(context.Background(),
			`DELETE FROM terminal_session_usage WHERE session_id = ANY($1::uuid[])`,
			[]string{sessionA, sessionB})
	})

	model := "claude-opus-4-8"

	// First cumulative report for session A.
	mustUpsertTerminalUsage(t, q, sessionA, wsUUID, userUUID, model, 10, 20, 100, 5)
	// Second report = later cumulative snapshot of the SAME session. REPLACE,
	// not add.
	mustUpsertTerminalUsage(t, q, sessionA, wsUUID, userUUID, model, 13, 27, 140, 6)
	// A second session for the same user.
	mustUpsertTerminalUsage(t, q, sessionB, wsUUID, userUUID, model, 1, 2, 0, 0)

	since := pgtype.Timestamptz{Time: time.Now().Add(-24 * time.Hour), Valid: true}
	rows, err := q.ListTerminalUsageByUser(ctx, db.ListTerminalUsageByUserParams{
		WorkspaceID: wsUUID,
		Since:       since,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	var row *db.ListTerminalUsageByUserRow
	for i := range rows {
		if uuidToString(rows[i].UserID) == userID {
			row = &rows[i]
			break
		}
	}
	if row == nil {
		t.Fatal("user not found in aggregation")
	}

	// session A replaced to (13,27,140,6); session B (1,2,0,0). Sums:
	if row.InputTokens != 14 || row.OutputTokens != 29 || row.CacheReadTokens != 140 || row.CacheWriteTokens != 6 {
		t.Fatalf("aggregate wrong: %+v", row)
	}
	if row.SessionCount != 2 {
		t.Fatalf("expected session_count 2, got %d", row.SessionCount)
	}
}

func mustUpsertTerminalUsage(t *testing.T, q *db.Queries, sessionID string, ws, user pgtype.UUID, model string, in, out, cacheRead, cacheWrite int64) {
	t.Helper()
	if err := q.UpsertTerminalSessionUsage(context.Background(), db.UpsertTerminalSessionUsageParams{
		SessionID:        parseUUID(sessionID),
		WorkspaceID:      ws,
		UserID:           user,
		Model:            model,
		InputTokens:      in,
		OutputTokens:     out,
		CacheReadTokens:  cacheRead,
		CacheWriteTokens: cacheWrite,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
}
