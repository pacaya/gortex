package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A selector call on a self-rooted field-access receiver
// (`self.config.line_term.as_byte()`) binds by walking the declared field
// types from the enclosing impl type down the chain: Searcher.config ->
// Config, Config.line_term -> LineTerminator, then LineTerminator.as_byte.
func TestRustScope_FieldReceiverWalk(t *testing.T) {
	g := buildRustGraph(t, map[string]string{
		"lib.rs": `
struct LineTerminator { b: u8 }
impl LineTerminator {
    fn as_byte(&self) -> u8 { self.b }
}

struct Config { line_term: LineTerminator }

struct Searcher { config: Config }
impl Searcher {
    fn run(&self) -> u8 {
        self.config.line_term.as_byte()
    }
}
`,
	})
	ResolveRustScopeCalls(g)
	targets := callTargetsFromRust(g, "lib.rs::Searcher.run")
	require.Contains(t, targets, "lib.rs::LineTerminator.as_byte")
}
