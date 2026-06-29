package resolver

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// TestCrossRepoResolveAll_ConcurrentEdits is the safety gate for the chunked
// resolve. It runs CrossRepoResolver.ResolveAll while an "editor" goroutine
// repeatedly evicts and re-indexes caller files, taking the SAME resolve mutex
// an interactive single-file edit takes — exactly the interleaving the chunked
// path enables. Without the resolveEdge liveness guards this corrupts the graph
// (ReindexEdge half-resurrects an evicted edge and later panics with an
// index-out-of-range during eviction); with them the run is clean and every
// resolved edge points at a live node.
//
// Run with -race -count=N for scheduling variation.
func TestCrossRepoResolveAll_ConcurrentEdits(t *testing.T) {
	const (
		files          = 40
		callersPerFile = 600 // 24000 pending -> ~12 chunks at the default 2048
	)
	g := graph.New()

	callerFile := func(k int) string { return fmt.Sprintf("repoA/a%d.go", k) }

	// repoB targets: one Helper per (file,caller) slot.
	for k := 0; k < files; k++ {
		for i := 0; i < callersPerFile; i++ {
			id := fmt.Sprintf("repoB/c%d_%d.go::Helper%d_%d", k, i, k, i)
			g.AddNode(&graph.Node{
				ID: id, Kind: graph.KindFunction, Name: fmt.Sprintf("Helper%d_%d", k, i),
				FilePath: fmt.Sprintf("repoB/c%d_%d.go", k, i), Language: "go", RepoPrefix: "repoB",
			})
		}
	}

	// addCallerFile (re)indexes one repoA caller file: its caller functions, the
	// unresolved call edges into repoB, and the import-reachability evidence.
	addCallerFile := func(k int) {
		for i := 0; i < callersPerFile; i++ {
			g.AddNode(&graph.Node{
				ID: fmt.Sprintf("%s::Caller%d_%d", callerFile(k), k, i), Kind: graph.KindFunction,
				Name: fmt.Sprintf("Caller%d_%d", k, i), FilePath: callerFile(k), Language: "go", RepoPrefix: "repoA",
			})
			g.AddEdge(&graph.Edge{
				From: fmt.Sprintf("%s::Caller%d_%d", callerFile(k), k, i),
				To:   fmt.Sprintf("unresolved::Helper%d_%d", k, i),
				Kind: graph.EdgeCalls, FilePath: callerFile(k), Line: i + 1,
			})
		}
		wireImport(g, callerFile(k), "repoB", fmt.Sprintf("repoB/c%d_0.go", k))
	}
	for k := 0; k < files; k++ {
		addCallerFile(k)
	}

	var resolveDone atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		cr := NewCrossRepo(g)
		cr.ResolveAll()
		resolveDone.Store(true)
	}()

	// Editor: between ResolveAll's chunks (when it yields the resolve mutex),
	// re-index a caller file — evict it (drops its nodes + the call edges the
	// resolver's pending snapshot still points at) and add it back. Takes the
	// resolve mutex, exactly as an interactive edit's ResolveFileAndIncoming does.
	mu := g.ResolveMutex()
	var edits int
	for k := 0; !resolveDone.Load(); k = (k + 1) % files {
		mu.Lock()
		g.EvictFile(callerFile(k))
		addCallerFile(k)
		mu.Unlock()
		edits++
		runtime.Gosched()
	}
	wg.Wait()

	require.Greater(t, edits, 0, "editor never interleaved — increase the work size")

	// No resolved edge may point at a missing node: that is the dangling-edge /
	// half-resurrection signature the guards exist to prevent.
	for _, e := range g.AllEdges() {
		if e == nil || strings.HasPrefix(e.To, "unresolved::") || isSyntheticResolveTarget(e.To) {
			continue
		}
		require.NotNilf(t, g.GetNode(e.To),
			"edge %s -> %s resolved to a node not in the graph (dangling)", e.From, e.To)
	}
}
