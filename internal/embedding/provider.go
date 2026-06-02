// Package embedding provides pluggable embedding providers for semantic search.
//
// The default build includes the Hugot provider (pure-Go ONNX runtime via
// hugot.NewGoSession) which auto-downloads MiniLM-L6-v2 on first use — no
// external runtime, no manual model placement. The legacy StaticProvider
// (GloVe word vectors) and APIProvider (Ollama/OpenAI) are also always
// available.
//
// Opt-in build tags enable faster transformer backends for users who are
// willing to manage native dependencies:
//   - embeddings_onnx  — yalue/onnxruntime_go with libonnxruntime on PATH
//   - embeddings_gomlx — hugot with XLA/PJRT plugin (~100MB auto-download)
package embedding

import (
	"context"
	"fmt"
)

// Provider generates embedding vectors from text.
type Provider interface {
	// Embed returns the embedding vector for the given text.
	Embed(ctx context.Context, text string) ([]float32, error)

	// EmbedBatch returns embeddings for multiple texts.
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)

	// Dimensions returns the embedding vector size.
	Dimensions() int

	// Close releases resources.
	Close() error
}

// NewHugotProvider exposes the pure-Go Hugot backend (MiniLM-L6-v2)
// directly, without the NewLocalProvider fallback chain. Useful when a
// caller wants a hard error if Hugot can't start (e.g. eval harnesses
// that mustn't silently degrade to static GloVe).
func NewHugotProvider() (Provider, error) { return newHugotProvider() }

// NewHugotProviderWithVariant loads a specific embedder variant from
// any registered HuggingFace repo (MiniLM variants, code-tuned models,
// …). Pass a name returned by KnownHugotVariants (e.g. "fp32",
// "qint8_arm64", "jina_code", "bge_code"). Returns an error if the
// variant name is unknown or the download/load fails.
func NewHugotProviderWithVariant(variant string) (Provider, error) {
	v, ok := LookupHugotVariant(variant)
	if !ok {
		return nil, fmt.Errorf("unknown hugot variant %q (known: %v)", variant, KnownHugotVariants())
	}
	return newHugotProviderWithSpec(v)
}

// ProviderConfig is the subset of an embedding configuration that
// NewProviderFromConfig needs. It is a local struct — not the
// config.EmbeddingConfig type — so the embedding package stays free of
// an import dependency on internal/config. Callers translate their
// config block into this shape.
type ProviderConfig struct {
	// Provider selects the backend: "static" (baked GloVe, the
	// default), "local" (best available transformer), or "api" (an
	// external embedding endpoint). Empty is treated as "static".
	Provider string
	// APIURL / APIModel parameterise the "api" provider.
	APIURL   string
	APIModel string
	// Variant names a specific local transformer model to load (a key
	// from KnownHugotVariants, e.g. "fp32", "bge_small", "jina_code").
	// Honoured only when Provider is "local": a non-empty Variant pins
	// that exact model via NewHugotProviderWithVariant instead of the
	// auto-selected NewLocalProvider backend. Empty preserves the
	// existing default-selection behaviour. Ignored for other providers.
	Variant string
}

// NewProviderFromConfig constructs an embedding provider from a
// configuration block. The selection logic:
//
//   - "static" (or empty)  → NewStaticProvider — baked GloVe word
//     vectors, zero download, CPU-only. This is the default because
//     it makes semantic search work with no setup.
//   - "local"              → NewLocalProvider — the best available
//     transformer backend (Hugot MiniLM auto-downloads on first use).
//     When cfg.Variant names a specific model, that exact variant is
//     loaded via NewHugotProviderWithVariant instead.
//   - "api"                → NewAPIProvider against cfg.APIURL.
//
// An unknown provider name is an error so a typo in `.gortex.yaml`
// fails loudly instead of silently degrading.
func NewProviderFromConfig(cfg ProviderConfig) (Provider, error) {
	switch cfg.Provider {
	case "", "static":
		return NewStaticProvider()
	case "local":
		// A pinned variant loads that exact model; an empty variant
		// keeps the existing auto-selection (ONNX → GoMLX → Hugot →
		// static) so every prior config behaves identically.
		if cfg.Variant != "" {
			return NewHugotProviderWithVariant(cfg.Variant)
		}
		return NewLocalProvider()
	case "api":
		if cfg.APIURL == "" {
			return nil, fmt.Errorf("embedding provider %q requires an api_url", cfg.Provider)
		}
		return NewAPIProvider(cfg.APIURL, cfg.APIModel), nil
	default:
		return nil, fmt.Errorf("unknown embedding provider %q (want static, local, or api)", cfg.Provider)
	}
}

// NewLocalProvider returns the best available local embedding provider.
// Preference order: ONNX (fastest, requires libonnxruntime) → GoMLX (XLA) →
// Hugot (pure Go, always compiled in) → Static (GloVe word vectors fallback).
func NewLocalProvider() (Provider, error) {
	// Opt-in transformer backends (compiled in via build tags), then the
	// default Hugot pure-Go ONNX runtime which auto-downloads MiniLM-L6-v2
	// to ~/.gortex/models/ on first use.
	factories := []func() (Provider, error){
		newONNXProvider,
		newGoMLXProvider,
		newHugotProvider,
	}
	for _, factory := range factories {
		if p, err := factory(); err == nil {
			return p, nil
		}
	}
	// Fallback: static word vectors (always available, no network).
	return NewStaticProvider()
}
