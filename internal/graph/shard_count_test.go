package graph

import (
	"fmt"
	"sync"
	"testing"
)

func isPowerOfTwo(n int) bool { return n > 0 && n&(n-1) == 0 }

// TestNextPow2 pins the rounding helper that guarantees the power-of-two
// invariant the shardMask trick depends on.
func TestNextPow2(t *testing.T) {
	cases := map[int]int{
		-3: 1, 0: 1, 1: 1, 2: 2, 3: 4, 4: 4,
		5: 8, 9: 16, 15: 16, 16: 16, 17: 32, 255: 256, 256: 256, 257: 512,
	}
	for in, want := range cases {
		if got := nextPow2(in); got != want {
			t.Errorf("nextPow2(%d) = %d, want %d", in, got, want)
		}
	}
}

// TestCoerceShardCountBounds asserts every coerced count is a power of
// two within [minShards, maxShards] regardless of the input.
func TestCoerceShardCountBounds(t *testing.T) {
	for _, in := range []int{-100, 0, 1, 3, 4, 7, 16, 100, 256, 511, 512, 1000, 1 << 20} {
		got := coerceShardCount(in)
		if !isPowerOfTwo(got) {
			t.Errorf("coerceShardCount(%d) = %d, not a power of two", in, got)
		}
		if got < minShards || got > maxShards {
			t.Errorf("coerceShardCount(%d) = %d, outside [%d, %d]", in, got, minShards, maxShards)
		}
	}
}

// TestDefaultShardCountDerived asserts the CPU-derived default (no
// override) is always a power of two in the derived range.
func TestDefaultShardCountDerived(t *testing.T) {
	t.Setenv(shardCountEnv, "") // ensure no override is in effect
	got := defaultShardCount()
	if !isPowerOfTwo(got) {
		t.Fatalf("defaultShardCount() = %d, not a power of two", got)
	}
	if got < derivedShardFloor || got > derivedShardCeiling {
		t.Fatalf("defaultShardCount() = %d, outside derived range [%d, %d]", got, derivedShardFloor, derivedShardCeiling)
	}
}

// TestGraphShardFieldsConsistent asserts the constructed graph's shard
// bookkeeping is internally consistent: the slice length equals
// shardCount, shardCount is a power of two, and shardMask == count-1.
func TestGraphShardFieldsConsistent(t *testing.T) {
	for _, req := range []int{1, 2, 4, 7, 16, 64, 300, 1000} {
		g := newWithShardCount(req)
		if !isPowerOfTwo(g.shardCount) {
			t.Errorf("newWithShardCount(%d): shardCount = %d, not a power of two", req, g.shardCount)
		}
		if g.shardCount < minShards || g.shardCount > maxShards {
			t.Errorf("newWithShardCount(%d): shardCount = %d, outside [%d, %d]", req, g.shardCount, minShards, maxShards)
		}
		if g.shardMask != uint32(g.shardCount-1) {
			t.Errorf("newWithShardCount(%d): shardMask = %d, want %d", req, g.shardMask, g.shardCount-1)
		}
		if len(g.shards) != g.shardCount {
			t.Errorf("newWithShardCount(%d): len(shards) = %d, want %d", req, len(g.shards), g.shardCount)
		}
		for i, s := range g.shards {
			if s == nil || s.nodes == nil {
				t.Errorf("newWithShardCount(%d): shard %d not initialized", req, i)
			}
		}
	}
}

// TestShardCountEnvOverride asserts GORTEX_GRAPH_SHARDS is honored,
// coerced to a power of two within bounds, and reflected in the graph
// built by New().
func TestShardCountEnvOverride(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"4", 4},      // exact power of two below the derived floor — still honored
		{"5", 8},      // not a power of two — coerced up
		{"64", 64},    // exact
		{"300", 512},  // not a power of two — coerced up
		{"1000", 512}, // above maxShards — clamped
		{"1", 1},      // minimum
	}
	for _, c := range cases {
		t.Run(c.env, func(t *testing.T) {
			t.Setenv(shardCountEnv, c.env)
			if got := defaultShardCount(); got != c.want {
				t.Fatalf("defaultShardCount() with %s=%q = %d, want %d", shardCountEnv, c.env, got, c.want)
			}
			g := New()
			if g.shardCount != c.want {
				t.Fatalf("New().shardCount = %d, want %d", g.shardCount, c.want)
			}
			if g.shardMask != uint32(c.want-1) {
				t.Fatalf("New().shardMask = %d, want %d", g.shardMask, c.want-1)
			}
			if len(g.shards) != c.want {
				t.Fatalf("len(New().shards) = %d, want %d", len(g.shards), c.want)
			}
		})
	}
}

// TestShardCountInvalidEnvFallsBack asserts a malformed or non-positive
// override is ignored and the graph falls back to the derived default.
func TestShardCountInvalidEnvFallsBack(t *testing.T) {
	for _, v := range []string{"abc", "0", "-4", "  ", "3.5"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(shardCountEnv, v)
			got := defaultShardCount()
			if !isPowerOfTwo(got) || got < derivedShardFloor || got > derivedShardCeiling {
				t.Fatalf("override %q: defaultShardCount() = %d, want power-of-two in [%d, %d]", v, got, derivedShardFloor, derivedShardCeiling)
			}
		})
	}
}

// TestShardedConcurrentInsertNoLoss inserts a large, disjoint set of
// nodes and edges from many goroutines into graphs built with several
// shard counts (including non-default 1, 4, 64) and asserts nothing is
// lost or duplicated — proving every shard is correctly initialized and
// indexed regardless of count.
func TestShardedConcurrentInsertNoLoss(t *testing.T) {
	for _, count := range []int{1, 4, 16, 64} {
		t.Run(fmt.Sprintf("shards=%d", count), func(t *testing.T) {
			g := newWithShardCount(count)
			if g.shardCount != count {
				t.Fatalf("newWithShardCount(%d).shardCount = %d", count, g.shardCount)
			}

			const workers = 32
			const perWorker = 500
			var wg sync.WaitGroup
			wg.Add(workers)
			for w := range workers {
				go func(w int) {
					defer wg.Done()
					for i := range perWorker {
						nid := fmt.Sprintf("w%d-n%d::N", w, i)
						g.AddNode(&Node{ID: nid, Name: "N", Kind: KindFunction, FilePath: "f"})
						// One out-edge per node to a shared sink, so the From
						// and To endpoints frequently land in different shards.
						g.AddEdge(&Edge{
							From:     nid,
							To:       fmt.Sprintf("sink-%d::S", i%17),
							Kind:     EdgeCalls,
							FilePath: "f",
							Line:     w,
						})
					}
				}(w)
			}
			wg.Wait()

			wantNodes := workers * perWorker
			for w := range workers {
				for i := range perWorker {
					nid := fmt.Sprintf("w%d-n%d::N", w, i)
					if g.GetNode(nid) == nil {
						t.Fatalf("node %q missing after concurrent insert (shards=%d)", nid, count)
					}
					if outs := g.GetOutEdges(nid); len(outs) != 1 {
						t.Fatalf("node %q has %d out-edges, want 1 (shards=%d)", nid, len(outs), count)
					}
				}
			}
			// Sink targets are never AddNode'd, so NodeCount counts only the
			// inserted N nodes — exactly one per (worker, index) pair.
			if got := g.NodeCount(); got != wantNodes {
				t.Fatalf("NodeCount() = %d, want %d (shards=%d)", got, wantNodes, count)
			}
		})
	}
}
