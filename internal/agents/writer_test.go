package agents

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteIfNotExistsCreatesAndSkips covers both the create and
// skip branches of the helper plus the DryRun prediction. Golden
// fixtures don't exercise DryRun, so we test it explicitly here.
func TestWriteIfNotExistsCreatesAndSkips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "file.txt")

	var buf bytes.Buffer

	// 1. First call creates the file.
	a, err := WriteIfNotExists(&buf, path, "hello", ApplyOpts{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.Action != ActionCreate {
		t.Fatalf("expected create, got %q", a.Action)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("content: got %q want %q", got, "hello")
	}

	// 2. Second call finds the file and skips.
	a, err = WriteIfNotExists(&buf, path, "different", ApplyOpts{})
	if err != nil {
		t.Fatalf("skip: %v", err)
	}
	if a.Action != ActionSkip {
		t.Fatalf("expected skip, got %q", a.Action)
	}
	// Content must be unchanged — skip is never overwrite.
	got, _ = os.ReadFile(path)
	if string(got) != "hello" {
		t.Fatalf("skip must not overwrite: got %q", got)
	}

	// 3. DryRun on a missing file reports would-create, doesn't write.
	missing := filepath.Join(dir, "new.txt")
	a, err = WriteIfNotExists(&buf, missing, "x", ApplyOpts{DryRun: true})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if a.Action != ActionWouldCreate {
		t.Fatalf("expected would-create, got %q", a.Action)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote file: %v", err)
	}
}

// TestMergeJSONCreatesMergesAndSkipsIdempotent covers the three
// transitions the MCP installer relies on: fresh file, merge into
// existing, and no-op on re-run. This is the behavioural contract
// golden tests will compare against byte-for-byte.
func TestMergeJSONCreatesMergesAndSkipsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")

	var buf bytes.Buffer

	addGortex := func(root map[string]any, existed bool) (bool, error) {
		return UpsertMCPServer(root, "gortex", DefaultGortexMCPEntry(), ApplyOpts{}), nil
	}

	// 1. Missing file -> create.
	a, err := MergeJSON(&buf, path, addGortex, ApplyOpts{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.Action != ActionCreate {
		t.Fatalf("expected create, got %q", a.Action)
	}

	// 2. Re-run -> skip (idempotent).
	a, err = MergeJSON(&buf, path, addGortex, ApplyOpts{})
	if err != nil {
		t.Fatalf("skip: %v", err)
	}
	if a.Action != ActionSkip {
		t.Fatalf("expected skip, got %q", a.Action)
	}

	// 3. Pre-populate with an unrelated MCP server, merge adds ours
	//    without clobbering theirs.
	existing := map[string]any{
		"mcpServers": map[string]any{
			"other": map[string]any{"command": "other"},
		},
	}
	data, _ := json.MarshalIndent(existing, "", "  ")
	fresh := filepath.Join(dir, "mcp-pre.json")
	if err := os.WriteFile(fresh, data, 0o644); err != nil {
		t.Fatal(err)
	}
	a, err = MergeJSON(&buf, fresh, addGortex, ApplyOpts{})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if a.Action != ActionMerge {
		t.Fatalf("expected merge, got %q", a.Action)
	}
	content, _ := os.ReadFile(fresh)
	var out map[string]any
	if err := json.Unmarshal(content, &out); err != nil {
		t.Fatal(err)
	}
	servers := out["mcpServers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Fatalf("merge clobbered existing 'other' server: %v", servers)
	}
	if _, ok := servers["gortex"]; !ok {
		t.Fatalf("merge didn't add 'gortex': %v", servers)
	}
}

// TestRegistryFilterValidatesNames ensures we hard-error on typos
// rather than silently dropping them — a key UX requirement from the
// init plan.
func TestRegistryFilterValidatesNames(t *testing.T) {
	r := NewRegistry()
	r.Register(stubAdapter{name: "alpha"})
	r.Register(stubAdapter{name: "beta"})

	if _, err := r.Filter("alpha,gamma", ""); err == nil {
		t.Fatal("expected error on unknown 'gamma', got nil")
	}
	got, err := r.Filter("alpha", "beta")
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(got) != 1 || got[0].Name() != "alpha" {
		t.Fatalf("filter returned %v, want [alpha]", names(got))
	}
	// auto + skip should yield everything minus the skipped.
	got, err = r.Filter("auto", "alpha")
	if err != nil {
		t.Fatalf("auto+skip: %v", err)
	}
	if len(got) != 1 || got[0].Name() != "beta" {
		t.Fatalf("auto+skip returned %v, want [beta]", names(got))
	}
}

type stubAdapter struct{ name string }

func (s stubAdapter) Name() string                              { return s.name }
func (s stubAdapter) DocsURL() string                           { return "" }
func (s stubAdapter) Detect(Env) (bool, error)                  { return true, nil }
func (s stubAdapter) Plan(Env) (*Plan, error)                   { return &Plan{}, nil }
func (s stubAdapter) Apply(Env, ApplyOpts) (*Result, error)     { return &Result{Name: s.name}, nil }

func names(as []Adapter) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Name())
	}
	return out
}
