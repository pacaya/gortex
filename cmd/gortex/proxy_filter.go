package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	gortexmcp "github.com/zzet/gortex/internal/mcp"
)

// The stdio proxy relays newline-delimited JSON-RPC frames between the
// MCP client (stdin/stdout) and the daemon socket. When a tool surface
// is active (`gortex mcp --tools` / GORTEX_TOOLS), these helpers filter
// the relayed traffic per connection — the daemon is never touched, so
// one client can scope its own pipe while the daemon keeps serving the
// full surface to everyone else. The filter runs from the first
// tools/list, so it works on every client (no tools/list_changed needed).

// toolPolicyConfigFromFlags builds a ToolPolicyConfig from the
// --tools / --tools-mode flags. The config file is not consulted on the
// proxy path (it governs the daemon); GORTEX_TOOLS / GORTEX_TOOLS_MODE
// still override when NewToolSurface resolves it.
func toolPolicyConfigFromFlags(flagTools, flagMode string) gortexmcp.ToolPolicyConfig {
	var cfg gortexmcp.ToolPolicyConfig
	if flagTools != "" {
		preset, allow, deny := gortexmcp.ParseToolSpec(flagTools)
		cfg.Preset, cfg.Allow, cfg.Deny = preset, allow, deny
	}
	cfg.Mode = flagMode
	return cfg
}

// proxyToolSurface resolves the per-connection tool surface from the
// --tools / --tools-mode flags plus the GORTEX_TOOLS env override. An
// inactive surface (no flag, no env) leaves the proxy a raw byte pump.
func proxyToolSurface() *gortexmcp.ToolSurface {
	return gortexmcp.NewToolSurface(toolPolicyConfigFromFlags(mcpTools, mcpToolsMode), nil)
}

// clientToolPreference returns the raw tool-surface spec + mode this
// client wants, to hand to the daemon in the handshake so it can resolve
// the effective surface for this session authoritatively (the proxy's
// own byte-pump filter can only subtract from the daemon's list, never
// widen it — see proxy_filter.go and Handshake.Tools). GORTEX_TOOLS /
// GORTEX_TOOLS_MODE env win over the --tools / --tools-mode flags,
// mirroring the repo-wide "env overrides file/flag config" convention.
func clientToolPreference() (spec, mode string) {
	spec = strings.TrimSpace(os.Getenv("GORTEX_TOOLS"))
	if spec == "" {
		spec = strings.TrimSpace(mcpTools)
	}
	mode = strings.TrimSpace(os.Getenv("GORTEX_TOOLS_MODE"))
	if mode == "" {
		mode = strings.TrimSpace(mcpToolsMode)
	}
	return spec, mode
}

// gateToolCallFrame inspects one client→daemon frame. If it is a
// tools/call request for a tool the surface forbids, it returns a
// synthesized JSON-RPC error reply (to write back to the client) and
// gated=true — the original frame must NOT reach the daemon, so a client
// that hard-codes a hidden tool name cannot bypass the restriction.
// Only hide mode gates calls: in defer mode the surface trims tools/list
// but keeps every tool callable, mirroring the server-side defer
// semantics where tools_search promotion makes deferred tools live.
func gateToolCallFrame(line []byte, surface *gortexmcp.ToolSurface) (reply []byte, gated bool) {
	if !surface.GateCalls() {
		return nil, false
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, false // not a single JSON object — forward untouched
	}
	if msg.Method != "tools/call" || msg.Params.Name == "" || len(msg.ID) == 0 {
		return nil, false
	}
	if surface.Allows(msg.Params.Name) {
		return nil, false
	}
	out, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      msg.ID,
		"error": map[string]any{
			"code":    -32601,
			"message": "tool " + strconv.Quote(msg.Params.Name) + " is not available in this client's tool set (gortex mcp --tools)",
		},
	})
	if err != nil {
		return nil, false
	}
	return append(out, '\n'), true
}

// filterToolsListFrame rewrites a daemon→client frame: if it is a
// tools/list result, the tools array is filtered down to the surface.
// Every other frame — and any frame that doesn't parse, or where nothing
// is dropped — passes through verbatim.
func filterToolsListFrame(line []byte, surface *gortexmcp.ToolSurface) []byte {
	if !surface.Active() {
		return line
	}
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return line
	}
	resultRaw, ok := msg["result"]
	if !ok {
		return line
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		return line
	}
	toolsRaw, ok := result["tools"]
	if !ok {
		return line
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(toolsRaw, &tools); err != nil {
		return line
	}
	kept := make([]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		var nameOnly struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(t, &nameOnly); err != nil || surface.Allows(nameOnly.Name) {
			kept = append(kept, t)
		}
	}
	if len(kept) == len(tools) {
		return line // nothing dropped — preserve the original bytes
	}
	newTools, err := json.Marshal(kept)
	if err != nil {
		return line
	}
	result["tools"] = newTools
	newResult, err := json.Marshal(result)
	if err != nil {
		return line
	}
	msg["result"] = newResult
	out, err := json.Marshal(msg)
	if err != nil {
		return line
	}
	return append(out, '\n')
}

// pumpRequestsFiltered relays client→daemon frames, gating disallowed
// tools/call frames with a synthesized error reply written to clientOut.
func pumpRequestsFiltered(src io.Reader, daemonW, clientOut io.Writer, outMu *sync.Mutex, surface *gortexmcp.ToolSurface) error {
	r := bufio.NewReaderSize(src, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if reply, gated := gateToolCallFrame(line, surface); gated {
				outMu.Lock()
				_, werr := clientOut.Write(reply)
				outMu.Unlock()
				if werr != nil {
					return werr
				}
			} else if _, werr := daemonW.Write(line); werr != nil {
				return werr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// pumpResponsesFiltered relays daemon→client frames, filtering tools/list
// results down to the surface.
func pumpResponsesFiltered(src io.Reader, clientOut io.Writer, outMu *sync.Mutex, surface *gortexmcp.ToolSurface) error {
	r := bufio.NewReaderSize(src, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			out := filterToolsListFrame(line, surface)
			outMu.Lock()
			_, werr := clientOut.Write(out)
			outMu.Unlock()
			if werr != nil {
				return werr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}
