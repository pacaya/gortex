package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func newNotebookTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	s := &Server{
		graph:      graph.New(),
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
	s.InitNotebook(dir)
	return s, dir
}

func callNotebookHandler(t *testing.T, h func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error), args map[string]any) map[string]any {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := h(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	if res.IsError {
		return map[string]any{"is_error": true}
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &m))
	return m
}

func TestNotebook_SaveCreatesFile(t *testing.T) {
	s, dir := newNotebookTestServer(t)
	out := callNotebookHandler(t, s.handleNotebookSave, map[string]any{
		"title": "auth invariant",
		"body":  "Bar must hold the mutex.",
		"tags":  "invariant,auth",
	})

	id := out["id"].(string)
	require.NotEmpty(t, id)
	path := filepath.Join(dir, ".gortex", "notebook", id+".md")
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(body), "title: auth invariant")
	assert.Contains(t, string(body), "Bar must hold the mutex")
	assert.Contains(t, string(body), "tags: [invariant, auth]")
}

func TestNotebook_UpdatePreservesCreated(t *testing.T) {
	s, _ := newNotebookTestServer(t)
	created := callNotebookHandler(t, s.handleNotebookSave, map[string]any{
		"title": "first", "body": "body1",
	})
	id := created["id"].(string)
	originalCreated := created["created"].(string)

	// Sleep tiny bit to make sure Updated bumps.
	time.Sleep(10 * time.Millisecond)
	updated := callNotebookHandler(t, s.handleNotebookSave, map[string]any{
		"id":   id,
		"body": "body2",
	})
	assert.Equal(t, originalCreated, updated["created"], "Created preserved across update")
	assert.NotEqual(t, originalCreated, updated["updated"], "Updated bumped on save")
}

func TestNotebook_RejectsEmptySave(t *testing.T) {
	s, _ := newNotebookTestServer(t)
	out := callNotebookHandler(t, s.handleNotebookSave, map[string]any{})
	assert.True(t, out["is_error"] == true)
}

func TestNotebook_FindByBody(t *testing.T) {
	s, _ := newNotebookTestServer(t)
	_ = callNotebookHandler(t, s.handleNotebookSave, map[string]any{
		"title": "alpha", "body": "tokio runtime quirks",
	})
	_ = callNotebookHandler(t, s.handleNotebookSave, map[string]any{
		"title": "beta", "body": "axum router setup",
	})

	out := callNotebookHandler(t, s.handleNotebookFind, map[string]any{"query": "tokio"})
	entries, _ := out["entries"].([]any)
	require.Len(t, entries, 1)
	assert.Equal(t, "alpha", entries[0].(map[string]any)["title"])
}

func TestNotebook_FindByTag(t *testing.T) {
	s, _ := newNotebookTestServer(t)
	_ = callNotebookHandler(t, s.handleNotebookSave, map[string]any{
		"title": "a", "body": "x", "tags": "decision,api",
	})
	_ = callNotebookHandler(t, s.handleNotebookSave, map[string]any{
		"title": "b", "body": "y", "tags": "gotcha",
	})

	out := callNotebookHandler(t, s.handleNotebookFind, map[string]any{"query": "decision"})
	entries, _ := out["entries"].([]any)
	require.Len(t, entries, 1)
}

func TestNotebook_FindCaseInsensitive(t *testing.T) {
	s, _ := newNotebookTestServer(t)
	_ = callNotebookHandler(t, s.handleNotebookSave, map[string]any{
		"title": "Tokio Stuff", "body": "x",
	})
	out := callNotebookHandler(t, s.handleNotebookFind, map[string]any{"query": "tokio"})
	entries, _ := out["entries"].([]any)
	assert.Len(t, entries, 1)
}

func TestNotebook_ListReturnsAllSortedDesc(t *testing.T) {
	s, _ := newNotebookTestServer(t)
	for i := range 3 {
		_ = callNotebookHandler(t, s.handleNotebookSave, map[string]any{
			"title": "t" + string(rune('A'+i)), "body": "x",
		})
		time.Sleep(5 * time.Millisecond)
	}

	out := callNotebookHandler(t, s.handleNotebookList, map[string]any{})
	entries, _ := out["entries"].([]any)
	require.Len(t, entries, 3)
	// Most recently updated first.
	assert.Equal(t, "tC", entries[0].(map[string]any)["title"])
}

func TestNotebook_ShowReturnsBody(t *testing.T) {
	s, _ := newNotebookTestServer(t)
	created := callNotebookHandler(t, s.handleNotebookSave, map[string]any{
		"title": "x", "body": "the full markdown body here",
	})
	id := created["id"].(string)

	out := callNotebookHandler(t, s.handleNotebookShow, map[string]any{"id": id})
	assert.Equal(t, "the full markdown body here\n", out["body"], "show returns full body including trailing newline")
}

