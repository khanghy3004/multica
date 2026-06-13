package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Browser ↔ daemon terminal relay. The browser opens a WebSocket to the
// server; the server mints a session_id, relays open/stdin/resize/close down
// to the target daemon over the existing daemon WS hub, and pipes the daemon's
// stdout/exit/error frames back up to the browser. The daemon owns the PTY;
// the server is a stateless byte relay plus the authorization boundary.
//
// SECURITY: the session_id is server-authoritative. The browser never sets it
// — every frame the server forwards to the daemon is stamped with the id this
// connection was assigned, so one browser cannot address another's PTY. Access
// is gated to workspace owner/admin (route middleware) and the daemon must
// belong to the same workspace and be currently connected.

const (
	terminalWriteWait  = 10 * time.Second
	terminalPongWait   = 60 * time.Second
	terminalPingPeriod = (terminalPongWait * 9) / 10
	// terminalBrowserSendBuffer bounds buffered output frames per browser
	// session. A slow browser that fills this is disconnected rather than
	// letting the daemon relay grow unbounded.
	terminalBrowserSendBuffer = 256
)

var terminalUpgrader = websocket.Upgrader{CheckOrigin: realtime.CheckOrigin}

// terminalSession is one browser terminal connection on the server side.
type terminalSession struct {
	id        string
	runtimeID string
	conn      *websocket.Conn
	send      chan []byte

	closeOnce sync.Once
	closed    chan struct{}
}

// TerminalBridge routes daemon terminal frames to the browser session that
// owns them and cleans up sessions when the serving runtime disconnects.
// Registered on the daemon hub via SetTerminalHandlers.
type TerminalBridge struct {
	mu        sync.RWMutex
	sessions  map[string]*terminalSession            // session_id -> session
	byRuntime map[string]map[string]*terminalSession // runtime_id -> set
}

func NewTerminalBridge() *TerminalBridge {
	return &TerminalBridge{
		sessions:  make(map[string]*terminalSession),
		byRuntime: make(map[string]map[string]*terminalSession),
	}
}

func (b *TerminalBridge) add(s *terminalSession) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sessions[s.id] = s
	conns := b.byRuntime[s.runtimeID]
	if conns == nil {
		conns = make(map[string]*terminalSession)
		b.byRuntime[s.runtimeID] = conns
	}
	conns[s.id] = s
}

func (b *TerminalBridge) remove(s *terminalSession) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sessions, s.id)
	if conns := b.byRuntime[s.runtimeID]; conns != nil {
		delete(conns, s.id)
		if len(conns) == 0 {
			delete(b.byRuntime, s.runtimeID)
		}
	}
}

// OnFrame is the daemonws.TerminalFrameHandler: forward a daemon's terminal:*
// frame to the browser session named by the payload's session_id. The
// session_id is a server-minted UUID handed only to the target daemon, so it
// is the routing capability — no separate identity check is needed (and the
// daemon WS identity carries no reliable daemon_id anyway).
func (b *TerminalBridge) OnFrame(msgType string, payload json.RawMessage) {
	var head struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(payload, &head); err != nil || head.SessionID == "" {
		return
	}
	b.mu.RLock()
	s := b.sessions[head.SessionID]
	b.mu.RUnlock()
	if s == nil {
		return
	}
	frame, err := json.Marshal(protocol.Message{Type: msgType, Payload: payload})
	if err != nil {
		return
	}
	s.enqueue(frame)
	// An exit frame ends the session — let the browser render it, then close.
	if msgType == protocol.EventTerminalExit {
		s.close()
	}
}

// OnRuntimeGone is the daemonws.TerminalDisconnectHandler: the last connection
// for runtimeID dropped, so every browser terminal bound to it must close.
func (b *TerminalBridge) OnRuntimeGone(runtimeID string) {
	b.mu.Lock()
	conns := b.byRuntime[runtimeID]
	doomed := make([]*terminalSession, 0, len(conns))
	for _, s := range conns {
		doomed = append(doomed, s)
	}
	b.mu.Unlock()
	for _, s := range doomed {
		s.enqueue(mustTerminalErrorFrame(s.id, "daemon disconnected"))
		s.close()
	}
}

func (s *terminalSession) enqueue(frame []byte) {
	select {
	case s.send <- frame:
	case <-s.closed:
	default:
		// Browser is too slow — drop. Closing here would race the writePump;
		// the ping/pong deadline will reap a truly dead connection.
	}
}

func (s *terminalSession) close() {
	s.closeOnce.Do(func() { close(s.closed) })
}

