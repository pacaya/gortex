package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"sync"
	"time"
)

// Session represents one proxy or CLI connection to the daemon. Per-session
// state (recent activity, symbol history, token stats for this client)
// lives here; shared state (the graph, feedback store, cumulative savings)
// lives on the Server.
//
// A Session is created on a successful handshake and destroyed when its
// socket connection closes. The daemon routes every inbound frame to its
// session by looking up the net.Conn in the session registry.
type Session struct {
	ID         string
	Mode       ConnectionMode
	CWD        string
	ClientName string
	// ClientVersion is the version reported by the MCP client in its
	// `initialize` request (`params.clientInfo.version`). Empty until
	// the daemon dispatcher sees that frame; the env-var sniff in
	// `cmd/gortex/proxy.go::detectClientName` only fills ClientName.
	ClientVersion string
	// ClientNameSource records where ClientName came from so the
	// MCP-frame snooper can decide whether to overwrite it. "handshake"
	// is the env-var fallback the proxy posts at connect time;
	// "initialize" is the authoritative MCP-protocol value. Anything
	// from "initialize" wins over any "handshake" — including the
	// "unknown" string the proxy uses when env-var detection fails.
	ClientNameSource string
	ClientPID        int
	DefaultRepo      string
	ActiveProject    string
	StartedAt        time.Time

	// ToolSpec / ToolMode are the client-forwarded tool-surface
	// preference (GORTEX_TOOLS / --tools + mode) from the handshake. The
	// daemon resolves the effective per-session tool surface from these
	// so a client can scope (or widen) its own pipe while the daemon keeps
	// serving the full graph to everyone else. Empty = no preference.
	ToolSpec string
	ToolMode string

	// Conn is the underlying socket. Kept for close-on-shutdown and
	// logging; handlers should not read from or write to it directly —
	// framing is the transport's job.
	Conn net.Conn

	// Per-session mutable state that will move over from internal/mcp's
	// Server during the session-isolation refactor. Left as interface{}
	// for now so the types can evolve without churning this file every
	// iteration — the refactor will replace this with concrete pointers.
	SessionState any
	SymHistory   any
	TokenStats   any

	// remoteOverrides is the per-session enable/disable layer over the
	// global roster: slug -> enabled. An absent slug means "no
	// override" (the global Enabled state wins). It is ephemeral by
	// construction — freed when the *Session is dropped on disconnect
	// via either teardown path (Remove for an AF_UNIX session,
	// RemoveByID for a detached /mcp session) — so no explicit cleanup
	// is needed. Guarded by mu.
	remoteOverrides map[string]bool

	// mu protects ClientName / ClientVersion / ClientNameSource and
	// remoteOverrides, which can be updated by the dispatcher and the
	// proxy-toggle tools mid-session.
	mu sync.RWMutex
}

// SetClientInfo updates the session's client metadata from the MCP
// `initialize` frame. Called by the daemon dispatcher when it sees
// the first `initialize` request on this session. Idempotent — a
// second call (e.g. on protocol re-init) just overwrites.
func (s *Session) SetClientInfo(name, version string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if name != "" {
		s.ClientName = name
		s.ClientNameSource = "initialize"
	}
	if version != "" {
		s.ClientVersion = version
	}
	s.mu.Unlock()
}

// SetRemoteOverride sets a per-session enable/disable override for a
// remote slug. It wins over the remote's global Enabled state for the
// lifetime of this session only.
func (s *Session) SetRemoteOverride(slug string, enabled bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.remoteOverrides == nil {
		s.remoteOverrides = make(map[string]bool)
	}
	s.remoteOverrides[slug] = enabled
	s.mu.Unlock()
}

// ClearRemoteOverride removes a per-session override so the remote
// reverts to its global Enabled state.
func (s *Session) ClearRemoteOverride(slug string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.remoteOverrides, slug)
	s.mu.Unlock()
}

// RemoteOverrides returns a copy of the per-session override map under
// the read lock, so callers can iterate without racing a concurrent
// toggle. nil when no override has been set.
func (s *Session) RemoteOverrides() map[string]bool {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.remoteOverrides) == 0 {
		return nil
	}
	out := make(map[string]bool, len(s.remoteOverrides))
	for k, v := range s.remoteOverrides {
		out[k] = v
	}
	return out
}

// SnapshotClientInfo returns the current client name/version pair
// safely under the session lock. Used by the status path which reads
// while the dispatcher may be writing.
func (s *Session) SnapshotClientInfo() (name, version string) {
	if s == nil {
		return "", ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ClientName, s.ClientVersion
}

// SessionRegistry tracks active sessions. Safe for concurrent access from
// the accept goroutine and the control-surface handlers.
type SessionRegistry struct {
	mu       sync.RWMutex
	sessions map[string]*Session // session_id → Session
	byConn   map[net.Conn]*Session
}

func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{
		sessions: make(map[string]*Session),
		byConn:   make(map[net.Conn]*Session),
	}
}

