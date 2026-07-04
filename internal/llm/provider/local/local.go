//go:build llama

// Package local is the in-process llama.cpp llm.Provider. It wraps the
// CGO model/context from package llm and is the only provider that
// needs a `-tags llama` build; the non-llama build (stub.go) compiles
// a New that reports the provider as unavailable.
package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/llm"
)

// assistCtxSize is the KV-cache window for the short-call assist
// context (expand / rerank / verify). Sized for the heaviest user —
// verify with body + callers at ~3.5K tokens for 10 candidates.
const assistCtxSize = 4096

// defaultMaxTokens caps a Complete call whose request leaves
// MaxTokens unset.
const defaultMaxTokens = 512

// Provider is the local llama.cpp implementation of llm.Provider.
//
// It keeps two llama contexts behind separate mutexes: a small
// assistCtx for the structured search-assist shapes, and a full-size
// mainCtx for the agent tool-loop (ShapeToolCall) and freeform
// generation. Splitting them means a long agent run can't head-of-line
// block a hot-path assist call — at the llama.cpp level both share the
// model weights, each holds its own KV cache.
//
// Every Complete call is self-contained: it resets the context's KV
// cache and prefills the entire conversation passed in the request, so
// no cross-call state lives in a context. That makes per-call locking
// (rather than per-agent-run locking) correct even under concurrent
// callers.
//
// The model (a ~4 GiB mmap) is loaded lazily on the first Complete and,
// when GORTEX_LLM_IDLE_TTL is enabled, unloaded again after it sits idle
// — freeing Metal/RAM the daemon doesn't need between bursts of use. The
// idleGate serialises load/unload against in-flight calls so an idle
// reap can never free the model out from under an active Complete; a
// later Complete transparently reloads. All of this concurrency logic
// lives in the untagged gate (gate.go); this file only supplies the
// CGO-touching load/unload hooks.
type Provider struct {
	cfg  llm.LocalConfig
	tmpl chatTemplate

	gate   *idleGate
	ticker *idleTicker
	ttl    time.Duration

	logMu  sync.Mutex
	logger *zap.Logger

	// Resource handles. Written only by loadResources / unloadResources
	// (both invoked under gate.mu); read by Complete while it holds a
	// gate refcount, so no read ever races the write that (re)allocates
	// them. The per-context mutexes serialise the two concurrent shapes.
	model     *llm.Model
	assistMu  sync.Mutex
	assistCtx *llm.Context
	mainMu    sync.Mutex
	mainCtx   *llm.Context
}

// compile-time assertion that *Provider satisfies the interface.
var _ llm.Provider = (*Provider)(nil)

// New constructs the local provider from its config sub-block. The
// model is NOT loaded here — that happens lazily on the first Complete
// call so daemon startup isn't slowed. New only validates that a model
// path is set and the file exists, and that the chat template is
// known, so misconfiguration surfaces immediately.
//
// Returns the llm.Provider interface (not the concrete *Provider) so
// the signature matches the non-llama stub and the provider factory
// can treat both builds uniformly.
func New(cfg llm.LocalConfig) (llm.Provider, error) {
	path := strings.TrimSpace(cfg.Model)
	if path == "" {
		return nil, errors.New("local: llm.local.model is empty")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("local: model file: %w", err)
	}
	tmpl, err := templateByName(cfg.Template)
	if err != nil {
		return nil, err
	}
	if cfg.Ctx <= 0 {
		cfg.Ctx = 4096
	}
	p := &Provider{cfg: cfg, tmpl: tmpl, ttl: idleTTLFromEnv()}
	p.gate = newIdleGate(p.loadResources, p.unloadResources)
	// The idle reaper only runs when a TTL is configured; a disabled TTL
	// (GORTEX_LLM_IDLE_TTL=0/off/none) keeps the model resident once
	// loaded, matching the original always-loaded behaviour.
	if p.ttl > 0 {
		p.ticker = startIdleTicker(tickInterval(p.ttl), p.reapIdle)
	}
	return p, nil
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return "local" }

// SetLogger attaches the structured logger the provider emits its
// lifecycle diagnostics (model load / idle unload) to. Called once by
// the svc layer after construction, before any Complete. A nil logger
// (or never calling it) degrades to a no-op logger.
func (p *Provider) SetLogger(l *zap.Logger) {
	p.logMu.Lock()
	p.logger = l
	p.logMu.Unlock()
}

// log returns the configured logger, or a no-op logger when none was
// set. Guarded so the reaper goroutine and a startup SetLogger can't
// race on the field.
func (p *Provider) log() *zap.Logger {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	if p.logger == nil {
		return zap.NewNop()
	}
	return p.logger
}

// Loaded reports whether the model is currently resident. Implements the
// svc-side loadable interface: the search-assist gate consults it to
// avoid triggering an expensive cold load while enrichment is in flight.
func (p *Provider) Loaded() bool {
	return p.gate.isLoaded()
}