// TerminalWebSocket upgrades a browser connection and relays an interactive
// terminal to the daemon serving ?runtime_id=. Route is gated to workspace
// owner/admin. Routing is by runtime_id because the daemon WS authenticates
// with a user/CLI token and carries no reliable daemon_id — the runtime set is
// the only per-connection key the hub indexes.
func (h *Handler) TerminalWebSocket(w http.ResponseWriter, r *http.Request) {
	if h.DaemonHub == nil || h.TerminalBridge == nil {
		writeError(w, http.StatusServiceUnavailable, "terminal unavailable")
		return
	}

	workspaceID := middleware.WorkspaceIDFromContext(r.Context())
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id required")
		return
	}
	runtimeID := r.URL.Query().Get("runtime_id")
	if runtimeID == "" {
		writeError(w, http.StatusBadRequest, "runtime_id required")
		return
	}

	// The runtime must belong to this (owner/admin-gated) workspace — the
	// authorization tie between the workspace and the machine the browser is
	// about to get a shell on.
	if !h.runtimeBelongsToWorkspace(r, workspaceID, runtimeID) {
		writeError(w, http.StatusNotFound, "runtime not found in this workspace")
		return
	}
	if !h.DaemonHub.RuntimeConnected(runtimeID) {
		writeError(w, http.StatusConflict, "daemon is offline")
		return
	}

	conn, err := terminalUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Debug("terminal websocket upgrade failed", "error", err)
		return
	}

	session := &terminalSession{
		id:        uuid.NewString(),
		runtimeID: runtimeID,
		conn:      conn,
		send:      make(chan []byte, terminalBrowserSendBuffer),
		closed:    make(chan struct{}),
	}
	h.TerminalBridge.add(session)
	slog.Info("terminal session opened",
		"session_id", session.id,
		"runtime_id", runtimeID,
		"workspace_id", workspaceID,
		"user_id", requestUserID(r),
	)

	// Ask the daemon to spawn the PTY. If the send buffer is full the open is
	// lost; the browser will see no output and can retry.
	openFrame, _ := json.Marshal(protocol.Message{
		Type: protocol.EventTerminalOpen,
		Payload: mustMarshalJSON(protocol.TerminalOpenPayload{
			SessionID:   session.id,
			WorkspaceID: workspaceID,
			UserID:      requestUserID(r),
			Cols:        0,
			Rows:        0,
		}),
	})
	if !h.DaemonHub.SendToRuntime(runtimeID, openFrame) {
		session.enqueue(mustTerminalErrorFrame(session.id, "failed to reach daemon"))
		session.close()
	}

	go h.terminalWritePump(session)
	h.terminalReadPump(session)
}

// terminalReadPump reads browser frames (stdin/resize), stamps the
// server-authoritative session_id, and forwards them to the daemon. Runs on
// the request goroutine; returns when the browser disconnects or the session
// is closed elsewhere.
func (h *Handler) terminalReadPump(s *terminalSession) {
	defer func() {
		// Tell the daemon to tear the PTY down, then drop the session.
		closeFrame, _ := json.Marshal(protocol.Message{
			Type:    protocol.EventTerminalClose,
			Payload: mustMarshalJSON(protocol.TerminalClosePayload{SessionID: s.id}),
		})
		h.DaemonHub.SendToRuntime(s.runtimeID, closeFrame)
		h.TerminalBridge.remove(s)
		s.close()
		s.conn.Close()
	}()

	s.conn.SetReadLimit(1024 * 1024)
	s.conn.SetReadDeadline(time.Now().Add(terminalPongWait))
	s.conn.SetPongHandler(func(string) error {
		s.conn.SetReadDeadline(time.Now().Add(terminalPongWait))
		return nil
	})

	for {
		_, raw, err := s.conn.ReadMessage()
		if err != nil {
			return
		}
		var in struct {
			Type    string `json:"type"`
			Payload struct {
				Data string `json:"data"`
				Cols uint16 `json:"cols"`
				Rows uint16 `json:"rows"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			continue
		}
		switch in.Type {
		case protocol.EventTerminalStdin:
			frame, _ := json.Marshal(protocol.Message{
				Type: protocol.EventTerminalStdin,
				Payload: mustMarshalJSON(protocol.TerminalStdinPayload{
					SessionID: s.id,
					Data:      in.Payload.Data,
				}),
			})
			h.DaemonHub.SendToRuntime(s.runtimeID, frame)
		case protocol.EventTerminalResize:
			frame, _ := json.Marshal(protocol.Message{
				Type: protocol.EventTerminalResize,
				Payload: mustMarshalJSON(protocol.TerminalResizePayload{
					SessionID: s.id,
					Cols:      in.Payload.Cols,
					Rows:      in.Payload.Rows,
				}),
			})
			h.DaemonHub.SendToRuntime(s.runtimeID, frame)
		}
	}
}

func (h *Handler) terminalWritePump(s *terminalSession) {
	ticker := time.NewTicker(terminalPingPeriod)
	defer func() {
		ticker.Stop()
		s.conn.Close()
	}()
	for {
		select {
		case frame := <-s.send:
			s.conn.SetWriteDeadline(time.Now().Add(terminalWriteWait))
			if err := s.conn.WriteMessage(websocket.TextMessage, frame); err != nil {
				return
			}
		case <-ticker.C:
			s.conn.SetWriteDeadline(time.Now().Add(terminalWriteWait))
			if err := s.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-s.closed:
			// Flush nothing further; a CloseMessage tells xterm the session
			// ended cleanly. Best-effort.
			s.conn.SetWriteDeadline(time.Now().Add(terminalWriteWait))
			s.conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return
		}
	}
}

// runtimeBelongsToWorkspace reports whether runtimeID is a runtime in the
// workspace. Validates the path-supplied runtime against the gated workspace
// before the browser gets a shell on the machine hosting it.
func (h *Handler) runtimeBelongsToWorkspace(r *http.Request, workspaceID, runtimeID string) bool {
	rtUUID, err := util.ParseUUID(runtimeID)
	if err != nil {
		return false
	}
	wsUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return false
	}
	_, err = h.Queries.GetAgentRuntimeForWorkspace(r.Context(), db.GetAgentRuntimeForWorkspaceParams{
		ID:          rtUUID,
		WorkspaceID: wsUUID,
	})
	return err == nil
}

func mustTerminalErrorFrame(sessionID, message string) []byte {
	frame, _ := json.Marshal(protocol.Message{
		Type: protocol.EventTerminalError,
		Payload: mustMarshalJSON(protocol.TerminalErrorPayload{
			SessionID: sessionID,
			Message:   message,
		}),
	})
	return frame
}

func mustMarshalJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return data
}
