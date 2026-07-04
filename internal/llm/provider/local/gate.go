// Package local's load/idle-unload concurrency core. This file carries
// NO build tag: it compiles in both the `llama` and non-`llama` builds
// so the state machine is unit-testable (gate_test.go) without the
// llama.cpp libraries. The CGO-touching state mutation lives in the
// tagged provider (local.go), which drives this gate through injected
// load/unload hooks.
package local

import (
	"errors"
	"os"
	"strings"
	"sync"
	"time"
)

// errGateClosed is returned by acquire once the gate has been closed
// (provider shutdown) — no further loads are permitted.
var errGateClosed = errors.New("local: provider closed")

// defaultIdleTTL is the idle window after which the local model is
// unloaded when GORTEX_LLM_IDLE_TTL is unset.
const defaultIdleTTL = 10 * time.Minute

// idleTTLFromEnv resolves the idle-unload TTL from GORTEX_LLM_IDLE_TTL.
// The value is a verbatim Go duration (e.g. "5m"); "0" / "off" / "none"
// disables idle unloading (the model, once loaded, stays resident); an
// unset or unparseable value falls back to defaultIdleTTL. Parsed the
// same way the enrichment timeout override is (see
// semantic.enrichRepoTimeout).
func idleTTLFromEnv() time.Duration {
	switch v := strings.TrimSpace(os.Getenv("GORTEX_LLM_IDLE_TTL")); v {
	case "":
		return defaultIdleTTL
	case "0", "off", "none":
		return 0
	default:
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		return defaultIdleTTL
	}
}

// tickInterval derives how often the idle reaper checks the gate from
// the TTL: half the TTL, clamped to [1s, 1m] so a short test TTL is
// checked promptly and a long production TTL isn't polled needlessly.
func tickInterval(ttl time.Duration) time.Duration {
	iv := ttl / 2
	if iv < time.Second {
		iv = time.Second
	}
	if iv > time.Minute {
		iv = time.Minute
	}
	return iv
}

// idleGate is the untagged concurrency core of the local provider's
// load / idle-unload lifecycle. It guards a single expensive resource
// (the llama model + its contexts) behind three invariants:
//
//   - lazy (re)load: the resource is loaded on the first acquire after
//     construction or after an idle unload, via the injected load hook;
//   - refcount safety: an idle unload only runs when no acquire is in
//     flight, so a Complete already using the model can never have it
//     freed underneath it;
//   - idle TTL: unload fires only once the resource has sat unused for
//     at least the configured TTL.
//
// load and unload are always invoked while the gate mutex is held, so
// they never overlap each other or a state read.
type idleGate struct {
	mu       sync.Mutex
	loaded   bool
	inFlight int
	lastUse  time.Time
	closed   bool

	// load (re)allocates the resource. On error the gate stays unloaded
	// and the error is returned to the acquirer; a later acquire retries.
	load func() error
	// unload frees the resource. Called only with loaded == true, and
	// (for the idle path) inFlight == 0.
	unload func()
	// now is the clock, injectable so tests can drive the TTL.
	now func() time.Time
}

// newIdleGate builds a gate around the load / unload hooks. The clock
// defaults to time.Now; tests replace it via the returned gate's now
// field before first use.
func newIdleGate(load func() error, unload func()) *idleGate {
	return &idleGate{load: load, unload: unload, now: time.Now}
}

// acquire ensures the resource is loaded and registers one in-flight
// use. On success the caller MUST pair it with release(); on error no
// refcount is taken and the resource is left unloaded. coldLoaded
// reports whether a real (cold) load ran during this acquire — the
// tagged provider uses it to log the load event with its reason.
func (g *idleGate) acquire() (coldLoaded bool, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false, errGateClosed
	}
	if !g.loaded {
		if lerr := g.load(); lerr != nil {
			return false, lerr
		}
		g.loaded = true
		coldLoaded = true
	}
	g.inFlight++
	g.lastUse = g.now()
	return coldLoaded, nil
}

// release records the end of one in-flight use and refreshes the idle
// clock so the TTL is measured from the last completed call.
func (g *idleGate) release() {
	g.mu.Lock()
	if g.inFlight > 0 {
		g.inFlight--
	}
	g.lastUse = g.now()
	g.mu.Unlock()
}

// maybeUnload frees the resource when it is loaded, idle (no in-flight
// use), not closed, and has been idle at least ttl. Returns the idle
// duration and true when it unloaded; (0, false) otherwise. A
// non-positive ttl disables idle unloading.
func (g *idleGate) maybeUnload(ttl time.Duration) (time.Duration, bool) {
	if ttl <= 0 {
		return 0, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.loaded || g.inFlight > 0 || g.closed {
		return 0, false
	}
	idle := g.now().Sub(g.lastUse)
	if idle < ttl {
		return 0, false
	}
	g.unload()
	g.loaded = false
	return idle, true
}

// isLoaded reports whether the resource is currently resident — the
// signal the assist gate consults to decide whether a request would
// trigger a cold load.
func (g *idleGate) isLoaded() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.loaded
}

// close unloads the resource if loaded and blocks all further acquires.
// Idempotent. Unlike the idle path this frees regardless of inFlight —
// it is the shutdown teardown, sequenced after the idle reaper has been
// stopped.
func (g *idleGate) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.closed = true
	if g.loaded {
		g.unload()
		g.loaded = false
	}
}

// idleTicker periodically invokes tick until Stop. Generic and untagged
// so the provider's reaper loop is covered by the same llama-free tests.
type idleTicker struct {
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// startIdleTicker launches a goroutine that calls tick every interval
// until Stop is called.
func startIdleTicker(interval time.Duration, tick func()) *idleTicker {
	t := &idleTicker{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(t.done)
		tk := time.NewTicker(interval)
		defer tk.Stop()
		for {
			select {
			case <-t.stop:
				return
			case <-tk.C:
				tick()
			}
		}
	}()
	return t
}

// Stop halts the ticker and waits for its goroutine to exit. Safe to
// call multiple times and on a nil ticker (TTL disabled).
func (t *idleTicker) Stop() {
	if t == nil {
		return
	}
	t.stopOnce.Do(func() { close(t.stop) })
	<-t.done
}
