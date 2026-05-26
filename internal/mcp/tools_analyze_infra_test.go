package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

func callAnalyzeInfra(t *testing.T, srv *Server, kind string, args map[string]any) map[string]any {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	args["kind"] = kind
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = args
	res, err := srv.handleAnalyze(context.Background(), req)
	if err != nil {
		t.Fatalf("handleAnalyze: %v", err)
	}
	if res.IsError {
		t.Fatalf("error: %+v", res.Content)
	}
	textBlock := res.Content[0].(mcplib.TextContent)
	var out map[string]any
	if err := json.Unmarshal([]byte(textBlock.Text), &out); err != nil {
		t.Fatalf("json: %v\n%s", err, textBlock.Text)
	}
	return out
}

func seedK8sFixture(g graph.Store) {
	deploy := &graph.Node{
		ID: "k8s::Deployment::prod::api", Kind: graph.KindResource,
		Name: "api", FilePath: "k8s/api.yaml", StartLine: 1,
		Meta: map[string]any{
			"k8s_kind": "Deployment", "namespace": "prod",
		},
	}
	cm := &graph.Node{
		ID: "k8s::ConfigMap::prod::api-config", Kind: graph.KindResource,
		Name: "api-config", FilePath: "k8s/cm.yaml", StartLine: 1,
		Meta: map[string]any{
			"k8s_kind": "ConfigMap", "namespace": "prod",
		},
	}
	svc := &graph.Node{
		ID: "k8s::Service::prod::api", Kind: graph.KindResource,
		Name: "api", FilePath: "k8s/svc.yaml", StartLine: 1,
		Meta: map[string]any{
			"k8s_kind": "Service", "namespace": "prod",
		},
	}
	img := &graph.Node{
		ID: "image::ghcr.io/acme/api:1.2.3", Kind: graph.KindImage,
		Name: "ghcr.io/acme/api:1.2.3", FilePath: "Dockerfile", StartLine: 1,
		Meta: map[string]any{
			"role": "base", "ref": "ghcr.io/acme/api", "tag": "1.2.3",
		},
	}
	cfg := &graph.Node{
		ID: "cfg::env::DATABASE_URL", Kind: graph.KindConfigKey,
		Name: "DATABASE_URL", FilePath: "k8s/api.yaml", StartLine: 5,
		Meta: map[string]any{"source": "env"},
	}
	g.AddNode(deploy)
	g.AddNode(cm)
	g.AddNode(svc)
	g.AddNode(img)
	g.AddNode(cfg)

	g.AddEdge(&graph.Edge{From: deploy.ID, To: img.ID, Kind: graph.EdgeDependsOn})
	g.AddEdge(&graph.Edge{From: deploy.ID, To: cm.ID, Kind: graph.EdgeConfigures})
	g.AddEdge(&graph.Edge{From: deploy.ID, To: cm.ID, Kind: graph.EdgeMounts})
	g.AddEdge(&graph.Edge{From: deploy.ID, To: cfg.ID, Kind: graph.EdgeUsesEnv})
	g.AddEdge(&graph.Edge{From: svc.ID, To: "port::tcp::80", Kind: graph.EdgeExposes})
}

func TestAnalyzeK8sResources_AllAndFilters(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedK8sFixture(srv.graph)

	out := callAnalyzeInfra(t, srv, "k8s_resources", nil)
	rows, _ := out["resources"].([]any)
	if len(rows) != 3 {
		t.Fatalf("expected 3 resources, got %d", len(rows))
	}

	out = callAnalyzeInfra(t, srv, "k8s_resources", map[string]any{
		"k8s_kind": "Deployment",
	})
	if got, _ := out["total"].(float64); got != 1 {
		t.Errorf("expected 1 deployment, got %v", got)
	}
	row := out["resources"].([]any)[0].(map[string]any)
	if row["depends_on"].(float64) != 1 {
		t.Errorf("deployment should have 1 depends_on (image), got %v", row["depends_on"])
	}
	if row["uses_env"].(float64) != 1 {
		t.Errorf("deployment should have 1 uses_env, got %v", row["uses_env"])
	}
	if row["configures"].(float64) != 1 {
		t.Errorf("deployment should have 1 configures, got %v", row["configures"])
	}

	out = callAnalyzeInfra(t, srv, "k8s_resources", map[string]any{
		"namespace": "prod",
		"name":      "api-config",
	})
	if got, _ := out["total"].(float64); got != 1 {
		t.Errorf("expected 1 row after name filter, got %v", got)
	}
}

func TestAnalyzeImages_ConsumerCount(t *testing.T) {
	srv, _ := setupTestServer(t)
	seedK8sFixture(srv.graph)

	out := callAnalyzeInfra(t, srv, "images", nil)
	rows := out["images"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 image, got %d", len(rows))
	}
	row := rows[0].(map[string]any)
	if row["consumers"].(float64) != 1 {
		t.Errorf("expected 1 consumer (the deployment), got %v", row["consumers"])
	}

	out = callAnalyzeInfra(t, srv, "images", map[string]any{
		"ref": "ghcr.io/acme",
	})
	if got, _ := out["total"].(float64); got != 1 {
		t.Errorf("expected 1 image after ref filter, got %v", got)
	}
}

func TestAnalyzeKustomize_OverlayRollup(t *testing.T) {
	srv, _ := setupTestServer(t)
	overlay := &graph.Node{
		ID: "kustomize::k8s/overlays/staging", Kind: graph.KindKustomization,
		Name: "k8s/overlays/staging",
		FilePath: "k8s/overlays/staging/kustomization.yaml", StartLine: 1,
		Meta: map[string]any{"dir": "k8s/overlays/staging"},
	}
	srv.graph.AddNode(overlay)
	srv.graph.AddEdge(&graph.Edge{From: overlay.ID, To: "kustomize::k8s/overlays/base", Kind: graph.EdgeDependsOn})
	srv.graph.AddEdge(&graph.Edge{From: overlay.ID, To: "k8s/overlays/staging/service.yaml", Kind: graph.EdgeReferences})
	srv.graph.AddEdge(&graph.Edge{From: overlay.ID, To: "k8s/overlays/staging/ingress.yaml", Kind: graph.EdgeReferences})

	out := callAnalyzeInfra(t, srv, "kustomize", nil)
	rows := out["kustomizations"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 kustomization, got %d", len(rows))
	}
	row := rows[0].(map[string]any)
	if row["depends_on"].(float64) != 1 {
		t.Errorf("expected 1 base, got %v", row["depends_on"])
	}
	if row["resources"].(float64) != 2 {
		t.Errorf("expected 2 resource files, got %v", row["resources"])
	}
}
