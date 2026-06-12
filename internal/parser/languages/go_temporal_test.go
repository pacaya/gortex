package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// temporalEdgesByVia returns every EdgeCalls edge tagged with the given
// `via` value (e.g. "temporal.stub" or "temporal.register").
func temporalEdgesByVia(fix *extractedFixture, via string) []*graph.Edge {
	var found []*graph.Edge
	for _, e := range fix.edgesByKind[graph.EdgeCalls] {
		if e.Meta != nil && e.Meta["via"] == via {
			found = append(found, e)
		}
	}
	return found
}

func TestGoTemporal_ExecuteActivity_IdentifierName(t *testing.T) {
	fix := runGoExtract(t, `package wf

import "go.temporal.io/sdk/workflow"

func OrderWorkflow(ctx workflow.Context, id string) error {
	workflow.ExecuteActivity(ctx, ChargeCard, id)
	return nil
}
`)
	edges := temporalEdgesByVia(fix, "temporal.stub")
	require.Len(t, edges, 1)
	e := edges[0]
	assert.Equal(t, "unresolved::temporal::activity::ChargeCard", e.To)
	assert.Equal(t, "activity", e.Meta["temporal_kind"])
	assert.Equal(t, "ChargeCard", e.Meta["temporal_name"])
	_, isLocal := e.Meta["temporal_local"]
	assert.False(t, isLocal, "ExecuteActivity must not flag temporal_local")
}

func TestGoTemporal_ExecuteActivity_StringLiteralName(t *testing.T) {
	fix := runGoExtract(t, `package wf

import "go.temporal.io/sdk/workflow"

func WF(ctx workflow.Context) {
	workflow.ExecuteActivity(ctx, "RemoteActivity", nil)
}
`)
	edges := temporalEdgesByVia(fix, "temporal.stub")
	require.Len(t, edges, 1)
	assert.Equal(t, "unresolved::temporal::activity::RemoteActivity", edges[0].To)
	assert.Equal(t, "RemoteActivity", edges[0].Meta["temporal_name"])
}

func TestGoTemporal_ExecuteActivity_SelectorName(t *testing.T) {
	// `workflow.ExecuteActivity(ctx, pkg.Charge, ...)` → name is "Charge"
	// (the trailing identifier of the selector).
	fix := runGoExtract(t, `package wf

import (
	"go.temporal.io/sdk/workflow"
	"example.com/activities"
)

func WF(ctx workflow.Context) {
	workflow.ExecuteActivity(ctx, activities.Charge, 1)
}
`)
	edges := temporalEdgesByVia(fix, "temporal.stub")
	require.Len(t, edges, 1)
	assert.Equal(t, "unresolved::temporal::activity::Charge", edges[0].To)
}

func TestGoTemporal_ExecuteLocalActivity_FlagsTemporalLocal(t *testing.T) {
	fix := runGoExtract(t, `package wf

import "go.temporal.io/sdk/workflow"

func WF(ctx workflow.Context) {
	workflow.ExecuteLocalActivity(ctx, Lookup, "k")
}
`)
	edges := temporalEdgesByVia(fix, "temporal.stub")
	require.Len(t, edges, 1)
	e := edges[0]
	assert.Equal(t, "activity", e.Meta["temporal_kind"])
	assert.Equal(t, true, e.Meta["temporal_local"], "ExecuteLocalActivity must flag temporal_local")
}

func TestGoTemporal_ExecuteChildWorkflow_KindIsWorkflow(t *testing.T) {
	fix := runGoExtract(t, `package wf

import "go.temporal.io/sdk/workflow"

func Parent(ctx workflow.Context) {
	workflow.ExecuteChildWorkflow(ctx, ChildWorkflow, 42)
}
`)
	edges := temporalEdgesByVia(fix, "temporal.stub")
	require.Len(t, edges, 1)
	assert.Equal(t, "unresolved::temporal::workflow::ChildWorkflow", edges[0].To)
	assert.Equal(t, "workflow", edges[0].Meta["temporal_kind"])
}

