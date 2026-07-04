package resolver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

// loadCSources extracts each C-family fixture with the C extractor and loads
// its nodes/edges into a fresh graph — the faithful extract→resolve harness for
// generated-table reference recovery.
func loadCSources(t *testing.T, files map[string]string) graph.Store {
	t.Helper()
	g := graph.New()
	c := languages.NewCExtractor()
	for path, src := range files {
		r, err := c.Extract(path, []byte(src))
		if err != nil {
			t.Fatalf("extract %s: %v", path, err)
		}
		for _, n := range r.Nodes {
			g.AddNode(n)
		}
		for _, e := range r.Edges {
			g.AddEdge(e)
		}
	}
	return g
}

func refEdgeAt(g graph.Store, from string, line int) *graph.Edge {
	for _, e := range g.GetOutEdges(from) {
		if e.Kind == graph.EdgeReferences && e.Line == line && e.Meta != nil {
			if v, _ := e.Meta["via"].(string); v == fnValueRegistrationVia {
				return e
			}
		}
	}
	return nil
}

// TestCCommandTableReferences pins the redis command-table shape: a generated
// `.def` fragment holding `MAKE_CMD(..., fooCommand, ...)` rows produces a usage
// edge from the fragment to the command function defined in another translation
// unit. Without it, find_usages(fooCommand) returns zero and mislabels the
// function as dead.
func TestCCommandTableReferences(t *testing.T) {
	g := loadCSources(t, map[string]string{
		"commands.def": "" +
			"struct redisCommand redisCommandTable[] = {\n" + // line 1
			"{MAKE_CMD(\"get\", getCommand, 2)},\n" + //         line 2
			"{MAKE_CMD(\"strlen\", strlenCommand, 2)},\n" + //   line 3
			"};\n",
		"t_string.c": "" +
			"robj *getCommand(client *c) { return lookupKey(c); }\n" +
			"void strlenCommand(client *c) { addReplyLongLong(c, 0); }\n",
	})

	require.Equal(t, "t_string.c::getCommand", resolveUniqueFnValue(g, "getCommand"),
		"the pointer-return command function must be a real node")

	ResolveFnValueCallbacks(g)

	get := refEdgeAt(g, "commands.def", 2)
	require.NotNil(t, get, "MAKE_CMD row must reference getCommand")
	assert.Equal(t, "t_string.c::getCommand", get.To)
	assert.Equal(t, "getCommand", get.Meta["fn_value_name"])

	strlen := refEdgeAt(g, "commands.def", 3)
	require.NotNil(t, strlen, "MAKE_CMD row must reference strlenCommand")
	assert.Equal(t, "t_string.c::strlenCommand", strlen.To)
}

// TestCCommandTableDesignatedInitializer covers the designated-initializer table
// form `{ .proc = fooCommand }`, not just the macro-call form.
func TestCCommandTableDesignatedInitializer(t *testing.T) {
	g := loadCSources(t, map[string]string{
		"table.c": "" +
			"struct cmd table[] = {\n" +
			"{ .name = \"ping\", .proc = pingCommand },\n" + // line 2
			"};\n",
		"server.c": "void pingCommand(client *c) { addReply(c); }\n",
	})

	ResolveFnValueCallbacks(g)

	e := refEdgeAt(g, "table.c", 2)
	require.NotNil(t, e, "designated .proc initializer must reference pingCommand")
	assert.Equal(t, "server.c::pingCommand", e.To)
}

// tableRefTo reports whether targetID has an incoming bound command-table
// reference edge originating in fromFile.
func tableRefTo(g graph.Store, targetID, fromFile string) *graph.Edge {
	for _, e := range g.GetInEdges(targetID) {
		if e.From != fromFile || e.Kind != graph.EdgeReferences || e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v == fnValueRegistrationVia {
			return e
		}
	}
	return nil
}

// TestCDispatchTableInitializerListPositional covers the positional
// initializer-list dispatch table `{ "name", fnPtr, arity }` — the second-slot
// handler resolves to its cross-file definition, exactly like the macro form.
func TestCDispatchTableInitializerListPositional(t *testing.T) {
	g := loadCSources(t, map[string]string{
		"table.c": "" +
			"struct cmd table[] = {\n" + // line 1
			"{ \"ping\", pingCommand, 2 },\n" + // line 2
			"{ \"echo\", echoCommand, 2 },\n" + // line 3
			"};\n",
		"server.c": "" +
			"void pingCommand(client *c) { addReply(c); }\n" +
			"void echoCommand(client *c) { addReply(c); }\n",
	})

	ResolveFnValueCallbacks(g)

	ping := refEdgeAt(g, "table.c", 2)
	require.NotNil(t, ping, "positional dispatch-table row must reference pingCommand")
	assert.Equal(t, "server.c::pingCommand", ping.To)

	echo := refEdgeAt(g, "table.c", 3)
	require.NotNil(t, echo, "positional dispatch-table row must reference echoCommand")
	assert.Equal(t, "server.c::echoCommand", echo.To)
}

