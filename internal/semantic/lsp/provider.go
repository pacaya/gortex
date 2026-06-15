package lsp

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/lspuri"
	"github.com/zzet/gortex/internal/semantic"
)

// Provider uses an LSP server for on-demand semantic queries.
type Provider struct {
	command string
	args    []string
	// env carries extra KEY=VALUE entries for the server subprocess,
	// from a .gortex.yaml override (e.g. JAVA_HOME for jdtls).
	env []string
	// workspaceFolders are additional roots advertised to the server's
	// initialize request alongside the primary workspace root.
	workspaceFolders []string
	languages        []string
	daemon           bool
	maxParallel      int
	logger           *zap.Logger
	// spec is the ServerSpec this provider was built from (when the
	// caller used NewProviderFromSpec). nil for legacy NewProvider
	// invocations — those fall back to single-language routing.
	spec *ServerSpec

	client *Client

	// sourceCache holds file contents read by openDocument so the
	// per-symbol column-resolution lookups don't reread the file
	// for every hover / references / implementation query. Keyed
	// by absolute path. Eviction is not implemented — the cache
	// lives only for the duration of one Enrich pass.
	sourceCache map[string][]byte

	// docMu guards docVersions / openDocs / lastDiag so concurrent
	// callers (LSP push notifications + MCP request goroutines) can
	// share one client safely.
	docMu       sync.RWMutex
	docVersions map[string]int          // absPath → most-recent didOpen / didChange version
	openDocs    map[string]bool         // absPath → already opened
	lastDiag    map[string][]Diagnostic // absPath → most recent diagnostics from publishDiagnostics

	// diagWaitersMu guards diagWaiters which lets sync code wait for
	// the next publishDiagnostics for a given file (e.g. fix-all
	// loops re-collecting diagnostics after each apply).
	diagWaitersMu sync.Mutex
	diagWaiters   map[string][]chan []Diagnostic

	// diagHookMu guards diagHook — a single persistent subscriber the
	// router (or any caller) can install to be notified on every
	// publishDiagnostics. The hook MUST be non-blocking; it runs on
	// the LSP client's message-pump goroutine.
	diagHookMu sync.RWMutex
	diagHook   func(absPath string, diags []Diagnostic)

	// capsMu guards caps and dynamicCaps. Read on every Supports()
	// call (hot path), so use RWMutex — register/unregister bursts
	// are rare relative to capability checks.
	capsMu sync.RWMutex
	// caps is the snapshot returned by the server's initialize reply
	// — what the server statically supports. Set once per subprocess
	// lifetime; reset to a zero value on respawn.
	caps ServerCapabilities
	// dynamicCaps holds capabilities the server announced lazily via
	// client/registerCapability. Keyed by Registration.ID (the wire
	// handle the server uses for unregisterCapability). Reset on
	// every ensureClient — a fresh subprocess starts with an empty
	// dynamic table and re-registers what it needs.
	dynamicCaps map[string]Registration

	// connect, when non-nil, switches ensureClient into passive-
	// attach mode: the Provider dials the configured endpoint
	// instead of spawning a subprocess. Reconnect on EOF retries
	// the dial with exponential backoff; fallback to spawn happens
	// only when connect.FallbackSpawn is true.
	connect *ConnectSpec
	// dialBackoff is the current backoff window between failed dial
	// attempts. Doubles on each failure (capped at maxDialBackoff)
	// and resets to dialBackoffStart on the first success.
	dialBackoff time.Duration

	// dialBackoffStart / maxDialBackoff are the per-instance bounds for
	// the reconnect-with-backoff loop (Enrich hover recovery). They
	// default to the package consts of the same name; tests pin them to
	// tiny values to keep recovery fast. dialOrSpawn keeps using the
	// package consts directly — only the mid-flight reconnect path reads
	// these fields.
	dialBackoffStart time.Duration
	maxDialBackoff   time.Duration

	// reconnectMu serialises mid-flight reconnection attempts so that
	// when many enrichment goroutines observe "LSP server exited" at the
	// same instant, exactly one of them rebuilds the client while the
	// others wait and then retry their hover against the fresh session.
	reconnectMu sync.Mutex
	// reconnectAttempts counts how many reconnect *cycles* ran across the
	// whole Enrich pass. Surfaced in the final metrics log.
	reconnectAttempts atomic.Int64

	// connectOnce establishes a fresh client connection (one attempt, no
	// backoff). Defaults to ensureClient. Injected in tests to swap in an
	// in-memory piped client instead of spawning a subprocess.
	connectOnce func(absRoot string) error
}

// Dial-retry constants for passive-attach reconnect. The window
// doubles on each failure and tops out at maxDialBackoff, then a
// successful dial resets it. Test fixtures can pin these via the
// helpers in the test file; production code uses the defaults.
const (
	dialBackoffStart = 100 * time.Millisecond
	maxDialBackoff   = 30 * time.Second
)

// NewProvider creates an LSP provider.
func NewProvider(command string, args []string, languages []string, daemon bool, maxParallel int, logger *zap.Logger) *Provider {
	if maxParallel <= 0 {
		maxParallel = 10
	}
	return &Provider{
		command:          command,
		args:             args,
		languages:        languages,
		daemon:           daemon,
		maxParallel:      maxParallel,
		logger:           logger,
		docVersions:      map[string]int{},
		openDocs:         map[string]bool{},
		lastDiag:         map[string][]Diagnostic{},
		diagWaiters:      map[string][]chan []Diagnostic{},
		dynamicCaps:      map[string]Registration{},
		dialBackoffStart: dialBackoffStart,
		maxDialBackoff:   maxDialBackoff,
	}
}

// NewProviderFromSpec builds a Provider directly from a ServerSpec.
// Mostly equivalent to NewProvider but lets the runtime router resolve
// the right `languageId` per file extension and pick the first
// available command from the spec's alternatives.
func NewProviderFromSpec(spec *ServerSpec, logger *zap.Logger) *Provider {
	cmd := spec.Command
	args := spec.Args
	if _, err := exec.LookPath(cmd); err != nil {
		for _, alt := range spec.AlternativeCommands {
			if _, err := exec.LookPath(alt.Command); err == nil {
				cmd = alt.Command
				args = alt.Args
				break
			}
		}
	}
	maxParallel := spec.MaxParallel
	if maxParallel <= 0 {
		maxParallel = 10
	}
	p := &Provider{
		command:          cmd,
		args:             args,
		env:              spec.Env,
		languages:        spec.Languages,
		daemon:           spec.Daemon,
		maxParallel:      maxParallel,
		logger:           logger,
		spec:             spec,
		docVersions:      map[string]int{},
		openDocs:         map[string]bool{},
		lastDiag:         map[string][]Diagnostic{},
		diagWaiters:      map[string][]chan []Diagnostic{},
		dynamicCaps:      map[string]Registration{},
		connect:          spec.Connect,
		dialBackoff:      dialBackoffStart,
		dialBackoffStart: dialBackoffStart,
		maxDialBackoff:   maxDialBackoff,
	}
	return p
}

func (p *Provider) Name() string        { return "lsp-" + p.command }
func (p *Provider) Languages() []string { return p.languages }

func (p *Provider) Available() bool {
	_, err := exec.LookPath(p.command)
	return err == nil
}

func (p *Provider) Close() error {
	if p.client != nil {
		return p.client.Shutdown()
	}
	return nil
}

