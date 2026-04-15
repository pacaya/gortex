package contracts

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// newBindTestGraph builds a minimal graph with one repo and the given
// method nodes. Receiver types and names drive the tier selection in
// bindGRPCProvider.
func newBindTestGraph(repoPrefix string, methods []struct {
	id, name, recv string
}) *graph.Graph {
	g := graph.New()
	for _, m := range methods {
		n := &graph.Node{
			ID:         m.id,
			Kind:       graph.KindMethod,
			Name:       m.name,
			FilePath:   "server.go",
			RepoPrefix: repoPrefix,
			Meta:       map[string]any{},
		}
		if m.recv != "" {
			n.Meta["receiver"] = m.recv
		}
		g.AddNode(n)
	}
	return g
}

func TestBindGRPCProvider_SingleCandidateExactReceiver(t *testing.T) {
	g := newBindTestGraph("auth", []struct{ id, name, recv string }{
		{"auth/server.go::UsersServer.GetUser", "GetUser", "UsersServer"},
	})
	reg := NewRegistry()
	reg.Add(Contract{
		ID:         "grpc::Users::GetUser",
		Type:       ContractGRPC,
		Role:       RoleProvider,
		RepoPrefix: "auth",
		Meta:       map[string]any{"service": "Users", "method": "GetUser"},
	})

	bound := BindProviderSymbols(reg, g)
	if bound != 1 {
		t.Fatalf("expected 1 binding, got %d", bound)
	}
	cs := reg.ByID("grpc::Users::GetUser")
	if len(cs) == 0 {
		t.Fatal("contract lost from registry after binding")
	}
	// Registry keeps appends; the bound copy is the most recent.
	got := cs[len(cs)-1].SymbolID
	if got != "auth/server.go::UsersServer.GetUser" {
		t.Errorf("SymbolID: want auth/server.go::UsersServer.GetUser, got %q", got)
	}
}

func TestBindGRPCProvider_AmbiguousReceiversNoBind(t *testing.T) {
	// Two methods named GetUser in the same repo, neither on the
	// canonical "{Service}Server" receiver. Binding must not guess —
	// the provider stays unbound and no bridge forms (acceptable v1
	// behavior documented in bind.go).
	g := newBindTestGraph("auth", []struct{ id, name, recv string }{
		{"auth/a.go::HandlerA.GetUser", "GetUser", "HandlerA"},
		{"auth/b.go::HandlerB.GetUser", "GetUser", "HandlerB"},
	})
	reg := NewRegistry()
	reg.Add(Contract{
		ID:         "grpc::Users::GetUser",
		Type:       ContractGRPC,
		Role:       RoleProvider,
		RepoPrefix: "auth",
		Meta:       map[string]any{"service": "Users", "method": "GetUser"},
	})

	bound := BindProviderSymbols(reg, g)
	if bound != 0 {
		t.Errorf("expected 0 bindings when ambiguous, got %d", bound)
	}
}

func TestBindGRPCProvider_NoCandidatesSkip(t *testing.T) {
	g := graph.New()
	reg := NewRegistry()
	reg.Add(Contract{
		ID:         "grpc::Users::GetUser",
		Type:       ContractGRPC,
		Role:       RoleProvider,
		RepoPrefix: "auth",
		Meta:       map[string]any{"service": "Users", "method": "GetUser"},
	})
	if got := BindProviderSymbols(reg, g); got != 0 {
		t.Errorf("expected 0 bindings with no candidates, got %d", got)
	}
}

func TestBindGRPCProvider_SkipsAlreadyBound(t *testing.T) {
	g := newBindTestGraph("auth", []struct{ id, name, recv string }{
		{"auth/server.go::UsersServer.GetUser", "GetUser", "UsersServer"},
	})
	reg := NewRegistry()
	reg.Add(Contract{
		ID:         "grpc::Users::GetUser",
		Type:       ContractGRPC,
		Role:       RoleProvider,
		RepoPrefix: "auth",
		SymbolID:   "manually/bound::Elsewhere",
		Meta:       map[string]any{"service": "Users", "method": "GetUser"},
	})
	// Contracts that already carry SymbolID must not be touched —
	// the binding pass is a recovery step, not an override.
	if got := BindProviderSymbols(reg, g); got != 0 {
		t.Errorf("expected 0 bindings, got %d", got)
	}
	cs := reg.ByID("grpc::Users::GetUser")
	if cs[0].SymbolID != "manually/bound::Elsewhere" {
		t.Errorf("existing SymbolID was overwritten: %q", cs[0].SymbolID)
	}
}

func TestBindGRPCProvider_CrossRepoCandidatesIgnored(t *testing.T) {
	// A same-named method in a DIFFERENT repo must not be picked.
	g := newBindTestGraph("other-repo", []struct{ id, name, recv string }{
		{"other-repo/server.go::UsersServer.GetUser", "GetUser", "UsersServer"},
	})
	reg := NewRegistry()
	reg.Add(Contract{
		ID:         "grpc::Users::GetUser",
		Type:       ContractGRPC,
		Role:       RoleProvider,
		RepoPrefix: "auth",
		Meta:       map[string]any{"service": "Users", "method": "GetUser"},
	})
	if got := BindProviderSymbols(reg, g); got != 0 {
		t.Errorf("expected 0 bindings (cross-repo candidate), got %d", got)
	}
}
