package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestTemporalE2E_GoWorkflowToActivity exercises the full pipeline —
// parser detection → graph emission → resolver rewriting — on a tiny
// Go fixture that registers an activity + a workflow and dispatches
// the activity from the workflow body. After indexing, the
// EdgeCalls placeholder must point at the real activity function
// node and both the activity and the workflow must carry
// `temporal_role` Meta tags.
func TestTemporalE2E_GoWorkflowToActivity(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "workflow.go"), `package wf

import "go.temporal.io/sdk/workflow"

func OrderWorkflow(ctx workflow.Context, id string) error {
	return workflow.ExecuteActivity(ctx, ChargeCard, id).Get(ctx, nil)
}
`)
	writeFile(t, filepath.Join(dir, "activity.go"), `package wf

import "context"

func ChargeCard(ctx context.Context, id string) error {
	return nil
}
`)
	writeFile(t, filepath.Join(dir, "main.go"), `package wf

func setupWorker(w Worker) {
	w.RegisterWorkflow(OrderWorkflow)
	w.RegisterActivity(ChargeCard)
}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	// The activity function node was discovered via the
	// `worker.RegisterActivity` edge and stamped temporal_role.
	activityNodes := g.FindNodesByName("ChargeCard")
	require.Len(t, activityNodes, 1)
	activity := activityNodes[0]
	assert.Equal(t, "activity", activity.Meta["temporal_role"],
		"registered activity must carry temporal_role meta")
	assert.Equal(t, "ChargeCard", activity.Meta["temporal_name"])

	// The workflow was stamped too.
	workflowNodes := g.FindNodesByName("OrderWorkflow")
	require.Len(t, workflowNodes, 1)
	wf := workflowNodes[0]
	assert.Equal(t, "workflow", wf.Meta["temporal_role"])

	// The workflow.ExecuteActivity call edge was rewritten from the
	// placeholder to the real activity function.
	var stubCall *graph.Edge
	for _, e := range g.GetOutEdges(wf.ID) {
		if e == nil || e.Meta == nil {
			continue
		}
		if e.Meta["via"] == "temporal.stub" {
			stubCall = e
			break
		}
	}
	require.NotNil(t, stubCall, "workflow must have an outbound temporal.stub edge")
	assert.Equal(t, activity.ID, stubCall.To,
		"stub-call edge must land on the registered activity, not the placeholder")
	assert.Equal(t, graph.OriginASTResolved, stubCall.Origin)
}

// TestTemporalE2E_GoChildWorkflow exercises the same pipeline on a
// child-workflow dispatch — a different temporal_kind, same resolver
// path.
func TestTemporalE2E_GoChildWorkflow(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "parent.go"), `package wf

import "go.temporal.io/sdk/workflow"

func ParentWorkflow(ctx workflow.Context) error {
	return workflow.ExecuteChildWorkflow(ctx, ChildWorkflow, 42).Get(ctx, nil)
}
`)
	writeFile(t, filepath.Join(dir, "child.go"), `package wf

import "go.temporal.io/sdk/workflow"

func ChildWorkflow(ctx workflow.Context, n int) error {
	return nil
}
`)
	writeFile(t, filepath.Join(dir, "main.go"), `package wf

func setup(w Worker) {
	w.RegisterWorkflow(ParentWorkflow)
	w.RegisterWorkflow(ChildWorkflow)
}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	parent := g.FindNodesByName("ParentWorkflow")[0]
	child := g.FindNodesByName("ChildWorkflow")[0]
	assert.Equal(t, "workflow", parent.Meta["temporal_role"])
	assert.Equal(t, "workflow", child.Meta["temporal_role"])

	var stubCall *graph.Edge
	for _, e := range g.GetOutEdges(parent.ID) {
		if e != nil && e.Meta != nil && e.Meta["via"] == "temporal.stub" {
			stubCall = e
			break
		}
	}
	require.NotNil(t, stubCall, "parent workflow must have an outbound temporal.stub edge")
	assert.Equal(t, child.ID, stubCall.To)
	assert.Equal(t, "workflow", stubCall.Meta["temporal_kind"])
}

