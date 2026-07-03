package semantic

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// slowSolutionProvider models a Roslyn/MSBuild-class server: its queries only
// succeed once its solution has loaded, which the manager triggers by calling
// WaitReady before the enrichment deadline. It records whether readiness ran
// and only lands edges when it did.
type slowSolutionProvider struct {
	name      string
	languages []string
	waited    bool
	ready     bool
}

func (s *slowSolutionProvider) Name() string        { return s.name }
func (s *slowSolutionProvider) Languages() []string { return s.languages }
func (s *slowSolutionProvider) Available() bool     { return true }
func (s *slowSolutionProvider) Close() error        { return nil }

func (s *slowSolutionProvider) Enrich(g graph.Store, repoRoot string) (*EnrichResult, error) {
	return nil, nil
}

func (s *slowSolutionProvider) EnrichFile(g graph.Store, repoRoot, filePath string) (*EnrichResult, error) {
	return nil, nil
}

func (s *slowSolutionProvider) WaitReady(ctx context.Context, repoRoot string) error {
	s.waited = true
	s.ready = true
	return nil
}

func (s *slowSolutionProvider) EnrichRepoContext(ctx context.Context, g graph.Store, repoPrefix, repoRoot string) (*EnrichResult, error) {
	res := &EnrichResult{Provider: s.name, Language: s.languages[0]}
	// A solution-load server serves empty results until its load finishes; the
	// readiness gate must run first for the pass to land any edges.
	if s.ready {
		res.EdgesAdded = 3
	}
	return res, nil
}

// TestRunEnrichOne_ReadinessGate: a ReadinessProber provider is brought to
// readiness BEFORE the enrichment deadline starts, so its pass lands edges
// instead of spending the whole budget on the cold solution load.
func TestRunEnrichOne_ReadinessGate(t *testing.T) {
	mgr := NewManager(Config{Enabled: true}, zap.NewNop())
	p := &slowSolutionProvider{name: "csharp-like", languages: []string{"csharp"}}

	results := mgr.runEnrichOne(graph.New(), "repo", "/tmp/repo", "csharp", p, 10, RepoEnrichState{}, nil, map[string]bool{})

	require.True(t, p.waited, "readiness prober must be invoked before the enrichment pass")
	require.Len(t, results, 1)
	assert.Equal(t, 3, results[0].EdgesAdded,
		"the pass lands edges because the deadline started only after readiness")
}

// TestRunEnrichOne_FastProviderSkipsReadiness: a provider that does not
// implement ReadinessProber (a gopls-shaped server, ready after initialize)
// runs without incurring the readiness wait.
func TestRunEnrichOne_FastProviderSkipsReadiness(t *testing.T) {
	mgr := NewManager(Config{Enabled: true}, zap.NewNop())
	// mockProvider (from manager_test.go) does not implement ReadinessProber.
	p := &mockProvider{name: "gopls-like", languages: []string{"go"}, available: true}

	results := mgr.runEnrichOne(graph.New(), "repo", "/tmp/repo", "go", p, 10, RepoEnrichState{}, nil, map[string]bool{})

	require.Len(t, results, 1, "a fast provider still runs; the readiness gate is simply skipped")
	assert.Equal(t, "gopls-like", results[0].Provider)
}
