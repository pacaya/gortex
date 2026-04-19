package contracts

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// -----------------------------------------------------------------------------
// NestJS
// -----------------------------------------------------------------------------

func TestHTTPEnrich_TS_NestJS_BodyQueryReturn(t *testing.T) {
	src := []byte(`import { Controller, Post, Body, Query, HttpCode, HttpStatus } from '@nestjs/common'
import { CreateUserDto } from './dto'
import { UserResp } from './resp'

@Controller('users')
export class UsersController {
  @Post('/')
  @HttpCode(201)
  create(@Body() dto: CreateUserDto, @Query('tenant') tenant: string): Promise<UserResp> {
    return Promise.resolve({} as UserResp)
  }
}
`)
	nodes := []*graph.Node{
		{ID: "pkg/users.ts::UsersController", Name: "UsersController", Kind: graph.KindType, FilePath: "pkg/users.ts", StartLine: 5, EndLine: 11},
		{ID: "pkg/users.ts::UsersController.create", Name: "create", Kind: graph.KindMethod, FilePath: "pkg/users.ts", StartLine: 9, EndLine: 11},
		{ID: "pkg/users.ts::CreateUserDto", Name: "CreateUserDto", Kind: graph.KindType, FilePath: "pkg/users.ts", StartLine: 2, EndLine: 2},
		{ID: "pkg/users.ts::UserResp", Name: "UserResp", Kind: graph.KindType, FilePath: "pkg/users.ts", StartLine: 3, EndLine: 3},
	}
	cs := (&HTTPExtractor{}).Extract("pkg/users.ts", src, nodes, nil)
	c := findContract(t, cs, "http::POST::/", RoleProvider)

	assertMetaString(t, c, "request_type", "pkg/users.ts::CreateUserDto")
	assertMetaString(t, c, "response_type", "pkg/users.ts::UserResp")
	assertMetaStrings(t, c, "query_params", []string{"tenant"})
	assertMetaInts(t, c, "status_codes", []int{201})
	assertMetaString(t, c, "schema_source", "extracted")
}

// -----------------------------------------------------------------------------
// Express
// -----------------------------------------------------------------------------

func TestHTTPEnrich_TS_Express_BodyCastResJSON(t *testing.T) {
	src := []byte(`import type { Request, Response } from 'express'
import { UserReq } from './req'
import { UserResp } from './resp'

export function register(app: any) {
  app.post('/users', createUser)
}

function createUser(req: Request, res: Response) {
  const body = req.body as UserReq
  const result: UserResp = toResp(body)
  res.status(201).json(result)
}

function toResp(_: UserReq): UserResp { return {} as UserResp }
`)
	nodes := []*graph.Node{
		{ID: "pkg/api.ts::register", Name: "register", Kind: graph.KindFunction, FilePath: "pkg/api.ts", StartLine: 5, EndLine: 7},
		{ID: "pkg/api.ts::createUser", Name: "createUser", Kind: graph.KindFunction, FilePath: "pkg/api.ts", StartLine: 9, EndLine: 13},
		{ID: "pkg/api.ts::UserReq", Name: "UserReq", Kind: graph.KindType, FilePath: "pkg/api.ts", StartLine: 2, EndLine: 2},
		{ID: "pkg/api.ts::UserResp", Name: "UserResp", Kind: graph.KindType, FilePath: "pkg/api.ts", StartLine: 3, EndLine: 3},
	}
	cs := (&HTTPExtractor{}).Extract("pkg/api.ts", src, nodes, nil)
	c := findContract(t, cs, "http::POST::/users", RoleProvider)

	assertMetaString(t, c, "request_type", "pkg/api.ts::UserReq")
	assertMetaString(t, c, "response_type", "pkg/api.ts::UserResp")
	assertMetaInts(t, c, "status_codes", []int{201})
	assertMetaString(t, c, "schema_source", "extracted")
}

// -----------------------------------------------------------------------------
// Axios consumer
// -----------------------------------------------------------------------------

func TestHTTPEnrich_TS_Axios_GenericsAndPayload(t *testing.T) {
	src := []byte(`import axios from 'axios'
import type { UserReq, UserResp } from './types'

export async function createUser(payload: UserReq): Promise<UserResp> {
  const { data } = await axios.post<UserResp, UserReq>('/api/users', payload)
  return data
}
`)
	nodes := []*graph.Node{
		{ID: "pkg/client.ts::createUser", Name: "createUser", Kind: graph.KindFunction, FilePath: "pkg/client.ts", StartLine: 4, EndLine: 7},
		{ID: "pkg/client.ts::UserReq", Name: "UserReq", Kind: graph.KindType, FilePath: "pkg/client.ts", StartLine: 2, EndLine: 2},
		{ID: "pkg/client.ts::UserResp", Name: "UserResp", Kind: graph.KindType, FilePath: "pkg/client.ts", StartLine: 2, EndLine: 2},
	}
	cs := (&HTTPExtractor{}).Extract("pkg/client.ts", src, nodes, nil)
	c := findContract(t, cs, "http::POST::/api/users", RoleConsumer)

	assertMetaString(t, c, "request_type", "pkg/client.ts::UserReq")
	assertMetaString(t, c, "response_type", "pkg/client.ts::UserResp")
	assertMetaString(t, c, "schema_source", "extracted")
}

