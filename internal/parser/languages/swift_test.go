package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestSwiftExtractor_ClassWithMethods(t *testing.T) {
	src := []byte(`class Server {
    var port: Int

    func start() {
        print("starting")
    }

    func stop() {
        print("stopping")
    }
}
`)
	e := NewSwiftExtractor()
	result, err := e.Extract("server.swift", src)
	require.NoError(t, err)

	// Class should be extracted as KindType.
	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "Server", types[0].Name)

	// Methods inside the class.
	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.Len(t, methods, 2)
	names := []string{methods[0].Name, methods[1].Name}
	assert.Contains(t, names, "start")
	assert.Contains(t, names, "stop")

	// Methods should NOT appear as top-level functions.
	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	assert.Len(t, funcs, 0)

	// EdgeMemberOf edges.
	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	assert.Len(t, memberEdges, 2)
	for _, e := range memberEdges {
		assert.Equal(t, "server.swift::Server", e.To)
	}
}

func TestSwiftExtractor_Struct(t *testing.T) {
	src := []byte(`struct Config {
    var port: Int
    var host: String
}
`)
	e := NewSwiftExtractor()
	result, err := e.Extract("config.swift", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "Config", types[0].Name)
}

func TestSwiftExtractor_Protocol(t *testing.T) {
	src := []byte(`protocol Repository {
    func findById(id: String) -> User?
    func save(user: User)
}
`)
	e := NewSwiftExtractor()
	result, err := e.Extract("store.swift", src)
	require.NoError(t, err)

	ifaces := nodesOfKind(result.Nodes, graph.KindInterface)
	require.Len(t, ifaces, 1)
	assert.Equal(t, "Repository", ifaces[0].Name)

	// Protocol methods in Meta.
	methods, ok := ifaces[0].Meta["methods"].([]string)
	require.True(t, ok)
	assert.Equal(t, []string{"findById", "save"}, methods)
}

func TestSwiftExtractor_Imports(t *testing.T) {
	src := []byte(`import Foundation
import UIKit
`)
	e := NewSwiftExtractor()
	result, err := e.Extract("main.swift", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	require.Len(t, imports, 2)
}

func TestSwiftExtractor_Enum(t *testing.T) {
	src := []byte(`enum Direction {
    case north
    case south
    case east
    case west
}
`)
	e := NewSwiftExtractor()
	result, err := e.Extract("direction.swift", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "Direction", types[0].Name)
}

func TestSwiftExtractor_FreeFunction(t *testing.T) {
	src := []byte(`func greet(name: String) -> String {
    return "Hello \(name)"
}
`)
	e := NewSwiftExtractor()
	result, err := e.Extract("greet.swift", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	require.Len(t, funcs, 1)
	assert.Equal(t, "greet", funcs[0].Name)
}
