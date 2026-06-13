package daemonws

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10
)

// ClientIdentity captures the already-authenticated daemon connection scope.
type ClientIdentity struct {
	DaemonID      string
	UserID        string
	WorkspaceID   string
	RuntimeIDs    []string
	ClientVersion string
}

type client struct {
	hub      *Hub
	conn     *websocket.Conn
	send     chan []byte
	identity ClientIdentity
	runtimes map[string]struct{}

	dedupMu  sync.Mutex
	seenIDs  map[string]struct{}
	seenList []string
}

const eventDedupCapacity = 128

// markSeen records eventID as already delivered to this client. Empty event IDs
// disable dedup and are always delivered.
func (c *client) markSeen(eventID string) bool {
	if eventID == "" {
		return true
	}
	c.dedupMu.Lock()
	defer c.dedupMu.Unlock()
	if c.seenIDs == nil {
		c.seenIDs = make(map[string]struct{}, eventDedupCapacity)
	}
	if _, ok := c.seenIDs[eventID]; ok {
		return false
	}
	c.seenIDs[eventID] = struct{}{}
	c.seenList = append(c.seenList, eventID)
	if len(c.seenList) > eventDedupCapacity {
		drop := c.seenList[0]
		c.seenList = c.seenList[1:]
		delete(c.seenIDs, drop)
	}
	return true
}

// HeartbeatHandler processes a daemon:heartbeat frame. It must verify that
// runtimeID is one of identity.RuntimeIDs (the connection's authenticated
// scope) and return the ack payload to send back. Returning an error skips
// the ack and is logged at debug level.
type HeartbeatHandler func(ctx context.Context, identity ClientIdentity, runtimeID string, supportsBatchImport bool, subagentReport *protocol.DaemonHeartbeatSubagentReport) (*protocol.DaemonHeartbeatAckPayload, error)

// MessageKindRecorder is the optional metric hook called once per inbound
// daemon WebSocket frame. kind is the protocol message type with the
// "daemon:" prefix stripped (e.g. "heartbeat") or the literal "unknown" for
// types we don't model. A nil recorder is safely no-op'd.
type MessageKindRecorder interface {
	RecordDaemonWSMessageReceived(kind string)
}

// TerminalFrameHandler is invoked for each inbound terminal:* frame a daemon
// sends back (stdout/exit/error). The bridge routes it to the browser session
// keyed by the payload's session_id. A nil handler drops terminal frames.
//
// We route on runtime_id, not daemon_id: a daemon authenticates the WS with a
// user/CLI token, so identity.DaemonID is frequently empty — the only reliable
// per-connection key the hub indexes is the runtime set (byRuntime). The
// session_id (a server-minted UUID handed only to the target daemon) is the
// real routing capability for inbound frames.
type TerminalFrameHandler func(msgType string, payload json.RawMessage)

// TerminalDisconnectHandler is invoked for each runtime whose last connection
// dropped, so the bridge can close any browser terminals bound to it.
type TerminalDisconnectHandler func(runtimeID string)

// Hub keeps daemon WebSocket connections indexed by runtime ID. Messages are
// best-effort wakeup hints; the daemon still uses HTTP claim for correctness.
type Hub struct {
	upgrader websocket.Upgrader

	mu        sync.RWMutex
	clients   map[*client]bool
	byRuntime map[string]map[*client]bool

	hbMu        sync.RWMutex
	onHeartbeat HeartbeatHandler

	termMu         sync.RWMutex
	onTerminal     TerminalFrameHandler
	onTerminalGone TerminalDisconnectHandler

	kindMu       sync.RWMutex
	kindRecorder MessageKindRecorder
}

