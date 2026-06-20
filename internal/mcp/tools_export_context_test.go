package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExportContext_DoesNotBreakUnderGCXSession is the regression
// guard for the parse-failure path that this commit fixes.
//
// export_context delegates to handleSmartContext for the raw data
// and then json.Unmarshal's it. Before the fix, the inner call
// inherited the session's client-aware format default — for known
// clients like claude-code that resolves to GCX1, which made the
// unmarshal fail with "invalid character 'G'". The fix forces
// `format: "json"` on the inner call so the unmarshal sees JSON
// regardless of session or outer format.
func TestExportContext_DoesNotBreakUnderGCXSession(t *testing.T) {
	srv, _ := setupTestServer(t)
	// Bind a session with a client name that maps to GCX, so the
	// session-resolved format would otherwise leak into the inner
	// smart_context call.
	srv.NoteSessionClient("session_export", "claude-code", "1.0.42")
	ctx := WithSessionID(context.Background(), "session_export")

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"task": "validate the export_context wiring",
	}

	res, err := srv.handleExportContext(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError,
		"export_context must not return an error result under a GCX-resolving session; got: %+v",
		res.Content)

	require.NotEmpty(t, res.Content)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	assert.Contains(t, tc.Text, "# Context Briefing",
		"markdown header should be present in the default render")
}

// TestExportContext_OuterFormatJSONStillWorks verifies the JSON
// output path still produces structured data (format=json on the
// outer call), independent of the inner GCX-forcing patch.
func TestExportContext_OuterFormatJSONStillWorks(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.NoteSessionClient("session_export_json", "claude-code", "1.0.42")
	ctx := WithSessionID(context.Background(), "session_export_json")

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"task":   "validate the export_context json path",
		"format": "json",
	}

	res, err := srv.handleExportContext(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError)

	tc := res.Content[0].(mcp.TextContent)
	// JSON output: should start with `{` not `# Context Briefing`.
	assert.True(t, strings.HasPrefix(strings.TrimSpace(tc.Text), "{"),
		"format=json must produce JSON, got: %q", tc.Text)
}

// TestExportContext_OuterRequestArgsUnmutated guards against the
// inner-format override leaking back into the caller's arg map.
// The handler clones args before mutating; a shared-map regression
// would corrupt the outer request and confuse downstream telemetry.
func TestExportContext_OuterRequestArgsUnmutated(t *testing.T) {
	srv, _ := setupTestServer(t)
	ctx := context.Background()

	outerArgs := map[string]any{
		"task":   "argument-isolation check",
		"format": "markdown",
	}
	req := mcp.CallToolRequest{}
	req.Params.Arguments = outerArgs

	_, err := srv.handleExportContext(ctx, req)
	require.NoError(t, err)

	// The caller-provided map should still say "markdown" — the
	// handler's clone-then-override must not have written through.
	assert.Equal(t, "markdown", outerArgs["format"],
		"outer args mutated by the inner-format override")
}

// TestRenderContextMarkdown_FenceMatchesSymbolLanguage is the regression
// guard for the hardcoded-fence bug: embedded source snippets were always
// wrapped in a ```go fence regardless of the symbol's real language, so a
// TypeScript snippet was mislabelled as Go. The fence must track the
// symbol's own indexed language.
func TestRenderContextMarkdown_FenceMatchesSymbolLanguage(t *testing.T) {
	data := map[string]any{
		"task": "token invalidation",
		"relevant_symbols": []any{
			map[string]any{
				"id":         "src/auth.ts::invalidateToken",
				"kind":       "function",
				"name":       "invalidateToken",
				"file_path":  "src/auth.ts",
				"language":   "typescript",
				"start_line": float64(10),
				"source":     "function invalidateToken(t: string) {}",
			},
		},
	}

	md := renderContextMarkdown(data, 2000)

	assert.Contains(t, md, "```typescript\n",
		"a TypeScript symbol must be fenced as typescript")
	assert.NotContains(t, md, "```go",
		"a TypeScript snippet must never be fenced as go")
}

