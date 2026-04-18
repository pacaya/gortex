package mcp

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/wire"
)

// isGCX reports whether the caller requested the GCX1 compact wire
// format. The selection order mirrors ParseFormat: the `format` arg
// wins, otherwise legacy `compact: true` falls through to text (not
// GCX), and the absence of either keeps JSON as the default.
func isGCX(req mcp.CallToolRequest) bool {
	if v, ok := req.GetArguments()["format"].(string); ok {
		return wire.ParseFormat(v) == wire.FormatGCX
	}
	return false
}

// gcxResponse wraps a GCX byte payload into an MCP text-result. If the
// encoder returned an error, the caller gets a structured MCP error
// result instead of a half-written payload.
func gcxResponse(payload []byte, err error) (*mcp.CallToolResult, error) {
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("wire encode failed: %v", err)), nil
	}
	return mcp.NewToolResultText(string(payload)), nil
}

// newGCX creates an encoder writing to w with the given tool + fields.
// Encoders own their header so section layout stays visible at the
// call site.
func newGCX(w *bytes.Buffer, tool string, fields []string, metaKV ...string) *wire.Encoder {
	meta := map[string]string{}
	for i := 0; i+1 < len(metaKV); i += 2 {
		meta[metaKV[i]] = metaKV[i+1]
	}
	return wire.NewEncoder(w, wire.Header{
		Tool:   tool,
		Fields: fields,
		Meta:   meta,
	})
}

// nodeShort returns the short form of a node ID — whatever follows
// the last "::" separator. For methods this carries the receiver; for
// functions / types it is the plain name.
func nodeShort(n *graph.Node) string {
	if n == nil {
		return ""
	}
	if idx := strings.LastIndex(n.ID, "::"); idx >= 0 {
		return n.ID[idx+2:]
	}
	return n.Name
}

// nodeSig returns the rendered signature string for a node, falling
// back to "" when no signature was extracted.
func nodeSig(n *graph.Node) string {
	if n == nil || n.Meta == nil {
		return ""
	}
	if s, ok := n.Meta["signature"].(string); ok {
		return s
	}
	return ""
}

// shouldSkipGraphNode filters File and Import pseudo-nodes the way the
// legacy compact / TOON formatters do — they add noise without
// informational value in symbol-oriented outputs.
func shouldSkipGraphNode(n *graph.Node) bool {
	if n == nil {
		return true
	}
	return n.Kind == graph.KindFile || n.Kind == graph.KindImport
}

// --------------------------------------------------------------------
// Hand-tuned encoders for the top-10 hot-path tools.
// --------------------------------------------------------------------

// encodeWinnowSymbols emits one row per ranked hit with per-axis score
// contributions. The contributions column is a pipe-separated list of
// `axis=value` pairs so decoders can recover the attribution without a
// nested structure.
func encodeWinnowSymbols(rows []winnowResult, total, limit int) ([]byte, error) {
	truncated := total > limit
	var buf bytes.Buffer
	enc := newGCX(&buf, "winnow_symbols",
		[]string{"id", "kind", "name", "path", "line", "sig", "score", "fan_in", "fan_out", "churn", "community", "contributions"},
		"total", fmt.Sprintf("%d", total),
		"truncated", boolString(truncated),
		"weights", formatAxisWeights(winnowAxisWeights),
	)
	if err := enc.WriteComment(fmt.Sprintf("%d result(s)", len(rows))); err != nil {
		return nil, err
	}
	for _, r := range rows {
		if shouldSkipGraphNode(r.Node) {
			continue
		}
		if err := enc.WriteRow(
			r.Node.ID,
			string(r.Node.Kind),
			nodeShort(r.Node),
			r.Node.FilePath,
			r.Node.StartLine,
			nodeSig(r.Node),
			roundFloat(r.Score),
			r.FanIn,
			r.FanOut,
			r.Churn,
			r.Community,
			formatContributions(r.Contributions),
		); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// formatContributions renders the per-axis attribution map as a stable
// pipe-separated key=value list (sorted by key for determinism).
func formatContributions(m map[string]float64) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('|')
		}
		fmt.Fprintf(&b, "%s=%.3f", k, m[k])
	}
	return b.String()
}

// formatAxisWeights mirrors formatContributions for the meta header.
func formatAxisWeights(m map[string]float64) string {
	return formatContributions(m)
}

