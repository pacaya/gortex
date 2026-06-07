package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
)

// TestReloadServers_BuildSwapTeardown drives the live-reload lifecycle:
// build a router when the first remote appears, swap it in place when the
// roster changes, and tear it down when the last remote is removed.
func TestReloadServers_BuildSwapTeardown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.toml")
	t.Setenv("GORTEX_DAEMON_SERVERS", path)

	var published []*daemon.Router // nil entries == teardown
	c := &realController{
		logger:       zap.NewNop(),
		localExecute: func(context.Context, string, []byte) ([]byte, int, error) { return []byte(`{}`), 200, nil },
	}
	c.publishRouter = func(r *daemon.Router) { published = append(published, r) }

	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reload := func() map[string]any {
		raw, err := c.ReloadServers(context.Background())
		if err != nil {
			t.Fatalf("ReloadServers: %v", err)
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return m
	}

	// 1. First remote added => router built + published.
	write("[[server]]\nslug = \"r2\"\nurl = \"https://r2:4747\"\n")
	m := reload()
	if m["router_wired"] != true || m["servers"].(float64) != 1 {
		t.Fatalf("first add should wire 1 server, got %v", m)
	}
	if c.liveRouter == nil {
		t.Fatal("liveRouter should be built")
	}
	if len(published) != 1 || published[0] == nil {
		t.Fatalf("a non-nil router should have been published, got %v", published)
	}

	// 2. Roster grows to 2 => in-place swap, no re-publish, new count visible.
	write("[[server]]\nslug = \"r2\"\nurl = \"https://r2:4747\"\n[[server]]\nslug = \"r3\"\nurl = \"https://r3:4747\"\n")
	m = reload()
	if m["servers"].(float64) != 2 {
		t.Fatalf("swap should reflect 2 servers, got %v", m)
	}
	if len(published) != 1 {
		t.Fatal("an in-place swap must not re-publish the router")
	}
	if got := len(c.liveRouter.CurrentConfig().Server); got != 2 {
		t.Fatalf("liveRouter should serve 2 servers after swap, got %d", got)
	}

	// 3. All remotes removed => router torn down (nil published).
	write("")
	m = reload()
	if m["router_wired"] != false || m["servers"].(float64) != 0 {
		t.Fatalf("teardown should report 0/false, got %v", m)
	}
	if c.liveRouter != nil {
		t.Fatal("liveRouter should be nil after teardown")
	}
	if len(published) != 2 || published[1] != nil {
		t.Fatalf("teardown should publish a nil router, got %v", published)
	}
}
