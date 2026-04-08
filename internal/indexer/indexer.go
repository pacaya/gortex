package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/search"
)

// IndexResult holds the outcome of an indexing operation.
type IndexResult struct {
	NodeCount  int          `json:"node_count"`
	EdgeCount  int          `json:"edge_count"`
	FileCount  int          `json:"file_count"`
	DurationMs int64        `json:"duration_ms"`
	Errors     []IndexError `json:"errors,omitempty"`
}

// IndexError records a per-file parsing failure.
type IndexError struct {
	FilePath string `json:"file_path"`
	Error    string `json:"error"`
}

// Indexer walks a repository and populates the graph.
type Indexer struct {
	graph    *graph.Graph
	registry *parser.Registry
	resolver *resolver.Resolver
	search   search.Backend
	config   config.IndexConfig
	rootPath string
	logger   *zap.Logger

	// repoPrefix is set in multi-repo mode to prefix all file paths and node IDs.
	// When empty, the indexer operates in single-repo mode (backward compatible).
	repoPrefix string

	// Mtime tracking and parse error retention for index health diagnostics.
	parseErrors   []IndexError
	fileMtimes    map[string]int64
	lastIndexTime time.Time
	totalDetected int
	mtimeMu       sync.RWMutex
}

// New creates an Indexer.
func New(g *graph.Graph, reg *parser.Registry, cfg config.IndexConfig, logger *zap.Logger) *Indexer {
	return &Indexer{
		graph:      g,
		registry:   reg,
		resolver:   resolver.New(g),
		search:     search.NewAuto(),
		config:     cfg,
		logger:     logger,
		fileMtimes: make(map[string]int64),
	}
}

// Graph returns the underlying graph.
func (idx *Indexer) Graph() *graph.Graph { return idx.graph }

// Search returns the search backend.
func (idx *Indexer) Search() search.Backend { return idx.search }

// RootPath returns the root path used for relative path computation.
func (idx *Indexer) RootPath() string { return idx.rootPath }

// SetRepoPrefix sets the repository prefix for multi-repo mode.
// When non-empty, all node IDs and file paths are prefixed with "<repoPrefix>/".
func (idx *Indexer) SetRepoPrefix(prefix string) { idx.repoPrefix = prefix }

// RepoPrefix returns the current repository prefix.
func (idx *Indexer) RepoPrefix() string { return idx.repoPrefix }

// prefixPath prepends the repoPrefix to a relative path when in multi-repo mode.
// Returns the path unchanged when repoPrefix is empty.
func (idx *Indexer) prefixPath(relPath string) string {
	if idx.repoPrefix == "" {
		return relPath
	}
	return idx.repoPrefix + "/" + relPath
}

// applyRepoPrefix transforms nodes and edges produced by an extractor to include
// the repo prefix in IDs and file paths. Sets Node.RepoPrefix on all nodes.
// This is a no-op when repoPrefix is empty (single-repo mode).
func (idx *Indexer) applyRepoPrefix(nodes []*graph.Node, edges []*graph.Edge) {
	if idx.repoPrefix == "" {
		return
	}
	prefix := idx.repoPrefix + "/"
	for _, n := range nodes {
		n.ID = prefix + n.ID
		n.FilePath = prefix + n.FilePath
		n.RepoPrefix = idx.repoPrefix
	}
	for _, e := range edges {
		e.From = prefix + e.From
		e.To = prefix + e.To
		e.FilePath = prefix + e.FilePath
	}
}

