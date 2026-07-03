package semantic

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// TestManager_EnrichOne_AbandonsOnDeadline verifies the per-repo enrichment
// deadline: a provider that blocks past the deadline is abandoned (the
// enrichment WaitGroup proceeds) rather than pinning it indefinitely — the
// MSBuild/Roslyn-stuck failure mode, generalised to "slow across many
// symbols".
func TestManager_EnrichOne_AbandonsOnDeadline(t *testing.T) {
	t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "50ms")

	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "slow-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}
	mgr := NewManager(cfg, zap.NewNop())

	release := make(chan struct{})
	var enrichReturned atomic.Bool
	mgr.RegisterProvider(&mockProvider{
		name:      "slow-go",
		languages: []string{"go"},
		available: true,
		enrichFunc: func(g graph.Store, root string) (*EnrichResult, error) {
			<-release // block well past the 50ms deadline
			enrichReturned.Store(true)
			return &EnrichResult{Provider: "slow-go", Language: "go"}, nil
		},
	})

	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "main.go", Language: "go"})
	roots := map[string]string{"default": "/tmp/test"}

	resultCh := make(chan []*EnrichResult, 1)
	go func() {
		res, _, _ := mgr.EnrichAll(g, roots, EnrichOptions{})
		resultCh <- res
	}()

	select {
	case res := <-resultCh:
		// The abandoned provider contributes no result.
		assert.Empty(t, res, "enrichment past the per-repo deadline must be abandoned, yielding no result")
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("EnrichAll blocked on a slow provider instead of abandoning it at the deadline")
	}

	// Unblock the detached goroutine so it unwinds cleanly.
	close(release)
	require.Eventually(t, enrichReturned.Load, time.Second, 10*time.Millisecond,
		"the abandoned enrichment goroutine should still drain and return")
}

// TestManager_EnrichOne_DisabledDeadline verifies the bound can be switched
// off: with GORTEX_LSP_ENRICH_TIMEOUT=off a provider runs to completion even
// if slow.
func TestManager_EnrichOne_DisabledDeadline(t *testing.T) {
	t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "off")

	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}
	mgr := NewManager(cfg, zap.NewNop())
	mgr.RegisterProvider(&mockProvider{
		name:      "go",
		languages: []string{"go"},
		available: true,
		enrichFunc: func(g graph.Store, root string) (*EnrichResult, error) {
			time.Sleep(80 * time.Millisecond)
			return &EnrichResult{Provider: "go", Language: "go", EdgesConfirmed: 7}, nil
		},
	})

	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "main.go", Language: "go"})

	results, _, err := mgr.EnrichAll(g, map[string]string{"default": "/tmp/test"}, EnrichOptions{})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, 7, results[0].EdgesConfirmed)
}

// mockCtxProvider is a mockProvider that also implements ContextEnricher,
// so the Manager dispatches it on the cooperative-cancellation path.
type mockCtxProvider struct {
	mockProvider
	enrichCtxFunc func(ctx context.Context, g graph.Store, repoPrefix, repoRoot string) (*EnrichResult, error)
}

func (m *mockCtxProvider) EnrichRepoContext(ctx context.Context, g graph.Store, repoPrefix, repoRoot string) (*EnrichResult, error) {
	return m.enrichCtxFunc(ctx, g, repoPrefix, repoRoot)
}

