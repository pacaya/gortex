package serverstack

import (
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// TestOpenBackend_MemoryDefault asserts the memory backend (and the empty
// default) returns a usable in-process store.
func TestOpenBackend_MemoryDefault(t *testing.T) {
	for _, name := range []string{"", "memory", "mem", "in-memory"} {
		store, cleanup, err := OpenBackend(name, "", 0, zap.NewNop(), false)
		if err != nil {
			t.Fatalf("OpenBackend(%q): %v", name, err)
		}
		if store == nil {
			t.Fatalf("OpenBackend(%q): nil store", name)
		}
		if _, ok := store.(*graph.Graph); !ok {
			t.Errorf("OpenBackend(%q): want *graph.Graph, got %T", name, store)
		}
		cleanup()
	}
}

// TestOpenBackend_SqliteOpensFile asserts the sqlite backend opens (and
// creates) a store at the resolved path.
func TestOpenBackend_SqliteOpensFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	store, cleanup, err := OpenBackend("sqlite", path, 0, zap.NewNop(), true)
	if err != nil {
		t.Fatalf("OpenBackend(sqlite): %v", err)
	}
	if store == nil {
		t.Fatal("nil sqlite store")
	}
	cleanup()
}

// TestOpenBackend_Unknown asserts only memory|sqlite are accepted — a
// stale backend name (e.g. the removed ladybug) errors rather than
// silently falling back.
func TestOpenBackend_Unknown(t *testing.T) {
	if _, _, err := OpenBackend("ladybug", "", 0, zap.NewNop(), false); err == nil {
		t.Fatal("an unknown backend must error")
	}
}
