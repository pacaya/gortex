// Untagged tests for the load / idle-unload concurrency core. These run
// in both the `llama` and non-`llama` builds — they never touch the
// llama.cpp libraries, exercising the gate through fake load/unload
// hooks so the refcount + TTL state machine is verified under -race
// without a real 4 GiB model.
package local

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeResource stands in for the model + contexts. load/unload just flip
// counters so the gate's lifecycle is observable.
type fakeResource struct {
	mu      sync.Mutex
	loads   int
	unloads int
	live    bool
	failNth int // when >0, the load'th call returns an error
}

func (f *fakeResource) load() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	if f.failNth > 0 && f.loads == f.failNth {
		return errors.New("boom")
	}
	f.live = true
	return nil
}

func (f *fakeResource) unload() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unloads++
	f.live = false
}

func (f *fakeResource) snapshot() (loads, unloads int, live bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loads, f.unloads, f.live
}

func TestIdleGate_LazyLoadOnFirstAcquire(t *testing.T) {
	fr := &fakeResource{}
	g := newIdleGate(fr.load, fr.unload)

	if g.isLoaded() {
		t.Fatal("gate should start unloaded")
	}
	cold, err := g.acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !cold {
		t.Fatal("first acquire must report a cold load")
	}
	if !g.isLoaded() {
		t.Fatal("gate should be loaded after acquire")
	}
	// A second acquire while the first is still in flight must NOT reload.
	cold2, err := g.acquire()
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	if cold2 {
		t.Fatal("second concurrent acquire must not cold-load")
	}
	g.release()
	g.release()

	loads, _, live := fr.snapshot()
	if loads != 1 || !live {
		t.Fatalf("want exactly one load and live resource, got loads=%d live=%v", loads, live)
	}
}

func TestIdleGate_LoadErrorLeavesUnloadedAndRetries(t *testing.T) {
	fr := &fakeResource{failNth: 1}
	g := newIdleGate(fr.load, fr.unload)

	if _, err := g.acquire(); err == nil {
		t.Fatal("acquire must surface the load error")
	}
	if g.isLoaded() {
		t.Fatal("a failed load must leave the gate unloaded")
	}
	// The next acquire retries the load (unlike a sync.Once, which would
	// have cached the failure forever).
	cold, err := g.acquire()
	if err != nil {
		t.Fatalf("retry acquire: %v", err)
	}
	if !cold {
		t.Fatal("retry after a failed load must cold-load")
	}
	g.release()
}

func TestIdleGate_IdleUnloadAfterTTL(t *testing.T) {
	fr := &fakeResource{}
	g := newIdleGate(fr.load, fr.unload)

	now := time.Unix(0, 0)
	g.now = func() time.Time { return now }

	cold, err := g.acquire()
	if err != nil || !cold {
		t.Fatalf("acquire: cold=%v err=%v", cold, err)
	}
	g.release()

	ttl := 10 * time.Minute
	// Not yet idle enough: no unload.
	now = now.Add(5 * time.Minute)
	if idle, ok := g.maybeUnload(ttl); ok {
		t.Fatalf("unloaded too early after %s idle", idle)
	}
	if !g.isLoaded() {
		t.Fatal("must still be loaded before TTL elapses")
	}
	// Past the TTL: unload fires and reports the idle duration.
	now = now.Add(6 * time.Minute)
	idle, ok := g.maybeUnload(ttl)
	if !ok {
		t.Fatal("expected an idle unload past the TTL")
	}
	if idle < ttl {
		t.Fatalf("reported idle %s < ttl %s", idle, ttl)
	}
	if g.isLoaded() {
		t.Fatal("gate must be unloaded after the idle reap")
	}
	_, unloads, live := fr.snapshot()
	if unloads != 1 || live {
		t.Fatalf("want one unload and dead resource, got unloads=%d live=%v", unloads, live)
	}
}

func TestIdleGate_NeverUnloadsWhileInFlight(t *testing.T) {
	fr := &fakeResource{}
	g := newIdleGate(fr.load, fr.unload)
	now := time.Unix(0, 0)
	g.now = func() time.Time { return now }

	if _, err := g.acquire(); err != nil { // held — not released
		t.Fatalf("acquire: %v", err)
	}
	now = now.Add(time.Hour) // long past any TTL
	if idle, ok := g.maybeUnload(time.Minute); ok {
		t.Fatalf("unloaded while a use was in flight (idle=%s)", idle)
	}
	if !g.isLoaded() {
		t.Fatal("in-flight use must keep the resource loaded")
	}
	g.release()
	// release refreshes lastUse to "now"; advance again so the gate is
	// idle past the TTL before the reaper runs.
	now = now.Add(time.Hour)
	if _, ok := g.maybeUnload(time.Minute); !ok {
		t.Fatal("expected unload once the in-flight use drained")
	}
}

func TestIdleGate_DisabledTTLNeverUnloads(t *testing.T) {
	fr := &fakeResource{}
	g := newIdleGate(fr.load, fr.unload)
	now := time.Unix(0, 0)
	g.now = func() time.Time { return now }
	if _, err := g.acquire(); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	g.release()
	now = now.Add(24 * time.Hour)
	if _, ok := g.maybeUnload(0); ok {
		t.Fatal("a non-positive TTL must disable idle unloading")
	}
	if !g.isLoaded() {
		t.Fatal("resource must stay loaded when TTL is disabled")
	}
}

