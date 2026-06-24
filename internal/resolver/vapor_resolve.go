package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Vapor (server-side Swift) directory-convention name-resolution. A fallback
// for the references sourcekit-lsp leaves unresolved: a `*Controller` binds to
// /Controllers/, a `*Middleware` to /Middleware/. Fluent models are bare
// PascalCase (`final class User: Model`) and resolve to /Models/; that case is
// gated on the resolved definition actually living under a /Models/ directory
// so a built-in or unrelated same-named type is never mis-bound. The
// `*ViewController` shape is left to the UIKit pass.

var (
	vaporControllerDirs = []string{"/Controllers/", "/Controller/"}
	vaporMiddlewareDirs = []string{"/Middleware/", "/Middlewares/"}
	vaporModelDirs      = []string{"/Models/", "/Model/"}
)

// ResolveVaporRefs binds residual unresolved Vapor references to their
// directory-located definitions. Returns the count bound.
func ResolveVaporRefs(g graph.Store) int {
	if g == nil {
		return 0
	}
	resolved := 0
	var reindex []graph.EdgeReindex
	for _, kind := range []graph.EdgeKind{graph.EdgeInstantiates, graph.EdgeReferences, graph.EdgeTypedAs, graph.EdgeCalls} {
		for e := range g.EdgesByKind(kind) {
			if e == nil || !graph.IsUnresolvedTarget(e.To) {
				continue
			}
			name := graph.UnresolvedName(e.To)
			if name == "" || strings.ContainsRune(name, '.') {
				continue
			}
			dirs, modelOnly, ok := vaporDirsFor(name)
			if !ok {
				continue
			}
			fromFile := ""
			if n := g.GetNode(e.From); n != nil {
				fromFile = n.FilePath
			}
			if !strings.HasSuffix(fromFile, ".swift") {
				continue
			}
			targetID, conf := ResolveByConvention(g, name, "", dirs, fromFile)
			if targetID == "" {
				continue
			}
			if modelOnly {
				// A bare-PascalCase model binds only when its definition
				// actually lives under a /Models/ directory, so a built-in
				// or unrelated same-named type is never mis-bound.
				tn := g.GetNode(targetID)
				if tn == nil || !swiftUIPathHasDir(tn.FilePath, vaporModelDirs) {
					continue
				}
			}
			oldTo := e.To
			e.To = targetID
			e.Origin = graph.OriginASTInferred
			e.Confidence = conf
			e.ConfidenceLabel = graph.ConfidenceLabelFor(e.Kind, conf)
			StampSynthesized(e, SynthVaporResolve)
			reindex = append(reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
			resolved++
		}
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

// vaporDirsFor classifies a Vapor reference name into its convention dirs;
// modelOnly marks the bare-PascalCase Fluent-model fallback, which is
// additionally gated on the resolved definition living under a /Models/
// directory. A `*ViewController` is excluded — that is the UIKit pass's shape.
func vaporDirsFor(name string) (dirs []string, modelOnly bool, ok bool) {
	switch {
	case strings.HasSuffix(name, "Controller") && !strings.HasSuffix(name, "ViewController"):
		return vaporControllerDirs, false, true
	case strings.HasSuffix(name, "Middleware"):
		return vaporMiddlewareDirs, false, true
	}
	if c := name[0]; c >= 'A' && c <= 'Z' {
		return vaporModelDirs, true, true
	}
	return nil, false, false
}
