package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

func TestToolPolicy_Presets(t *testing.T) {
	cases := []struct {
		preset string
		tool   string
		want   bool
	}{
		// full → no restriction.
		{"full", "analyze", true},
		{"full", "edit_file", true},
		{"", "analyze", true},
		// core (the shipped default) → curated dev-cycle surface only.
		{"core", "smart_context", true},
		{"core", "edit_file", true},
		{"core", "analyze", true},
		{"core", "store_memory", true},
		{"core", "review", true},
		{"core", "get_architecture", false},
		{"core", "taint_paths", false},
		{"core", "overlay_push", false},
		// "default" / "classic" are aliases of core.
		{"default", "smart_context", true},
		{"default", "get_architecture", false},
		{"classic", "edit_file", true},
		// edit → only the headless editing surface.
		{"edit", "edit_file", true},
		{"edit", "find_files", true},
		{"edit", "search_symbols", true},
		{"edit", "analyze", false},
		{"edit", "get_communities", false},
		// nav → read-only navigation; no editors.
		{"nav", "search_symbols", true},
		{"nav", "find_files", true},
		{"nav", "edit_file", false},
		{"nav", "write_file", false},
		// readonly → everything except mutating tools.
		{"readonly", "search_symbols", true},
		{"readonly", "analyze", true},
		{"readonly", "edit_file", false},
		{"readonly", "write_file", false},
		{"readonly", "index_repository", false},
	}
	for _, c := range cases {
		p := newToolPolicy(ToolPolicyConfig{Preset: c.preset}, zap.NewNop())
		require.Equalf(t, c.want, p.allows(c.tool),
			"preset=%q tool=%q", c.preset, c.tool)
	}
}

func TestToolPolicy_AlwaysKeptAndDeltas(t *testing.T) {
	// tool_profile + tools_search survive any preset.
	edit := newToolPolicy(ToolPolicyConfig{Preset: "edit"}, zap.NewNop())
	require.True(t, edit.allows("tool_profile"))
	require.True(t, edit.allows(LazyToolsSearchName))

	// An explicit deny overrides the preset and the always-kept set.
	denied := newToolPolicy(ToolPolicyConfig{Preset: "edit", Deny: []string{"edit_file", "tool_profile"}}, zap.NewNop())
	require.False(t, denied.allows("edit_file"))
	require.False(t, denied.allows("tool_profile"))

	// An explicit allow adds a tool on top of the preset.
	plus := newToolPolicy(ToolPolicyConfig{Preset: "edit", Allow: []string{"analyze"}}, zap.NewNop())
	require.True(t, plus.allows("analyze"))

	// Deny-only against the full surface is active and subtractive.
	deny := newToolPolicy(ToolPolicyConfig{Deny: []string{"write_file"}}, zap.NewNop())
	require.True(t, deny.isActive())
	require.False(t, deny.allows("write_file"))
	require.True(t, deny.allows("search_symbols"))
}

func TestToolPolicy_ModesAndUnknown(t *testing.T) {
	require.True(t, newToolPolicy(ToolPolicyConfig{Preset: "edit"}, zap.NewNop()).hideMode())
	require.False(t, newToolPolicy(ToolPolicyConfig{Preset: "edit"}, zap.NewNop()).deferMode())
	require.True(t, newToolPolicy(ToolPolicyConfig{Preset: "edit", Mode: "defer"}, zap.NewNop()).deferMode())

	// full is inactive regardless of mode.
	require.False(t, newToolPolicy(ToolPolicyConfig{Preset: "full", Mode: "defer"}, zap.NewNop()).deferMode())

	// core aliases normalise to the canonical "core" label.
	require.Equal(t, "core", newToolPolicy(ToolPolicyConfig{Preset: "default"}, zap.NewNop()).preset)
	require.Equal(t, "core", newToolPolicy(ToolPolicyConfig{Preset: "classic"}, zap.NewNop()).preset)

	// Unknown preset fails open (full surface), never strands the agent.
	unknown := newToolPolicy(ToolPolicyConfig{Preset: "bogus"}, zap.NewNop())
	require.False(t, unknown.isActive())
	require.True(t, unknown.allows("anything"))
}

func TestParseToolSpec(t *testing.T) {
	// Preset + deltas.
	preset, allow, deny := ParseToolSpec("edit,+find_files,-write_file")
	require.Equal(t, "edit", preset)
	require.Equal(t, []string{"find_files"}, allow)
	require.Equal(t, []string{"write_file"}, deny)

	// A known preset followed by a bare tool name → preset + allow delta.
	preset, allow, _ = ParseToolSpec("edit,find_files")
	require.Equal(t, "edit", preset)
	require.Equal(t, []string{"find_files"}, allow)

	// No known preset → every bare token is an explicit tool name.
	preset, allow, _ = ParseToolSpec("search_symbols,edit_file,find_files")
	require.Equal(t, "", preset)
	require.Equal(t, []string{"search_symbols", "edit_file", "find_files"}, allow)
}