// -----------------------------------------------------------------------------
// Custom request<T>() wrapper
// -----------------------------------------------------------------------------

// The wrapper pattern real web clients use: `request<UserResp>(path, token, opts)`.
// The enricher should pick the generic type argument as the response
// and the body argument as the request.
func TestHTTPEnrich_TS_WrapperGeneric_ResponseType(t *testing.T) {
	src := []byte(`import type { EmailSource, UpdateReq } from './types'

export async function blockEmailSource(token: string, id: string): Promise<EmailSource> {
  return request<EmailSource>('/v1/email-sources/' + id + '/block', token, {
    method: 'POST',
  })
}

export async function updateSource(token: string, id: string, payload: UpdateReq): Promise<EmailSource> {
  return request<EmailSource>('/v1/email-sources/' + id, token, {
    method: 'PUT',
    body: payload,
  })
}
`)
	nodes := []*graph.Node{
		{ID: "pkg/api.ts::blockEmailSource", Name: "blockEmailSource", Kind: graph.KindFunction, FilePath: "pkg/api.ts", StartLine: 3, EndLine: 7},
		{ID: "pkg/api.ts::updateSource", Name: "updateSource", Kind: graph.KindFunction, FilePath: "pkg/api.ts", StartLine: 9, EndLine: 14},
		{ID: "pkg/api.ts::EmailSource", Name: "EmailSource", Kind: graph.KindType, FilePath: "pkg/api.ts", StartLine: 1, EndLine: 1},
		{ID: "pkg/api.ts::UpdateReq", Name: "UpdateReq", Kind: graph.KindType, FilePath: "pkg/api.ts", StartLine: 1, EndLine: 1},
	}
	// Use a fetch pattern so httpPatterns picks it up; the wrapper
	// enricher doesn't register a new route pattern, it just
	// enriches existing consumer contracts. For this test we
	// synthesize a Contract and run enrichment directly.
	// Easier: use fetch-visible pattern on the path, so we have a
	// contract to enrich. But the wrapper detector runs on whatever
	// contract exists.
	// Shortcut: craft a consumer contract directly and call the
	// exported enricher.
	for _, tc := range []struct {
		id      string
		startFn int
		wantReq string
	}{
		{id: "http::POST::/v1/email-sources/{p1}/block", startFn: 3, wantReq: ""},
		{id: "http::PUT::/v1/email-sources/{p1}", startFn: 9, wantReq: "pkg/api.ts::UpdateReq"},
	} {
		c := Contract{
			ID:       tc.id,
			Type:     ContractHTTP,
			Role:     RoleConsumer,
			SymbolID: "pkg/api.ts::" + nodes[len(nodes)-4+0].Name, // unused
			FilePath: "pkg/api.ts",
			Line:     tc.startFn,
			Meta:     map[string]any{"method": "POST", "path": "/dummy"},
		}
		lines := splitLinesForTest(src)
		EnrichHTTPContract(&c, lines, nodes, "typescript")
		if got := c.Meta["response_type"]; got != "pkg/api.ts::EmailSource" {
			t.Errorf("%s response_type = %v, want pkg/api.ts::EmailSource", tc.id, got)
		}
		if tc.wantReq != "" {
			if got := c.Meta["request_type"]; got != tc.wantReq {
				t.Errorf("%s request_type = %v, want %s", tc.id, got, tc.wantReq)
			}
		}
	}
}

func splitLinesForTest(src []byte) []string {
	out := make([]string, 0)
	start := 0
	for i, b := range src {
		if b == '\n' {
			out = append(out, string(src[start:i]))
			start = i + 1
		}
	}
	out = append(out, string(src[start:]))
	return out
}

// -----------------------------------------------------------------------------
// Fetch consumer
// -----------------------------------------------------------------------------

func TestHTTPEnrich_TS_Fetch_StringifyAndCast(t *testing.T) {
	src := []byte(`import type { TuckReq, TuckResp } from './types'

export async function createTuck(): Promise<TuckResp> {
  const payload: TuckReq = { title: 'a' }
  const resp = await fetch('/v1/tucks', {
    method: 'POST',
    body: JSON.stringify(payload),
  })
  const data = (await resp.json()) as TuckResp
  return data
}
`)
	nodes := []*graph.Node{
		{ID: "pkg/client.ts::createTuck", Name: "createTuck", Kind: graph.KindFunction, FilePath: "pkg/client.ts", StartLine: 3, EndLine: 10},
		{ID: "pkg/client.ts::TuckReq", Name: "TuckReq", Kind: graph.KindType, FilePath: "pkg/client.ts", StartLine: 1, EndLine: 1},
		{ID: "pkg/client.ts::TuckResp", Name: "TuckResp", Kind: graph.KindType, FilePath: "pkg/client.ts", StartLine: 1, EndLine: 1},
	}
	cs := (&HTTPExtractor{}).Extract("pkg/client.ts", src, nodes, nil)
	c := findContract(t, cs, "http::POST::/v1/tucks", RoleConsumer)

	assertMetaString(t, c, "request_type", "pkg/client.ts::TuckReq")
	assertMetaString(t, c, "response_type", "pkg/client.ts::TuckResp")
	assertMetaString(t, c, "schema_source", "extracted")
}
