package indexer

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
)

// ChangeKind describes the type of filesystem change.
type ChangeKind string

const (
	ChangeCreated  ChangeKind = "created"
	ChangeModified ChangeKind = "modified"
	ChangeDeleted  ChangeKind = "deleted"
	ChangeRenamed  ChangeKind = "renamed"
)

// GraphChangeEvent is emitted after a successful graph patch.
type GraphChangeEvent struct {
	FilePath     string     `json:"file_path"`
	Kind         ChangeKind `json:"kind"`
	NodesAdded   int        `json:"nodes_added"`
	NodesRemoved int        `json:"nodes_removed"`
	EdgesAdded   int        `json:"edges_added"`
	EdgesRemoved int        `json:"edges_removed"`
	Timestamp    time.Time  `json:"timestamp"`
	DurationMs   int64      `json:"duration_ms"`
}

// SymbolChangeCallback is called when symbols change during file re-indexing.
// It receives the file path, old symbols (before eviction), and new symbols (after re-index).
type SymbolChangeCallback func(filePath string, oldSymbols, newSymbols []*graph.Node)

// Watcher keeps the knowledge graph in live sync with the filesystem.
type Watcher struct {
	indexer          *Indexer
	fsw              *fsnotify.Watcher
	config           config.WatchConfig
	excludes         *excludes.Matcher
	events           chan GraphChangeEvent
	history          []GraphChangeEvent
	historyMu        sync.Mutex
	pending          map[string]*time.Timer
	mu               sync.Mutex
	logger           *zap.Logger
	done             chan struct{}
	stopped          chan struct{}
	symbolChangeCb   SymbolChangeCallback
	symbolChangeCbMu sync.RWMutex

	// Storm-mode state. Guarded by stormMu so the hot per-file
	// debounce path (mu) doesn't contend with rate-tracking.
	stormMu      sync.Mutex
	eventTimes   []time.Time           // sliding window of recent event timestamps
	stormBatch   map[string]ChangeKind // dirty set during an event storm
	stormTimer   *time.Timer           // fires after the quiet period
	stormActive  bool                  // true while waiting to drain
	stormDrained func(int)             // test hook: batch drained; batch size arg
}

const maxHistory = 1000

// NewWatcher creates a Watcher for the given indexer.
//
// cfg.Exclude is expected to carry the full effective pattern list (from
// ConfigManager.EffectiveExclude). If it is empty — e.g. a direct caller
// that bypasses ConfigManager — the watcher falls back to the builtin
// baseline so the obvious non-source dirs stay ignored.
func NewWatcher(idx *Indexer, cfg config.WatchConfig, logger *zap.Logger) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	debounce := cfg.DebounceMs
	if debounce <= 0 {
		debounce = 150
	}
	cfg.DebounceMs = debounce

	// Storm-mode defaults — kept conservative so a repo producing
	// normal save traffic stays on the per-file path. Threshold of
	// zero means the user explicitly disabled storm mode; negative is
	// coerced to zero for safety.
	if cfg.StormThreshold < 0 {
		cfg.StormThreshold = 0
	}
	if cfg.StormWindowMs <= 0 {
		cfg.StormWindowMs = 500
	}
	if cfg.StormQuietPeriodMs <= 0 {
		cfg.StormQuietPeriodMs = 500
	}

	patterns := cfg.Exclude
	if len(patterns) == 0 {
		patterns = excludes.Builtin
	}

	return &Watcher{
		indexer:    idx,
		fsw:        fsw,
		config:     cfg,
		excludes:   excludes.New(patterns),
		events:     make(chan GraphChangeEvent, 64),
		pending:    make(map[string]*time.Timer),
		stormBatch: make(map[string]ChangeKind),
		logger:     logger,
		done:       make(chan struct{}),
		stopped:    make(chan struct{}),
	}, nil
}

// Start begins watching the given paths recursively.
func (w *Watcher) Start(paths []string) error {
	for _, p := range paths {
		absPath, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		if err := w.addRecursive(absPath); err != nil {
			return err
		}
	}

	go w.loop()
	return nil
}

// Stop halts the watcher and cleans up resources.
func (w *Watcher) Stop() error {
	close(w.done)
	err := w.fsw.Close()
	<-w.stopped
	return err
}

// Events returns a read-only channel of graph change events.
func (w *Watcher) Events() <-chan GraphChangeEvent {
	return w.events
}

