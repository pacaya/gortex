package analysis

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// TestScoreEntryPoint_JavaVisibility checks a private Java helper is not
// handed the public-API entry-point boost (it must score below an
// otherwise-identical public method).
func TestScoreEntryPoint_JavaVisibility(t *testing.T) {
	pub := &graph.Node{
		ID: "S.java::doWork", Kind: graph.KindMethod, Name: "doWork", Language: "java",
		Meta: map[string]any{"visibility": "public"},
	}
	priv := &graph.Node{
		ID: "S.java::doWorkImpl", Kind: graph.KindMethod, Name: "doWorkImpl", Language: "java",
		Meta: map[string]any{"visibility": "private"},
	}
	pubScore := scoreEntryPoint(pub, 3, 0)
	privScore := scoreEntryPoint(priv, 3, 0)
	require.Greater(t, pubScore, privScore, "public method must outscore an identical private one")
}

// TestScoreEntryPoint_JavaEntryBoost checks a stamped Spring handler is
// boosted as a process root, while a stamped JUnit test is not (tests
// stay live for dead-code but are noise as top-level processes).
func TestScoreEntryPoint_JavaEntryBoost(t *testing.T) {
	// All three share a name with no entry/util pattern boost, so the
	// only score difference comes from the entry-point stamp.
	plain := &graph.Node{
		ID: "S.java::execute", Kind: graph.KindMethod, Name: "execute", Language: "java",
		Meta: map[string]any{"visibility": "public"},
	}
	handler := &graph.Node{
		ID: "C.java::execute", Kind: graph.KindMethod, Name: "execute", Language: "java",
		Meta: map[string]any{"visibility": "public", "entry_point": true, "entry_point_kind": "spring:handler"},
	}
	test := &graph.Node{
		ID: "T.java::execute", Kind: graph.KindMethod, Name: "execute", Language: "java",
		Meta: map[string]any{"visibility": "public", "entry_point": true, "entry_point_kind": "junit:test"},
	}
	require.Greater(t, scoreEntryPoint(handler, 3, 0), scoreEntryPoint(plain, 3, 0),
		"a stamped Spring handler must outscore a plain method")
	require.Equal(t, scoreEntryPoint(test, 3, 0), scoreEntryPoint(plain, 3, 0),
		"a JUnit test must not get the process-root boost")
}
