package semantic

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// LSPRouter is the slice of lsp.Router that semantic.Manager needs to
// drive batch enrichment without importing the lsp package directly
// (which would create an import cycle, since lsp already imports
// semantic for the Provider interface).
type LSPRouter interface {
	// EnabledSpecNames returns the names of LSP specs the user has
	// enabled in config (no spawn implied — call ProviderForSpec to
	// trigger lazy spawn).
	EnabledSpecNames() []string

	// SpecAvailable reports whether the named spec is enabled AND
	// its command resolves on PATH. Pure read — no subprocess
	// spawn. Used by Manager.HasProviders.
	SpecAvailable(name string) bool

	// SpecLanguages returns the language codes the named spec
	// serves. Pure metadata read — no spawn. Used by EnrichAll
	// arbitration to compare candidate specs per language.
	SpecLanguages(name string) []string

	// SpecPriority returns the spec's default priority (lower = wins
	// over higher). Pure metadata read — no spawn.
	SpecPriority(name string) int

	// ProviderForSpec lazy-spawns and returns the LSP provider for
	// the given spec name as a semantic.Provider. Returns an error
	// if the spec is not enabled or its command is not on PATH.
	ProviderForSpec(name string) (Provider, error)

	// ProviderForSpecWorkspace is ProviderForSpec scoped to a workspace
	// root, so each repo gets its own provider instance (keyed by
	// (spec, workspace)). Per-repo instances are what let cross-repo
	// enrichment run concurrently without sharing one provider's LSP
	// connection or document caches. While the returned provider is held,
	// it is pinned in-use; the caller MUST pair it with ReleaseSpecWorkspace.
	ProviderForSpecWorkspace(name, workspace string) (Provider, error)

	// ReleaseSpecWorkspace marks a provider obtained via
	// ProviderForSpecWorkspace as no longer in active use, so the router's
	// LRU evictor / idle reaper may reclaim it. Pairs one-to-one with
	// ProviderForSpecWorkspace so a slow, in-use server is never evicted
	// mid-pass by another repo's concurrent spawn.
	ReleaseSpecWorkspace(name, workspace string)

	// Close shuts down every active provider. Called by Manager.Close.
	Close() error
}

// SupplementalProvider is an optional interface a Provider MAY
// implement to opt out of the per-language arbitration: instead of
// competing for a language slot it always runs (when available and not
// config-disabled) in addition to whichever provider won the slot.
// The in-process tree-sitter type resolvers implement it so they
// coexist with LSP / SCIP providers — their AST-grade provenance never
// downgrades a compiler-grade edge, and a language with no external
// tooling still gets type-aware enrichment.
type SupplementalProvider interface {
	Supplemental() bool
}

func isSupplemental(p Provider) bool {
	sp, ok := p.(SupplementalProvider)
	return ok && sp.Supplemental()
}

// Manager orchestrates multiple semantic providers and coordinates enrichment.
type Manager struct {
	providers []Provider
	config    Config
	logger    *zap.Logger

	// lspRouter, when non-nil, owns subprocess lifecycle for LSP
	// providers (idle reaper + LRU eviction + PATH-availability
	// cache). EnrichAll asks it for providers via ProviderForSpec
	// instead of holding hard references that would defeat reaping.
	lspRouter LSPRouter

	mu          sync.RWMutex
	lastResults map[string]*EnrichResult // provider name → last result
}

// NewManager creates a Manager from configuration.
// It registers providers based on config, probes availability, and logs results.
func NewManager(cfg Config, logger *zap.Logger) *Manager {
	m := &Manager{
		config:      cfg,
		logger:      logger,
		lastResults: make(map[string]*EnrichResult),
	}
	return m
}

// RegisterProvider adds a provider to the manager.
func (m *Manager) RegisterProvider(p Provider) {
	m.providers = append(m.providers, p)
	m.logger.Info("semantic provider registered",
		zap.String("name", p.Name()),
		zap.Strings("languages", p.Languages()),
		zap.Bool("available", p.Available()),
	)
}

// SetLSPRouter installs the daemon-managed LSP router. Once set,
// EnrichAll will lazy-spawn LSP providers via the router (allowing
// idle reaping + LRU eviction) instead of expecting them to be
// pre-registered via RegisterProvider. Pass nil to detach.
//
// Boot order matters — call SetLSPRouter before EnrichAll runs the
// first time. The router does not need to be populated yet; specs are
// resolved lazily via ProviderForSpec.
func (m *Manager) SetLSPRouter(r LSPRouter) {
	m.lspRouter = r
}

