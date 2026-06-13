package daemon

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// terminalUsageInterval is how often the usage reader re-parses a live
// session's transcript and reports cumulative totals to the server.
const terminalUsageInterval = 30 * time.Second

// terminalReadChunk caps a single PTY read. The server hub accepts frames up
// to 2 MiB; base64 inflates by ~4/3, so 32 KiB of raw output stays well under
// that even after encoding plus the JSON envelope.
const terminalReadChunk = 32 * 1024

// terminalSession is one live PTY hosting an interactive agent CLI for a
// single browser tab. The session is identified by the server-minted
// SessionID and torn down when the process exits, the browser closes, or the
// daemon's WS connection drops.
type terminalSession struct {
	id  string
	cmd *exec.Cmd
	pty *os.File

	// Usage attribution: who opened the terminal, in which workspace, and the
	// claude --session-id we assigned so the transcript file is locatable.
	userID          string
	workspaceID     string
	claudeSessionID string

	closeOnce sync.Once

	// stopUsage is closed once when the session ends; the usage reader does a
	// final transcript flush and exits.
	stopUsage     chan struct{}
	stopUsageOnce sync.Once
}

// endUsage signals the usage reader to do its final flush and stop. Safe to
// call multiple times.
func (s *terminalSession) endUsage() {
	s.stopUsageOnce.Do(func() { close(s.stopUsage) })
}

// terminalManager owns every live terminalSession on this daemon and the
// single channel back to the server (the active WS write channel). The sink
// is swapped on each (re)connect; when nil, output is dropped and new opens
// fail fast.
type terminalManager struct {
	d *Daemon

	mu       sync.Mutex
	sessions map[string]*terminalSession
	sink     chan<- []byte // current WS write channel; nil when disconnected
}

func newTerminalManager(d *Daemon) *terminalManager {
	return &terminalManager{
		d:        d,
		sessions: make(map[string]*terminalSession),
	}
}

// setSink installs the WS write channel for the active connection. Passing nil
// detaches it (on disconnect) and kills every live session — a terminal can't
// survive its transport, and a dangling PTY would leak a process.
func (m *terminalManager) setSink(sink chan<- []byte) {
	m.mu.Lock()
	m.sink = sink
	var doomed []*terminalSession
	if sink == nil {
		for _, s := range m.sessions {
			doomed = append(doomed, s)
		}
		m.sessions = make(map[string]*terminalSession)
	}
	m.mu.Unlock()
	for _, s := range doomed {
		s.close()
	}
}

// send enqueues a frame to the server. Non-blocking: a backed-up writer drops
// the frame rather than stalling a PTY read pump (terminal output is lossy by
// nature; the user can scroll/redraw).
func (m *terminalManager) send(msgType string, payload any) {
	m.mu.Lock()
	sink := m.sink
	m.mu.Unlock()
	if sink == nil {
		return
	}
	frame, err := json.Marshal(protocol.Message{Type: msgType, Payload: marshalRaw(payload)})
	if err != nil {
		return
	}
	select {
	case sink <- frame:
	default:
		m.d.logger.Debug("terminal frame dropped: writer backlog", "type", msgType)
	}
}

