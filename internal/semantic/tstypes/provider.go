package tstypes

import (
	"runtime"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// Provider is the semantic.Provider over one LangSpec. Pure in-process
// — Available is unconditionally true, no subprocess is ever spawned,
// Close is a no-op. It is supplemental: it augments whichever provider
// wins the per-language arbitration (LSP / SCIP) instead of competing
// with it, and only ever stamps AST-grade provenance, so a
// compiler-grade pass running before or after never gets downgraded.
type Provider struct {
	spec   *LangSpec
	logger *zap.Logger
}

// NewProvider wraps a LangSpec as a semantic provider.
func NewProvider(spec *LangSpec, logger *zap.Logger) *Provider {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Provider{spec: spec, logger: logger}
}

// DefaultProviders returns the in-process type resolvers for every
// supported language. Registered unconditionally at daemon boot —
// disable one via a `semantic.providers` config entry with
// `enabled: false` under its name.
func DefaultProviders(logger *zap.Logger) []*Provider {
	return []*Provider{
		NewProvider(JavaSpec(), logger),
		NewProvider(PythonSpec(), logger),
		NewProvider(RubySpec(), logger),
		NewProvider(RustSpec(), logger),
		NewProvider(TypeScriptSpec(), logger),
		NewProvider(CSharpSpec(), logger),
	}
}

func (p *Provider) Name() string        { return p.spec.ProviderName }
func (p *Provider) Languages() []string { return p.spec.Languages }
func (p *Provider) Available() bool     { return true }
func (p *Provider) Close() error        { return nil }

// Supplemental marks this provider as augmenting (see
// semantic.SupplementalProvider): the manager runs it for its
// languages in addition to the arbitration winner.
func (p *Provider) Supplemental() bool { return true }

// Enrich runs the full-repo pass for a single-repo (un-prefixed) graph.
// It delegates to EnrichRepo with an empty prefix — the in-memory single
// repo case where every real node carries RepoPrefix "".
func (p *Provider) Enrich(g graph.Store, repoRoot string) (*semantic.EnrichResult, error) {
	return p.EnrichRepo(g, "", repoRoot)
}

// EnrichRepo runs the full-repo pass: parse every file of the provider's
// languages that belong to repoPrefix under repoRoot in a bounded worker
// pool, then apply the per-file facts to the graph from a single
// goroutine. repoPrefix scopes file selection so a multi-repo graph with
// a colliding relative path never reads the wrong repo's bytes.
func (p *Provider) EnrichRepo(g graph.Store, repoPrefix, repoRoot string) (*semantic.EnrichResult, error) {
	start := time.Now()
	res := &semantic.EnrichResult{
		Provider: p.Name(),
		Language: p.spec.Languages[0],
	}

	files := languageFiles(g, p.spec, repoPrefix, repoRoot)
	if len(files) > 0 {
		workers := runtime.GOMAXPROCS(0)
		if workers > 8 {
			workers = 8
		}
		if workers > len(files) {
			workers = len(files)
		}
		jobs := make(chan fileRef)
		factsCh := make(chan *fileFacts, workers)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for ref := range jobs {
					facts, err := analyzeFile(p.spec, ref)
					if err != nil {
						p.logger.Debug("tstypes: file analysis failed",
							zap.String("provider", p.Name()),
							zap.String("file", ref.node.FilePath),
							zap.Error(err))
						continue
					}
					if facts != nil {
						factsCh <- facts
					}
				}
			}()
		}
		go func() {
			for _, ref := range files {
				jobs <- ref
			}
			close(jobs)
			wg.Wait()
			close(factsCh)
		}()

		var all []*fileFacts
		for facts := range factsCh {
			all = append(all, facts)
		}
		// Parsing above is pure and fans out across workers; the apply
		// phase mutates the shared graph (retargets edges, reindexes,
		// stamps provenance) and MUST run under the graph-wide resolve
		// mutex so it serialises against concurrent resolver / cross-repo
		// passes — the same lock every other edge-mutating pass holds.
		mu := g.ResolveMutex()
		mu.Lock()
		ap := newApplier(g, p.spec, p.Name())
		ap.applyAll(all, res)
		ap.flush()
		analyzed := make(map[string]bool, len(all))
		for _, facts := range all {
			analyzed[facts.file] = true
		}
		p.countCoverage(g, repoPrefix, analyzed, res)
		mu.Unlock()
	}

	res.DurationMs = time.Since(start).Milliseconds()
	return res, nil
}

// EnrichFile runs the single-file incremental pass. filePath is the graph
// file key (prefixed in multi-repo mode), which is globally unique — the
// file's own node names the repo, so this is inherently scoped to the
// right repo without a separate prefix argument.
func (p *Provider) EnrichFile(g graph.Store, repoRoot, filePath string) (*semantic.EnrichResult, error) {
	start := time.Now()
	res := &semantic.EnrichResult{
		Provider: p.Name(),
		Language: p.spec.Languages[0],
	}
	// Find the file's own node by its exact graph key. It carries the
	// RepoPrefix that maps the prefixed path back to the on-disk file.
	var fileNode *graph.Node
	for _, n := range g.GetFileNodes(filePath) {
		if n.Kind == graph.KindFile {
			fileNode = n
			break
		}
	}
	if fileNode == nil || !p.spec.handles(fileNode.Language) {
		res.DurationMs = time.Since(start).Milliseconds()
		return res, nil
	}
	ref, ok := fileRefFor(fileNode, repoRoot)
	if !ok {
		res.DurationMs = time.Since(start).Milliseconds()
		return res, nil
	}
	facts, err := analyzeFile(p.spec, ref)
	if err != nil {
		return nil, err
	}
	if facts != nil {
		// Same contract as the full pass: the apply phase mutates the
		// shared graph and runs under the resolve mutex so it does not
		// race a concurrent watcher / resolver pass on another file.
		mu := g.ResolveMutex()
		mu.Lock()
		ap := newApplier(g, p.spec, p.Name())
		ap.applyAll([]*fileFacts{facts}, res)
		ap.flush()
		p.countCoverage(g, fileNode.RepoPrefix, map[string]bool{facts.file: true}, res)
		mu.Unlock()
	}
	res.DurationMs = time.Since(start).Milliseconds()
	return res, nil
}

// countCoverage fills the symbols-covered counters: total is every
// symbol of the provider's languages, covered is the subset living in
// files the pass analyzed.
func (p *Provider) countCoverage(g graph.Store, repoPrefix string, analyzed map[string]bool, res *semantic.EnrichResult) {
	langs := make(map[string]bool, len(p.spec.Languages))
	for _, l := range p.spec.Languages {
		langs[l] = true
	}
	// Indexed repo-scoped scan rather than a whole-graph AllNodes walk; the
	// whole-graph form also counted every other repo's symbols against this
	// repo's coverage. Fall back to AllNodes only for the embedded ("") path.
	nodes := g.GetRepoNodes(repoPrefix)
	if len(nodes) == 0 && repoPrefix == "" {
		nodes = g.AllNodes()
	}
	for _, n := range nodes {
		if n.RepoPrefix != repoPrefix || !langs[n.Language] || n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		res.SymbolsTotal++
		if analyzed[n.FilePath] {
			res.SymbolsCovered++
		}
	}
	if res.SymbolsTotal > 0 {
		res.CoveragePercent = float64(res.SymbolsCovered) / float64(res.SymbolsTotal) * 100
	}
}