func (p *Provider) Enrich(g graph.Store, repoRoot string) (*semantic.EnrichResult, error) {
	start := time.Now()

	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}

	// Start or reuse client.
	if err := p.ensureClient(absRoot); err != nil {
		return nil, fmt.Errorf("start LSP server: %w", err)
	}

	// Default the reconnect seam to the real ensureClient unless a test
	// injected its own (in-memory piped client). Set once per pass.
	if p.connectOnce == nil {
		p.connectOnce = p.ensureClient
	}
	// Reset the cross-pass reconnect counter so the metrics log reflects
	// only this Enrich invocation.
	p.reconnectAttempts.Store(0)

	result := &semantic.EnrichResult{
		Provider: p.Name(),
		Language: p.languages[0],
	}

	// Collect nodes that need enrichment (AMBIGUOUS or INFERRED edges).
	type enrichTarget struct {
		node *graph.Node
		edge *graph.Edge
	}

	var targets []enrichTarget
	for _, e := range g.AllEdges() {
		if e.Confidence >= 1.0 {
			continue
		}
		fromNode := g.GetNode(e.From)
		if fromNode == nil {
			continue
		}
		langMatch := false
		for _, lang := range p.languages {
			if fromNode.Language == lang {
				langMatch = true
				break
			}
		}
		if langMatch {
			targets = append(targets, enrichTarget{node: fromNode, edge: e})
		}
	}

	// Count total symbols.
	for _, n := range g.AllNodes() {
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		for _, lang := range p.languages {
			if n.Language == lang {
				result.SymbolsTotal++
				break
			}
		}
	}

	// Per-goroutine document lifecycle + bounded concurrency (spec
	// docs/spec-lsp-enrichment-hardening.md, Issues 1 & 2). The previous
	// implementation bulk-opened every target file up front and closed
	// them all in one deferred sweep after a fully sequential hover loop —
	// at peak that pinned tens of thousands of documents open in the
	// language server and OOM-killed it. Now each node is handled by its
	// own goroutine that opens its file, hovers, and closes it
	// immediately; a semaphore caps both the goroutine count and the
	// simultaneously-open documents at maxParallel.
	enrichedNodes := make(map[string]bool)
	// EnrichNodeMeta mutates Node.Meta in place; on disk backends n is a
	// per-call AllNodes reconstruction, so collect stamped nodes and
	// round-trip them through the store at the end or the semantic_type
	// stamp is discarded on the disk backend. See semantic.EnrichNodeMeta.
	var stampedNodes []*graph.Node

	// Race-safe metric counters for the concurrent hover phase.
	var diagTotalNodes, diagHoverOK, diagHoverErr, diagHoverNil, diagTypeEmpty, diagEnriched atomic.Int64

	// mu guards the cross-goroutine aggregation: stampedNodes,
	// enrichedNodes, the EnrichResult counters, and the best-effort
	// first-sample diagnostics below.
	var mu sync.Mutex
	var diagFirstHoverValue, diagFirstHoverError, diagFirstNodeName, diagFirstNodeFile string

	// activeClient is the client the hover goroutines currently target.
	// reconnectWithBackoff swaps it (under reconnectMu) when the server
	// dies mid-flight; goroutines load it atomically so the swap never
	// races an in-flight hover.
	var activeClient atomic.Pointer[Client]
	activeClient.Store(p.client)

	// Abort coordination: if reconnection fails permanently, the first
	// goroutine to learn it records the error and flips aborted; the rest
	// stop early and Enrich returns that error.
	var aborted atomic.Bool
	var abortErr error
	var abortOnce sync.Once

	// reconnect serialises mid-flight recovery so that when a burst of
	// goroutines observe the same dead client only the first rebuilds it;
	// the others wait on reconnectMu and then reuse the fresh client.
	reconnect := func(stale *Client) (*Client, error) {
		p.reconnectMu.Lock()
		defer p.reconnectMu.Unlock()
		if aborted.Load() {
			return nil, abortErr
		}
		if cur := activeClient.Load(); cur != stale {
			return cur, nil // someone else already reconnected
		}
		newC, err := p.reconnectWithBackoff(absRoot)
		if err != nil {
			abortOnce.Do(func() {
				abortErr = err
				aborted.Store(true)
			})
			return nil, err
		}
		activeClient.Store(newC)
		return newC, nil
	}

	// Open-document reference counting, keyed by (client, file): several
	// nodes can live in one file, but each client must see exactly one
	// didOpen / didClose per file (TestLSP_Provider_OpensEachFileOnce).
	// Keying by client is what makes reconnection strict — a reconnect's
	// fresh client starts with an empty open-set, so the first node to
	// touch a file after recovery re-opens it on the new session instead
	// of hovering against a document the dead session held. Peak open
	// files stays bounded by the distinct files in flight, itself bounded
	// by the semaphore at maxParallel.
	type enrichDocKey struct {
		c    *Client
		path string
	}
	openRefs := map[enrichDocKey]int{}
	var openMu sync.Mutex
	acquireDoc := func(c *Client, absPath string, content []byte) error {
		key := enrichDocKey{c: c, path: absPath}
		openMu.Lock()
		defer openMu.Unlock()
		if openRefs[key] == 0 {
			if err := p.enrichOpenDoc(c, absPath, content); err != nil {
				return err
			}
		}
		openRefs[key]++
		return nil
	}
	releaseDoc := func(c *Client, absPath string) {
		key := enrichDocKey{c: c, path: absPath}
		openMu.Lock()
		openRefs[key]--
		last := openRefs[key] <= 0
		if last {
			delete(openRefs, key)
		}
		openMu.Unlock()
		if last {
			_ = p.enrichCloseDoc(c, absPath)
		}
	}

	sem := make(chan struct{}, p.maxParallel)
	var wg sync.WaitGroup

	for _, n := range g.AllNodes() {
		if aborted.Load() {
			break
		}
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		langMatch := false
		for _, lang := range p.languages {
			if n.Language == lang {
				langMatch = true
				break
			}
		}
		if !langMatch {
			continue
		}

		diagTotalNodes.Add(1)
		wg.Add(1)
		sem <- struct{}{} // acquire — bounds concurrency AND open docs
		go func(n *graph.Node) {
			defer func() {
				<-sem
				wg.Done()
			}()
			if aborted.Load() {
				return
			}

			absPath := filepath.Join(absRoot, n.FilePath)
			content, err := os.ReadFile(absPath)
			if err != nil {
				p.logger.Debug("LSP enrich: read source failed",
					zap.String("file", n.FilePath), zap.Error(err))
				return
			}

			c := activeClient.Load()
			if err := acquireDoc(c, absPath, content); err != nil {
				p.logger.Debug("LSP enrich: didOpen failed",
					zap.String("file", n.FilePath), zap.Error(err))
				return
			}
			// Release (and close on the last node out) always, even on
			// hover error (acceptance criterion 2). Bound to the client we
			// opened on — args are captured here, so a later reconnect that
			// reassigns c does not redirect this close to the wrong session.
			defer releaseDoc(c, absPath)

			col := identifierColumn(content, n.StartLine, n.Name)
			hoverResult, err := p.hoverWith(c, absRoot, n.FilePath, n.StartLine-1, col)
			if err != nil && isServerExitError(err) {
				// Server died mid-flight — recover once and retry this
				// node's hover against the fresh session. The new client
				// has no record of our document, so re-open it there (and
				// close it on the way out) before retrying.
				newC, rerr := reconnect(c)
				if rerr != nil {
					return // aborted; wg.Wait + abort check below handles it
				}
				c = newC
				if err := acquireDoc(c, absPath, content); err != nil {
					p.logger.Debug("LSP enrich: reopen after reconnect failed",
						zap.String("file", n.FilePath), zap.Error(err))
					return
				}
				defer releaseDoc(c, absPath)
				hoverResult, err = p.hoverWith(c, absRoot, n.FilePath, n.StartLine-1, col)
			}
			if err != nil {
				diagHoverErr.Add(1)
				mu.Lock()
				if diagFirstHoverError == "" {
					diagFirstHoverError = err.Error()
					diagFirstNodeName = n.Name
					diagFirstNodeFile = n.FilePath
				}
				mu.Unlock()
				return
			}
			if hoverResult == nil {
				diagHoverNil.Add(1)
				return
			}
			diagHoverOK.Add(1)

			typeInfo := extractTypeFromHover(hoverResult.Contents.Value)
			mu.Lock()
			if diagFirstHoverValue == "" {
				diagFirstHoverValue = hoverResult.Contents.Value
				if len(diagFirstHoverValue) > 200 {
					diagFirstHoverValue = diagFirstHoverValue[:200]
				}
			}
			mu.Unlock()
			if typeInfo == "" {
				diagTypeEmpty.Add(1)
				return
			}

			semantic.EnrichNodeMeta(n, "semantic_type", typeInfo, p.Name())
			diagEnriched.Add(1)
			mu.Lock()
			stampedNodes = append(stampedNodes, n)
			if !enrichedNodes[n.ID] {
				result.NodesEnriched++
				result.SymbolsCovered++
				enrichedNodes[n.ID] = true
			}
			mu.Unlock()
		}(n)
	}
	wg.Wait()

	// Enrichment metrics (acceptance criterion 6).
	p.logger.Info("LSP enrich: hover phase complete",
		zap.Int64("total_nodes", diagTotalNodes.Load()),
		zap.Int64("hover_ok", diagHoverOK.Load()),
		zap.Int64("hover_err", diagHoverErr.Load()),
		zap.Int64("hover_nil", diagHoverNil.Load()),
		zap.Int64("type_empty", diagTypeEmpty.Load()),
		zap.Int64("enriched", diagEnriched.Load()),
		zap.Int64("reconnect_attempts", p.reconnectAttempts.Load()),
		zap.String("first_hover_value", diagFirstHoverValue),
		zap.String("first_hover_error", diagFirstHoverError),
		zap.String("first_node_name", diagFirstNodeName),
		zap.String("first_node_file", diagFirstNodeFile),
	)

	if aborted.Load() {
		return result, fmt.Errorf("LSP enrichment aborted: %w", abortErr)
	}

	if len(stampedNodes) > 0 {
		g.AddBatch(stampedNodes, nil)
	}

	// Query implementations for interface nodes.
	for _, n := range g.AllNodes() {
		if n.Kind != graph.KindInterface {
			continue
		}
		langMatch := false
		for _, lang := range p.languages {
			if n.Language == lang {
				langMatch = true
				break
			}
		}
		if !langMatch {
			continue
		}

		// Per-item doc lifecycle (no bulk pre-open): open this interface's
		// file, query, close immediately so memory stays bounded.
		if err := p.openDocument(absRoot, n.FilePath); err != nil {
			continue
		}
		col := identifierColumn(p.getSource(absRoot, n.FilePath), n.StartLine, n.Name)
		impls, err := p.findImplementations(absRoot, n.FilePath, n.StartLine-1, col)
		_ = p.closeDocument(filepath.Join(absRoot, n.FilePath))
		if err != nil || len(impls) == 0 {
			continue
		}

		for _, loc := range impls {
			implPath := uriToPath(loc.URI, absRoot)
			if implPath == "" {
				continue
			}
			implNode := semantic.MatchNodeByFileLine(g, implPath, loc.Range.Start.Line+1)
			if implNode == nil {
				continue
			}

			existing := semantic.FindMatchingEdge(g, implNode.ID, n.ID, graph.EdgeImplements)
			if existing != nil {
				if existing.Confidence < 1.0 {
					semantic.ConfirmEdge(existing, p.Name())
					result.EdgesConfirmed++
				}
			} else {
				semantic.AddSemanticEdge(g, implNode.ID, n.ID, graph.EdgeImplements,
					implNode.FilePath, implNode.StartLine, p.Name())
				result.EdgesAdded++
			}
		}
	}

	// Call hierarchy: ask gopls/jdtls/rust-analyzer/... for
	// outgoing calls per indexed function and use them to promote
	// existing call edges to lsp_resolved (or add edges that AST
	// extraction missed when the callee is in another file).
	p.enrichCallHierarchy(g, absRoot, result)

	// Type hierarchy: ask the server for super- and sub-types of
	// each indexed type/interface and emit EdgeExtends /
	// EdgeImplements / EdgeComposes — the single biggest non-Go
	// win because the AST extractor handles interface and type
	// inheritance with very low fidelity outside Go.
	p.enrichTypeHierarchy(g, absRoot, result)

	// Query references for AMBIGUOUS edges to confirm/refute.
	for _, t := range targets {
		toNode := g.GetNode(t.edge.To)
		if toNode == nil {
			continue
		}

		// Per-item doc lifecycle (no bulk pre-open): open the referent's
		// file, query, close immediately so memory stays bounded.
		if err := p.openDocument(absRoot, toNode.FilePath); err != nil {
			continue
		}
		col := identifierColumn(p.getSource(absRoot, toNode.FilePath), toNode.StartLine, toNode.Name)
		refs, err := p.findReferences(absRoot, toNode.FilePath, toNode.StartLine-1, col)
		_ = p.closeDocument(filepath.Join(absRoot, toNode.FilePath))
		if err != nil || len(refs) == 0 {
			continue
		}

		// Check if any reference matches the caller's location.
		confirmed := false
		for _, ref := range refs {
			refPath := uriToPath(ref.URI, absRoot)
			if refPath == t.node.FilePath &&
				ref.Range.Start.Line+1 >= t.node.StartLine &&
				ref.Range.Start.Line+1 <= t.node.EndLine {
				confirmed = true
				break
			}
		}

		if confirmed {
			semantic.ConfirmEdge(t.edge, p.Name())
			result.EdgesConfirmed++
		}
	}

	if result.SymbolsTotal > 0 {
		result.CoveragePercent = float64(result.SymbolsCovered) / float64(result.SymbolsTotal) * 100
	}

	result.DurationMs = time.Since(start).Milliseconds()
	return result, nil
}

