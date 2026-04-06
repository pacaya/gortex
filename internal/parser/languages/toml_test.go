package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestTOMLExtractor_Tables(t *testing.T) {
	src := []byte(`[package]
name = "my-app"
version = "0.1.0"

[dependencies]
serde = "1.0"
`)
	e := NewTOMLExtractor()
	result, err := e.Extract("Cargo.toml", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	assert.GreaterOrEqual(t, len(types), 2, "should extract [package] and [dependencies]")
}

func TestTOMLExtractor_KeyValuePairs(t *testing.T) {
	src := []byte(`name = "my-app"
version = "1.0"
`)
	e := NewTOMLExtractor()
	result, err := e.Extract("config.toml", src)
	require.NoError(t, err)

	vars := nodesOfKind(result.Nodes, graph.KindVariable)
	assert.GreaterOrEqual(t, len(vars), 2, "should extract name and version")
}

func TestTOMLExtractor_FileNode(t *testing.T) {
	src := []byte(`key = "value"
`)
	e := NewTOMLExtractor()
	result, err := e.Extract("test.toml", src)
	require.NoError(t, err)

	files := nodesOfKind(result.Nodes, graph.KindFile)
	assert.Equal(t, 1, len(files))
	assert.Equal(t, "test.toml", files[0].Name)
}
