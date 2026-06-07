package main

import (
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/serverstack"
)

// embedderRequest aliases the shared constructor's request type so the
// cmd entry points keep building it directly; resolveEmbedder /
// embeddingChunkOptions are thin wrappers over the relocated logic.
type embedderRequest = serverstack.EmbedderRequest

func resolveEmbedder(req embedderRequest, cfg *config.Config) (embedding.Provider, string, error) {
	return serverstack.ResolveEmbedder(req, cfg)
}

func embeddingChunkOptions(cfg *config.Config) embedding.ChunkOptions {
	return serverstack.EmbeddingChunkOptions(cfg)
}