func TestGoTemporal_RegisterActivity(t *testing.T) {
	fix := runGoExtract(t, `package main

func setup(w Worker) {
	w.RegisterActivity(ChargeCard)
}
`)
	edges := temporalEdgesByVia(fix, "temporal.register")
	require.Len(t, edges, 1)
	e := edges[0]
	assert.Equal(t, "activity", e.Meta["temporal_kind"])
	assert.Equal(t, "ChargeCard", e.Meta["temporal_name"])
}

func TestGoTemporal_RegisterActivityWithOptions(t *testing.T) {
	fix := runGoExtract(t, `package main

import "go.temporal.io/sdk/activity"

func setup(w Worker) {
	w.RegisterActivityWithOptions(ChargeCard, activity.RegisterOptions{Name: "Charge"})
}
`)
	edges := temporalEdgesByVia(fix, "temporal.register")
	require.Len(t, edges, 1)
	assert.Equal(t, "activity", edges[0].Meta["temporal_kind"])
	assert.Equal(t, "ChargeCard", edges[0].Meta["temporal_name"],
		"temporal_name keeps the function-reference identifier")
	assert.Equal(t, "Charge", edges[0].Meta["temporal_registered_name"],
		"RegisterOptions{Name} override is captured as the registered name")
}

func TestGoTemporal_RegisterWorkflow(t *testing.T) {
	fix := runGoExtract(t, `package main

func setup(w Worker) {
	w.RegisterWorkflow(OrderWorkflow)
}
`)
	edges := temporalEdgesByVia(fix, "temporal.register")
	require.Len(t, edges, 1)
	assert.Equal(t, "workflow", edges[0].Meta["temporal_kind"])
	assert.Equal(t, "OrderWorkflow", edges[0].Meta["temporal_name"])
}

func TestGoTemporal_OtherWorkflowMethodNotStubbed(t *testing.T) {
	// `workflow.Sleep` / `workflow.Now` / etc. must NOT be stamped as
	// temporal.stub — only the four explicit dispatch helpers are.
	fix := runGoExtract(t, `package wf

import "go.temporal.io/sdk/workflow"

func WF(ctx workflow.Context) {
	workflow.Sleep(ctx, 5)
	workflow.Now(ctx)
}
`)
	assert.Empty(t, temporalEdgesByVia(fix, "temporal.stub"),
		"only ExecuteActivity / ExecuteLocalActivity / ExecuteChildWorkflow should be stub-tagged")
}

func TestGoTemporal_AliasedImportDetected(t *testing.T) {
	// An aliased `import wf "go.temporal.io/sdk/workflow"` is resolved from
	// the file's import table and canonicalised to the "workflow" receiver,
	// so dispatch through the alias is detected.
	fix := runGoExtract(t, `package wf

import wf "go.temporal.io/sdk/workflow"

func WF(ctx wf.Context) {
	wf.ExecuteActivity(ctx, Charge, 1)
}
`)
	edges := temporalEdgesByVia(fix, "temporal.stub")
	require.Len(t, edges, 1)
	assert.Equal(t, "Charge", edges[0].Meta["temporal_name"])
}

func TestGoTemporal_NonWorkflowReceiverStillIgnored(t *testing.T) {
	// A same-named receiver that is NOT the workflow package alias must
	// not be misread as a dispatch — the alias gate only canonicalises the
	// actual workflow import.
	fix := runGoExtract(t, `package wf

import wf "go.temporal.io/sdk/workflow"

func WF(ctx wf.Context, other Helper) {
	other.ExecuteActivity(ctx, Charge, 1)
}
`)
	assert.Empty(t, temporalEdgesByVia(fix, "temporal.stub"),
		"a non-workflow receiver must not be detected even when workflow is aliased")
}

func TestGoTemporal_StubAndRegisterCoexistInSameFile(t *testing.T) {
	fix := runGoExtract(t, `package main

import "go.temporal.io/sdk/workflow"

func Charge() error { return nil }

func WF(ctx workflow.Context) {
	workflow.ExecuteActivity(ctx, Charge, 1)
}

func setup(w Worker) {
	w.RegisterActivity(Charge)
	w.RegisterWorkflow(WF)
}
`)
	stubs := temporalEdgesByVia(fix, "temporal.stub")
	registers := temporalEdgesByVia(fix, "temporal.register")
	require.Len(t, stubs, 1)
	require.Len(t, registers, 2)
}

