package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/persistence"
)

// gitInitRepo creates a real git repository at dir with one commit and
// returns its HEAD SHA. A real repo (not a bare .git dir) is required
// because runRepos shells out to `git rev-parse HEAD`.
func gitInitRepo(t *testing.T, dir string) (headSHA string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// Keep the commit deterministic and independent of the
		// developer's global git identity.
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %v: %s", args, out)
	}
	run("init")
	run("checkout", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644))
	run("add", "README.md")
	run("commit", "-m", "initial")
	return gitCommitHash(dir)
}

// gitCommitMore stages a new file and commits it, advancing HEAD.
// Returns the new HEAD SHA.
func gitCommitMore(t *testing.T, dir, file string) (headSHA string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %v: %s", args, out)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte("y"), 0o644))
	run("add", file)
	run("commit", "-m", "more")
	return gitCommitHash(dir)
}

// reposTestEnv writes a temp global config tracking the given repo
// entries, points the package-level cfgFile at it, and routes the
// persistence store the `repos` command reads at an isolated temp dir.
// Both package globals are restored on cleanup.
func reposTestEnv(t *testing.T, repos []config.RepoEntry) {
	t.Helper()
	root := t.TempDir()

	gc := &config.GlobalConfig{Repos: repos}
	gcPath := filepath.Join(root, "config.yaml")
	gc.SetConfigPath(gcPath)
	require.NoError(t, gc.Save())

	prevCfg := cfgFile
	cfgFile = gcPath
	prevCache := reposCacheDir
	reposCacheDir = filepath.Join(root, "cache")
	t.Cleanup(func() {
		cfgFile = prevCfg
		reposCacheDir = prevCache
	})
}

// seedSnapshot writes a persisted index snapshot for (repoPath, branch,
// commit) into the test's persistence cache, so runRepos sees the repo
// as indexed at that commit. The snapshot is keyed under the canonical
// repo path — the same key describeRepo reads with.
func seedSnapshot(t *testing.T, repoPath, branch, commit string, indexedAt time.Time) {
	t.Helper()
	store, err := persistence.NewFileStore(reposCacheDir, version)
	require.NoError(t, err)
	require.NoError(t, store.Save(&persistence.Snapshot{
		Version:    version,
		RepoPath:   canonicalRepo(repoPath),
		CommitHash: commit,
		Branch:     branch,
		IndexedAt:  indexedAt,
	}))
	require.NoError(t, store.Close())
}

func newReposCmd() (*cobra.Command, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	c := &cobra.Command{}
	c.SetOut(buf)
	c.SetErr(buf)
	return c, buf
}

// TestRunRepos_JSON_FreshAndStale covers the JSON shape, the head-commit
// field, and the freshness/staleness flag across the three states: a
// repo whose index matches HEAD (fresh), one whose HEAD has advanced
// past the indexed commit (stale), and one never indexed.
func TestRunRepos_JSON_FreshAndStale(t *testing.T) {
	base := t.TempDir()
	freshDir := filepath.Join(base, "fresh-repo")
	staleDir := filepath.Join(base, "stale-repo")
	neverDir := filepath.Join(base, "never-repo")

	freshHead := gitInitRepo(t, freshDir)
	oldHead := gitInitRepo(t, staleDir)
	neverHead := gitInitRepo(t, neverDir)

	reposTestEnv(t, []config.RepoEntry{
		{Path: freshDir, Name: "fresh-repo", Workspace: "ws-fresh"},
		{Path: staleDir, Name: "stale-repo"},
		{Path: neverDir, Name: "never-repo"},
	})

	indexedAt := time.Now().Add(-time.Hour).Truncate(time.Second)
	// fresh-repo: snapshot at the exact current HEAD.
	seedSnapshot(t, freshDir, "main", freshHead, indexedAt)
	// stale-repo: snapshot at the old HEAD, then advance HEAD.
	seedSnapshot(t, staleDir, "main", oldHead, indexedAt)
	newHead := gitCommitMore(t, staleDir, "second.txt")
	require.NotEqual(t, oldHead, newHead)
	// never-repo: no snapshot seeded.

	reposJSON = true
	t.Cleanup(func() { reposJSON = false })

	cmd, buf := newReposCmd()
	require.NoError(t, runRepos(cmd, nil))

	var got []repoStatus
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Len(t, got, 3)

	// Output is sorted by name: fresh, never, stale.
	byName := map[string]repoStatus{}
	for _, e := range got {
		byName[e.Name] = e
	}

	fresh := byName["fresh-repo"]
	assert.Equal(t, freshHead, fresh.HeadCommit, "head commit must be the current git HEAD")
	assert.Equal(t, freshHead, fresh.IndexedCommit)
	assert.Equal(t, "main", fresh.Branch)
	assert.Equal(t, "ws-fresh", fresh.Workspace)
	assert.True(t, fresh.Indexed)
	assert.False(t, fresh.Stale, "index matches HEAD → not stale")
	require.NotNil(t, fresh.LastIndexed)
	assert.Equal(t, indexedAt.UTC(), fresh.LastIndexed.UTC())

	stale := byName["stale-repo"]
	assert.Equal(t, newHead, stale.HeadCommit)
	assert.Equal(t, oldHead, stale.IndexedCommit)
	assert.True(t, stale.Indexed)
	assert.True(t, stale.Stale, "HEAD advanced past the indexed commit → stale")
	require.NotNil(t, stale.LastIndexed)

	never := byName["never-repo"]
	assert.Equal(t, neverHead, never.HeadCommit, "head commit reported even without an index")
	assert.Empty(t, never.IndexedCommit)
	assert.False(t, never.Indexed)
	assert.True(t, never.Stale, "never indexed → stale")
	assert.Nil(t, never.LastIndexed)
}

