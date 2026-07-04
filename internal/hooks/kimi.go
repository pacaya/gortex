package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// RunKimi handles the Kimi Code CLI hook wire shape. Kimi consumes plain
// stdout as hook-added context, so this dispatcher intentionally avoids
// Claude-style structured additionalContext JSON.
func RunKimi() {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	runKimi(data)
}

func runKimi(data []byte) {
	var input struct {
		HookEventName string         `json:"hook_event_name"`
		CWD           string         `json:"cwd"`
		ToolName      string         `json:"tool_name"`
		ToolInput     map[string]any `json:"tool_input"`
		Prompt        any            `json:"prompt"`
	}
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	switch input.HookEventName {
	case "UserPromptSubmit", "PreToolUse":
	default:
		return
	}
	if !kimiGortexEnabledProject(input.CWD) {
		return
	}
	switch input.HookEventName {
	case "UserPromptSubmit":
		ctx := buildUserPromptSubmitContext(input.HookEventName, kimiPromptText(input.Prompt))
		if ctx == "" {
			return
		}
		fmt.Print(ctx)
	case "PreToolUse":
		runKimiPreToolUse(input.ToolName, input.ToolInput)
	}
}

func runKimiPreToolUse(toolName string, toolInput map[string]any) {
	if !kimiGortexReadPreToolUseTool(toolName) {
		return
	}
	ctx := gortexReadNudge(toolName, toolInput)
	if ctx == "" {
		return
	}
	fmt.Print(ctx)
}

func kimiGortexReadPreToolUseTool(toolName string) bool {
	switch strings.TrimSpace(toolName) {
	case gortexReadFileTool, gortexEditingContextTool, "read_file", "get_editing_context":
		return true
	default:
		return false
	}
}

func kimiPromptText(v any) string {
	switch p := v.(type) {
	case string:
		return p
	case []any:
		var parts []string
		for _, item := range p {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			if typ != "" && typ != "text" {
				continue
			}
			text, _ := m["text"].(string)
			if strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func kimiGortexEnabledProject(cwd string) bool {
	dir := strings.TrimSpace(cwd)
	if dir == "" {
		return false
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return false
	}
	for {
		if kimiProjectMCPHasGortex(abs) || gortexProjectMarker(abs) {
			return true
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return false
		}
		abs = parent
	}
}

func kimiProjectMCPHasGortex(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, ".kimi-code", "mcp.json"))
	if err != nil {
		return false
	}
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return false
	}
	servers, ok := root["mcpServers"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = servers["gortex"].(map[string]any)
	return ok
}

func gortexProjectMarker(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, ".gortex.yaml")); err == nil {
		return true
	}
	info, err := os.Stat(filepath.Join(dir, ".gortex"))
	if err != nil || !info.IsDir() {
		return false
	}
	data, err := os.ReadFile(filepath.Join(dir, ".gortex", ".gitignore"))
	if err != nil {
		return false
	}
	normalized := strings.TrimSpace(string(data))
	return strings.Contains(normalized, "Gortex-managed") || normalized == "*"
}
