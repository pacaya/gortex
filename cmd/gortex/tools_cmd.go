package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

var (
	toolsIndex        string
	toolsListCategory string
	toolsListMutating bool
	toolsListPreset   string
	toolsListFormat   string
	toolsSearchLimit  int
	toolsSearchFormat string
)

// toolsDaemonTool is the daemon-tool relay seam so the tools subcommands can be
// unit-tested with a canned tool_profile / tools_search response.
var toolsDaemonTool = requireDaemonTool

var toolsCmd = &cobra.Command{
	Use:   "tools",
	Short: "Discover and describe the daemon's MCP tool surface",
	Long: `Introspects the MCP tool catalog the daemon publishes — list every tool with
its category / read-write classification / presets, full-text search the
catalog, or print one tool's full parameter schema. Requires a running daemon
that tracks the repo.`,
	SilenceUsage: true, // subcommands' daemon errors should read cleanly, not dump usage
}

var toolsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List every registered tool with its category, R/W class, and presets",
	RunE:  runToolsList,
}

var toolsSearchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Search the tool catalog and print matching tool names + summaries",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runToolsSearch,
}

var toolsDescribeCmd = &cobra.Command{
	Use:   "describe <name>",
	Short: "Print one tool's full parameter schema",
	Args:  cobra.ExactArgs(1),
	RunE:  runToolsDescribe,
}

func init() {
	toolsCmd.PersistentFlags().StringVar(&toolsIndex, "index", ".", "repository path the daemon must track")
	toolsCmd.PersistentFlags().StringVar(&toolsIndex, "repo", ".", "alias for --index")

	toolsListCmd.Flags().StringVar(&toolsListCategory, "category", "", "show only tools in this functional category")
	toolsListCmd.Flags().BoolVar(&toolsListMutating, "mutating", false, "show only tools that write to the working tree / graph")
	toolsListCmd.Flags().StringVar(&toolsListPreset, "preset", "", "show only tools published by this builtin preset (core|edit|nav|readonly)")
	toolsListCmd.Flags().StringVar(&toolsListFormat, "format", "text", "output format: text or json")

	toolsSearchCmd.Flags().IntVar(&toolsSearchLimit, "limit", 10, "max number of tools to return")
	toolsSearchCmd.Flags().StringVar(&toolsSearchFormat, "format", "text", "output format: text or json")

	toolsCmd.AddCommand(toolsListCmd)
	toolsCmd.AddCommand(toolsSearchCmd)
	toolsCmd.AddCommand(toolsDescribeCmd)
	rootCmd.AddCommand(toolsCmd)
}

