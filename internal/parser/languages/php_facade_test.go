package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// callEdgeReceiver returns the receiver_type meta of the first EdgeCalls whose
// target method bare-name matches `method`.
func callEdgeReceiver(edges []*graph.Edge, method string) (string, bool, bool) {
	for _, e := range edges {
		if e.Kind != graph.EdgeCalls || e.To != "unresolved::*."+method {
			continue
		}
		if e.Meta == nil {
			return "", false, true
		}
		rt, ok := e.Meta["receiver_type"].(string)
		return rt, ok, true
	}
	return "", false, false
}

// TestPHPFacade_BuiltinMapsToBackingClass pins the Laravel facade table: a
// `Cache::get()` static call is stamped with the backing Repository as its
// receiver type, so the resolver binds it to Repository::get rather than an
// unrelated name-only `get`.
func TestPHPFacade_BuiltinMapsToBackingClass(t *testing.T) {
	src := []byte(`<?php
class Svc {
    public function run() {
        return Cache::get('k');
    }
}
`)
	res, err := NewPHPExtractor().Extract("svc.php", src)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	rt, ok, found := callEdgeReceiver(res.Edges, "get")
	if !found {
		t.Fatal("no Cache::get() call edge emitted")
	}
	if !ok || rt != "Repository" {
		t.Errorf("Cache::get() receiver_type = %q (ok=%v), want Repository", rt, ok)
	}
}

// TestPHPFacade_NonFacadeStaticCallUnstamped pins that an ordinary static call
// to a class that is not a registered facade carries no receiver_type hint.
func TestPHPFacade_NonFacadeStaticCallUnstamped(t *testing.T) {
	src := []byte(`<?php
class Svc {
    public function run() {
        return Helper::frobnicate();
    }
}
`)
	res, err := NewPHPExtractor().Extract("svc.php", src)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	rt, ok, found := callEdgeReceiver(res.Edges, "frobnicate")
	if !found {
		t.Fatal("no Helper::frobnicate() call edge emitted")
	}
	if ok && rt != "" {
		t.Errorf("non-facade static call must not stamp receiver_type, got %q", rt)
	}
}

// TestPHPFacade_RegisterExtensible pins config-extensibility: a custom facade
// registered via RegisterPHPFacade resolves to its backing class.
func TestPHPFacade_RegisterExtensible(t *testing.T) {
	RegisterPHPFacade("Billing", "StripeGateway")
	src := []byte(`<?php
class Svc {
    public function run() {
        return Billing::charge();
    }
}
`)
	res, err := NewPHPExtractor().Extract("svc.php", src)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	rt, ok, found := callEdgeReceiver(res.Edges, "charge")
	if !found {
		t.Fatal("no Billing::charge() call edge emitted")
	}
	if !ok || rt != "StripeGateway" {
		t.Errorf("Billing::charge() receiver_type = %q (ok=%v), want StripeGateway", rt, ok)
	}
}
