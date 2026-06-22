package rerank

import (
	"strconv"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// addCallers wires n incoming EdgeCalls into target so the fan-in signal
// sees it as load-bearing. Caller nodes are throwaway functions.
func addCallers(g *graph.Graph, targetID, prefix string, n int) {
	for i := range n {
		cid := prefix + strconv.Itoa(i)
		g.AddNode(&graph.Node{ID: cid, Name: cid, Kind: graph.KindFunction, FilePath: "callers.go"})
		g.AddEdge(&graph.Edge{From: cid, To: targetID, Kind: graph.EdgeCalls})
	}
}

func rankOf(out []*Candidate, id string) int {
	for i, c := range out {
		if c.Node.ID == id {
			return i
		}
	}
	return -1
}

func idOrder(out []*Candidate) string {
	var b strings.Builder
	for i, c := range out {
		if i > 0 {
			b.WriteString(" > ")
		}
		b.WriteString(c.Node.ID)
	}
	return b.String()
}

func scoreOf(out []*Candidate, id string) float64 {
	for _, c := range out {
		if c.Node.ID == id {
			return c.Score
		}
	}
	return -1
}

// TestCanonicalPick_TypeBeatsMethodForTypeQuery: a bare "Session" query must
// land on the type `Session`, not a `urlSession` method that merely shares the
// camelCase token — even when the method is more heavily called and BM25-ranked
// ahead. Mirrors the resolver's bestTypeCandidate canonical pick on the search
// side. Adversarial: the method gets the better BM25 rank AND far more fan-in.
func TestCanonicalPick_TypeBeatsMethodForTypeQuery(t *testing.T) {
	g := newTestGraph()
	sess := mustNode(g, "net/session.go::Session", "Session", graph.KindType)
	sess.FilePath = "net/session.go"
	urlSess := mustNode(g, "net/client.go::urlSession", "urlSession", graph.KindMethod)
	urlSess.FilePath = "net/client.go"

	addCallers(g, urlSess.ID, "callurl", 30)
	addCallers(g, sess.ID, "refsess", 1)

	// Stack the deck against the type: the method is BM25 #1.
	cands := []*Candidate{
		candidateFor(urlSess, 0, -1),
		candidateFor(sess, 1, -1),
	}
	out := NewDefault().Rerank("Session", cands, &Context{Graph: g})
	if rankOf(out, sess.ID) != 0 {
		t.Fatalf("expected type Session ranked #1 for query \"Session\"; got %s (Session=%.3f urlSession=%.3f)",
			idOrder(out), scoreOf(out, sess.ID), scoreOf(out, urlSess.ID))
	}
}

// TestCanonicalPick_ProductionBeatsTestDef: a bare "Model" query must land on
// the production type, not a same-named definition in a _test.go file — even
// when the test fixture is BM25 #1 and more heavily referenced.
func TestCanonicalPick_ProductionBeatsTestDef(t *testing.T) {
	g := newTestGraph()
	prod := mustNode(g, "app/model.go::Model", "Model", graph.KindType)
	prod.FilePath = "app/model.go"
	test := mustNode(g, "app/model_test.go::Model", "Model", graph.KindType)
	test.FilePath = "app/model_test.go"

	addCallers(g, test.ID, "reftest", 15)
	addCallers(g, prod.ID, "refprod", 1)

	cands := []*Candidate{
		candidateFor(test, 0, -1),
		candidateFor(prod, 1, -1),
	}
	out := NewDefault().Rerank("Model", cands, &Context{Graph: g})
	if rankOf(out, prod.ID) != 0 {
		t.Fatalf("expected production Model ranked #1 for query \"Model\"; got %s (prod=%.3f test=%.3f)",
			idOrder(out), scoreOf(out, prod.ID), scoreOf(out, test.ID))
	}
}
