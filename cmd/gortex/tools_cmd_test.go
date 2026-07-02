package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// newToolsTestCmd resets the tools subcommand flag state and binds a buffer.
func newToolsTestCmd(t *testing.T, run func(*cobra.Command, []string) error) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	toolsIndex = "."
	toolsListCategory = ""
	toolsListMutating = false
	toolsListPreset = ""
	toolsListFormat = "text"
	toolsSearchLimit = 10
	toolsSearchFormat = "text"

	buf := &bytes.Buffer{}
	cmd := &cobra.Command{Use: "tools", RunE: run}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	return cmd, buf
}

// TestToolsList_RendersTable asserts `tools list` renders the descriptor table
// (NAME / CATEGORY / R/W / PRESETS / SUMMARY) from a canned tool_profile.
func TestToolsList_RendersTable(t *testing.T) {
	orig := toolsDaemonTool
	t.Cleanup(func() { toolsDaemonTool = orig })

	var gotTool string
	toolsDaemonTool = func(_ string, tool string, _ map[string]any) (json.RawMessage, error) {
		gotTool = tool
		return json.RawMessage(cannedToolProfileJSON), nil
	}

	cmd, buf := newToolsTestCmd(t, runToolsList)
	require.NoError(t, runToolsList(cmd, nil))
	require.Equal(t, "tool_profile", gotTool)

	out := buf.String()
	require.Contains(t, out, "NAME")
	require.Contains(t, out, "CATEGORY")
	require.Contains(t, out, "R/W")
	require.Contains(t, out, "PRESETS")
	require.Contains(t, out, "search_symbols")
	require.Contains(t, out, "edit_file")
	// A read tool is "R", a mutating tool is "R/W".
	require.Regexp(t, `search_symbols\s+nav\s+R\s`, out)
	require.Regexp(t, `edit_file\s+edit\s+R/W`, out)
}

// TestToolsList_FilterMutating asserts --mutating keeps only mutating tools.
func TestToolsList_FilterMutating(t *testing.T) {
	orig := toolsDaemonTool
	t.Cleanup(func() { toolsDaemonTool = orig })
	toolsDaemonTool = func(string, string, map[string]any) (json.RawMessage, error) {
		return json.RawMessage(cannedToolProfileJSON), nil
	}

	cmd, buf := newToolsTestCmd(t, runToolsList)
	toolsListMutating = true
	require.NoError(t, runToolsList(cmd, nil))
	out := buf.String()
	require.Contains(t, out, "edit_file")
	require.Contains(t, out, "rename_symbol")
	require.NotContains(t, out, "search_symbols")
	require.NotContains(t, out, "get_callers")
}

// TestToolsList_FilterCategoryAndPreset asserts --category and --preset filter
// the descriptor list client-side.
func TestToolsList_FilterCategoryAndPreset(t *testing.T) {
	orig := toolsDaemonTool
	t.Cleanup(func() { toolsDaemonTool = orig })
	toolsDaemonTool = func(string, string, map[string]any) (json.RawMessage, error) {
		return json.RawMessage(cannedToolProfileJSON), nil
	}

	t.Run("category", func(t *testing.T) {
		cmd, buf := newToolsTestCmd(t, runToolsList)
		toolsListCategory = "edit"
		require.NoError(t, runToolsList(cmd, nil))
		out := buf.String()
		require.Contains(t, out, "edit_file")
		require.NotContains(t, out, "search_symbols")
	})

	t.Run("preset core", func(t *testing.T) {
		cmd, buf := newToolsTestCmd(t, runToolsList)
		toolsListPreset = "core"
		require.NoError(t, runToolsList(cmd, nil))
		out := buf.String()
		require.Contains(t, out, "search_symbols") // core
		require.Contains(t, out, "edit_file")       // core
		require.NotContains(t, out, "rename_symbol") // edit only, not core
	})
}

