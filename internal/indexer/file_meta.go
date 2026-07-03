package indexer

import (
	"encoding/hex"
	"encoding/json"

	"github.com/zeebo/blake3"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// persistFileMeta records one file's per-file metadata row — the BLAKE3
// content hash (the same algorithm as the Merkle leaf), byte size, extracted
// node count, and a JSON array of parse-error locations — in the backend's
// files sidecar (when it implements graph.FileMetaWriter; the on-disk and
// in-memory backends both do). index_health reads these rows to report
// per-file parse errors + node counts; the Merkle tree stays the authoritative
// change detector.
//
// relPath / src are the pre-repo-prefix path and the (transformed) content;
// the persisted file_path is repo-prefixed so it matches the graph node ids.
// The file's prior row is deleted first so a reindex replaces it cleanly.
func (idx *Indexer) persistFileMeta(relPath string, src []byte, result *parser.ExtractionResult) {
	if result == nil || relPath == "" {
		return
	}
	fw, ok := idx.graph.(graph.FileMetaWriter)
	if !ok {
		return
	}
	filePath := relPath
	if idx.repoPrefix != "" {
		filePath = idx.repoPrefix + "/" + relPath
	}

	h := blake3.New()
	_, _ = h.Write(src)
	contentHash := hex.EncodeToString(h.Sum(nil))

	errs := ""
	if locs := result.Tree.ParseErrorLocations(); len(locs) > 0 {
		if b, err := json.Marshal(locs); err == nil {
			errs = string(b)
		}
	}

	row := graph.FileMetaRow{
		FilePath:    filePath,
		ContentHash: contentHash,
		Size:        len(src),
		NodeCount:   len(result.Nodes),
		Errors:      errs,
	}
	_ = fw.DeleteFileMetasByFiles(idx.repoPrefix, []string{filePath})
	_ = fw.SetFileMetas(idx.repoPrefix, []graph.FileMetaRow{row})
}

// setReparsePendingEnrichment sets or clears
// graph.MetaReparsePendingEnrichment on a file's KindFile node, marking
// whether the most recent live re-parse resolved the file without re-running
// semantic enrichment. It round-trips the node through AddNode so a disk
// backend persists the meta change (an in-place map write is lost on sqlite),
// and skips the write entirely when the marker is already in the desired state
// so a save storm never re-persists unchanged nodes.
func (idx *Indexer) setReparsePendingEnrichment(graphPath string, pending bool) {
	for _, n := range idx.graph.GetFileNodes(graphPath) {
		if n.Kind != graph.KindFile {
			continue
		}
		_, had := n.Meta[graph.MetaReparsePendingEnrichment]
		switch {
		case pending && !had:
			if n.Meta == nil {
				n.Meta = map[string]any{}
			}
			n.Meta[graph.MetaReparsePendingEnrichment] = true
		case !pending && had:
			delete(n.Meta, graph.MetaReparsePendingEnrichment)
		default:
			return // already in the desired state — no round-trip
		}
		idx.graph.AddNode(n)
		return
	}
}
