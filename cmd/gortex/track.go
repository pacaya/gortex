package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/config"
)

var trackCmd = &cobra.Command{
	Use:   "track <path>",
	Short: "Add a repository to the tracked workspace",
	Long:  "Resolves the path to absolute, validates it exists, and adds it to the global config.",
	Args:  cobra.ExactArgs(1),
	RunE:  runTrack,
}

var untrackCmd = &cobra.Command{
	Use:   "untrack <path>",
	Short: "Remove a repository from the tracked workspace",
	Long:  "Resolves the path and removes the matching entry from the global config.",
	Args:  cobra.ExactArgs(1),
	RunE:  runUntrack,
}

func init() {
	rootCmd.AddCommand(trackCmd)
	rootCmd.AddCommand(untrackCmd)
}

func runTrack(_ *cobra.Command, args []string) error {
	rawPath := args[0]

	// Resolve to absolute path.
	absPath, err := filepath.Abs(rawPath)
	if err != nil {
		return fmt.Errorf("resolving path %s: %w", rawPath, err)
	}

	// Validate path exists and is a directory.
	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("path does not exist: %s", absPath)
	}
	if !info.IsDir() {
		return fmt.Errorf("path is not a directory: %s", absPath)
	}

	// Load global config.
	gc, err := config.LoadGlobal()
	if err != nil {
		return fmt.Errorf("loading global config: %w", err)
	}

	// Check for duplicate before adding.
	for _, existing := range gc.Repos {
		existingAbs, _ := filepath.Abs(existing.Path)
		if existingAbs == absPath {
			fmt.Fprintf(os.Stderr, "[gortex] already tracked: %s\n", absPath)
			return nil
		}
	}

	// Add repo entry.
	entry := config.RepoEntry{Path: absPath}
	if err := gc.AddRepo(entry); err != nil {
		return err
	}

	// Persist to disk.
	if err := gc.Save(); err != nil {
		return fmt.Errorf("saving global config: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[gortex] tracked %s\n", absPath)
	return nil
}

func runUntrack(_ *cobra.Command, args []string) error {
	rawPath := args[0]

	// Resolve to absolute path.
	absPath, err := filepath.Abs(rawPath)
	if err != nil {
		return fmt.Errorf("resolving path %s: %w", rawPath, err)
	}

	// Load global config.
	gc, err := config.LoadGlobal()
	if err != nil {
		return fmt.Errorf("loading global config: %w", err)
	}

	// Remove repo.
	if err := gc.RemoveRepo(absPath); err != nil {
		return err
	}

	// Persist to disk.
	if err := gc.Save(); err != nil {
		return fmt.Errorf("saving global config: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[gortex] untracked %s\n", absPath)
	return nil
}
