package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRExtractor_Functions(t *testing.T) {
	src := []byte(`add <- function(a, b) {
  a + b
}

multiply = function(x, y) {
  x * y
}
`)
	e := NewRExtractor()
	result, err := e.Extract("math.R", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	names := make([]string, len(funcs))
	for i, f := range funcs {
		names[i] = f.Name
	}
	assert.Contains(t, names, "add")
	assert.Contains(t, names, "multiply")
}

func TestRExtractor_Imports(t *testing.T) {
	src := []byte(`library(ggplot2)
require(dplyr)
source("utils.R")
`)
	e := NewRExtractor()
	result, err := e.Extract("main.R", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	assert.Len(t, imports, 3)
}

func TestRExtractor_Variables(t *testing.T) {
	src := []byte(`max_size <- 100
threshold = 0.5
name <- "test"
`)
	e := NewRExtractor()
	result, err := e.Extract("config.R", src)
	require.NoError(t, err)

	vars := nodesOfKind(result.Nodes, graph.KindVariable)
	varNames := make([]string, len(vars))
	for i, v := range vars {
		varNames[i] = v.Name
	}
	assert.Contains(t, varNames, "max_size")
	assert.Contains(t, varNames, "threshold")
	assert.Contains(t, varNames, "name")
}

// TestRClassSystemsAndDispatch is the C6 test: S4 (setClass+contains,
// setMethod), R6/Reference classes, and S3 generic.class methods all extract,
// and a call to a generic reaches its methods through dispatch edges.
func TestRClassSystemsAndDispatch(t *testing.T) {
	src := []byte("setClass(\"Circle\", contains = \"Shape\")\n" +
		"setGeneric(\"area\", function(obj) standardGeneric(\"area\"))\n" +
		"setMethod(\"area\", \"Circle\", function(obj) pi)\n" +
		"Counter <- R6Class(\"Counter\", public = list())\n" +
		"Acc <- setRefClass(\"Account\")\n" +
		"print.Circle <- function(x) cat(\"c\")\n")
	res, err := NewRExtractor().Extract("m.R", src)
	require.NoError(t, err)

	kind := map[string]graph.NodeKind{}
	system := map[string]string{}
	for _, n := range res.Nodes {
		kind[n.ID] = n.Kind
		if n.Meta != nil {
			if s, _ := n.Meta["class_system"].(string); s != "" {
				system[n.ID] = s
			}
		}
	}
	assert.Equal(t, graph.KindType, kind["m.R::Circle"], "S4 class")
	assert.Equal(t, "S4", system["m.R::Circle"])
	assert.Equal(t, graph.KindMethod, kind["m.R::area.Circle"], "S4 method")
	assert.Equal(t, graph.KindType, kind["m.R::Counter"], "R6 class")
	assert.Equal(t, "R6", system["m.R::Counter"])
	assert.Equal(t, graph.KindType, kind["m.R::Account"], "Reference class")

	var s4Dispatch, s3Dispatch, inherit bool
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeCalls && e.From == "m.R::area" && e.To == "m.R::area.Circle" {
			s4Dispatch = true
		}
		if e.Kind == graph.EdgeCalls && e.From == "m.R::print" && e.To == "m.R::print.Circle" {
			s3Dispatch = true
		}
		if e.Kind == graph.EdgeExtends && e.From == "m.R::Circle" && e.To == "unresolved::Shape" {
			inherit = true
		}
	}
	assert.True(t, s4Dispatch, "the S4 generic area should dispatch to its Circle method")
	assert.True(t, s3Dispatch, "the S3 generic print should dispatch to print.Circle")
	assert.True(t, inherit, "setClass contains= should produce an inheritance edge")
}

// TestRPkgFnDispatchPreserved proves the namespace + extract-dispatch recall
// the tag pass dropped: a `pkg::fn(...)` call preserves its package qualifier
// in the edge target (so `dplyr::filter` stays distinct from a base-R
// `filter`), and an `obj$method(...)` extract-dispatch — which the tag pass
// records as nothing — gets its own receiver-tagged call edge.
func TestRPkgFnDispatchPreserved(t *testing.T) {
	src := []byte("result <- dplyr::filter(df, x > 1)\nobj$method(a)\nval <- base::sum(v)\nplain(z)\n")
	res, err := NewRExtractor().Extract("a.R", src)
	require.NoError(t, err)

	namespace := map[string]string{} // call target -> package
	var dollar *graph.Edge
	for _, e := range res.Edges {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		if ns, _ := e.Meta["r_namespace"].(string); ns != "" {
			namespace[e.To] = ns
		}
		if via, _ := e.Meta["via"].(string); via == "dollar_dispatch" {
			dollar = e
		}
	}

	// Package qualifier preserved, in-place (no bare duplicate).
	assert.Equal(t, "dplyr", namespace["unresolved::dplyr::filter"], "dplyr::filter must carry its package")
	assert.Equal(t, "base", namespace["unresolved::base::sum"], "base::sum must carry its package")
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeCalls && (e.To == "unresolved::filter" || e.To == "unresolved::sum") {
			t.Errorf("namespace call left a stripped bare edge %q (should be rewritten)", e.To)
		}
	}

	// $-dispatch edge exists and preserves the receiver.
	require.NotNil(t, dollar, "obj$method() must emit a dollar_dispatch call edge")
	assert.Equal(t, "unresolved::method", dollar.To)
	r, _ := dollar.Meta["r_receiver"].(string)
	assert.Equal(t, "obj", r, "dollar dispatch must preserve the receiver")
}
