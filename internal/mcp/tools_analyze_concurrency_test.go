package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

// concurrencyServer wires a Server around a hand-built graph so the
// tests can pin exactly which spawn / send / write / call edges
// exist. The fixture is intentionally graph-level (not source-level)
// because the analyzers are language-agnostic and run on edges, not
// on parser output.
func concurrencyServer(t *testing.T) *Server {
	t.Helper()
	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Default()
	idx := indexer.New(g, reg, cfg.Index, zap.NewNop())
	eng := query.NewEngine(g)
	return NewServer(eng, g, idx, nil, zap.NewNop(), nil)
}

func addFn(g graph.Store, id, name, path string) {
	g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: name, FilePath: path, Language: "go"})
}

func addField(g graph.Store, id, name, path string) {
	g.AddNode(&graph.Node{ID: id, Kind: graph.KindField, Name: name, FilePath: path, Language: "go"})
}

func addEdge(g graph.Store, from, to string, kind graph.EdgeKind, path string, line int) {
	g.AddEdge(&graph.Edge{From: from, To: to, Kind: kind, FilePath: path, Line: line, Confidence: 1})
}

// TestRaceWrites_FlagsUnguardedFieldWriteFromGoroutine: the
// canonical positive case. spawnRoot runs in a goroutine and writes
// to a field with no detected lock guard.
func TestRaceWrites_FlagsUnguardedFieldWriteFromGoroutine(t *testing.T) {
	srv := concurrencyServer(t)
	addFn(srv.graph, "main.go::Main", "Main", "main.go")
	addFn(srv.graph, "main.go::Worker", "Worker", "main.go")
	addField(srv.graph, "main.go::State.counter", "counter", "main.go")
	addEdge(srv.graph, "main.go::Main", "main.go::Worker", graph.EdgeSpawns, "main.go", 10)
	addEdge(srv.graph, "main.go::Worker", "main.go::State.counter", graph.EdgeWrites, "main.go", 20)

	res, err := srv.handleAnalyzeRaceWrites(context.Background(), mcplib.CallToolRequest{})
	require.NoError(t, err)
	require.False(t, res.IsError)

	text := res.Content[0].(mcplib.TextContent).Text
	var payload struct {
		Total      int `json:"total"`
		RaceWrites []struct {
			Field    string `json:"field"`
			Writer   string `json:"writer"`
			FilePath string `json:"file_path"`
			Line     int    `json:"line"`
		} `json:"race_writes"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &payload))
	require.Equal(t, 1, payload.Total)
	assert.Equal(t, "main.go::State.counter", payload.RaceWrites[0].Field)
	assert.Equal(t, "main.go::Worker", payload.RaceWrites[0].Writer)
	assert.Equal(t, 20, payload.RaceWrites[0].Line)
}

// TestRaceWrites_SuppressedByLockGuard: the writer calls Lock() in
// the same function, so the analyzer must not flag the write.
// Catches the guard-cache logic AND the per-name lock detection.
func TestRaceWrites_SuppressedByLockGuard(t *testing.T) {
	srv := concurrencyServer(t)
	addFn(srv.graph, "main.go::Main", "Main", "main.go")
	addFn(srv.graph, "main.go::Worker", "Worker", "main.go")
	addField(srv.graph, "main.go::State.counter", "counter", "main.go")
	// Pretend Worker calls (*sync.Mutex).Lock — the call edge's
	// target name is "Lock", which the lock-guard heuristic catches.
	srv.graph.AddNode(&graph.Node{ID: "sync::Mutex.Lock", Kind: graph.KindMethod, Name: "Lock", FilePath: "sync/mutex.go"})
	addEdge(srv.graph, "main.go::Main", "main.go::Worker", graph.EdgeSpawns, "main.go", 10)
	addEdge(srv.graph, "main.go::Worker", "sync::Mutex.Lock", graph.EdgeCalls, "main.go", 18)
	addEdge(srv.graph, "main.go::Worker", "main.go::State.counter", graph.EdgeWrites, "main.go", 20)

	res, _ := srv.handleAnalyzeRaceWrites(context.Background(), mcplib.CallToolRequest{})
	require.False(t, res.IsError)
	var payload struct {
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &payload))
	assert.Equal(t, 0, payload.Total, "Lock()-guarded write must not be flagged")
}

// TestRaceWrites_IgnoresMainThreadWrites: a write that happens
// outside any goroutine-reachable function is not racy — must not
// appear in the report.
func TestRaceWrites_IgnoresMainThreadWrites(t *testing.T) {
	srv := concurrencyServer(t)
	addFn(srv.graph, "main.go::Main", "Main", "main.go")
	addField(srv.graph, "main.go::State.counter", "counter", "main.go")
	addEdge(srv.graph, "main.go::Main", "main.go::State.counter", graph.EdgeWrites, "main.go", 10)

	res, _ := srv.handleAnalyzeRaceWrites(context.Background(), mcplib.CallToolRequest{})
	require.False(t, res.IsError)
	var payload struct {
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &payload))
	assert.Equal(t, 0, payload.Total, "main-thread writes must not appear in race_writes")
}

// TestRaceWrites_TransitiveGoroutineReach: the writer is reached
// via EdgeCalls from a goroutine — must still flag, proving the
// reach closure isn't limited to the immediate spawn target.
func TestRaceWrites_TransitiveGoroutineReach(t *testing.T) {
	srv := concurrencyServer(t)
	addFn(srv.graph, "main.go::Main", "Main", "main.go")
	addFn(srv.graph, "main.go::Worker", "Worker", "main.go")
	addFn(srv.graph, "main.go::Helper", "Helper", "main.go")
	addField(srv.graph, "main.go::State.counter", "counter", "main.go")
	addEdge(srv.graph, "main.go::Main", "main.go::Worker", graph.EdgeSpawns, "main.go", 5)
	addEdge(srv.graph, "main.go::Worker", "main.go::Helper", graph.EdgeCalls, "main.go", 7)
	addEdge(srv.graph, "main.go::Helper", "main.go::State.counter", graph.EdgeWrites, "main.go", 12)

	res, _ := srv.handleAnalyzeRaceWrites(context.Background(), mcplib.CallToolRequest{})
	require.False(t, res.IsError)
	var payload struct {
		Total      int `json:"total"`
		RaceWrites []struct {
			Writer string `json:"writer"`
		} `json:"race_writes"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &payload))
	require.Equal(t, 1, payload.Total, "transitively-reached writer must still be flagged")
	assert.Equal(t, "main.go::Helper", payload.RaceWrites[0].Writer)
}

