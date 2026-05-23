package mcp

import (
	"fmt"
	"regexp"

	"github.com/mark3labs/mcp-go/mcp"
)

// toolNamePattern is the tool-name convention enforced by the MCP
// ecosystem (and the Anthropic tool API): a lowercase identifier of
// letters, digits and underscores, 1..64 characters.
var toolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// SchemaViolation is one problem found in a tool's MCP schema by
// LintToolSchema. Tool names the offending tool, Rule the convention
// broken, Detail a human-readable explanation.
type SchemaViolation struct {
	Tool   string `json:"tool"`
	Rule   string `json:"rule"`
	Detail string `json:"detail"`
}

func (v SchemaViolation) String() string {
	return fmt.Sprintf("%s [%s]: %s", v.Tool, v.Rule, v.Detail)
}

// LintToolSchema checks one tool's MCP schema against the spec
// conventions every Gortex tool must satisfy:
//
//   - name is a lowercase [a-z0-9_] identifier, 1..64 chars
//   - description is non-empty and carries no control characters
//   - the input schema, when present, has type "object"
//   - every property declares a "type" (or a "$ref")
//   - every entry in `required` names a declared property
//
// It returns every violation found (nil when the tool is clean) so a
// release-gating test can lint the whole surface in one pass and
// report all problems at once.
func LintToolSchema(tool mcp.Tool) []SchemaViolation {
	var out []SchemaViolation
	add := func(rule, detail string) {
		out = append(out, SchemaViolation{Tool: tool.Name, Rule: rule, Detail: detail})
	}

	switch {
	case tool.Name == "":
		add("name", "tool name is empty")
	case !toolNamePattern.MatchString(tool.Name):
		add("name", fmt.Sprintf("name %q is not a lowercase [a-z0-9_] identifier of 1..64 chars", tool.Name))
	}

	if tool.Description == "" {
		add("description", "description is empty")
	} else if scrubControlChars(tool.Description) != tool.Description {
		add("description", "description carries control characters or ANSI escapes")
	}

	schema := tool.InputSchema
	hasSchema := schema.Type != "" || len(schema.Properties) > 0 || len(schema.Required) > 0
	if hasSchema {
		if schema.Type != "" && schema.Type != "object" {
			add("input_schema", fmt.Sprintf("input schema type is %q, want \"object\"", schema.Type))
		}
		for propName, raw := range schema.Properties {
			m, ok := raw.(map[string]any)
			if !ok {
				add("property", fmt.Sprintf("property %q is not a JSON-Schema object", propName))
				continue
			}
			_, hasType := m["type"]
			_, hasRef := m["$ref"]
			if !hasType && !hasRef {
				add("property", fmt.Sprintf("property %q declares no \"type\"", propName))
			}
		}
		for _, req := range schema.Required {
			if _, ok := schema.Properties[req]; !ok {
				add("required", fmt.Sprintf("required property %q is not declared in properties", req))
			}
		}
	}
	return out
}

// LintAllTools lints every tool currently registered live with the
// server. The lazy split is off by default, so the full surface is
// already live; if a test or runtime sets GORTEX_LAZY_TOOLS=1 to
// exercise the deferred path, clear it before linting. Returns every
// violation across every tool.
func LintAllTools(s *Server) []SchemaViolation {
	var out []SchemaViolation
	for _, st := range s.mcpServer.ListTools() {
		out = append(out, LintToolSchema(st.Tool)...)
	}
	return out
}