// TestTemporalE2E_GoEnvDefaultActivity exercises the env-var-with-literal
// -default dispatch name: the workflow names its activity through a
// variable read from os.Getenv with a literal fallback. The pipeline must
// land the call on the default activity but at the speculative tier.
func TestTemporalE2E_GoEnvDefaultActivity(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "workflow.go"), `package wf

import (
	"cmp"
	"os"

	"go.temporal.io/sdk/workflow"
)

func OrderWorkflow(ctx workflow.Context, id string) error {
	actName := cmp.Or(os.Getenv("CHARGE_ACTIVITY"), "ChargeCard")
	return workflow.ExecuteActivity(ctx, actName, id).Get(ctx, nil)
}
`)
	writeFile(t, filepath.Join(dir, "activity.go"), `package wf

import "context"

func ChargeCard(ctx context.Context, id string) error {
	return nil
}
`)
	writeFile(t, filepath.Join(dir, "main.go"), `package wf

func setupWorker(w Worker) {
	w.RegisterWorkflow(OrderWorkflow)
	w.RegisterActivity(ChargeCard)
}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	wf := g.FindNodesByName("OrderWorkflow")[0]
	activity := g.FindNodesByName("ChargeCard")[0]

	var stubCall *graph.Edge
	for _, e := range g.GetOutEdges(wf.ID) {
		if e != nil && e.Meta != nil && e.Meta["via"] == "temporal.stub" {
			stubCall = e
			break
		}
	}
	require.NotNil(t, stubCall, "workflow must have an outbound temporal.stub edge")
	assert.Equal(t, activity.ID, stubCall.To,
		"env-default dispatch must land on the default activity")
	assert.Equal(t, "env_default", stubCall.Meta["temporal_name_origin"])
	assert.Equal(t, graph.OriginSpeculative, stubCall.Origin,
		"env-default resolution must be speculative")
	assert.Equal(t, true, stubCall.Meta[graph.MetaSpeculative],
		"env-default edge must be hidden-by-default")
}

// TestTemporalE2E_GoQueryHandler exercises in-workflow handler detection:
// a workflow.SetQueryHandler call must surface as a via=temporal.handler
// edge from the enclosing workflow carrying its kind + name.
func TestTemporalE2E_GoQueryHandler(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "workflow.go"), `package wf

import "go.temporal.io/sdk/workflow"

func StatusWorkflow(ctx workflow.Context) error {
	workflow.SetQueryHandler(ctx, "status", func() (string, error) { return "ok", nil })
	return nil
}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	wf := g.FindNodesByName("StatusWorkflow")[0]
	var handler *graph.Edge
	for _, e := range g.GetOutEdges(wf.ID) {
		if e != nil && e.Meta != nil && e.Meta["via"] == "temporal.handler" {
			handler = e
			break
		}
	}
	require.NotNil(t, handler, "workflow must have an outbound temporal.handler edge")
	assert.Equal(t, "query", handler.Meta["temporal_kind"])
	assert.Equal(t, "status", handler.Meta["temporal_name"])
}

// TestTemporalE2E_GoOutboundSignalQuery exercises the consumer side of the
// signal/query namespaces through the real indexer: a workflow that signals
// an external workflow and a service that queries a running workflow must
// surface via=temporal.signal-send / via=temporal.query-call edges carrying
// the signal/query name (the 4th positional string literal).
func TestTemporalE2E_GoOutboundSignalQuery(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "orchestrator.go"), `package wf

import "go.temporal.io/sdk/workflow"

func Orchestrator(ctx workflow.Context) error {
	return workflow.SignalExternalWorkflow(ctx, "order-123", "", "cancel-request", nil).Get(ctx, nil)
}
`)
	writeFile(t, filepath.Join(dir, "service.go"), `package wf

type Client interface {
	QueryWorkflow(ctx any, wid, rid, queryType string, args ...any) (any, error)
}

func CheckStatus(ctx any, c Client) {
	c.QueryWorkflow(ctx, "order-123", "", "get-status")
}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	findOut := func(fnName, via string) *graph.Edge {
		fn := g.FindNodesByName(fnName)
		require.NotEmpty(t, fn, "function %s must be indexed", fnName)
		for _, e := range g.GetOutEdges(fn[0].ID) {
			if e != nil && e.Meta != nil && e.Meta["via"] == via {
				return e
			}
		}
		return nil
	}

	sig := findOut("Orchestrator", "temporal.signal-send")
	require.NotNil(t, sig, "Orchestrator must have an outbound temporal.signal-send edge")
	assert.Equal(t, "signal", sig.Meta["temporal_kind"])
	assert.Equal(t, "cancel-request", sig.Meta["temporal_name"])

	qry := findOut("CheckStatus", "temporal.query-call")
	require.NotNil(t, qry, "CheckStatus must have an outbound temporal.query-call edge")
	assert.Equal(t, "query", qry.Meta["temporal_kind"])
	assert.Equal(t, "get-status", qry.Meta["temporal_name"])
}

// TestTemporalE2E_GoRegisterActivitiesPlural exercises struct registration:
// w.RegisterActivities(&Activities{}) must promote every exported method of
// the struct to a temporal activity, so a workflow that dispatches one of
// those methods by name resolves to the method node.
func TestTemporalE2E_GoRegisterActivitiesPlural(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "activities.go"), `package wf

import "context"

type Activities struct{}

func (a *Activities) ChargeCard(ctx context.Context, id string) error { return nil }
func (a *Activities) Refund(ctx context.Context, id string) error     { return nil }
func (a *Activities) internalHelper() {}
`)
	writeFile(t, filepath.Join(dir, "workflow.go"), `package wf

import "go.temporal.io/sdk/workflow"

func OrderWorkflow(ctx workflow.Context, id string) error {
	return workflow.ExecuteActivity(ctx, "ChargeCard", id).Get(ctx, nil)
}
`)
	writeFile(t, filepath.Join(dir, "main.go"), `package wf

func setup(w Worker) {
	w.RegisterActivities(&Activities{})
}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	wf := g.FindNodesByName("OrderWorkflow")[0]
	var stubCall *graph.Edge
	for _, e := range g.GetOutEdges(wf.ID) {
		if e != nil && e.Meta != nil && e.Meta["via"] == "temporal.stub" {
			stubCall = e
			break
		}
	}
	require.NotNil(t, stubCall, "workflow must have an outbound temporal.stub edge")

	// The stub must land on the promoted ChargeCard method, which must
	// carry the activity role.
	charge := g.FindNodesByName("ChargeCard")
	require.NotEmpty(t, charge, "ChargeCard method must be indexed")
	assert.Equal(t, charge[0].ID, stubCall.To,
		"dispatch must resolve to the struct's promoted method")
	assert.Equal(t, "activity", charge[0].Meta["temporal_role"])
}

