package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTokenStats_RecordFansOutToParent pins the fix for graph_stats's
// always-zero token_savings counter: every per-session record() must
// aggregate into the parent (process-wide) tokenStats so the shared
// default surfaces daemon-wide live activity instead of always
// reporting zero. Without the fanout, a fresh session that asked
// graph_stats first saw token_savings.calls_counted == 0 even when
// the daemon had served thousands of source-fetching calls — the
// shared default never received observations because every real call
// carried a session ID and went to the per-session counter.
func TestTokenStats_RecordFansOutToParent(t *testing.T) {
	parent := &tokenStats{}
	child := &tokenStats{parent: parent}

	child.record(nil, "get_symbol_source", 100, 500)
	child.record(nil, "smart_context", 50, 200)

	require.Equal(t, int64(2), child.callCount, "child counter must record both calls")
	require.Equal(t, int64(150), child.tokensReturned, "child tokens_returned must accumulate")
	require.Equal(t, int64(550), child.tokensSaved, "child tokens_saved must accumulate (400+150)")

	require.Equal(t, int64(2), parent.callCount, "parent must aggregate child calls")
	require.Equal(t, int64(150), parent.tokensReturned, "parent tokens_returned must aggregate")
	require.Equal(t, int64(550), parent.tokensSaved, "parent tokens_saved must aggregate")
}

// TestTokenStats_RootHasNoParent confirms the recursion floor: the
// process-wide root tokenStats (constructed by NewServer) must not
// have a parent pointer, otherwise record() would loop or push into
// an unrelated counter.
func TestTokenStats_RootHasNoParent(t *testing.T) {
	root := &tokenStats{}
	root.record(nil, "get_symbol_source", 100, 500)
	require.Equal(t, int64(1), root.callCount)
	require.Nil(t, root.parent, "root counter must keep parent nil")
}