// Index walks root and populates the graph using a concurrent worker pool.
func (idx *Indexer) Index(root string) (*IndexResult, error) {
	start := time.Now()

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	idx.rootPath = absRoot

	// Collect files.
	var files []string
	err = filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if idx.shouldExclude(path, absRoot) {
				return filepath.SkipDir
			}
			return nil
		}
		if _, ok := idx.registry.DetectLanguage(path); ok {
			if !idx.shouldExclude(path, absRoot) {
				files = append(files, path)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Worker pool.
	workers := idx.config.Workers
	if workers <= 0 {
		workers = 1
	}

	type fileResult struct {
		nodes []*graph.Node
		edges []*graph.Edge
		err   error
		file  string
	}

	fileCh := make(chan string, len(files))
	resultCh := make(chan fileResult, len(files))

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range fileCh {
				fr := fileResult{file: path}
				src, err := os.ReadFile(path)
				if err != nil {
					fr.err = err
					resultCh <- fr
					continue
				}

				relPath, _ := filepath.Rel(absRoot, path)
				lang, _ := idx.registry.DetectLanguage(path)
				ext, _ := idx.registry.GetByLanguage(lang)
				if ext == nil {
					continue
				}

				result, err := ext.Extract(relPath, src)
				if err != nil {
					fr.err = err
					resultCh <- fr
					continue
				}
				fr.nodes = result.Nodes
				fr.edges = result.Edges
				resultCh <- fr
			}
		}()
	}

	for _, f := range files {
		fileCh <- f
	}
	close(fileCh)

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	var errors []IndexError
	fileCount := 0
	for fr := range resultCh {
		if fr.err != nil {
			errors = append(errors, IndexError{FilePath: fr.file, Error: fr.err.Error()})
			continue
		}
		fileCount++
		idx.applyRepoPrefix(fr.nodes, fr.edges)
		for _, n := range fr.nodes {
			idx.graph.AddNode(n)
		}
		for _, e := range fr.edges {
			idx.graph.AddEdge(e)
		}
	}

	// Populate fileMtimes for all detected files.
	idx.mtimeMu.Lock()
	idx.fileMtimes = make(map[string]int64, len(files))
	for _, f := range files {
		if info, err := os.Stat(f); err == nil {
			relPath, _ := filepath.Rel(absRoot, f)
			idx.fileMtimes[filepath.ToSlash(relPath)] = info.ModTime().UnixNano()
		}
	}
	idx.mtimeMu.Unlock()

	// Retain parse errors and record index metadata.
	idx.parseErrors = errors
	idx.totalDetected = len(files)
	idx.lastIndexTime = time.Now()

	// Resolve cross-file references.
	idx.resolver.ResolveAll()

	// Infer structural interface satisfaction.
	idx.resolver.InferImplements()

	// Build search index.
	idx.buildSearchIndex()

	// Auto-upgrade to Bleve if above threshold.
	if idx.search.Count() >= search.AutoThreshold {
		if blv, err := search.NewBleve(); err == nil {
			old := idx.search
			for _, n := range idx.graph.AllNodes() {
				if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
					continue
				}
				sig, _ := n.Meta["signature"].(string)
				blv.Add(n.ID, n.Name, n.FilePath, sig)
			}
			idx.search = blv
			old.Close()
			idx.logger.Info("search: upgraded to Bleve backend",
				zap.Int("symbols", idx.search.Count()))
		}
	}

	return &IndexResult{
		NodeCount:  idx.graph.NodeCount(),
		EdgeCount:  idx.graph.EdgeCount(),
		FileCount:  fileCount,
		DurationMs: time.Since(start).Milliseconds(),
		Errors:     errors,
	}, nil
}