// History returns recent change events (up to maxHistory).
func (w *Watcher) History() []GraphChangeEvent {
	w.historyMu.Lock()
	defer w.historyMu.Unlock()
	out := make([]GraphChangeEvent, len(w.history))
	copy(out, w.history)
	return out
}

// HistorySince returns change events after the given timestamp.
func (w *Watcher) HistorySince(since time.Time) []GraphChangeEvent {
	w.historyMu.Lock()
	defer w.historyMu.Unlock()
	var out []GraphChangeEvent
	for _, ev := range w.history {
		if ev.Timestamp.After(since) {
			out = append(out, ev)
		}
	}
	return out
}

// OnSymbolChange registers a callback that is invoked when symbols change
// during file re-indexing. The callback receives old symbols (before eviction)
// and new symbols (after re-index).
func (w *Watcher) OnSymbolChange(cb SymbolChangeCallback) {
	w.symbolChangeCbMu.Lock()
	defer w.symbolChangeCbMu.Unlock()
	w.symbolChangeCb = cb
}

func (w *Watcher) loop() {
	defer close(w.stopped)
	for {
		select {
		case <-w.done:
			return
		case event, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handleEvent(event)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			w.logger.Warn("watcher error", zap.Error(err))
		}
	}
}

func (w *Watcher) handleEvent(event fsnotify.Event) {
	path := event.Name

	// Skip events from excluded paths. A single matcher call covers
	// what the old code split across inExcludedDir + isExcluded.
	if w.isExcluded(path) {
		return
	}

	// If a new directory is created, watch it recursively.
	if event.Has(fsnotify.Create) {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			_ = w.addRecursive(path)
			return
		}
	}

	// Only process files with known extensions.
	if _, ok := w.indexer.registry.DetectLanguage(path); !ok {
		// Still handle remove for previously indexed files.
		if !event.Has(fsnotify.Remove) && !event.Has(fsnotify.Rename) {
			return
		}
	}

	var kind ChangeKind
	switch {
	case event.Has(fsnotify.Create):
		kind = ChangeCreated
	case event.Has(fsnotify.Write):
		kind = ChangeModified
	case event.Has(fsnotify.Remove):
		kind = ChangeDeleted
	case event.Has(fsnotify.Rename):
		kind = ChangeRenamed
	default:
		return
	}

	// Storm mode — if more than StormThreshold events arrived within
	// StormWindowMs, skip the per-file debounced path and accumulate
	// into a batch. The batch drains once StormQuietPeriodMs has
	// passed with no further events.
	if w.shouldEnterStorm() {
		w.recordInStorm(path, kind)
		return
	}

	// Debounce: reset or start timer for this file.
	w.mu.Lock()
	if timer, exists := w.pending[path]; exists {
		timer.Stop()
	}
	debounce := time.Duration(w.config.DebounceMs) * time.Millisecond
	w.pending[path] = time.AfterFunc(debounce, func() {
		w.patchGraph(path, kind)
		w.mu.Lock()
		delete(w.pending, path)
		w.mu.Unlock()
	})
	w.mu.Unlock()
}

// shouldEnterStorm records the current event in the rate window and
// reports whether the watcher is over threshold. Returns false when
// storm mode is disabled (threshold <= 0). The returned-true path
// guarantees the caller will enqueue to the batch, so any single
// event that crosses the threshold is captured correctly.
func (w *Watcher) shouldEnterStorm() bool {
	if w.config.StormThreshold <= 0 {
		return false
	}
	now := time.Now()
	window := time.Duration(w.config.StormWindowMs) * time.Millisecond
	cutoff := now.Add(-window)

	w.stormMu.Lock()
	defer w.stormMu.Unlock()
	// Already batching — stay in storm until the drain completes.
	if w.stormActive {
		return true
	}
	// Drop timestamps older than the window. The slice is append-only
	// so a linear scan from the front is the minimal thing that
	// works; the window is O(threshold) bounded in steady state.
	trimFrom := 0
	for i, t := range w.eventTimes {
		if t.After(cutoff) {
			trimFrom = i
			break
		}
		trimFrom = i + 1
	}
	if trimFrom > 0 {
		w.eventTimes = w.eventTimes[trimFrom:]
	}
	w.eventTimes = append(w.eventTimes, now)
	return len(w.eventTimes) > w.config.StormThreshold
}

