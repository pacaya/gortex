package tstypes

import (
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// mixedFixture is one small caller/callee pair per supported language.
func mixedFixture() map[string]string {
	return map[string]string{
		"a/Svc.java": javaSvc,
		"b/App.java": `package b;

import a.Svc;

public class App {
    public void main() {
        Svc s = new Svc();
        s.run();
    }
}
`,
		"app/svc.py": pySvc,
		"app/main.py": `from app.svc import Svc


def main():
    s = Svc()
    s.run()
`,
		"src/svc.ts": tsSvc,
		"src/app.ts": `import { Svc } from "./svc";

export function main(): void {
  const s = new Svc();
  s.run();
}
`,
		"lib/svc.rb": rubySvc,
		"lib/app.rb": `class App
  def main
    s = Svc.new
    s.run
  end
end
`,
		"rs/engine.rs": rustSvc,
		"rs/app.rs": `use crate::engine::Svc;

pub fn main() {
    let s = Svc::new();
    s.run();
}
`,
		"A/Svc.cs": csSvc,
		"B/App.cs": `namespace B {
    public class App {
        public void Main() {
            var s = new Svc();
            s.Run();
        }
    }
}
`,
	}
}

// All in-process providers register on a plain manager and resolve the
// mixed fixture without any LSP router or external binary.
func TestManager_SupplementalProvidersEnrichWithoutLSP(t *testing.T) {
	g, dir := buildFixture(t, mixedFixture())
	mgr := semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop())
	defer mgr.Close()
	for _, p := range DefaultProviders(zap.NewNop()) {
		mgr.RegisterProvider(p)
	}
	if !mgr.HasProviders() {
		t.Fatal("manager reports no available providers")
	}

	results, _, err := mgr.EnrichAll(g, map[string]string{"": dir}, semantic.EnrichOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]*semantic.EnrichResult, len(results))
	for _, r := range results {
		got[r.Provider] = r
	}
	for _, want := range []string{"java-types", "python-types", "ruby-types", "rust-types", "typescript-types", "csharp-types"} {
		r, ok := got[want]
		if !ok {
			t.Errorf("no enrich result from %s", want)
			continue
		}
		if r.EdgesConfirmed+r.EdgesAdded == 0 {
			t.Errorf("%s did no edge work: %+v", want, r)
		}
	}

	// Spot-check one resolution per language family actually landed.
	checks := []struct {
		caller, callerKind, target string
	}{
		{"main", "java", "a/Svc.java::Svc.run"},
		{"main", "python", "app/svc.py::Svc.run"},
		{"main", "typescript", "src/svc.ts::Svc.run"},
		{"main", "ruby", "lib/svc.rb::Svc.run"},
		{"main", "rust", "rs/engine.rs::Svc.run"},
		{"Main", "csharp", "A/Svc.cs::Svc.Run"},
	}
	for _, c := range checks {
		var caller *graph.Node
		for _, n := range g.FindNodesByName(c.caller) {
			if n.Language == c.callerKind && (n.Kind == graph.KindFunction || n.Kind == graph.KindMethod) {
				caller = n
				break
			}
		}
		if caller == nil {
			t.Errorf("no %s caller named %s", c.callerKind, c.caller)
			continue
		}
		if callEdgeTo(g, caller.ID, c.target) == nil {
			t.Errorf("%s: call %s -> %s not resolved; edges: %v", c.callerKind, caller.ID, c.target, g.GetOutEdges(caller.ID))
		}
	}

	// Stats must report every in-process provider ready.
	ready := make(map[string]bool)
	for _, st := range mgr.Stats() {
		if st.Status == "ready" {
			ready[st.Name] = true
		}
	}
	for _, want := range []string{"java-types", "python-types", "ruby-types", "rust-types", "typescript-types", "csharp-types"} {
		if !ready[want] {
			t.Errorf("provider %s not reported ready in Stats", want)
		}
	}
}

// An `enabled: false` config entry switches one provider off while the
// others keep running.
func TestManager_ConfigDisablesOneProvider(t *testing.T) {
	g, dir := buildFixture(t, mixedFixture())
	cfg := semantic.Config{
		Enabled: true,
		Providers: []semantic.ProviderConfig{
			{Name: "java-types", Languages: []string{"java"}, Enabled: false},
		},
	}
	mgr := semantic.NewManager(cfg, zap.NewNop())
	defer mgr.Close()
	for _, p := range DefaultProviders(zap.NewNop()) {
		mgr.RegisterProvider(p)
	}
	results, _, err := mgr.EnrichAll(g, map[string]string{"": dir}, semantic.EnrichOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Provider == "java-types" {
			t.Fatalf("disabled provider still ran: %+v", r)
		}
	}
	assertUntouched(t, g, "b/App.java::App.main", "run", "java-types")
	if callEdgeTo(g, "app/main.py::main", "app/svc.py::Svc.run") == nil {
		t.Errorf("python provider should keep running when java is disabled")
	}
}

// The manager's incremental path runs supplemental providers for the
// file's language even when no arbitration winner exists.
func TestManager_EnrichFileRunsSupplemental(t *testing.T) {
	g, dir := buildFixture(t, mixedFixture())
	mgr := semantic.NewManager(semantic.Config{Enabled: true, EnrichOnWatch: true}, zap.NewNop())
	defer mgr.Close()
	for _, p := range DefaultProviders(zap.NewNop()) {
		mgr.RegisterProvider(p)
	}
	res, err := mgr.EnrichFile(g, dir, "b/App.java")
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("EnrichFile returned no result")
	}
	caller := "b/App.java::App.main"
	if callEdgeTo(g, caller, "a/Svc.java::Svc.run") == nil {
		t.Fatalf("incremental enrichment did not resolve the call; edges: %v", g.GetOutEdges(caller))
	}
}
