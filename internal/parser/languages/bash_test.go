package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestBashExtractor_Functions(t *testing.T) {
	src := []byte(`#!/bin/bash

function greet() {
    echo "Hello $1"
}

deploy() {
    echo "deploying..."
}
`)
	e := NewBashExtractor()
	result, err := e.Extract("deploy.sh", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	require.Len(t, funcs, 2)

	names := make(map[string]bool)
	for _, f := range funcs {
		names[f.Name] = true
		assert.Equal(t, "bash", f.Language)
	}
	assert.True(t, names["greet"], "expected function 'greet'")
	assert.True(t, names["deploy"], "expected function 'deploy'")

	// Both should have EdgeDefines from the file node.
	defines := edgesOfKind(result.Edges, graph.EdgeDefines)
	funcDefines := 0
	for _, e := range defines {
		if e.From == "deploy.sh" {
			funcDefines++
		}
	}
	assert.GreaterOrEqual(t, funcDefines, 2)
}

func TestBashExtractor_Variables(t *testing.T) {
	src := []byte(`#!/bin/bash

APP_NAME="myapp"
VERSION=1
LOCAL_VAR="hello"

my_func() {
    INNER="should not be extracted"
}
`)
	e := NewBashExtractor()
	result, err := e.Extract("config.sh", src)
	require.NoError(t, err)

	vars := nodesOfKind(result.Nodes, graph.KindVariable)
	require.Len(t, vars, 3, "expected 3 top-level variables")

	names := make(map[string]bool)
	for _, v := range vars {
		names[v.Name] = true
	}
	assert.True(t, names["APP_NAME"])
	assert.True(t, names["VERSION"])
	assert.True(t, names["LOCAL_VAR"])
}

func TestBashExtractor_SourceImports(t *testing.T) {
	src := []byte(`#!/bin/bash

source ./lib/utils.sh
. ./lib/helpers.sh
source config.sh
`)
	e := NewBashExtractor()
	result, err := e.Extract("main.sh", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	require.Len(t, imports, 3)

	targets := make(map[string]bool)
	for _, imp := range imports {
		targets[imp.To] = true
	}
	assert.True(t, targets["unresolved::import::./lib/utils.sh"])
	assert.True(t, targets["unresolved::import::./lib/helpers.sh"])
	assert.True(t, targets["unresolved::import::config.sh"])
}

func TestBashExtractor_CallSites(t *testing.T) {
	src := []byte(`#!/bin/bash

deploy() {
    docker build .
    kubectl apply -f deploy.yaml
}
`)
	e := NewBashExtractor()
	result, err := e.Extract("run.sh", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	require.Len(t, calls, 2)

	callNames := make(map[string]bool)
	for _, c := range calls {
		callNames[c.To] = true
	}
	assert.True(t, callNames["unresolved::docker"])
	assert.True(t, callNames["unresolved::kubectl"])
}
