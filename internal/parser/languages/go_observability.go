package languages

import (
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// goLogMethods is the set of method names that, when called as a
// selector (`slog.Info("…")`, `logger.Error("…")`), are treated as
// log emissions for the observability coverage domain. The list is
// intentionally cross-library — slog, zap, zerolog, logrus, log15
// and most internal wrappers all share the same naming, so the
// heuristic catches a wide range without per-provider plumbing.
//
// False positives are possible (a domain function named Info that
// takes a string), but in practice the cost is a few extraneous
// KindEvent nodes — the gate strips them when the user disables
// observability.
var goLogMethods = map[string]string{
	"Debug":   "log",
	"Info":    "log",
	"Warn":    "log",
	"Warning": "log",
	"Error":   "log",
	"Fatal":   "log",
	"Panic":   "log",
	"Trace":   "log",
}

// goObservabilityEvent is a deferred record of one log call. The
// event name is the call's first string-literal argument; calls
// whose first arg is not a literal (e.g. dynamic format strings)
// are simply not recorded — agents can grep for those if they need
// to.
type goObservabilityEvent struct {
	method string // matched log method
	name   string // event name, taken from the first string-literal arg
	line   int    // 1-based line of the call expression
}

// detectGoLogEvent inspects a callm.expr capture and, when it
// matches the log heuristic, returns the resolved event name and
// the log-method classification. ok=false when the call is not a
// log emission.
//
// The check is deliberately strict: method must be in goLogMethods,
// and the first argument must be a string literal. Variadic /
// formatted overloads (`slog.Info(ctx, msg, ...)`) still match as
// long as the *first* string-literal argument is found anywhere in
// the leading arg list — slog passes `msg` as the first string
// after a context, while package-level slog accepts msg directly.
func detectGoLogEvent(callExpr *sitter.Node, method string, src []byte) (string, bool) {
	if callExpr == nil {
		return "", false
	}
	if _, ok := goLogMethods[method]; !ok {
		return "", false
	}
	args := callExpr.ChildByFieldName("arguments")
	if args == nil {
		return "", false
	}
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		if c.Type() != "interpreted_string_literal" && c.Type() != "raw_string_literal" {
			continue
		}
		text := strings.Trim(c.Content(src), "\"`")
		if text == "" {
			return "", false
		}
		return text, true
	}
	return "", false
}

// emitGoObservabilityEvents emits one KindEvent per deferred event
// plus the EdgeEmits edges from the enclosing function to that
// event. Event nodes share IDs across files in a repo — the same
// "user.signup" event name produces a single node that all emitters
// link to, which is the intended graph topology for the
// observability domain.
//
// callerLookup maps a 1-based line to the enclosing function ID.
func emitGoObservabilityEvents(events []goObservabilityEvent, callerLookup func(line int) string, filePath string, result *parser.ExtractionResult) {
	if len(events) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(events))
	seenStringNodes := make(map[string]struct{}, len(events))
	seenStringEdges := make(map[string]struct{}, len(events))
	for _, e := range events {
		callerID := callerLookup(e.line)
		if callerID == "" {
			continue
		}
		eventID := goEventNodeID(e.name)
		if _, ok := seen[eventID]; !ok {
			seen[eventID] = struct{}{}
			result.Nodes = append(result.Nodes, &graph.Node{
				ID:       eventID,
				Kind:     graph.KindEvent,
				Name:     e.name,
				FilePath: filePath, // first sighting; not authoritative
				Language: "go",
				Meta: map[string]any{
					"event_kind": "log",
					"name":       e.name,
				},
			})
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From:     callerID,
			To:       eventID,
			Kind:     graph.EdgeEmits,
			FilePath: filePath,
			Line:     e.line,
			Origin:   graph.OriginASTInferred,
			Meta: map[string]any{
				"method": e.method,
			},
		})
		// Anchor a KindString context="log_message" registry node so
		// the log_events analyzer can aggregate emitters by raw
		// literal (richer than the canonical KindEvent ID).
		// Meta["level"] is the matched method's severity bucket
		// (every entry in goLogMethods today resolves to "log", but
		// the explicit field future-proofs a per-severity split —
		// Debug→debug, Error→error). Meta["event"] back-links to the
		// KindEvent ID so the registry view and the event view stay
		// round-trippable.
		strID := goStringNodeID(stringCtxLogMessage, e.name)
		if _, dup := seenStringNodes[strID]; !dup {
			seenStringNodes[strID] = struct{}{}
			result.Nodes = append(result.Nodes, &graph.Node{
				ID:       strID,
				Kind:     graph.KindString,
				Name:     e.name,
				FilePath: filePath,
				Language: "go",
				Meta: map[string]any{
					"context": string(stringCtxLogMessage),
					"value":   e.name,
					"level":   goLogMethods[e.method],
					"event":   eventID,
				},
			})
		}
		edgeKey := callerID + "->" + strID
		if _, dup := seenStringEdges[edgeKey]; !dup {
			seenStringEdges[edgeKey] = struct{}{}
			result.Edges = append(result.Edges, &graph.Edge{
				From:     callerID,
				To:       strID,
				Kind:     graph.EdgeEmits,
				FilePath: filePath,
				Line:     e.line,
				Origin:   graph.OriginASTInferred,
				Meta: map[string]any{
					"context": string(stringCtxLogMessage),
					"method":  e.method,
					"level":   goLogMethods[e.method],
				},
			})
		}
	}
}

// goEventNodeID is the canonical ID for a log/metric/trace event
// node. The "event::" prefix matches the synthetic-ID convention
// the exporter recognises (alongside `module::`, `external::`,
// `annotation::`, `unresolved::`), so cross-file events
// de-duplicate even without a real source-file backing.
func goEventNodeID(name string) string {
	return "event::log::" + name
}