func (p *Provider) EnrichFile(g graph.Store, repoRoot, filePath string) (*semantic.EnrichResult, error) {
	// LSP supports incremental updates, but for simplicity we skip it.
	// The full Enrich pass handles this.
	return nil, nil
}

// resetForReconnect clears the per-connection state that a dead client
// invalidated: open-document tracking, doc versions, and the dynamic
// capability table. The server (whether a freshly spawned subprocess
// or a freshly dialed IDE) has no knowledge of the documents we
// previously opened against the dead session — re-opening on first
// touch lets the next call (textDocument/hover etc.) succeed.
//
// Caps are reset separately at the top of ensureClient so the reset
// also covers the initial-connect path; here we only clear what the
// reconnect-specific recovery needs.
func (p *Provider) resetForReconnect() {
	// Drop the dead client so the next ensureClient branch builds a
	// fresh transport. Close it best-effort first to free any
	// pending pending-map entries; the dead read loop already closed
	// `done` so Shutdown() is a no-op past that point.
	if p.client != nil {
		_ = p.client.Shutdown()
		p.client = nil
	}
	p.docMu.Lock()
	p.docVersions = map[string]int{}
	p.openDocs = map[string]bool{}
	p.lastDiag = map[string][]Diagnostic{}
	p.docMu.Unlock()
}

// dialOrSpawn builds the LSP client according to the provider's spec.
// When p.connect is set, it dials the configured endpoint; on dial
// failure with FallbackSpawn=true it falls back to spawning the
// subprocess; with FallbackSpawn=false it returns the dial error so
// the language stays unavailable rather than racing the IDE.
//
// The dial path retries with exponential backoff capped at
// maxDialBackoff. Each successful dial resets the backoff window.
func (p *Provider) dialOrSpawn(workspaceRoot string) (*Client, error) {
	if p.connect != nil {
		if err := p.connect.Validate(); err != nil {
			return nil, fmt.Errorf("lsp passive attach: %w", err)
		}
		client, dialErr := NewClientWithTransport(&DialTransport{
			Network: p.connect.Network,
			Address: p.connect.Address,
		}, p.logger)
		if dialErr == nil {
			// Reset the backoff so a flap doesn't punish the next
			// reconnect with the last failure's window.
			p.dialBackoff = dialBackoffStart
			return client, nil
		}
		// Bump the backoff for the next attempt — callers that retry
		// immediately on failure can pace themselves via
		// SinceLastDialAttempt(), and the router's reaper / on-demand
		// callers naturally space attempts apart.
		nextBackoff := p.dialBackoff * 2
		if nextBackoff > maxDialBackoff {
			nextBackoff = maxDialBackoff
		}
		if nextBackoff <= 0 {
			nextBackoff = dialBackoffStart
		}
		p.dialBackoff = nextBackoff

		if !p.connect.FallbackSpawn {
			return nil, fmt.Errorf("lsp passive attach %s %s: %w (no spawn fallback configured)",
				p.connect.Network, p.connect.Address, dialErr)
		}
		// Fallback to spawn — log loudly so operators see the IDE
		// went away. The next ensureClient will retry dial first
		// (resetForReconnect clears p.client), so once the IDE
		// comes back we drift back to the passive path on next
		// reconnect.
		if p.logger != nil {
			p.logger.Warn("lsp: passive dial failed, falling back to spawn",
				zap.String("network", p.connect.Network),
				zap.String("address", p.connect.Address),
				zap.Error(dialErr),
			)
		}
	}
	if p.command == "" {
		return nil, fmt.Errorf("lsp: no command configured and no passive attach available")
	}
	return NewClient(p.command, p.args, p.env, workspaceRoot, p.logger)
}