// TestTemporalE2E_GoServiceStartsWorkflow exercises the workflow-start
// family: a service that calls client.ExecuteWorkflow(ctx, opts, WorkflowFn)
// must get a via=temporal.start edge resolved to the registered workflow —
// the "who starts this workflow" relationship.
func TestTemporalE2E_GoServiceStartsWorkflow(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "workflow.go"), `package wf

import "go.temporal.io/sdk/workflow"

func OrderWorkflow(ctx workflow.Context, id string) error { return nil }
`)
	writeFile(t, filepath.Join(dir, "service.go"), `package wf

import "go.temporal.io/sdk/client"

func StartOrder(ctx any, c client.Client, id string) error {
	_, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{}, OrderWorkflow, id)
	return err
}
`)
	writeFile(t, filepath.Join(dir, "main.go"), `package wf

func setup(w Worker) {
	w.RegisterWorkflow(OrderWorkflow)
}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	starter := g.FindNodesByName("StartOrder")
	require.NotEmpty(t, starter)
	wf := g.FindNodesByName("OrderWorkflow")
	require.NotEmpty(t, wf)

	var start *graph.Edge
	for _, e := range g.GetOutEdges(starter[0].ID) {
		if e != nil && e.Meta != nil && e.Meta["via"] == "temporal.start" {
			start = e
			break
		}
	}
	require.NotNil(t, start, "StartOrder must have an outbound temporal.start edge")
	assert.Equal(t, "workflow", start.Meta["temporal_kind"])
	assert.Equal(t, "OrderWorkflow", start.Meta["temporal_name"])
	assert.Equal(t, wf[0].ID, start.To,
		"the start edge must resolve to the registered workflow")
}

// TestTemporalE2E_GoConstNamedActivity exercises cross-file const-value
// retention + dereference: the activity name is a string const declared in
// a separate file (the dominant real-world shape), and the dispatch names
// it through the const identifier. The pipeline must persist the const
// value and dereference it so the stub resolves to the activity.
func TestTemporalE2E_GoConstNamedActivity(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "constants.go"), `package wf

const ChargeCardActivity = "ChargeCard"
`)
	writeFile(t, filepath.Join(dir, "workflow.go"), `package wf

import "go.temporal.io/sdk/workflow"

func OrderWorkflow(ctx workflow.Context, id string) error {
	return workflow.ExecuteActivity(ctx, ChargeCardActivity, id).Get(ctx, nil)
}
`)
	writeFile(t, filepath.Join(dir, "activity.go"), `package wf

import "context"

func ChargeCard(ctx context.Context, id string) error { return nil }
`)
	writeFile(t, filepath.Join(dir, "main.go"), `package wf

func setup(w Worker) {
	w.RegisterWorkflow(OrderWorkflow)
	w.RegisterActivityWithOptions(ChargeCard, RegisterOptions{Name: "ChargeCard"})
}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	wf := g.FindNodesByName("OrderWorkflow")[0]
	activity := g.FindNodesByName("ChargeCard")
	require.NotEmpty(t, activity)

	var stubCall *graph.Edge
	for _, e := range g.GetOutEdges(wf.ID) {
		if e != nil && e.Meta != nil && e.Meta["via"] == "temporal.stub" {
			stubCall = e
			break
		}
	}
	require.NotNil(t, stubCall, "workflow must have an outbound temporal.stub edge")
	assert.Equal(t, "ChargeCardActivity", stubCall.Meta["temporal_name"],
		"the stub keeps the const identifier as its name")
	assert.Equal(t, activity[0].ID, stubCall.To,
		"the const-named dispatch must dereference to the activity")
	assert.Equal(t, "ChargeCard", stubCall.Meta["temporal_const_deref"],
		"the dereferenced literal value is recorded on the edge")
}

