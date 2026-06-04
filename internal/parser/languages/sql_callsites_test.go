package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func sqlCallTargets(edges []*graph.Edge) []string {
	var out []string
	for _, e := range edges {
		if e.Kind == graph.EdgeCalls && e.Meta != nil {
			if v, _ := e.Meta["via"].(string); v == sqlCallsiteVia {
				out = append(out, e.To)
			}
		}
	}
	return out
}

func hasTarget(targets []string, want string) bool {
	for _, t := range targets {
		if t == want {
			return true
		}
	}
	return false
}

func TestTSExtract_SupabaseRPC(t *testing.T) {
	src := `export async function load() {
  const { data } = await supabase.rpc('get_user_stats', { uid: 1 });
  return data;
}`
	r, err := NewTypeScriptExtractor().Extract("load.ts", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !hasTarget(sqlCallTargets(r.Edges), "unresolved::sqlfn::get_user_stats") {
		t.Errorf("supabase.rpc call not captured: %v", sqlCallTargets(r.Edges))
	}
}

func TestPyExtract_SupabaseAndSQLAlchemy(t *testing.T) {
	src := `def report(db, supabase):
    supabase.rpc("get_user_stats", {"uid": 1})
    db.execute(select(func.calc_total(Order.id)))
`
	r, err := NewPythonExtractor().Extract("report.py", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	targets := sqlCallTargets(r.Edges)
	if !hasTarget(targets, "unresolved::sqlfn::get_user_stats") {
		t.Errorf("supabase.rpc not captured: %v", targets)
	}
	if !hasTarget(targets, "unresolved::sqlfn::calc_total") {
		t.Errorf("SQLAlchemy func.calc_total not captured: %v", targets)
	}
}