// recordInStorm adds the event to the pending batch and resets the
// drain timer. Repeated create/modify collapse to a single patch; a
// later delete of the same path overwrites an earlier create so the
// drain does the right final thing (treats the path as deleted).
func (w *Watcher) recordInStorm(path string, kind ChangeKind) {
	w.stormMu.Lock()
	defer w.stormMu.Unlock()
	w.stormActive = true
	// Cancel any pending per-file timers for this path — storm mode
	// takes over.
	w.mu.Lock()
	if timer, exists := w.pending[path]; exists {
		timer.Stop()
		delete(w.pending, path)
	}
	w.mu.Unlock()
	w.stormBatch[path] = kind

	quiet := time.Duration(w.config.StormQuietPeriodMs) * time.Millisecond
	if w.stormTimer != nil {
		w.stormTimer.Stop()
	}
	w.stormTimer = time.AfterFunc(quiet, w.drainStorm)
}

// drainStorm processes every path accumulated during the storm as a
// single batch: per-path evict/index with the resolver stage skipped,
// then one global ResolveAll at the end. Cuts a 500-file checkout
// from "resolver runs 500 times" to "resolver runs once."
func (w *Watcher) drainStorm() {
	w.stormMu.Lock()
	batch := w.stormBatch
	w.stormBatch = make(map[string]ChangeKind)
	w.eventTimes = nil
	w.stormActive = false
	drained := w.stormDrained
	w.stormMu.Unlock()

	if len(batch) == 0 {
		return
	}
	start := time.Now()
	w.logger.Info("watcher: storm drain starting", zap.Int("paths", len(batch)))

	for path, kind := range batch {
		w.patchGraphNoResolve(path, kind)
	}
	w.indexer.ResolveAll()

	w.logger.Info("watcher: storm drain complete",
		zap.Int("paths", len(batch)),
		zap.Duration("elapsed", time.Since(start)))
	if drained != nil {
		drained(len(batch))
	}
}

// patchGraphNoResolve is patchGraph for the batched path: same evict
// / index dispatch, but without per-file resolver work. The caller is
// responsible for running indexer.ResolveAll() after the batch.
func (w *Watcher) patchGraphNoResolve(path string, kind ChangeKind) {
	switch kind {
	case ChangeCreated, ChangeModified:
		if err := w.indexer.IndexFileNoResolve(path); err != nil {
			w.logger.Warn("storm: index file failed",
				zap.String("path", path), zap.Error(err))
		}
	case ChangeDeleted, ChangeRenamed:
		w.indexer.EvictFile(path)
	}
}

func (w *Watcher) patchGraph(path string, kind ChangeKind) {
	start := time.Now()
	var nodesAdded, nodesRemoved, edgesAdded, edgesRemoved int

	nodesBefore := w.indexer.graph.NodeCount()
	edgesBefore := w.indexer.graph.EdgeCount()

	// Compute the relative path for snapshotting old symbols.
	relPath := path
	if w.indexer.rootPath != "" {
		if rp, err := filepath.Rel(w.indexer.rootPath, path); err == nil {
			relPath = rp
		}
	}

	switch kind {
	case ChangeCreated:
		if err := w.indexer.IndexFile(path); err != nil {
			w.logger.Warn("index file failed", zap.String("path", path), zap.Error(err))
			return
		}
		nodesAdded = w.indexer.graph.NodeCount() - nodesBefore
		edgesAdded = w.indexer.graph.EdgeCount() - edgesBefore

		// Notify callback: no old symbols, only new symbols.
		w.symbolChangeCbMu.RLock()
		cb := w.symbolChangeCb
		w.symbolChangeCbMu.RUnlock()
		if cb != nil {
			newSymbols := w.indexer.graph.GetFileNodes(relPath)
			cb(relPath, nil, newSymbols)
		}

	case ChangeModified:
		// Snapshot old symbols before eviction.
		oldSymbols := w.snapshotSymbols(relPath)

		nr, er := w.indexer.EvictFile(path)
		nodesRemoved = nr
		edgesRemoved = er
		if err := w.indexer.IndexFile(path); err != nil {
			w.logger.Warn("reindex file failed", zap.String("path", path), zap.Error(err))
			return
		}
		nodesAdded = w.indexer.graph.NodeCount() - (nodesBefore - nr)
		edgesAdded = w.indexer.graph.EdgeCount() - (edgesBefore - er)

		// Notify callback with old and new symbols.
		w.symbolChangeCbMu.RLock()
		cb := w.symbolChangeCb
		w.symbolChangeCbMu.RUnlock()
		if cb != nil {
			newSymbols := w.indexer.graph.GetFileNodes(relPath)
			cb(relPath, oldSymbols, newSymbols)
		}

	case ChangeDeleted, ChangeRenamed:
		// Snapshot old symbols before eviction.
		oldSymbols := w.snapshotSymbols(relPath)

		nr, er := w.indexer.EvictFile(path)
		nodesRemoved = nr
		edgesRemoved = er

		// Notify callback: old symbols removed, no new symbols.
		w.symbolChangeCbMu.RLock()
		cb := w.symbolChangeCb
		w.symbolChangeCbMu.RUnlock()
		if cb != nil {
			cb(relPath, oldSymbols, nil)
		}
	}

	ev := GraphChangeEvent{
		FilePath:     path,
		Kind:         kind,
		NodesAdded:   nodesAdded,
		NodesRemoved: nodesRemoved,
		EdgesAdded:   edgesAdded,
		EdgesRemoved: edgesRemoved,
		Timestamp:    time.Now(),
		DurationMs:   time.Since(start).Milliseconds(),
	}

	w.historyMu.Lock()
	w.history = append(w.history, ev)
	if len(w.history) > maxHistory {
		w.history = w.history[len(w.history)-maxHistory:]
	}
	w.historyMu.Unlock()

	// Non-blocking send.
	select {
	case w.events <- ev:
	default:
	}

	w.logger.Info("graph patch",
		zap.String("kind", string(kind)),
		zap.String("file", path),
		zap.Int("nodes+", nodesAdded),
		zap.Int("nodes-", nodesRemoved),
		zap.Int("edges+", edgesAdded),
		zap.Int("edges-", edgesRemoved),
		zap.Int64("ms", ev.DurationMs),
	)
}

