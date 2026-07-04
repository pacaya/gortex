package hooks

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/daemon"
)

// hookCWD carries the working directory of the in-flight hook invocation so the
// daemon-socket fallback in callServerTool can scope its tools/call handshake to
// the right workspace. A hook binary is single-shot — it reads one stdin
// payload, handles one event, and exits — so a package-level value set once at
// dispatch entry is race-free in production; tests set-and-restore it. The
// mutex is only for tidiness under `go test -race`, where several dispatchers
// run in one process.
var (
	hookCWDMu sync.RWMutex
	hookCWD   string
)

// setHookCWD records the payload CWD for the current hook invocation. Pass ""
// to clear it (dispatchers defer a clear so a value never leaks across the
// sequential tests sharing one process).
func setHookCWD(cwd string) {
	hookCWDMu.Lock()
	hookCWD = cwd
	hookCWDMu.Unlock()
}

// loadHookCWD returns the CWD recorded for the current hook invocation, or ""
// when none was set (the pure-HTTP unit tests, or a dispatcher that opted out
// of the socket fallback).
func loadHookCWD() string {
	hookCWDMu.RLock()
	defer hookCWDMu.RUnlock()
	return hookCWD
}

// callServerToolDaemonFn is the seam production uses to reach the daemon over
// its unix socket; tests swap it so a hook never touches a real daemon.
var callServerToolDaemonFn = callServerToolViaDaemon

// callServerToolTimeout bounds the whole daemon round-trip (dial + handshake +
// tools/call) per call, so a wedged daemon can never stall a Stop / subagent
// hook past the host's own hook timeout.
const callServerToolTimeout = 5 * time.Second

// callServerToolViaDaemon runs one MCP tools/call against the local daemon over
// its AF_UNIX socket and returns the first text content block, or "" on any
// error. cwd scopes the handshake to the caller's workspace so tools that read
// the working tree (detect_changes) or the active project (analyze) resolve the
// right repo. Mirrors fileIndexedViaDaemon's transport; kept separate so the
// file-indexed probe stays a tight count-only path.
func callServerToolViaDaemon(cwd, name string, args map[string]any) string {
	if args == nil {
		args = map[string]any{}
	}
	client, err := daemon.Dial(daemon.Handshake{
		Mode:       daemon.ModeMCP,
		ClientName: "gortex-hook",
		CWD:        cwd,
	})
	if err != nil {
		return ""
	}
	defer client.Close()
	_ = client.Conn.SetDeadline(time.Now().Add(callServerToolTimeout))

	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": args,
		},
	})
	if err != nil {
		return ""
	}
	if err := client.WriteMCPFrame(frame); err != nil {
		return ""
	}
	resp, err := client.ReadMCPFrame()
	if err != nil {
		return ""
	}
	return parseToolCallText(resp)
}

// parseToolCallText unwraps a tools/call JSON-RPC response to the first content
// block's text. Returns "" on a tool error or a shape mismatch so the caller
// treats it the same as an unreachable bridge.
func parseToolCallText(resp []byte) string {
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &rpc); err != nil {
		return ""
	}
	if rpc.Result.IsError || len(rpc.Result.Content) == 0 {
		return ""
	}
	return rpc.Result.Content[0].Text
}
