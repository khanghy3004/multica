package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// cleanupSyncedAgents wipes any agent rows seeded by reconcileSubagents into
// the shared test fixture's workspace so each test starts from a clean slate.
func cleanupSyncedAgents(t *testing.T) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`DELETE FROM agent WHERE workspace_id = $1 AND source_path IS NOT NULL`,
		testWorkspaceID,
	)
	if err != nil {
		t.Fatalf("cleanup synced agents: %v", err)
	}
}

func mustHaveTestRuntime(t *testing.T) {
	t.Helper()
	if testRuntimeID == "" {
		t.Skip("test runtime not initialised — DB skip")
	}
}

func TestReconcileSubagents_NewFileInsertsRow(t *testing.T) {
	mustHaveTestRuntime(t)
	defer cleanupSyncedAgents(t)

	runtimeUUID := parseUUID(testRuntimeID)
	now := time.Now().UTC().Truncate(time.Second)
	snapshot := []protocol.DaemonHeartbeatSubagentRow{{
		Path:        "/tmp/.claude/agents/insert.md",
		Slug:        "insert",
		Name:        "Insert Test",
		Description: "first appearance",
		Model:       "claude-opus-4-7",
		Tools:       []string{"Read", "Edit"},
		Body:        "system prompt body",
		MtimeUnix:   now.Unix(),
	}}
	writes, deletes, err := testHandler.reconcileSubagents(context.Background(), runtimeUUID, "claude", snapshot)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(writes) != 0 || len(deletes) != 0 {
		t.Errorf("expected no writes/deletes on fresh insert, got w=%d d=%d", len(writes), len(deletes))
	}

	var name, instructions string
	if err := testPool.QueryRow(context.Background(),
		`SELECT name, instructions FROM agent WHERE workspace_id = $1 AND source_path = $2`,
		testWorkspaceID, "/tmp/.claude/agents/insert.md",
	).Scan(&name, &instructions); err != nil {
		t.Fatalf("lookup inserted row: %v", err)
	}
	if name != "Insert Test" || instructions != "system prompt body" {
		t.Errorf("inserted row wrong: name=%q instructions=%q", name, instructions)
	}
}