// TestRaceWrites_OnlyKindFieldTargets: a write to a local variable
// or a non-field node must not appear in the report.
func TestRaceWrites_OnlyKindFieldTargets(t *testing.T) {
	srv := concurrencyServer(t)
	addFn(srv.graph, "main.go::Main", "Main", "main.go")
	addFn(srv.graph, "main.go::Worker", "Worker", "main.go")
	srv.graph.AddNode(&graph.Node{
		ID: "main.go::localVar", Kind: graph.KindVariable, Name: "localVar",
		FilePath: "main.go", Language: "go",
	})
	addEdge(srv.graph, "main.go::Main", "main.go::Worker", graph.EdgeSpawns, "main.go", 5)
	addEdge(srv.graph, "main.go::Worker", "main.go::localVar", graph.EdgeWrites, "main.go", 8)

	res, _ := srv.handleAnalyzeRaceWrites(context.Background(), mcplib.CallToolRequest{})
	require.False(t, res.IsError)
	var payload struct {
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &payload))
	assert.Equal(t, 0, payload.Total, "variable writes must not appear in race_writes")
}

// TestUnclosedChannels_FlagsMultiSenderFanInWithoutClose: the
// canonical positive case — a fan-in channel with multiple senders
// and a ranging receiver, nobody calls close().
func TestUnclosedChannels_FlagsMultiSenderFanInWithoutClose(t *testing.T) {
	srv := concurrencyServer(t)
	addFn(srv.graph, "main.go::SenderA", "SenderA", "main.go")
	addFn(srv.graph, "main.go::SenderB", "SenderB", "main.go")
	addFn(srv.graph, "main.go::Receiver", "Receiver", "main.go")
	addEdge(srv.graph, "main.go::SenderA", "main.go::resultsCh", graph.EdgeSends, "main.go", 10)
	addEdge(srv.graph, "main.go::SenderB", "main.go::resultsCh", graph.EdgeSends, "main.go", 15)
	addEdge(srv.graph, "main.go::Receiver", "main.go::resultsCh", graph.EdgeRecvs, "main.go", 20)

	res, _ := srv.handleAnalyzeUnclosedChannels(context.Background(), mcplib.CallToolRequest{})
	require.False(t, res.IsError)
	var payload struct {
		Total            int `json:"total"`
		UnclosedChannels []struct {
			Channel string `json:"channel"`
			Sends   int    `json:"sends"`
			Senders int    `json:"senders"`
			Recvs   int    `json:"recvs"`
			Risk    string `json:"risk"`
		} `json:"unclosed_channels"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &payload))
	require.Equal(t, 1, payload.Total)
	row := payload.UnclosedChannels[0]
	assert.Equal(t, "main.go::resultsCh", row.Channel)
	assert.Equal(t, 2, row.Sends)
	assert.Equal(t, 2, row.Senders)
	assert.Equal(t, 1, row.Recvs)
	assert.Equal(t, "high", row.Risk)
}

// TestUnclosedChannels_SuppressedByCloseCall: the sender's function
// also calls close(), so the channel is "covered" and must not be
// flagged.
func TestUnclosedChannels_SuppressedByCloseCall(t *testing.T) {
	srv := concurrencyServer(t)
	addFn(srv.graph, "main.go::Sender", "Sender", "main.go")
	srv.graph.AddNode(&graph.Node{ID: "builtin::close", Kind: graph.KindFunction, Name: "close", FilePath: "builtin"})
	addEdge(srv.graph, "main.go::Sender", "main.go::ch", graph.EdgeSends, "main.go", 5)
	addEdge(srv.graph, "main.go::Sender", "builtin::close", graph.EdgeCalls, "main.go", 6)

	res, _ := srv.handleAnalyzeUnclosedChannels(context.Background(), mcplib.CallToolRequest{})
	require.False(t, res.IsError)
	var payload struct {
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &payload))
	assert.Equal(t, 0, payload.Total, "channel with close()-calling sender must not be flagged")
}

// TestUnclosedChannels_PureReceiveSkipped: a channel that only
// receives (no sends in this scope) is not the analyzer's
// responsibility — closing falls to the sender side.
func TestUnclosedChannels_PureReceiveSkipped(t *testing.T) {
	srv := concurrencyServer(t)
	addFn(srv.graph, "main.go::Receiver", "Receiver", "main.go")
	addEdge(srv.graph, "main.go::Receiver", "extern::ch", graph.EdgeRecvs, "main.go", 5)

	res, _ := srv.handleAnalyzeUnclosedChannels(context.Background(), mcplib.CallToolRequest{})
	require.False(t, res.IsError)
	var payload struct {
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &payload))
	assert.Equal(t, 0, payload.Total, "pure-receive channel must not be flagged")
}

// TestUnclosedChannels_RiskClassification: validates the
// high/medium/low risk assignment matches the classifyUnclosed rules.
func TestUnclosedChannels_RiskClassification(t *testing.T) {
	srv := concurrencyServer(t)
	addFn(srv.graph, "main.go::S1", "S1", "main.go")
	addFn(srv.graph, "main.go::S2", "S2", "main.go")
	addFn(srv.graph, "main.go::R", "R", "main.go")

	// HIGH: 2 senders + 1 receiver, no close.
	addEdge(srv.graph, "main.go::S1", "main.go::highCh", graph.EdgeSends, "main.go", 1)
	addEdge(srv.graph, "main.go::S2", "main.go::highCh", graph.EdgeSends, "main.go", 2)
	addEdge(srv.graph, "main.go::R", "main.go::highCh", graph.EdgeRecvs, "main.go", 3)

	// MEDIUM: 1 sender + 1 receiver, no close.
	addEdge(srv.graph, "main.go::S1", "main.go::medCh", graph.EdgeSends, "main.go", 10)
	addEdge(srv.graph, "main.go::R", "main.go::medCh", graph.EdgeRecvs, "main.go", 11)

	// LOW: 1 sender, no receiver (fire-and-forget signal).
	addEdge(srv.graph, "main.go::S2", "main.go::lowCh", graph.EdgeSends, "main.go", 20)

	res, _ := srv.handleAnalyzeUnclosedChannels(context.Background(), mcplib.CallToolRequest{})
	require.False(t, res.IsError)
	var payload struct {
		Total            int `json:"total"`
		UnclosedChannels []struct {
			Channel string `json:"channel"`
			Risk    string `json:"risk"`
		} `json:"unclosed_channels"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &payload))
	require.Equal(t, 3, payload.Total)
	byCh := map[string]string{}
	for _, r := range payload.UnclosedChannels {
		byCh[r.Channel] = r.Risk
	}
	assert.Equal(t, "high", byCh["main.go::highCh"])
	assert.Equal(t, "medium", byCh["main.go::medCh"])
	assert.Equal(t, "low", byCh["main.go::lowCh"])
	// High-risk row must sort first under risk-rank ordering.
	assert.Equal(t, "main.go::highCh", payload.UnclosedChannels[0].Channel,
		"high-risk rows must sort first")
}

