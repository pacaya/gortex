package lsp

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/platform"
)

// isJdtlsCommand reports whether a resolved LSP command is the Eclipse JDT
// language server, whether configured bare ("jdtls") or as an absolute path.
func isJdtlsCommand(command string) bool {
	return filepath.Base(command) == "jdtls"
}

// jdtlsWorkspaceDir returns the Eclipse workspace directory jdtls should use
// for a repo root, rooted at Gortex's cache home (via internal/platform, so
// XDG_CACHE_HOME overrides win and the path is Windows-safe). Without an
// explicit -data the brew launcher defaults the workspace to
// ~/Library/Caches/jdtls/<hash> — outside the isolation every other engine
// artifact honours — and two daemons indexing the same repo path collide there.
func jdtlsWorkspaceDir(repoRoot string) string {
	sum := sha256.Sum256([]byte(repoRoot))
	hash := hex.EncodeToString(sum[:])[:16]
	return filepath.Join(platform.CacheDir(), "lsp", "jdtls", hash)
}

// jdtlsDataArgs appends `-data <workspace>` to a jdtls launch argv unless the
// caller already pinned an explicit workspace. It creates the workspace and
// clears a stale Eclipse lock so a workspace left locked by a crashed or killed
// server does not wedge the next launch.
func jdtlsDataArgs(baseArgs []string, repoRoot string) []string {
	for _, a := range baseArgs {
		if a == "-data" {
			return baseArgs
		}
	}
	dir := jdtlsWorkspaceDir(repoRoot)
	prepareJdtlsWorkspace(dir)
	out := append([]string(nil), baseArgs...)
	return append(out, "-data", dir)
}

// prepareJdtlsWorkspace ensures the workspace directory exists and removes a
// stale Eclipse lock (.metadata/.lock) left by a crashed or killed jdtls, which
// would otherwise refuse to open the workspace and wedge enrichment. Best
// effort — a live server re-creates the lock, and any error is non-fatal so a
// launch is never blocked on workspace hygiene.
func prepareJdtlsWorkspace(dir string) {
	_ = os.MkdirAll(dir, 0o755)
	_ = os.Remove(filepath.Join(dir, ".metadata", ".lock"))
}
