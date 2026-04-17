package main

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"strings"

	"github.com/zzet/gortex/internal/bridge"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/persistence"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/semantic/goanalysis"
	"github.com/zzet/gortex/internal/semantic/lsp"
	"github.com/zzet/gortex/internal/semantic/scip"
	"github.com/zzet/gortex/internal/web"
	"github.com/zzet/gortex/internal/web/hub"
)

var (
	bridgePort       int
	bridgeIndex      string
	bridgeCORSOrigin string
	bridgeWeb        bool
	bridgeWatch      bool
	bridgeTrack      []string
	bridgeProject    string
	bridgeCacheDir        string
	bridgeNoCache         bool
	bridgeEmbeddings      bool
	bridgeEmbeddingsURL   string
	bridgeEmbeddingsModel string
	bridgeSemantic        bool
	bridgeNoSemantic      bool
	bridgeSemanticMode    string
)

var bridgeCmd = &cobra.Command{
	Use:   "bridge",
	Short: "Start the HTTP bridge API for external integrations",
	Long:  "Exposes Gortex MCP tools as an HTTP/JSON API. Endpoints: /health, /tools, /tool/{name}, /stats. Optionally serves the web UI on the same port.",
	RunE:  runBridge,
}

func init() {
	bridgeCmd.Flags().IntVar(&bridgePort, "port", 4747, "HTTP port to listen on")
	bridgeCmd.Flags().StringVar(&bridgeIndex, "index", "", "repository path to index on startup")
	bridgeCmd.Flags().StringVar(&bridgeCORSOrigin, "cors-origin", "*", "allowed CORS origin (use '*' for any)")
	bridgeCmd.Flags().BoolVar(&bridgeWeb, "web", false, "serve web visualization UI on the same port")
	bridgeCmd.Flags().BoolVar(&bridgeWatch, "watch", false, "keep graph in sync with filesystem changes")
	bridgeCmd.Flags().StringSliceVar(&bridgeTrack, "track", nil, "additional repository paths to track")
	bridgeCmd.Flags().StringVar(&bridgeProject, "project", "", "active project name")
	bridgeCmd.Flags().StringVar(&bridgeCacheDir, "cache-dir", "", "graph cache directory (default ~/.cache/gortex/)")
	bridgeCmd.Flags().BoolVar(&bridgeNoCache, "no-cache", false, "disable graph caching")
	bridgeCmd.Flags().BoolVar(&bridgeEmbeddings, "embeddings", false, "enable semantic search")
	bridgeCmd.Flags().StringVar(&bridgeEmbeddingsURL, "embeddings-url", "", "embedding API URL (e.g. http://localhost:11434 for Ollama)")
	bridgeCmd.Flags().StringVar(&bridgeEmbeddingsModel, "embeddings-model", "", "embedding model name")
	bridgeCmd.Flags().BoolVar(&bridgeSemantic, "semantic", false, "enable semantic enrichment (SCIP, go/types, LSP)")
	bridgeCmd.Flags().BoolVar(&bridgeNoSemantic, "no-semantic", false, "disable semantic enrichment")
	bridgeCmd.Flags().StringVar(&bridgeSemanticMode, "semantic-mode", "typecheck", "Go analysis mode: typecheck or callgraph")
	rootCmd.AddCommand(bridgeCmd)
}

