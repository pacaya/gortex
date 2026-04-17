package search

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
	"github.com/blevesearch/bleve/v2/analysis/token/lowercase"
	"github.com/blevesearch/bleve/v2/analysis/tokenizer/unicode"
	"github.com/blevesearch/bleve/v2/mapping"

	// Register default KV store.
	_ "github.com/blevesearch/bleve/v2/index/upsidedown/store/gtreap"
)

// BleveBackend wraps Bleve for full-text search over code symbols.
// Better for large repos (50k+ symbols) and multi-repo mode.
type BleveBackend struct {
	index    bleve.Index
	count    atomic.Int64
	diskPath string // non-empty when the index is disk-backed (scorch)
}

// DiskPath returns the directory containing the on-disk index, or ""
// when the backend is running fully in memory.
func (b *BleveBackend) DiskPath() string { return b.diskPath }

// DiskBytes walks the index directory and sums file sizes. Zero when
// in-memory. Called at most once per `gortex daemon status` invocation.
func (b *BleveBackend) DiskBytes() uint64 {
	if b.diskPath == "" {
		return 0
	}
	var total uint64
	_ = filepath.Walk(b.diskPath, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		total += uint64(info.Size())
		return nil
	})
	return total
}

// symbolDoc is the document structure indexed by Bleve.
type symbolDoc struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Signature string `json:"signature"`
	// Combined field for broader matching.
	All string `json:"all"`
}

// NewBleve creates a Bleve-backed search index (in-memory via
// upsidedown + gtreap). Heavy but self-contained — no disk writes.
func NewBleve() (*BleveBackend, error) {
	indexMapping := buildMapping()

	idx, err := bleve.NewMemOnly(indexMapping)
	if err != nil {
		return nil, err
	}

	return &BleveBackend{index: idx}, nil
}

// NewBleveDisk creates a disk-backed Bleve index under dir using the
// scorch storage engine. The dir is created if missing; any existing
// index at the same path is removed first because we always rebuild
// from scratch (we don't have incremental-write semantics yet).
// Scorch is ~10-20× more memory-efficient than the in-memory
// upsidedown+gtreap store at the cost of disk IO on write, which only
// matters during initial index construction.
func NewBleveDisk(dir string) (*BleveBackend, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("bleve disk dir: %w", err)
	}
	// Use a fixed child name so a caller passing a shared parent dir
	// doesn't overwrite neighbouring state.
	indexPath := filepath.Join(dir, "bleve.scorch")
	if _, err := os.Stat(indexPath); err == nil {
		if rmErr := os.RemoveAll(indexPath); rmErr != nil {
			return nil, fmt.Errorf("clearing old bleve index at %s: %w", indexPath, rmErr)
		}
	}

	indexMapping := buildMapping()
	idx, err := bleve.New(indexPath, indexMapping)
	if err != nil {
		return nil, fmt.Errorf("opening bleve disk index at %s: %w", indexPath, err)
	}

	return &BleveBackend{index: idx, diskPath: indexPath}, nil
}

func buildMapping() *mapping.IndexMappingImpl {
	indexMapping := bleve.NewIndexMapping()

	// Custom analyzer: unicode tokenizer + lowercase.
	// We pre-tokenize camelCase in Add(), so the analyzer just needs
	// to handle the space-separated tokens we give it.
	err := indexMapping.AddCustomAnalyzer("code", map[string]any{
		"type":      custom.Name,
		"tokenizer": unicode.Name,
		"token_filters": []string{
			lowercase.Name,
		},
	})
	if err != nil {
		// Fallback to default analyzer.
		return indexMapping
	}

	// Document mapping.
	docMapping := bleve.NewDocumentMapping()

	nameField := bleve.NewTextFieldMapping()
	nameField.Analyzer = "code"
	nameField.Store = false
	docMapping.AddFieldMappingsAt("name", nameField)

	pathField := bleve.NewTextFieldMapping()
	pathField.Analyzer = "code"
	pathField.Store = false
	docMapping.AddFieldMappingsAt("path", pathField)

	sigField := bleve.NewTextFieldMapping()
	sigField.Analyzer = "code"
	sigField.Store = false
	docMapping.AddFieldMappingsAt("signature", sigField)

	allField := bleve.NewTextFieldMapping()
	allField.Analyzer = "code"
	allField.Store = false
	docMapping.AddFieldMappingsAt("all", allField)

	indexMapping.DefaultMapping = docMapping
	indexMapping.DefaultAnalyzer = "code"

	return indexMapping
}

func (b *BleveBackend) Add(id string, fields ...string) {
	// Pre-tokenize camelCase and rejoin with spaces so Bleve's
	// unicode tokenizer can split them.
	var parts []string
	for _, f := range fields {
		tokens := Tokenize(f)
		parts = append(parts, strings.Join(tokens, " "))
	}

	doc := symbolDoc{
		All: strings.Join(parts, " "),
	}
	if len(parts) > 0 {
		doc.Name = parts[0]
	}
	if len(parts) > 1 {
		doc.Path = parts[1]
	}
	if len(parts) > 2 {
		doc.Signature = parts[2]
	}

	if err := b.index.Index(id, doc); err == nil {
		b.count.Add(1)
	}
}

func (b *BleveBackend) Remove(id string) {
	if err := b.index.Delete(id); err == nil {
		b.count.Add(-1)
	}
}

func (b *BleveBackend) Search(query string, limit int) []SearchResult {
	// Pre-tokenize the query for camelCase splitting.
	tokens := TokenizeQuery(query)
	if len(tokens) == 0 {
		return nil
	}
	q := strings.Join(tokens, " ")

	searchReq := bleve.NewSearchRequest(bleve.NewQueryStringQuery(q))
	searchReq.Size = limit

	res, err := b.index.Search(searchReq)
	if err != nil || res.Total == 0 {
		return nil
	}

	out := make([]SearchResult, 0, len(res.Hits))
	for _, hit := range res.Hits {
		out = append(out, SearchResult{
			ID:    hit.ID,
			Score: hit.Score,
		})
	}
	return out
}

func (b *BleveBackend) Count() int {
	return int(b.count.Load())
}

// SizeBytes approximates Bleve's in-memory footprint. Bleve (with the
// default upsidedown + gtreap KV store) is much hungrier than the
// fields-and-postings accounting would suggest — gtreap's immutable
// persistent trees retain copy-on-write versions, and the upsidedown
// row layout expands every symbol into many keys. Calibrated against
// heap profiles on a ~65k-symbol index: live Bleve heap was ~2.0 GiB,
// i.e. ~32 KiB per document. Earlier estimates of ~2 KiB/doc were off
// by 16× and were the root cause of the large "other" bucket users
// saw in `gortex daemon status`.
func (b *BleveBackend) SizeBytes() uint64 {
	return uint64(b.count.Load()) * 32768
}

func (b *BleveBackend) Close() {
	if b.index != nil {
		_ = b.index.Close()
	}
}