func TestToolPolicy_ExplicitList(t *testing.T) {
	// An explicit comma list (no preset) is exactly that surface.
	p := newToolPolicy(ToolPolicyConfig{Allow: []string{"search_symbols", "edit_file"}}, zap.NewNop())
	require.True(t, p.isActive())
	require.Equal(t, "custom", p.preset)
	require.True(t, p.allows("search_symbols"))
	require.True(t, p.allows("edit_file"))
	require.True(t, p.allows("tool_profile")) // always kept
	require.False(t, p.allows("analyze"))
	require.False(t, p.allows("write_file"))

	// End-to-end through the spec parser + env resolution.
	cfg := func(spec string) ToolPolicyConfig {
		pr, al, dn := ParseToolSpec(spec)
		return ToolPolicyConfig{Preset: pr, Allow: al, Deny: dn}
	}
	surface := NewToolSurface(cfg("find_files,get_symbol_source"), zap.NewNop())
	require.True(t, surface.Active())
	require.True(t, surface.Allows("find_files"))
	require.True(t, surface.Allows("get_symbol_source"))
	require.False(t, surface.Allows("edit_file"))
}

func TestToolPolicy_EnvOverride(t *testing.T) {
	t.Setenv(toolPresetEnv, "nav")
	t.Setenv(toolPresetModeEnv, "defer")
	// Base config says edit/hide; env must win.
	p := resolveToolPolicy(ToolPolicyConfig{Preset: "edit"}, zap.NewNop())
	require.Equal(t, "nav", p.preset)
	require.True(t, p.deferMode())
	require.True(t, p.allows("search_symbols"))
	require.False(t, p.allows("edit_file"))
}

// setupPresetServer builds a server over a one-file repo with the given
// tool policy so the registration + filtering paths can be exercised.
func setupPresetServer(t *testing.T, cfg ToolPolicyConfig) *Server {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package app\n\nfunc Main() {}\n"), 0o644))
	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	conf := config.Default()
	idx := indexer.New(g, reg, conf.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	eng := query.NewEngine(g)
	return NewServer(eng, g, idx, nil, zap.NewNop(), nil, MultiRepoOptions{ToolPolicy: &cfg})
}

func TestToolPolicy_HideModeServer(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "edit"})

	// The edit surface is visible; everything else is filtered out.
	require.True(t, srv.IsToolEnabled("edit_file"))
	require.True(t, srv.IsToolEnabled("find_files"))
	require.True(t, srv.IsToolEnabled("tool_profile"))
	require.False(t, srv.IsToolEnabled("analyze"))
	require.Equal(t, "blocked", srv.toolStatus("analyze"))

	live := srv.liveToolNames()
	require.Contains(t, live, "edit_file")
	require.NotContains(t, live, "analyze")

	// Calls to filtered tools are hard-blocked; allowed tools pass.
	require.NotNil(t, srv.checkToolPresetGate("analyze"))
	require.Nil(t, srv.checkToolPresetGate("edit_file"))
	require.Nil(t, srv.checkToolPresetGate("tool_profile"))
}

func TestToolPolicy_DeferModeServer(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "edit", Mode: "defer"})

	// Defer mode parks non-allowed tools behind tools_search instead of
	// removing them: allowed tools are live, the rest deferred.
	require.True(t, srv.lazy.Enabled())
	deferred := srv.lazy.DeferredNames()
	require.Contains(t, deferred, "analyze")
	require.NotContains(t, deferred, "edit_file")

	// A deferred tool is still "enabled" (reachable via tools_search),
	// never call-gated in defer mode.
	require.True(t, srv.IsToolEnabled("analyze"))
	require.Equal(t, "deferred", srv.toolStatus("analyze"))
	require.Nil(t, srv.checkToolPresetGate("analyze"))
}

// TestToolPolicy_CoreDefaultServer exercises the shipped default surface
// (config.Default(): core preset in defer mode) end to end: every core
// tool ships eagerly, specialised tools are deferred behind tools_search
// (never call-gated), and the cold surface stays far below the full
// catalogue.
func TestToolPolicy_CoreDefaultServer(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})

	require.True(t, srv.lazy.Enabled(), "core/defer must enable the lazy registry")
	require.True(t, srv.toolPolicy.deferMode())
	require.Equal(t, "core", srv.toolPolicy.preset)

	// Every curated core tool is eagerly live in the cold tools/list.
	for _, name := range corePresetTools {
		require.Equalf(t, "live", srv.toolStatus(name),
			"core tool %q must be eagerly live", name)
	}
	// Introspection + discovery survive every preset.
	require.Equal(t, "live", srv.toolStatus("tool_profile"))
	require.Equal(t, "live", srv.toolStatus(LazyToolsSearchName))

	// Specialised tools are deferred (reachable on demand), not blocked.
	for _, name := range []string{"get_architecture", "get_communities", "taint_paths", "flow_between"} {
		require.Equalf(t, "deferred", srv.toolStatus(name),
			"non-core tool %q must be deferred under core/defer", name)
		require.Nilf(t, srv.checkToolPresetGate(name),
			"defer mode must never call-gate %q", name)
		require.Truef(t, srv.IsToolEnabled(name),
			"deferred tool %q is still reachable via tools_search", name)
	}

	// The eager surface stays lean — a fraction of the full ~180 catalogue.
	live := srv.liveToolNames()
	t.Logf("core/defer cold surface = %d live tools", len(live))
	require.GreaterOrEqual(t, len(live), len(corePresetTools),
		"all core tools must be live")
	require.Lessf(t, len(live), 60,
		"core/defer cold surface should be lean, got %d", len(live))
}
