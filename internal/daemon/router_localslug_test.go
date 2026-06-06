package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestRouter_DefaultRemoteIsProxiedNotLocal is the core localSlug
// foot-gun fix: a remote marked default=true must be proxied, never
// mistaken for the daemon's own graph. Local identity is the reserved
// sentinel, which no roster entry can carry.
func TestRouter_DefaultRemoteIsProxiedNotLocal(t *testing.T) {
	var localCalled atomic.Bool
	remoteHit := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case remoteHit <- struct{}{}:
		default:
		}
		_, _ = w.Write([]byte(`{"from":"remote"}`))
	}))
	defer srv.Close()

	cfg := &ServersConfig{Server: []ServerEntry{{Slug: "r2", URL: srv.URL, Default: true}}}
	router := NewRouter(RouterConfig{
		Servers:     cfg,
		Rosters:     NewWorkspaceRosterCache(time.Minute),
		CwdResolver: func(string) (string, bool) { return "", false },
		LocalSlug:   LocalServerSentinel,
		LocalExecute: func(context.Context, string, []byte) ([]byte, int, error) {
			localCalled.Store(true)
			return []byte(`{"from":"local"}`), http.StatusOK, nil
		},
	})

	out, status, err := router.RouteToolCall(context.Background(), "find_usages", []byte(`{}`), RouteContext{})
	if err != nil {
		t.Fatalf("RouteToolCall: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if string(out) != `{"from":"remote"}` {
		t.Fatalf("expected the remote result, got %s", out)
	}
	if localCalled.Load() {
		t.Fatal("a default=true remote must be proxied, never executed locally")
	}
	select {
	case <-remoteHit:
	default:
		t.Fatal("the remote endpoint was never hit")
	}
}

// TestRouter_LocalOnlyCallsLocal asserts a daemon with no roster
// (cfg==nil) always runs locally.
func TestRouter_LocalOnlyCallsLocal(t *testing.T) {
	var localCalled atomic.Bool
	router := NewRouter(RouterConfig{
		LocalSlug: LocalServerSentinel,
		LocalExecute: func(context.Context, string, []byte) ([]byte, int, error) {
			localCalled.Store(true)
			return []byte(`{"from":"local"}`), http.StatusOK, nil
		},
	})
	if _, _, err := router.RouteToolCall(context.Background(), "find_usages", []byte(`{}`), RouteContext{}); err != nil {
		t.Fatalf("RouteToolCall: %v", err)
	}
	if !localCalled.Load() {
		t.Fatal("a local-only daemon must call the local executor")
	}
}

// TestValidate_RejectsLocalSentinelSlug asserts a roster cannot claim
// the reserved local sentinel as a server slug.
func TestValidate_RejectsLocalSentinelSlug(t *testing.T) {
	cfg := &ServersConfig{Server: []ServerEntry{{Slug: LocalServerSentinel, URL: "https://x:4747"}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate must reject a [[server]] claiming the local sentinel slug")
	}
}

// TestScanWorkspaceField_RejectsSentinelAndPathChars asserts the
// workspace-resolver peek degrades a sentinel-colliding or
// path-containing workspace slug to "no workspace", closing the
// foot-gun from the workspace side.
func TestScanWorkspaceField_RejectsSentinelAndPathChars(t *testing.T) {
	cases := []struct{ in, want string }{
		{"workspace: @local\n", ""},
		{"workspace: a/b\n", ""},
		{"workspace: ~home\n", ""},
		{"workspace: tuck\n", "tuck"},
		{"workspace: \"quoted\"\n", "quoted"},
	}
	for _, c := range cases {
		if got := scanWorkspaceField([]byte(c.in)); got != c.want {
			t.Errorf("scanWorkspaceField(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