// LSPRouter returns the configured LSPRouter, or nil if none has been
// installed.
func (m *Manager) LSPRouter() LSPRouter {
	return m.lspRouter
}

// EnrichAll runs all available providers against the graph.
// For each language, only the highest-priority available provider runs.
func (m *Manager) EnrichAll(g graph.Store, roots map[string]string) ([]*EnrichResult, error) {
	if !m.config.Enabled {
		return nil, nil
	}

	// Build a map of language → sorted providers (by priority from config).
	// This covers SCIP / go-analysis / legacy LSP providers eagerly
	// registered via RegisterProvider.
	langProviders := m.selectProviders()

	// Languages actually present (in symbol-bearing nodes) across the repos
	// being enriched, from the indexed repo-scoped node scan. A provider — and
	// the LSP server spawn it would trigger — is skipped when none of its
	// languages are present, so a clangd / sourcekit / ruby-lsp never starts
	// for a repo that has no C / Swift / Ruby. This is the same condition the
	// per-provider EnrichRepo gate already applies, lifted ahead of the spawn.
	//
	// The gate fires only on POSITIVE evidence of absence: when the repo set
	// has indexed symbols (present is non-empty) but none in a provider's
	// languages. An empty / unindexed graph yields no evidence, so we don't
	// gate — providers fall through to their own per-pass gate as before.
	present := m.repoLanguages(g, roots)
	gateOnPresence := len(present) > 0
	if gateOnPresence {
		langs := make([]string, 0, len(present))
		for l := range present {
			langs = append(langs, l)
		}
		sort.Strings(langs)
		m.logger.Info("semantic enrichment: repo languages present",
			zap.Strings("languages", langs),
		)
	}

	var results []*EnrichResult

	for lang, provider := range langProviders {
		if !provider.Available() {
			m.logger.Debug("semantic provider unavailable, skipping",
				zap.String("provider", provider.Name()),
				zap.String("language", lang),
			)
			continue
		}
		if gateOnPresence && !anyLangPresent(provider.Languages(), present) {
			m.logger.Debug("semantic provider skipped, no nodes for its languages",
				zap.String("provider", provider.Name()),
				zap.String("language", lang),
			)
			continue
		}

		results = m.runEnrichForProvider(g, roots, lang, provider, results)
	}

	// Router-backed LSP providers: arbitrate by priority per language
	// BEFORE spawning so unused candidates never start a subprocess.
	// Two router specs claiming the same language pick the lowest
	// priority number; ties break by spec-name lexicographic order.
	// Eager providers from selectProviders already won their language;
	// router specs may only fill gaps.
	if m.lspRouter != nil {
		// Pre-pass: pure metadata, no spawn.
		bestSpec := make(map[string]string) // language → winning spec name
		bestPrio := make(map[string]int)
		for _, name := range m.lspRouter.EnabledSpecNames() {
			if !m.lspRouter.SpecAvailable(name) {
				continue
			}
			prio := m.lspRouter.SpecPriority(name)
			if cfgPrio, ok := m.configPriorityFor(name); ok {
				prio = cfgPrio
			}
			for _, lang := range m.lspRouter.SpecLanguages(name) {
				if _, eagerCovered := langProviders[lang]; eagerCovered {
					continue
				}
				// Only compete for a language the repo set actually contains —
				// a spec that wins no present language never enters runOrder and
				// so is never spawned via ProviderForSpec. Skipped when there is
				// no presence evidence at all (empty / unindexed graph).
				if gateOnPresence && !present[lang] {
					continue
				}
				cur, exists := bestSpec[lang]
				if !exists || prio < bestPrio[lang] || (prio == bestPrio[lang] && name < cur) {
					bestSpec[lang] = name
					bestPrio[lang] = prio
				}
			}
		}
		// One spec may win multiple languages — dedup so Enrich runs
		// once per spec, not once per (spec, language) pair.
		runOrder := make([]string, 0)
		seenSpec := make(map[string]bool)
		for _, name := range bestSpec {
			if seenSpec[name] {
				continue
			}
			seenSpec[name] = true
			runOrder = append(runOrder, name)
		}
		sort.Strings(runOrder)
		for _, name := range runOrder {
			// Fetch a provider per repo root (keyed by workspace) rather than
			// one shared default-workspace provider, so concurrent cross-repo
			// enrichment never shares a single LSP connection or document cache.
			for repoName, repoRoot := range roots {
				provider, err := m.lspRouter.ProviderForSpecWorkspace(name, repoRoot)
				if err != nil {
					m.logger.Debug("router-backed LSP provider unavailable, skipping",
						zap.String("spec", name),
						zap.String("repo", repoName),
						zap.Error(err),
					)
					continue
				}
				// ProviderForSpecWorkspace pinned the provider in-use; release
				// it once this repo's pass returns so the evictor can reclaim it.
				func() {
					defer m.lspRouter.ReleaseSpecWorkspace(name, repoRoot)
					langs := provider.Languages()
					if len(langs) == 0 {
						return
					}
					results = m.runEnrichOne(g, repoName, repoRoot, langs[0], provider, results)
				}()
			}
		}
	}

	// Supplemental providers run last, outside arbitration: they only
	// hold AST-grade provenance, so running after a compiler-grade
	// winner can confirm-but-never-downgrade what it stamped.
	for _, p := range m.providers {
		if !isSupplemental(p) || !p.Available() || m.providerDisabled(p.Name()) {
			continue
		}
		langs := p.Languages()
		if len(langs) == 0 {
			continue
		}
		if gateOnPresence && !anyLangPresent(langs, present) {
			continue
		}
		results = m.runEnrichForProvider(g, roots, langs[0], p, results)
	}

	return results, nil
}