// ensureClient starts the LSP server if not already running, OR
// reconnects if the previous client's transport went away (e.g. the
// IDE that owned a passive-attach LSP restarted, closing the socket).
//
// For spawn-mode providers this matches the original behaviour: first
// call spawns, subsequent calls are no-ops while the subprocess lives.
// For passive (connect) mode, a dead client triggers a re-dial with
// exponential backoff, falling back to spawn only if the spec's
// Connect.FallbackSpawn is true.
func (p *Provider) ensureClient(workspaceRoot string) error {
	// Liveness probe — if we have a client but its read loop has
	// terminated, the server (or socket) is gone. Treat as
	// disconnected: drop the dead handle and reset per-connection
	// state so the next branch can build a fresh transport.
	if p.client != nil {
		select {
		case <-p.client.Done():
			p.resetForReconnect()
		default:
			return nil
		}
	}

	// Reset the dynamic capability table — a fresh subprocess (or a
	// fresh dialed connection) has no dynamic registrations until it
	// re-announces them. Reset under the lock so any racing
	// Supports() reader sees a coherent state.
	p.capsMu.Lock()
	p.caps = ServerCapabilities{}
	p.dynamicCaps = map[string]Registration{}
	p.capsMu.Unlock()

	client, err := p.dialOrSpawn(workspaceRoot)
	if err != nil {
		return err
	}

	// Wire diagnostic + reverse-RPC handlers before initialize so we
	// don't lose the first publishDiagnostics burst that some servers
	// emit during workspace warmup.
	client.OnNotification("textDocument/publishDiagnostics",
		func(_ string, params json.RawMessage) {
			var pd PublishDiagnosticsParams
			if err := json.Unmarshal(params, &pd); err != nil {
				return
			}
			abs := uriToAbsPath(pd.URI)
			if abs == "" {
				return
			}
			p.docMu.Lock()
			p.lastDiag[abs] = pd.Diagnostics
			p.docMu.Unlock()
			p.fanoutDiagnostics(abs, pd.Diagnostics)
		})
	// Some servers (rust-analyzer, jdtls) emit progress / log messages
	// — silently swallow them; they're not actionable for the indexer.
	for _, m := range []string{
		"$/progress", "window/logMessage", "window/showMessage",
		"telemetry/event", "$/cancelRequest",
	} {
		client.OnNotification(m, func(_ string, _ json.RawMessage) {})
	}

	// Reply OK to common reverse-RPC requests so servers don't stall.
	// We never *need* to mutate workspace settings — saying "applied"
	// to applyEdit when we're an indexer is wrong, so we say no by
	// default and let the apply-code-action path opt in explicitly.
	client.OnRequest("workspace/configuration",
		func(_ string, _ json.RawMessage) (any, *jsonRPCError) {
			// Reply with one nil per requested item — servers that ask
			// for configuration treat null as "use defaults".
			return []any{nil}, nil
		})
	// client/registerCapability and client/unregisterCapability —
	// dynamic capability announcements. Some servers send these as
	// requests (with id, expecting a null ack); others send them as
	// notifications. We wire both forms to the same handlers so the
	// caps table converges regardless of which framing the server
	// chose. See applyRegistrations / applyUnregistrations.
	client.OnRequest("client/registerCapability",
		func(_ string, params json.RawMessage) (any, *jsonRPCError) {
			p.applyRegistrations(params)
			// LSP spec: reply with null (an empty success result).
			return nil, nil
		})
	client.OnRequest("client/unregisterCapability",
		func(_ string, params json.RawMessage) (any, *jsonRPCError) {
			p.applyUnregistrations(params)
			return nil, nil
		})
	client.OnNotification("client/registerCapability",
		func(_ string, params json.RawMessage) {
			p.applyRegistrations(params)
		})
	client.OnNotification("client/unregisterCapability",
		func(_ string, params json.RawMessage) {
			p.applyUnregistrations(params)
		})
	client.OnRequest("workspace/applyEdit",
		func(_ string, _ json.RawMessage) (any, *jsonRPCError) {
			// Default: refuse. The apply-code-action path swaps this
			// handler before issuing executeCommand so server-driven
			// applies land on disk via WriteWorkspaceEdit.
			return ApplyWorkspaceEditResponse{Applied: false, FailureReason: "applies are routed through gortex"}, nil
		})

	initParams := InitializeParams{
		ProcessID:        os.Getpid(),
		RootURI:          pathToURI(workspaceRoot),
		WorkspaceFolders: buildWorkspaceFolders(workspaceRoot, p.workspaceFolders),
		Capabilities: ClientCapabilities{
			Workspace: &WorkspaceClientCapabilities{
				ApplyEdit: true,
				WorkspaceEdit: &WorkspaceEditClientCapabilities{
					DocumentChanges:    true,
					ResourceOperations: []string{"create", "rename", "delete"},
				},
				ExecuteCommand:   &ExecuteCommandCapability{DynamicRegistration: true},
				WorkspaceFolders: true,
				Configuration:    true,
			},
			TextDocument: TextDocumentClientCapabilities{
				Synchronization: &SynchronizationCapability{DynamicRegistration: true},
				Implementation:  &ImplementationCapability{DynamicRegistration: true},
				References:      &ReferencesCapability{DynamicRegistration: true},
				Definition:      &DefinitionCapability{DynamicRegistration: true},
				Hover:           &HoverCapability{ContentFormat: []string{"plaintext"}},
				CallHierarchy:   &CallHierarchyCapability{DynamicRegistration: true},
				TypeHierarchy:   &TypeHierarchyCapability{DynamicRegistration: true},
				CodeAction: &CodeActionCapability{
					DynamicRegistration: true,
					CodeActionLiteralSupport: &CodeActionLiteralSupport{
						CodeActionKind: CodeActionKindCapability{
							ValueSet: []string{
								CodeActionKindEmpty,
								CodeActionKindQuickFix,
								CodeActionKindRefactor,
								CodeActionKindRefactorExtract,
								CodeActionKindRefactorInline,
								CodeActionKindRefactorRewrite,
								CodeActionKindSource,
								CodeActionKindSourceOrganizeImports,
								CodeActionKindSourceFixAll,
							},
						},
					},
					IsPreferredSupport: true,
				},
				PublishDiagnostics: &PublishDiagnosticsCapability{
					RelatedInformation: true,
					VersionSupport:     true,
				},
			},
		},
	}
	// Pass server-specific InitializationOptions (e.g. Maven/Gradle import
	// settings for jdtls) when the provider was built from a ServerSpec.
	if opts := effectiveInitializationOptions(p.spec); len(opts) > 0 {
		initParams.InitializationOptions = opts
	}

	var initResult InitializeResult
	if err := client.Call("initialize", initParams, &initResult); err != nil {
		_ = client.Shutdown()
		return fmt.Errorf("initialize: %w", err)
	}

	// Snapshot the server's static capabilities so Supports() can
	// answer "did the server advertise this at initialize time?".
	// Dynamic registrations may arrive any time after this point.
	p.capsMu.Lock()
	p.caps = initResult.Capabilities
	p.capsMu.Unlock()

	// Send initialized notification.
	if err := client.Notify("initialized", struct{}{}); err != nil {
		_ = client.Shutdown()
		return fmt.Errorf("initialized: %w", err)
	}

	p.client = client
	return nil
}

// fanoutDiagnostics wakes everyone who called WaitForDiagnostics for
// this absPath AND invokes the persistent hook installed via
// SetDiagnosticsHook (if any). Runs with no provider lock held.
//
// The hook MUST NOT block — this method runs on the LSP client's
// message-pump goroutine. The MCP-level wiring uses
// `SendNotificationToAllClients` which is non-blocking by design (the
// SDK drops to an error hook when a session's notification channel is
// full).
func (p *Provider) fanoutDiagnostics(absPath string, diags []Diagnostic) {
	p.diagWaitersMu.Lock()
	waiters := p.diagWaiters[absPath]
	delete(p.diagWaiters, absPath)
	p.diagWaitersMu.Unlock()
	for _, ch := range waiters {
		select {
		case ch <- diags:
		default:
		}
	}
	p.diagHookMu.RLock()
	hook := p.diagHook
	p.diagHookMu.RUnlock()
	if hook != nil {
		hook(absPath, diags)
	}
}

// SetDiagnosticsHook installs a persistent callback invoked for every
// `textDocument/publishDiagnostics` the LSP server emits for this
// provider. Pass nil to detach. The Router uses this to forward LSP
// diagnostics to MCP clients via `notifications/diagnostics`.
//
// The hook MUST NOT block — see fanoutDiagnostics doc.
func (p *Provider) SetDiagnosticsHook(hook func(absPath string, diags []Diagnostic)) {
	p.diagHookMu.Lock()
	p.diagHook = hook
	p.diagHookMu.Unlock()
}

