package serverstack

import (
	"fmt"
	"os"
	"strings"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/embedding"
)

// EmbedderRequest carries the explicit, per-invocation embedding inputs a
// command collected from its flags and environment. ResolveEmbedder
// merges these with the on-disk config to decide what provider — if any —
// to construct. Fields are exported so cmd entry points can build the
// request across the package boundary.
type EmbedderRequest struct {
	// FlagChanged reports whether the `--embeddings` boolean flag was
	// explicitly set (cmd.Flags().Changed). Only an explicitly-set flag
	// overrides the config.
	FlagChanged bool
	// FlagEnabled is the value of `--embeddings`. Meaningful only when
	// FlagChanged is true.
	FlagEnabled bool
	// FlagURL / FlagModel are `--embeddings-url` / `--embeddings-model`.
	// A non-empty URL forces the API provider — the most explicit request.
	FlagURL   string
	FlagModel string
}

// ResolveEmbedder decides which embedding.Provider (if any) to install,
// applying a fixed precedence: an explicit URL (flag or env) forces the
// API provider; an explicit on/off signal (flag or env) decides
// enablement and the provider comes from the `embedding:` config; else
// the config decides (default: semantic search ON with the zero-download
// static GloVe provider). The returned string describes the decision for
// logging ("" when no embedder was built); a non-nil error means an
// embedder was requested but could not be constructed.
func ResolveEmbedder(req EmbedderRequest, cfg *config.Config) (embedding.Provider, string, error) {
	if url := firstNonEmpty(req.FlagURL, os.Getenv("GORTEX_EMBEDDINGS_URL")); url != "" {
		model := firstNonEmpty(req.FlagModel, os.Getenv("GORTEX_EMBEDDINGS_MODEL"))
		return embedding.NewAPIProvider(url, model), fmt.Sprintf("api (%s)", url), nil
	}

	embCfg := config.EmbeddingConfig{}
	if cfg != nil {
		embCfg = cfg.Embedding
	}

	explicitEnabled, haveExplicit := explicitEmbeddingToggle(req)
	if haveExplicit {
		if !explicitEnabled {
			return nil, "", nil
		}
		return buildConfiguredEmbedder(embCfg, "enabled by flag/env")
	}

	if !embCfg.EmbeddingEnabledOrDefault() {
		return nil, "", nil
	}
	return buildConfiguredEmbedder(embCfg, "enabled by config default")
}

// explicitEmbeddingToggle reports whether the caller gave an explicit
// on/off signal for embeddings, and what it was. The flag takes
// precedence over GORTEX_EMBEDDINGS.
func explicitEmbeddingToggle(req EmbedderRequest) (enabled, haveExplicit bool) {
	if req.FlagChanged {
		return req.FlagEnabled, true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GORTEX_EMBEDDINGS"))) {
	case "1", "true", "yes", "on", "y":
		return true, true
	case "0", "false", "no", "off", "n":
		return false, true
	default:
		return false, false
	}
}

// buildConfiguredEmbedder constructs the provider named by the config
// block (defaulting to the static GloVe provider).
func buildConfiguredEmbedder(embCfg config.EmbeddingConfig, why string) (embedding.Provider, string, error) {
	provider := embCfg.EmbeddingProviderOrDefault()
	variant := firstNonEmpty(os.Getenv("GORTEX_EMBEDDINGS_VARIANT"), embCfg.Variant)
	p, err := embedding.NewProviderFromConfig(embedding.ProviderConfig{
		Provider: provider,
		APIURL:   embCfg.APIURL,
		APIModel: embCfg.APIModel,
		Variant:  variant,
	})
	if err != nil {
		return nil, "", err
	}
	desc := provider
	if variant != "" && provider == "local" {
		desc = fmt.Sprintf("%s/%s", provider, variant)
	}
	return p, fmt.Sprintf("%s — %s", desc, why), nil
}

// EmbeddingChunkOptions translates the chunking knobs of an
// EmbeddingConfig into the embedding package's ChunkOptions. Zero values
// pass through — the chunker substitutes its own defaults.
func EmbeddingChunkOptions(cfg *config.Config) embedding.ChunkOptions {
	if cfg == nil {
		return embedding.ChunkOptions{}
	}
	return embedding.ChunkOptions{
		ThresholdLines: cfg.Embedding.ChunkThresholdLines,
		WindowLines:    cfg.Embedding.ChunkWindowLines,
	}
}

// firstNonEmpty returns the first non-empty string argument.
func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