// TestManager_EnrichOne_ContextProviderPartialIsCounted verifies the
// cooperative deadline path: a ContextEnricher that runs past the
// deadline is cancelled via its context, returns a Partial result, and
// that result is COUNTED (appended to the results, recorded in
// lastResults, surfaced as "partial" in EnrichmentStatuses) instead of
// being discarded like the legacy detach path.
func TestManager_EnrichOne_ContextProviderPartialIsCounted(t *testing.T) {
	t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "50ms")

	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "ctx-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}
	mgr := NewManager(cfg, zap.NewNop())
	mgr.RegisterProvider(&mockCtxProvider{
		mockProvider: mockProvider{
			name:      "ctx-go",
			languages: []string{"go"},
			available: true,
		},
		enrichCtxFunc: func(ctx context.Context, g graph.Store, repoPrefix, repoRoot string) (*EnrichResult, error) {
			// Simulate a pass that lands work incrementally and is cut
			// by the deadline: block until cancellation, then report the
			// work already landed.
			<-ctx.Done()
			return &EnrichResult{
				Provider:       "ctx-go",
				Language:       "go",
				EdgesConfirmed: 3,
				EdgesAdded:     2,
				NodesEnriched:  5,
				Partial:        true,
				AbortReason:    ctx.Err().Error(),
			}, nil
		},
	})

	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "main.go", Language: "go"})

	done := make(chan []*EnrichResult, 1)
	go func() {
		res, _, _ := mgr.EnrichAll(g, map[string]string{"default": "/tmp/test"}, EnrichOptions{})
		done <- res
	}()

	var results []*EnrichResult
	select {
	case results = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("EnrichAll did not return after the context deadline")
	}

	require.Len(t, results, 1, "a partial result must be counted, not discarded")
	assert.True(t, results[0].Partial)
	assert.Equal(t, 3, results[0].EdgesConfirmed)
	assert.Equal(t, 5, results[0].NodesEnriched)

	statuses := mgr.EnrichmentStatuses()
	require.Len(t, statuses, 1)
	assert.Equal(t, "default", statuses[0].Repo)
	assert.Equal(t, "ctx-go", statuses[0].Provider)
	assert.Equal(t, EnrichStatePartial, statuses[0].State)
	assert.Equal(t, 3, statuses[0].EdgesConfirmed)
	assert.Equal(t, 5, statuses[0].NodesEnriched)
	assert.Greater(t, statuses[0].DeadlineSeconds, 0.0)
}

// TestManager_EnrichOne_ContextProviderWedgedPastGraceIsAbandoned
// verifies liveness: a ContextEnricher that ignores its cancellation
// (wedged in an uncancellable call) is abandoned after the grace window
// instead of pinning EnrichAll forever.
func TestManager_EnrichOne_ContextProviderWedgedPastGraceIsAbandoned(t *testing.T) {
	t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "20ms")
	oldGrace := enrichCancelGrace
	enrichCancelGrace = 30 * time.Millisecond
	t.Cleanup(func() { enrichCancelGrace = oldGrace })

	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "wedged-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}
	mgr := NewManager(cfg, zap.NewNop())

	release := make(chan struct{})
	defer close(release)
	mgr.RegisterProvider(&mockCtxProvider{
		mockProvider: mockProvider{
			name:      "wedged-go",
			languages: []string{"go"},
			available: true,
		},
		enrichCtxFunc: func(ctx context.Context, g graph.Store, repoPrefix, repoRoot string) (*EnrichResult, error) {
			<-release // ignores ctx entirely
			return &EnrichResult{Provider: "wedged-go", Language: "go"}, nil
		},
	})

	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "main.go", Language: "go"})

	done := make(chan []*EnrichResult, 1)
	go func() {
		res, _, _ := mgr.EnrichAll(g, map[string]string{"default": "/tmp/test"}, EnrichOptions{})
		done <- res
	}()

	select {
	case res := <-done:
		assert.Empty(t, res, "a provider wedged past deadline+grace must be abandoned")
	case <-time.After(3 * time.Second):
		t.Fatal("EnrichAll blocked on a wedged context provider instead of abandoning it")
	}

	statuses := mgr.EnrichmentStatuses()
	require.Len(t, statuses, 1)
	assert.Equal(t, EnrichStateAbandoned, statuses[0].State)
}