// Register creates and stores a new session for the given connection.
// Called after a successful handshake. Generates the session ID.
func (r *SessionRegistry) Register(conn net.Conn, h Handshake) *Session {
	s := &Session{
		ID:               newSessionID(),
		Mode:             h.Mode,
		CWD:              h.CWD,
		ClientName:       h.ClientName,
		ClientNameSource: "handshake",
		ClientPID:        h.PID,
		ToolSpec:         h.Tools,
		ToolMode:         h.ToolsMode,
		StartedAt:        time.Now(),
		Conn:             conn,
	}
	r.mu.Lock()
	r.sessions[s.ID] = s
	r.byConn[conn] = s
	r.mu.Unlock()
	return s
}

// RegisterDetached creates a session that isn't bound to a net.Conn —
// used by HTTP-side transports (Streamable HTTP, future SSE/WebSocket
// adapters) where the daemon doesn't own a persistent socket. The
// supplied id becomes the session ID verbatim so the transport's own
// session-id space (e.g. streamable.SessionStore) lines up with the
// daemon's status/metrics surface; pass "" to auto-generate one.
func (r *SessionRegistry) RegisterDetached(id string, h Handshake) *Session {
	if id == "" {
		id = newSessionID()
	}
	s := &Session{
		ID:               id,
		Mode:             h.Mode,
		CWD:              h.CWD,
		ClientName:       h.ClientName,
		ClientNameSource: "handshake",
		ClientPID:        h.PID,
		ToolSpec:         h.Tools,
		ToolMode:         h.ToolsMode,
		StartedAt:        time.Now(),
	}
	r.mu.Lock()
	r.sessions[s.ID] = s
	r.mu.Unlock()
	return s
}

// RemoveByID deletes a session by id (used by detached sessions which
// have no net.Conn to key off). Idempotent.
func (r *SessionRegistry) RemoveByID(id string) *Session {
	if id == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[id]
	if s == nil {
		return nil
	}
	delete(r.sessions, id)
	if s.Conn != nil {
		delete(r.byConn, s.Conn)
	}
	return s
}

// GetByID returns a session by its id, or nil when no session is
// registered under that id. Used by detached-session lookup paths.
func (r *SessionRegistry) GetByID(id string) *Session {
	if id == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sessions[id]
}

// Remove deletes the session for a connection. Idempotent — safe to call
// from both the accept-loop's defer and the shutdown path.
func (r *SessionRegistry) Remove(conn net.Conn) *Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.byConn[conn]
	if s == nil {
		return nil
	}
	delete(r.byConn, conn)
	delete(r.sessions, s.ID)
	return s
}

// Get returns the session for a connection, or nil if the connection hasn't
// completed its handshake yet (or was already removed).
func (r *SessionRegistry) Get(conn net.Conn) *Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byConn[conn]
}

// Count returns the number of live sessions — used by the status command
// and for metrics.
func (r *SessionRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sessions)
}

// SweepDead removes every session whose originating client process (by its
// handshake PID) is no longer alive, closing the session's connection. Sessions
// with no recorded PID (0 — detached/HTTP transports, or a client that reported
// none) are left untouched, since liveness can't be judged from a PID we don't
// have. Returns the removed sessions so the caller can log / adjust metrics.
// isAlive is injected (platform.ProcessAlive in production) so the sweep is
// testable without spawning real processes.
func (r *SessionRegistry) SweepDead(isAlive func(int) bool) []*Session {
	r.mu.Lock()
	var dead []*Session
	for id, s := range r.sessions {
		if s.ClientPID <= 0 || isAlive(s.ClientPID) {
			continue
		}
		dead = append(dead, s)
		delete(r.sessions, id)
		if s.Conn != nil {
			delete(r.byConn, s.Conn)
		}
	}
	r.mu.Unlock()
	// Close connections outside the lock — Close can block.
	for _, s := range dead {
		if s.Conn != nil {
			_ = s.Conn.Close()
		}
	}
	return dead
}

// All returns a snapshot of every live session. The caller must not
// mutate the returned Session objects; they're shared with the registry.
func (r *SessionRegistry) All() []*Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, s)
	}
	return out
}

// newSessionID generates a short URL-safe identifier. 8 bytes of entropy
// gives us 16 hex chars — collision-resistant enough for a per-user
// single-process registry without bloating log lines.
func newSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "sess_" + hex.EncodeToString(b[:])
}
