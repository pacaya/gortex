package lsp

import (
	"os"
	"strings"
)

// SweepEnv is the environment variable that overrides the configured
// per-file enrichment sweep mode. When set to a recognised value it wins
// over the provider's configured mode (and therefore over .gortex.yaml),
// so an operator can dial sweep intensity for one run without editing
// config.
const SweepEnv = "GORTEX_LSP_SWEEP"

// Per-file enrichment sweep modes. The sweep is the whole-repo hover /
// call-hierarchy phase that runs after the tier-deciding confirm and add
// passes: it stamps hover type strings and interrogates the server for
// call/type-hierarchy edges the AST extractor missed, file by file.
//
// On an already-resolved graph (a warm restart) that sweep is pure churn —
// it re-opens and re-hovers every file to confirm zero new edges. The mode
// gates how much of it runs:
//
//   - sweepModeDemand (DEFAULT): sweep a file when its declarations still
//     carry unresolved same-name call candidates (enrichment demand) OR it
//     declares a type / interface whose super/subtype hierarchy the sweep
//     interrogates (dispatch-relevant). The dispatch disjunct is load-bearing:
//     a type / interface never contributes call demand, yet the sweep is the
//     only path that recovers its cross-file / dynamic extends / supertype
//     edges, so gating on demand alone would silently drop them. A file with
//     neither signal is skipped, so a warm restart pays no sweep for it while
//     the already-enriched declarations that are swept skip their redundant
//     hover.
//   - sweepModeFull: sweep every file of the language — the pre-knob
//     behaviour, kept for a cold index that wants maximal hover coverage.
//   - sweepModeOff: skip the per-file sweep entirely. The confirm / add /
//     interface passes still run, so tiers and recall are unaffected.
const (
	sweepModeDemand = "demand"
	sweepModeFull   = "full"
	sweepModeOff    = "off"
)

// normalizeSweepMode canonicalises a configured / env sweep-mode string.
// Case- and whitespace-insensitive; "none" is accepted as an alias for
// "off". An empty or unrecognised value returns "" so the caller can fall
// through to the next precedence source (env → config → default).
func normalizeSweepMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case sweepModeDemand:
		return sweepModeDemand
	case sweepModeFull:
		return sweepModeFull
	case sweepModeOff, "none":
		return sweepModeOff
	default:
		return ""
	}
}

// resolveSweepMode picks the effective per-file sweep mode by precedence:
// the GORTEX_LSP_SWEEP env override wins over the configured value, which
// wins over the demand-gated default. An unrecognised value at any level
// is ignored (falls through) rather than failing the pass.
func resolveSweepMode(configured string) string {
	if env := normalizeSweepMode(os.Getenv(SweepEnv)); env != "" {
		return env
	}
	if cfg := normalizeSweepMode(configured); cfg != "" {
		return cfg
	}
	return sweepModeDemand
}

// effectiveSweepMode resolves the sweep mode for this provider, honouring
// the GORTEX_LSP_SWEEP env override over the router-configured field.
func (p *Provider) effectiveSweepMode() string {
	return resolveSweepMode(p.sweepMode)
}

// sweepFile reports whether the per-file hover / call-hierarchy sweep should
// run for a file under mode, given its unresolved-demand count and whether it
// carries a dispatch-relevant declaration. Under the demand default a file is
// swept when at least one of its declarations still has unresolved same-name
// call candidates (demand > 0) OR it declares a type / interface whose
// super/subtype hierarchy the sweep interrogates (dispatch) — the latter never
// surfaces as demand, so without this disjunct a type-only file would drop the
// extends / supertype edges only this sweep recovers. "full" always sweeps,
// "off" never does.
func sweepFile(mode string, demand int, dispatch bool) bool {
	switch mode {
	case sweepModeOff:
		return false
	case sweepModeFull:
		return true
	default: // sweepModeDemand and any unrecognised residue
		return demand > 0 || dispatch
	}
}