func TestReconcileSubagents_FileNewerPullsToDB(t *testing.T) {
	mustHaveTestRuntime(t)
	defer cleanupSyncedAgents(t)

	runtimeUUID := parseUUID(testRuntimeID)
	old := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Second)
	// Seed.
	if _, _, err := testHandler.reconcileSubagents(context.Background(), runtimeUUID, "claude",
		[]protocol.DaemonHeartbeatSubagentRow{{
			Path: "/tmp/.claude/agents/pull.md", Slug: "pull", Name: "Old Name",
			Body: "old body", MtimeUnix: old.Unix(),
		}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Bump file mtime, change body. Also bump the row's updated_at
	// backwards so the file-newer check holds.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE agent SET updated_at = $2 WHERE source_path = $1`,
		"/tmp/.claude/agents/pull.md", old,
	); err != nil {
		t.Fatalf("rewind updated_at: %v", err)
	}
	newer := old.Add(30 * time.Minute)
	if _, _, err := testHandler.reconcileSubagents(context.Background(), runtimeUUID, "claude",
		[]protocol.DaemonHeartbeatSubagentRow{{
			Path: "/tmp/.claude/agents/pull.md", Slug: "pull", Name: "New Name",
			Body: "new body", MtimeUnix: newer.Unix(),
		}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var name, body string
	if err := testPool.QueryRow(context.Background(),
		`SELECT name, instructions FROM agent WHERE source_path = $1`,
		"/tmp/.claude/agents/pull.md",
	).Scan(&name, &body); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if name != "New Name" || body != "new body" {
		t.Errorf("pull lost: name=%q body=%q", name, body)
	}
}

func TestReconcileSubagents_DBNewerEmitsPushWrite(t *testing.T) {
	mustHaveTestRuntime(t)
	defer cleanupSyncedAgents(t)

	runtimeUUID := parseUUID(testRuntimeID)
	baseline := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Second)
	if _, _, err := testHandler.reconcileSubagents(context.Background(), runtimeUUID, "claude",
		[]protocol.DaemonHeartbeatSubagentRow{{
			Path: "/tmp/.claude/agents/push.md", Slug: "push", Name: "PushAgent",
			Body: "ui-edited body", MtimeUnix: baseline.Unix(),
		}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Simulate UI edit: bump updated_at past source_mtime.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE agent SET updated_at = $2, instructions = $3 WHERE source_path = $1`,
		"/tmp/.claude/agents/push.md", baseline.Add(30*time.Minute), "ui-edited body",
	); err != nil {
		t.Fatalf("simulate ui edit: %v", err)
	}

	writes, _, err := testHandler.reconcileSubagents(context.Background(), runtimeUUID, "claude",
		[]protocol.DaemonHeartbeatSubagentRow{{
			Path: "/tmp/.claude/agents/push.md", Slug: "push", Name: "PushAgent",
			Body: "ui-edited body", MtimeUnix: baseline.Unix(),
		}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(writes) != 1 {
		t.Fatalf("expected 1 push write, got %d", len(writes))
	}
	if writes[0].Path != "/tmp/.claude/agents/push.md" || writes[0].Body != "ui-edited body" {
		t.Errorf("unexpected write payload: %+v", writes[0])
	}
}

func TestReconcileSubagents_MissingFileArchives(t *testing.T) {
	mustHaveTestRuntime(t)
	defer cleanupSyncedAgents(t)

	runtimeUUID := parseUUID(testRuntimeID)
	now := time.Now().UTC().Truncate(time.Second)
	if _, _, err := testHandler.reconcileSubagents(context.Background(), runtimeUUID, "claude",
		[]protocol.DaemonHeartbeatSubagentRow{{
			Path: "/tmp/.claude/agents/gone.md", Slug: "gone", Name: "Gone",
			Body: "body", MtimeUnix: now.Unix(),
		}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Snapshot without the file.
	if _, _, err := testHandler.reconcileSubagents(context.Background(), runtimeUUID, "claude", nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var archived bool
	if err := testPool.QueryRow(context.Background(),
		`SELECT archived_at IS NOT NULL FROM agent WHERE source_path = $1`,
		"/tmp/.claude/agents/gone.md",
	).Scan(&archived); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !archived {
		t.Error("expected row to be archived after file disappeared")
	}
}

func TestReconcileSubagents_ReappearUnarchives(t *testing.T) {
	mustHaveTestRuntime(t)
	defer cleanupSyncedAgents(t)

	runtimeUUID := parseUUID(testRuntimeID)
	mt := time.Now().UTC().Truncate(time.Second)
	// Seed → archive → reappear.
	if _, _, err := testHandler.reconcileSubagents(context.Background(), runtimeUUID, "claude",
		[]protocol.DaemonHeartbeatSubagentRow{{
			Path: "/tmp/.claude/agents/reappear.md", Slug: "reappear", Name: "Reappear",
			Body: "first", MtimeUnix: mt.Unix(),
		}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := testHandler.reconcileSubagents(context.Background(), runtimeUUID, "claude", nil); err != nil {
		t.Fatalf("archive sweep: %v", err)
	}
	mt2 := mt.Add(1 * time.Minute)
	if _, _, err := testHandler.reconcileSubagents(context.Background(), runtimeUUID, "claude",
		[]protocol.DaemonHeartbeatSubagentRow{{
			Path: "/tmp/.claude/agents/reappear.md", Slug: "reappear", Name: "Reappear",
			Body: "first", MtimeUnix: mt2.Unix(),
		}}); err != nil {
		t.Fatalf("reappear: %v", err)
	}
	var archived bool
	if err := testPool.QueryRow(context.Background(),
		`SELECT archived_at IS NOT NULL FROM agent WHERE source_path = $1`,
		"/tmp/.claude/agents/reappear.md",
	).Scan(&archived); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if archived {
		t.Error("expected un-archive on reappear")
	}
}

func TestPostRuntimeSubagentsSync_Accepted(t *testing.T) {
	mustHaveTestRuntime(t)

	req := newRequest(http.MethodPost, "/api/runtimes/"+testRuntimeID+"/subagents/sync", nil)
	req = withURLParam(req, "runtimeId", testRuntimeID)
	w := httptest.NewRecorder()
	testHandler.PostRuntimeSubagentsSync(w, req)
	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202; body = %s", w.Code, w.Body.String())
	}
}
