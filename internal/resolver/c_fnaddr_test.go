package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCFnAddressCrossFileComparison pins the classic function-pointer identity
// check: `c->cmd->proc != execCommand` in one translation unit references a
// function defined in another. C's flat extern namespace makes these bind
// repo-globally.
func TestCFnAddressCrossFileComparison(t *testing.T) {
	g := loadCSources(t, map[string]string{
		"cmd.h":  "typedef void (*cmdProc)(void);\n",
		"exec.c": "void execCommand(void) {}\n",
		"server.c": "" +
			"int isExec(cmdProc p) {\n" + // line 1
			"  return p != execCommand;\n" + // line 2
			"}\n",
	})
	ResolveFnValueCallbacks(g)
	e := refEdgeAt(g, "server.c::isExec", 2)
	require.NotNil(t, e, "cross-file function-address comparison must bind")
	assert.Equal(t, "exec.c::execCommand", e.To)
}

// TestCFnAddressAssignment covers a function-pointer assignment right-hand side.
func TestCFnAddressAssignment(t *testing.T) {
	g := loadCSources(t, map[string]string{
		"exec.c": "void execCommand(void) {}\n",
		"wire.c": "" +
			"void wire(cmdProc *slot) {\n" + // line 1
			"  *slot = execCommand;\n" + // line 2
			"}\n",
	})
	ResolveFnValueCallbacks(g)
	e := refEdgeAt(g, "wire.c::wire", 2)
	require.NotNil(t, e, "function-pointer assignment must bind")
	assert.Equal(t, "exec.c::execCommand", e.To)
}

// TestCFnAddressAmpersand covers the `&fn` address-of form and its tag.
func TestCFnAddressAmpersand(t *testing.T) {
	g := loadCSources(t, map[string]string{
		"h.c": "void handler(void) {}\n",
		"reg.c": "" +
			"void setup(void) {\n" + // line 1
			"  install(&handler);\n" + // line 2
			"}\n",
	})
	ResolveFnValueCallbacks(g)
	e := refEdgeAt(g, "reg.c::setup", 2)
	require.NotNil(t, e, "&handler must bind")
	assert.Equal(t, "h.c::handler", e.To)
	assert.Equal(t, "address_of", e.Meta["fn_ref_form"])
}

// TestCFnAddressStaticNotCrossFile pins the scope_static guard: a file-local
// static function is invisible to another translation unit, so a same-named
// cross-file reference must not bind it.
func TestCFnAddressStaticNotCrossFile(t *testing.T) {
	g := loadCSources(t, map[string]string{
		"a.c": "static void helper(void) {}\n",
		"b.c": "" +
			"void use(cmdProc *slot) {\n" + // line 1
			"  *slot = helper;\n" + // line 2
			"}\n",
	})
	ResolveFnValueCallbacks(g)
	assert.Nil(t, refEdgeAt(g, "b.c::use", 2),
		"a cross-file reference must not bind a file-local static function")
}

// TestCFnAddressSameFileStaticWins pins same-file precedence: when a static in
// the referencing file and an extern elsewhere share a name, the same-file
// definition is chosen.
func TestCFnAddressSameFileStaticWins(t *testing.T) {
	g := loadCSources(t, map[string]string{
		"a.c": "" +
			"static void dispatch(void) {}\n" + // line 1
			"void user(void) {\n" + // line 2
			"  register_cb(dispatch);\n" + // line 3
			"}\n",
		"c.c": "void dispatch(void) {}\n", // extern, different file
	})
	ResolveFnValueCallbacks(g)
	e := refEdgeAt(g, "a.c::user", 3)
	require.NotNil(t, e, "same-file dispatch must bind")
	assert.Equal(t, "a.c::dispatch", e.To, "same-file static wins over cross-file extern")
}