func runBridge(_ *cobra.Command, _ []string) error {
	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Build graph/parser/indexer/query/MCP stack.
	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(g, reg, cfg.Index, logger)

	// Set up embedding provider for semantic search. Kept local so it
	// can be handed off to MultiIndexer below; otherwise per-repo
	// indexers built inside TrackRepoCtx have embedder=nil.
	var embedder embedding.Provider
	if bridgeEmbeddingsURL != "" {
		embedder = embedding.NewAPIProvider(bridgeEmbeddingsURL, bridgeEmbeddingsModel)
		fmt.Fprintf(os.Stderr, "[gortex] bridge: semantic search enabled (API: %s)\n", bridgeEmbeddingsURL)
	} else if bridgeEmbeddings {
		e, err := embedding.NewLocalProvider()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[gortex] bridge: embeddings disabled: %v\n", err)
		} else {
			embedder = e
			fmt.Fprintf(os.Stderr, "[gortex] bridge: semantic search enabled (local)\n")
		}
	}
	if embedder != nil {
		idx.SetEmbedder(embedder)
	}

	// Set up semantic enrichment.
	if !bridgeNoSemantic && (bridgeSemantic || cfg.Semantic.Enabled) {
		semCfg := cfg.Semantic
		semCfg.Enabled = true

		semInternalCfg := semantic.Config{
			Enabled:           semCfg.Enabled,
			TimeoutSeconds:    semCfg.TimeoutSeconds,
			EnrichOnWatch:     semCfg.EnrichOnWatch,
			WatchDebounceMs:   semCfg.WatchDebounceMs,
			RefuteUnconfirmed: semCfg.RefuteUnconfirmed,
		}
		for _, pc := range semCfg.Providers {
			semInternalCfg.Providers = append(semInternalCfg.Providers, semantic.ProviderConfig{
				Name:        pc.Name,
				Command:     pc.Command,
				Args:        pc.Args,
				Languages:   pc.Languages,
				Priority:    pc.Priority,
				Enabled:     pc.Enabled,
				Mode:        pc.Mode,
				Daemon:      pc.Daemon,
				MaxParallel: pc.MaxParallel,
			})
		}

		semMgr := semantic.NewManager(semInternalCfg, logger)

		mode := goanalysis.ModeTypeCheck
		if bridgeSemanticMode == "callgraph" {
			mode = goanalysis.ModeCallGraph
		}
		semMgr.RegisterProvider(goanalysis.NewProvider(mode, false, logger))

		for _, pc := range semCfg.Providers {
			if !pc.Enabled {
				continue
			}
			switch {
			case strings.HasPrefix(pc.Name, "scip-") && pc.Command != "":
				semMgr.RegisterProvider(scip.NewProvider(pc.Command, pc.Args, pc.Languages, semCfg.TimeoutSeconds, logger))
			case strings.HasPrefix(pc.Name, "gopls") || pc.Daemon:
				semMgr.RegisterProvider(lsp.NewProvider(pc.Command, pc.Args, pc.Languages, pc.Daemon, pc.MaxParallel, logger))
			}
		}

		idx.SetSemanticManager(semMgr)
		fmt.Fprintf(os.Stderr, "[gortex] bridge: semantic enrichment enabled (mode: %s)\n", bridgeSemanticMode)
	}

	// Multi-repo support.
	cm, err := config.NewConfigManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gortex] warning: could not load global config: %v\n", err)
	}

	if cm != nil && len(bridgeTrack) > 0 {
		for _, trackPath := range bridgeTrack {
			absPath, err := filepath.Abs(trackPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[gortex] warning: could not resolve --track path %s: %v\n", trackPath, err)
				continue
			}
			if err := cm.Global().AddRepo(config.RepoEntry{Path: absPath}); err != nil {
				fmt.Fprintf(os.Stderr, "[gortex] warning: could not add --track repo %s: %v\n", absPath, err)
			}
		}
	}

	activeProject := bridgeProject
	if activeProject == "" && cm != nil {
		activeProject = cm.Global().ActiveProject
	}
	if cm != nil {
		cm.Global().ActiveProject = activeProject
	}

	var mi *indexer.MultiIndexer
	if cm != nil {
		mi = indexer.NewMultiIndexer(g, reg, idx.Search(), cm, logger)
		if embedder != nil {
			mi.SetEmbedder(embedder)
		}
	}

	var multiOpts []gortexmcp.MultiRepoOptions
	if mi != nil || cm != nil {
		multiOpts = append(multiOpts, gortexmcp.MultiRepoOptions{
			MultiIndexer:  mi,
			ConfigManager: cm,
			ActiveProject: activeProject,
		})
	}

	eng := query.NewEngine(g)
	eng.SetSearchProvider(idx.Search)
	gortexmcp.Version = version
	srv := gortexmcp.NewServer(eng, g, idx, nil, logger, cfg.Guards.Rules, multiOpts...)

	if semMgr := idx.SemanticManager(); semMgr != nil {
		srv.SetSemanticManager(semMgr)
	}

	// Create persistence store.
	var store persistence.Store
	if bridgeNoCache {
		store = persistence.NopStore{}
	} else {
		var err error
		store, err = persistence.NewFileStore(bridgeCacheDir, version)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[gortex] bridge: cache disabled: %v\n", err)
			store = persistence.NopStore{}
		}
	}

	// Build the HTTP handler — start serving immediately, index in background.
	bridgeHandler := bridge.NewHandler(srv.MCPServer(), g, version, logger)

	var handler http.Handler
	if bridgeWeb {
		// Compose bridge API + web UI on the same port.
		topMux := http.NewServeMux()

		// Bridge API routes.
		topMux.Handle("/health", bridgeHandler)
		topMux.Handle("/tools", bridgeHandler)
		topMux.Handle("/tool/", bridgeHandler)
		topMux.Handle("/stats", bridgeHandler)

		// Web UI.
		var eventHub *hub.Hub
		if bridgeWatch {
			wcfg := cfg.Watch
			wcfg.Enabled = true
			watcher, err := indexer.NewWatcher(idx, wcfg, logger)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[gortex] bridge: watcher setup failed: %v\n", err)
			} else {
				watchPaths := wcfg.Paths
				if len(watchPaths) == 0 && bridgeIndex != "" {
					watchPaths = []string{bridgeIndex}
				}
				if len(watchPaths) == 0 {
					watchPaths = []string{"."}
				}
				if err := watcher.Start(watchPaths); err != nil {
					fmt.Fprintf(os.Stderr, "[gortex] bridge: watcher start failed: %v\n", err)
				} else {
					srv.SetWatcher(watcher)
					eventHub = hub.New()
					go eventHub.Run(watcher.Events())
					srv.WatchForReanalysis(eventHub, 500)
					fmt.Fprintf(os.Stderr, "[gortex] bridge: watch mode active\n")
				}
			}
		}

		webSrv := web.NewServer(g, eng, eventHub, logger)
		topMux.Handle("/", webSrv.Handler())

		handler = topMux
		fmt.Fprintf(os.Stderr, "[gortex] bridge: web UI enabled\n")
	} else {
		handler = bridgeHandler

		// Watch mode without web UI.
		if bridgeWatch {
			wcfg := cfg.Watch
			wcfg.Enabled = true
			watcher, err := indexer.NewWatcher(idx, wcfg, logger)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[gortex] bridge: watcher setup failed: %v\n", err)
			} else {
				watchPaths := wcfg.Paths
				if len(watchPaths) == 0 && bridgeIndex != "" {
					watchPaths = []string{bridgeIndex}
				}
				if len(watchPaths) == 0 {
					watchPaths = []string{"."}
				}
				if err := watcher.Start(watchPaths); err != nil {
					fmt.Fprintf(os.Stderr, "[gortex] bridge: watcher start failed: %v\n", err)
				} else {
					srv.SetWatcher(watcher)
					eventHub := hub.New()
					go eventHub.Run(watcher.Events())
					srv.WatchForReanalysis(eventHub, 500)
					fmt.Fprintf(os.Stderr, "[gortex] bridge: watch mode active\n")
				}
			}
		}
	}

	// Wrap with CORS.
	corsOpts := bridge.CORSOptions{AllowOrigins: []string{bridgeCORSOrigin}}
	handler = bridge.WithCORS(handler, corsOpts)

	addr := fmt.Sprintf(":%d", bridgePort)
	httpServer := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	fmt.Fprintf(os.Stderr, "[gortex] bridge listening on http://localhost:%d\n", bridgePort)

	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	// Background: index, multi-repo, analyze — graph populates while HTTP is live.
	go func() {
		// When MultiIndexer is available (global config has repos), use it exclusively.
		// Single --index flag is only used when no multi-repo config exists.
		if mi != nil {
			fmt.Fprintf(os.Stderr, "[gortex] bridge: multi-repo indexing...\n")
			if _, err := mi.IndexAll(); err != nil {
				fmt.Fprintf(os.Stderr, "[gortex] bridge: multi-repo indexing error: %v\n", err)
			}
		} else if bridgeIndex != "" {
			commitHash := gitCommitHash(bridgeIndex)
			cached := false

			if commitHash != "" && store.Check(bridgeIndex, commitHash) && store.Validate(bridgeIndex, commitHash) {
				snap, err := store.Load(bridgeIndex, commitHash)
				if err == nil {
					for _, n := range snap.Nodes {
						g.AddNode(n)
					}
					for _, e := range snap.Edges {
						g.AddEdge(e)
					}
					idx.SetFileMtimes(snap.FileMtimes)
					idx.SetRootPath(bridgeIndex)

					if len(snap.VectorIndex) > 0 && snap.VectorDims > 0 {
						if err := idx.ImportVectorIndex(snap.VectorIndex, snap.VectorDims, snap.VectorCount); err != nil {
							fmt.Fprintf(os.Stderr, "[gortex] bridge: vector index restore failed: %v\n", err)
						}
					}

					result, err := idx.IncrementalReindex(bridgeIndex)
					if err != nil {
						fmt.Fprintf(os.Stderr, "[gortex] bridge: incremental reindex failed: %v\n", err)
					} else {
						fmt.Fprintf(os.Stderr, "[gortex] bridge: restored graph (%d nodes, %d edges), re-indexed %d stale files in %dms\n",
							result.NodeCount, result.EdgeCount, result.FileCount, result.DurationMs)
					}
					cached = true
				} else {
					fmt.Fprintf(os.Stderr, "[gortex] bridge: cache load failed, will re-index: %v\n", err)
				}
			}

			if !cached {
				fmt.Fprintf(os.Stderr, "[gortex] bridge: indexing %s...\n", bridgeIndex)
				result, err := idx.Index(bridgeIndex)
				if err != nil {
					fmt.Fprintf(os.Stderr, "[gortex] bridge: indexing failed: %v\n", err)
					return
				}
				fmt.Fprintf(os.Stderr, "[gortex] bridge: indexed %d files (%d nodes, %d edges) in %dms\n",
					result.FileCount, result.NodeCount, result.EdgeCount, result.DurationMs)
			}
		}

		// Search backend is auto-updated via SearchProvider (idx.Search)

		// Set contract registry: in multi-repo mode, merge all per-repo registries.
		if mi != nil {
			srv.SetContractRegistry(mi.MergedContractRegistry())
		} else if cr := idx.ContractRegistry(); cr != nil {
			srv.SetContractRegistry(cr)
		}

		srv.RunAnalysis()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return fmt.Errorf("bridge: %w", err)
	case sig := <-sigCh:
		fmt.Fprintf(os.Stderr, "\n[gortex] bridge: received %s, shutting down\n", sig)

		if bridgeIndex != "" {
			commitHash := gitCommitHash(bridgeIndex)
			if commitHash != "" {
				snap := &persistence.Snapshot{
					Version:    version,
					RepoPath:   bridgeIndex,
					CommitHash: commitHash,
					IndexedAt:  time.Now(),
					Nodes:      g.AllNodes(),
					Edges:      g.AllEdges(),
					FileMtimes: idx.FileMtimes(),
				}
				snap.VectorIndex, snap.VectorDims, snap.VectorCount = idx.ExportVectorIndex()
				if err := store.Save(snap); err != nil {
					fmt.Fprintf(os.Stderr, "[gortex] bridge: cache save failed: %v\n", err)
				} else {
					fmt.Fprintf(os.Stderr, "[gortex] bridge: saved graph snapshot (%d nodes, %d edges)\n",
						len(snap.Nodes), len(snap.Edges))
				}
			}
		}

		return httpServer.Close()
	}
}
