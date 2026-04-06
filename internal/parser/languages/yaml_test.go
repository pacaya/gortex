package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestYAMLExtractor_TopLevelKeys(t *testing.T) {
	src := []byte(`name: my-app
version: "1.0"
services:
  web:
    image: nginx
  db:
    image: postgres
`)
	e := NewYAMLExtractor()
	result, err := e.Extract("docker-compose.yml", src)
	require.NoError(t, err)

	vars := nodesOfKind(result.Nodes, graph.KindVariable)
	assert.GreaterOrEqual(t, len(vars), 3, "should extract at least name, version, services")

	defines := edgesOfKind(result.Edges, graph.EdgeDefines)
	assert.GreaterOrEqual(t, len(defines), 3)
}

func TestYAMLExtractor_SimpleMapping(t *testing.T) {
	src := []byte(`database:
  host: localhost
  port: 5432
logging:
  level: info
`)
	e := NewYAMLExtractor()
	result, err := e.Extract("config.yaml", src)
	require.NoError(t, err)

	vars := nodesOfKind(result.Nodes, graph.KindVariable)
	names := make(map[string]bool)
	for _, v := range vars {
		names[v.Name] = true
	}
	assert.True(t, names["database"], "should extract 'database' key")
	assert.True(t, names["logging"], "should extract 'logging' key")
}

func TestYAMLExtractor_FileNode(t *testing.T) {
	src := []byte(`key: value
`)
	e := NewYAMLExtractor()
	result, err := e.Extract("test.yml", src)
	require.NoError(t, err)

	files := nodesOfKind(result.Nodes, graph.KindFile)
	assert.Equal(t, 1, len(files))
	assert.Equal(t, "test.yml", files[0].Name)
}
