package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDefault_MCPToolsCoreDefer pins the shipped default tool surface:
// a curated dev-cycle "core" preset in defer mode, so a cold MCP session
// pays for the workhorse set instead of the full ~180-tool catalogue.
func TestDefault_MCPToolsCoreDefer(t *testing.T) {
	d := Default()
	require.Equal(t, "core", d.MCP.Tools.Preset)
	require.Equal(t, "defer", d.MCP.Tools.Mode)
}

// TestLoad_MCPToolsDefaultAndOverride proves the default survives a
// config file that omits mcp.tools, and that an explicit preset / mode
// overrides it (file > default).
func TestLoad_MCPToolsDefaultAndOverride(t *testing.T) {
	// A config file with no mcp.tools block keeps the core/defer default.
	bare := filepath.Join(t.TempDir(), ".gortex.yaml")
	require.NoError(t, os.WriteFile(bare, []byte("index:\n  workers: 2\n"), 0o644))
	cfg, err := Load(bare)
	require.NoError(t, err)
	require.Equal(t, "core", cfg.MCP.Tools.Preset, "omitted mcp.tools keeps the core default")
	require.Equal(t, "defer", cfg.MCP.Tools.Mode)

	// An explicit preset overrides the default — the documented opt-out
	// back to the full eager surface.
	full := filepath.Join(t.TempDir(), ".gortex.yaml")
	require.NoError(t, os.WriteFile(full, []byte("mcp:\n  tools:\n    preset: full\n    mode: hide\n"), 0o644))
	cfg2, err := Load(full)
	require.NoError(t, err)
	require.Equal(t, "full", cfg2.MCP.Tools.Preset)
	require.Equal(t, "hide", cfg2.MCP.Tools.Mode)
}
