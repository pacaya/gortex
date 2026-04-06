package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestProtobufExtractor_Message(t *testing.T) {
	src := []byte(`syntax = "proto3";

message User {
  string name = 1;
  string email = 2;
}
`)
	e := NewProtobufExtractor()
	result, err := e.Extract("user.proto", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "User", types[0].Name)

	vars := nodesOfKind(result.Nodes, graph.KindVariable)
	assert.Len(t, vars, 2)

	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	assert.Len(t, memberEdges, 2)
}

func TestProtobufExtractor_Service(t *testing.T) {
	src := []byte(`syntax = "proto3";

service UserService {
  rpc GetUser(GetUserRequest) returns (User);
  rpc CreateUser(CreateUserRequest) returns (User);
}
`)
	e := NewProtobufExtractor()
	result, err := e.Extract("service.proto", src)
	require.NoError(t, err)

	ifaces := nodesOfKind(result.Nodes, graph.KindInterface)
	require.Len(t, ifaces, 1)
	assert.Equal(t, "UserService", ifaces[0].Name)

	methods, ok := ifaces[0].Meta["methods"].([]string)
	require.True(t, ok)
	assert.Contains(t, methods, "GetUser")
	assert.Contains(t, methods, "CreateUser")

	methodNodes := nodesOfKind(result.Nodes, graph.KindMethod)
	assert.Len(t, methodNodes, 2)

	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	assert.Len(t, memberEdges, 2)
}

func TestProtobufExtractor_Import(t *testing.T) {
	src := []byte(`syntax = "proto3";
import "google/protobuf/timestamp.proto";
`)
	e := NewProtobufExtractor()
	result, err := e.Extract("app.proto", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	assert.Len(t, imports, 1)
}

func TestProtobufExtractor_Enum(t *testing.T) {
	src := []byte(`syntax = "proto3";

enum Status {
  UNKNOWN = 0;
  ACTIVE = 1;
}
`)
	e := NewProtobufExtractor()
	result, err := e.Extract("status.proto", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "Status", types[0].Name)
}
