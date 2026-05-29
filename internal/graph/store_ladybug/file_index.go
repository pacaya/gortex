package store_ladybug

import (
	"sync"

	"github.com/zzet/gortex/internal/graph"
)

// fileIDIndex is a Go-side accelerator that maps each file path to the
// set of node IDs anchored to that file. Kuzu does not expose a
// secondary index on `Node.file_path`, so every "find the symbols in
// this file" lookup defaulted to a full Node-table scan
// (`MATCH (n {file_path: $f})` — 213 k rows on the gortex graph for one
// call). This map turns the lookup into a single RLock + map probe, at
// a per-node cost of one string slot in a set entry.
//
// The set form (map[id]struct{}) is intentional: AddBatch / AddNode
// can be called multiple times for the same node id (the indexer
// re-runs after an incremental re-index, the resolver re-stamps
// metadata) and we want idempotent membership rather than duplicated
// slice entries.
//
// Concurrency: the store's writeMu serialises mutations, so every
// add/remove call already runs under that lock when invoked from the
// store's public API. The dedicated fileMu only guards the readers
// (GetFileSubGraph and friends), which run without writeMu. Holding a
// finer-grained mutex than writeMu lets readers proceed in parallel
// with each other even when a writer is mid-commit.
type fileIDIndex struct {
	mu sync.RWMutex
	m  map[string]map[string]struct{}
}

func newFileIDIndex() *fileIDIndex {
	return &fileIDIndex{m: make(map[string]map[string]struct{})}
}

// add registers (id, filePath). No-op when either is empty.
func (f *fileIDIndex) add(filePath, id string) {
	if filePath == "" || id == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	set, ok := f.m[filePath]
	if !ok {
		set = make(map[string]struct{}, 4)
		f.m[filePath] = set
	}
	set[id] = struct{}{}
}

// addNodes bulk-loads node IDs in one lock acquisition. The bulk-load
// fast path drains thousands of nodes per call; per-node add() would
// thrash the mutex.
func (f *fileIDIndex) addNodes(nodes []*graph.Node) {
	if len(nodes) == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range nodes {
		if n == nil || n.ID == "" || n.FilePath == "" {
			continue
		}
		set, ok := f.m[n.FilePath]
		if !ok {
			set = make(map[string]struct{}, 4)
			f.m[n.FilePath] = set
		}
		set[n.ID] = struct{}{}
	}
}

// removeFile drops every entry for filePath.
func (f *fileIDIndex) removeFile(filePath string) {
	if filePath == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, filePath)
}

// removeFiles drops every entry under any of paths. Used by
// EvictRepo (which first asks the store which file paths belong to
// the repo, then forwards the list here).
func (f *fileIDIndex) removeFiles(paths []string) {
	if len(paths) == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range paths {
		delete(f.m, p)
	}
}

// idsFor returns a copy of the id set for filePath, or nil. Returning a
// slice rather than the underlying map keeps callers' iteration
// independent of subsequent writes — they don't need to hold the lock
// past the call.
func (f *fileIDIndex) idsFor(filePath string) []string {
	if filePath == "" {
		return nil
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	set := f.m[filePath]
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out
}