// loadResources mmaps the model and allocates both contexts. Invoked by
// the gate under its mutex on the first Complete after construction or
// after an idle unload. On any error it frees whatever it allocated and
// returns — the gate stays unloaded and a later Complete retries.
func (p *Provider) loadResources() error {
	p.log().Info("loading local model",
		zap.String("model", p.cfg.Model),
		zap.Int("gpu_layers", p.cfg.GPULayers))
	m, err := llm.LoadModel(p.cfg.Model, p.cfg.GPULayers)
	if err != nil {
		return fmt.Errorf("local: load model: %w", err)
	}
	assistCtx, err := m.NewContext(assistCtxSize, 0)
	if err != nil {
		m.Close()
		return fmt.Errorf("local: assist context: %w", err)
	}
	mainCtx, err := m.NewContext(p.cfg.Ctx, 0)
	if err != nil {
		assistCtx.Close()
		m.Close()
		return fmt.Errorf("local: main context: %w", err)
	}
	p.model = m
	p.assistCtx = assistCtx
	p.mainCtx = mainCtx
	return nil
}

// unloadResources frees the contexts and the model. Invoked by the gate
// under its mutex — from the idle reaper (with no call in flight) or
// from close (shutdown). The per-context mutexes are taken so a shutdown
// unload waits for any Generate that is still holding one; the idle path
// takes them uncontended (the gate guarantees inFlight == 0 there).
func (p *Provider) unloadResources() {
	p.assistMu.Lock()
	if p.assistCtx != nil {
		p.assistCtx.Close()
		p.assistCtx = nil
	}
	p.assistMu.Unlock()

	p.mainMu.Lock()
	if p.mainCtx != nil {
		p.mainCtx.Close()
		p.mainCtx = nil
	}
	p.mainMu.Unlock()

	if p.model != nil {
		p.model.Close()
		p.model = nil
	}
}

// reapIdle is the idle-ticker callback: it asks the gate to unload the
// model if it has sat idle past the TTL, and logs the reap with the idle
// duration when it fires.
func (p *Provider) reapIdle() {
	if idle, ok := p.gate.maybeUnload(p.ttl); ok {
		p.log().Info("unloaded idle local model",
			zap.Duration("idle", idle),
			zap.String("model", p.cfg.Model))
	}
}

// loadReason classifies why a cold load happened, for the load log line.
// The structured search-assist shapes are implicit ("assist"); the agent
// tool-loop and freeform generation are the explicit "ask" path.
func loadReason(shape llm.JSONShape) string {
	switch shape {
	case llm.ShapeExpandTerms, llm.ShapeRerankOrder, llm.ShapeVerifyKeep:
		return "assist"
	default:
		return "ask"
	}
}

// Complete implements llm.Provider. It flattens the conversation
// through the chat template, installs the GBNF grammar implied by
// req.Shape, and runs greedy decoding with a JSON-complete early-stop.
func (p *Provider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.CompletionResponse, error) {
	if err := ctx.Err(); err != nil {
		return llm.CompletionResponse{}, err
	}

	// acquire (re)loads the model if needed and pins it for the duration
	// of this call so the idle reaper can't free it mid-generation.
	t0 := time.Now()
	coldLoaded, err := p.gate.acquire()
	if err != nil {
		return llm.CompletionResponse{}, err
	}
	defer p.gate.release()
	if coldLoaded {
		p.log().Info("loaded local model",
			zap.String("reason", loadReason(req.Shape)),
			zap.Duration("duration", time.Since(t0)),
			zap.String("model", p.cfg.Model))
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	prompt := p.tmpl.flatten(req.Messages)
	grammar := grammarForShape(req.Shape, req.Tools)
	structured := req.Shape != llm.ShapeFreeform

	llmCtx, mu := p.contextFor(req.Shape)
	mu.Lock()
	defer mu.Unlock()

	llmCtx.Reset()
	if err := llmCtx.SetGrammar(grammar); err != nil {
		return llm.CompletionResponse{}, fmt.Errorf("local: install grammar: %w", err)
	}

	var buf strings.Builder
	_, err = llmCtx.Generate(prompt, maxTokens, func(piece string) bool {
		buf.WriteString(piece)
		// For a structured shape the grammar guarantees the output is
		// JSON; stop as soon as the top-level object closes and parses
		// instead of waiting on EOS. Freeform runs to EOS / maxTokens.
		if structured {
			return !jsonComplete(buf.String())
		}
		return true
	})
	if err != nil {
		return llm.CompletionResponse{}, err
	}
	return llm.CompletionResponse{Text: strings.TrimSpace(buf.String())}, nil
}

// contextFor routes a shape to its context + mutex. The structured
// search-assist shapes use the small assist context; the agent loop
// and freeform generation use the full-size main context. Only called
// while the caller holds a gate refcount, so the returned pointers are
// non-nil and stable for the duration of the call.
func (p *Provider) contextFor(shape llm.JSONShape) (*llm.Context, *sync.Mutex) {
	switch shape {
	case llm.ShapeExpandTerms, llm.ShapeRerankOrder, llm.ShapeVerifyKeep:
		return p.assistCtx, &p.assistMu
	default:
		return p.mainCtx, &p.mainMu
	}
}

// Close stops the idle reaper and releases the contexts and the model.
// Safe to call multiple times and before any Complete (when nothing was
// ever loaded).
func (p *Provider) Close() error {
	p.ticker.Stop()
	p.gate.close()
	return nil
}

// jsonComplete reports whether s is a complete, parseable top-level
// JSON object — the early-stop predicate for grammar-constrained
// generation.
func jsonComplete(s string) bool {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return false
	}
	var v any
	return json.Unmarshal([]byte(s), &v) == nil
}