// TestRunRepos_Table renders the default (non-JSON) form and asserts
// the freshness vocabulary appears for each state.
func TestRunRepos_Table(t *testing.T) {
	base := t.TempDir()
	freshDir := filepath.Join(base, "alpha")
	neverDir := filepath.Join(base, "beta")
	freshHead := gitInitRepo(t, freshDir)
	gitInitRepo(t, neverDir)

	reposTestEnv(t, []config.RepoEntry{
		{Path: freshDir, Name: "alpha"},
		{Path: neverDir, Name: "beta"},
	})
	seedSnapshot(t, freshDir, "main", freshHead, time.Now().Truncate(time.Second))

	reposJSON = false
	cmd, buf := newReposCmd()
	require.NoError(t, runRepos(cmd, nil))

	out := buf.String()
	assert.Contains(t, out, "alpha")
	assert.Contains(t, out, "beta")
	assert.Contains(t, out, "fresh")
	assert.Contains(t, out, "not indexed")
	// The short-SHA prefix of the fresh repo's HEAD must be in the table.
	assert.Contains(t, out, freshHead[:12])
	// The never-indexed repo shows the placeholder timestamp.
	assert.Contains(t, out, "(never)")
}

// TestRunRepos_NoTrackedRepos exercises the empty-config path for both
// output modes.
func TestRunRepos_NoTrackedRepos(t *testing.T) {
	reposTestEnv(t, nil)

	reposJSON = false
	cmd, buf := newReposCmd()
	require.NoError(t, runRepos(cmd, nil))
	assert.Contains(t, buf.String(), "(no tracked repos)")

	reposJSON = true
	t.Cleanup(func() { reposJSON = false })
	cmd, buf = newReposCmd()
	require.NoError(t, runRepos(cmd, nil))
	var got []repoStatus
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	assert.Empty(t, got)
}

// TestRunRepos_StaleWhenBranchSlotMissing proves that a repo on a branch
// with no persisted snapshot at all reports as not-indexed and stale —
// the snapshot for a different branch must not count.
func TestRunRepos_StaleWhenBranchSlotMissing(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "repo")
	head := gitInitRepo(t, dir)

	reposTestEnv(t, []config.RepoEntry{{Path: dir, Name: "repo"}})
	// Snapshot stored under a different branch slot.
	seedSnapshot(t, dir, "other-branch", head, time.Now().Truncate(time.Second))

	reposJSON = true
	t.Cleanup(func() { reposJSON = false })
	cmd, buf := newReposCmd()
	require.NoError(t, runRepos(cmd, nil))

	var got []repoStatus
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Len(t, got, 1)
	assert.False(t, got[0].Indexed, "a snapshot on a different branch must not count")
	assert.True(t, got[0].Stale)
}

// TestShortSHA covers the table SHA abbreviation helper.
func TestShortSHA(t *testing.T) {
	assert.Equal(t, "(none)", shortSHA(""))
	assert.Equal(t, "abc", shortSHA("abc"))
	assert.Equal(t, "0123456789ab", shortSHA("0123456789abcdef0123"))
}

// TestRunWorkspaceList_JSON covers the --json flag added to
// `gortex workspace list`: the JSON array carries each repo's resolved
// workspace, project, and source.
func TestRunWorkspaceList_JSON(t *testing.T) {
	root := t.TempDir()
	repoGlobal := filepath.Join(root, "with-global")
	repoYAML := filepath.Join(root, "with-yaml")
	repoDefault := filepath.Join(root, "plain")
	for _, d := range []string{repoGlobal, repoYAML, repoDefault} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	// with-yaml declares its workspace in .gortex.yaml.
	require.NoError(t, os.WriteFile(
		filepath.Join(repoYAML, ".gortex.yaml"),
		[]byte("workspace: yaml-ws\nproject: yaml-proj\n"), 0o644))

	gc := &config.GlobalConfig{Repos: []config.RepoEntry{
		{Path: repoGlobal, Name: "with-global", Workspace: "global-ws", Project: "global-proj"},
		{Path: repoYAML, Name: "with-yaml"},
		{Path: repoDefault, Name: "plain"},
	}}
	gcPath := filepath.Join(root, "config.yaml")
	gc.SetConfigPath(gcPath)
	require.NoError(t, gc.Save())

	prevCfg := cfgFile
	cfgFile = gcPath
	t.Cleanup(func() { cfgFile = prevCfg })

	workspaceListJSON = true
	t.Cleanup(func() { workspaceListJSON = false })

	cmd, buf := newReposCmd()
	require.NoError(t, runWorkspaceList(cmd, nil))

	var got []workspaceListEntry
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Len(t, got, 3)

	byRepo := map[string]workspaceListEntry{}
	for _, e := range got {
		byRepo[e.Repo] = e
	}

	g := byRepo["with-global"]
	assert.Equal(t, "global-ws", g.Workspace)
	assert.Equal(t, "global-proj", g.Project)
	assert.Equal(t, "global", g.Source)

	y := byRepo["with-yaml"]
	assert.Equal(t, "yaml-ws", y.Workspace)
	assert.Equal(t, "yaml-proj", y.Project)
	assert.Equal(t, ".gortex.yaml", y.Source)

	p := byRepo["plain"]
	assert.Equal(t, "default", p.Source)
	assert.Contains(t, p.Workspace, "default")
}