// TestTemporalE2E_CrossLangJavaStartsGoWorkflow exercises the full
// cross-language join: a Java service that creates a workflow stub for a
// workflow implemented (and registered) in Go must get a via=temporal.start
// edge that resolves to the Go workflow node, at the speculative tier.
func TestTemporalE2E_CrossLangJavaStartsGoWorkflow(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "workflow.go"), `package wf

import "go.temporal.io/sdk/workflow"

func OrderWorkflow(ctx workflow.Context, id string) error { return nil }
`)
	writeFile(t, filepath.Join(dir, "main.go"), `package wf

func setup(w Worker) {
	w.RegisterWorkflow(OrderWorkflow)
}
`)
	writeFile(t, filepath.Join(dir, "OrderService.java"), `public class OrderService {
    public void start(WorkflowClient client) {
        OrderWorkflow wf = client.newWorkflowStub(OrderWorkflow.class, options);
        wf.processOrder("id");
    }
}
`)

	g := graph.New()
	idx := newTestIndexerGoJava(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	javaStart := g.FindNodesByName("start")
	require.NotEmpty(t, javaStart, "Java start method must be indexed")
	goWf := g.FindNodesByName("OrderWorkflow")
	require.NotEmpty(t, goWf)
	var goWfID string
	for _, n := range goWf {
		if n.Language == "go" {
			goWfID = n.ID
		}
	}
	require.NotEmpty(t, goWfID)

	var start *graph.Edge
	for _, e := range g.GetOutEdges(javaStart[0].ID) {
		if e != nil && e.Meta != nil && e.Meta["via"] == "temporal.start" {
			start = e
			break
		}
	}
	require.NotNil(t, start, "Java service must have an outbound temporal.start edge")
	assert.Equal(t, goWfID, start.To,
		"the Java start must cross-resolve to the Go workflow")
	assert.Equal(t, true, start.Meta["temporal_cross_lang"])
	assert.Equal(t, graph.OriginSpeculative, start.Origin)
}

