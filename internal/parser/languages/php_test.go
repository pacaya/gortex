package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestPHPExtractor_Class(t *testing.T) {
	src := []byte(`<?php
class UserService {
    private $db;

    public function __construct($db) {
        $this->db = $db;
    }

    public function findUser($id) {
        return $this->db->query($id);
    }
}
`)
	e := NewPHPExtractor()
	result, err := e.Extract("service.php", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	assert.GreaterOrEqual(t, len(types), 1)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	assert.GreaterOrEqual(t, len(methods), 1)
}

func TestPHPExtractor_Function(t *testing.T) {
	src := []byte(`<?php
function greet($name) {
    echo "Hello $name";
}
`)
	e := NewPHPExtractor()
	result, err := e.Extract("app.php", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	assert.GreaterOrEqual(t, len(funcs), 1)
}

func TestPHPExtractor_Interface(t *testing.T) {
	src := []byte(`<?php
interface Repository {
    public function findById($id);
    public function save($entity);
}
`)
	e := NewPHPExtractor()
	result, err := e.Extract("repo.php", src)
	require.NoError(t, err)

	ifaces := nodesOfKind(result.Nodes, graph.KindInterface)
	require.Len(t, ifaces, 1)
	assert.Equal(t, "Repository", ifaces[0].Name)
}

func TestPHPExtractor_MethodMemberOf(t *testing.T) {
	src := []byte(`<?php
class UserService {
    public function findUser($id) {
        return $this->db->query($id);
    }
}
`)
	e := NewPHPExtractor()
	result, err := e.Extract("service.php", src)
	require.NoError(t, err)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.GreaterOrEqual(t, len(methods), 1)

	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	require.GreaterOrEqual(t, len(memberEdges), 1)
	for _, e := range memberEdges {
		assert.Equal(t, "service.php::UserService", e.To)
	}
}

func TestPHPExtractor_Namespace(t *testing.T) {
	src := []byte(`<?php
namespace App\Models;

class User {}
`)
	e := NewPHPExtractor()
	result, err := e.Extract("user.php", src)
	require.NoError(t, err)

	pkgs := nodesOfKind(result.Nodes, graph.KindPackage)
	require.GreaterOrEqual(t, len(pkgs), 1)
}

func TestPHPExtractor_UseImport(t *testing.T) {
	src := []byte(`<?php
use App\Models\User;
use App\Services\UserService;

class Controller {}
`)
	e := NewPHPExtractor()
	result, err := e.Extract("controller.php", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	assert.GreaterOrEqual(t, len(imports), 2)
}

func TestPHPExtractor_CallSites(t *testing.T) {
	src := []byte(`<?php
class Service {
    public function run() {
        $this->helper();
        doSomething();
    }
}
`)
	e := NewPHPExtractor()
	result, err := e.Extract("svc.php", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	assert.GreaterOrEqual(t, len(calls), 1)
}