// TestToolsList_JSON asserts --format json dumps the (filtered) descriptors
// array as parseable JSON.
func TestToolsList_JSON(t *testing.T) {
	orig := toolsDaemonTool
	t.Cleanup(func() { toolsDaemonTool = orig })
	toolsDaemonTool = func(string, string, map[string]any) (json.RawMessage, error) {
		return json.RawMessage(cannedToolProfileJSON), nil
	}

	cmd, buf := newToolsTestCmd(t, runToolsList)
	toolsListFormat = "json"
	require.NoError(t, runToolsList(cmd, nil))

	var descs []toolDescriptorCLI
	require.NoError(t, json.Unmarshal(buf.Bytes(), &descs))
	require.Len(t, descs, 4)
	require.Equal(t, "edit_file", descs[0].Name) // sorted by name
}

// TestToolDescriptorCLI_JSONIncludesDescription asserts the CLI-side mirror
// of mcp.ToolDescriptor serializes the new Description field.
func TestToolDescriptorCLI_JSONIncludesDescription(t *testing.T) {
	d := toolDescriptorCLI{Name: "get_callers", Summary: "Show who calls a function.", Description: "Show who calls a function.\nReturns the caller subgraph."}
	b, err := json.Marshal(d)
	require.NoError(t, err)
	require.Contains(t, string(b), `"description":"Show who calls a function.\nReturns the caller subgraph."`)

	var round toolDescriptorCLI
	require.NoError(t, json.Unmarshal(b, &round))
	require.Equal(t, d, round)
}

// cannedToolsSearchStructured is the structured tools_search payload shape (a
// `tools` array with name / description / input_schema).
const cannedToolsSearchStructured = `{
  "query": "callers",
  "tools": [
    {"name":"get_callers","description":"Show who calls a function across the graph.\nReturns the caller subgraph.","input_schema":{"type":"object","properties":{"id":{"type":"string"}}}},
    {"name":"get_call_chain","description":"Trace the transitive call chain from a function.","input_schema":{"type":"object","properties":{"id":{"type":"string"},"depth":{"type":"number"}}}}
  ]
}`

// cannedToolsSearchFunctionsBlock is the alternate <functions> text-block shape
// the tool also emits (parser must handle this fallback).
const cannedToolsSearchFunctionsBlock = `<functions>
<function>{"description":"Edit a file at a path.","name":"edit_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}</function>
</functions>
`

// TestToolsSearch_Structured asserts `tools search` lists matched names + the
// one-line summary, parsing the structured payload.
func TestToolsSearch_Structured(t *testing.T) {
	orig := toolsDaemonTool
	t.Cleanup(func() { toolsDaemonTool = orig })

	var gotArgs map[string]any
	toolsDaemonTool = func(_ string, tool string, args map[string]any) (json.RawMessage, error) {
		require.Equal(t, "tools_search", tool)
		gotArgs = args
		return json.RawMessage(cannedToolsSearchStructured), nil
	}

	cmd, buf := newToolsTestCmd(t, runToolsSearch)
	require.NoError(t, runToolsSearch(cmd, []string{"who", "calls"}))
	require.Equal(t, "who calls", gotArgs["query"], "multi-word query is joined")
	require.Equal(t, false, gotArgs["promote"], "a CLI search must not promote tools")

	out := buf.String()
	require.Contains(t, out, "get_callers")
	require.Contains(t, out, "Show who calls a function")
	require.Contains(t, out, "get_call_chain")
	// Only the first line of the description is shown.
	require.NotContains(t, out, "Returns the caller subgraph")
}

// TestToolsSearch_FunctionsBlock asserts the parser also handles the
// <functions> text-block fallback shape.
func TestToolsSearch_FunctionsBlock(t *testing.T) {
	orig := toolsDaemonTool
	t.Cleanup(func() { toolsDaemonTool = orig })
	toolsDaemonTool = func(string, string, map[string]any) (json.RawMessage, error) {
		// Deliver the block as a JSON string (a common transport shape).
		s, _ := json.Marshal(cannedToolsSearchFunctionsBlock)
		return json.RawMessage(s), nil
	}

	cmd, buf := newToolsTestCmd(t, runToolsSearch)
	require.NoError(t, runToolsSearch(cmd, []string{"edit"}))
	out := buf.String()
	require.Contains(t, out, "edit_file")
	require.Contains(t, out, "Edit a file at a path")
}

