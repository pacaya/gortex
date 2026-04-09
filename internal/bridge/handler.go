// Package bridge exposes Gortex MCP tools over HTTP/JSON.
// It provides the general-purpose HTTP handler used by both the standalone
// bridge command and the eval server.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

// Handler wraps an MCP server's tool dispatch as an HTTP handler.
// It exposes /health, /tools, /tool/{tool_name}, and /stats endpoints.
type Handler struct {
	mcpServer *mcpserver.MCPServer
	graph     *graph.Graph
	version   string
	logger    *zap.Logger
	mux       *http.ServeMux
	startTime time.Time
}

// NewHandler creates an HTTP handler that dispatches to MCP tools.
func NewHandler(mcpServer *mcpserver.MCPServer, g *graph.Graph, version string, logger *zap.Logger) *Handler {
	h := &Handler{
		mcpServer: mcpServer,
		graph:     g,
		version:   version,
		logger:    logger,
		mux:       http.NewServeMux(),
		startTime: time.Now(),
	}
	h.registerRoutes()
	return h
}

// Mux returns the underlying ServeMux so sub-handlers can register
// additional routes (e.g. eval-specific /augment endpoint).
func (h *Handler) Mux() *http.ServeMux { return h.mux }

// Graph returns the graph instance for sub-handlers that need direct access.
func (h *Handler) Graph() *graph.Graph { return h.graph }

// ServeHTTP implements http.Handler with panic recovery middleware.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			stack := debug.Stack()
			h.logger.Error("panic recovered in HTTP handler",
				zap.Any("panic", rec),
				zap.String("stack", string(stack)),
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
			)
			WriteJSONError(w, http.StatusInternalServerError, "internal server error")
		}
	}()
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) registerRoutes() {
	h.mux.HandleFunc("GET /health", h.handleHealth)
	h.mux.HandleFunc("GET /tools", h.handleListTools)
	h.mux.HandleFunc("POST /tool/", h.handleToolCall)
	h.mux.HandleFunc("GET /stats", h.handleStats)
}

// --- /health ---

// HealthResponse is the JSON structure for the /health endpoint.
type HealthResponse struct {
	Status        string  `json:"status"`
	Indexed       bool    `json:"indexed"`
	Nodes         int     `json:"nodes"`
	Edges         int     `json:"edges"`
	Version       string  `json:"version"`
	UptimeSeconds float64 `json:"uptime_seconds"`
}

func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	stats := h.graph.Stats()
	resp := HealthResponse{
		Status:        "ok",
		Indexed:       stats.TotalNodes > 0,
		Nodes:         stats.TotalNodes,
		Edges:         stats.TotalEdges,
		Version:       h.version,
		UptimeSeconds: time.Since(h.startTime).Seconds(),
	}
	WriteJSON(w, http.StatusOK, resp)
}

// --- /tools ---

type toolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (h *Handler) handleListTools(w http.ResponseWriter, _ *http.Request) {
	tools := h.mcpServer.ListTools()
	result := make([]toolInfo, 0, len(tools))
	for name, t := range tools {
		result = append(result, toolInfo{
			Name:        name,
			Description: t.Tool.Description,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	WriteJSON(w, http.StatusOK, result)
}

// --- /tool/{name} ---

// ToolRequest is the expected JSON body for POST /tool/{tool_name}.
type ToolRequest struct {
	Arguments map[string]any `json:"arguments"`
}

// ToolResponse wraps the MCP tool call result for JSON serialization.
type ToolResponse struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ToolContent is a simplified content item from the MCP tool result.
type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func (h *Handler) handleToolCall(w http.ResponseWriter, r *http.Request) {
	toolName := strings.TrimPrefix(r.URL.Path, "/tool/")
	if toolName == "" {
		WriteJSONError(w, http.StatusBadRequest, "missing tool name in path")
		return
	}

	tool := h.mcpServer.GetTool(toolName)
	if tool == nil {
		available := h.availableToolNames()
		WriteJSON(w, http.StatusNotFound, map[string]any{
			"error":           "tool_not_found",
			"message":         fmt.Sprintf("tool '%s' not found", toolName),
			"available_tools": available,
		})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		WriteJSONError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var args map[string]any
	if len(body) > 0 {
		var req ToolRequest
		if err := json.Unmarshal(body, &req); err != nil {
			if err2 := json.Unmarshal(body, &args); err2 != nil {
				WriteJSONError(w, http.StatusBadRequest, fmt.Sprintf("malformed JSON: %s", err.Error()))
				return
			}
		} else {
			args = req.Arguments
			if args == nil {
				_ = json.Unmarshal(body, &args)
			}
		}
	}

	mcpReq := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      toolName,
			Arguments: args,
		},
	}

	result, err := tool.Handler(r.Context(), mcpReq)
	if err != nil {
		h.logger.Error("tool call failed",
			zap.String("tool", toolName),
			zap.Error(err),
		)
		WriteJSON(w, http.StatusInternalServerError, map[string]any{
			"error":   "tool_error",
			"message": err.Error(),
		})
		return
	}

	resp := ToolResponse{
		IsError: result.IsError,
	}
	for _, c := range result.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			resp.Content = append(resp.Content, ToolContent{
				Type: "text",
				Text: tc.Text,
			})
		}
	}

	WriteJSON(w, http.StatusOK, resp)
}

// --- /stats ---

// StatsResponse is the JSON structure for the /stats endpoint.
type StatsResponse struct {
	TotalNodes int            `json:"total_nodes"`
	TotalEdges int            `json:"total_edges"`
	ByKind     map[string]int `json:"by_kind"`
	ByLanguage map[string]int `json:"by_language"`
}

func (h *Handler) handleStats(w http.ResponseWriter, _ *http.Request) {
	stats := h.graph.Stats()
	resp := StatsResponse{
		TotalNodes: stats.TotalNodes,
		TotalEdges: stats.TotalEdges,
		ByKind:     stats.ByKind,
		ByLanguage: stats.ByLanguage,
	}
	WriteJSON(w, http.StatusOK, resp)
}

// --- Tool invocation helper ---

// CallTool invokes an MCP tool by name and returns the concatenated text content.
// Returns empty string on error or if the tool is not found.
func (h *Handler) CallTool(ctx context.Context, toolName string, args map[string]any) string {
	tool := h.mcpServer.GetTool(toolName)
	if tool == nil {
		return ""
	}

	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      toolName,
			Arguments: args,
		},
	}

	result, err := tool.Handler(ctx, req)
	if err != nil {
		h.logger.Debug("internal tool call failed",
			zap.String("tool", toolName),
			zap.Error(err),
		)
		return ""
	}

	var sb strings.Builder
	for _, c := range result.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// --- Helpers ---

func (h *Handler) availableToolNames() []string {
	tools := h.mcpServer.ListTools()
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// WriteJSON writes a JSON response with the given status code.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteJSONError writes a JSON error response.
func WriteJSONError(w http.ResponseWriter, status int, message string) {
	WriteJSON(w, status, map[string]string{
		"error":   http.StatusText(status),
		"message": message,
	})
}