// repoLanguages returns the union of languages present (in symbol-bearing
// nodes) across the given repo roots, computed from the indexed repo-scoped
// node scan. Used to skip providers — and the LSP server spawns they would
// trigger — for languages a repo set does not contain. Mirrors the
// node-language condition the per-provider EnrichRepo gate applies, so a
// provider is gated here exactly when its own pass would have found no work.
func (m *Manager) repoLanguages(g graph.Store, roots map[string]string) map[string]bool {
	present := make(map[string]bool)
	for repoPrefix := range roots {
		nodes := g.GetRepoNodes(repoPrefix)
		if len(nodes) == 0 && repoPrefix == "" {
			nodes = g.AllNodes()
		}
		for _, n := range nodes {
			// Include file/import nodes too: the per-provider EnrichRepo gate
			// can spawn on an ambiguous edge sourced from a file/import node, so
			// presence here must be at least as permissive — otherwise we would
			// gate out a provider whose own pass would have run.
			if n.RepoPrefix != repoPrefix || n.Language == "" {
				continue
			}
			// Generated / vendored files don't make a language "present" — a
			// repo whose only C is tree-sitter's generated parser.c should not
			// spawn clangd just to index it.
			if IsLowValueForEnrichment(n.FilePath, m.config.ExcludeGlobs) {
				continue
			}
			present[n.Language] = true
		}
	}
	return present
}

// anyLangPresent reports whether any of langs is in the present set.
func anyLangPresent(langs []string, present map[string]bool) bool {
	for _, l := range langs {
		if present[l] {
			return true
		}
	}
	return false
}

// providerDisabled reports an explicit `enabled: false` config entry
// for the named provider. Used by the supplemental run loop, which
// never passes through selectProviders' config gate.
func (m *Manager) providerDisabled(name string) bool {
	for _, pc := range m.config.Providers {
		if pc.Name == name {
			return !pc.Enabled
		}
	}
	return false
}

// configPriorityFor returns the user's config-overridden priority for
// the named provider, if any. Used to let `.gortex.yaml` take
// precedence over the spec's built-in default.
func (m *Manager) configPriorityFor(name string) (int, bool) {
	for _, pc := range m.config.Providers {
		if pc.Name == name && pc.Enabled {
			return pc.Priority, true
		}
	}
	return 0, false
}

// runEnrichForProvider executes Enrich for one provider against every
// repo root and appends the results. Extracted so EnrichAll can share
// the logging + lastResults bookkeeping between eager and Router-backed
// providers.
func (m *Manager) runEnrichForProvider(g graph.Store, roots map[string]string, lang string, provider Provider, results []*EnrichResult) []*EnrichResult {
	for repoName, repoRoot := range roots {
		results = m.runEnrichOne(g, repoName, repoRoot, lang, provider, results)
	}
	return results
}