// encodeSearchSymbols emits one row per search hit with the minimum
// fields an agent needs to decide whether to fetch more detail.
func encodeSearchSymbols(nodes []*graph.Node, total, limit int) ([]byte, error) {
	truncated := total > limit
	if len(nodes) > limit {
		nodes = nodes[:limit]
	}
	var buf bytes.Buffer
	enc := newGCX(&buf, "search_symbols",
		[]string{"id", "kind", "name", "path", "line", "sig"},
		"total", fmt.Sprintf("%d", total),
		"truncated", boolString(truncated),
	)
	if err := enc.WriteComment(fmt.Sprintf("%d result(s)", len(nodes))); err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if shouldSkipGraphNode(n) {
			continue
		}
		if err := enc.WriteRow(
			n.ID,
			string(n.Kind),
			nodeShort(n),
			n.FilePath,
			n.StartLine,
			nodeSig(n),
		); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// encodeGetSymbolSource emits a single row carrying the node
// metadata plus the full source. Source is escaped line-by-line.
func encodeGetSymbolSource(node *graph.Node, source string, fromLine int, etag string) ([]byte, error) {
	var buf bytes.Buffer
	enc := newGCX(&buf, "get_symbol_source",
		[]string{"id", "kind", "name", "path", "start_line", "end_line", "from_line", "sig", "etag", "source"},
		"etag", etag,
	)
	if err := enc.WriteRow(
		node.ID,
		string(node.Kind),
		node.Name,
		node.FilePath,
		node.StartLine,
		node.EndLine,
		fromLine,
		nodeSig(node),
		etag,
		source,
	); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

// encodeBatchSymbols emits one row per requested symbol; missing
// symbols carry an error cell instead of a real node.
func encodeBatchSymbols(rows []map[string]any, includeSource bool) ([]byte, error) {
	fields := []string{"id", "kind", "name", "path", "start_line", "end_line", "sig"}
	if includeSource {
		fields = append(fields, "source")
	}
	fields = append(fields, "error")
	var buf bytes.Buffer
	enc := newGCX(&buf, "batch_symbols", fields,
		"count", fmt.Sprintf("%d", len(rows)),
	)
	for _, r := range rows {
		values := []any{
			str(r["id"]),
			str(r["kind"]),
			str(r["name"]),
			str(r["file_path"]),
			r["start_line"],
			r["end_line"],
			str(r["signature"]),
		}
		if includeSource {
			values = append(values, str(r["source"]))
		}
		values = append(values, str(r["error"]))
		if err := enc.WriteRow(values...); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// encodeFindUsages emits one row per usage edge. Each row names the
// caller symbol, its location, the edge kind, and the origin tier so
// agents can filter without a second call.
func encodeFindUsages(sg *query.SubGraph) ([]byte, error) {
	var buf bytes.Buffer
	enc := newGCX(&buf, "find_usages",
		[]string{"from", "to", "edge_kind", "origin", "confidence", "from_name", "from_path", "from_line"},
		"edges", fmt.Sprintf("%d", len(sg.Edges)),
	)
	nodeIdx := indexNodes(sg.Nodes)
	for _, e := range sg.Edges {
		fn := nodeIdx[e.From]
		var fname, fpath string
		var fline int
		if fn != nil {
			fname = nodeShort(fn)
			fpath = fn.FilePath
			fline = fn.StartLine
		}
		if err := enc.WriteRow(
			e.From, e.To, string(e.Kind), e.Origin, e.Confidence,
			fname, fpath, fline,
		); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// encodeSubGraph is a shared encoder for the edge-returning traversal
// tools (get_callers, get_call_chain, get_dependencies, get_dependents,
// find_implementations). Emits two sections: nodes then edges.
func encodeSubGraph(tool string, sg *query.SubGraph) ([]byte, error) {
	var buf bytes.Buffer
	nodes := make([]*graph.Node, 0, len(sg.Nodes))
	for _, n := range sg.Nodes {
		if shouldSkipGraphNode(n) {
			continue
		}
		nodes = append(nodes, n)
	}
	nodeEnc := newGCX(&buf, tool+".nodes",
		[]string{"id", "kind", "name", "path", "line"},
		"total", fmt.Sprintf("%d", sg.TotalNodes),
		"truncated", boolString(sg.Truncated),
	)
	for _, n := range nodes {
		if err := nodeEnc.WriteRow(n.ID, string(n.Kind), nodeShort(n), n.FilePath, n.StartLine); err != nil {
			return nil, err
		}
	}
	if err := nodeEnc.Close(); err != nil {
		return nil, err
	}
	edgeEnc := newGCX(&buf, tool+".edges",
		[]string{"from", "to", "kind", "origin", "confidence", "label"},
		"count", fmt.Sprintf("%d", len(sg.Edges)),
	)
	for _, e := range sg.Edges {
		label := e.ConfidenceLabel
		if label == "" {
			label = graph.ConfidenceLabelFor(e.Kind, e.Confidence)
		}
		if err := edgeEnc.WriteRow(e.From, e.To, string(e.Kind), e.Origin, e.Confidence, label); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), edgeEnc.Close()
}

// encodeFileSummary emits one row per symbol in a file plus a trailing
// edge-distribution comment.
func encodeFileSummary(sg *query.SubGraph, etag string) ([]byte, error) {
	var buf bytes.Buffer
	enc := newGCX(&buf, "get_file_summary",
		[]string{"id", "kind", "name", "line", "sig"},
		"total_nodes", fmt.Sprintf("%d", sg.TotalNodes),
		"total_edges", fmt.Sprintf("%d", len(sg.Edges)),
		"truncated", boolString(sg.Truncated),
		"etag", etag,
	)
	for _, n := range sg.Nodes {
		if shouldSkipGraphNode(n) {
			continue
		}
		if err := enc.WriteRow(n.ID, string(n.Kind), nodeShort(n), n.StartLine, nodeSig(n)); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// encodeAnalyze routes to per-kind encoders. The `kind` field is
// placed in the meta header rather than per-row so consumers can
// dispatch without inspecting every row.
func encodeAnalyze(kind string, payload any) ([]byte, error) {
	var buf bytes.Buffer
	switch kind {
	case "dead_code":
		items, _ := payload.([]deadCodeItem)
		enc := newGCX(&buf, "analyze.dead_code",
			[]string{"id", "kind", "name", "path", "line", "reason"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.ID, it.Kind, it.Name, it.Path, it.Line, it.Reason); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "hotspots":
		items, _ := payload.([]hotspotItem)
		enc := newGCX(&buf, "analyze.hotspots",
			[]string{"id", "name", "path", "line", "fan_in", "fan_out", "cross_cut", "score"},
			"count", fmt.Sprintf("%d", len(items)),
		)
		for _, it := range items {
			if err := enc.WriteRow(it.ID, it.Name, it.Path, it.Line, it.FanIn, it.FanOut, it.CrossCommunity, it.Score); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	case "cycles":
		cycles, _ := payload.([]cycleItem)
		enc := newGCX(&buf, "analyze.cycles",
			[]string{"size", "severity", "nodes"},
			"count", fmt.Sprintf("%d", len(cycles)),
		)
		for _, c := range cycles {
			if err := enc.WriteRow(c.Size, c.Severity, strings.Join(c.Nodes, ",")); err != nil {
				return nil, err
			}
		}
		return buf.Bytes(), enc.Close()
	default:
		// Fall back to generic so analyze variants without a hand-tuned
		// encoder still produce valid GCX instead of failing.
		if err := wire.EncodeAny(&buf, "analyze."+kind, payload); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
}

// Narrow row contracts used by analyze.* encoders so the caller can
// build them from heterogeneous internal structs.
type deadCodeItem struct {
	ID, Kind, Name, Path, Reason string
	Line                         int
}

type hotspotItem struct {
	ID, Name, Path string
	Line, FanIn    int
	FanOut         int
	CrossCommunity int
	Score          float64
}

type cycleItem struct {
	Size     int
	Severity string
	Nodes    []string
}

// encodeGeneric serialises an arbitrary handler payload through the
// generic shape-inferring encoder in internal/wire. It is the safety
// net for tools that declare `format: "gcx"` support but don't have
// a hand-tuned encoder bound yet (get_editing_context, smart_context,
// contracts, get_repo_outline, and the long tail). The benchmark
// harness shows this still saves tokens on list-shaped payloads and
// stays round-trippable everywhere.
func encodeGeneric(tool string, payload any) ([]byte, error) {
	var buf bytes.Buffer
	if err := wire.EncodeAny(&buf, tool, payload); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// --------------------------------------------------------------------
// small utilities
// --------------------------------------------------------------------

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func indexNodes(nodes []*graph.Node) map[string]*graph.Node {
	m := make(map[string]*graph.Node, len(nodes))
	for _, n := range nodes {
		m[n.ID] = n
	}
	return m
}

