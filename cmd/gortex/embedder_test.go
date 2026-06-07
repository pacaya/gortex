package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/embedding"
)

// withEnv sets an environment variable for the duration of a test and
// restores it afterwards.
func withEnv(t *testing.T, key, val string) {
	t.Helper()
	t.Setenv(key, val)
}

// TestResolveEmbedder_DefaultOnStatic asserts the core default-on
// behaviour: with no `--embeddings` flag, no GORTEX_EMBEDDINGS env, and
// a config whose Embedding block is the zero value (Enabled == nil),
// resolveEmbedder returns a working static GloVe provider.
func TestResolveEmbedder_DefaultOnStatic(t *testing.T) {
	withEnv(t, "GORTEX_EMBEDDINGS", "")
	withEnv(t, "GORTEX_EMBEDDINGS_URL", "")

	cfg := config.Default()
	require.Nil(t, cfg.Embedding.Enabled, "precondition: default config leaves Enabled nil")

	emb, desc, err := resolveEmbedder(embedderRequest{}, cfg)
	require.NoError(t, err)
	require.NotNil(t, emb, "default-on must produce an embedder")
	defer func() { _ = emb.Close() }()

	_, isStatic := emb.(*embedding.StaticProvider)
	assert.True(t, isStatic, "the default-on embedder must be the static GloVe provider, got %T", emb)
	assert.Contains(t, desc, "static")
}

// TestResolveEmbedder_ConfigDisabled asserts an explicit
// `embedding.enabled: false` in config turns the embedder off when no
// flag/env override is present.
func TestResolveEmbedder_ConfigDisabled(t *testing.T) {
	withEnv(t, "GORTEX_EMBEDDINGS", "")
	withEnv(t, "GORTEX_EMBEDDINGS_URL", "")

	disabled := false
	cfg := config.Default()
	cfg.Embedding.Enabled = &disabled

	emb, _, err := resolveEmbedder(embedderRequest{}, cfg)
	require.NoError(t, err)
	assert.Nil(t, emb, "embedding.enabled: false must yield no embedder")
}

// TestResolveEmbedder_FlagOverridesConfig asserts an explicit
// `--embeddings=false` flag overrides a config that enables embeddings.
func TestResolveEmbedder_FlagOverridesConfig(t *testing.T) {
	withEnv(t, "GORTEX_EMBEDDINGS", "")
	withEnv(t, "GORTEX_EMBEDDINGS_URL", "")

	enabled := true
	cfg := config.Default()
	cfg.Embedding.Enabled = &enabled

	// flagChanged=true, flagEnabled=false → explicit off wins.
	emb, _, err := resolveEmbedder(embedderRequest{FlagChanged: true, FlagEnabled: false}, cfg)
	require.NoError(t, err)
	assert.Nil(t, emb, "an explicit --embeddings=false flag must override config-enabled")
}

// TestResolveEmbedder_EnvOverridesConfig asserts GORTEX_EMBEDDINGS=0
// overrides a config that would otherwise enable the default-on path.
func TestResolveEmbedder_EnvOverridesConfig(t *testing.T) {
	withEnv(t, "GORTEX_EMBEDDINGS", "0")
	withEnv(t, "GORTEX_EMBEDDINGS_URL", "")

	cfg := config.Default() // Enabled nil → default-on

	emb, _, err := resolveEmbedder(embedderRequest{}, cfg)
	require.NoError(t, err)
	assert.Nil(t, emb, "GORTEX_EMBEDDINGS=0 must override the default-on config")
}

// TestResolveEmbedder_ExplicitURLForcesAPI asserts an explicit
// embedding URL (flag or env) forces the API provider regardless of the
// config block.
func TestResolveEmbedder_ExplicitURLForcesAPI(t *testing.T) {
	withEnv(t, "GORTEX_EMBEDDINGS", "")
	withEnv(t, "GORTEX_EMBEDDINGS_URL", "")

	disabled := false
	cfg := config.Default()
	cfg.Embedding.Enabled = &disabled // even with embeddings off in config…

	emb, desc, err := resolveEmbedder(embedderRequest{
		FlagURL: "http://localhost:11434",
	}, cfg)
	require.NoError(t, err)
	require.NotNil(t, emb, "an explicit URL must produce an embedder even when config disables embeddings")
	defer func() { _ = emb.Close() }()

	_, isAPI := emb.(*embedding.APIProvider)
	assert.True(t, isAPI, "an explicit URL must select the API provider, got %T", emb)
	assert.Contains(t, desc, "api")
}

// TestResolveEmbedder_ConfigProviderHonored asserts that when the
// config enables embeddings and names the `api` provider, resolveEmbedder
// builds an API provider from the config's URL.
func TestResolveEmbedder_ConfigProviderHonored(t *testing.T) {
	withEnv(t, "GORTEX_EMBEDDINGS", "")
	withEnv(t, "GORTEX_EMBEDDINGS_URL", "")

	cfg := config.Default()
	cfg.Embedding.Provider = "api"
	cfg.Embedding.APIURL = "http://localhost:11434"

	emb, _, err := resolveEmbedder(embedderRequest{}, cfg)
	require.NoError(t, err)
	require.NotNil(t, emb)
	defer func() { _ = emb.Close() }()

	_, isAPI := emb.(*embedding.APIProvider)
	assert.True(t, isAPI, "config provider:api must build an APIProvider, got %T", emb)
}