// runEnrichOne runs one provider against one repo root and appends the
// result. Split out of runEnrichForProvider so the Router-backed path can
// fetch a per-repo provider instance (keyed by the repo's workspace) before
// dispatching — distinct providers per repo are what makes concurrent
// cross-repo enrichment safe.
func (m *Manager) runEnrichOne(g graph.Store, repoName, repoRoot, lang string, provider Provider, results []*EnrichResult) []*EnrichResult {
	start := time.Now()
	m.logger.Info("semantic enrichment starting",
		zap.String("provider", provider.Name()),
		zap.String("language", lang),
		zap.String("repo", repoName),
	)

	// repoName is the roots-map key. In multi-repo mode it carries the
	// repo prefix (the MultiIndexer keys roots by prefix; the per-repo
	// indexer passes its own RepoPrefix()); a repo-scoped provider uses
	// it to scope file selection to the repo actually being enriched.
	var result *EnrichResult
	var err error
	if rsp, ok := provider.(RepoScopedProvider); ok {
		result, err = rsp.EnrichRepo(g, repoName, repoRoot)
	} else {
		result, err = provider.Enrich(g, repoRoot)
	}
	if err != nil {
		m.logger.Warn("semantic enrichment failed",
			zap.String("provider", provider.Name()),
			zap.String("language", lang),
			zap.Error(err),
		)
		return results
	}

	if result != nil {
		result.DurationMs = time.Since(start).Milliseconds()
		results = append(results, result)

		m.mu.Lock()
		m.lastResults[provider.Name()] = result
		m.mu.Unlock()

		m.logger.Info("semantic enrichment complete",
			zap.String("provider", provider.Name()),
			zap.String("language", lang),
			zap.Int("confirmed", result.EdgesConfirmed),
			zap.Int("added", result.EdgesAdded),
			zap.Int("refuted", result.EdgesRefuted),
			zap.Int("nodes_enriched", result.NodesEnriched),
			zap.Float64("coverage", result.CoveragePercent),
			zap.Int64("duration_ms", result.DurationMs),
		)
	}
	return results
}

// EnrichFile runs incremental enrichment for a single file change.
func (m *Manager) EnrichFile(g graph.Store, repoRoot, filePath string) (*EnrichResult, error) {
	if !m.config.Enabled || !m.config.EnrichOnWatch {
		return nil, nil
	}

	langProviders := m.selectProviders()

	// Determine language from file nodes.
	nodes := g.GetFileNodes(filePath)
	if len(nodes) == 0 {
		return nil, nil
	}
	lang := nodes[0].Language

	var primary *EnrichResult
	var primaryErr error
	if provider, ok := langProviders[lang]; ok && provider.Available() {
		primary, primaryErr = provider.EnrichFile(g, repoRoot, filePath)
	}

	// Supplemental providers for this language run regardless of the
	// arbitration outcome — same contract as EnrichAll.
	for _, p := range m.providers {
		if !isSupplemental(p) || !p.Available() || m.providerDisabled(p.Name()) {
			continue
		}
		for _, l := range p.Languages() {
			if l != lang {
				continue
			}
			res, err := p.EnrichFile(g, repoRoot, filePath)
			if err != nil {
				m.logger.Debug("supplemental incremental enrichment failed",
					zap.String("provider", p.Name()),
					zap.String("file", filePath),
					zap.Error(err),
				)
				break
			}
			if primary == nil {
				primary = res
			}
			break
		}
	}

	return primary, primaryErr
}