func NewHub() *Hub {
	return &Hub{
		upgrader: websocket.Upgrader{
			// Daemon clients authenticate with Authorization headers before the
			// upgrade. Browsers cannot set those headers through the native WS API,
			// and DaemonAuth does not accept cookies, so cookie-based CSWSH does
			// not apply to this endpoint. Re-evaluate this if DaemonAuth ever
			// grows cookie fallback.
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		clients:   make(map[*client]bool),
		byRuntime: make(map[string]map[*client]bool),
	}
}

// SetHeartbeatHandler installs the callback used for daemon:heartbeat frames.
// Wiring is done after handler construction because the handler depends on
// DB queries that aren't available when the hub is built. A nil handler
// disables WS heartbeat processing — daemons fall back to HTTP heartbeat
// transparently because their fallback timer fires whenever no ack arrives.
func (h *Hub) SetHeartbeatHandler(fn HeartbeatHandler) {
	if h == nil {
		return
	}
	h.hbMu.Lock()
	h.onHeartbeat = fn
	h.hbMu.Unlock()
}

func (h *Hub) heartbeatHandler() HeartbeatHandler {
	h.hbMu.RLock()
	defer h.hbMu.RUnlock()
	return h.onHeartbeat
}

// SetTerminalHandlers installs the callbacks used to relay terminal:* frames
// from daemons to browser sessions and to clean up on daemon disconnect.
// Either may be nil to disable that path.
func (h *Hub) SetTerminalHandlers(frame TerminalFrameHandler, gone TerminalDisconnectHandler) {
	if h == nil {
		return
	}
	h.termMu.Lock()
	h.onTerminal = frame
	h.onTerminalGone = gone
	h.termMu.Unlock()
}

func (h *Hub) terminalFrameHandler() TerminalFrameHandler {
	h.termMu.RLock()
	defer h.termMu.RUnlock()
	return h.onTerminal
}

func (h *Hub) terminalDisconnectHandler() TerminalDisconnectHandler {
	if h == nil {
		return nil
	}
	h.termMu.RLock()
	defer h.termMu.RUnlock()
	return h.onTerminalGone
}

// SendToRuntime delivers a frame to a single live connection serving
// runtimeID. Returns false when no connection exists or the candidate's send
// buffer is full. Used by the terminal bridge to push open/stdin/resize/close
// frames to the daemon hosting that runtime.
func (h *Hub) SendToRuntime(runtimeID string, frame []byte) bool {
	if h == nil || runtimeID == "" {
		return false
	}
	h.mu.RLock()
	var target *client
	for c := range h.byRuntime[runtimeID] {
		target = c
		break
	}
	h.mu.RUnlock()
	if target == nil {
		return false
	}
	select {
	case target.send <- frame:
		return true
	default:
		return false
	}
}

// RuntimeConnected reports whether at least one live connection serves
// runtimeID. The terminal bridge uses this to fail an open fast with a clear
// error rather than spawning a session that can never reach the daemon.
func (h *Hub) RuntimeConnected(runtimeID string) bool {
	if h == nil || runtimeID == "" {
		return false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.byRuntime[runtimeID]) > 0
}

// SetMessageKindRecorder installs an optional callback fired exactly once per
// inbound daemon WebSocket frame. Used by the metrics layer to count traffic
// by handler kind without hard-coupling the hub to any specific collector.
func (h *Hub) SetMessageKindRecorder(rec MessageKindRecorder) {
	if h == nil {
		return
	}
	h.kindMu.Lock()
	h.kindRecorder = rec
	h.kindMu.Unlock()
}

func (h *Hub) messageKindRecorder() MessageKindRecorder {
	if h == nil {
		return nil
	}
	h.kindMu.RLock()
	defer h.kindMu.RUnlock()
	return h.kindRecorder
}

func (h *Hub) HandleWebSocket(w http.ResponseWriter, r *http.Request, identity ClientIdentity) {
	if len(identity.RuntimeIDs) == 0 {
		http.Error(w, `{"error":"runtime_ids required"}`, http.StatusBadRequest)
		return
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("daemon websocket upgrade failed", "error", err)
		return
	}

	runtimes := make(map[string]struct{}, len(identity.RuntimeIDs))
	for _, runtimeID := range identity.RuntimeIDs {
		if runtimeID != "" {
			runtimes[runtimeID] = struct{}{}
		}
	}
	if len(runtimes) == 0 {
		conn.WriteMessage(websocket.TextMessage, []byte(`{"error":"runtime_ids required"}`))
		conn.Close()
		return
	}

	c := &client{
		hub:      h,
		conn:     conn,
		send:     make(chan []byte, 16),
		identity: identity,
		runtimes: runtimes,
	}
	h.register(c)

	go c.writePump()
	go c.readPump()
}

// NotifyTaskAvailable sends a best-effort wakeup to daemons watching runtimeID.
func (h *Hub) NotifyTaskAvailable(runtimeID, taskID string) {
	h.notifyTaskAvailable(runtimeID, taskID, "")
}

func (h *Hub) notifyTaskAvailable(runtimeID, taskID, eventID string) {
	if h == nil || runtimeID == "" {
		return
	}
	data, err := taskAvailableFrame(runtimeID, taskID)
	if err != nil {
		return
	}
	delivered, deduped := h.notifyFrame(runtimeID, data, eventID)
	if delivered {
		M.WakeupDeliveredHit.Add(1)
	} else if !deduped {
		M.WakeupDeliveredMiss.Add(1)
	}
}

func (h *Hub) DeliverDaemonRuntime(scopeID string, frame []byte, eventID string) {
	if h == nil {
		return
	}
	M.WakeupReceivedTotal.Add(1)
	var msg protocol.Message
	if err := json.Unmarshal(frame, &msg); err != nil {
		slog.Debug("daemon websocket relay: invalid frame", "error", err, "scope_id", scopeID, "event_id", eventID)
		M.WakeupDeliveredMiss.Add(1)
		return
	}
	if msg.Type != protocol.EventDaemonTaskAvailable {
		M.WakeupDeliveredMiss.Add(1)
		return
	}
	var payload protocol.TaskAvailablePayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil || payload.RuntimeID == "" {
		slog.Debug("daemon websocket relay: invalid task_available payload", "error", err, "scope_id", scopeID, "event_id", eventID)
		M.WakeupDeliveredMiss.Add(1)
		return
	}
	delivered, deduped := h.notifyFrame(payload.RuntimeID, frame, eventID)
	if delivered {
		M.WakeupDeliveredHit.Add(1)
	} else if !deduped {
		M.WakeupDeliveredMiss.Add(1)
	}
}

func (h *Hub) notifyFrame(runtimeID string, data []byte, eventID string) (delivered bool, deduped bool) {
	h.mu.RLock()
	clients := h.byRuntime[runtimeID]
	slow := make([]*client, 0)
	for c := range clients {
		if !c.markSeen(eventID) {
			deduped = true
			continue
		}
		select {
		case c.send <- data:
			delivered = true
		default:
			slow = append(slow, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range slow {
		h.unregister(c)
		c.conn.Close()
	}
	if len(slow) > 0 {
		M.SlowEvictionsTotal.Add(int64(len(slow)))
	}
	return delivered, deduped
}

func taskAvailableFrame(runtimeID, taskID string) ([]byte, error) {
	return json.Marshal(protocol.Message{
		Type: protocol.EventDaemonTaskAvailable,
		Payload: mustMarshalRaw(protocol.TaskAvailablePayload{
			RuntimeID: runtimeID,
			TaskID:    taskID,
		}),
	})
}

func mustMarshalRaw(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return data
}

func (h *Hub) RuntimeConnectionCount(runtimeID string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.byRuntime[runtimeID])
}

func (h *Hub) register(c *client) {
	h.mu.Lock()
	h.clients[c] = true
	for runtimeID := range c.runtimes {
		conns := h.byRuntime[runtimeID]
		if conns == nil {
			conns = make(map[*client]bool)
			h.byRuntime[runtimeID] = conns
		}
		conns[c] = true
	}
	total := len(h.clients)
	h.mu.Unlock()

	M.ConnectsTotal.Add(1)
	M.ActiveConnections.Add(1)
	slog.Info("daemon websocket connected",
		"daemon_id", c.identity.DaemonID,
		"user_id", c.identity.UserID,
		"workspace_id", c.identity.WorkspaceID,
		"runtimes", len(c.runtimes),
		"client_version", c.identity.ClientVersion,
		"total_clients", total,
	)
}

func (h *Hub) unregister(c *client) {
	h.mu.Lock()
	if !h.clients[c] {
		h.mu.Unlock()
		return
	}
	delete(h.clients, c)
	var emptiedRuntimes []string
	for runtimeID := range c.runtimes {
		if conns := h.byRuntime[runtimeID]; conns != nil {
			delete(conns, c)
			if len(conns) == 0 {
				delete(h.byRuntime, runtimeID)
				emptiedRuntimes = append(emptiedRuntimes, runtimeID)
			}
		}
	}
	close(c.send)
	total := len(h.clients)
	h.mu.Unlock()

	// Notify the terminal bridge for each runtime whose last connection just
	// dropped, so it can close any browser terminals bound to it.
	if len(emptiedRuntimes) > 0 {
		if fn := h.terminalDisconnectHandler(); fn != nil {
			for _, rid := range emptiedRuntimes {
				fn(rid)
			}
		}
	}

	M.DisconnectsTotal.Add(1)
	M.ActiveConnections.Add(-1)
	slog.Info("daemon websocket disconnected",
		"daemon_id", c.identity.DaemonID,
		"user_id", c.identity.UserID,
		"workspace_id", c.identity.WorkspaceID,
		"runtimes", len(c.runtimes),
		"total_clients", total,
	)
}

func (c *client) readPump() {
	defer func() {
		c.hub.unregister(c)
		c.conn.Close()
	}()

	c.conn.SetReadLimit(2 * 1024 * 1024)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				slog.Debug("daemon websocket read error", "error", err, "daemon_id", c.identity.DaemonID)
			}
			return
		}
		c.handleFrame(raw)
	}
}