// DiagnosticsSnapshot returns a copy of the most recent
// publishDiagnostics payload per absolute path. Used to replay current
// state to a freshly-subscribed MCP client so it doesn't have to wait
// for the next edit to learn what's currently broken.
//
// The map is a defensive copy — callers may mutate freely.
func (p *Provider) DiagnosticsSnapshot() map[string][]Diagnostic {
	p.docMu.RLock()
	defer p.docMu.RUnlock()
	out := make(map[string][]Diagnostic, len(p.lastDiag))
	for path, diags := range p.lastDiag {
		cp := make([]Diagnostic, len(diags))
		copy(cp, diags)
		out[path] = cp
	}
	return out
}

// uriToAbsPath converts a file:// URI to an absolute filesystem path.
// Returns "" for non-file URIs or malformed input.
func uriToAbsPath(uri string) string {
	return lspuri.URIToAbsPath(uri)
}

// openDocument sends textDocument/didOpen for a file. Tracks version
// 1 in docVersions so a later didChange can monotonically bump it.
// Idempotent — a second call to openDocument with the same path is a
// no-op.
func (p *Provider) openDocument(repoRoot, relPath string) error {
	absPath := filepath.Join(repoRoot, relPath)
	p.docMu.Lock()
	if p.openDocs[absPath] {
		p.docMu.Unlock()
		return nil
	}
	p.docMu.Unlock()

	content, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}
	if p.sourceCache == nil {
		p.sourceCache = map[string][]byte{}
	}
	p.sourceCache[absPath] = content

	langID := p.languageIDFor(absPath)

	if err := p.client.Notify("textDocument/didOpen", DidOpenTextDocumentParams{
		TextDocument: TextDocumentItem{
			URI:        pathToURI(absPath),
			LanguageID: langID,
			Version:    1,
			Text:       string(content),
		},
	}); err != nil {
		return err
	}
	p.docMu.Lock()
	p.openDocs[absPath] = true
	p.docVersions[absPath] = 1
	p.docMu.Unlock()
	return nil
}

// languageIDFor picks the LSP `languageId` to send in didOpen. When
// the provider was built from a ServerSpec, the spec's per-extension
// table wins; otherwise we fall back to the first configured language.
// Final fallback is the file's extension stripped of its leading dot.
func (p *Provider) languageIDFor(absPath string) string {
	if p.spec != nil {
		ext := strings.ToLower(filepath.Ext(absPath))
		if id, ok := p.spec.LanguageIDs[ext]; ok && id != "" {
			return id
		}
	}
	if len(p.languages) > 0 {
		return p.languages[0]
	}
	if ext := strings.ToLower(filepath.Ext(absPath)); ext != "" {
		return strings.TrimPrefix(ext, ".")
	}
	return ""
}

// changeDocument sends textDocument/didChange with a full-text replace
// and bumps the document's version monotonically.
func (p *Provider) changeDocument(absPath, newText string) error {
	p.docMu.Lock()
	v := p.docVersions[absPath] + 1
	p.docVersions[absPath] = v
	p.docMu.Unlock()
	if p.sourceCache == nil {
		p.sourceCache = map[string][]byte{}
	}
	p.sourceCache[absPath] = []byte(newText)
	return p.client.Notify("textDocument/didChange", DidChangeTextDocumentParams{
		TextDocument: VersionedTextDocumentIdentifier{
			URI:     pathToURI(absPath),
			Version: v,
		},
		ContentChanges: []TextDocumentContentChangeEvent{{Text: newText}},
	})
}

// closeDocument sends textDocument/didClose. Idempotent.
func (p *Provider) closeDocument(absPath string) error {
	p.docMu.Lock()
	if !p.openDocs[absPath] {
		p.docMu.Unlock()
		return nil
	}
	delete(p.openDocs, absPath)
	delete(p.docVersions, absPath)
	p.docMu.Unlock()
	return p.client.Notify("textDocument/didClose", DidCloseTextDocumentParams{
		TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
	})
}

// PushSimulatedContent sends a textDocument/didChange carrying
// `newText` as a full-text replacement, so the LSP server re-analyses
// the file as if it contained that buffer without anything ever
// touching disk. Used by the simulation engine (preview_edit /
// simulate_chain) to round-trip diagnostics for hypothetical edits.
// The caller is responsible for restoring the original on-disk
// content with a second PushSimulatedContent at simulation
// completion — otherwise other sessions that share this Provider
// will read diagnostics that reflect the simulated state instead of
// the saved file. EnsureFileOpen must be called first so the server
// has the document in its open-documents set; calling on an unopened
// path returns the underlying transport error.
func (p *Provider) PushSimulatedContent(absPath, newText string) error {
	return p.changeDocument(absPath, newText)
}

// LastDiagnostics returns the most recent diagnostics published for a
// file. Returns nil + false when the server has not (yet) emitted
// diagnostics for that path.
func (p *Provider) LastDiagnostics(absPath string) ([]Diagnostic, bool) {
	p.docMu.RLock()
	defer p.docMu.RUnlock()
	d, ok := p.lastDiag[absPath]
	if !ok {
		return nil, false
	}
	out := make([]Diagnostic, len(d))
	copy(out, d)
	return out, true
}

// WaitForDiagnostics blocks until the server publishes the next
// publishDiagnostics for absPath, or the timeout elapses (returning the
// last known diagnostics if any). Callers register their interest
// before triggering the change that will cause the publish, otherwise
// they may miss the event.
func (p *Provider) WaitForDiagnostics(absPath string, timeout time.Duration) []Diagnostic {
	ch := make(chan []Diagnostic, 1)
	p.diagWaitersMu.Lock()
	p.diagWaiters[absPath] = append(p.diagWaiters[absPath], ch)
	p.diagWaitersMu.Unlock()
	select {
	case d := <-ch:
		return d
	case <-time.After(timeout):
		// Drain & remove our waiter so we don't leak.
		p.diagWaitersMu.Lock()
		var kept []chan []Diagnostic
		for _, w := range p.diagWaiters[absPath] {
			if w != ch {
				kept = append(kept, w)
			}
		}
		p.diagWaiters[absPath] = kept
		p.diagWaitersMu.Unlock()
		if d, ok := p.LastDiagnostics(absPath); ok {
			return d
		}
		return nil
	}
}

// Client exposes the underlying LSP client for advanced callers (e.g.
// the daemon router). Returns nil before ensureClient succeeds.
func (p *Provider) Client() *Client { return p.client }

// EnsureClient is the exported form of ensureClient — it spawns the
// LSP server (idempotent) so callers that want diagnostics or code
// actions outside an Enrich pass can prime the connection on demand.
func (p *Provider) EnsureClient(workspaceRoot string) error {
	abs, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return err
	}
	return p.ensureClient(abs)
}

// EnsureFileOpen makes sure the document is opened on the server (with
// version 1) so request methods that take a position can proceed.
func (p *Provider) EnsureFileOpen(repoRoot, relPath string) error {
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return err
	}
	return p.openDocument(abs, relPath)
}

// getSource returns cached file content from the most recent
// openDocument call. Returns nil when not cached — callers fall
// back to col=0 then.
func (p *Provider) getSource(repoRoot, relPath string) []byte {
	if p.sourceCache == nil {
		return nil
	}
	return p.sourceCache[filepath.Join(repoRoot, relPath)]
}

// hoverWith issues textDocument/hover against an explicit client.
// PURPOSE — race-free per-goroutine hover during concurrent enrichment.
// RATIONALE — enrichment goroutines pass the client they captured
// atomically, so a concurrent reconnect that swaps p.client never races
// an in-flight hover (the goroutine holds its own pointer value).
// KEYWORDS — lsp, hover, concurrency, reconnect
func (p *Provider) hoverWith(c *Client, repoRoot, relPath string, line, col int) (*HoverResult, error) {
	absPath := filepath.Join(repoRoot, relPath)
	params := HoverParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
			Position:     Position{Line: line, Character: col},
		},
	}

	var result HoverResult
	if err := c.Call("textDocument/hover", params, &result); err != nil {
		return nil, err
	}
	if result.Contents.Value == "" {
		return nil, nil
	}
	return &result, nil
}

