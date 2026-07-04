package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/semantic"
)

// shrinkReadinessTimers speeds the readiness poll loop for tests and restores
// the production values on cleanup.
func shrinkReadinessTimers(t *testing.T) {
	t.Helper()
	pi, lt := csharpReadinessProbeInterval, csharpSolutionLoadTimeout
	csharpReadinessProbeInterval = 5 * time.Millisecond
	csharpSolutionLoadTimeout = 2 * time.Second
	t.Cleanup(func() {
		csharpReadinessProbeInterval = pi
		csharpSolutionLoadTimeout = lt
	})
}

// TestPollWorkspaceReady_ReadyAfterLoad models a Roslyn / MSBuild server whose
// workspace/symbol answers empty while the solution loads and returns a match
// once it is live. pollWorkspaceReady must block across the empty phase and
// return nil only once the probe is non-empty, so the enrichment sweep starts
// against a server that can actually resolve.
func TestPollWorkspaceReady_ReadyAfterLoad(t *testing.T) {
	shrinkReadinessTimers(t)

	var calls int64
	srv := newFakeLSPServer()
	srv.handle("workspace/symbol", func(json.RawMessage) (any, *jsonRPCError) {
		if atomic.AddInt64(&calls, 1) < 3 {
			return []any{}, nil // still loading — empty
		}
		return []map[string]any{{"name": "Foo", "kind": 5}}, nil
	})
	p, cleanup := providerWithFakeServer(t, srv, []string{"csharp"})
	defer cleanup()

	require.NoError(t, p.pollWorkspaceReady(context.Background(), "Foo"))
	assert.GreaterOrEqual(t, atomic.LoadInt64(&calls), int64(3),
		"probe must poll across the empty solution-load phase before reporting ready")
}

// TestPollWorkspaceReady_NeverReady: a server that never loads (always empty)
// must surface semantic.ErrWorkspaceNotReady so the Manager records an honest
// state and skips the futile sweep.
func TestPollWorkspaceReady_NeverReady(t *testing.T) {
	shrinkReadinessTimers(t)

	srv := newFakeLSPServer()
	srv.handle("workspace/symbol", func(json.RawMessage) (any, *jsonRPCError) {
		return []any{}, nil
	})
	p, cleanup := providerWithFakeServer(t, srv, []string{"csharp"})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := p.pollWorkspaceReady(ctx, "Foo")
	require.Error(t, err)
	assert.True(t, errors.Is(err, semantic.ErrWorkspaceNotReady),
		"a workspace that never loads must report ErrWorkspaceNotReady, got %v", err)
}

// TestCSharpProbeQuery derives a workspace/symbol query from the first C#
// declaration under the repo root, and reports none for a tree with no C#.
func TestCSharpProbeQuery(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Widget.cs"),
		[]byte("namespace Demo {\n  public interface IWidget { void Do(); }\n}\n"), 0o644))
	q, ok := csharpProbeQuery(dir)
	require.True(t, ok)
	assert.Equal(t, "IWidget", q)

	empty := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(empty, "README.md"), []byte("hi"), 0o644))
	_, ok = csharpProbeQuery(empty)
	assert.False(t, ok)
}

// TestCSharpDeclName covers the identifier extraction the probe query relies on.
func TestCSharpDeclName(t *testing.T) {
	cases := []struct{ src, want string }{
		{"public class Foo {}", "Foo"},
		{"internal interface IBar { }", "IBar"},
		{"public record class Money(decimal V);", "Money"},
		{"public record Point(int X, int Y);", "Point"},
		{"namespace A.B.Baz;", "Baz"},
		{"// just a comment\n", ""},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, csharpDeclName([]byte(c.src)), "src=%q", c.src)
	}
}
