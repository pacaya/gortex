package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestLuaExtractor_Functions(t *testing.T) {
	src := []byte(`
function greet(name)
  print("Hello, " .. name)
end

function add(a, b)
  return a + b
end

local function helper()
  return 42
end
`)
	e := NewLuaExtractor()
	result, err := e.Extract("utils.lua", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	names := make([]string, len(funcs))
	for i, f := range funcs {
		names[i] = f.Name
	}
	assert.Contains(t, names, "greet")
	assert.Contains(t, names, "add")
	assert.Contains(t, names, "helper")
}

func TestLuaExtractor_Methods(t *testing.T) {
	src := []byte(`
local M = {}

function M.init()
  M.value = 0
end

function M.getValue()
  return M.value
end
`)
	e := NewLuaExtractor()
	result, err := e.Extract("module.lua", src)
	require.NoError(t, err)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.GreaterOrEqual(t, len(methods), 2)
	for _, m := range methods {
		assert.Equal(t, "M", m.Meta["receiver"])
	}

	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	require.GreaterOrEqual(t, len(memberEdges), 2)
}

func TestLuaExtractor_Variables(t *testing.T) {
	src := []byte(`
local version = "1.0"
local count = 42
`)
	e := NewLuaExtractor()
	result, err := e.Extract("config.lua", src)
	require.NoError(t, err)

	vars := nodesOfKind(result.Nodes, graph.KindVariable)
	require.Len(t, vars, 2)
}

func TestLuaExtractor_Require(t *testing.T) {
	src := []byte(`
require("utils")
require("lib.json")

local x = 1
`)
	e := NewLuaExtractor()
	result, err := e.Extract("main.lua", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	require.Len(t, imports, 2)
}

func TestLuaExtractor_CallSites(t *testing.T) {
	src := []byte(`
function main()
  print("hello")
  greet("world")
end
`)
	e := NewLuaExtractor()
	result, err := e.Extract("main.lua", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	assert.GreaterOrEqual(t, len(calls), 2)
}