// enrichOpenDoc sends a bare textDocument/didOpen against an explicit
// client without touching the shared openDocs / sourceCache tables.
// PURPOSE — per-goroutine document open for the concurrent hover phase.
// RATIONALE — each enrichment goroutine owns its document's lifecycle, so
// it must not contend on the provider-wide doc maps; the matching close
// is enrichCloseDoc, always deferred.
// KEYWORDS — lsp, didOpen, enrichment, per-goroutine
func (p *Provider) enrichOpenDoc(c *Client, absPath string, content []byte) error {
	return c.Notify("textDocument/didOpen", DidOpenTextDocumentParams{
		TextDocument: TextDocumentItem{
			URI:        pathToURI(absPath),
			LanguageID: p.languageIDFor(absPath),
			Version:    1,
			Text:       string(content),
		},
	})
}

// enrichCloseDoc sends a bare textDocument/didClose against an explicit
// client — the counterpart to enrichOpenDoc.
// PURPOSE — release the server's per-document state immediately after a
// node's hover so simultaneously-open docs stay capped at maxParallel.
// RATIONALE — bulk-holding documents open was the enrichment OOM root
// cause; closing eagerly per goroutine bounds heap pressure.
// KEYWORDS — lsp, didClose, enrichment, lifecycle
func (p *Provider) enrichCloseDoc(c *Client, absPath string) error {
	return c.Notify("textDocument/didClose", DidCloseTextDocumentParams{
		TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
	})
}

// isServerExitError reports whether err signals that the language server
// process / transport is gone, so the enrichment loop should reconnect
// rather than keep hammering a dead session.
// PURPOSE — classify hover errors into "server died" vs "ordinary".
// RATIONALE — only transport/exit failures warrant a reconnect; protocol
// errors (e.g. an internal-error JSON-RPC reply) must not.
// KEYWORDS — lsp, reconnect, server-exit, error-classification
func isServerExitError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{
		"LSP server exited",
		"client is closed",
		"broken pipe",
		"connection reset",
		"use of closed network connection",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// reconnectWithBackoff rebuilds the LSP client after the server exits
// mid-enrichment. It retries connectOnce with exponential backoff
// (dialBackoffStart → maxDialBackoff) up to maxReconnectAttempts and
// returns the fresh client, or an error if every attempt failed.
// PURPOSE — automatic recovery so one mid-flight crash doesn't fail every
// remaining hover.
// RATIONALE — callers hold reconnectMu, so exactly one reconnection runs
// at a time; backoff prevents a tight retry loop against a persistently
// dead server.
// KEYWORDS — lsp, reconnect, backoff, recovery
func (p *Provider) reconnectWithBackoff(absRoot string) (*Client, error) {
	const maxReconnectAttempts = 5
	backoff := p.dialBackoffStart
	if backoff <= 0 {
		backoff = dialBackoffStart
	}
	maxBackoff := p.maxDialBackoff
	if maxBackoff <= 0 {
		maxBackoff = maxDialBackoff
	}
	var lastErr error
	for attempt := 1; attempt <= maxReconnectAttempts; attempt++ {
		p.reconnectAttempts.Add(1)
		p.logger.Warn("LSP enrich: reconnecting after server exit",
			zap.Int("attempt", attempt),
			zap.Int("max_attempts", maxReconnectAttempts),
		)
		if err := p.connectOnce(absRoot); err != nil {
			lastErr = err
			p.logger.Warn("LSP enrich: reconnect attempt failed",
				zap.Int("attempt", attempt), zap.Error(err))
			time.Sleep(backoff)
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		p.logger.Info("LSP enrich: reconnected", zap.Int("attempt", attempt))
		return p.client, nil
	}
	return nil, fmt.Errorf("LSP reconnect failed after %d attempts: %w", maxReconnectAttempts, lastErr)
}

// findImplementations queries textDocument/implementation.
func (p *Provider) findImplementations(repoRoot, relPath string, line, col int) ([]Location, error) {
	absPath := filepath.Join(repoRoot, relPath)
	params := ImplementationParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
			Position:     Position{Line: line, Character: col},
		},
	}

	var locations []Location
	if err := p.client.Call("textDocument/implementation", params, &locations); err != nil {
		return nil, err
	}
	return locations, nil
}

// CodeActionsRequest carries the params for a single
// textDocument/codeAction call.
type CodeActionsRequest struct {
	// AbsPath is the absolute path to the file the cursor is in.
	AbsPath string
	// Range narrows the request. Pass {} for the whole file.
	Range Range
	// Diagnostics is the set of diagnostics the actions should
	// address — typically a recent slice from LastDiagnostics.
	Diagnostics []Diagnostic
	// Only restricts the kind of actions returned (e.g.
	// CodeActionKindQuickFix, CodeActionKindSourceOrganizeImports).
	Only []string
}

// GetCodeActions issues textDocument/codeAction and returns a unified
// list of CodeActionOrCommand. The provider must already have opened
// the document via EnsureFileOpen before calling this.
func (p *Provider) GetCodeActions(req CodeActionsRequest) ([]CodeActionOrCommand, error) {
	if p.client == nil {
		return nil, fmt.Errorf("LSP client not initialized")
	}
	params := CodeActionParams{
		TextDocument: TextDocumentIdentifier{URI: pathToURI(req.AbsPath)},
		Range:        req.Range,
		Context: CodeActionContext{
			Diagnostics: req.Diagnostics,
			Only:        req.Only,
			TriggerKind: 1, // Invoked.
		},
	}
	var raw []json.RawMessage
	if err := p.client.Call("textDocument/codeAction", params, &raw); err != nil {
		return nil, err
	}
	out := make([]CodeActionOrCommand, 0, len(raw))
	for _, item := range raw {
		var u CodeActionOrCommand
		if err := json.Unmarshal(item, &u); err != nil {
			continue
		}
		// Legacy Command form has the shape {title, command, arguments}.
		// CodeAction literal has {title, kind?, edit?, command?, ...}.
		// json.Unmarshal handles both with the unified struct above.
		out = append(out, u)
	}
	return out, nil
}

// ResolveCodeAction calls codeAction/resolve. Some servers (rust-
// analyzer, jdtls) defer the heavy WorkspaceEdit computation until
// resolve to keep the initial codeAction call cheap.
func (p *Provider) ResolveCodeAction(action CodeActionOrCommand) (CodeActionOrCommand, error) {
	if p.client == nil {
		return action, fmt.Errorf("LSP client not initialized")
	}
	var resolved CodeActionOrCommand
	if err := p.client.Call("codeAction/resolve", action, &resolved); err != nil {
		return action, err
	}
	return resolved, nil
}

// ExecuteCommand issues workspace/executeCommand. Used by the
// apply-code-action path when a CodeAction has only a Command
// (legacy) form.
func (p *Provider) ExecuteCommand(cmd Command) (json.RawMessage, error) {
	if p.client == nil {
		return nil, fmt.Errorf("LSP client not initialized")
	}
	params := ExecuteCommandParams{Command: cmd.Command, Arguments: cmd.Arguments}
	var result json.RawMessage
	if err := p.client.Call("workspace/executeCommand", params, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// findReferences queries textDocument/references.
func (p *Provider) findReferences(repoRoot, relPath string, line, col int) ([]Location, error) {
	absPath := filepath.Join(repoRoot, relPath)
	params := ReferenceParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
			Position:     Position{Line: line, Character: col},
		},
		Context: ReferenceContext{IncludeDeclaration: false},
	}

	var locations []Location
	if err := p.client.Call("textDocument/references", params, &locations); err != nil {
		return nil, err
	}
	return locations, nil
}

// FindDefinition queries textDocument/definition with a per-call
// timeout so a stalled LSP can't block the resolve-time hot path.
// Returns the locations reported by the server (typically one) or
// an error / empty slice on timeout, no-result, or transport failure.
//
// repoRoot is the absolute workspace root; relPath is repo-relative.
// (line, col) are 0-based, matching LSP convention.
func (p *Provider) FindDefinition(repoRoot, relPath string, line, col int, timeout time.Duration) ([]Location, error) {
	if p.client == nil {
		return nil, fmt.Errorf("LSP client not initialised")
	}
	absPath := filepath.Join(repoRoot, relPath)
	params := TextDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
		Position:     Position{Line: line, Character: col},
	}

	type result struct {
		locations []Location
		err       error
	}
	ch := make(chan result, 1)
	go func() {
		// Tsserver replies with either a single Location, an array of
		// Location, or null. The unified handling: try array first
		// (most common), fall back to single Location on unmarshal
		// error.
		var raw json.RawMessage
		if err := p.client.Call("textDocument/definition", params, &raw); err != nil {
			ch <- result{nil, err}
			return
		}
		if len(raw) == 0 || string(raw) == "null" {
			ch <- result{nil, nil}
			return
		}
		var locs []Location
		if err := json.Unmarshal(raw, &locs); err == nil {
			ch <- result{locs, nil}
			return
		}
		var single Location
		if err := json.Unmarshal(raw, &single); err == nil {
			ch <- result{[]Location{single}, nil}
			return
		}
		ch <- result{nil, fmt.Errorf("unexpected definition response shape")}
	}()

	if timeout <= 0 {
		r := <-ch
		return r.locations, r.err
	}
	select {
	case r := <-ch:
		return r.locations, r.err
	case <-time.After(timeout):
		return nil, fmt.Errorf("textDocument/definition: timeout after %s", timeout)
	}
}

