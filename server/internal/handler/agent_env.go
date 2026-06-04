package handler

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// base64DecodeStd is a thin alias for base64.StdEncoding.DecodeString
// so call sites read cleaner inline.
func base64DecodeStd(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// marshalCustomEnv encodes a map for at-rest storage. When
// h.AgentEnvBox is non-nil the payload is wrapped in the v1 envelope
// (AES-256-GCM via secretbox.Seal). With box unset it falls back to
// plaintext JSONB so an operator that hasn't configured the master
// key still has working env management — the GET/PUT endpoints stay
// admin-only and audited regardless.
func (h *Handler) marshalCustomEnv(m map[string]string) ([]byte, error) {
	if m == nil {
		m = map[string]string{}
	}
	plain, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("custom_env: marshal plaintext: %w", err)
	}
	if h.AgentEnvBox == nil || len(m) == 0 {
		// Empty maps always land as `{}` so a row with no env is
		// trivially distinguishable from an encrypted one in DB dumps
		// (no envelope structure to confuse later operators).
		return plain, nil
	}
	sealed, err := h.AgentEnvBox.Seal(plain)
	if err != nil {
		return nil, fmt.Errorf("custom_env: seal: %w", err)
	}
	env := envEnvelopeV1{
		V:  envEnvelopeVersion,
		CT: base64.StdEncoding.EncodeToString(sealed),
	}
	return json.Marshal(env)
}

// envSentinel is the masked marker the UI / clients see in place of a
// real value. A PUT body carrying it for a given key means "do not
// overwrite the existing value for that key" — a defense-in-depth
// guard so a client that round-trips a partially-revealed map cannot
// silently destroy real secrets by saving the masked placeholder.
const envSentinel = "****"

// agentEnvActivityRevealed and agentEnvActivityUpdated are the
// activity_log `action` constants for the two env-management
// endpoints. Stored on rows where `issue_id IS NULL` (env access is not
// tied to any issue). Owners can later query them — a queryable audit
// UI is out of scope for this PR, but the rows are written now so the
// data is captured from day one. Workspace activity history will
// eventually surface them; for now they're forensic-only.
const (
	agentEnvActivityRevealed = "agent_env_revealed"
	agentEnvActivityUpdated  = "agent_env_updated"
)

// AgentEnvResponse is the wire shape for the dedicated env-management
// endpoint. Kept distinct from `AgentResponse` so secrets cannot leak
// back into the generic agent resource by accident — a future
// refactor that adds a field to AgentResponse cannot accidentally
// pull env values along.
type AgentEnvResponse struct {
	AgentID   string            `json:"agent_id"`
	CustomEnv map[string]string `json:"custom_env"`
}

// UpdateAgentEnvRequest is the wire shape for `PUT
// /api/agents/{id}/env`. Only `custom_env` is accepted — fewer
// surfaces, less to misuse.
type UpdateAgentEnvRequest struct {
	CustomEnv map[string]string `json:"custom_env"`
}

// authorizeAgentEnv enforces the per-request auth contract for the env
// endpoints:
//
//  1. The actor MUST resolve to a member (human). Any request authored
//     by an agent token — even one whose backing member is a workspace
//     owner — is rejected. This is the key fix for the
//     impersonation/lateral-movement risk that motivated MUL-2600: an
//     agent running in the workspace cannot use its host's owner
//     credentials to reveal another agent's secrets.
//  2. The member must be a workspace owner or admin.
//
// Returns the loaded agent and the authenticated member on success.
// All non-2xx branches write their own response and return ok=false.
func (h *Handler) authorizeAgentEnv(w http.ResponseWriter, r *http.Request) (db.Agent, db.Member, bool) {
	agentID := chi.URLParam(r, "id")
	agent, ok := h.loadAgentForUser(w, r, agentID)
	if !ok {
		return db.Agent{}, db.Member{}, false
	}

	workspaceID := uuidToString(agent.WorkspaceID)
	userID := requestUserID(r)

	// Reject agent actors before anything else. resolveActor returns
	// "agent" iff both X-Agent-ID and a valid X-Task-ID are present and
	// the task belongs to that agent — so this guard is precise and
	// cannot be tricked by a member-supplied header.
	actorType, _ := h.resolveActor(r, userID, workspaceID)
	if actorType == "agent" {
		writeError(w, http.StatusForbidden, "agents may not access env management endpoints")
		return db.Agent{}, db.Member{}, false
	}

	member, ok := h.requireWorkspaceRole(w, r, workspaceID, "agent not found", "owner", "admin")
	if !ok {
		return db.Agent{}, db.Member{}, false
	}

	return agent, member, true
}