// selectProviders returns the highest-priority available provider per language.
func (m *Manager) selectProviders() map[string]Provider {
	// Build priority map from config.
	type configEntry struct {
		name     string
		priority int
		enabled  bool
	}
	configMap := make(map[string]configEntry)
	for _, pc := range m.config.Providers {
		configMap[pc.Name] = configEntry{
			name:     pc.Name,
			priority: pc.Priority,
			enabled:  pc.Enabled,
		}
	}

	// Group providers by language with priority.
	type langCandidate struct {
		provider Provider
		priority int
	}
	langCandidates := make(map[string][]langCandidate)

	for _, p := range m.providers {
		// Supplemental providers never occupy a language slot — they
		// run unconditionally after arbitration (see EnrichAll), so a
		// router-backed LSP spec can still win the language.
		if isSupplemental(p) {
			continue
		}
		ce, ok := configMap[p.Name()]
		if ok && !ce.enabled {
			continue
		}
		priority := 99
		if ok {
			priority = ce.priority
		}
		for _, lang := range p.Languages() {
			langCandidates[lang] = append(langCandidates[lang], langCandidate{
				provider: p,
				priority: priority,
			})
		}
	}

	// Select highest-priority (lowest number) per language.
	result := make(map[string]Provider)
	for lang, candidates := range langCandidates {
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].priority < candidates[j].priority
		})
		result[lang] = candidates[0].provider
	}

	return result
}

// Stats returns the current status of all providers — eager
// (RegisterProvider) plus router-enabled LSP specs. Router specs are
// reported as "lsp-<spec-name>" for discoverability; status is "ready"
// when SpecAvailable is true and "unavailable" otherwise. No
// subprocess is spawned by Stats — pure read.
func (m *Manager) Stats() []ProviderStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	seen := make(map[string]bool)
	var statuses []ProviderStatus
	for _, p := range m.providers {
		seen[p.Name()] = true
		for _, lang := range p.Languages() {
			status := "unavailable"
			if p.Available() {
				status = "ready"
			}

			ps := ProviderStatus{
				Name:     p.Name(),
				Language: lang,
				Status:   status,
			}

			if lr, ok := m.lastResults[p.Name()]; ok {
				ps.CoveragePercent = lr.CoveragePercent
				ps.LastResult = lr
			}

			statuses = append(statuses, ps)
		}
	}
	if m.lspRouter != nil {
		for _, name := range m.lspRouter.EnabledSpecNames() {
			provName := "lsp-" + name
			// Skip when the eager path already registered an
			// identically-named provider (avoids double-counting
			// in legacy boot configurations).
			if seen[provName] {
				continue
			}
			status := "unavailable"
			if m.lspRouter.SpecAvailable(name) {
				status = "ready"
			}
			for _, lang := range m.lspRouter.SpecLanguages(name) {
				ps := ProviderStatus{
					Name:     provName,
					Language: lang,
					Status:   status,
				}
				if lr, ok := m.lastResults[provName]; ok {
					ps.CoveragePercent = lr.CoveragePercent
					ps.LastResult = lr
				}
				statuses = append(statuses, ps)
			}
		}
	}
	return statuses
}

// Close shuts down all providers, including any LSP subprocesses
// owned by the installed LSPRouter.
func (m *Manager) Close() error {
	var errs []error
	for _, p := range m.providers {
		if err := p.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing %s: %w", p.Name(), err))
		}
	}
	if m.lspRouter != nil {
		if err := m.lspRouter.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing lsp router: %w", err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("semantic manager close errors: %v", errs)
	}
	return nil
}

// Enabled returns whether semantic enrichment is enabled.
func (m *Manager) Enabled() bool {
	return m.config.Enabled
}

// HasProviders returns whether any providers are registered and available.
// Includes Router-enabled LSP specs — Router providers are spawned
// lazily but their availability is decided by exec.LookPath, so
// Router.EnabledSpecNames() seen-and-resolvable counts as "have one".
func (m *Manager) HasProviders() bool {
	for _, p := range m.providers {
		if p.Available() {
			return true
		}
	}
	if m.lspRouter != nil {
		for _, name := range m.lspRouter.EnabledSpecNames() {
			if m.lspRouter.SpecAvailable(name) {
				return true
			}
		}
	}
	return false
}

// AllProviders returns the unfiltered list of registered providers.
// Used by the daemon's LSP-action surface to find the right LSP
// provider for a file (call sites need the *lsp.Provider concrete
// type, so this stays untyped here and the caller does the type
// assertion against the lsp package).
func (m *Manager) AllProviders() []Provider {
	out := make([]Provider, len(m.providers))
	copy(out, m.providers)
	return out
}

// ProviderForLanguage returns the highest-priority registered provider
// for the given language code, or nil. The returned provider is the
// same one selectProviders would dispatch Enrich to.
func (m *Manager) ProviderForLanguage(lang string) Provider {
	if !m.config.Enabled {
		return nil
	}
	candidates := m.selectProviders()
	return candidates[lang]
}
