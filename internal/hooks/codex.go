package hooks

import (
	"encoding/json"
	"io"
	"os"
)

// RunCodex handles the Codex hook wire shape. Codex support is deliberately
// soft-only: PreToolUse is forced through ModeEnrich, and PostToolUse only
// emits additionalContext.
func RunCodex(port int) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	runCodex(data, port)
}

func runCodex(data []byte, port int) {
	var peek struct {
		HookEventName string `json:"hook_event_name"`
		ToolName      string `json:"tool_name"`
	}
	if err := json.Unmarshal(data, &peek); err != nil {
		return
	}

	switch {
	case peek.HookEventName == "PreToolUse" && peek.ToolName == "Bash":
		runPreToolUse(data, port, ModeEnrich)
	case peek.HookEventName == "PostToolUse" && peek.ToolName == "Bash":
		runCodexPostToolUse(data, port)
	}
}

func runCodexPostToolUse(data []byte, port int) {
	var input postHookInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	if input.HookEventName != "PostToolUse" || input.ToolName != "Bash" {
		return
	}

	cmd, _ := input.ToolInput["command"].(string)
	classification := classifyBashCommand(cmd)
	switch classification.Action {
	case BashActionGrepLike:
		// Codex wraps grep/rg/ag in Bash. Re-label that narrow shape as Grep so
		// the existing PostToolUse enrichment can parse path:line output and do
		// the graph lookup without changing Claude Code behavior.
		input.ToolName = "Grep"
	case BashActionFindName:
		input.ToolName = "Glob"
	case BashActionReadSource:
		if classification.Path == "" {
			return
		}
		if input.ToolInput == nil {
			input.ToolInput = make(map[string]any)
		}
		input.ToolName = "Read"
		input.ToolInput["file_path"] = classification.Path
	default:
		return
	}

	normalized, err := json.Marshal(input)
	if err != nil {
		return
	}
	runPostToolUse(normalized, port)
}
