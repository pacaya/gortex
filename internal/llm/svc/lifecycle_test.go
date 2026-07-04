package svc

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/llm"
)

// fakeLoadable is a minimal llm.Provider that also implements the
// svc-side loadable + loggerConfigurable interfaces, so the service's
// ModelLoaded / EnrichmentBusy / logger-propagation seams can be
// exercised without the llama.cpp libraries.
type fakeLoadable struct {
	loaded    bool
	gotLogger *zap.Logger
}

func (f *fakeLoadable) Name() string { return "local" }
func (f *fakeLoadable) Complete(context.Context, llm.CompletionRequest) (llm.CompletionResponse, error) {
	return llm.CompletionResponse{}, nil
}
func (f *fakeLoadable) Close() error            { return nil }
func (f *fakeLoadable) Loaded() bool            { return f.loaded }
func (f *fakeLoadable) SetLogger(l *zap.Logger) { f.gotLogger = l }

// fakeHTTPProvider is a provider with no cold-load cost — it does NOT
// implement loadable, standing in for an HTTP backend.
type fakeHTTPProvider struct{}

func (fakeHTTPProvider) Name() string { return "openai" }
func (fakeHTTPProvider) Complete(context.Context, llm.CompletionRequest) (llm.CompletionResponse, error) {
	return llm.CompletionResponse{}, nil
}
func (fakeHTTPProvider) Close() error { return nil }

func TestService_ModelLoaded(t *testing.T) {
	// No provider: nothing to load, always ready.
	if !(&Service{}).ModelLoaded() {
		t.Fatal("a service with no provider must report ModelLoaded=true")
	}
	// Nil service is safe.
	var nilSvc *Service
	if !nilSvc.ModelLoaded() {
		t.Fatal("nil service must report ModelLoaded=true")
	}

	fake := &fakeLoadable{loaded: false}
	s := &Service{provider: fake}
	if s.ModelLoaded() {
		t.Fatal("an unloaded loadable provider must report ModelLoaded=false")
	}
	fake.loaded = true
	if !s.ModelLoaded() {
		t.Fatal("a loaded loadable provider must report ModelLoaded=true")
	}
}

// A provider that does NOT implement loadable (an HTTP backend) has no
// cold-load cost and must always report ModelLoaded=true.
func TestService_ModelLoaded_NonLoadableAlwaysReady(t *testing.T) {
	s := &Service{provider: fakeHTTPProvider{}}
	if !s.ModelLoaded() {
		t.Fatal("a non-loadable provider must always report ModelLoaded=true")
	}
}

func TestService_EnrichmentBusy(t *testing.T) {
	// Nil predicate reads as not busy.
	if (&Service{}).EnrichmentBusy() {
		t.Fatal("a service with no busy predicate must report EnrichmentBusy=false")
	}
	var nilSvc *Service
	if nilSvc.EnrichmentBusy() {
		t.Fatal("nil service must report EnrichmentBusy=false")
	}

	busy := false
	s := &Service{}
	WithBusyPredicate(func() bool { return busy })(s)
	if s.EnrichmentBusy() {
		t.Fatal("predicate returning false must read as not busy")
	}
	busy = true
	if !s.EnrichmentBusy() {
		t.Fatal("predicate returning true must read as busy")
	}
}

func TestService_ConfigureProviderPropagatesLogger(t *testing.T) {
	fake := &fakeLoadable{}
	log := zap.NewNop()
	s := &Service{logger: log}
	s.configureProvider(fake)
	if fake.gotLogger != log {
		t.Fatal("configureProvider must call SetLogger on a lifecycle-aware provider")
	}

	// A nil logger leaves the provider untouched.
	fake2 := &fakeLoadable{}
	(&Service{}).configureProvider(fake2)
	if fake2.gotLogger != nil {
		t.Fatal("a nil logger must not call SetLogger")
	}
}