// GetAgentEnv returns the agent's env KEY NAMES with every value
// masked as the `****` sentinel. The server NEVER sends plaintext
// values to any client — not even an authenticated workspace owner —
// because a browser cache, screenshot, or accidental log line is an
// unrecoverable secret disclosure. Owners and admins who need a real
// value must either re-source it from where they originally got it
// (vault, API console, key file) or PUT a new value through
// UpdateAgentEnv to overwrite the old one.
//
// The `****` round-trip is honoured by mergeAgentEnv on write: a PUT
// carrying `****` for an existing key preserves the stored value, so
// the UI can add/remove keys or replace values without the server
// ever surfacing the originals.
//
// We still write an `agent_env_revealed` row (now interpreted as
// "key names listed") because operators want a trail of who poked at
// agent secrets even if no value was disclosed. Fail-closed: an
// audit-write outage refuses the read so we don't quietly serve key
// lists without recording it.
func (h *Handler) GetAgentEnv(w http.ResponseWriter, r *http.Request) {
	agent, member, ok := h.authorizeAgentEnv(w, r)
	if !ok {
		return
	}

	customEnv := h.unmarshalCustomEnv(agent)
	revealedKeys := sortedKeys(customEnv)

	// Build the masked response: same shape as before, every value
	// replaced with the `****` sentinel. The wire payload is now
	// independent of the at-rest ciphertext — a successful response
	// proves nothing about the secret.
	masked := make(map[string]string, len(revealedKeys))
	for _, k := range revealedKeys {
		masked[k] = envSentinel
	}

	details, _ := json.Marshal(map[string]any{
		"agent_id":      uuidToString(agent.ID),
		"agent_name":    agent.Name,
		"revealed_keys": revealedKeys,
		"key_count":     len(revealedKeys),
	})
	if _, err := h.Queries.CreateActivity(r.Context(), db.CreateActivityParams{
		WorkspaceID: agent.WorkspaceID,
		IssueID:     pgtype.UUID{}, // env access is not tied to an issue
		ActorType:   pgtype.Text{String: "member", Valid: true},
		ActorID:     parseUUID(uuidToString(member.UserID)),
		Action:      agentEnvActivityRevealed,
		Details:     details,
	}); err != nil {
		slog.Error("agent_env_revealed audit write failed; refusing to serve key list",
			append(logger.RequestAttrs(r), "error", err, "agent_id", uuidToString(agent.ID))...)
		writeError(w, http.StatusInternalServerError, "audit log write failed; refusing to serve env without a recorded reveal")
		return
	}

	writeJSON(w, http.StatusOK, AgentEnvResponse{
		AgentID:   uuidToString(agent.ID),
		CustomEnv: masked,
	})
}

