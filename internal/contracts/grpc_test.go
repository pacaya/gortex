package contracts

import (
	"testing"
)

func TestGRPCExtractor_ProtoProvider(t *testing.T) {
	ext := &GRPCExtractor{}
	src := []byte(`
syntax = "proto3";

service UserService {
  rpc GetUser(GetUserRequest) returns (GetUserResponse) {}
  rpc ListUsers(ListUsersRequest) returns (ListUsersResponse) {}
}
`)
	contracts := ext.Extract("user.proto", src, nil, nil)
	if len(contracts) != 2 {
		t.Fatalf("expected 2 contracts, got %d", len(contracts))
	}
	assertContract(t, contracts[0], "grpc::UserService::GetUser", ContractGRPC, RoleProvider)
	assertContract(t, contracts[1], "grpc::UserService::ListUsers", ContractGRPC, RoleProvider)
}

func TestGRPCExtractor_GoConsumer(t *testing.T) {
	ext := &GRPCExtractor{}
	src := []byte(`
package main

func main() {
	client := pb.NewUserServiceClient(conn)
}
`)
	contracts := ext.Extract("main.go", src, nil, nil)
	if len(contracts) < 1 {
		t.Fatalf("expected at least 1 contract, got %d", len(contracts))
	}
	assertContract(t, contracts[0], "grpc::UserService", ContractGRPC, RoleConsumer)
}

func TestGRPCExtractor_PythonConsumer(t *testing.T) {
	ext := &GRPCExtractor{}
	src := []byte(`
stub = UserServiceStub(channel)
response = stub.GetUser(request)
`)
	contracts := ext.Extract("client.py", src, nil, nil)
	if len(contracts) < 1 {
		t.Fatalf("expected at least 1 contract, got %d", len(contracts))
	}
	assertContract(t, contracts[0], "grpc::UserService", ContractGRPC, RoleConsumer)
}

// TestGRPCExtractor_GoConsumer_MethodLevel covers the redesigned
// two-pass scan: per-method contracts with IDs matching the provider
// format "grpc::Service::Method", and SymbolID on the enclosing
// function so matcher pairing produces EdgeMatches bridges.
func TestGRPCExtractor_GoConsumer_MethodLevel(t *testing.T) {
	ext := &GRPCExtractor{}
	src := []byte(`package main

import (
	"context"
	"example.com/pb"
)

func makeRPCCall(ctx context.Context) {
	userClient := pb.NewUsersClient(conn)
	_, _ = userClient.GetUser(ctx, &pb.GetUserRequest{Id: "x"})
	_, _ = userClient.ListUsers(ctx, &pb.ListUsersRequest{})
}
`)
	nodes := makeNodes("main.go", []struct {
		name       string
		start, end int
	}{
		{"makeRPCCall", 8, 12},
	})

	contracts := ext.Extract("main.go", src, nodes, nil)

	want := map[string]string{
		"grpc::Users::GetUser":   "main.go::makeRPCCall",
		"grpc::Users::ListUsers": "main.go::makeRPCCall",
	}
	got := map[string]string{}
	for _, c := range contracts {
		if c.Role == RoleConsumer && c.Type == ContractGRPC {
			got[c.ID] = c.SymbolID
		}
	}
	for id, wantSym := range want {
		gotSym, ok := got[id]
		if !ok {
			t.Errorf("missing consumer contract %s; all consumers: %v", id, got)
			continue
		}
		if gotSym != wantSym {
			t.Errorf("consumer %s: SymbolID want %q, got %q", id, wantSym, gotSym)
		}
	}

	// Fallback service-level contract must be suppressed when
	// method-level contracts already cover the service — otherwise
	// the registry fills with duplicates.
	for _, c := range contracts {
		if c.ID == "grpc::Users" {
			t.Errorf("unwanted service-level fallback emitted alongside method-level contracts: %+v", c)
		}
	}
}

// TestGRPCExtractor_GoConsumer_UnrelatedCallsAreNotGRPC guards the
// false-positive case: "(\w+).(\w+)(" matches every method call in a
// Go file, but we must only emit a gRPC consumer contract when the
// receiver was previously established as a gRPC client.
func TestGRPCExtractor_GoConsumer_UnrelatedCallsAreNotGRPC(t *testing.T) {
	ext := &GRPCExtractor{}
	src := []byte(`package main

func main() {
	logger.Info("hi")
	unrelated.Handle("msg")
}
`)
	contracts := ext.Extract("main.go", src, nil, nil)
	for _, c := range contracts {
		if c.Type == ContractGRPC {
			t.Errorf("unexpected gRPC contract %+v — no NewClient assignment exists", c)
		}
	}
}

func assertContract(t *testing.T, c Contract, id string, ctype ContractType, role Role) {
	t.Helper()
	if c.ID != id {
		t.Errorf("expected ID %q, got %q", id, c.ID)
	}
	if c.Type != ctype {
		t.Errorf("expected Type %q, got %q", ctype, c.Type)
	}
	if c.Role != role {
		t.Errorf("expected Role %q, got %q", role, c.Role)
	}
}
