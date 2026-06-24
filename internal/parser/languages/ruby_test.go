package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRbExtractor_Class(t *testing.T) {
	src := []byte(`class UserService
  def initialize(db)
    @db = db
  end

  def find_user(id)
    @db.query(id)
  end
end
`)
	e := NewRubyExtractor()
	result, err := e.Extract("service.rb", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "UserService", types[0].Name)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	assert.Len(t, methods, 2)

	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	assert.Len(t, memberEdges, 2)
}

func TestRbExtractor_TopLevelMethod(t *testing.T) {
	src := []byte(`def greet(name)
  puts "Hello #{name}"
end
`)
	e := NewRubyExtractor()
	result, err := e.Extract("app.rb", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	require.Len(t, funcs, 1)
	assert.Equal(t, "greet", funcs[0].Name)
}

func TestRbExtractor_Require(t *testing.T) {
	src := []byte(`require "json"
require_relative "helper"
`)
	e := NewRubyExtractor()
	result, err := e.Extract("app.rb", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	assert.GreaterOrEqual(t, len(imports), 1)
}

func TestRbExtractor_Module(t *testing.T) {
	src := []byte(`module Authentication
  def self.verify(token)
    true
  end
end
`)
	e := NewRubyExtractor()
	result, err := e.Extract("auth.rb", src)
	require.NoError(t, err)

	pkgs := nodesOfKind(result.Nodes, graph.KindPackage)
	require.Len(t, pkgs, 1)
	assert.Equal(t, "Authentication", pkgs[0].Name)
}

func TestRbExtractor_Constants(t *testing.T) {
	src := []byte(`MAX_RETRIES = 3
DEFAULT_HOST = "localhost"
`)
	e := NewRubyExtractor()
	result, err := e.Extract("config.rb", src)
	require.NoError(t, err)

	consts := nodesOfKind(result.Nodes, graph.KindConstant)
	require.Len(t, consts, 2)

	names := []string{consts[0].Name, consts[1].Name}
	assert.Contains(t, names, "MAX_RETRIES")
	assert.Contains(t, names, "DEFAULT_HOST")
}

func TestRbExtractor_CallSites(t *testing.T) {
	src := []byte(`class Greeter
  def greet(name)
    puts("Hello")
    logger.info("greeting")
  end
end
`)
	e := NewRubyExtractor()
	result, err := e.Extract("greeter.rb", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	assert.GreaterOrEqual(t, len(calls), 1)
}

func TestRbExtractor_NoTopLevelMethodInClass(t *testing.T) {
	// Methods inside a class should NOT appear as top-level functions.
	src := []byte(`class Foo
  def bar
  end
end

def baz
end
`)
	e := NewRubyExtractor()
	result, err := e.Extract("test.rb", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	require.Len(t, funcs, 1)
	assert.Equal(t, "baz", funcs[0].Name)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.Len(t, methods, 1)
	assert.Equal(t, "bar", methods[0].Name)
}

func TestRubyExtractor_RailsBeforeAction(t *testing.T) {
	// before_action :name binds a callback method to the class's
	// action methods. With only: filters, only the listed actions
	// fire the callback. This test covers the default (all actions
	// bound) plus an only: list.
	src := []byte(`
class UsersController
  before_action :authenticate
  before_action :load_user, only: [:show, :update]

  def index; end
  def show; @user end
  def update; @user end

  private

  def authenticate; end
  def load_user; end
end
`)
	e := NewRubyExtractor()
	result, err := e.Extract("users_controller.rb", src)
	require.NoError(t, err)

	var authEdges, loadEdges int
	actionsForAuth := map[string]bool{}
	actionsForLoad := map[string]bool{}
	for _, ed := range edgesOfKind(result.Edges, graph.EdgeCalls) {
		if ed.Meta == nil {
			continue
		}
		cb, _ := ed.Meta["rails_callback"].(string)
		if cb == "authenticate" {
			authEdges++
			actionsForAuth[ed.From] = true
		}
		if cb == "load_user" {
			loadEdges++
			actionsForLoad[ed.From] = true
		}
	}
	// authenticate guards every action (no only:/except:).
	assert.Equal(t, 3, authEdges, "authenticate should bind to every action")
	// load_user has only: [:show, :update].
	assert.Equal(t, 2, loadEdges, "load_user should bind only to :show, :update")
	assert.Contains(t, actionsForLoad, "users_controller.rb::UsersController.show")
	assert.Contains(t, actionsForLoad, "users_controller.rb::UsersController.update")
	assert.NotContains(t, actionsForLoad, "users_controller.rb::UsersController.index")
}

func TestRubyExtractor_SingletonMethod(t *testing.T) {
	// `def self.x` (singleton_method) must be extracted alongside
	// regular instance methods — previously the extractor missed them.
	src := []byte(`
class User
  def instance; 1 end
  def self.class_method; 2 end
end
`)
	e := NewRubyExtractor()
	result, err := e.Extract("user.rb", src)
	require.NoError(t, err)
	names := make(map[string]bool)
	for _, n := range result.Nodes {
		if n.Kind == graph.KindMethod {
			names[n.Name] = true
		}
	}
	assert.True(t, names["instance"])
	assert.True(t, names["class_method"])
}

func TestRubyExtractor_DocAndVisibility(t *testing.T) {
	src := []byte(`# Greeter handles welcomes.
class Greeter
  # Says hi to the user.
  def hello
  end

  def silent
  end
end
`)
	e := NewRubyExtractor()
	result, err := e.Extract("greeter.rb", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}

	greeter := byID["greeter.rb::Greeter"]
	require.NotNil(t, greeter)
	if greeter.Meta["visibility"] != "public" {
		t.Fatalf("Greeter.vis = %q", greeter.Meta["visibility"])
	}
	if greeter.Meta["doc"] != "Greeter handles welcomes." {
		t.Fatalf("Greeter.doc = %q", greeter.Meta["doc"])
	}

	hello := byID["greeter.rb::Greeter.hello"]
	require.NotNil(t, hello)
	if hello.Meta["doc"] != "Says hi to the user." {
		t.Fatalf("hello.doc = %q", hello.Meta["doc"])
	}
	if hello.Meta["visibility"] != "public" {
		t.Fatalf("hello.vis = %q", hello.Meta["visibility"])
	}
}

// TestRubyExtractor_MixinsBareCallsVisibility is the C5 test: include/extend/
// prepend produce module-composition edges, a bare receiver-less call becomes a
// call edge, and private/protected section markers (and the targeted form) set
// method visibility.
func TestRubyExtractor_MixinsBareCallsVisibility(t *testing.T) {
	src := []byte("class Foo\n" +
		"  include Comparable\n" +
		"  prepend Loggable\n" +
		"  def pub; helper; end\n" +
		"  private\n" +
		"  def sec; end\n" +
		"  public\n" +
		"  def pub2; end\n" +
		"  private :pub2\n" +
		"end\n")
	res, err := NewRubyExtractor().Extract("foo.rb", src)
	require.NoError(t, err)

	vis := map[string]string{}
	for _, n := range res.Nodes {
		if n.Kind == graph.KindMethod {
			v, _ := n.Meta["visibility"].(string)
			vis[n.Name] = v
		}
	}
	assert.Equal(t, "public", vis["pub"], "pub before any marker is public")
	assert.Equal(t, "private", vis["sec"], "sec after `private` is private")
	assert.Equal(t, "private", vis["pub2"], "pub2 flipped private by the targeted `private :pub2`")

	mixins := map[string]string{}
	var sawBareCall bool
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeExtends && e.From == "foo.rb::Foo" && e.Meta != nil {
			via, _ := e.Meta["via"].(string)
			mixins[e.To] = via
		}
		if e.Kind == graph.EdgeCalls && e.From == "foo.rb::Foo.pub" && e.To == "unresolved::helper" {
			sawBareCall = true
		}
	}
	assert.Equal(t, "include", mixins["unresolved::Comparable"], "include composes Comparable")
	assert.Equal(t, "prepend", mixins["unresolved::Loggable"], "prepend composes Loggable")
	assert.True(t, sawBareCall, "a bare `helper` call should produce a call edge")
}
