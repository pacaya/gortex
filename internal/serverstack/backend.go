// Package serverstack is the single construction path for Gortex's server
// stack: it builds graph.Store -> parser.Registry -> indexer -> query.Engine
// -> mcp.Server plus every side-effect init, so the daemon, the HTTP
// surface, and the one-shot embedded path all share one wiring instead of
// three near-identical copies.
//
// It lives in its own leaf package (not internal/daemon) because the
// constructor builds an *mcp.Server, and internal/mcp already imports
// internal/daemon one-way; hosting the constructor in internal/daemon
// would form the cycle daemon -> mcp -> daemon.
package serverstack

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/platform"
)

// OpenBackend constructs the graph.Store the server will run against,
// picking the implementation by name:
//
//   - "" / "memory" — in-process *graph.Graph; nothing persists across
//     runs; matches every existing test fixture.
//   - "sqlite" — the pure-Go modernc.org/sqlite store under the resolved
//     path (defaults to ~/.gortex/store/store.sqlite).
//
// Returns the store, a cleanup func the caller must defer (closes the
// underlying handle on disk-backed stores), and any open error.
// allowRebuild permits the on-disk sqlite backend to drop and recreate a
// database whose schema version is incompatible. The caller must hold the
// store lock; NewSharedServer passes true only in the branch where it acquired
// the exclusive flock.
func OpenBackend(name, path string, bufferPoolMB uint64, logger *zap.Logger, allowRebuild bool) (graph.Store, func(), error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "memory", "mem", "in-memory":
		s := graph.New()
		return s, func() {}, nil
	case "sqlite", "sqlite3":
		resolved, err := resolveBackendPath(path, "store.sqlite")
		if err != nil {
			return nil, nil, err
		}
		if logger != nil {
			logger.Info("opening sqlite backend", zap.String("path", resolved))
		}
		return openSqliteBackend(resolved, bufferPoolMB, allowRebuild)
	default:
		return nil, nil, fmt.Errorf("unknown backend %q (expected: memory, sqlite)", name)
	}
}

// isSqliteBackend reports whether name selects the on-disk sqlite store.
func isSqliteBackend(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "sqlite", "sqlite3":
		return true
	default:
		return false
	}
}

// resolveBackendPath turns an empty backend path into a default under the
// unified store directory (~/.gortex/store/<filename>, or the XDG_DATA_HOME
// equivalent). Otherwise expands ~ and returns the absolute path, creating
// the parent directory so the on-disk store can open the leaf.
func resolveBackendPath(in, filename string) (string, error) {
	in = strings.TrimSpace(in)
	if in == "" {
		in = filepath.Join(platform.StoreDir(), filename)
	} else if strings.HasPrefix(in, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		in = filepath.Join(home, in[2:])
	}
	abs, err := filepath.Abs(in)
	if err != nil {
		return "", fmt.Errorf("abs path %q: %w", in, err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("mkdir parent %q: %w", filepath.Dir(abs), err)
	}
	return abs, nil
}

// openSqliteBackend opens (or creates) the SQLite store at path. The
// pure-Go modernc.org/sqlite driver keeps the binary CGo-free while still
// getting a real query planner that drives the graph's secondary indexes.
// bufferPoolMB is accepted for signature parity with other on-disk
// backends but unused — SQLite sizes its page cache via a pragma.
func openSqliteBackend(path string, bufferPoolMB uint64, allowRebuild bool) (graph.Store, func(), error) {
	_ = bufferPoolMB
	var opts []store_sqlite.Option
	if allowRebuild {
		opts = append(opts, store_sqlite.WithRebuild())
	}
	s, err := store_sqlite.Open(path, opts...)
	if err != nil {
		hint := "if another gortex daemon is using this store, stop it first (`gortex daemon status` / `gortex daemon stop`)"
		if pid, ok := daemon.RunningPID(); ok {
			hint = fmt.Sprintf("a gortex daemon is already running (pid %d) — stop it with `gortex daemon stop`, or use `gortex daemon restart`", pid)
		}
		return nil, nil, fmt.Errorf("open sqlite store at %q: %w (%s)", path, err, hint)
	}
	return s, func() { _ = s.Close() }, nil
}
