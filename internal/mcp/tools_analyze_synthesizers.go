package mcp

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// Edge.Meta keys the framework dynamic-dispatch synthesizer engine
// stamps (mirrors resolver.MetaSynthesizedBy / MetaProvenance — kept as
// literals here so the MCP layer doesn't depend on the resolver package
// just for two string constants).
const (
	metaSynthesizedByKey = "synthesized_by"
	metaProvenanceKey    = "provenance"
)

// handleAnalyzeSynthesizers rolls up the framework dynamic-dispatch
// synthesizer engine's output: every edge carrying a `synthesized_by`
// provenance marker, grouped by the synthesizer that produced it. This
// is the queryable face of the engine — "which framework-dispatch passes
// fired, and how many edges did each materialise?" — letting an agent
// audit the heuristic, framework-wired call edges (gRPC stub → handler,
// Temporal proxy → activity, event-channel emit → listener, native
// bridge call → implementation) separately from compiler-verified ones.
//
// Optional `name` filters to a single synthesizer.
func (s *Server) handleAnalyzeSynthesizers(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	nameFilter := strings.TrimSpace(stringArg(args, "name"))

	type sample struct {
		From string `json:"from"`
		To   string `json:"to"`
		Kind string `json:"kind"`
		Via  string `json:"via,omitempty"`
	}
	type synthRow struct {
		Name       string         `json:"synthesizer"`
		Provenance string         `json:"provenance"`
		Edges      int            `json:"edges"`
		ByKind     map[string]int `json:"by_kind"`
		Samples    []sample       `json:"samples,omitempty"`
	}
	const maxSamples = 5
	rows := map[string]*synthRow{}
	for _, e := range s.graph.AllEdges() {
		if e == nil || e.Meta == nil {
			continue
		}
		by, _ := e.Meta[metaSynthesizedByKey].(string)
		if by == "" {
			continue
		}
		if nameFilter != "" && by != nameFilter {
			continue
		}
		row, ok := rows[by]
		if !ok {
			prov, _ := e.Meta[metaProvenanceKey].(string)
			row = &synthRow{Name: by, Provenance: prov, ByKind: map[string]int{}}
			rows[by] = row
		}
		row.Edges++
		row.ByKind[string(e.Kind)]++
		if len(row.Samples) < maxSamples {
			via, _ := e.Meta["via"].(string)
			row.Samples = append(row.Samples, sample{
				From: e.From,
				To:   e.To,
				Kind: string(e.Kind),
				Via:  via,
			})
		}
	}

	out := make([]*synthRow, 0, len(rows))
	total := 0
	for _, r := range rows {
		total += r.Edges
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Edges != out[j].Edges {
			return out[i].Edges > out[j].Edges
		}
		return out[i].Name < out[j].Name
	})

	if isCompact(req) {
		var b strings.Builder
		for _, r := range out {
			b.WriteString(r.Name)
			b.WriteString(": ")
			b.WriteString(strconv.Itoa(r.Edges))
			b.WriteString(" edges (")
			b.WriteString(r.Provenance)
			b.WriteString(")\n")
		}
		if len(out) == 0 {
			b.WriteString("no synthesized edges\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}

	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"synthesizers": out,
		"total_edges":  total,
	})
}
