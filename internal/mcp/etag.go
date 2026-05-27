package mcp

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/query"
)

// computeETag produces a short content hash suitable for conditional fetch.
// Streams the JSON serialization straight into the hash so we don't
// allocate the full marshaled byte slice (significant on large
// payloads — a 500-symbol SubGraph used to allocate ~100 KiB just to
// feed sha256).
func computeETag(data any) string {
	h := sha256.New()
	if err := json.NewEncoder(h).Encode(data); err != nil {
		return ""
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:8]) // 16 hex chars — collision-safe for session use
}

// etagSubGraph is a fast structural ETag specialised for query.SubGraph
// payloads (the get_file_summary / get_editing_context hot path).
// Instead of going through json.Marshal on every node + edge + Meta map
// (which is the dominant cost for a 500-symbol file), it hashes a
// stable structural fingerprint: each node's id + line range, each
// edge's (from, to, kind), and the truncation / total counts. That
// keeps the invariant the callers depend on — "the etag changes when
// the file's listing changes" — without paying for the body of every
// Meta map on every call.
func etagSubGraph(sg *query.SubGraph) string {
	if sg == nil {
		return ""
	}
	h := sha256.New()
	var buf [16]byte
	for _, n := range sg.Nodes {
		if n == nil {
			continue
		}
		h.Write([]byte(n.ID))
		binary.BigEndian.PutUint32(buf[0:4], uint32(n.StartLine))
		binary.BigEndian.PutUint32(buf[4:8], uint32(n.EndLine))
		h.Write(buf[:8])
		h.Write([]byte{0})
	}
	h.Write([]byte{1})
	for _, e := range sg.Edges {
		if e == nil {
			continue
		}
		h.Write([]byte(e.From))
		h.Write([]byte{31})
		h.Write([]byte(e.To))
		h.Write([]byte{31})
		h.Write([]byte(e.Kind))
		h.Write([]byte{0})
	}
	binary.BigEndian.PutUint64(buf[0:8], uint64(sg.TotalNodes))
	binary.BigEndian.PutUint64(buf[8:16], uint64(sg.TotalEdges))
	h.Write(buf[:16])
	if sg.Truncated {
		h.Write([]byte{1})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:8])
}

// notModifiedResult returns a minimal "not modified" response with the matching etag.
func notModifiedResult(etag string) *mcp.CallToolResult {
	result, _ := mcp.NewToolResultJSON(map[string]any{
		"not_modified": true,
		"etag":         etag,
	})
	return result
}

// withETag adds an etag field to a map result and returns the JSON tool result.
func withETag(data map[string]any) (*mcp.CallToolResult, error) {
	etag := computeETag(data)
	data["etag"] = etag
	return mcp.NewToolResultJSON(data)
}

// computePackRoot derives an order-insensitive content hash over the
// symbols a smart_context call selected — its "pack root". Unlike
// computeETag it ignores the incidental ordering of result lists (the
// search rerank can reorder an otherwise-unchanged pack), so a
// repeated call on unchanged code yields the same root and the
// if_none_match dedup fires reliably.
//
// Each symbol contributes its ID, start line, and source — all
// stable: the source is read from disk and the line is set at index
// time. Derived metadata (signatures) is deliberately excluded, since
// it can be re-enriched between calls. The root therefore changes when
// the set of selected symbols changes, when one moves, or when its
// source does.
func computePackRoot(result map[string]any) string {
	var items []string
	add := func(e map[string]any) {
		id, _ := e["id"].(string)
		body, _ := e["source"].(string)
		line := ""
		switch v := e["start_line"].(type) {
		case int:
			line = strconv.Itoa(v)
		case float64:
			line = strconv.Itoa(int(v))
		}
		items = append(items, id+"\x1f"+line+"\x1f"+body)
	}
	// A graded response carries its symbols (with source) in the
	// manifest; a flat one carries them in relevant_symbols. Hash
	// whichever is present so the same symbol is never counted twice.
	if mani, ok := result["context_manifest"].(map[string]any); ok {
		if entries, ok := mani["entries"].([]map[string]any); ok {
			for _, e := range entries {
				add(e)
			}
		}
	} else if syms, ok := result["relevant_symbols"].([]map[string]any); ok {
		for _, e := range syms {
			add(e)
		}
	}
	sort.Strings(items)

	h := sha256.New()
	if task, ok := result["task"].(string); ok {
		h.Write([]byte(task))
	}
	h.Write([]byte{0})
	for _, it := range items {
		h.Write([]byte(it))
		h.Write([]byte{0})
	}
	for _, key := range []string{"files_to_edit", "related_test_files"} {
		if list, ok := result[key].([]string); ok {
			cp := append([]string(nil), list...)
			sort.Strings(cp)
			for _, s := range cp {
				h.Write([]byte(s))
				h.Write([]byte{0})
			}
		}
		h.Write([]byte{1})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:8])
}
