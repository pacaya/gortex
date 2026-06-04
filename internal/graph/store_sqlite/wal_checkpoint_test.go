package store_sqlite

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// TestCheckpointWALBoundsFileAndPreservesData writes enough rows to push pages
// into the -wal file, then forces a TRUNCATE checkpoint and asserts the WAL is
// drained (bounded well under journal_size_limit) without losing any data.
func TestCheckpointWALBoundsFileAndPreservesData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// On-disk stores must arm the background checkpoint loop.
	if s.stopCheckpoint == nil || s.checkpointDone == nil {
		t.Fatal("on-disk store did not start the WAL-checkpoint loop")
	}

	const n = 4000
	nodes := make([]*graph.Node, 0, n)
	for i := range n {
		nodes = append(nodes, &graph.Node{
			ID:       fmt.Sprintf("pkg/f.go::Sym%d", i),
			Kind:     graph.KindFunction,
			Name:     fmt.Sprintf("Sym%d", i),
			FilePath: "pkg/f.go",
			Language: "go",
		})
	}
	s.AddBatch(nodes, nil)

	if err := s.CheckpointWAL(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	// journal_size_limit caps the WAL at 64 MiB; after a TRUNCATE checkpoint
	// with no concurrent reader it should be far smaller still.
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() > 64<<20 {
		t.Fatalf("wal not bounded after checkpoint: %d bytes", fi.Size())
	}

	if got := s.NodeCount(); got != n {
		t.Fatalf("checkpoint lost data: NodeCount = %d, want %d", got, n)
	}
}

// TestCloseStopsCheckpointLoop verifies Close signals the loop and waits for it
// to exit, and that calling Close twice does not panic on the stop channel.
func TestCloseStopsCheckpointLoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	done := s.checkpointDone

	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("checkpoint loop did not stop within 2s of Close")
	}

	// stopCheckpointLoop is guarded by sync.Once, so a second stop is a no-op
	// rather than a close-of-closed-channel panic.
	s.stopCheckpointLoop()
}

// TestInMemoryStoreSkipsCheckpointLoop confirms ":memory:" stores, which have
// no WAL, never spawn the checkpoint goroutine.
func TestInMemoryStoreSkipsCheckpointLoop(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.stopCheckpoint != nil || s.checkpointDone != nil {
		t.Fatal("in-memory store should not arm the WAL-checkpoint loop")
	}
}
