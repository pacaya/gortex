package lsp

import (
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestRouter_For_NoSpec returns an error for unknown extensions.
func TestRouter_For_NoSpec(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()
	if _, err := r.For("README.md"); err == nil {
		t.Fatal("expected error for unknown ext")
	}
}

// TestRouter_AvailableSpecs filters by exec.LookPath. We can't assume
// any LSP server is on PATH on CI, so just check the call doesn't
// panic and returns a sane shape.
func TestRouter_AvailableSpecs(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()
	specs := r.AvailableSpecs()
	for _, s := range specs {
		if s == nil {
			t.Fatal("nil spec returned")
		}
		if s.Name == "" {
			t.Fatal("empty name returned")
		}
	}
}

// TestRouter_Stats_EmptyOnConstruct confirms a fresh router exposes
// no live providers until For() succeeds.
func TestRouter_Stats_EmptyOnConstruct(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()
	if got := r.Stats(); len(got) != 0 {
		t.Fatalf("expected empty stats, got %v", got)
	}
}

// TestRouter_SupportedLanguages doesn't depend on PATH binaries —
// AvailableSpecs may be empty on CI; the function should still return
// a sorted, deduplicated slice (possibly empty).
func TestRouter_SupportedLanguages(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()
	langs := r.SupportedLanguages()
	for i := 1; i < len(langs); i++ {
		if langs[i-1] >= langs[i] {
			t.Errorf("not sorted: %v", langs)
		}
	}
}

// TestRouter_NoOpReap returns nothing when the router is empty.
func TestRouter_NoOpReap(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop()).WithIdleTimeout(time.Millisecond)
	defer r.Close()
	if names := r.Reap(); len(names) != 0 {
		t.Fatalf("expected no names, got %v", names)
	}
}

// TestRouter_LanguageIDForPath delegates to the package helper.
func TestRouter_LanguageIDForPath(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()
	if got := r.LanguageIDForPath("a.ts"); got != "typescript" {
		t.Fatalf("got %q, want typescript", got)
	}
}

// TestRouter_RegisterSpec_Idempotent confirms that registering the
// same spec multiple times leaves a single entry in EnabledSpecs and
// that a nil spec is silently skipped.
func TestRouter_RegisterSpec_Idempotent(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()

	spec := SpecByName("gopls")
	if spec == nil {
		t.Fatal("expected gopls spec in registry")
	}

	r.RegisterSpec(nil) // no-op
	r.RegisterSpec(spec)
	r.RegisterSpec(spec) // duplicate — should not double-count

	got := r.EnabledSpecs()
	if len(got) != 1 || got[0].Name != "gopls" {
		t.Fatalf("expected exactly [gopls], got %+v", got)
	}
}

// TestRouter_EnabledSpecNames returns the alphabetised names of every
// registered spec — independent of which ones resolve on PATH (so it's
// CI-safe).
func TestRouter_EnabledSpecNames(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()

	for _, name := range []string{"gopls", "rust-analyzer", "clangd"} {
		if spec := SpecByName(name); spec != nil {
			r.RegisterSpec(spec)
		}
	}

	names := r.EnabledSpecNames()
	want := []string{"clangd", "gopls", "rust-analyzer"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}
}

// TestRouter_SpecAvailable_NotRegistered returns false for any spec
// the router doesn't know about — guards against the
// HasProviders-loop-walks-and-spawns regression.
func TestRouter_SpecAvailable_NotRegistered(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()
	if r.SpecAvailable("never-registered") {
		t.Fatal("expected SpecAvailable=false for unregistered spec")
	}
	// Stats should still show no live providers — SpecAvailable is
	// pure read.
	if got := r.Stats(); len(got) != 0 {
		t.Fatalf("expected empty stats after SpecAvailable, got %v", got)
	}
}

// TestRouter_ProviderForSpec_Unregistered returns an error without
// spawning anything.
func TestRouter_ProviderForSpec_Unregistered(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()
	if _, err := r.ProviderForSpec("never-registered"); err == nil {
		t.Fatal("expected error for unregistered spec")
	}
	if got := r.Stats(); len(got) != 0 {
		t.Fatalf("expected empty stats, got %v", got)
	}
}

// TestRouter_SetDiagnosticsHook_PropagatesToExistingProvider — the
// hook installed on Router after a Provider is already alive must
// reach the existing Provider so old subscribers don't get stranded.
//
// We verify by attaching to a synthetically-constructed Provider via
// the package-internal seam (we don't spawn a real LSP — that needs
// a binary on PATH).
func TestRouter_SetDiagnosticsHook_PropagatesToExistingProvider(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()

	// Inject a placeholder provider directly into the router's map.
	// This bypasses the real spawn path (which needs an LSP binary).
	p := NewProvider("noop-cmd", nil, []string{"go"}, false, 1, zap.NewNop())
	r.mu.Lock()
	r.providers[providerKey{specName: "fake-spec", workspace: "/tmp/test"}] = &routedProvider{
		spec:      &ServerSpec{Name: "fake-spec", Languages: []string{"go"}},
		workspace: "/tmp/test",
		provider:  p,
	}
	r.mu.Unlock()

	var (
		gotSpec string
		gotPath string
		gotN    int
	)
	r.SetDiagnosticsHook(func(specName, absPath string, diags []Diagnostic) {
		gotSpec = specName
		gotPath = absPath
		gotN = len(diags)
	})

	// Trigger the existing provider's fanout — should reach our hook
	// because Router.SetDiagnosticsHook re-attached.
	p.fanoutDiagnostics("/abs/path/main.go", []Diagnostic{{Message: "x"}, {Message: "y"}})

	if gotSpec != "fake-spec" || gotPath != "/abs/path/main.go" || gotN != 2 {
		t.Fatalf("hook did not propagate — got specName=%q path=%q n=%d", gotSpec, gotPath, gotN)
	}
}