// --- Dispatch name from an env-var-with-literal-default variable -----
//
// When the activity / workflow name is a local variable read from an
// env var with a literal fallback, resolve to the literal default and
// flag the stub edge `temporal_name_origin=env_default` so the resolver
// lands it at the speculative tier (the runtime env override may differ
// from the default). Anchored on a literal os.Getenv / os.LookupEnv read
// so the value is provably env-sourced — no general data-flow guessing.

func TestGoTemporal_ExecuteActivity_EnvDefault_CmpOr(t *testing.T) {
	fix := runGoExtract(t, `package wf

import (
	"cmp"
	"os"
	"go.temporal.io/sdk/workflow"
)

func WF(ctx workflow.Context) {
	actName := cmp.Or(os.Getenv("CHARGE_ACTIVITY"), "ChargeCard")
	workflow.ExecuteActivity(ctx, actName, 1)
}
`)
	edges := temporalEdgesByVia(fix, "temporal.stub")
	require.Len(t, edges, 1)
	e := edges[0]
	assert.Equal(t, "unresolved::temporal::activity::ChargeCard", e.To,
		"name must resolve to the literal default, not the variable identifier")
	assert.Equal(t, "ChargeCard", e.Meta["temporal_name"])
	assert.Equal(t, "env_default", e.Meta["temporal_name_origin"])
}

func TestGoTemporal_ExecuteActivity_EnvDefault_IfEmpty(t *testing.T) {
	fix := runGoExtract(t, `package wf

import (
	"os"
	"go.temporal.io/sdk/workflow"
)

func WF(ctx workflow.Context) {
	name := os.Getenv("CHARGE_ACTIVITY")
	if name == "" {
		name = "ChargeCard"
	}
	workflow.ExecuteActivity(ctx, name, 1)
}
`)
	edges := temporalEdgesByVia(fix, "temporal.stub")
	require.Len(t, edges, 1)
	assert.Equal(t, "unresolved::temporal::activity::ChargeCard", edges[0].To)
	assert.Equal(t, "ChargeCard", edges[0].Meta["temporal_name"])
	assert.Equal(t, "env_default", edges[0].Meta["temporal_name_origin"])
}

func TestGoTemporal_ExecuteActivity_PlainVarNotEnvDefault(t *testing.T) {
	// A variable NOT sourced from an env read keeps the existing
	// behaviour (trailing identifier as the name) and carries no
	// env_default flag — we don't guess at arbitrary variables.
	fix := runGoExtract(t, `package wf

import "go.temporal.io/sdk/workflow"

func WF(ctx workflow.Context, picked string) {
	actName := picked
	workflow.ExecuteActivity(ctx, actName, 1)
}
`)
	edges := temporalEdgesByVia(fix, "temporal.stub")
	require.Len(t, edges, 1)
	assert.Equal(t, "actName", edges[0].Meta["temporal_name"])
	_, flagged := edges[0].Meta["temporal_name_origin"]
	assert.False(t, flagged, "plain variable must not be flagged env_default")
}

func TestGoTemporal_ExecuteActivity_EnvReadNoLiteralDefault(t *testing.T) {
	// os.Getenv with no literal fallback can't be pinned to a name —
	// keep the variable identifier, no env_default flag.
	fix := runGoExtract(t, `package wf

import (
	"os"
	"go.temporal.io/sdk/workflow"
)

func WF(ctx workflow.Context) {
	name := os.Getenv("CHARGE_ACTIVITY")
	workflow.ExecuteActivity(ctx, name, 1)
}
`)
	edges := temporalEdgesByVia(fix, "temporal.stub")
	require.Len(t, edges, 1)
	assert.Equal(t, "name", edges[0].Meta["temporal_name"])
	_, flagged := edges[0].Meta["temporal_name_origin"]
	assert.False(t, flagged)
}

