package embedding

import (
	"strings"
	"testing"
)

// TestNewProviderFromConfig_EmptyVariantUsesDefault asserts that a
// "local" provider with no variant takes the default-selection path
// (NewLocalProvider), never the variant lookup. We can't assert the
// concrete returned type without triggering a model download, so we
// assert the negative: an empty variant must NOT produce the
// "unknown hugot variant" error that only the variant branch can emit.
func TestNewProviderFromConfig_EmptyVariantUsesDefault(t *testing.T) {
	p, err := NewProviderFromConfig(ProviderConfig{Provider: "local", Variant: ""})
	if err != nil {
		// NewLocalProvider falls back to the static provider if no
		// transformer backend is available, so it must never error.
		t.Fatalf("local provider with empty variant errored: %v", err)
	}
	if p == nil {
		t.Fatalf("local provider with empty variant returned nil provider")
	}
	_ = p.Close()
}

// TestNewProviderFromConfig_NamedVariantRoutesToHugot asserts that a
// non-empty variant routes through NewHugotProviderWithVariant. An
// unknown variant name lets us prove the routing without a download:
// only the variant branch produces the "unknown hugot variant" error;
// NewLocalProvider would silently fall back to static and never raise
// it. The error must also enumerate the known variants.
func TestNewProviderFromConfig_NamedVariantRoutesToHugot(t *testing.T) {
	_, err := NewProviderFromConfig(ProviderConfig{
		Provider: "local",
		Variant:  "definitely-not-a-real-variant",
	})
	if err == nil {
		t.Fatalf("expected an error for an unknown variant, got nil — routing did not reach the variant branch")
	}
	if !strings.Contains(err.Error(), "unknown hugot variant") {
		t.Fatalf("error %q does not name the variant branch — routing likely went through NewLocalProvider", err)
	}
	// The error must list the real variants, proving KnownHugotVariants
	// is the routing table.
	for _, name := range KnownHugotVariants() {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("unknown-variant error must enumerate known variants; missing %q in %q", name, err.Error())
		}
	}
}

// TestNewProviderFromConfig_VariantIgnoredForNonLocal asserts the
// variant is honoured only for the "local" provider — a static provider
// with a bogus variant must still construct fine (the variant is
// ignored), so an existing static config can never break by carrying a
// stray variant value.
func TestNewProviderFromConfig_VariantIgnoredForNonLocal(t *testing.T) {
	p, err := NewProviderFromConfig(ProviderConfig{
		Provider: "static",
		Variant:  "definitely-not-a-real-variant",
	})
	if err != nil {
		t.Fatalf("static provider must ignore the variant, got error: %v", err)
	}
	if p == nil {
		t.Fatalf("static provider returned nil")
	}
	_ = p.Close()
}

// TestKnownHugotVariants_AllLookupable asserts every name returned by
// KnownHugotVariants resolves through LookupHugotVariant — the variant
// table and the public enumeration can never drift apart, so a name a
// caller reads from KnownHugotVariants is always a valid variant arg.
func TestKnownHugotVariants_AllLookupable(t *testing.T) {
	names := KnownHugotVariants()
	if len(names) == 0 {
		t.Fatal("KnownHugotVariants returned no variants")
	}
	for _, name := range names {
		if _, ok := LookupHugotVariant(name); !ok {
			t.Fatalf("KnownHugotVariants lists %q but LookupHugotVariant rejects it", name)
		}
	}
	// The default variant must itself be a known, lookupable name.
	if _, ok := LookupHugotVariant(DefaultHugotVariant); !ok {
		t.Fatalf("DefaultHugotVariant %q is not a known variant", DefaultHugotVariant)
	}
}