// TestManager_EnrichmentStatuses_AbandonedAndCompleted verifies the
// health surface for the legacy detach path (abandoned — result
// discarded) and the happy path (completed).
func TestManager_EnrichmentStatuses_AbandonedAndCompleted(t *testing.T) {
	t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "50ms")

	cfg := Config{
		Enabled: true,
		Providers: []ProviderConfig{
			{Name: "slow-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
			{Name: "fast-py", Languages: []string{"python"}, Priority: 1, Enabled: true},
		},
	}
	mgr := NewManager(cfg, zap.NewNop())

	release := make(chan struct{})
	defer close(release)
	mgr.RegisterProvider(&mockProvider{
		name:      "slow-go",
		languages: []string{"go"},
		available: true,
		enrichFunc: func(g graph.Store, root string) (*EnrichResult, error) {
			<-release
			return &EnrichResult{Provider: "slow-go", Language: "go"}, nil
		},
	})
	mgr.RegisterProvider(&mockProvider{
		name:      "fast-py",
		languages: []string{"python"},
		available: true,
		enrichFunc: func(g graph.Store, root string) (*EnrichResult, error) {
			return &EnrichResult{Provider: "fast-py", Language: "python", EdgesAdded: 1}, nil
		},
	})

	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "main.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "app.py::run", Kind: graph.KindFunction, Name: "run", FilePath: "app.py", Language: "python"})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = mgr.EnrichAll(g, map[string]string{"default": "/tmp/test"}, EnrichOptions{})
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("EnrichAll blocked instead of abandoning the slow provider")
	}

	byProvider := map[string]EnrichmentStatus{}
	for _, st := range mgr.EnrichmentStatuses() {
		byProvider[st.Provider] = st
	}
	require.Contains(t, byProvider, "slow-go")
	require.Contains(t, byProvider, "fast-py")
	assert.Equal(t, EnrichStateAbandoned, byProvider["slow-go"].State)
	assert.NotEmpty(t, byProvider["slow-go"].Detail)
	assert.Equal(t, EnrichStateCompleted, byProvider["fast-py"].State)
	assert.Equal(t, 1, byProvider["fast-py"].EdgesAdded)
}

// TestScaleEnrichTimeout is the table for the size-scaled per-repo
// deadline: floor for small repos, linear per-node growth, hard ceiling.
func TestScaleEnrichTimeout(t *testing.T) {
	cases := []struct {
		name      string
		nodeCount int
		want      time.Duration
	}{
		{"empty repo gets the floor", 0, 10 * time.Minute},
		{"negative count clamps to the floor", -5, 10 * time.Minute},
		{"small repo stays near the floor", 1000, 10*time.Minute + 40*time.Second},
		{"medium repo scales linearly", 30_000, 30 * time.Minute},
		{"prometheus-sized repo fits under the ceiling", 93_584, 10*time.Minute + time.Duration(93_584)*40*time.Millisecond},
		{"monorepo hits the ceiling", 1_000_000, 90 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, scaleEnrichTimeout(tc.nodeCount))
		})
	}
}

// TestEnrichRepoTimeout_EnvResolution verifies the env override wins
// verbatim over the scaled default, the off switch disables the bound,
// and garbage falls back to the scaled value.
func TestEnrichRepoTimeout_EnvResolution(t *testing.T) {
	t.Run("unset scales with node count", func(t *testing.T) {
		t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "")
		assert.Equal(t, scaleEnrichTimeout(50_000), enrichRepoTimeout(50_000))
	})
	t.Run("explicit override wins verbatim", func(t *testing.T) {
		t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "5m")
		assert.Equal(t, 5*time.Minute, enrichRepoTimeout(1_000_000))
	})
	t.Run("off disables the bound", func(t *testing.T) {
		t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "off")
		assert.Equal(t, time.Duration(0), enrichRepoTimeout(1_000_000))
	})
	t.Run("garbage falls back to the scaled default", func(t *testing.T) {
		t.Setenv("GORTEX_LSP_ENRICH_TIMEOUT", "not-a-duration")
		assert.Equal(t, scaleEnrichTimeout(123), enrichRepoTimeout(123))
	})
}