func TestGoTemporal_ExecuteActivity_EnvDefault_CmpOrFirstLiteral(t *testing.T) {
	// cmp.Or returns the FIRST non-zero argument, so with the env unset
	// the runtime value is the first literal ("First"), not the last.
	fix := runGoExtract(t, `package wf

import (
	"cmp"
	"os"
	"go.temporal.io/sdk/workflow"
)

func WF(ctx workflow.Context) {
	actName := cmp.Or(os.Getenv("A"), os.Getenv("B"), "First", "Second")
	workflow.ExecuteActivity(ctx, actName, 1)
}
`)
	edges := temporalEdgesByVia(fix, "temporal.stub")
	require.Len(t, edges, 1)
	assert.Equal(t, "First", edges[0].Meta["temporal_name"],
		"cmp.Or default must be the first literal, not the last")
	assert.Equal(t, "env_default", edges[0].Meta["temporal_name_origin"])
}

func TestGoTemporal_ExecuteActivity_NonCmpOrCalleeNotEnvDefault(t *testing.T) {
	// An arbitrary user function mixing an env read with a literal is NOT
	// the cmp.Or env-or-default idiom — keep the bare identifier, no flag.
	fix := runGoExtract(t, `package wf

import (
	"os"
	"go.temporal.io/sdk/workflow"
)

func combine(a, b string) string { return a + b }

func WF(ctx workflow.Context) {
	actName := combine(os.Getenv("K"), "Suffix")
	workflow.ExecuteActivity(ctx, actName, 1)
}
`)
	edges := temporalEdgesByVia(fix, "temporal.stub")
	require.Len(t, edges, 1)
	assert.Equal(t, "actName", edges[0].Meta["temporal_name"])
	_, flagged := edges[0].Meta["temporal_name_origin"]
	assert.False(t, flagged, "non-cmp.Or callee must not be treated as env_default")
}

func TestGoTemporal_ExecuteActivity_EnvDefaultOverwrittenNotFlagged(t *testing.T) {
	// A later plain reassignment is the live value at the call site; the
	// earlier env-default write must not win — leave it unresolved.
	fix := runGoExtract(t, `package wf

import (
	"cmp"
	"os"
	"go.temporal.io/sdk/workflow"
)

func pick() string { return "Other" }

func WF(ctx workflow.Context) {
	actName := cmp.Or(os.Getenv("K"), "ChargeCard")
	actName = pick()
	workflow.ExecuteActivity(ctx, actName, 1)
}
`)
	edges := temporalEdgesByVia(fix, "temporal.stub")
	require.Len(t, edges, 1)
	assert.Equal(t, "actName", edges[0].Meta["temporal_name"])
	_, flagged := edges[0].Meta["temporal_name_origin"]
	assert.False(t, flagged, "a later non-env reassignment must clear the env_default flag")
}

func TestGoTemporal_ExecuteActivity_ShadowInNestedClosureNotMatched(t *testing.T) {
	// A same-named variable declared in a nested closure is a different
	// scope; it must not be matched for the outer dispatch's name.
	fix := runGoExtract(t, `package wf

import (
	"cmp"
	"os"
	"go.temporal.io/sdk/workflow"
)

func run(f func()) { f() }

func WF(ctx workflow.Context, picked string) {
	actName := picked
	run(func() {
		actName := cmp.Or(os.Getenv("K"), "Inner")
		_ = actName
	})
	workflow.ExecuteActivity(ctx, actName, 1)
}
`)
	edges := temporalEdgesByVia(fix, "temporal.stub")
	require.Len(t, edges, 1)
	assert.Equal(t, "actName", edges[0].Meta["temporal_name"])
	_, flagged := edges[0].Meta["temporal_name_origin"]
	assert.False(t, flagged, "a shadowing var in a nested closure must not flag the outer dispatch")
}

// --- In-workflow handler declarations (query / signal / update) -----
//
// These mirror the Java SDK's @QueryMethod / @SignalMethod /
// @UpdateMethod annotations: from inside a workflow body the Go SDK
// declares the named query / signal / update channels the workflow
// serves. We surface each as a `via=temporal.handler` EdgeCalls edge
// carrying temporal_kind + temporal_name so the graph records, per
// workflow, the named handlers it exposes — symmetric with the Java
// side's per-method annotation edges.

