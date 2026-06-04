package indexer

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// processExecAPIs maps a callee — the dotted path as it appears in an
// unresolved call target — to the canonical process-execution mechanism
// it represents. Covers the os/exec, subprocess, child_process, and
// process-builder families across Go, Python, JS/TS, Rust, and Java.
var processExecAPIs = map[string]string{
	// Go
	"exec.Command": "exec.Command", "exec.CommandContext": "exec.CommandContext",
	"os.StartProcess": "os.StartProcess", "syscall.Exec": "syscall.Exec",
	"syscall.ForkExec": "syscall.ForkExec",
	// Python
	"subprocess.run": "subprocess.run", "subprocess.Popen": "subprocess.Popen",
	"subprocess.call": "subprocess.call", "subprocess.check_call": "subprocess.check_call",
	"subprocess.check_output": "subprocess.check_output", "subprocess.getoutput": "subprocess.getoutput",
	"os.system": "os.system", "os.popen": "os.popen",
	// JS / TS
	"child_process.exec": "child_process.exec", "child_process.execSync": "child_process.execSync",
	"child_process.spawn": "child_process.spawn", "child_process.spawnSync": "child_process.spawnSync",
	"child_process.execFile": "child_process.execFile", "child_process.execFileSync": "child_process.execFileSync",
	"child_process.fork": "child_process.fork",
	// Rust
	"Command::new": "Command::new", "process::Command::new": "Command::new",
	// Java
	"Runtime.exec": "Runtime.exec",
}

// processExecBareNames are unqualified callee names that strongly imply
// process execution regardless of receiver — PHP shell builtins and the
// destructured-import JS forms (const {execSync} = require('child_process')).
// Generic words (system, exec, spawn, popen) are intentionally excluded to
// avoid false positives on unrelated same-named calls.
var processExecBareNames = map[string]string{
	"shell_exec": "shell_exec", "passthru": "passthru", "proc_open": "proc_open",
	"execSync": "child_process.execSync", "spawnSync": "child_process.spawnSync",
	"execFileSync": "child_process.execFileSync",
}

// processExecMechanism returns the canonical process-execution mechanism
// for a callee, or "" when the callee is not a recognised exec API.
func processExecMechanism(callee string) string {
	if m := processExecAPIs[callee]; m != "" {
		return m
	}
	last := callee
	if i := strings.LastIndexAny(callee, "."); i >= 0 {
		last = callee[i+1:]
	}
	return processExecBareNames[last]
}

// synthesizeCapabilityEdges materialises the three first-class capability
// edge kinds (NEW-KNW-3) from edges the language extractors already emit,
// so a supply-chain / least-privilege audit can traverse one edge kind
// instead of joining through the config, dataflow, and call layers:
//
//   - EdgeReadsEnv: every reads_config edge whose target is a cfg::env::
//     node, re-pointed at the same typed env-var node.
//   - EdgeAccessesField: every reads / writes edge that lands on a
//     KindField node, with Meta["access"] = read|write.
//   - EdgeExecutesProcess: every calls edge whose callee is a known
//     process-exec API, pointed at a synthetic typed process node
//     (string::process::<mechanism>).
//
// It runs after the resolver (so call/field targets are settled) and is
// idempotent — AddEdge dedupes by edge key and a reindex re-derives from
// the current base edges. Returns per-kind counts for telemetry.
func synthesizeCapabilityEdges(g graph.Store) (readsEnv, execProc, fieldAccess int) {
	if g == nil {
		return 0, 0, 0
	}
	g.ResolveMutex().Lock()
	defer g.ResolveMutex().Unlock()

	type edgeSpec struct {
		from, to, origin, file string
		line                   int
		kind                   graph.EdgeKind
		meta                   map[string]any
	}
	var pending []edgeSpec
	seen := map[string]bool{}
	add := func(from, to string, kind graph.EdgeKind, origin, file string, line int, meta map[string]any) bool {
		key := string(kind) + "\x00" + from + "\x00" + to
		if seen[key] {
			return false
		}
		seen[key] = true
		pending = append(pending, edgeSpec{from, to, origin, file, line, kind, meta})
		return true
	}

	// reads_env — parallel to reads_config edges that target an env key.
	for e := range g.EdgesByKind(graph.EdgeReadsConfig) {
		if e == nil || !strings.Contains(e.To, "cfg::env::") {
			continue
		}
		if add(e.From, e.To, graph.EdgeReadsEnv, graph.OriginASTResolved, e.FilePath, e.Line, nil) {
			readsEnv++
		}
	}

	// accesses_field — reads / writes that land on a struct field. Build
	// the KindField id set once instead of a GetNode per edge (cheap on
	// the disk-backed store).
	fieldIDs := map[string]bool{}
	for n := range g.NodesByKind(graph.KindField) {
		if n != nil {
			fieldIDs[n.ID] = true
		}
	}
	for _, base := range []graph.EdgeKind{graph.EdgeReads, graph.EdgeWrites} {
		mode := "read"
		if base == graph.EdgeWrites {
			mode = "write"
		}
		for e := range g.EdgesByKind(base) {
			if e == nil || !fieldIDs[e.To] {
				continue
			}
			if add(e.From, e.To, graph.EdgeAccessesField, graph.OriginASTResolved, e.FilePath, e.Line, map[string]any{"access": mode}) {
				fieldAccess++
			}
		}
	}

	// executes_process — calls to a known process-exec API, pointed at a
	// shared synthetic process node per mechanism.
	procNodes := map[string]*graph.Node{}
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil {
			continue
		}
		mech := processExecMechanism(strings.TrimPrefix(e.To, "unresolved::"))
		if mech == "" {
			continue
		}
		procID := "string::process::" + mech
		if procNodes[procID] == nil {
			procNodes[procID] = &graph.Node{
				ID: procID, Kind: graph.KindString, Name: mech,
				Meta: map[string]any{"context": "process", "mechanism": mech},
			}
		}
		if add(e.From, procID, graph.EdgeExecutesProcess, graph.OriginASTInferred, e.FilePath, e.Line, nil) {
			execProc++
		}
	}

	for _, n := range procNodes {
		g.AddNode(n)
	}
	for _, s := range pending {
		g.AddEdge(&graph.Edge{
			From: s.from, To: s.to, Kind: s.kind,
			FilePath: s.file, Line: s.line, Origin: s.origin, Meta: s.meta,
		})
	}
	return readsEnv, execProc, fieldAccess
}