// UpdateAgentEnv replaces an agent's custom_env wholesale. The **** marker is
// honoured per-key: any value equal to envSentinel is treated as
// "keep the existing value for that key", protecting against the
// scenario where a UI fetches the env, exposes some values but leaves
// others masked, and then naively PUTs the whole map back. A
// straightforward write would have stored literal `****` in place of
// the real secret. Audit log captures the symmetric difference between
// old and new keys but never values.
//
// Persist + audit run inside one DB transaction so they commit
// together or roll back together. An audit-write outage cannot leave
// an unaudited env mutation on disk, and a persist failure does not
// leave a phantom audit row claiming a change that never happened.
func (h *Handler) UpdateAgentEnv(w http.ResponseWriter, r *http.Request) {
	agent, member, ok := h.authorizeAgentEnv(w, r)
	if !ok {
		return
	}

	var req UpdateAgentEnvRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.CustomEnv == nil {
		req.CustomEnv = map[string]string{}
	}

	existing := h.unmarshalCustomEnv(agent)
	merged, audit := mergeAgentEnv(existing, req.CustomEnv)

	envBytes, err := h.marshalCustomEnv(merged)
	if err != nil {
		slog.Error("agent_env update: marshal/encrypt failed",
			append(logger.RequestAttrs(r), "error", err, "agent_id", uuidToString(agent.ID))...)
		writeError(w, http.StatusInternalServerError, "failed to encode env")
		return
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		slog.Error("agent_env update: begin tx failed",
			append(logger.RequestAttrs(r), "error", err, "agent_id", uuidToString(agent.ID))...)
		writeError(w, http.StatusInternalServerError, "failed to update env")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)

	updated, err := qtx.UpdateAgentCustomEnv(r.Context(), db.UpdateAgentCustomEnvParams{
		ID:        agent.ID,
		CustomEnv: envBytes,
	})
	if err != nil {
		slog.Warn("update agent custom_env failed",
			append(logger.RequestAttrs(r), "error", err, "agent_id", uuidToString(agent.ID))...)
		writeError(w, http.StatusInternalServerError, "failed to update env")
		return
	}

	auditDetails := map[string]any{
		"agent_id":       uuidToString(agent.ID),
		"agent_name":     agent.Name,
		"added_keys":     audit.added,
		"removed_keys":   audit.removed,
		"changed_keys":   audit.changed,
		"preserved_keys": audit.preserved,
	}
	details, _ := json.Marshal(auditDetails)
	if _, err := qtx.CreateActivity(r.Context(), db.CreateActivityParams{
		WorkspaceID: agent.WorkspaceID,
		IssueID:     pgtype.UUID{},
		ActorType:   pgtype.Text{String: "member", Valid: true},
		ActorID:     parseUUID(uuidToString(member.UserID)),
		Action:      agentEnvActivityUpdated,
		Details:     details,
	}); err != nil {
		slog.Error("agent_env_updated audit write failed; rolling back update",
			append(logger.RequestAttrs(r), "error", err, "agent_id", uuidToString(agent.ID))...)
		writeError(w, http.StatusInternalServerError, "audit log write failed; env update rolled back")
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		slog.Error("agent_env update: tx commit failed",
			append(logger.RequestAttrs(r), "error", err, "agent_id", uuidToString(agent.ID))...)
		writeError(w, http.StatusInternalServerError, "failed to update env")
		return
	}

	// Broadcast an agent:status update so connected clients refresh the
	// "N variables configured" indicator. Payload is the redacted
	// AgentResponse — no env values are sent. Skills are reloaded so the
	// broadcast doesn't tell subscribers the agent has no skills (#3459).
	resp := h.agentToResponse(updated)
	if err := h.attachAgentSkills(r.Context(), &resp, updated.ID); err != nil {
		slog.Warn("load agent skills after env update failed",
			append(logger.RequestAttrs(r), "error", err, "agent_id", uuidToString(updated.ID))...)
		writeError(w, http.StatusInternalServerError, "failed to load agent skills")
		return
	}
	workspaceID := uuidToString(updated.WorkspaceID)
	h.publish(protocol.EventAgentStatus, workspaceID, "member", uuidToString(member.UserID), map[string]any{"agent": broadcastAgentResponse(resp)})

	// Mirror GetAgentEnv: the PUT response shape includes a custom_env
	// map but every value is masked. Returning the just-written
	// plaintext would let any client that retains the response body
	// (devtools, log, error reporter) hold the secret indefinitely;
	// the UI doesn't need it anyway because the user just typed the
	// values into their own input fields.
	maskedMerged := make(map[string]string, len(merged))
	for k := range merged {
		maskedMerged[k] = envSentinel
	}
	writeJSON(w, http.StatusOK, AgentEnvResponse{
		AgentID:   uuidToString(updated.ID),
		CustomEnv: maskedMerged,
	})
}

// envAudit summarises the diff between an agent's existing env and the
// new one, broken down so an auditor can reconstruct exactly which
// keys an operation touched without leaking values. All slices are
// sorted to keep the activity row content deterministic for tests and
// downstream tooling.
type envAudit struct {
	added     []string
	removed   []string
	changed   []string
	preserved []string
}