// handleOpen spawns the interactive CLI in a PTY for sessionID and starts
// pumping its output back to the server.
func (m *terminalManager) handleOpen(p protocol.TerminalOpenPayload) {
	if p.SessionID == "" {
		return
	}

	m.mu.Lock()
	if m.sink == nil {
		m.mu.Unlock()
		return
	}
	if _, exists := m.sessions[p.SessionID]; exists {
		// Duplicate open for a live session — ignore; the first PTY stands.
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	// Hard rule: the web terminal only ever runs `claude`. No other provider
	// or arbitrary command is accepted.
	execPath := m.resolveProviderPath("claude")
	if execPath == "" {
		m.send(protocol.EventTerminalError, protocol.TerminalErrorPayload{
			SessionID: p.SessionID,
			Message:   "claude CLI not found on this machine",
		})
		m.send(protocol.EventTerminalExit, protocol.TerminalExitPayload{SessionID: p.SessionID, Code: 127})
		return
	}

	// Hard rule: the PTY is confined to this workspace's folder under the
	// daemon's WorkspacesRoot. The path is derived entirely on the daemon side
	// from the workspace_id — the client cannot supply an arbitrary cwd.
	workDir := m.resolveWorkspaceDir(p.WorkspaceID)
	if workDir == "" {
		m.send(protocol.EventTerminalError, protocol.TerminalErrorPayload{
			SessionID: p.SessionID,
			Message:   "workspace folder unavailable",
		})
		m.send(protocol.EventTerminalExit, protocol.TerminalExitPayload{SessionID: p.SessionID, Code: 1})
		return
	}

	// Confine this interactive session with a session-scoped settings file
	// passed via --settings. It is NOT written into the workspace tree, so it
	// only affects this terminal — task-execution agents that spawn `claude`
	// without the flag are unaffected. The deny rules (secrets / network /
	// destructive) are hard blocks the user cannot approve their way out of;
	// file tools are auto-allowed only inside the workspace folder.
	// Assign a known claude session id so the transcript file is deterministic
	// (~/.claude/projects/<slug>/<id>.jsonl); the usage reader locates it by id
	// and attributes the token usage to the user who opened the terminal.
	claudeSessionID := uuid.NewString()
	cmd := exec.Command(execPath, "--session-id", claudeSessionID, "--settings", terminalSettingsArg(workDir))
	cmd.Dir = workDir
	// Inherit the daemon's environment so the CLI sees the user's auth, PATH,
	// and provider keys exactly as a hand-launched `claude` would.
	cmd.Env = os.Environ()

	size := &pty.Winsize{Cols: clampDim(p.Cols, 80), Rows: clampDim(p.Rows, 24)}
	ptyFile, err := pty.StartWithSize(cmd, size)
	if err != nil {
		m.send(protocol.EventTerminalError, protocol.TerminalErrorPayload{
			SessionID: p.SessionID,
			Message:   "failed to start terminal: " + err.Error(),
		})
		m.send(protocol.EventTerminalExit, protocol.TerminalExitPayload{SessionID: p.SessionID, Code: 1})
		return
	}

	s := &terminalSession{
		id:              p.SessionID,
		cmd:             cmd,
		pty:             ptyFile,
		userID:          p.UserID,
		workspaceID:     p.WorkspaceID,
		claudeSessionID: claudeSessionID,
		stopUsage:       make(chan struct{}),
	}

	// Register only if the connection is still up; otherwise the open raced a
	// disconnect and we must not leak the process.
	m.mu.Lock()
	if m.sink == nil {
		m.mu.Unlock()
		s.close()
		return
	}
	m.sessions[p.SessionID] = s
	m.mu.Unlock()

	m.d.logger.Info("terminal session started", "session_id", p.SessionID, "provider", "claude", "cwd", cmd.Dir)
	go m.pump(s)
	// Track token usage only when we know who to attribute it to.
	if s.userID != "" && s.workspaceID != "" {
		go m.trackUsage(s)
	}
}

// pump streams PTY output to the server until EOF, then reaps the process and
// emits a terminal:exit frame.
func (m *terminalManager) pump(s *terminalSession) {
	buf := make([]byte, terminalReadChunk)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			m.send(protocol.EventTerminalStdout, protocol.TerminalStdoutPayload{
				SessionID: s.id,
				Data:      base64.StdEncoding.EncodeToString(buf[:n]),
			})
		}
		if err != nil {
			break
		}
	}

	// Reap the process so it doesn't linger as a zombie. PTY EOF means the
	// child closed its controlling terminal — Wait collects the exit status.
	code := 0
	if err := s.cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			code = 1
		}
	}

	m.mu.Lock()
	delete(m.sessions, s.id)
	m.mu.Unlock()
	s.close()
	// Trigger the usage reader's final flush so the last turn's tokens land.
	s.endUsage()

	m.send(protocol.EventTerminalExit, protocol.TerminalExitPayload{SessionID: s.id, Code: code})
	m.d.logger.Info("terminal session ended", "session_id", s.id, "code", code)
}

// terminalUsageTotals accumulates per-model cumulative token counts parsed
// from a claude session transcript.
type terminalUsageTotals struct {
	input      int64
	output     int64
	cacheRead  int64
	cacheWrite int64
}

// trackUsage periodically (and once more on session end) parses the session's
// transcript and reports cumulative per-model token totals to the server.
func (m *terminalManager) trackUsage(s *terminalSession) {
	t := time.NewTicker(terminalUsageInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			m.flushUsage(s)
		case <-s.stopUsage:
			m.flushUsage(s)
			return
		}
	}
}

// flushUsage locates the session transcript, sums per-model totals, and reports
// them. Cumulative + REPLACE-upsert on the server makes repeated flushes
// idempotent, so a partial read or a missing-yet transcript is harmless.
func (m *terminalManager) flushUsage(s *terminalSession) {
	path := locateTranscript(s.claudeSessionID)
	if path == "" {
		return
	}
	totals, err := parseTranscriptUsage(path)
	if err != nil || len(totals) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for model, u := range totals {
		if err := m.d.client.ReportTerminalUsage(ctx, s.id, s.workspaceID, s.userID, model,
			u.input, u.output, u.cacheRead, u.cacheWrite); err != nil {
			m.d.logger.Debug("terminal usage report failed", "session_id", s.id, "model", model, "error", err)
		}
	}
}

