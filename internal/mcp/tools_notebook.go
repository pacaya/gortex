package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// notebookTimeFormat is RFC3339Nano so consecutive saves within
// the same second remain distinguishable on wire output.
const notebookTimeFormat = time.RFC3339Nano

// registerNotebookTools wires the repository-local persistent
// notebook — the third memory axis below session notes
// (per-session) and cross-session memories (workspace-wide, in the
// cache dir). Notebook entries live at .gortex/notebook/<id>.md so
// they can be committed to git and reviewed in PRs.
//
// Surface:
//   notebook_save  — create or update an entry
//   notebook_find  — case-insensitive substring search over
//                    title / body / tags
//   notebook_list  — every entry sorted Updated DESC
//   notebook_show  — return a single entry by ID
//   notebook_used  — bump UsedCount + LastUsed (resets TTL)
//
// 30-day TTL pruner runs at every save: entries unused (or never
// touched after Updated) for longer than that age out.
func (s *Server) registerNotebookTools() {
	s.addTool(
		mcp.NewTool("notebook_save",
			mcp.WithDescription("Persist a repository-local notebook entry to .gortex/notebook/<id>.md. Entries are checked into git, travel with the repo, and survive daemon restarts. Use for agent-authored notes that should be visible in PR review (vs cross-session memories which live in the cache dir). Pass `id` to update an existing entry; omit to create with a fresh ID."),
			mcp.WithString("body", mcp.Description("Markdown body. Required for create; optional for update.")),
			mcp.WithString("title", mcp.Description("Short caption for the entry list.")),
			mcp.WithString("tags", mcp.Description("Comma-separated tags.")),
			mcp.WithString("id", mcp.Description("Existing entry ID — passing it switches the call from create to update.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleNotebookSave,
	)

	s.addTool(
		mcp.NewTool("notebook_find",
			mcp.WithDescription("Case-insensitive substring search over notebook entries' title / body / tags. Empty query returns every entry sorted Updated DESC. Pairs with notebook_show for full-body retrieval after a hit."),
			mcp.WithString("query", mcp.Description("Substring to match (case-insensitive). Empty = list all.")),
			mcp.WithNumber("limit", mcp.Description("Cap on the result set (default: 50).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleNotebookFind,
	)

	s.addTool(
		mcp.NewTool("notebook_list",
			mcp.WithDescription("List every notebook entry sorted Updated DESC. Returns metadata only — call notebook_show for the body. Pairs with notebook_find when you want to filter."),
			mcp.WithNumber("limit", mcp.Description("Cap on the result set (default: 50).")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleNotebookList,
	)

	s.addTool(
		mcp.NewTool("notebook_show",
			mcp.WithDescription("Return a single notebook entry by ID with its full markdown body. Use after notebook_find / notebook_list to drill into a hit."),
			mcp.WithString("id", mcp.Description("Notebook entry ID.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleNotebookShow,
	)

	s.addTool(
		mcp.NewTool("notebook_used",
			mcp.WithDescription("Mark a notebook entry as 'in active use' — increments UsedCount and resets LastUsed, which also resets the TTL pruner's clock so load-bearing entries don't age out. Pin a draft you'll come back to without copying it into memories."),
			mcp.WithString("id", mcp.Description("Notebook entry ID.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx, or toon")),
		),
		s.handleNotebookUsed,
	)
}

// notebookEntryToWire shapes an entry for JSON / GCX / TOON output.
// Timestamps render in RFC3339Nano so consecutive saves within the
// same second remain distinguishable in test assertions and audit
// logs.
func notebookEntryToWire(e notebookEntry, includeBody bool) map[string]any {
	m := map[string]any{
		"id":      e.ID,
		"title":   e.Title,
		"tags":    e.Tags,
		"created": e.Created.UTC().Format(notebookTimeFormat),
		"updated": e.Updated.UTC().Format(notebookTimeFormat),
	}
	if !e.LastUsed.IsZero() {
		m["last_used"] = e.LastUsed.UTC().Format(notebookTimeFormat)
	}
	if e.UsedCount > 0 {
		m["used_count"] = e.UsedCount
	}
	if includeBody {
		m["body"] = e.Body
	}
	return m
}

func (s *Server) handleNotebookSave(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.notebook == nil {
		return mcp.NewToolResultError("notebook not initialised (call InitNotebook)"), nil
	}
	body := req.GetString("body", "")
	title := strings.TrimSpace(req.GetString("title", ""))
	tags := splitCSV(req.GetString("tags", ""))
	id := strings.TrimSpace(req.GetString("id", ""))

	entry := notebookEntry{
		ID:    id,
		Title: title,
		Tags:  tags,
		Body:  body,
	}
	// Preserve creation timestamp + used_count on update.
	if id != "" {
		if existing, ok := s.notebook.Get(id); ok {
			entry.Created = existing.Created
			entry.UsedCount = existing.UsedCount
			entry.LastUsed = existing.LastUsed
			if body == "" {
				entry.Body = existing.Body
			}
			if title == "" {
				entry.Title = existing.Title
			}
			if len(tags) == 0 {
				entry.Tags = existing.Tags
			}
		}
	}
	if entry.Body == "" && entry.Title == "" && len(entry.Tags) == 0 {
		return mcp.NewToolResultError("notebook_save requires at least one of: body, title, tags"), nil
	}
	saved, err := s.notebook.Save(entry)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("save notebook: %v", err)), nil
	}
	return s.respondJSONOrTOON(ctx, req, notebookEntryToWire(saved, true))
}

func (s *Server) handleNotebookFind(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.notebook == nil {
		return mcp.NewToolResultError("notebook not initialised"), nil
	}
	query := req.GetString("query", "")
	limit := max(req.GetInt("limit", 50), 1)
	hits := s.notebook.Find(query)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	wire := make([]map[string]any, 0, len(hits))
	for _, e := range hits {
		wire = append(wire, notebookEntryToWire(e, false))
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"entries": wire,
		"total":   len(wire),
		"query":   query,
	})
}

func (s *Server) handleNotebookList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.notebook == nil {
		return mcp.NewToolResultError("notebook not initialised"), nil
	}
	limit := max(req.GetInt("limit", 50), 1)
	all := s.notebook.List()
	if len(all) > limit {
		all = all[:limit]
	}
	wire := make([]map[string]any, 0, len(all))
	for _, e := range all {
		wire = append(wire, notebookEntryToWire(e, false))
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"entries": wire,
		"total":   len(wire),
	})
}

func (s *Server) handleNotebookShow(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.notebook == nil {
		return mcp.NewToolResultError("notebook not initialised"), nil
	}
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	entry, ok := s.notebook.Get(id)
	if !ok {
		return mcp.NewToolResultError(fmt.Sprintf("notebook entry %q not found", id)), nil
	}
	return s.respondJSONOrTOON(ctx, req, notebookEntryToWire(entry, true))
}

func (s *Server) handleNotebookUsed(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.notebook == nil {
		return mcp.NewToolResultError("notebook not initialised"), nil
	}
	id, err := req.RequireString("id")
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}
	entry, mErr := s.notebook.MarkUsed(id)
	if mErr != nil {
		return mcp.NewToolResultError(fmt.Sprintf("mark used: %v", mErr)), nil
	}
	return s.respondJSONOrTOON(ctx, req, notebookEntryToWire(entry, false))
}
