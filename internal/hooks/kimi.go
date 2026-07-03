package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// RunKimi handles the Kimi Code CLI hook wire shape. PR 1 intentionally
// supports only UserPromptSubmit: Kimi reliably appends normal stdout from that
// event to the turn context, while PreToolUse/PostToolUse need separate
// contract work.
func RunKimi() {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	runKimi(data)
}

func runKimi(data []byte) {
	var input struct {
		HookEventName string `json:"hook_event_name"`
		Prompt        any    `json:"prompt"`
	}
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	if input.HookEventName != "UserPromptSubmit" {
		return
	}
	ctx := buildUserPromptSubmitContext(input.HookEventName, kimiPromptText(input.Prompt))
	if ctx == "" {
		return
	}
	fmt.Print(ctx)
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