// TestCCommandTableNoiseProducesNoEdge is the strong precision pin: even when a
// repo genuinely defines functions whose names are ALL_CAPS or shorter than four
// characters, a command-table row naming them must NOT mint a reference. The
// capture guard drops them before the gate, so the coincidental function
// definitions can never become false usages — only the real mixed-case handler
// binds.
func TestCCommandTableNoiseProducesNoEdge(t *testing.T) {
	g := loadCSources(t, map[string]string{
		"commands.def": "" +
			"struct redisCommand t[] = {\n" + // line 1
			"{MAKE_CMD(\"get\", CMD_READONLY, run, getCommand)},\n" + // line 2
			"};\n",
		"t_string.c": "" +
			"robj *getCommand(client *c) { return 0; }\n" +
			// Decoys: an ALL_CAPS function name and a sub-4-char function name.
			// Both are real, uniquely-named functions the gate WOULD bind if a
			// candidate reached it — proving the guard, not the gate, is what
			// suppresses them.
			"void CMD_READONLY(void) {}\n" +
			"void run(void) {}\n",
	})

	ResolveFnValueCallbacks(g)

	require.NotNil(t, tableRefTo(g, "t_string.c::getCommand", "commands.def"),
		"the real mixed-case handler must be referenced by the table row")
	assert.Nil(t, tableRefTo(g, "t_string.c::CMD_READONLY", "commands.def"),
		"an ALL_CAPS function name must not be referenced from a table row")
	assert.Nil(t, tableRefTo(g, "t_string.c::run", "commands.def"),
		"a sub-4-char function name must not be referenced from a table row")
}

// TestCCommandTableEndToEndIncomingNotStub is the end-to-end pin: index a
// two-file fixture (the handler defined in defs.c, the row in a generated
// table.def), resolve, and assert the handler's incoming edges include the table
// row carrying the correct file:line and pointing at the real, non-stub function
// node — the exact shape find_usages(handler) walks.
func TestCCommandTableEndToEndIncomingNotStub(t *testing.T) {
	g := loadCSources(t, map[string]string{
		"defs.c": "void handleGet(client *c) { addReply(c); }\n",
		"table.def": "" +
			"struct redisCommand redisCommandTable[] = {\n" + // line 1
			"{MAKE_CMD(\"get\", \"Get the value\", 2, CMD_READONLY, handleGet, 1, 1)},\n" + // line 2
			"};\n",
	})

	ResolveFnValueCallbacks(g)

	const target = "defs.c::handleGet"
	node := g.GetNode(target)
	require.NotNil(t, node, "the handler must be a real node")
	assert.Equal(t, graph.KindFunction, node.Kind, "the handler is a function")
	assert.False(t, node.Stub, "the handler is real source, not a federation proxy")
	assert.False(t, graph.IsStub(target), "the handler id is not a stub id")

	ref := tableRefTo(g, target, "table.def")
	require.NotNil(t, ref, "the handler's incoming edges must include the .def table row")
	assert.Equal(t, "table.def", ref.FilePath, "the reference carries the .def file")
	assert.Equal(t, 2, ref.Line, "the reference carries the exact table-row line")
	assert.Equal(t, target, ref.To)
	assert.False(t, graph.IsUnresolvedTarget(ref.To), "the bound edge no longer points at an unresolved placeholder")
}

// TestCCommandTablePrototypeNotAmbiguous pins the shape that silenced every
// real-world command table: a C codebase declares each handler in a shared
// header (`void strlenCommand(client *c);`) AND defines it in its own
// translation unit, so the handler's name matches two KindFunction nodes. A
// forward declaration names the same extern symbol as the definition — it must
// not make the name ambiguous, and the reference must bind to the definition,
// not the header line.
func TestCCommandTablePrototypeNotAmbiguous(t *testing.T) {
	g := loadCSources(t, map[string]string{
		"commands.def": "" +
			"struct redisCommand t[] = {\n" + // line 1
			"{MAKE_CMD(\"strlen\", CMD_READONLY, strlenCommand)},\n" + // line 2
			"};\n",
		"server.h":   "void strlenCommand(client *c);\n",
		"t_string.c": "void strlenCommand(client *c) { addReplyLongLong(c, 0); }\n",
	})

	ResolveFnValueCallbacks(g)

	e := refEdgeAt(g, "commands.def", 2)
	require.NotNil(t, e, "a header prototype must not make the handler ambiguous")
	assert.Equal(t, "t_string.c::strlenCommand", e.To, "the definition wins over the prototype")
	assert.Nil(t, tableRefTo(g, "server.h::strlenCommand", "commands.def"),
		"the header declaration line is not the reference target")
}