func TestNotebook_ShowUnknownIDErrors(t *testing.T) {
	s, _ := newNotebookTestServer(t)
	out := callNotebookHandler(t, s.handleNotebookShow, map[string]any{"id": "doesnotexist"})
	assert.True(t, out["is_error"] == true)
}

func TestNotebook_UsedBumpsCount(t *testing.T) {
	s, _ := newNotebookTestServer(t)
	created := callNotebookHandler(t, s.handleNotebookSave, map[string]any{
		"title": "x", "body": "y",
	})
	id := created["id"].(string)

	for range 3 {
		_ = callNotebookHandler(t, s.handleNotebookUsed, map[string]any{"id": id})
	}
	out := callNotebookHandler(t, s.handleNotebookShow, map[string]any{"id": id})
	assert.EqualValues(t, 3, out["used_count"].(float64))
	_, hasLastUsed := out["last_used"]
	assert.True(t, hasLastUsed)
}

func TestNotebook_RejectsMissingNotebook(t *testing.T) {
	s := &Server{
		graph:      graph.New(),
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
	// No InitNotebook — s.notebook is nil.
	out := callNotebookHandler(t, s.handleNotebookSave, map[string]any{"body": "x"})
	assert.True(t, out["is_error"] == true)
}

func TestNotebook_FrontmatterRoundTrip(t *testing.T) {
	entry := notebookEntry{
		ID:      "abc",
		Title:   "a title: with colon",
		Tags:    []string{"x", "y"},
		Created: time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC),
		Updated: time.Date(2026, 5, 19, 11, 0, 0, 0, time.UTC),
		Body:    "line one\nline two\n",
	}
	md := notebookMarshal(entry)
	parsed, err := notebookUnmarshal(md)
	require.NoError(t, err)
	assert.Equal(t, entry.Title, parsed.Title)
	assert.Equal(t, entry.Tags, parsed.Tags)
	assert.Equal(t, entry.Created.Unix(), parsed.Created.Unix())
	assert.Equal(t, entry.Body, parsed.Body)
}

func TestNotebook_NoDirManagerStillSafe(t *testing.T) {
	nm := newNotebookManager("")
	// Save returns an entry but doesn't error.
	e, err := nm.Save(notebookEntry{Title: "x"})
	require.NoError(t, err)
	assert.NotEmpty(t, e.ID)
	// Get / List / Find return empty/false on a no-disk manager.
	_, ok := nm.Get(e.ID)
	assert.False(t, ok)
	assert.Empty(t, nm.List())
}

func TestNotebook_PrunesByTTL(t *testing.T) {
	dir := t.TempDir()
	nm := newNotebookManager(dir)
	nm.ttl = 1 * time.Millisecond
	// Write an entry with Updated far in the past so the prune
	// purges it on the next save.
	stale := notebookEntry{ID: "stale", Title: "stale", Updated: time.Now().Add(-time.Hour)}
	_ = os.MkdirAll(filepath.Join(dir, ".gortex", "notebook"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, ".gortex", "notebook", "stale.md"), []byte(notebookMarshal(stale)), 0o644)

	// Trigger a save which fires the prune.
	_, _ = nm.Save(notebookEntry{Title: "fresh", Body: "x"})

	// stale.md should be gone.
	_, err := os.Stat(filepath.Join(dir, ".gortex", "notebook", "stale.md"))
	assert.True(t, os.IsNotExist(err), "TTL-expired entry pruned")
}

func TestNotebook_DeleteIdempotent(t *testing.T) {
	dir := t.TempDir()
	nm := newNotebookManager(dir)
	_, _ = nm.Save(notebookEntry{ID: "x", Title: "x"})
	require.NoError(t, nm.Delete("x"))
	require.NoError(t, nm.Delete("x"), "second delete on missing entry is not an error")
}

func TestParseYAMLInlineList(t *testing.T) {
	assert.Equal(t, []string{"a", "b"}, parseYAMLInlineList("[a, b]"))
	assert.Equal(t, []string{"a", "b"}, parseYAMLInlineList(" a , b "))
	assert.Nil(t, parseYAMLInlineList("[]"))
	assert.Nil(t, parseYAMLInlineList(""))
}

func TestYAMLEscapeRoundTrip(t *testing.T) {
	cases := []string{
		`plain`,
		`with: colon`,
		`with "quote"`,
		``,
	}
	for _, c := range cases {
		escaped := yamlEscapeOneLine(c)
		back := yamlUnescapeOneLine(escaped)
		assert.Equal(t, c, back, "roundtrip %q", c)
		_ = strings.HasPrefix(escaped, `"`) // suppress unused
	}
}