// TestRenderContextMarkdown_FenceFallsBackToExtension covers the path where
// the entry carries no language field (older/cached data or a federated
// merge): the fence is derived from the file extension rather than defaulting
// to go.
func TestRenderContextMarkdown_FenceFallsBackToExtension(t *testing.T) {
	data := map[string]any{
		"task": "extension fallback",
		"relevant_symbols": []any{
			map[string]any{
				"id":   "lib/util.py::helper",
				"kind": "function",
				"name": "helper",
				// language deliberately omitted to exercise the fallback.
				"file_path":  "lib/util.py",
				"start_line": float64(1),
				"source":     "def helper():\n    pass",
			},
		},
	}

	md := renderContextMarkdown(data, 2000)

	assert.Contains(t, md, "```python\n",
		"with no language field the fence must be derived from the .py extension")
	assert.NotContains(t, md, "```go",
		"the extension fallback must not produce a go fence")
}

// TestRenderContextMarkdown_GoSymbolStillFencedAsGo guards the common case:
// the fix must not regress Go snippets, which should still be fenced as go.
func TestRenderContextMarkdown_GoSymbolStillFencedAsGo(t *testing.T) {
	data := map[string]any{
		"task": "go path",
		"relevant_symbols": []any{
			map[string]any{
				"id":         "main.go::run",
				"kind":       "function",
				"name":       "run",
				"file_path":  "main.go",
				"language":   "go",
				"start_line": float64(1),
				"source":     "func run() {}",
			},
		},
	}

	md := renderContextMarkdown(data, 2000)

	assert.Contains(t, md, "```go\n", "a Go symbol must still be fenced as go")
}

// TestRenderContextMarkdown_GradedManifestRendersSource is the regression
// guard for the graded-fidelity path: smart_context with fidelity:graded emits
// a context_manifest holding the source and builds relevant_symbols without
// source. The renderer previously only looked at relevant_symbols, so a graded
// briefing showed symbol headers but no code at all. The manifest must be
// rendered, fenced by each entry's own language.
func TestRenderContextMarkdown_GradedManifestRendersSource(t *testing.T) {
	data := map[string]any{
		"task": "token invalidation",
		// In graded mode relevant_symbols carries no source — only the
		// manifest does. The renderer must prefer the manifest.
		"relevant_symbols": []any{
			map[string]any{
				"id":         "src/auth.ts::invalidateToken",
				"kind":       "function",
				"name":       "invalidateToken",
				"file_path":  "src/auth.ts",
				"language":   "typescript",
				"start_line": float64(10),
			},
		},
		"context_manifest": map[string]any{
			"omitted": float64(2),
			"entries": []any{
				map[string]any{
					"id":         "src/auth.ts::invalidateToken",
					"kind":       "function",
					"name":       "invalidateToken",
					"file_path":  "src/auth.ts",
					"language":   "typescript",
					"start_line": float64(10),
					"tier":       "focus",
					"source":     "function invalidateToken(t: string) {}",
				},
			},
		},
	}

	md := renderContextMarkdown(data, 2000)

	assert.Contains(t, md, "function invalidateToken(t: string) {}",
		"graded manifest source must be rendered")
	assert.Contains(t, md, "```typescript\n",
		"manifest source must be fenced by the entry's language")
	assert.NotContains(t, md, "```go",
		"a TypeScript manifest snippet must never be fenced as go")
	assert.Contains(t, md, "**Tier:** focus",
		"the manifest tier should be surfaced")
	assert.Contains(t, md, "2 more symbol(s) omitted",
		"the manifest omitted count should be surfaced")
}

// TestRenderContextMarkdown_GradedManifestMarksCompressed verifies a
// compressed (body-elided) manifest entry is labelled as such so the reader
// knows the snippet is a skeleton rather than the full body.
func TestRenderContextMarkdown_GradedManifestMarksCompressed(t *testing.T) {
	data := map[string]any{
		"task": "compressed entry",
		"context_manifest": map[string]any{
			"entries": []any{
				map[string]any{
					"id":         "pkg/big.go::Huge",
					"kind":       "type",
					"name":       "Huge",
					"file_path":  "pkg/big.go",
					"language":   "go",
					"start_line": float64(1),
					"tier":       "focus",
					"compressed": true,
					"source":     "type Huge struct { /* … */ }",
				},
			},
		},
	}

	md := renderContextMarkdown(data, 2000)

	assert.Contains(t, md, "source compressed",
		"a compressed manifest entry must be labelled compressed")
	assert.Contains(t, md, "```go\n", "the compressed snippet must still be fenced")
}