// toolDescriptorCLI mirrors mcp.ToolDescriptor's wire shape (the per-tool
// catalog tool_profile returns in its `descriptors` array).
type toolDescriptorCLI struct {
	Name        string   `json:"name"`
	Category    string   `json:"category"`
	Mutating    bool     `json:"mutating"`
	Presets     []string `json:"presets"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
}

func runToolsList(cmd *cobra.Command, _ []string) error {
	raw, err := toolsDaemonTool(toolsIndex, "tool_profile", map[string]any{})
	if err != nil {
		return err
	}
	var profile struct {
		Descriptors []toolDescriptorCLI `json:"descriptors"`
	}
	if err := json.Unmarshal(raw, &profile); err != nil {
		// Unknown shape — fall back to pretty JSON rather than fail.
		return emitDaemonJSON(cmd, raw)
	}

	// Apply the client-side filters.
	filtered := make([]toolDescriptorCLI, 0, len(profile.Descriptors))
	for _, d := range profile.Descriptors {
		if toolsListCategory != "" && !strings.EqualFold(d.Category, toolsListCategory) {
			continue
		}
		if toolsListMutating && !d.Mutating {
			continue
		}
		if toolsListPreset != "" && !descriptorHasPreset(d, toolsListPreset) {
			continue
		}
		filtered = append(filtered, d)
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Name < filtered[j].Name })

	if toolsListFormat == "json" {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(filtered)
	}
	return printToolsTable(cmd, filtered)
}

// descriptorHasPreset reports whether the descriptor lists the given preset
// (case-insensitive).
func descriptorHasPreset(d toolDescriptorCLI, preset string) bool {
	for _, p := range d.Presets {
		if strings.EqualFold(p, preset) {
			return true
		}
	}
	return false
}

// printToolsTable renders the descriptor list as a NAME · CATEGORY · R/W ·
// PRESETS · SUMMARY table.
func printToolsTable(cmd *cobra.Command, descs []toolDescriptorCLI) error {
	out := cmd.OutOrStdout()
	if len(descs) == 0 {
		fmt.Fprintln(out, "no tools match the filter")
		return nil
	}
	// Width the NAME column to the widest name (clamped) so the table lines up.
	nameW := 4
	for _, d := range descs {
		if len(d.Name) > nameW {
			nameW = len(d.Name)
		}
	}
	if nameW > 32 {
		nameW = 32
	}
	fmt.Fprintf(out, "%-*s  %-10s  %-3s  %-18s  %s\n", nameW, "NAME", "CATEGORY", "R/W", "PRESETS", "SUMMARY")
	for _, d := range descs {
		rw := "R"
		if d.Mutating {
			rw = "R/W"
		}
		fmt.Fprintf(out, "%-*s  %-10s  %-3s  %-18s  %s\n",
			nameW, d.Name, d.Category, rw, strings.Join(d.Presets, ","), d.Summary)
	}
	return nil
}

func runToolsSearch(cmd *cobra.Command, args []string) error {
	query := strings.Join(args, " ")
	raw, err := toolsDaemonTool(toolsIndex, "tools_search", map[string]any{
		"query":       query,
		"max_results": toolsSearchLimit,
		// Inspection only — don't mutate the live tool set as a side effect of
		// a CLI search.
		"promote": false,
	})
	if err != nil {
		return err
	}
	entries := parseToolsSearchEntries(raw)
	out := cmd.OutOrStdout()
	if toolsSearchFormat == "json" {
		type searchResultJSON struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		results := make([]searchResultJSON, 0, len(entries))
		for _, e := range entries {
			results = append(results, searchResultJSON{Name: e.Name, Description: strings.TrimSpace(e.Description)})
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(results)
	}
	if len(entries) == 0 {
		fmt.Fprintf(out, "no tools match %q\n", query)
		return nil
	}
	for _, e := range entries {
		fmt.Fprintf(out, "%-32s %s\n", e.Name, firstLineCLI(e.Description))
	}
	return nil
}

func runToolsDescribe(cmd *cobra.Command, args []string) error {
	name := args[0]
	raw, err := toolsDaemonTool(toolsIndex, "tools_search", map[string]any{
		"query":   "select:" + name,
		"promote": false,
	})
	if err != nil {
		return err
	}
	entries := parseToolsSearchEntries(raw)
	out := cmd.OutOrStdout()
	if len(entries) == 0 {
		fmt.Fprintf(out, "no tool named %q is registered (it may already be live; try `gortex tools list`)\n", name)
		return nil
	}
	for _, e := range entries {
		fmt.Fprintf(out, "%s\n", e.Name)
		if e.Description != "" {
			fmt.Fprintf(out, "\n%s\n", strings.TrimSpace(e.Description))
		}
		if len(e.InputSchema) > 0 && strings.TrimSpace(string(e.InputSchema)) != "" {
			fmt.Fprintln(out, "\nparameters:")
			var v any
			if err := json.Unmarshal(e.InputSchema, &v); err == nil {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				_ = enc.Encode(v)
			} else {
				fmt.Fprintln(out, string(e.InputSchema))
			}
		}
	}
	return nil
}

// toolsSearchEntryCLI is the per-tool record parsed out of a tools_search
// response. The tool emits both a structured payload (with these fields) and a
// human <functions> text block; we prefer the structured form and fall back to
// the <functions> block.
type toolsSearchEntryCLI struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// parseToolsSearchEntries pulls the matched-tool entries out of a tools_search
// response. The daemon may hand back either the structured JSON payload
// (toolsSearchPayload with a `tools` array) or, robustly, the <functions> text
// block; this handles both.
func parseToolsSearchEntries(raw json.RawMessage) []toolsSearchEntryCLI {
	// Preferred: the structured payload shape.
	var structured struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &structured); err == nil && len(structured.Tools) > 0 {
		out := make([]toolsSearchEntryCLI, 0, len(structured.Tools))
		for _, t := range structured.Tools {
			out = append(out, toolsSearchEntryCLI{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: t.InputSchema,
			})
		}
		return out
	}

	// Fallback: the daemon may have handed us the text content directly (a
	// <functions>{...}</function> block). Some transports deliver the text as a
	// JSON string; unwrap that first.
	text := string(raw)
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil && asString != "" {
		text = asString
	}
	return parseFunctionsBlock(text)
}

// parseFunctionsBlock extracts entries from a <functions>...<function>{json}
// </function>...</functions> text block. Each <function> line carries a JSON
// object with description / name / parameters (the renderToolsSearchResult
// shape).
func parseFunctionsBlock(text string) []toolsSearchEntryCLI {
	var out []toolsSearchEntryCLI
	const open = "<function>"
	const close = "</function>"
	rest := text
	for {
		i := strings.Index(rest, open)
		if i < 0 {
			break
		}
		rest = rest[i+len(open):]
		j := strings.Index(rest, close)
		if j < 0 {
			break
		}
		body := strings.TrimSpace(rest[:j])
		rest = rest[j+len(close):]
		var entry struct {
			Description string          `json:"description"`
			Name        string          `json:"name"`
			Parameters  json.RawMessage `json:"parameters"`
		}
		if err := json.Unmarshal([]byte(body), &entry); err != nil {
			continue
		}
		out = append(out, toolsSearchEntryCLI{
			Name:        entry.Name,
			Description: entry.Description,
			InputSchema: entry.Parameters,
		})
	}
	return out
}

// firstLineCLI returns the first non-empty line of s, trimmed — the one-line
// summary shown in the search listing.
func firstLineCLI(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}