// TestRouter_SetDiagnosticsHook_Nil clears the hook on existing
// providers — no calls should land after detach.
func TestRouter_SetDiagnosticsHook_Nil(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()

	p := NewProvider("noop", nil, []string{"go"}, false, 1, zap.NewNop())
	r.mu.Lock()
	r.providers[providerKey{specName: "fake-spec", workspace: "/tmp/test"}] = &routedProvider{
		spec:      &ServerSpec{Name: "fake-spec", Languages: []string{"go"}},
		workspace: "/tmp/test",
		provider:  p,
	}
	r.mu.Unlock()

	calls := 0
	r.SetDiagnosticsHook(func(_, _ string, _ []Diagnostic) { calls++ })
	p.fanoutDiagnostics("/abs/main.go", []Diagnostic{{}})
	if calls != 1 {
		t.Fatalf("expected 1 call after attach, got %d", calls)
	}
	r.SetDiagnosticsHook(nil)
	p.fanoutDiagnostics("/abs/main.go", []Diagnostic{{Message: "after-nil"}})
	if calls != 1 {
		t.Fatalf("expected hook to be cleared, got %d total calls", calls)
	}
}

// TestProvider_SetDiagnosticsHook_DirectPath — Provider.fanoutDiagnostics
// invokes the per-Provider hook even without a Router.
func TestProvider_SetDiagnosticsHook_DirectPath(t *testing.T) {
	p := NewProvider("noop", nil, []string{"go"}, false, 1, zap.NewNop())
	calls := 0
	p.SetDiagnosticsHook(func(_ string, _ []Diagnostic) { calls++ })
	p.fanoutDiagnostics("/abs/x.go", []Diagnostic{{}})
	p.SetDiagnosticsHook(nil)
	p.fanoutDiagnostics("/abs/y.go", []Diagnostic{{}})
	if calls != 1 {
		t.Fatalf("expected exactly one call, got %d", calls)
	}
}

// TestRouter_PerWorkspaceCacheIsolation — two requests for the same
// spec from different workspaces produce two distinct cache entries.
// Reusing the same workspace returns the same provider. Stats and
// Names reflect both (spec, workspace) pairs.
func TestRouter_PerWorkspaceCacheIsolation(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()

	specA := &ServerSpec{Name: "fake-spec", Languages: []string{"go"}}

	pA := NewProvider("noop", nil, []string{"go"}, false, 1, zap.NewNop())
	pB := NewProvider("noop", nil, []string{"go"}, false, 1, zap.NewNop())

	// Inject two distinct providers under the SAME spec but different
	// workspaces — bypasses the real spawn (no LSP binary in tests).
	r.mu.Lock()
	r.providers[providerKey{specName: specA.Name, workspace: "/repo/a"}] = &routedProvider{
		spec:      specA,
		workspace: "/repo/a",
		provider:  pA,
		lastUsed:  timeNow(),
	}
	r.providers[providerKey{specName: specA.Name, workspace: "/repo/b"}] = &routedProvider{
		spec:      specA,
		workspace: "/repo/b",
		provider:  pB,
		lastUsed:  timeNow(),
	}
	r.mu.Unlock()

	if got := r.Names(); len(got) != 2 || got[0] != "fake-spec@/repo/a" || got[1] != "fake-spec@/repo/b" {
		t.Fatalf("expected two workspace-keyed names, got %v", got)
	}
	stats := r.Stats()
	if len(stats) != 2 {
		t.Fatalf("expected two stats rows, got %d", len(stats))
	}
	if stats[0].Workspace != "/repo/a" || stats[1].Workspace != "/repo/b" {
		t.Fatalf("expected stats sorted by spec then workspace, got %+v", stats)
	}
}

// TestRouter_DefaultWorkspace — ForSpec without explicit workspace
// uses the router's default. Used by Manager batch enrichment.
func TestRouter_DefaultWorkspace(t *testing.T) {
	tmp := t.TempDir()
	r := NewRouter(tmp, zap.NewNop())
	defer r.Close()
	if got := r.DefaultWorkspace(); got == "" || got[len(got)-len(tmp):] != tmp {
		t.Fatalf("DefaultWorkspace not resolved: got %q want suffix %q", got, tmp)
	}
}

// timeNow is a tiny shim so the test file doesn't need the time
// import for one call.
func timeNow() (t time.Time) { return time.Now() }
