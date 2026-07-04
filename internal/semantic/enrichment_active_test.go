package semantic

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// EnrichmentActive must report true while any (repo, provider) pass is
// in the running state and false once every pass has settled — the
// signal the local-model load gate reads to defer a cold load.
func TestEnrichmentActive(t *testing.T) {
	mgr := NewManager(Config{Enabled: true}, zap.NewNop())

	// No passes recorded yet.
	assert.False(t, mgr.EnrichmentActive(), "an idle manager must report EnrichmentActive=false")

	// A running pass flips it true.
	mgr.setEnrichStatus("repo-a", "gopls", "go", EnrichStateRunning, 0, nil, "")
	assert.True(t, mgr.EnrichmentActive(), "a running pass must report EnrichmentActive=true")

	// A second, completed pass on another repo doesn't change the answer:
	// repo-a is still running.
	mgr.setEnrichStatus("repo-b", "gopls", "go", EnrichStateCompleted, 0, nil, "")
	assert.True(t, mgr.EnrichmentActive(), "one still-running pass keeps EnrichmentActive=true")

	// repo-a completing settles everything.
	mgr.setEnrichStatus("repo-a", "gopls", "go", EnrichStateCompleted, 0, nil, "")
	assert.False(t, mgr.EnrichmentActive(), "with no running pass EnrichmentActive must be false")

	// Terminal non-running states never read as active.
	for _, state := range []string{EnrichStatePartial, EnrichStateAbandoned, EnrichStateFailed} {
		mgr.setEnrichStatus("repo-a", "gopls", "go", state, 0, nil, "")
		assert.Falsef(t, mgr.EnrichmentActive(), "state %q must not read as active", state)
	}
}