func TestGoTemporal_SetQueryHandler(t *testing.T) {
	fix := runGoExtract(t, `package wf

import "go.temporal.io/sdk/workflow"

func OrderWorkflow(ctx workflow.Context) error {
	workflow.SetQueryHandler(ctx, "status", func() (string, error) { return "ok", nil })
	return nil
}
`)
	edges := temporalEdgesByVia(fix, "temporal.handler")
	require.Len(t, edges, 1)
	e := edges[0]
	assert.Equal(t, "query", e.Meta["temporal_kind"])
	assert.Equal(t, "status", e.Meta["temporal_name"])
	assert.Equal(t, "pkg/foo.go::OrderWorkflow", e.From,
		"handler edge must originate from the enclosing workflow function")
}

func TestGoTemporal_GetSignalChannel(t *testing.T) {
	fix := runGoExtract(t, `package wf

import "go.temporal.io/sdk/workflow"

func OrderWorkflow(ctx workflow.Context) error {
	ch := workflow.GetSignalChannel(ctx, "cancel")
	_ = ch
	return nil
}
`)
	edges := temporalEdgesByVia(fix, "temporal.handler")
	require.Len(t, edges, 1)
	assert.Equal(t, "signal", edges[0].Meta["temporal_kind"])
	assert.Equal(t, "cancel", edges[0].Meta["temporal_name"])
}

func TestGoTemporal_SetUpdateHandler(t *testing.T) {
	fix := runGoExtract(t, `package wf

import "go.temporal.io/sdk/workflow"

func OrderWorkflow(ctx workflow.Context) error {
	workflow.SetUpdateHandler(ctx, "retry", func() error { return nil })
	return nil
}
`)
	edges := temporalEdgesByVia(fix, "temporal.handler")
	require.Len(t, edges, 1)
	assert.Equal(t, "update", edges[0].Meta["temporal_kind"])
	assert.Equal(t, "retry", edges[0].Meta["temporal_name"])
}

func TestGoTemporal_SetUpdateHandlerWithOptions(t *testing.T) {
	fix := runGoExtract(t, `package wf

import "go.temporal.io/sdk/workflow"

func OrderWorkflow(ctx workflow.Context) error {
	workflow.SetUpdateHandlerWithOptions(ctx, "retry", func() error { return nil }, workflow.UpdateHandlerOptions{})
	return nil
}
`)
	edges := temporalEdgesByVia(fix, "temporal.handler")
	require.Len(t, edges, 1)
	assert.Equal(t, "update", edges[0].Meta["temporal_kind"])
	assert.Equal(t, "retry", edges[0].Meta["temporal_name"])
}

func TestGoTemporal_SetQueryHandlerWithOptions(t *testing.T) {
	fix := runGoExtract(t, `package wf

import "go.temporal.io/sdk/workflow"

func OrderWorkflow(ctx workflow.Context) error {
	workflow.SetQueryHandlerWithOptions(ctx, "status", func() (string, error) { return "ok", nil }, workflow.QueryHandlerOptions{})
	return nil
}
`)
	edges := temporalEdgesByVia(fix, "temporal.handler")
	require.Len(t, edges, 1)
	assert.Equal(t, "query", edges[0].Meta["temporal_kind"])
	assert.Equal(t, "status", edges[0].Meta["temporal_name"])
}

func TestGoTemporal_GetSignalChannelWithOptions(t *testing.T) {
	fix := runGoExtract(t, `package wf

import "go.temporal.io/sdk/workflow"

func OrderWorkflow(ctx workflow.Context) error {
	ch := workflow.GetSignalChannelWithOptions(ctx, "cancel", workflow.SignalChannelOptions{})
	_ = ch
	return nil
}
`)
	edges := temporalEdgesByVia(fix, "temporal.handler")
	require.Len(t, edges, 1)
	assert.Equal(t, "signal", edges[0].Meta["temporal_kind"])
	assert.Equal(t, "cancel", edges[0].Meta["temporal_name"])
}