func (c *client) handleFrame(raw []byte) {
	var msg protocol.Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		slog.Debug("daemon websocket invalid frame", "error", err, "daemon_id", c.identity.DaemonID)
		if rec := c.hub.messageKindRecorder(); rec != nil {
			rec.RecordDaemonWSMessageReceived("invalid")
		}
		return
	}
	kind := strings.TrimPrefix(msg.Type, "daemon:")
	if kind == "" {
		kind = "unknown"
	}
	if rec := c.hub.messageKindRecorder(); rec != nil {
		rec.RecordDaemonWSMessageReceived(kind)
	}
	switch msg.Type {
	case protocol.EventDaemonHeartbeat:
		c.handleHeartbeatFrame(msg.Payload)
	case protocol.EventTerminalStdout, protocol.EventTerminalExit, protocol.EventTerminalError:
		if fn := c.hub.terminalFrameHandler(); fn != nil {
			fn(msg.Type, msg.Payload)
		}
	default:
		// Unknown app messages are intentionally ignored for forward
		// compatibility with future daemon → server message types.
	}
}

// handleHeartbeatFrame processes an inbound daemon:heartbeat from the daemon,
// invokes the hub's handler, and writes back a daemon:heartbeat_ack.
func (c *client) handleHeartbeatFrame(raw json.RawMessage) {
	handler := c.hub.heartbeatHandler()
	if handler == nil {
		// Server doesn't have a heartbeat handler wired — daemon will time
		// out waiting for an ack and fall back to HTTP heartbeat.
		return
	}

	var payload protocol.DaemonHeartbeatRequestPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		slog.Debug("daemon websocket heartbeat invalid payload", "error", err, "daemon_id", c.identity.DaemonID)
		return
	}
	if payload.RuntimeID == "" {
		slog.Debug("daemon websocket heartbeat missing runtime_id", "daemon_id", c.identity.DaemonID)
		return
	}
	if _, ok := c.runtimes[payload.RuntimeID]; !ok {
		// The connection authenticated for a fixed runtime set; reject any
		// heartbeat for a runtime the client did not register for.
		slog.Warn("daemon websocket heartbeat for unauthorized runtime",
			"daemon_id", c.identity.DaemonID,
			"runtime_id", payload.RuntimeID)
		return
	}

	// Intentionally do NOT wrap this ctx with WithTimeout. The handler
	// reaches LocalSkill{List,Import}Store.PopPending, whose Redis Lua
	// claim script has side effects (ZREM + SET-running) that cannot be
	// safely un-run if the client cancels mid-script — the same invariant
	// that keeps the HTTP heartbeat from putting a per-call timeout on
	// PopPending. The natural bound is the read pump's lifetime (the conn
	// closes if the daemon goes away) plus Redis's own server-side limits.
	ack, err := handler(context.Background(), c.identity, payload.RuntimeID, payload.SupportsBatchImport, payload.SubagentReport)
	if err != nil {
		slog.Warn("daemon websocket heartbeat handler failed",
			"error", err,
			"daemon_id", c.identity.DaemonID,
			"runtime_id", payload.RuntimeID)
		return
	}
	if ack == nil {
		return
	}
	frame, err := json.Marshal(protocol.Message{
		Type:    protocol.EventDaemonHeartbeatAck,
		Payload: mustMarshalRaw(ack),
	})
	if err != nil {
		slog.Debug("daemon websocket heartbeat ack marshal failed", "error", err)
		return
	}
	select {
	case c.send <- frame:
	default:
		// Send buffer is full — slow client. Don't block the read pump; the
		// next writePump tick or notifyFrame eviction will clean up.
		slog.Debug("daemon websocket heartbeat ack dropped: send buffer full",
			"daemon_id", c.identity.DaemonID,
			"runtime_id", payload.RuntimeID)
	}
}

func (c *client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				slog.Debug("daemon websocket write error", "error", err, "daemon_id", c.identity.DaemonID)
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