// mergeAgentEnv applies the **** sentinel rule and returns both the
// final map to persist and an audit summary of which keys changed.
// Behaviour:
//   - request key present, value == "****", key exists in `existing`
//     → keep the existing value, append to preserved
//   - request key present, value == "****", key NOT in `existing`
//     → drop the key (literal "****" is never a valid stored value)
//   - request key present, value != "****", key already in existing
//     with same value → no-op (not counted)
//   - request key present, value != "****", different from existing
//     → write new value, append to changed
//   - request key present, value != "****", key NOT in existing
//     → write new value, append to added
//   - key in existing but absent from request → removed
func mergeAgentEnv(existing, request map[string]string) (map[string]string, envAudit) {
	merged := make(map[string]string, len(request))
	audit := envAudit{}

	for k, v := range request {
		if v == envSentinel {
			if old, ok := existing[k]; ok {
				merged[k] = old
				audit.preserved = append(audit.preserved, k)
			}
			// else: drop. We never persist a literal "****".
			continue
		}
		if old, ok := existing[k]; ok {
			if old == v {
				merged[k] = v
				continue
			}
			merged[k] = v
			audit.changed = append(audit.changed, k)
			continue
		}
		merged[k] = v
		audit.added = append(audit.added, k)
	}

	for k := range existing {
		if _, ok := request[k]; !ok {
			audit.removed = append(audit.removed, k)
		}
	}

	sort.Strings(audit.added)
	sort.Strings(audit.removed)
	sort.Strings(audit.changed)
	sort.Strings(audit.preserved)
	return merged, audit
}

// envEnvelopeVersion is the current at-rest envelope schema version.
// Bump when introducing key rotation or AEAD changes; the read path
// switches on it.
const envEnvelopeVersion = 1

// envEnvelopeV1 is the JSONB shape used when MULTICA_AGENT_ENV_KEY is
// set. Sentinel keys (_v, _ct) start with underscore to prevent
// collision with caller-supplied env names, which by convention are
// uppercase letters / digits / underscores starting with a letter.
type envEnvelopeV1 struct {
	V  int    `json:"_v"`
	CT string `json:"_ct"` // base64-std of nonce||ciphertext||tag
}

// isEnvelopeShape returns true if raw decodes as a v1 envelope. Reads
// only the top-level keys to keep the probe cheap.
func isEnvelopeShape(raw []byte) bool {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	_, hasV := probe["_v"]
	_, hasCT := probe["_ct"]
	return hasV && hasCT
}

// unmarshalCustomEnv decodes an agent's stored custom_env into a map,
// returning an empty (never nil) map so callers can iterate safely.
// Supports both the v1 envelope (encrypted, requires h.AgentEnvBox)
// and legacy plaintext JSONB so existing rows keep reading until an
// admin saves them through the encrypted write path.
func (h *Handler) unmarshalCustomEnv(a db.Agent) map[string]string {
	out := map[string]string{}
	if len(a.CustomEnv) == 0 {
		return out
	}
	if isEnvelopeShape(a.CustomEnv) {
		if h.AgentEnvBox == nil {
			slog.Error("custom_env: row is encrypted but MULTICA_AGENT_ENV_KEY is unset", "agent_id", uuidToString(a.ID))
			return map[string]string{}
		}
		var env envEnvelopeV1
		if err := json.Unmarshal(a.CustomEnv, &env); err != nil {
			slog.Warn("custom_env: envelope decode failed", "agent_id", uuidToString(a.ID), "error", err)
			return map[string]string{}
		}
		if env.V != envEnvelopeVersion {
			slog.Error("custom_env: unknown envelope version", "agent_id", uuidToString(a.ID), "version", env.V)
			return map[string]string{}
		}
		sealed, err := base64DecodeStd(env.CT)
		if err != nil {
			slog.Warn("custom_env: base64 decode failed", "agent_id", uuidToString(a.ID), "error", err)
			return map[string]string{}
		}
		plain, err := h.AgentEnvBox.Open(sealed)
		if err != nil {
			slog.Warn("custom_env: secretbox open failed", "agent_id", uuidToString(a.ID), "error", err)
			return map[string]string{}
		}
		if err := json.Unmarshal(plain, &out); err != nil {
			slog.Warn("custom_env: plaintext decode failed", "agent_id", uuidToString(a.ID), "error", err)
			return map[string]string{}
		}
		if out == nil {
			return map[string]string{}
		}
		return out
	}
	// Legacy plaintext fallback. Existing rows read transparently;
	// they get upgraded to the envelope on the next PUT through
	// UpdateAgentEnv.
	if err := json.Unmarshal(a.CustomEnv, &out); err != nil {
		slog.Warn("failed to unmarshal agent custom_env", "agent_id", uuidToString(a.ID), "error", err)
		return map[string]string{}
	}
	if out == nil {
		return map[string]string{}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