// TestTemporalE2E_WrapperFollowing exercises the full wrapper-following pipeline:
// a thin dispatch wrapper forwards its `name` parameter to ExecuteActivity,
// and a workflow calls the wrapper with a literal activity name. The pipeline
// must produce a resolved temporal.stub edge from the workflow caller to the
// registered ChargeCard activity.
func TestTemporalE2E_WrapperFollowing(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "wrapper.go"), `package wf

import "go.temporal.io/sdk/workflow"

func execAct(ctx workflow.Context, name string, in any) workflow.Future {
    return workflow.ExecuteActivity(ctx, name, in)
}
`)
	writeFile(t, filepath.Join(dir, "workflow.go"), `package wf

import "go.temporal.io/sdk/workflow"

func OrderWorkflow(ctx workflow.Context, id string) error {
    return execAct(ctx, "ChargeCard", id).Get(ctx, nil)
}
`)
	writeFile(t, filepath.Join(dir, "activity.go"), `package wf

import "context"

func ChargeCard(ctx context.Context, id string) error {
    return nil
}
`)
	writeFile(t, filepath.Join(dir, "main.go"), `package wf

func setupWorker(w Worker) {
    w.RegisterWorkflow(OrderWorkflow)
    w.RegisterActivity(ChargeCard)
}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	wf := g.FindNodesByName("OrderWorkflow")
	require.NotEmpty(t, wf)
	activity := g.FindNodesByName("ChargeCard")
	require.NotEmpty(t, activity)

	// Find the temporal.stub edge from OrderWorkflow that names ChargeCard
	var stubCall *graph.Edge
	for _, e := range g.GetOutEdges(wf[0].ID) {
		if e == nil || e.Meta == nil {
			continue
		}
		if e.Meta["via"] == "temporal.stub" && e.Meta["temporal_name"] == "ChargeCard" {
			stubCall = e
			break
		}
	}
	require.NotNil(t, stubCall,
		"workflow caller must have a wrapper-synthesized temporal.stub edge for ChargeCard")
	assert.Equal(t, activity[0].ID, stubCall.To,
		"the wrapper-following stub must resolve to the registered ChargeCard activity")
}

// TestTemporalE2E_GoExecutorFieldDispatch exercises the full pipeline for
// executor struct-field dispatch: a struct method that dispatches via a
// field, constructed with a string literal, must resolve through the
// real indexer to the registered activity.
func TestTemporalE2E_GoExecutorFieldDispatch(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "executor.go"), `package wf

import "go.temporal.io/sdk/workflow"

type ActivityExecutor struct{ ActivityName string }

func (e ActivityExecutor) Run(ctx workflow.Context) {
    workflow.ExecuteActivity(ctx, e.ActivityName)
}
`)
	writeFile(t, filepath.Join(dir, "activity.go"), `package wf

import "context"

func ChargeCard(ctx context.Context) error { return nil }
`)
	writeFile(t, filepath.Join(dir, "main.go"), `package wf

func setup(w Worker) {
    w.RegisterActivity(ChargeCard)
    _ = ActivityExecutor{ActivityName: "ChargeCard"}
}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	// Find the Run method node.
	runners := g.FindNodesByName("Run")
	require.NotEmpty(t, runners, "Run method must be indexed")
	var runNode *graph.Node
	for _, n := range runners {
		if n.Language == "go" {
			runNode = n
			break
		}
	}
	require.NotNil(t, runNode)

	activity := g.FindNodesByName("ChargeCard")
	require.NotEmpty(t, activity)

	// The stub from Run must resolve to ChargeCard.
	var stubCall *graph.Edge
	for _, e := range g.GetOutEdges(runNode.ID) {
		if e != nil && e.Meta != nil && e.Meta["via"] == "temporal.stub" {
			stubCall = e
			break
		}
	}
	require.NotNil(t, stubCall, "Run must have an outbound temporal.stub edge")
	assert.Equal(t, activity[0].ID, stubCall.To,
		"executor-field dispatch must resolve to ChargeCard")
}

// TestTemporalE2E_GoFuncReturningConstantDispatch exercises the full pipeline
// for G2: a func that returns a string literal used as Temporal dispatch arg.
//
//	func GetChargeActivityName() string { return "ChargeActivity" }
//	workflow.ExecuteActivity(ctx, constants.GetChargeActivityName())
//
// After indexing, the stub edge must point at the real ChargeActivity node.
func TestTemporalE2E_GoFuncReturningConstantDispatch(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "constants.go"), `package wf

func GetChargeActivityName() string { return "ChargeActivity" }
`)
	writeFile(t, filepath.Join(dir, "workflow.go"), `package wf

import "go.temporal.io/sdk/workflow"

func OrderWorkflow(ctx workflow.Context) error {
	return workflow.ExecuteActivity(ctx, GetChargeActivityName()).Get(ctx, nil)
}
`)
	writeFile(t, filepath.Join(dir, "activity.go"), `package wf

import "context"

func ChargeActivity(ctx context.Context) error {
	return nil
}
`)
	writeFile(t, filepath.Join(dir, "main.go"), `package wf

func setupWorker(w Worker) {
	w.RegisterWorkflow(OrderWorkflow)
	w.RegisterActivity(ChargeActivity)
}
`)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	// ChargeActivity must be stamped as a registered activity
	actNodes := g.FindNodesByName("ChargeActivity")
	require.Len(t, actNodes, 1)
	activity := actNodes[0]
	assert.Equal(t, "activity", activity.Meta["temporal_role"])
	assert.Equal(t, "ChargeActivity", activity.Meta["temporal_name"])

	// OrderWorkflow must have its stub edge resolved to ChargeActivity
	wfNodes := g.FindNodesByName("OrderWorkflow")
	require.Len(t, wfNodes, 1)
	wf := wfNodes[0]

	var stubCall *graph.Edge
	for _, e := range g.GetOutEdges(wf.ID) {
		if e != nil && e.Meta != nil && e.Meta["via"] == "temporal.stub" {
			stubCall = e
			break
		}
	}
	require.NotNil(t, stubCall, "OrderWorkflow must have an outbound temporal.stub edge")
	assert.Equal(t, activity.ID, stubCall.To,
		"func-returning-constant dispatch must resolve to ChargeActivity")
	assert.Equal(t, graph.OriginASTResolved, stubCall.Origin)
}
