package contracts

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestHTTPEnrich_Ruby_RenderStatusAndPermit(t *testing.T) {
	src := []byte(`Rails.application.routes.draw do
  post '/users', to: 'users#create'
end

class UsersController < ApplicationController
  def create
    user = User.create!(params.require(:user).permit(:name, :email))
    render json: user, status: :created
  end
end
`)
	nodes := []*graph.Node{
		{ID: "pkg/controller.rb::UsersController", Name: "UsersController", Kind: graph.KindType, FilePath: "pkg/controller.rb", StartLine: 5, EndLine: 10},
		{ID: "pkg/controller.rb::UsersController.create", Name: "create", Kind: graph.KindMethod, FilePath: "pkg/controller.rb", StartLine: 6, EndLine: 9},
	}
	cs := (&HTTPExtractor{}).Extract("pkg/controller.rb", src, nodes, nil)
	c := findContract(t, cs, "http::POST::/users", RoleProvider)
	assertMetaInts(t, c, "status_codes", []int{201})
	assertMetaStrings(t, c, "query_params", []string{"email", "name"})
}

func TestHTTPEnrich_PHP_LaravelResponseJson(t *testing.T) {
	src := []byte(`<?php
Route::post('/users', [UsersController::class, 'create']);

class UsersController {
    public function create(Request $request) {
        $name = $request->input('name');
        return response()->json(['id' => 'x'], 201);
    }
}
`)
	nodes := []*graph.Node{
		{ID: "pkg/routes.php::create", Name: "create", Kind: graph.KindFunction, FilePath: "pkg/routes.php", StartLine: 5, EndLine: 8},
	}
	cs := (&HTTPExtractor{}).Extract("pkg/routes.php", src, nodes, nil)
	c := findContract(t, cs, "http::POST::/users", RoleProvider)
	assertMetaInts(t, c, "status_codes", []int{201})
	assertMetaStrings(t, c, "query_params", []string{"name"})
}

func TestHTTPEnrich_Elixir_PhoenixStatusAndJSON(t *testing.T) {
	src := []byte(`defmodule AppRouter do
  use Phoenix.Router
  post "/users", UsersController, :create
end

defmodule UsersController do
  def create(conn, params) do
    user = Users.create!(params)
    conn
    |> put_status(:created)
    |> json(user)
  end
end
`)
	nodes := []*graph.Node{
		{ID: "pkg/router.ex::AppRouter", Name: "AppRouter", Kind: graph.KindType, FilePath: "pkg/router.ex", StartLine: 1, EndLine: 4},
		{ID: "pkg/router.ex::UsersController.create", Name: "create", Kind: graph.KindFunction, FilePath: "pkg/router.ex", StartLine: 7, EndLine: 12},
	}
	cs := (&HTTPExtractor{}).Extract("pkg/router.ex", src, nodes, nil)
	c := findContract(t, cs, "http::POST::/users", RoleProvider)
	assertMetaInts(t, c, "status_codes", []int{201})
}

func TestHTTPEnrich_Dart_ShelfRouterProvider(t *testing.T) {
	src := []byte(`import 'package:shelf_router/shelf_router.dart';

class Tuck { final String id; Tuck(this.id); }

final router = Router()
  ..get('/health', health)
  ..post('/v1/tucks', createTuck);

Response createTuck(Request req) {
  final tuck = Tuck('x');
  return Response(201, body: jsonEncode(tuck.toJson()));
}
`)
	nodes := []*graph.Node{
		{ID: "pkg/server.dart::createTuck", Name: "createTuck", Kind: graph.KindFunction, FilePath: "pkg/server.dart", StartLine: 9, EndLine: 12},
		{ID: "pkg/server.dart::Tuck", Name: "Tuck", Kind: graph.KindType, FilePath: "pkg/server.dart", StartLine: 3, EndLine: 3},
	}
	cs := (&HTTPExtractor{}).Extract("pkg/server.dart", src, nodes, nil)
	c := findContract(t, cs, "http::POST::/v1/tucks", RoleProvider)
	assertMetaInts(t, c, "status_codes", []int{201})
}

func TestHTTPEnrich_TS_CurriedWrapper(t *testing.T) {
	src := []byte(`import type { User } from './types'

export async function fetchUser(id: string): Promise<User> {
  return createClient(config).get<User>('/users/' + id)
}
`)
	nodes := []*graph.Node{
		{ID: "pkg/client.ts::fetchUser", Name: "fetchUser", Kind: graph.KindFunction, FilePath: "pkg/client.ts", StartLine: 3, EndLine: 5},
		{ID: "pkg/client.ts::User", Name: "User", Kind: graph.KindType, FilePath: "pkg/client.ts", StartLine: 1, EndLine: 1},
	}
	// Use a synthetic contract to isolate the enricher — the curried
	// call isn't in httpPatterns, but the wrapper enricher runs on
	// any consumer contract.
	c := Contract{
		ID:       "http::GET::/users/{p1}",
		Type:     ContractHTTP,
		Role:     RoleConsumer,
		SymbolID: "pkg/client.ts::fetchUser",
		FilePath: "pkg/client.ts",
		Line:     4,
		Meta:     map[string]any{"method": "GET", "path": "/users/{p1}"},
	}
	lines := splitLinesForTest(src)
	EnrichHTTPContract(&c, lines, nodes, "typescript")
	if got := c.Meta["response_type"]; got != "pkg/client.ts::User" {
		t.Errorf("response_type = %v, want pkg/client.ts::User", got)
	}
}

func TestGRPCEnrich_TS_Consumer_MethodLevel(t *testing.T) {
	src := []byte(`import { UsersClient } from './users_grpc_pb'
import { GetUserRequest } from './users_pb'

const stub = new UsersClient('localhost:50051')

export function getUser(id: string) {
  return stub.getUser(new GetUserRequest({ id }))
}
`)
	nodes := []*graph.Node{
		{ID: "pkg/client.ts::getUser", Name: "getUser", Kind: graph.KindFunction, FilePath: "pkg/client.ts", StartLine: 6, EndLine: 8},
	}
	cs := (&GRPCExtractor{}).Extract("pkg/client.ts", src, nodes, nil)
	c := findContract(t, cs, "grpc::Users::getUser", RoleConsumer)
	assertMetaString(t, c, "request_type", "GetUserRequest")
}

func TestGRPCEnrich_Python_Consumer_MethodLevel(t *testing.T) {
	src := []byte(`import users_pb2
import users_pb2_grpc

def get_user(channel, uid):
    stub = users_pb2_grpc.UsersStub(channel)
    return stub.GetUser(users_pb2.GetUserRequest(id=uid))
`)
	nodes := []*graph.Node{
		{ID: "pkg/client.py::get_user", Name: "get_user", Kind: graph.KindFunction, FilePath: "pkg/client.py", StartLine: 4, EndLine: 6},
	}
	cs := (&GRPCExtractor{}).Extract("pkg/client.py", src, nodes, nil)
	c := findContract(t, cs, "grpc::Users::GetUser", RoleConsumer)
	assertMetaString(t, c, "request_type", "GetUserRequest")
}
