package query

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

// The test graph's call chain is main -> Start -> Connect -> Ping.
// These tests pin a community over a subset of those nodes and assert
// the walk's community gate keeps in-community neighbours, drops
// cross-community ones, and lets membership-less nodes through.

func TestWalkBudgeted_CommunityFilter_DropsCrossCommunity(t *testing.T) {
	e := NewEngine(buildTestGraph())

	// main + Start are in community "a"; Connect is in community "b".
	nodeToComm := map[string]string{
		"main.go::main":        "a",
		"pkg/server.go::Start": "a",
		"pkg/db.go::Connect":   "b",
		"pkg/db.go::Ping":      "b",
	}

	sg := e.WalkBudgeted("main.go::main", WalkOptions{
		EdgeKinds:   []graph.EdgeKind{graph.EdgeCalls},
		Direction:   "out",
		CommunityID: "a",
		NodeToComm:  nodeToComm,
	})
	ids := nodeIDs(sg.Nodes)
	assert.Contains(t, ids, "main.go::main")
	assert.Contains(t, ids, "pkg/server.go::Start")
	// Connect is in community "b" — dropped, and Ping behind it never
	// gets reached.
	assert.NotContains(t, ids, "pkg/db.go::Connect")
	assert.NotContains(t, ids, "pkg/db.go::Ping")
}

func TestWalkBudgeted_CommunityFilter_KeepsInCommunity(t *testing.T) {
	e := NewEngine(buildTestGraph())

	// Whole chain in community "a".
	nodeToComm := map[string]string{
		"main.go::main":        "a",
		"pkg/server.go::Start": "a",
		"pkg/db.go::Connect":   "a",
		"pkg/db.go::Ping":      "a",
	}

	sg := e.WalkBudgeted("main.go::main", WalkOptions{
		EdgeKinds:   []graph.EdgeKind{graph.EdgeCalls},
		Direction:   "out",
		CommunityID: "a",
		NodeToComm:  nodeToComm,
	})
	ids := nodeIDs(sg.Nodes)
	assert.Contains(t, ids, "pkg/server.go::Start")
	assert.Contains(t, ids, "pkg/db.go::Connect")
	assert.Contains(t, ids, "pkg/db.go::Ping")
}

func TestWalkBudgeted_CommunityFilter_StructuralNodePassthrough(t *testing.T) {
	e := NewEngine(buildTestGraph())

	// Only main + Start carry a membership. Connect / Ping have NO
	// entry — they are treated as membership-less structural nodes and
	// must pass the gate (an absent entry is not exclusion).
	nodeToComm := map[string]string{
		"main.go::main":        "a",
		"pkg/server.go::Start": "a",
	}

	sg := e.WalkBudgeted("main.go::main", WalkOptions{
		EdgeKinds:   []graph.EdgeKind{graph.EdgeCalls},
		Direction:   "out",
		CommunityID: "a",
		NodeToComm:  nodeToComm,
	})
	ids := nodeIDs(sg.Nodes)
	assert.Contains(t, ids, "pkg/server.go::Start")
	// Connect + Ping have no membership -> not excluded.
	assert.Contains(t, ids, "pkg/db.go::Connect")
	assert.Contains(t, ids, "pkg/db.go::Ping")
}

func TestWalkBudgeted_CommunityFilter_NilMapNoOp(t *testing.T) {
	e := NewEngine(buildTestGraph())

	// CommunityID set but NodeToComm nil — the filter must no-op (the
	// production wiring passes nil when analysis hasn't run).
	sg := e.WalkBudgeted("main.go::main", WalkOptions{
		EdgeKinds:   []graph.EdgeKind{graph.EdgeCalls},
		Direction:   "out",
		CommunityID: "a",
		NodeToComm:  nil,
	})
	ids := nodeIDs(sg.Nodes)
	assert.Contains(t, ids, "pkg/server.go::Start")
	assert.Contains(t, ids, "pkg/db.go::Connect")
	assert.Contains(t, ids, "pkg/db.go::Ping")
}

func TestWalkBudgeted_CommunityFilter_EmptyIDNoOp(t *testing.T) {
	e := NewEngine(buildTestGraph())

	// Empty CommunityID disables the gate even when NodeToComm is set.
	sg := e.WalkBudgeted("main.go::main", WalkOptions{
		EdgeKinds:   []graph.EdgeKind{graph.EdgeCalls},
		Direction:   "out",
		CommunityID: "",
		NodeToComm:  map[string]string{"pkg/db.go::Connect": "b"},
	})
	assert.Contains(t, nodeIDs(sg.Nodes), "pkg/db.go::Connect")
}
