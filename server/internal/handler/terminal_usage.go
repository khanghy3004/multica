package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TerminalUsageReport is the daemon-reported CUMULATIVE per-(session, model)
// token totals for an interactive terminal claude session. The daemon parses
// the session transcript and re-reports the running totals; the backend stores
// them with REPLACE semantics so repeated reports (including after a daemon
// restart re-reads the whole transcript) are idempotent.
type TerminalUsageReport struct {
	SessionID        string `json:"session_id"`
	WorkspaceID      string `json:"workspace_id"`
	UserID           string `json:"user_id"`
	Model            string `json:"model"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
}

// ReportTerminalUsage ingests a daemon terminal-usage report.
// POST /api/daemon/terminal-usage (DaemonAuth). The user_id originates from the
// terminal:open frame the backend itself sent (set from the authenticated
// cookie user), so it is trustworthy; we additionally verify the user is a
// member of the workspace as a defensive integrity check.
func (h *Handler) ReportTerminalUsage(w http.ResponseWriter, r *http.Request) {
	var req TerminalUsageReport
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	sessionUUID, ok := parseUUIDOrBadRequest(w, req.SessionID, "session_id")
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, req.WorkspaceID, "workspace_id")
	if !ok {
		return
	}
	userUUID, ok := parseUUIDOrBadRequest(w, req.UserID, "user_id")
	if !ok {
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}

	if _, err := h.Queries.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{
		UserID:      userUUID,
		WorkspaceID: wsUUID,
	}); err != nil {
		writeError(w, http.StatusForbidden, "user is not a member of the workspace")
		return
	}

	if err := h.Queries.UpsertTerminalSessionUsage(r.Context(), db.UpsertTerminalSessionUsageParams{
		SessionID:        sessionUUID,
		WorkspaceID:      wsUUID,
		UserID:           userUUID,
		Model:            req.Model,
		InputTokens:      req.InputTokens,
		OutputTokens:     req.OutputTokens,
		CacheReadTokens:  req.CacheReadTokens,
		CacheWriteTokens: req.CacheWriteTokens,
	}); err != nil {
		slog.Warn("upsert terminal usage failed", "session_id", req.SessionID, "model", req.Model, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to store terminal usage")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// DashboardTerminalUsageByUserResponse is one per-user terminal token total.
type DashboardTerminalUsageByUserResponse struct {
	UserID           string `json:"user_id"`
	Name             string `json:"name"`
	Email            string `json:"email"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	SessionCount     int32  `json:"session_count"`
}

// GetDashboardTerminalUsageByUser returns per-user terminal token totals for
// the workspace over the viewer-tz `?days=` window. Powers the
// "Terminal usage (by user)" table on the Usage page.
func (h *Handler) GetDashboardTerminalUsageByUser(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.workspaceMember(w, r, workspaceID); !ok {
		return
	}
	tz := h.resolveViewingTZ(r)
	since := parseSinceParamInTZ(r, 30, tz)

	rows, err := h.Queries.ListTerminalUsageByUser(r.Context(), db.ListTerminalUsageByUserParams{
		WorkspaceID: parseUUID(workspaceID),
		Since:       since,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list terminal usage")
		return
	}

	resp := make([]DashboardTerminalUsageByUserResponse, len(rows))
	for i, row := range rows {
		resp[i] = DashboardTerminalUsageByUserResponse{
			UserID:           uuidToString(row.UserID),
			Name:             row.UserName,
			Email:            row.UserEmail,
			InputTokens:      row.InputTokens,
			OutputTokens:     row.OutputTokens,
			CacheReadTokens:  row.CacheReadTokens,
			CacheWriteTokens: row.CacheWriteTokens,
			TotalTokens:      row.InputTokens + row.OutputTokens + row.CacheReadTokens + row.CacheWriteTokens,
			SessionCount:     row.SessionCount,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