// TestAnalyzeDispatcher_RoutesNewKinds asserts the analyze switch
// statement accepts kind=race_writes and kind=unclosed_channels.
// Regression-protects the dispatcher wiring against a stray rename.
func TestAnalyzeDispatcher_RoutesNewKinds(t *testing.T) {
	srv, _ := setupTestServer(t)

	for _, kind := range []string{"race_writes", "unclosed_channels"} {
		t.Run(kind, func(t *testing.T) {
			req := mcplib.CallToolRequest{}
			req.Params.Name = "analyze"
			req.Params.Arguments = map[string]any{"kind": kind}
			res, err := srv.handleAnalyze(context.Background(), req)
			require.NoError(t, err)
			require.False(t, res.IsError,
				"dispatcher must route kind=%s without error; got %v", kind, res)
		})
	}
}

// TestAnalyzeRaceWrites_GCXEncodesRow asserts the GCX1 wire output
// includes the race_writes column header. Wire-format clients
// (gcx-go / gcx-ts) decode by header so a missing column breaks
// downstream consumers silently.
func TestAnalyzeRaceWrites_GCXEncodesRow(t *testing.T) {
	srv := concurrencyServer(t)
	addFn(srv.graph, "main.go::Main", "Main", "main.go")
	addFn(srv.graph, "main.go::Worker", "Worker", "main.go")
	addField(srv.graph, "main.go::State.counter", "counter", "main.go")
	addEdge(srv.graph, "main.go::Main", "main.go::Worker", graph.EdgeSpawns, "main.go", 5)
	addEdge(srv.graph, "main.go::Worker", "main.go::State.counter", graph.EdgeWrites, "main.go", 10)

	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{"format": "gcx"}
	res, err := srv.handleAnalyzeRaceWrites(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	text := res.Content[0].(mcplib.TextContent).Text
	assert.Contains(t, text, "analyze.race_writes")
	assert.Contains(t, text, "field")
	assert.Contains(t, text, "writer")
	assert.Contains(t, text, "reason")
}

// addMethod / addType / addTypedField build the node shapes the
// concurrency classifier reads: a method linked to its receiver type
// via EdgeMemberOf, and a typed field linked to its owning type.
func addMethod(g graph.Store, id, name, path string) {
	g.AddNode(&graph.Node{ID: id, Kind: graph.KindMethod, Name: name, FilePath: path, Language: "go"})
}

func addType(g graph.Store, id, name, path string) {
	g.AddNode(&graph.Node{ID: id, Kind: graph.KindType, Name: name, FilePath: path, Language: "go"})
}

func addTypedField(g graph.Store, id, name, fieldType, path string) {
	g.AddNode(&graph.Node{
		ID: id, Kind: graph.KindField, Name: name, FilePath: path, Language: "go",
		Meta: map[string]any{"field_type": fieldType},
	})
}

// callerNote is the JSON shape of one get_callers caller_notes entry.
type callerNote struct {
	SyncGuarded        bool   `json:"sync_guarded"`
	SyncGuardedWhy     string `json:"sync_guarded_why"`
	CrossConcurrent    bool   `json:"cross_concurrent"`
	CrossConcurrentWhy string `json:"cross_concurrent_why"`
}

// getCallersWithConcurrency builds a graph with two callers of a target
// function — one a method on a mutex-holding type, one a method on a
// lock-free type — plus a goroutine-launched caller, then returns the
// caller_notes map from a get_callers call.
//
// Graph:
//
//	Guarded.Do  --calls-->  Target   (Guarded holds a sync.Mutex)
//	Free.Do     --calls-->  Target   (Free holds only an int)
//	Spawned     --calls-->  Target   (Spawned is itself spawned)
//	someEntry   --spawns--> Spawned
func getCallersWithConcurrency(t *testing.T) map[string]callerNote {
	t.Helper()
	srv := concurrencyServer(t)
	g := srv.graph

	addFn(g, "main.go::Target", "Target", "main.go")

	// Mutex-holding type with a method that calls Target.
	addType(g, "main.go::Guarded", "Guarded", "main.go")
	addTypedField(g, "main.go::Guarded.mu", "mu", "sync.Mutex", "main.go")
	addEdge(g, "main.go::Guarded.mu", "main.go::Guarded", graph.EdgeMemberOf, "main.go", 2)
	addMethod(g, "main.go::Guarded.Do", "Do", "main.go")
	addEdge(g, "main.go::Guarded.Do", "main.go::Guarded", graph.EdgeMemberOf, "main.go", 5)
	addEdge(g, "main.go::Guarded.Do", "main.go::Target", graph.EdgeCalls, "main.go", 6)

	// Lock-free type with a method that calls Target.
	addType(g, "main.go::Free", "Free", "main.go")
	addTypedField(g, "main.go::Free.n", "n", "int", "main.go")
	addEdge(g, "main.go::Free.n", "main.go::Free", graph.EdgeMemberOf, "main.go", 12)
	addMethod(g, "main.go::Free.Do", "Do", "main.go")
	addEdge(g, "main.go::Free.Do", "main.go::Free", graph.EdgeMemberOf, "main.go", 15)
	addEdge(g, "main.go::Free.Do", "main.go::Target", graph.EdgeCalls, "main.go", 16)

	// A caller that is itself launched as a goroutine.
	addFn(g, "main.go::Spawned", "Spawned", "main.go")
	addFn(g, "main.go::someEntry", "someEntry", "main.go")
	g.AddEdge(&graph.Edge{
		From: "main.go::someEntry", To: "main.go::Spawned", Kind: graph.EdgeSpawns,
		FilePath: "main.go", Line: 20, Confidence: 1, Meta: map[string]any{"mode": "goroutine"},
	})
	addEdge(g, "main.go::Spawned", "main.go::Target", graph.EdgeCalls, "main.go", 22)

	res := callTool(t, srv, "get_callers", map[string]any{"id": "main.go::Target"})
	require.False(t, res.IsError)
	var sg struct {
		CallerNotes map[string]callerNote `json:"caller_notes"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &sg))
	return sg.CallerNotes
}

// TestGetCallers_SyncGuardedAnnotation: a caller method on a
// mutex-holding type is flagged sync_guarded; a caller method on a
// lock-free type is not.
func TestGetCallers_SyncGuardedAnnotation(t *testing.T) {
	notes := getCallersWithConcurrency(t)

	guarded, ok := notes["main.go::Guarded.Do"]
	require.True(t, ok, "caller on a mutex-holding type must carry a caller_notes entry")
	assert.True(t, guarded.SyncGuarded, "method on a sync.Mutex-holding type must be sync_guarded")
	assert.NotEmpty(t, guarded.SyncGuardedWhy, "sync_guarded must carry an explanation")

	// A caller on a lock-free type carries no flag, so it is absent
	// from caller_notes entirely.
	free, present := notes["main.go::Free.Do"]
	if present {
		assert.False(t, free.SyncGuarded, "method on a lock-free type must not be sync_guarded")
	}
}

// TestGetCallers_CrossConcurrentAnnotation: a caller that is itself
// spawned as a goroutine is flagged cross_concurrent; a plainly-called
// caller is not.
func TestGetCallers_CrossConcurrentAnnotation(t *testing.T) {
	notes := getCallersWithConcurrency(t)

	spawned, ok := notes["main.go::Spawned"]
	require.True(t, ok, "goroutine-launched caller must carry a caller_notes entry")
	assert.True(t, spawned.CrossConcurrent, "goroutine-launched caller must be cross_concurrent")
	assert.NotEmpty(t, spawned.CrossConcurrentWhy, "cross_concurrent must carry an explanation")

	// Guarded.Do is called plainly (not spawned) — must not be flagged
	// cross_concurrent even though it does carry sync_guarded.
	guarded := notes["main.go::Guarded.Do"]
	assert.False(t, guarded.CrossConcurrent,
		"a plainly-called caller must not be cross_concurrent")
}

// TestGetCallers_GCXEncodesCallerNotes asserts the GCX1 wire output
// carries the caller_notes section header and its columns so
// wire-format clients can decode the annotation.
func TestGetCallers_GCXEncodesCallerNotes(t *testing.T) {
	srv := concurrencyServer(t)
	g := srv.graph
	addFn(g, "main.go::Target", "Target", "main.go")
	addType(g, "main.go::Guarded", "Guarded", "main.go")
	addTypedField(g, "main.go::Guarded.mu", "mu", "sync.RWMutex", "main.go")
	addEdge(g, "main.go::Guarded.mu", "main.go::Guarded", graph.EdgeMemberOf, "main.go", 2)
	addMethod(g, "main.go::Guarded.Do", "Do", "main.go")
	addEdge(g, "main.go::Guarded.Do", "main.go::Guarded", graph.EdgeMemberOf, "main.go", 5)
	addEdge(g, "main.go::Guarded.Do", "main.go::Target", graph.EdgeCalls, "main.go", 6)

	req := mcplib.CallToolRequest{}
	req.Params.Name = "get_callers"
	req.Params.Arguments = map[string]any{"id": "main.go::Target", "format": "gcx"}
	res, err := srv.handleGetCallers(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	text := res.Content[0].(mcplib.TextContent).Text
	assert.Contains(t, text, "get_callers.caller_notes")
	assert.Contains(t, text, "sync_guarded")
	assert.Contains(t, text, "cross_concurrent")
}

// TestGetCallers_NoConcurrencyOmitsCallerNotes: when no caller carries
// a concurrency flag, the caller_notes field is absent from the
// response — the annotation is opt-in, not noise on every result.
func TestGetCallers_NoConcurrencyOmitsCallerNotes(t *testing.T) {
	srv := concurrencyServer(t)
	g := srv.graph
	addFn(g, "main.go::Target", "Target", "main.go")
	addFn(g, "main.go::Plain", "Plain", "main.go")
	addEdge(g, "main.go::Plain", "main.go::Target", graph.EdgeCalls, "main.go", 6)

	res := callTool(t, srv, "get_callers", map[string]any{"id": "main.go::Target"})
	require.False(t, res.IsError)
	text := res.Content[0].(mcplib.TextContent).Text
	assert.NotContains(t, text, "caller_notes",
		"caller_notes must be omitted when no caller carries a concurrency flag")
}

// TestAnalyzeGoroutineSpawns_ConcurrencyAnnotation: the goroutine_spawns
// analyzer surface carries the shared classification on each spawn
// target — sync_guarded when the goroutine body is a method on a
// lock-holding type, cross_concurrent because it is a spawn target.
func TestAnalyzeGoroutineSpawns_ConcurrencyAnnotation(t *testing.T) {
	srv := concurrencyServer(t)
	g := srv.graph

	// A goroutine body that is a method on a mutex-holding type.
	addType(g, "main.go::Worker", "Worker", "main.go")
	addTypedField(g, "main.go::Worker.mu", "mu", "sync.Mutex", "main.go")
	addEdge(g, "main.go::Worker.mu", "main.go::Worker", graph.EdgeMemberOf, "main.go", 2)
	addMethod(g, "main.go::Worker.Run", "Run", "main.go")
	addEdge(g, "main.go::Worker.Run", "main.go::Worker", graph.EdgeMemberOf, "main.go", 5)
	addFn(g, "main.go::Main", "Main", "main.go")
	g.AddEdge(&graph.Edge{
		From: "main.go::Main", To: "main.go::Worker.Run", Kind: graph.EdgeSpawns,
		FilePath: "main.go", Line: 9, Confidence: 1, Meta: map[string]any{"mode": "goroutine"},
	})

	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{"kind": "goroutine_spawns"}
	res, err := srv.handleAnalyzeGoroutineSpawns(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	var payload struct {
		Spawns []struct {
			Target      string      `json:"target"`
			Concurrency *callerNote `json:"concurrency"`
		} `json:"spawns"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &payload))
	require.Len(t, payload.Spawns, 1)
	row := payload.Spawns[0]
	assert.Equal(t, "main.go::Worker.Run", row.Target)
	require.NotNil(t, row.Concurrency, "spawn target on a mutex type must carry a concurrency block")
	assert.True(t, row.Concurrency.SyncGuarded, "goroutine body on a mutex type must be sync_guarded")
	assert.True(t, row.Concurrency.CrossConcurrent, "a spawn target must be cross_concurrent")
}

// TestAnalyzeGoroutineSpawns_GCXEncodesConcurrency asserts the GCX1
// wire output for goroutine_spawns carries the concurrency columns.
func TestAnalyzeGoroutineSpawns_GCXEncodesConcurrency(t *testing.T) {
	srv := concurrencyServer(t)
	g := srv.graph
	addFn(g, "main.go::Main", "Main", "main.go")
	addFn(g, "main.go::bg", "bg", "main.go")
	g.AddEdge(&graph.Edge{
		From: "main.go::Main", To: "main.go::bg", Kind: graph.EdgeSpawns,
		FilePath: "main.go", Line: 9, Confidence: 1, Meta: map[string]any{"mode": "goroutine"},
	})

	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{"format": "gcx"}
	res, err := srv.handleAnalyzeGoroutineSpawns(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	text := res.Content[0].(mcplib.TextContent).Text
	assert.Contains(t, text, "analyze.goroutine_spawns")
	assert.Contains(t, text, "sync_guarded")
	assert.Contains(t, text, "cross_concurrent")
}
