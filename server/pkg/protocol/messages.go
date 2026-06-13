package protocol

import "encoding/json"

// Message is the envelope for all WebSocket messages.
type Message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// TaskDispatchPayload is sent from server to daemon when a task is assigned.
type TaskDispatchPayload struct {
	TaskID      string `json:"task_id"`
	IssueID     string `json:"issue_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// TaskAvailablePayload is sent from server to daemon as a wakeup hint. The
// daemon still claims work through the existing HTTP claim endpoint.
type TaskAvailablePayload struct {
	RuntimeID string `json:"runtime_id"`
	TaskID    string `json:"task_id,omitempty"`
}

// TaskProgressPayload is sent from daemon to server during task execution.
type TaskProgressPayload struct {
	TaskID  string `json:"task_id"`
	Summary string `json:"summary"`
	Step    int    `json:"step,omitempty"`
	Total   int    `json:"total,omitempty"`
}

// TaskCompletedPayload is sent from daemon to server when a task finishes.
type TaskCompletedPayload struct {
	TaskID string `json:"task_id"`
	PRURL  string `json:"pr_url,omitempty"`
	Output string `json:"output,omitempty"`
}

// TaskMessagePayload represents a single agent execution message (tool call, text, etc.)
type TaskMessagePayload struct {
	TaskID  string         `json:"task_id"`
	IssueID string         `json:"issue_id,omitempty"`
	Seq     int            `json:"seq"`
	Type    string         `json:"type"`              // "text", "tool_use", "tool_result", "error"
	Tool    string         `json:"tool,omitempty"`    // tool name for tool_use/tool_result
	Content string         `json:"content,omitempty"` // text content
	Input   map[string]any `json:"input,omitempty"`   // tool input (tool_use only)
	Output  string         `json:"output,omitempty"`  // tool output (tool_result only)
}

// DaemonRegisterPayload is sent from daemon to server on connection.
type DaemonRegisterPayload struct {
	DaemonID string        `json:"daemon_id"`
	AgentID  string        `json:"agent_id"`
	Runtimes []RuntimeInfo `json:"runtimes"`
}

// RuntimeInfo describes an available agent runtime on the daemon's machine.
type RuntimeInfo struct {
	Type    string `json:"type"`
	Version string `json:"version"`
	Status  string `json:"status"`
}

// ChatMessagePayload is broadcast when a new chat message is created.
type ChatMessagePayload struct {
	ChatSessionID string `json:"chat_session_id"`
	MessageID     string `json:"message_id"`
	Role          string `json:"role"`
	Content       string `json:"content"`
	TaskID        string `json:"task_id,omitempty"`
	CreatedAt     string `json:"created_at"`
}

// ChatDonePayload is broadcast when an agent finishes responding to a chat
// message. Carries the freshly-persisted assistant ChatMessage so the client
// can write it into the messages cache inline — avoids a refetch round-trip
// during the live-timeline → AssistantMessage handoff that previously caused
// a visible flicker (#2123).
type ChatDonePayload struct {
	ChatSessionID string `json:"chat_session_id"`
	TaskID        string `json:"task_id"`
	MessageID     string `json:"message_id,omitempty"`
	Content       string `json:"content,omitempty"`
	ElapsedMs     int64  `json:"elapsed_ms,omitempty"`
	CreatedAt     string `json:"created_at,omitempty"`
}

// ChatSessionReadPayload is broadcast when the creator marks a session as read.
// Fires to other devices so their unread counts stay in sync.
type ChatSessionReadPayload struct {
	ChatSessionID string `json:"chat_session_id"`
}

// ChatSessionDeletedPayload is broadcast when a chat session is hard-deleted
// so other tabs/devices drop it from their session lists and reset the active
// pointer if it referenced the deleted session.
type ChatSessionDeletedPayload struct {
	ChatSessionID string `json:"chat_session_id"`
}

// ChatSessionUpdatedPayload is broadcast when a user-editable field on a
// chat session changes (today: title via inline rename). Other tabs/devices
// patch the session row in their cached list so the dropdown stays in sync
// without a full refetch.
type ChatSessionUpdatedPayload struct {
	ChatSessionID string `json:"chat_session_id"`
	Title         string `json:"title"`
	UpdatedAt     string `json:"updated_at"`
}

// DaemonHeartbeatRequestPayload is sent from daemon to server over WebSocket
// to update last_seen_at and pull pending actions for a single runtime.
// Mirrors the body of POST /api/daemon/heartbeat so both transports share
// identical semantics.
type DaemonHeartbeatRequestPayload struct {
	RuntimeID           string                         `json:"runtime_id"`
	SupportsBatchImport bool                           `json:"supports_batch_import,omitempty"`
	SubagentReport      *DaemonHeartbeatSubagentReport `json:"subagent_report,omitempty"`
}

// DaemonHeartbeatAckPayload is the server's reply to DaemonHeartbeatRequestPayload.
// JSON shape mirrors the HTTP heartbeat response so daemon code can decode either.
//
// RuntimeGone is the WebSocket replacement for the HTTP 404 "runtime not found"
// response. When the server discovers the runtime row was deleted (UI delete,
// 7-day offline GC), it sends back an ack with Status=HeartbeatStatusRuntimeGone
// and RuntimeGone=true rather than tearing down the connection with an error.
// The daemon reads this signal, prunes the stale runtime from its local state
// and re-registers; without it the dead UUID would keep heartbeating until the
// daemon process restarts.
type DaemonHeartbeatAckPayload struct {
	RuntimeID               string                                  `json:"runtime_id"`
	Status                  string                                  `json:"status"`
	RuntimeGone             bool                                    `json:"runtime_gone,omitempty"`
	PendingUpdate           *DaemonHeartbeatPendingUpdate           `json:"pending_update,omitempty"`
	PendingModelList        *DaemonHeartbeatPendingModelList        `json:"pending_model_list,omitempty"`
	PendingLocalSkills      *DaemonHeartbeatPendingLocalSkills      `json:"pending_local_skills,omitempty"`
	PendingLocalSkillImport *DaemonHeartbeatPendingLocalSkillImport `json:"pending_local_skill_import,omitempty"`
	// PendingLocalSkillImports carries multiple import requests in a single
	// heartbeat so the daemon can process them concurrently. Old daemons
	// that don't know this field silently ignore it (standard JSON behavior)
	// and fall back to the singular PendingLocalSkillImport above.
	PendingLocalSkillImports []DaemonHeartbeatPendingLocalSkillImport `json:"pending_local_skill_imports,omitempty"`

	// PendingSubagentWrites tells the daemon to overwrite specific
	// ~/.claude/agents/<slug>.md files with server-authoritative content.
	// Used by the DB-newer-than-file branch of the subagent reconciler.
	PendingSubagentWrites []DaemonHeartbeatPendingSubagentWrite `json:"pending_subagent_writes,omitempty"`
	// PendingSubagentDeletes tells the daemon to remove subagent files
	// whose DB row was archived (e.g. by an admin UI action).
	PendingSubagentDeletes []DaemonHeartbeatPendingSubagentDelete `json:"pending_subagent_deletes,omitempty"`
}

// HeartbeatStatusRuntimeGone is the ack Status used when the runtime row no
// longer exists server-side. Companion to DaemonHeartbeatAckPayload.RuntimeGone.
const HeartbeatStatusRuntimeGone = "runtime_gone"

// DaemonHeartbeatPendingUpdate describes a CLI-update action the daemon
// should run for the runtime.
type DaemonHeartbeatPendingUpdate struct {
	ID            string `json:"id"`
	TargetVersion string `json:"target_version"`
}

// DaemonHeartbeatPendingModelList describes a request for the daemon to
// enumerate the runtime's supported models.
type DaemonHeartbeatPendingModelList struct {
	ID string `json:"id"`
}

// DaemonHeartbeatPendingLocalSkills describes a request for the runtime's
// local-skill inventory.
type DaemonHeartbeatPendingLocalSkills struct {
	ID string `json:"id"`
}

// DaemonHeartbeatPendingLocalSkillImport describes a request to import a
// specific runtime local skill.
type DaemonHeartbeatPendingLocalSkillImport struct {
	ID       string `json:"id"`
	SkillKey string `json:"skill_key"`
}

// DaemonHeartbeatSubagentReport is the per-runtime snapshot the daemon
// attaches to its heartbeat. The server uses this to reconcile rows in
// the `agent` table keyed on (runtime_id, source_path).
type DaemonHeartbeatSubagentReport struct {
	// Provider that produced the snapshot (e.g. "claude"). Server stores
	// "<provider>_subagent" in agent.source_kind.
	Provider  string                       `json:"provider"`
	Subagents []DaemonHeartbeatSubagentRow `json:"subagents"`
}

// DaemonHeartbeatSubagentRow is one entry in the heartbeat snapshot.
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

// DaemonHeartbeatPendingSubagentWrite is a push-down instruction from
// the server: rewrite this file with the given content + frontmatter
// and stamp it with the given mtime.
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

// DaemonHeartbeatPendingSubagentDelete asks the daemon to remove a
// subagent file whose DB row was archived.
type DaemonHeartbeatPendingSubagentDelete struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// --- Terminal relay payloads ---------------------------------------------
//
// SessionID is minted server-side when a browser opens a terminal and scopes
// every subsequent frame so one daemon WS connection can multiplex several
// terminals. Output bytes ride as base64 in Data to stay binary-safe through
// the JSON envelope (control sequences, partial UTF-8 runes).

// TerminalOpenPayload (server → daemon) asks the daemon to spawn the
// interactive `claude` CLI in a fresh PTY for SessionID.
//
// The PTY is HARD-CONFINED to the daemon's per-workspace folder
// (WorkspacesRoot/<workspace_id>): the client supplies only the workspace_id
// and the daemon builds the path itself. There is intentionally no free-form
// cwd or provider field — the terminal always runs `claude`, always inside the
// workspace folder, and the client cannot point it anywhere else.
type TerminalOpenPayload struct {
	SessionID   string `json:"session_id"`
	WorkspaceID string `json:"workspace_id"`
	// UserID is the human who opened the terminal. The daemon tags the
	// session's token usage with it so the Usage page can attribute terminal
	// spend per user.
	UserID string `json:"user_id"`
	Cols   uint16 `json:"cols"`
	Rows   uint16 `json:"rows"`
}

// TerminalStdinPayload (server → daemon) forwards browser keystrokes.
type TerminalStdinPayload struct {
	SessionID string `json:"session_id"`
	Data      string `json:"data"` // base64-encoded input bytes
}

// TerminalResizePayload (server → daemon) updates the PTY window size.
type TerminalResizePayload struct {
	SessionID string `json:"session_id"`
	Cols      uint16 `json:"cols"`
	Rows      uint16 `json:"rows"`
}

// TerminalClosePayload (server → daemon) tears a PTY down (browser closed the
// tab or the session was revoked).
type TerminalClosePayload struct {
	SessionID string `json:"session_id"`
}

// TerminalStdoutPayload (daemon → server) carries a chunk of PTY output.
type TerminalStdoutPayload struct {
	SessionID string `json:"session_id"`
	Data      string `json:"data"` // base64-encoded output bytes
}

// TerminalExitPayload (daemon → server) reports the process exited.
type TerminalExitPayload struct {
	SessionID string `json:"session_id"`
	Code      int    `json:"code"`
}

// TerminalErrorPayload (daemon → server) reports a spawn or IO failure so the
// browser can surface it before the session closes.
type TerminalErrorPayload struct {
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
}