func TestIdleGate_ReloadAfterUnload(t *testing.T) {
	fr := &fakeResource{}
	g := newIdleGate(fr.load, fr.unload)
	now := time.Unix(0, 0)
	g.now = func() time.Time { return now }

	if _, err := g.acquire(); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	g.release()
	now = now.Add(time.Hour)
	if _, ok := g.maybeUnload(time.Minute); !ok {
		t.Fatal("expected idle unload")
	}
	// Next acquire transparently reloads.
	cold, err := g.acquire()
	if err != nil {
		t.Fatalf("reacquire: %v", err)
	}
	if !cold {
		t.Fatal("acquire after an idle unload must cold-load again")
	}
	g.release()
	loads, unloads, live := fr.snapshot()
	if loads != 2 || unloads != 1 || !live {
		t.Fatalf("want loads=2 unloads=1 live=true, got loads=%d unloads=%d live=%v", loads, unloads, live)
	}
}

func TestIdleGate_ClosedRejectsAcquire(t *testing.T) {
	fr := &fakeResource{}
	g := newIdleGate(fr.load, fr.unload)
	if _, err := g.acquire(); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	g.release()
	g.close()
	if _, err := g.acquire(); !errors.Is(err, errGateClosed) {
		t.Fatalf("acquire after close must return errGateClosed, got %v", err)
	}
	_, unloads, live := fr.snapshot()
	if unloads != 1 || live {
		t.Fatalf("close must unload a loaded resource, got unloads=%d live=%v", unloads, live)
	}
	// close is idempotent.
	g.close()
	if _, unloads, _ := fr.snapshot(); unloads != 1 {
		t.Fatalf("second close must not unload again, got unloads=%d", unloads)
	}
}

// TestIdleGate_ConcurrentAcquireReleaseVsUnload hammers the gate from
// many goroutines while a reaper races to unload, asserting the
// resource is never unloaded with a use in flight and load/unload stay
// balanced. Run under -race this is the core safety proof.
func TestIdleGate_ConcurrentAcquireReleaseVsUnload(t *testing.T) {
	fr := &fakeResource{}
	g := newIdleGate(fr.load, fr.unload)

	// A live-use counter the reaper checks: it must never observe an
	// unload (live flip to false) while this is > 0.
	var inUse int64
	var violations int64

	origUnload := g.unload
	g.unload = func() {
		if atomic.LoadInt64(&inUse) > 0 {
			atomic.AddInt64(&violations, 1)
		}
		origUnload()
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Reaper: a zero TTL here would be disabled, so use a tiny positive
	// TTL and a now() that always looks "long idle" so it unloads
	// aggressively whenever inFlight hits zero.
	g.now = func() time.Time { return time.Unix(1<<40, 0) }
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				g.maybeUnload(time.Nanosecond)
			}
		}
	}()

	// Workers: acquire, mark in-use, release.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 2000; j++ {
				cold, err := g.acquire()
				_ = cold
				if err != nil {
					continue
				}
				atomic.AddInt64(&inUse, 1)
				// tiny bit of "work" while the reaper spins
				atomic.AddInt64(&inUse, -1)
				g.release()
			}
		}()
	}

	// Let workers finish, then stop the reaper.
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(stop)
	}()
	wg.Wait()

	if v := atomic.LoadInt64(&violations); v != 0 {
		t.Fatalf("resource was unloaded %d times while a use was in flight", v)
	}
}

func TestIdleTicker_StopIsIdempotent(t *testing.T) {
	var ticks int64
	tk := startIdleTicker(time.Millisecond, func() { atomic.AddInt64(&ticks, 1) })
	time.Sleep(20 * time.Millisecond)
	tk.Stop()
	after := atomic.LoadInt64(&ticks)
	if after == 0 {
		t.Fatal("ticker never fired")
	}
	// Second Stop must not panic or hang.
	tk.Stop()
	// No ticks after Stop.
	time.Sleep(10 * time.Millisecond)
	if atomic.LoadInt64(&ticks) != after {
		t.Fatal("ticker fired after Stop")
	}
	// Stop on a nil ticker is a no-op.
	var nilTicker *idleTicker
	nilTicker.Stop()
}

func TestIdleTTLFromEnv(t *testing.T) {
	cases := []struct {
		val  string
		set  bool
		want time.Duration
	}{
		{set: false, want: defaultIdleTTL},
		{val: "", set: true, want: defaultIdleTTL},
		{val: "5m", set: true, want: 5 * time.Minute},
		{val: "30s", set: true, want: 30 * time.Second},
		{val: "0", set: true, want: 0},
		{val: "off", set: true, want: 0},
		{val: "none", set: true, want: 0},
		{val: "garbage", set: true, want: defaultIdleTTL},
	}
	for _, tc := range cases {
		if tc.set {
			t.Setenv("GORTEX_LLM_IDLE_TTL", tc.val)
		} else {
			_ = os.Unsetenv("GORTEX_LLM_IDLE_TTL")
		}
		if got := idleTTLFromEnv(); got != tc.want {
			t.Errorf("idleTTLFromEnv(%q,set=%v) = %s, want %s", tc.val, tc.set, got, tc.want)
		}
	}
}

func TestTickInterval(t *testing.T) {
	if got := tickInterval(10 * time.Minute); got != time.Minute {
		t.Errorf("tickInterval(10m) = %s, want 1m (clamped)", got)
	}
	if got := tickInterval(2 * time.Second); got != time.Second {
		t.Errorf("tickInterval(2s) = %s, want 1s", got)
	}
	if got := tickInterval(time.Millisecond); got != time.Second {
		t.Errorf("tickInterval(1ms) = %s, want 1s (floor)", got)
	}
}
