package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

var statusIndex string

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show index status: node/edge counts, languages, and file breakdown",
	RunE:  runStatus,
}

func init() {
	statusCmd.Flags().StringVar(&statusIndex, "index", ".", "repository path to index")
	rootCmd.AddCommand(statusCmd)
}

func runStatus(_ *cobra.Command, _ []string) error {
	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	idx := indexer.New(g, reg, cfg.Index, logger)
	result, err := idx.Index(statusIndex)
	if err != nil {
		return fmt.Errorf("indexing failed: %w", err)
	}

	stats := g.Stats()

	_, _ = fmt.Fprintf(os.Stdout, "Repository:  %s\n", statusIndex)
	_, _ = fmt.Fprintf(os.Stdout, "Files:       %d\n", result.FileCount)
	_, _ = fmt.Fprintf(os.Stdout, "Nodes:       %d\n", stats.TotalNodes)
	_, _ = fmt.Fprintf(os.Stdout, "Edges:       %d\n", stats.TotalEdges)
	_, _ = fmt.Fprintf(os.Stdout, "Duration:    %dms\n\n", result.DurationMs)

	if len(stats.ByLanguage) > 0 {
		_, _ = fmt.Fprintln(os.Stdout, "Languages:")
		for lang, count := range stats.ByLanguage {
			_, _ = fmt.Fprintf(os.Stdout, "  %-14s %d nodes\n", lang, count)
		}
		_, _ = fmt.Fprintln(os.Stdout)
	}

	if len(stats.ByKind) > 0 {
		_, _ = fmt.Fprintln(os.Stdout, "By kind:")
		for kind, count := range stats.ByKind {
			_, _ = fmt.Fprintf(os.Stdout, "  %-14s %d\n", kind, count)
		}
	}

	// Display per-repo and per-project stats from GlobalConfig.
	gc, err := config.LoadGlobal()
	if err == nil {
		printMultiRepoStatus(gc, g)
	}

	return nil
}

// printMultiRepoStatus displays per-repo and per-project statistics from the GlobalConfig.
func printMultiRepoStatus(gc *config.GlobalConfig, g *graph.Graph) {
	repoStats := g.RepoStats()
	hasMultiRepo := len(repoStats) > 1 || len(gc.Repos) > 0 || len(gc.Projects) > 0

	if !hasMultiRepo {
		return
	}

	_, _ = fmt.Fprintln(os.Stdout)

	// Active project indicator.
	if gc.ActiveProject != "" {
		_, _ = fmt.Fprintf(os.Stdout, "Active project: %s\n\n", gc.ActiveProject)
	}

	// Per-repo stats.
	if len(gc.Repos) > 0 {
		_, _ = fmt.Fprintln(os.Stdout, "Tracked repositories:")

		// Build a set of repos shared across projects.
		sharedRepos := findSharedRepos(gc)

		for _, repo := range gc.Repos {
			prefix := config.ResolvePrefix(repo)
			_, _ = fmt.Fprintf(os.Stdout, "  %s\n", prefix)
			_, _ = fmt.Fprintf(os.Stdout, "    Path: %s\n", repo.Path)

			if repo.Ref != "" {
				_, _ = fmt.Fprintf(os.Stdout, "    Ref:  %s\n", repo.Ref)
			}

			// Show graph stats if available.
			if rs, ok := repoStats[prefix]; ok {
				_, _ = fmt.Fprintf(os.Stdout, "    Nodes: %d  Edges: %d\n", rs.TotalNodes, rs.TotalEdges)
			}

			// Indicate shared repos.
			if projects, ok := sharedRepos[repo.Path]; ok && len(projects) > 0 {
				_, _ = fmt.Fprintf(os.Stdout, "    Shared in: %s\n", strings.Join(projects, ", "))
			}
		}
		_, _ = fmt.Fprintln(os.Stdout)
	}

	// Per-project stats.
	if len(gc.Projects) > 0 {
		_, _ = fmt.Fprintln(os.Stdout, "Projects:")

		// Sort project names for deterministic output.
		projNames := make([]string, 0, len(gc.Projects))
		for name := range gc.Projects {
			projNames = append(projNames, name)
		}
		sort.Strings(projNames)

		for _, projName := range projNames {
			proj := gc.Projects[projName]
			active := ""
			if projName == gc.ActiveProject {
				active = " (active)"
			}
			_, _ = fmt.Fprintf(os.Stdout, "  %s%s\n", projName, active)

			// Aggregate counts for the project.
			var totalNodes, totalEdges int
			for _, repo := range proj.Repos {
				prefix := config.ResolvePrefix(repo)
				refTag := ""
				if repo.Ref != "" {
					refTag = fmt.Sprintf(" [%s]", repo.Ref)
				}
				_, _ = fmt.Fprintf(os.Stdout, "    - %s%s (%s)\n", prefix, refTag, repo.Path)

				if rs, ok := repoStats[prefix]; ok {
					totalNodes += rs.TotalNodes
					totalEdges += rs.TotalEdges
				}
			}
			_, _ = fmt.Fprintf(os.Stdout, "    Total: %d nodes, %d edges\n", totalNodes, totalEdges)
		}
	}
}

// findSharedRepos returns a map of repo path → list of project names that include it.
// Only repos appearing in 2+ projects are included.
func findSharedRepos(gc *config.GlobalConfig) map[string][]string {
	pathProjects := make(map[string][]string)
	for projName, proj := range gc.Projects {
		for _, repo := range proj.Repos {
			pathProjects[repo.Path] = append(pathProjects[repo.Path], projName)
		}
	}

	// Filter to only shared repos.
	shared := make(map[string][]string)
	for path, projects := range pathProjects {
		if len(projects) > 1 {
			sort.Strings(projects)
			shared[path] = projects
		}
	}
	return shared
}