// IdentifierColumn is the exported form of the package-internal
// identifierColumn helper. Resolve-time callers (the resolver's LSP
// hot path) need the 0-based column for a given identifier on a
// given 1-based line to satisfy LSP servers that require the cursor
// to sit on the identifier.
func IdentifierColumn(src []byte, oneBasedLine int, name string) int {
	return identifierColumn(src, oneBasedLine, name)
}

// Source returns the cached source for relPath under repoRoot, or
// nil when the document has not been opened. Exported so the
// resolve-time helper can compute identifier columns without
// re-reading the file from disk.
func (p *Provider) Source(repoRoot, relPath string) []byte {
	return p.getSource(repoRoot, relPath)
}

// enrichCallHierarchy walks every function/method node in p.languages
// and uses callHierarchy/{prepare, outgoingCalls} to either promote a
// matching ast_inferred / text_matched EdgeCalls to lsp_resolved, or
// add a fresh EdgeCalls when the AST extractor missed the link
// (cross-file calls in languages without compile-unit info).
func (p *Provider) enrichCallHierarchy(g graph.Store, absRoot string, result *semantic.EnrichResult) {
	for _, n := range g.AllNodes() {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if !p.languageMatches(n.Language) {
			continue
		}
		col := identifierColumn(p.getSource(absRoot, n.FilePath), n.StartLine, n.Name)
		items, err := p.prepareCallHierarchy(absRoot, n.FilePath, n.StartLine-1, col)
		if err != nil || len(items) == 0 {
			continue
		}
		for _, item := range items {
			calls, err := p.outgoingCalls(item)
			if err == nil {
				for _, c := range calls {
					p.recordHierarchyCall(g, absRoot, n, c.To, true, result)
				}
			}
			incoming, err := p.incomingCalls(item)
			if err == nil {
				for _, c := range incoming {
					p.recordHierarchyCall(g, absRoot, n, c.From, false, result)
				}
			}
		}
	}
}

// recordHierarchyCall lands one call-hierarchy hop into the graph.
// asOutgoing=true means "this node calls other"; false means "other
// calls this node" (incoming-calls direction). Existing edges get
// promoted to lsp_resolved; missing edges get added.
func (p *Provider) recordHierarchyCall(g graph.Store, absRoot string, n *graph.Node, other CallHierarchyItem, asOutgoing bool, result *semantic.EnrichResult) {
	otherPath := uriToPath(other.URI, absRoot)
	if otherPath == "" {
		return
	}
	otherNode := semantic.MatchNodeByFileLine(g, otherPath,
		other.SelectionRange.Start.Line+1)
	if otherNode == nil {
		return
	}
	from, to := n, otherNode
	if !asOutgoing {
		from, to = otherNode, n
	}
	if from.ID == to.ID {
		return
	}
	existing := semantic.FindMatchingEdge(g, from.ID, to.ID, graph.EdgeCalls)
	if existing != nil {
		if graph.OriginRank(existing.Origin) < graph.OriginRank(graph.OriginLSPResolved) {
			semantic.ConfirmEdge(existing, p.Name())
			existing.Origin = graph.OriginLSPResolved
			result.EdgesConfirmed++
		}
		return
	}
	semantic.AddSemanticEdge(g, from.ID, to.ID, graph.EdgeCalls,
		from.FilePath, from.StartLine, p.Name())
	result.EdgesAdded++
}

// enrichTypeHierarchy walks every type / interface node and uses
// typeHierarchy/{prepare, supertypes, subtypes} to fill EdgeExtends
// / EdgeImplements / EdgeComposes for non-Go languages where AST
// extraction can't follow `extends X` / `implements I` across files.
//
//   - supertypes(T) = the parents T extends/implements. Emits
//     EdgeExtends T → super for class hierarchy and EdgeImplements
//     T → super when the super is an interface kind.
//   - subtypes(T) = the children of T. Emits EdgeImplements child
//     → T when T is an interface; EdgeExtends otherwise.
func (p *Provider) enrichTypeHierarchy(g graph.Store, absRoot string, result *semantic.EnrichResult) {
	for _, n := range g.AllNodes() {
		if n.Kind != graph.KindType && n.Kind != graph.KindInterface {
			continue
		}
		if !p.languageMatches(n.Language) {
			continue
		}
		col := identifierColumn(p.getSource(absRoot, n.FilePath), n.StartLine, n.Name)
		items, err := p.prepareTypeHierarchy(absRoot, n.FilePath, n.StartLine-1, col)
		if err != nil || len(items) == 0 {
			continue
		}
		for _, item := range items {
			supers, _ := p.supertypes(item)
			for _, s := range supers {
				p.linkTypeHierarchy(g, absRoot, n, s, true, result)
			}
			subs, _ := p.subtypes(item)
			for _, s := range subs {
				p.linkTypeHierarchy(g, absRoot, n, s, false, result)
			}
		}
	}
}

// linkTypeHierarchy emits the right edge kind for one super/subtype
// hop. When asSupertype=true, the hop is `cur → other` (cur extends
// or implements other). When false, the hop is `other → cur`.
//
// Beyond the type-level edge, it also walks the methods of the child
// type (the `from` side) and emits EdgeOverrides for every method
// whose name matches a method on the parent — closing the
// method-level half of the type hierarchy (Joern calls these
// CONTAINS + OVERRIDES).
func (p *Provider) linkTypeHierarchy(g graph.Store, absRoot string, cur *graph.Node, other TypeHierarchyItem, asSupertype bool, result *semantic.EnrichResult) {
	otherPath := uriToPath(other.URI, absRoot)
	if otherPath == "" {
		return
	}
	otherNode := semantic.MatchNodeByFileLine(g, otherPath, other.SelectionRange.Start.Line+1)
	if otherNode == nil {
		return
	}
	from, to := cur, otherNode
	if !asSupertype {
		from, to = otherNode, cur
	}
	kind := graph.EdgeExtends
	if to.Kind == graph.KindInterface {
		kind = graph.EdgeImplements
	}
	if from.ID == to.ID {
		return
	}
	existing := semantic.FindMatchingEdge(g, from.ID, to.ID, kind)
	if existing != nil {
		if graph.OriginRank(existing.Origin) < graph.OriginRank(graph.OriginLSPResolved) {
			semantic.ConfirmEdge(existing, p.Name())
			existing.Origin = graph.OriginLSPResolved
			result.EdgesConfirmed++
		}
	} else {
		semantic.AddSemanticEdge(g, from.ID, to.ID, kind, from.FilePath, from.StartLine, p.Name())
		result.EdgesAdded++
	}

	// Method-level override edges: child methods that share a name
	// with parent methods.
	addOverrideEdges(g, from, to, p.Name(), graph.OriginLSPDispatch, result)
}