func TestGoTemporal_HandlerNonLiteralNameUndetected(t *testing.T) {
	// Query / signal / update names are matched by string at runtime;
	// a non-literal name (variable / selector) can't be pinned here, so
	// no handler edge is emitted — high-precision, no guessing.
	fix := runGoExtract(t, `package wf

import "go.temporal.io/sdk/workflow"

func OrderWorkflow(ctx workflow.Context, q string) error {
	workflow.SetQueryHandler(ctx, q, func() (string, error) { return "ok", nil })
	return nil
}
`)
	assert.Empty(t, temporalEdgesByVia(fix, "temporal.handler"),
		"non-literal handler name must not be detected")
}

func TestGoTemporal_HandlerAliasedImportDetected(t *testing.T) {
	// Consistent with the dispatch detector: an aliased workflow import is
	// canonicalised and the handler declaration is detected.
	fix := runGoExtract(t, `package wf

import wf "go.temporal.io/sdk/workflow"

func OrderWorkflow(ctx wf.Context) error {
	wf.SetQueryHandler(ctx, "status", func() (string, error) { return "ok", nil })
	return nil
}
`)
	edges := temporalEdgesByVia(fix, "temporal.handler")
	require.Len(t, edges, 1)
	assert.Equal(t, "query", edges[0].Meta["temporal_kind"])
	assert.Equal(t, "status", edges[0].Meta["temporal_name"])
}

// --- Outbound signal sends / query calls ----------------------------
//
// A workflow (or a service holding a Temporal client) can signal or
// query an ALREADY-RUNNING workflow by name. These are the consumer
// side of the signal/query namespaces — distinct from the in-workflow
// handler declarations. We surface them as EdgeCalls edges tagged
// `via=temporal.signal-send` / `temporal.query-call` carrying the
// signal/query name (the 4th positional argument, a string literal).
//
// APIs (the name is always the 4th positional arg, after ctx +
// workflowID + runID):
//
//	workflow.SignalExternalWorkflow(ctx, wid, rid, "name", arg)   // workflow -> workflow
//	client.SignalWorkflow(ctx, wid, rid, "name", arg)            // service  -> workflow
//	client.QueryWorkflow(ctx, wid, rid, "name", args...)         // service  -> workflow
//
// (Note: there is no workflow.QueryWorkflow — querying is a client-side
// operation; and SignalExternalWorkflow already returns a Future, so
// there is no ...Async variant.)

func TestGoTemporal_SignalExternalWorkflow(t *testing.T) {
	fix := runGoExtract(t, `package wf

import "go.temporal.io/sdk/workflow"

func Orchestrator(ctx workflow.Context) error {
	return workflow.SignalExternalWorkflow(ctx, "order-123", "", "cancel-request", nil).Get(ctx, nil)
}
`)
	edges := temporalEdgesByVia(fix, "temporal.signal-send")
	require.Len(t, edges, 1)
	assert.Equal(t, "signal", edges[0].Meta["temporal_kind"])
	assert.Equal(t, "cancel-request", edges[0].Meta["temporal_name"])
}

func TestGoTemporal_ClientSignalWorkflow(t *testing.T) {
	// Receiver is an arbitrary client variable, so detection is by
	// method name (like the Register* helpers), gated on a string-literal
	// name in the 4th position.
	fix := runGoExtract(t, `package svc

func Cancel(c Client) error {
	return c.SignalWorkflow(ctx, "order-123", "", "cancel-request", nil)
}
`)
	edges := temporalEdgesByVia(fix, "temporal.signal-send")
	require.Len(t, edges, 1)
	assert.Equal(t, "signal", edges[0].Meta["temporal_kind"])
	assert.Equal(t, "cancel-request", edges[0].Meta["temporal_name"])
}

func TestGoTemporal_ClientQueryWorkflow(t *testing.T) {
	fix := runGoExtract(t, `package svc

func Status(c Client) {
	c.QueryWorkflow(ctx, "order-123", "", "get-status")
}
`)
	edges := temporalEdgesByVia(fix, "temporal.query-call")
	require.Len(t, edges, 1)
	assert.Equal(t, "query", edges[0].Meta["temporal_kind"])
	assert.Equal(t, "get-status", edges[0].Meta["temporal_name"])
}

