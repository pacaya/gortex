package main

import (
	"fmt"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
)

// resolveFederationConfig loads the .gortex.yaml `federation:` block and
// maps it to the daemon's FederationConfig. Unset knobs stay zero so
// NewFederator's defaults apply. Best-effort: a config load error yields
// the all-default config.
func resolveFederationConfig() daemon.FederationConfig {
	cfg, err := config.Load(cfgFile)
	if err != nil || cfg == nil {
		return daemon.FederationConfig{}
	}
	f := cfg.Federation
	return daemon.FederationConfig{
		PerRemoteTimeout:  time.Duration(f.PerRemoteTimeoutMs) * time.Millisecond,
		Budget:            time.Duration(f.BudgetMs) * time.Millisecond,
		BreakerThreshold:  f.BreakerThreshold,
		BreakerCooldown:   time.Duration(f.BreakerCooldownMs) * time.Millisecond,
		NameKeyedFallback: f.NameKeyedFallback,
	}
}

// sessionRemoteOverrideSink bridges the MCP session proxy-toggle tools to
// the daemon's per-session overrides and the live router's roster. It is
// wired into the shared MCP server at daemon start; the router accessor
// is dynamic so it tracks a live ControlProxy swap.
type sessionRemoteOverrideSink struct {
	sessions *daemon.SessionRegistry
	router   func() *daemon.Router
}

func (s *sessionRemoteOverrideSink) currentRoster() *daemon.ServersConfig {
	if s.router == nil {
		return nil
	}
	r := s.router()
	if r == nil {
		return nil
	}
	return r.CurrentConfig()
}

func (s *sessionRemoteOverrideSink) validateSlug(slug string) error {
	cfg := s.currentRoster()
	if cfg == nil || len(cfg.Server) == 0 {
		return fmt.Errorf("no remotes are configured")
	}
	for _, srv := range cfg.Server {
		if srv.Slug == slug {
			return nil
		}
	}
	return fmt.Errorf("unknown remote slug %q", slug)
}

func (s *sessionRemoteOverrideSink) SetRemoteOverride(sessionID, slug string, enabled bool) error {
	if err := s.validateSlug(slug); err != nil {
		return err
	}
	sess := s.sessions.GetByID(sessionID)
	if sess == nil {
		return fmt.Errorf("no active session for this override")
	}
	sess.SetRemoteOverride(slug, enabled)
	return nil
}

func (s *sessionRemoteOverrideSink) ClearRemoteOverride(sessionID, slug string) error {
	sess := s.sessions.GetByID(sessionID)
	if sess == nil {
		return fmt.Errorf("no active session for this override")
	}
	sess.ClearRemoteOverride(slug)
	return nil
}

func (s *sessionRemoteOverrideSink) RemoteRosterStatus(sessionID string) ([]gortexmcp.RemoteRosterStatus, error) {
	cfg := s.currentRoster()
	if cfg == nil {
		return nil, nil
	}
	var ov map[string]bool
	if sess := s.sessions.GetByID(sessionID); sess != nil {
		ov = sess.RemoteOverrides()
	}
	out := make([]gortexmcp.RemoteRosterStatus, 0, len(cfg.Server))
	for _, srv := range cfg.Server {
		global := srv.IsEnabled()
		effective := global
		var override *bool
		if v, set := ov[srv.Slug]; set {
			vv := v
			override = &vv
			effective = v
		}
		out = append(out, gortexmcp.RemoteRosterStatus{
			Slug:            srv.Slug,
			GlobalEnabled:   global,
			SessionOverride: override,
			Effective:       effective,
		})
	}
	return out, nil
}
