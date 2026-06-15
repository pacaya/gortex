package lsp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEffectiveInitializationOptions_JdtlsBuildGate(t *testing.T) {
	spec := &ServerSpec{Command: "jdtls", InitializationOptions: json.RawMessage(jdtlsSafeInitOptions)}

	// Default (gate unset): safe — no autobuild, no import.
	t.Setenv(JdtlsTrustBuildEnv, "")
	got := string(effectiveInitializationOptions(spec))
	assert.Contains(t, got, `"autobuild": {"enabled": false}`)
	assert.NotContains(t, got, `"autobuild": {"enabled": true}`)

	// Opted-in: build-backed options.
	t.Setenv(JdtlsTrustBuildEnv, "1")
	got = string(effectiveInitializationOptions(spec))
	assert.Contains(t, got, `"autobuild": {"enabled": true}`)
	assert.Contains(t, got, `"gradle": {"enabled": true}`)

	// Non-jdtls servers are never rewritten, even when the gate is on.
	other := &ServerSpec{Command: "gopls", InitializationOptions: json.RawMessage(`{"x":1}`)}
	assert.Equal(t, `{"x":1}`, string(effectiveInitializationOptions(other)))

	// The shipped jdtls spec is safe by default.
	for i := range Servers {
		if Servers[i].Command == "jdtls" {
			assert.Contains(t, string(Servers[i].InitializationOptions), `"autobuild": {"enabled": false}`,
				"shipped jdtls spec must be no-build by default")
		}
	}
}