// TestToolsSearch_JSON asserts `tools search --format json` emits the full
// (untruncated) trimmed description per match, not just the first line.
func TestToolsSearch_JSON(t *testing.T) {
	orig := toolsDaemonTool
	t.Cleanup(func() { toolsDaemonTool = orig })
	toolsDaemonTool = func(string, string, map[string]any) (json.RawMessage, error) {
		return json.RawMessage(cannedToolsSearchStructured), nil
	}

	cmd, buf := newToolsTestCmd(t, runToolsSearch)
	toolsSearchFormat = "json"
	t.Cleanup(func() { toolsSearchFormat = "text" })
	require.NoError(t, runToolsSearch(cmd, []string{"callers"}))

	type searchResultJSON struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	var results []searchResultJSON
	require.NoError(t, json.Unmarshal(buf.Bytes(), &results))
	require.Len(t, results, 2)
	require.Equal(t, "get_callers", results[0].Name)
	require.Equal(t, "Show who calls a function across the graph.\nReturns the caller subgraph.", results[0].Description)
	require.Equal(t, "get_call_chain", results[1].Name)
}

// TestToolsDescribe asserts `tools describe` issues a select:<name> query and
// prints the tool name, description, and the indented parameter schema.
func TestToolsDescribe(t *testing.T) {
	orig := toolsDaemonTool
	t.Cleanup(func() { toolsDaemonTool = orig })

	var gotQuery string
	toolsDaemonTool = func(_ string, tool string, args map[string]any) (json.RawMessage, error) {
		require.Equal(t, "tools_search", tool)
		gotQuery, _ = args["query"].(string)
		return json.RawMessage(cannedToolsSearchStructured), nil
	}

	cmd, buf := newToolsTestCmd(t, runToolsDescribe)
	require.NoError(t, runToolsDescribe(cmd, []string{"get_callers"}))
	require.Equal(t, "select:get_callers", gotQuery)

	out := buf.String()
	require.Contains(t, out, "get_callers")
	require.Contains(t, out, "Show who calls a function")
	require.Contains(t, out, "parameters:")
	// The schema is rendered as indented JSON.
	require.Contains(t, out, `"type": "object"`)
	require.Contains(t, out, `"id"`)
}

// TestToolsDescribe_Unknown asserts an empty match prints a friendly note.
func TestToolsDescribe_Unknown(t *testing.T) {
	orig := toolsDaemonTool
	t.Cleanup(func() { toolsDaemonTool = orig })
	toolsDaemonTool = func(string, string, map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{"query":"select:nope","tools":[]}`), nil
	}

	cmd, buf := newToolsTestCmd(t, runToolsDescribe)
	require.NoError(t, runToolsDescribe(cmd, []string{"nope"}))
	require.Contains(t, buf.String(), "no tool named")
}

// TestToolsList_DaemonRequired asserts the real `tools list` path returns the
// actionable daemon-required error when no daemon tracks the repo.
func TestToolsList_DaemonRequired(t *testing.T) {
	cmd, _ := newToolsTestCmd(t, runToolsList)
	toolsIndex = t.TempDir()
	err := runToolsList(cmd, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "gortex track")
}

// TestParseFunctionsBlock_Multiple asserts the <functions> parser extracts
// every <function> entry.
func TestParseFunctionsBlock_Multiple(t *testing.T) {
	block := "<functions>\n" +
		`<function>{"description":"a","name":"alpha","parameters":{}}</function>` + "\n" +
		`<function>{"description":"b","name":"beta","parameters":{"type":"object"}}</function>` + "\n" +
		"</functions>\n"
	entries := parseFunctionsBlock(block)
	require.Len(t, entries, 2)
	require.Equal(t, "alpha", entries[0].Name)
	require.Equal(t, "beta", entries[1].Name)
	require.True(t, strings.Contains(string(entries[1].InputSchema), "object"))
}
