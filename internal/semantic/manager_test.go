package semantic

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// mockProvider is a test provider that records calls.
type mockProvider struct {
	name       string
	languages  []string
	available  bool
	enrichFunc func(g *graph.Graph, root string) (*EnrichResult, error)
	closed     bool
}

func (m *mockProvider) Name() string       { return m.name }
func (m *mockProvider) Languages() []string { return m.languages }
func (m *mockProvider) Available() bool     { return m.available }
func (m *mockProvider) Close() error        { m.closed = true; return nil }

func (m *mockProvider) Enrich(g *graph.Graph, repoRoot string) (*EnrichResult, error) {
	if m.enrichFunc != nil {
		return m.enrichFunc(g, repoRoot)
	}
	return &EnrichResult{
		Provider:       m.name,
		Language:       m.languages[0],
		EdgesConfirmed: 5,
		EdgesAdded:     2,
		CoveragePercent: 95.0,
	}, nil
}

func (m *mockProvider) EnrichFile(g *graph.Graph, repoRoot, filePath string) (*EnrichResult, error) {
	return nil, nil
}

func TestManager_EnrichAll(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "test-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}

	mgr := NewManager(cfg, logger)
	mgr.RegisterProvider(&mockProvider{
		name:      "test-go",
		languages: []string{"go"},
		available: true,
	})

	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "main.go", Language: "go"})

	roots := map[string]string{"default": "/tmp/test"}
	results, err := mgr.EnrichAll(g, roots)
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, "test-go", results[0].Provider)
	assert.Equal(t, 5, results[0].EdgesConfirmed)
}

func TestManager_PrioritySelection(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "high-priority", Languages: []string{"go"}, Priority: 1, Enabled: true},
			{Name: "low-priority", Languages: []string{"go"}, Priority: 2, Enabled: true},
		},
	}

	mgr := NewManager(cfg, logger)

	highCalled := false
	lowCalled := false

	mgr.RegisterProvider(&mockProvider{
		name:      "high-priority",
		languages: []string{"go"},
		available: true,
		enrichFunc: func(g *graph.Graph, root string) (*EnrichResult, error) {
			highCalled = true
			return &EnrichResult{Provider: "high-priority", Language: "go"}, nil
		},
	})
	mgr.RegisterProvider(&mockProvider{
		name:      "low-priority",
		languages: []string{"go"},
		available: true,
		enrichFunc: func(g *graph.Graph, root string) (*EnrichResult, error) {
			lowCalled = true
			return &EnrichResult{Provider: "low-priority", Language: "go"}, nil
		},
	})

	g := graph.New()
	roots := map[string]string{"default": "/tmp/test"}
	_, err := mgr.EnrichAll(g, roots)
	require.NoError(t, err)

	assert.True(t, highCalled, "high-priority provider should run")
	assert.False(t, lowCalled, "low-priority provider should not run")
}

func TestManager_UnavailableProvider(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "unavailable", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}

	mgr := NewManager(cfg, logger)
	mgr.RegisterProvider(&mockProvider{
		name:      "unavailable",
		languages: []string{"go"},
		available: false,
	})

	g := graph.New()
	roots := map[string]string{"default": "/tmp/test"}
	results, err := mgr.EnrichAll(g, roots)
	require.NoError(t, err)
	assert.Len(t, results, 0)
}

func TestManager_Disabled(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{Enabled: false}

	mgr := NewManager(cfg, logger)
	mgr.RegisterProvider(&mockProvider{
		name:      "test",
		languages: []string{"go"},
		available: true,
	})

	g := graph.New()
	roots := map[string]string{"default": "/tmp/test"}
	results, err := mgr.EnrichAll(g, roots)
	require.NoError(t, err)
	assert.Nil(t, results)
}

func TestManager_Close(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{Enabled: true}

	mgr := NewManager(cfg, logger)
	p := &mockProvider{name: "test", languages: []string{"go"}, available: true}
	mgr.RegisterProvider(p)

	err := mgr.Close()
	require.NoError(t, err)
	assert.True(t, p.closed)
}

func TestManager_Stats(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "test-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}

	mgr := NewManager(cfg, logger)
	mgr.RegisterProvider(&mockProvider{
		name:      "test-go",
		languages: []string{"go"},
		available: true,
	})

	stats := mgr.Stats()
	require.Len(t, stats, 1)
	assert.Equal(t, "test-go", stats[0].Name)
	assert.Equal(t, "go", stats[0].Language)
	assert.Equal(t, "ready", stats[0].Status)
}
