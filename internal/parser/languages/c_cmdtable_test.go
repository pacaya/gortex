package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cValueRefCandidates returns the function-as-value candidates the C extractor
// captured for src — the placeholder edges (via=callback_candidate) awaiting the
// resolver gate — as a name → source-line map. It is the capture-side view a
// generated command table exercises before any resolution runs.
func cValueRefCandidates(t *testing.T, path, src string) map[string]int {
	t.Helper()
	r, err := NewCExtractor().Extract(path, []byte(src))
	require.NoError(t, err)
	out := map[string]int{}
	for _, e := range r.Edges {
		if e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != "callback_candidate" {
			continue
		}
		if name, _ := e.Meta["fn_value_name"].(string); name != "" {
			out[name] = e.Line
		}
	}
	return out
}

// TestCExtractor_CommandTableMacroCandidates pins the redis command-table shape:
// a `MAKE_CMD(..., handler, ...)` macro row in a generated `.def` fragment
// captures the handler as a function-value candidate, while the string summary,
// the ALL_CAPS flag macro, the short arity counts, and the MAKE_CMD callee
// itself do not become references.
func TestCExtractor_CommandTableMacroCandidates(t *testing.T) {
	src := "" +
		"struct redisCommand redisCommandTable[] = {\n" + // line 1
		"{MAKE_CMD(\"get\", \"Get the value\", -2, CMD_READONLY, getCommand, 1, 2)},\n" + // line 2
		"{MAKE_CMD(\"strlen\", \"String length\", 2, CMD_READONLY, strlenCommand, 1, 2)},\n" + // line 3
		"};\n"
	cands := cValueRefCandidates(t, "commands.def", src)

	assert.Equal(t, 2, cands["getCommand"], "handler in a macro arg list is captured")
	assert.Equal(t, 3, cands["strlenCommand"], "handler in a macro arg list is captured")

	assert.NotContains(t, cands, "CMD_READONLY", "an ALL_CAPS flag macro is not a function reference")
	assert.NotContains(t, cands, "MAKE_CMD", "the macro callee is a call, not a value reference")
	assert.NotContains(t, cands, "get", "a short string-literal table key is not an identifier reference")
	assert.NotContains(t, cands, "Get the value", "a string literal is never captured")
}

// TestCExtractor_DispatchTableInitListCandidates covers the classic positional
// initializer-list dispatch table `{ "name", fnPtr, arity }` — the handler in
// the second slot is captured as a function-value candidate.
func TestCExtractor_DispatchTableInitListCandidates(t *testing.T) {
	src := "" +
		"struct cmd table[] = {\n" + // line 1
		"{ \"ping\", pingCommand, 2 },\n" + // line 2
		"{ \"echo\", echoCommand, 2 },\n" + // line 3
		"};\n"
	cands := cValueRefCandidates(t, "table.c", src)

	assert.Equal(t, 2, cands["pingCommand"], "handler in an initializer-list slot is captured")
	assert.Equal(t, 3, cands["echoCommand"], "handler in an initializer-list slot is captured")
	assert.NotContains(t, cands, "ping", "a string key is not an identifier reference")
}

// TestCExtractor_CommandTableNoiseNotCaptured is the negative precision pin: the
// three noise classes the guard exists for — ALL_CAPS macros / enum constants,
// sub-4-character identifiers, and string literals — never become candidates,
// while the one mixed-case, long-enough handler in the same row does.
func TestCExtractor_CommandTableNoiseNotCaptured(t *testing.T) {
	src := "" +
		"struct redisCommand t[] = {\n" + // line 1
		"{MAKE_CMD(\"noise\", ARG_TYPE_KEY, CMD_WRITE, run, xy, realCommand)},\n" + // line 2
		"};\n"
	cands := cValueRefCandidates(t, "n.def", src)

	assert.Contains(t, cands, "realCommand", "the mixed-case, long-enough handler is captured")
	assert.NotContains(t, cands, "ARG_TYPE_KEY", "an ALL_CAPS enum constant is filtered")
	assert.NotContains(t, cands, "CMD_WRITE", "an ALL_CAPS flag macro is filtered")
	assert.NotContains(t, cands, "run", "a sub-4-character identifier is filtered")
	assert.NotContains(t, cands, "xy", "a two-character identifier is filtered")
}

// TestCExtractor_CommandTableErrorRecovery pins that a malformed generated
// fragment (a missing close paren plus a line of garbage — the shape a `.def`
// degrades to when its surrounding translation-unit context is absent) does not
// suppress extraction: the extractor recovers the handlers whose argument lists
// still parse rather than emitting nothing.
func TestCExtractor_CommandTableErrorRecovery(t *testing.T) {
	src := "" +
		"MAKE_CMD(\"get\", getCommand, 2)\n" + // line 1: clean
		"MAKE_CMD(\"strlen\", strlenCommand, 2\n" + // line 2: missing close paren
		"%%% garbage !!! {{{ nonsense\n" + // line 3: junk
		"MAKE_CMD(\"append\", appendCommand, 3)\n" // line 4

	r, err := NewCExtractor().Extract("broken.def", []byte(src))
	require.NoError(t, err, "extraction must not fail on an ERROR-recovered tree")

	cands := map[string]bool{}
	for _, e := range r.Edges {
		if e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != "callback_candidate" {
			continue
		}
		if name, _ := e.Meta["fn_value_name"].(string); name != "" {
			cands[name] = true
		}
	}
	assert.True(t, cands["getCommand"], "the cleanly-parsed row before the error survives")
	assert.True(t, cands["strlenCommand"], "the handler still in an arg list after the missing paren survives recovery")
}