// snapshotSymbols returns a deep copy of the symbols for a file, preserving
// their signatures in Meta so they can be compared after re-indexing.
func (w *Watcher) snapshotSymbols(relPath string) []*graph.Node {
	nodes := w.indexer.graph.GetFileNodes(relPath)
	snapshot := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		// Skip file and import nodes — we only track code symbols.
		if n.Kind == graph.KindFile || n.Kind == graph.KindImport {
			continue
		}
		cp := &graph.Node{
			ID:       n.ID,
			Kind:     n.Kind,
			Name:     n.Name,
			QualName: n.QualName,
			FilePath: n.FilePath,
		}
		if sig, ok := n.Meta["signature"]; ok {
			cp.Meta = map[string]any{"signature": sig}
		}
		snapshot = append(snapshot, cp)
	}
	return snapshot
}

// addRecursive walks the tree under root and registers an fsnotify watch
// for every directory that contains at least one indexable file. Empty
// directories and trees full of binary/doc files (where no parser claims
// any of the contents) are skipped — that's where the savings come from
// on huge repos. Linux's default inotify limit is 8192 user watches; a
// 200k-file monorepo can have 50k+ subdirectories, but typically only a
// fraction host code we care about.
//
// Errors from fsw.Add (most commonly ENOSPC when the inotify limit is
// exhausted) are counted but never bubble up — a partial watch is still
// useful, and aborting the walk would leave the user with no watcher at
// all. A single warning summarises the skipped count plus the OS-level
// hint for raising the limit.
func (w *Watcher) addRecursive(root string) error {
	needed := make(map[string]struct{})
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if w.isExcluded(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if w.isExcluded(path) {
			return nil
		}
		// File: only mark the parent as needing a watch when a parser
		// claims this extension. Avoids subscribing to dirs full of
		// PNGs, fixtures, generated assets, etc.
		if _, ok := w.indexer.registry.DetectLanguage(path); ok {
			needed[filepath.Dir(path)] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return err
	}

	var watched, skipped int
	var firstErr error
	for dir := range needed {
		if addErr := w.fsw.Add(dir); addErr != nil {
			skipped++
			if firstErr == nil {
				firstErr = addErr
			}
			continue
		}
		watched++
	}
	if skipped > 0 && w.logger != nil {
		w.logger.Warn("watcher: some directories could not be watched",
			zap.Int("skipped", skipped),
			zap.Int("watched", watched),
			zap.Error(firstErr),
			zap.String("hint", "Linux: bump fs.inotify.max_user_watches; macOS: usually no limit"))
	}
	return nil
}

// isExcluded reports whether path is excluded by the effective pattern list.
func (w *Watcher) isExcluded(path string) bool {
	root := w.indexer.rootPath
	if root == "" {
		return w.excludes.MatchRel(filepath.Base(path))
	}
	return w.excludes.MatchAbs(path, root)
}