// locateTranscript finds claude's transcript file for a given session id.
// claude writes ~/.claude/projects/<cwd-slug>/<session-id>.jsonl; we match by
// filename across all project dirs so we don't have to replicate claude's
// cwd-slug algorithm.
func locateTranscript(claudeSessionID string) string {
	if claudeSessionID == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	matches, err := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", claudeSessionID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// parseTranscriptUsage reads a claude transcript JSONL and sums per-model token
// usage across all assistant turns. Each assistant line carries a
// `message.usage` block; summing the per-turn figures yields the session's
// cumulative usage (the same per-turn accounting the agent task path uses).
func parseTranscriptUsage(path string) (map[string]terminalUsageTotals, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	totals := make(map[string]terminalUsageTotals)
	reader := bufio.NewReader(f)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			var entry struct {
				Message struct {
					Model string `json:"model"`
					Usage struct {
						InputTokens              int64 `json:"input_tokens"`
						OutputTokens             int64 `json:"output_tokens"`
						CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
						CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &entry) == nil {
				u := entry.Message.Usage
				model := entry.Message.Model
				if model != "" && (u.InputTokens|u.OutputTokens|u.CacheCreationInputTokens|u.CacheReadInputTokens) != 0 {
					t := totals[model]
					t.input += u.InputTokens
					t.output += u.OutputTokens
					t.cacheRead += u.CacheReadInputTokens
					t.cacheWrite += u.CacheCreationInputTokens
					totals[model] = t
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return totals, readErr
		}
	}
	return totals, nil
}

func (m *terminalManager) handleStdin(p protocol.TerminalStdinPayload) {
	s := m.lookup(p.SessionID)
	if s == nil {
		return
	}
	data, err := base64.StdEncoding.DecodeString(p.Data)
	if err != nil {
		return
	}
	if _, err := s.pty.Write(data); err != nil {
		m.d.logger.Debug("terminal stdin write failed", "session_id", p.SessionID, "error", err)
	}
}

func (m *terminalManager) handleResize(p protocol.TerminalResizePayload) {
	s := m.lookup(p.SessionID)
	if s == nil {
		return
	}
	_ = pty.Setsize(s.pty, &pty.Winsize{Cols: clampDim(p.Cols, 80), Rows: clampDim(p.Rows, 24)})
}

func (m *terminalManager) handleClose(p protocol.TerminalClosePayload) {
	m.mu.Lock()
	s := m.sessions[p.SessionID]
	delete(m.sessions, p.SessionID)
	m.mu.Unlock()
	if s != nil {
		s.close()
	}
}

func (m *terminalManager) lookup(sessionID string) *terminalSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[sessionID]
}

// resolveProviderPath returns the executable path for a provider, falling back
// to the bare command name so PATH resolution still applies.
func (m *terminalManager) resolveProviderPath(provider string) string {
	if entry, ok := m.d.cfg.Agents[provider]; ok && entry.Path != "" {
		return entry.Path
	}
	if path, err := exec.LookPath(provider); err == nil {
		return path
	}
	return ""
}

// resolveWorkspaceDir returns the absolute path the terminal is hard-confined
// to: WorkspacesRoot/<workspace_id>. The workspace_id comes from the server
// (already a validated UUID), but this re-checks it cannot escape the root —
// no path separators, no "..", and the joined path must stay under
// WorkspacesRoot. Returns "" (caller refuses the open) on any violation. The
// directory is created if missing so a brand-new workspace still gets a shell.
func (m *terminalManager) resolveWorkspaceDir(workspaceID string) string {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return ""
	}
	if strings.ContainsAny(workspaceID, `/\`) || strings.Contains(workspaceID, "..") {
		return ""
	}
	root := strings.TrimSpace(m.d.cfg.WorkspacesRoot)
	if root == "" {
		return ""
	}
	dir := filepath.Join(root, workspaceID)
	// Defense in depth: the joined path must remain inside the root.
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		m.d.logger.Warn("terminal: failed to create workspace dir", "dir", dir, "error", err)
		return ""
	}
	return dir
}

// terminalSettingsArg builds the inline JSON passed to `claude --settings` to
// confine the interactive terminal. It is session-scoped (the flag affects
// only this process), so task-execution agents are not impacted.
//
// Two layers:
//
//  1. permissions — gate claude's built-in tools (Read/Edit/Write/Bash).
//     deny beats allow and cannot be approved away. Secret reads + network /
//     destructive bash are hard-blocked; file tools auto-allow only inside the
//     workspace folder.
//
//  2. sandbox (macOS Seatbelt / Linux bubblewrap) — an OS-level boundary on
//     the Bash subprocess tree, the part the permission layer alone cannot
//     enforce. With it on, a Bash command (and ANY child it spawns —
//     python -c open(...), redirects, pipes) can only WRITE inside the
//     workspace folder + the session temp dir, and cannot READ the denied
//     secret directories. claude's own process (auth, config, API calls) is
//     NOT sandboxed, so the session keeps working normally.
//     allowUnsandboxedCommands:false + failIfUnavailable:true make this a hard
//     guarantee: if the sandbox can't be established, the terminal refuses to
//     run rather than falling back to an unconfined shell.
func terminalSettingsArg(workDir string) string {
	// Absolute path specifier for Read/Edit/Write permission rules uses a
	// leading "//". sandbox paths use ordinary absolute / ~ forms.
	abs := "//" + strings.TrimPrefix(workDir, "/")
	// ~/.ssh is intentionally NOT here: full git (push/pull/clone over SSH)
	// needs the user's SSH key + known_hosts, so it is carved into the sandbox
	// read/write allow-list below. The other secret stores stay hard-denied.
	secretDirs := []string{
		"~/.aws",
		"~/.gnupg",
		"~/.kube",
		"~/.config/gcloud",
		"~/Library/Keychains",
	}
	settings := map[string]any{
		"permissions": map[string]any{
			"defaultMode": "default",
			"allow": []string{
				"Read(" + abs + "/**)",
				"Edit(" + abs + "/**)",
				"Write(" + abs + "/**)",
				// Full git: never prompt or deny any git subcommand.
				"Bash(git *)",
			},
			"deny": []string{
				"Read(~/.ssh/**)",
				"Read(~/.aws/**)",
				"Read(~/.gnupg/**)",
				"Read(~/.kube/**)",
				"Read(~/.config/gcloud/**)",
				"Read(~/.npmrc)",
				"Read(~/.netrc)",
				"Read(**/.env)",
				"Read(**/.env.*)",
				"Read(**/*.pem)",
				"Read(**/*.key)",
				"Read(**/id_rsa*)",
				"Read(**/id_ed25519*)",
				"Bash(curl *)",
				"Bash(wget *)",
				"Bash(nc *)",
				"Bash(ssh *)",
				"Bash(scp *)",
				"Bash(sftp *)",
				"Bash(telnet *)",
				"Bash(sudo *)",
				"Bash(rm -rf /)",
				"Bash(rm -rf /*)",
				"Bash(rm -rf ~)",
				"Bash(rm -rf ~/*)",
			},
		},
		"sandbox": map[string]any{
			"enabled":                  true,
			"failIfUnavailable":        true,
			"allowUnsandboxedCommands": false,
			// Network egress is otherwise denied by the sandbox proxy. Whitelist
			// the common git hosts so full git (push/pull/clone/fetch over SSH or
			// HTTPS) works. Matcher: "*." is a suffix match, bare host is exact —
			// there is no global wildcard, so hosts are listed explicitly.
			"network": map[string]any{
				"allowedDomains": []string{
					"github.com", "*.github.com", "*.githubusercontent.com",
					"gitlab.com", "*.gitlab.com",
					"bitbucket.org", "*.bitbucket.org",
				},
			},
			"filesystem": map[string]any{
				// WRITE confined to the workspace folder (+ session temp, which
				// is allowed by default).
				"allowWrite": []string{workDir, "~/.ssh"},
				// READ confinement: deny the entire user home, then carve back
				// the workspace folder plus the language toolchains that live
				// under home (node via nvm, python via conda/pyenv, etc.) so
				// commands still execute. System dirs (/usr, /opt, /System,
				// /bin, $TMPDIR) are NOT under ~ and stay readable — they hold
				// only public OS/runtime files, not user data. Net effect: a
				// Bash subprocess (and its children) can read the workspace and
				// run tools, but cannot read ANY other user data — other repos,
				// ~/Documents, ~/.ssh, ~/.aws, browser profiles, etc.
				// allowRead overrides denyRead for overlapping paths.
				"denyRead": append([]string{"~/"}, secretDirs...),
				"allowRead": []string{
					workDir,
					// JS / Node
					"~/.nvm", "~/.npm", "~/.bun", "~/.pnpm-store", "~/.yarn",
					// Python
					"~/miniconda", "~/miniconda3", "~/anaconda3", "~/.pyenv", "~/.conda",
					// Other runtimes / version managers
					"~/.local", "~/.cargo", "~/.rustup", "~/.rbenv", "~/go", "~/sdkman",
					// Shared caches + git identity (non-secret)
					"~/.cache", "~/.gitconfig",
					// SSH key + known_hosts for full git over SSH.
					"~/.ssh",
				},
			},
		},
	}
	b, err := json.Marshal(settings)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func (s *terminalSession) close() {
	s.closeOnce.Do(func() {
		if s.pty != nil {
			_ = s.pty.Close()
		}
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
	})
}

// clampDim guards against a zero/garbage dimension from the client.
func clampDim(v, fallback uint16) uint16 {
	if v == 0 {
		return fallback
	}
	return v
}