// IndexFile parses a single file and patches the graph (evict then add).
func (idx *Indexer) IndexFile(filePath string) error {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return err
	}

	relPath, err := filepath.Rel(idx.rootPath, absPath)
	if err != nil {
		relPath = filePath
	}

	// In multi-repo mode, the graph stores prefixed file paths.
	graphPath := idx.prefixPath(relPath)

	// Evict existing data for this file (graph + search).
	for _, n := range idx.graph.GetFileNodes(graphPath) {
		if n.Kind != graph.KindFile && n.Kind != graph.KindImport {
			idx.search.Remove(n.ID)
		}
	}
	idx.graph.EvictFile(graphPath)

	src, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}

	lang, ok := idx.registry.DetectLanguage(absPath)
	if !ok {
		return nil
	}
	ext, _ := idx.registry.GetByLanguage(lang)
	if ext == nil {
		return nil
	}

	result, err := ext.Extract(relPath, src)
	if err != nil {
		return err
	}

	idx.applyRepoPrefix(result.Nodes, result.Edges)

	for _, n := range result.Nodes {
		idx.graph.AddNode(n)
	}
	for _, e := range result.Edges {
		idx.graph.AddEdge(e)
	}

	// Add new symbols to search index.
	for _, n := range result.Nodes {
		if n.Kind != graph.KindFile && n.Kind != graph.KindImport {
			sig, _ := n.Meta["signature"].(string)
			idx.search.Add(n.ID, n.Name, n.FilePath, sig)
		}
	}

	idx.resolver.ResolveFile(graphPath)

	// Update mtime for this file (uses raw relPath for disk-based tracking).
	if info, err := os.Stat(absPath); err == nil {
		idx.mtimeMu.Lock()
		idx.fileMtimes[filepath.ToSlash(relPath)] = info.ModTime().UnixNano()
		idx.mtimeMu.Unlock()
	}

	return nil
}

// EvictFile removes all nodes and edges belonging to filePath.
func (idx *Indexer) EvictFile(filePath string) (int, int) {
	relPath, err := filepath.Rel(idx.rootPath, filePath)
	if err != nil {
		relPath = filePath
	}
	// In multi-repo mode, the graph stores prefixed file paths.
	graphPath := idx.prefixPath(relPath)
	// Remove from search index.
	for _, n := range idx.graph.GetFileNodes(graphPath) {
		if n.Kind != graph.KindFile && n.Kind != graph.KindImport {
			idx.search.Remove(n.ID)
		}
	}
	return idx.graph.EvictFile(graphPath)
}

// buildSearchIndex populates the search backend from the current graph.
func (idx *Indexer) buildSearchIndex() {
	for _, n := range idx.graph.AllNodes() {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		sig, _ := n.Meta["signature"].(string)
		idx.search.Add(n.ID, n.Name, n.FilePath, sig)
	}
}

func (idx *Indexer) shouldExclude(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	// Normalize to forward slashes for pattern matching.
	rel = filepath.ToSlash(rel)

	for _, pattern := range idx.config.Exclude {
		// Simple directory-based exclusion.
		dir := strings.TrimSuffix(pattern, "/**")
		dir = strings.TrimSuffix(dir, "/*")
		dir = strings.TrimPrefix(dir, "**/")

		if strings.Contains(rel, dir+"/") || strings.HasPrefix(rel, dir+"/") || rel == dir {
			return true
		}
	}
	return false
}

// ParseErrors returns the parse errors from the last full index.
func (idx *Indexer) ParseErrors() []IndexError {
	return idx.parseErrors
}

// FileMtimes returns a copy of the file modification time map.
func (idx *Indexer) FileMtimes() map[string]int64 {
	idx.mtimeMu.RLock()
	defer idx.mtimeMu.RUnlock()
	out := make(map[string]int64, len(idx.fileMtimes))
	for k, v := range idx.fileMtimes {
		out[k] = v
	}
	return out
}

// LastIndexTime returns the timestamp of the last full index.
func (idx *Indexer) LastIndexTime() time.Time {
	return idx.lastIndexTime
}

// TotalDetected returns the total number of files detected during the last full index.
func (idx *Indexer) TotalDetected() int {
	return idx.totalDetected
}

// IsStale returns true if the file at relPath has been modified on disk since
// it was last indexed, based on comparing stored mtime against current disk mtime.
func (idx *Indexer) IsStale(relPath string) bool {
	relPath = filepath.ToSlash(relPath)

	idx.mtimeMu.RLock()
	storedMtime, ok := idx.fileMtimes[relPath]
	idx.mtimeMu.RUnlock()
	if !ok {
		// Unknown file — treat as stale.
		return true
	}

	absPath := filepath.Join(idx.rootPath, filepath.FromSlash(relPath))
	info, err := os.Stat(absPath)
	if err != nil {
		// Can't stat — treat as stale.
		return true
	}

	return info.ModTime().UnixNano() != storedMtime
}