// TestCCommandTablePrototypeOnlyBinds covers a handler whose definition is not
// indexed (another repo / excluded path) but whose header declaration is: the
// unique prototype is still a legitimate binding target.
func TestCCommandTablePrototypeOnlyBinds(t *testing.T) {
	g := loadCSources(t, map[string]string{
		"commands.def": "" +
			"struct redisCommand t[] = {\n" +
			"{MAKE_CMD(\"exec\", CMD_NOSCRIPT, execCommand)},\n" + // line 2
			"};\n",
		"server.h": "void execCommand(client *c);\n",
	})

	ResolveFnValueCallbacks(g)

	e := refEdgeAt(g, "commands.def", 2)
	require.NotNil(t, e, "a unique prototype binds when no definition is indexed")
	assert.Equal(t, "server.h::execCommand", e.To)
}

// TestCCommandTableTwoPrototypesStillAmbiguous keeps the conservative floor:
// with no definition and two same-named declarations in different headers,
// the candidate is dropped rather than guessed.
func TestCCommandTableTwoPrototypesStillAmbiguous(t *testing.T) {
	g := loadCSources(t, map[string]string{
		"commands.def": "" +
			"struct redisCommand t[] = {\n" +
			"{MAKE_CMD(\"exec\", CMD_NOSCRIPT, execCommand)},\n" +
			"};\n",
		"server.h":  "void execCommand(client *c);\n",
		"cluster.h": "void execCommand(client *c);\n",
	})

	assert.Equal(t, 0, ResolveFnValueCallbacks(g),
		"two prototypes with no definition stay ambiguous — dropped")
}

// TestCCommandTableRealFileSlice is the regression pin against the real
// generated-table shape: the fixture under testdata/redis_cmdtable is a
// verbatim slice of redis's generated src/commands.def (the file prelude —
// including a #ifdef inside an initializer list and #define/keySpec blocks —
// plus the contiguous string/transactions rows around the strlen entry and the
// table terminator), a verbatim run of the server.h handler declarations, and
// the verbatim strlenCommand definition from t_string.c. Hand-written
// idealizations of this file previously passed while the real shape produced
// zero edges (the header prototype made every handler name ambiguous), so this
// test reads the real bytes.
func TestCCommandTableRealFileSlice(t *testing.T) {
	load := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata", "redis_cmdtable", name))
		require.NoError(t, err)
		return b
	}
	defSrc := load("commands.def")
	g := loadCSources(t, map[string]string{
		"src/commands.def": string(defSrc),
		"src/server.h":     string(load("server.h")),
		"src/t_string.c":   string(load("t_string.c")),
	})

	// Locate the strlen row in the fixture by content, so the assertion tracks
	// the verbatim slice rather than a hardcoded offset.
	rowLine := 0
	for i, line := range strings.Split(string(defSrc), "\n") {
		if strings.HasPrefix(line, "{MAKE_CMD(\"strlen\"") {
			rowLine = i + 1
			break
		}
	}
	require.NotZero(t, rowLine, "fixture must contain the verbatim strlen row")

	ResolveFnValueCallbacks(g)

	const target = "src/t_string.c::strlenCommand"
	node := g.GetNode(target)
	require.NotNil(t, node, "the real definition slice must produce the handler node")
	assert.False(t, node.Stub)

	ref := tableRefTo(g, target, "src/commands.def")
	require.NotNil(t, ref, "the real table row must reference the handler definition")
	assert.Equal(t, rowLine, ref.Line, "the reference carries the verbatim row's line")
	assert.Equal(t, "src/commands.def", ref.FilePath)

	// The prototype-only handler in the same rows (execCommand is declared in
	// the server.h slice but its multi.c definition is not part of the
	// fixture) binds to the unique declaration instead of being dropped.
	assert.NotNil(t, tableRefTo(g, "src/server.h::execCommand", "src/commands.def"),
		"a prototype-only handler still gets its table-row reference")
}
