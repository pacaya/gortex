package main

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

var (
	filesFilter  string
	filesPattern string
	filesFormat  string
	filesIndex   string
)

var filesCmd = &cobra.Command{
	Use:   "files",
	Short: "List the indexed files in the repo, filtered and formatted for the shell",
	Long: `List the files the daemon has indexed for the repo, optionally filtered by a
name substring (--filter) or a path glob (--pattern), in one of three layouts:

  --format tree     a hierarchical directory tree (default)
  --format flat     one path per line (pipe-friendly)
  --format grouped  paths grouped under their directory`,
	RunE: runFiles,
}

func init() {
	filesCmd.Flags().StringVar(&filesFilter, "filter", "", "only files whose path contains this substring")
	filesCmd.Flags().StringVar(&filesPattern, "pattern", "", "only files matching this path glob (e.g. 'internal/**/*.go')")
	filesCmd.Flags().StringVar(&filesFormat, "format", "tree", "output layout: tree|flat|grouped")
	filesCmd.Flags().StringVar(&filesIndex, "index", "", "repository path the daemon tracks (default: current directory)")
	rootCmd.AddCommand(filesCmd)
}

func runFiles(cmd *cobra.Command, args []string) error {
	switch filesFormat {
	case "tree", "flat", "grouped":
	default:
		return fmt.Errorf("files: --format must be tree, flat, or grouped (got %q)", filesFormat)
	}

	repoPath := filesIndex
	if repoPath == "" {
		repoPath = "."
	}

	toolArgs := map[string]any{"limit": 5000}
	if filesFilter != "" {
		toolArgs["query"] = filesFilter
	}
	if filesPattern != "" {
		toolArgs["glob"] = filesPattern
	}
	// find_files needs at least one of query/glob; default to "match every
	// path" so a bare `gortex files` lists the whole tree.
	if filesFilter == "" && filesPattern == "" {
		toolArgs["glob"] = "**"
	}

	raw, err := requireDaemonTool(repoPath, "find_files", toolArgs)
	if err != nil {
		return err
	}
	paths := parseFindFilesPaths(raw)
	_, _ = fmt.Fprint(cmd.OutOrStdout(), renderFiles(paths, filesFormat))
	return nil
}

// parseFindFilesPaths extracts the file paths from a find_files response
// (its `files[].path` list).
func parseFindFilesPaths(raw []byte) []string {
	var resp struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if json.Unmarshal(raw, &resp) != nil {
		return nil
	}
	out := make([]string, 0, len(resp.Files))
	for _, f := range resp.Files {
		if f.Path != "" {
			out = append(out, f.Path)
		}
	}
	return out
}

// renderFiles renders a path list in the requested layout.
func renderFiles(paths []string, format string) string {
	switch format {
	case "flat":
		return renderFilesFlat(paths)
	case "grouped":
		return renderFilesGrouped(paths)
	default:
		return renderFilesTree(paths)
	}
}

// renderFilesFlat lists every path on its own line, sorted.
func renderFilesFlat(paths []string) string {
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	var b strings.Builder
	for _, p := range sorted {
		b.WriteString(path.Clean(toSlash(p)))
		b.WriteByte('\n')
	}
	return b.String()
}

// renderFilesGrouped groups paths under their parent directory, each directory
// a header followed by its files indented.
func renderFilesGrouped(paths []string) string {
	groups := make(map[string][]string)
	for _, p := range paths {
		sp := toSlash(p)
		dir := path.Dir(sp)
		if dir == "." {
			dir = "(root)"
		}
		groups[dir] = append(groups[dir], path.Base(sp))
	}
	dirs := make([]string, 0, len(groups))
	for d := range groups {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	var b strings.Builder
	for _, d := range dirs {
		b.WriteString(d)
		b.WriteString("/\n")
		files := groups[d]
		sort.Strings(files)
		for _, f := range files {
			b.WriteString("  ")
			b.WriteString(f)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// treeNode is a directory-tree node: a name keyed map of children plus a
// leaf flag.
type treeNode struct {
	children map[string]*treeNode
	isFile   bool
}

func newTreeNode() *treeNode { return &treeNode{children: map[string]*treeNode{}} }

// renderFilesTree renders the paths as an indented directory tree, directories
// suffixed with "/", every level sorted for determinism.
func renderFilesTree(paths []string) string {
	root := newTreeNode()
	for _, p := range paths {
		cur := root
		for _, seg := range strings.Split(toSlash(p), "/") {
			if seg == "" {
				continue
			}
			child, ok := cur.children[seg]
			if !ok {
				child = newTreeNode()
				cur.children[seg] = child
			}
			cur = child
		}
		cur.isFile = true
	}
	var b strings.Builder
	renderTreeNode(&b, root, 0)
	return b.String()
}

func renderTreeNode(b *strings.Builder, node *treeNode, depth int) {
	names := make([]string, 0, len(node.children))
	for name := range node.children {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		child := node.children[name]
		b.WriteString(strings.Repeat("  ", depth))
		b.WriteString(name)
		if len(child.children) > 0 {
			b.WriteByte('/')
		}
		b.WriteByte('\n')
		renderTreeNode(b, child, depth+1)
	}
}

// toSlash normalises a path to forward slashes without depending on the host
// separator (the graph stores slash paths).
func toSlash(p string) string { return strings.ReplaceAll(p, "\\", "/") }