func TestGoTemporal_OutboundNonLiteralNameUndetected(t *testing.T) {
	// Signal/query names are matched by string at runtime; a non-literal
	// name can't be pinned, so no outbound edge is emitted.
	fix := runGoExtract(t, `package svc

func Cancel(c Client, name string) error {
	return c.SignalWorkflow(ctx, "order-123", "", name, nil)
}
`)
	assert.Empty(t, temporalEdgesByVia(fix, "temporal.signal-send"))
}

func TestGoTemporal_SignalExternalAliasedDetected(t *testing.T) {
	// SignalExternalWorkflow is a workflow-package function; an aliased
	// import is canonicalised to the "workflow" receiver and detected.
	fix := runGoExtract(t, `package wf

import wf "go.temporal.io/sdk/workflow"

func Orchestrator(ctx wf.Context) error {
	return wf.SignalExternalWorkflow(ctx, "order-123", "", "cancel-request", nil).Get(ctx, nil)
}
`)
	edges := temporalEdgesByVia(fix, "temporal.signal-send")
	require.Len(t, edges, 1)
	assert.Equal(t, "signal", edges[0].Meta["temporal_kind"])
	assert.Equal(t, "cancel-request", edges[0].Meta["temporal_name"])
}

// --- Service-side workflow START (ExecuteWorkflow / SignalWithStartWorkflow) ---

func TestGoTemporal_ExecuteWorkflowStart(t *testing.T) {
	// client.ExecuteWorkflow(ctx, opts, WorkflowFn, args...) — workflow is
	// the 3rd positional arg, reduced from the func reference.
	fix := runGoExtract(t, `package svc

func Start(c Client) {
	c.ExecuteWorkflow(ctx, opts, OrderWorkflow, 1)
}
`)
	edges := temporalEdgesByVia(fix, "temporal.start")
	require.Len(t, edges, 1)
	assert.Equal(t, "workflow", edges[0].Meta["temporal_kind"])
	assert.Equal(t, "OrderWorkflow", edges[0].Meta["temporal_name"])
}

func TestGoTemporal_SignalWithStartWorkflow(t *testing.T) {
	// client.SignalWithStartWorkflow(ctx, wfID, sig, arg, opts, workflow, ...)
	// — the workflow is the 6th positional arg.
	fix := runGoExtract(t, `package svc

func Start(c Client) {
	c.SignalWithStartWorkflow(ctx, "order-1", "cancel", nil, opts, OrderWorkflow, 1)
}
`)
	edges := temporalEdgesByVia(fix, "temporal.start")
	require.Len(t, edges, 1)
	assert.Equal(t, "workflow", edges[0].Meta["temporal_kind"])
	assert.Equal(t, "OrderWorkflow", edges[0].Meta["temporal_name"])
}

// --- Dispatch wrapper detection (issue #80 Q2) ----------------------
//
// A function that forwards one of its parameters as the dispatch name is
// a wrapper; the parameter is not a real activity name, so emitting a stub
// for it is noise. We suppress the stub and mark the function as a wrapper
// (temporal_wrapper_*) for a future interprocedural follower. Propagating
// the caller's argument through the wrapper (cross-file) is not yet done.

func TestGoTemporal_WrapperParamDispatchSuppressed(t *testing.T) {
	fix := runGoExtract(t, `package wf

import "go.temporal.io/sdk/workflow"

func executeActivity(ctx workflow.Context, name string, args ...any) error {
	return workflow.ExecuteActivity(ctx, name, args...).Get(ctx, nil)
}
`)
	// The parameter-named dispatch must NOT emit a (never-resolvable) stub.
	assert.Empty(t, temporalEdgesByVia(fix, "temporal.stub"),
		"a parameter-forwarded dispatch must not emit a junk stub")

	// The wrapper function is marked for a future interprocedural follower.
	var wrapper *graph.Node
	for _, n := range fix.nodesByKind[graph.KindFunction] {
		if n.Name == "executeActivity" {
			wrapper = n
		}
	}
	require.NotNil(t, wrapper)
	assert.Equal(t, "activity", wrapper.Meta["temporal_wrapper_kind"])
	assert.Equal(t, "name", wrapper.Meta["temporal_wrapper_param"])
}