// addOverrideEdges emits EdgeOverrides from each method of child to
// the matching method of parent (matched by name). Parent methods are
// resolved via EdgeMemberOf (`m -member_of-> parent`) so the routine
// works regardless of language as long as the AST extractor recorded
// member_of for methods.
//
// origin lets the caller stamp the edges with lsp_dispatch (LSP-
// confirmed parent), ast_resolved (AST-confirmed parent in the same
// compilation unit), or ast_inferred (parent is a heuristic match).
func addOverrideEdges(g graph.Store, child, parent *graph.Node, provider, origin string, result *semantic.EnrichResult) {
	if child == nil || parent == nil || child.ID == parent.ID {
		return
	}
	parentMethods := map[string]*graph.Node{}
	for _, e := range g.GetInEdges(parent.ID) {
		if e.Kind != graph.EdgeMemberOf {
			continue
		}
		m := g.GetNode(e.From)
		if m == nil || m.Kind != graph.KindMethod {
			continue
		}
		parentMethods[m.Name] = m
	}
	if len(parentMethods) == 0 {
		return
	}
	for _, e := range g.GetInEdges(child.ID) {
		if e.Kind != graph.EdgeMemberOf {
			continue
		}
		m := g.GetNode(e.From)
		if m == nil || m.Kind != graph.KindMethod {
			continue
		}
		pm, ok := parentMethods[m.Name]
		if !ok || pm.ID == m.ID {
			continue
		}
		existing := semantic.FindMatchingEdge(g, m.ID, pm.ID, graph.EdgeOverrides)
		if existing != nil {
			if graph.OriginRank(existing.Origin) < graph.OriginRank(origin) {
				semantic.ConfirmEdge(existing, provider)
				existing.Origin = origin
				if result != nil {
					result.EdgesConfirmed++
				}
			}
			continue
		}
		ed := semantic.AddSemanticEdge(g, m.ID, pm.ID, graph.EdgeOverrides, m.FilePath, m.StartLine, provider)
		if ed != nil {
			ed.Origin = origin
		}
		if result != nil {
			result.EdgesAdded++
		}
	}
}

// languageMatches returns true when n.Language is one of the
// languages this provider serves.
func (p *Provider) languageMatches(lang string) bool {
	for _, l := range p.languages {
		if l == lang {
			return true
		}
	}
	return false
}

// prepareCallHierarchy queries textDocument/prepareCallHierarchy and
// returns the items the server resolved at the given position. Empty
// (and nil error) means the server doesn't recognise a function-like
// symbol at that location.
func (p *Provider) prepareCallHierarchy(repoRoot, relPath string, line, col int) ([]CallHierarchyItem, error) {
	absPath := filepath.Join(repoRoot, relPath)
	params := CallHierarchyPrepareParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
			Position:     Position{Line: line, Character: col},
		},
	}
	var items []CallHierarchyItem
	if err := p.client.Call("textDocument/prepareCallHierarchy", params, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// outgoingCalls queries callHierarchy/outgoingCalls for one item.
func (p *Provider) outgoingCalls(item CallHierarchyItem) ([]CallHierarchyOutgoingCall, error) {
	var calls []CallHierarchyOutgoingCall
	if err := p.client.Call("callHierarchy/outgoingCalls",
		CallHierarchyOutgoingCallsParams{Item: item}, &calls); err != nil {
		return nil, err
	}
	return calls, nil
}

// incomingCalls queries callHierarchy/incomingCalls for one item.
func (p *Provider) incomingCalls(item CallHierarchyItem) ([]CallHierarchyIncomingCall, error) {
	var calls []CallHierarchyIncomingCall
	if err := p.client.Call("callHierarchy/incomingCalls",
		CallHierarchyIncomingCallsParams{Item: item}, &calls); err != nil {
		return nil, err
	}
	return calls, nil
}

// prepareTypeHierarchy queries textDocument/prepareTypeHierarchy.
func (p *Provider) prepareTypeHierarchy(repoRoot, relPath string, line, col int) ([]TypeHierarchyItem, error) {
	absPath := filepath.Join(repoRoot, relPath)
	params := TypeHierarchyPrepareParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: pathToURI(absPath)},
			Position:     Position{Line: line, Character: col},
		},
	}
	var items []TypeHierarchyItem
	if err := p.client.Call("textDocument/prepareTypeHierarchy", params, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// supertypes queries typeHierarchy/supertypes.
func (p *Provider) supertypes(item TypeHierarchyItem) ([]TypeHierarchyItem, error) {
	var items []TypeHierarchyItem
	if err := p.client.Call("typeHierarchy/supertypes",
		TypeHierarchySupertypesParams{Item: item}, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// subtypes queries typeHierarchy/subtypes.
func (p *Provider) subtypes(item TypeHierarchyItem) ([]TypeHierarchyItem, error) {
	var items []TypeHierarchyItem
	if err := p.client.Call("typeHierarchy/subtypes",
		TypeHierarchySubtypesParams{Item: item}, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// pathToURI converts a file path to a file:// URI (Windows-correct).
func pathToURI(path string) string {
	return lspuri.PathToURI(path)
}

// buildWorkspaceFolders returns the LSP workspaceFolders list — the
// primary root followed by any additional roots. It returns nil when
// there are no additional roots so rootUri-only servers are unaffected
// (the field is omitempty).
func buildWorkspaceFolders(primary string, additional []string) []WorkspaceFolder {
	if len(additional) == 0 {
		return nil
	}
	folders := make([]WorkspaceFolder, 0, len(additional)+1)
	folders = append(folders, WorkspaceFolder{
		URI:  pathToURI(primary),
		Name: filepath.Base(primary),
	})
	for _, f := range additional {
		if f == "" {
			continue
		}
		if abs, err := filepath.Abs(f); err == nil {
			f = abs
		}
		folders = append(folders, WorkspaceFolder{
			URI:  pathToURI(f),
			Name: filepath.Base(f),
		})
	}
	return folders
}

// uriToPath converts a file:// URI to a repo-relative path (Windows-correct).
func uriToPath(uri, repoRoot string) string {
	return lspuri.URIToRepoRel(uri, repoRoot)
}

// identifierColumn returns the 0-based column of the first
// occurrence of name on the given 1-based line of src. Returns 0
// when the source doesn't have the line, the name isn't found on
// it, or name is empty — col=0 was the previous unconditional
// default and remains a safe fallback for those edge cases.
//
// Why this matters: most LSP servers (gopls, jdtls, rust-analyzer,
// kotlin-ls, omnisharp, pyright) require the position cursor to be
// _on_ the identifier for textDocument/references and
// textDocument/implementation. Pinning to col=0 silently empty-resulted
// every method declaration in indented contexts (`func (f *Foo) Bar()`
// — col=0 is the `func` keyword, not `Bar`). Resolving to the actual
// identifier column unblocks the bulk of cross-file edge promotion.
func identifierColumn(src []byte, oneBasedLine int, name string) int {
	if name == "" || oneBasedLine <= 0 || len(src) == 0 {
		return 0
	}
	// Walk to the start of the requested line.
	target := oneBasedLine - 1
	lineStart := 0
	cur := 0
	for cur < len(src) && target > 0 {
		if src[cur] == '\n' {
			target--
			lineStart = cur + 1
		}
		cur++
	}
	if target > 0 {
		return 0
	}
	lineEnd := lineStart
	for lineEnd < len(src) && src[lineEnd] != '\n' {
		lineEnd++
	}
	line := string(src[lineStart:lineEnd])
	idx := strings.Index(line, name)
	if idx < 0 {
		return 0
	}
	return idx
}

// extractTypeFromHover extracts type information from hover text.
func extractTypeFromHover(hover string) string {
	// Remove markdown code fences.
	hover = strings.TrimPrefix(hover, "```go\n")
	hover = strings.TrimPrefix(hover, "```java\n")
	hover = strings.TrimPrefix(hover, "```\n")
	hover = strings.TrimSuffix(hover, "\n```")
	hover = strings.TrimSpace(hover)

	lines := strings.SplitN(hover, "\n", 2)
	if len(lines) > 0 {
		line := strings.TrimSpace(lines[0])
		// Go keywords
		if strings.HasPrefix(line, "func ") ||
			strings.HasPrefix(line, "type ") ||
			strings.HasPrefix(line, "var ") ||
			strings.HasPrefix(line, "const ") ||
			strings.HasPrefix(line, "field ") ||
			strings.HasPrefix(line, "package ") {
			return line
		}
		// Java keywords / modifiers — jdtls hover format:
		//   "public class Foo", "void bar()", "private String baz",
		//   "abstract class X", "interface Y", "@Deprecated",
		//   "static final int N", "enum Color", "protected Object"
		if strings.HasPrefix(line, "public ") ||
			strings.HasPrefix(line, "private ") ||
			strings.HasPrefix(line, "protected ") ||
			strings.HasPrefix(line, "abstract ") ||
			strings.HasPrefix(line, "static ") ||
			strings.HasPrefix(line, "final ") ||
			strings.HasPrefix(line, "class ") ||
			strings.HasPrefix(line, "interface ") ||
			strings.HasPrefix(line, "enum ") ||
			strings.HasPrefix(line, "void ") ||
			strings.HasPrefix(line, "@") {
			return line
		}
		// Short type like "string", "*Foo", "[]byte", "int", "boolean".
		if !strings.Contains(line, " ") && len(line) > 0 && len(line) < 100 {
			return line
		}
	}
	return ""
}
