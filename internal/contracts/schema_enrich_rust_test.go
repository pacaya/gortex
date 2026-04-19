package contracts

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestHTTPEnrich_Rust_Axum_JsonBodyAndReturn(t *testing.T) {
	src := []byte(`use axum::{Json, Router, routing::post, http::StatusCode};

#[derive(Deserialize)]
struct CreateUserReq { email: String }

#[derive(Serialize)]
struct UserResp { id: String }

fn router() -> Router {
    Router::new().route("/users", post(create_user))
}

async fn create_user(Json(payload): Json<CreateUserReq>) -> (StatusCode, Json<UserResp>) {
    (StatusCode::CREATED, Json(UserResp { id: "x".into() }))
}
`)
	nodes := []*graph.Node{
		{ID: "pkg/api.rs::router", Name: "router", Kind: graph.KindFunction, FilePath: "pkg/api.rs", StartLine: 9, EndLine: 11},
		{ID: "pkg/api.rs::create_user", Name: "create_user", Kind: graph.KindFunction, FilePath: "pkg/api.rs", StartLine: 13, EndLine: 15},
		{ID: "pkg/api.rs::CreateUserReq", Name: "CreateUserReq", Kind: graph.KindType, FilePath: "pkg/api.rs", StartLine: 3, EndLine: 4},
		{ID: "pkg/api.rs::UserResp", Name: "UserResp", Kind: graph.KindType, FilePath: "pkg/api.rs", StartLine: 6, EndLine: 7},
	}
	cs := (&HTTPExtractor{}).Extract("pkg/api.rs", src, nodes, nil)
	c := findContract(t, cs, "http::POST::/users", RoleProvider)

	assertMetaString(t, c, "request_type", "pkg/api.rs::CreateUserReq")
	assertMetaString(t, c, "response_type", "pkg/api.rs::UserResp")
	assertMetaInts(t, c, "status_codes", []int{201})
	assertMetaString(t, c, "schema_source", "extracted")
}

func TestHTTPEnrich_Rust_Actix_MacroAndExtractor(t *testing.T) {
	src := []byte(`use actix_web::{get, web, HttpResponse, http::StatusCode};

struct User { id: String }

#[get("/users/{id}")]
async fn show_user(path: web::Path<String>) -> HttpResponse {
    HttpResponse::build(StatusCode::OK).json(User { id: "x".into() })
}
`)
	nodes := []*graph.Node{
		{ID: "pkg/api.rs::show_user", Name: "show_user", Kind: graph.KindFunction, FilePath: "pkg/api.rs", StartLine: 5, EndLine: 8},
		{ID: "pkg/api.rs::User", Name: "User", Kind: graph.KindType, FilePath: "pkg/api.rs", StartLine: 3, EndLine: 3},
	}
	cs := (&HTTPExtractor{}).Extract("pkg/api.rs", src, nodes, nil)
	c := findContract(t, cs, "http::GET::/users/{p1}", RoleProvider)
	assertMetaInts(t, c, "status_codes", []int{200})
}

func TestHTTPEnrich_Rust_Consumer_Reqwest(t *testing.T) {
	src := []byte(`use reqwest::Client;

struct CreateReq { name: String }
struct CreateResp { id: String }

async fn create(client: &Client, payload: CreateReq) -> Result<CreateResp, reqwest::Error> {
    let resp = client.post("/v1/users")
        .json(&payload)
        .send().await?
        .json::<CreateResp>().await?;
    Ok(resp)
}
`)
	nodes := []*graph.Node{
		{ID: "pkg/api.rs::create", Name: "create", Kind: graph.KindFunction, FilePath: "pkg/api.rs", StartLine: 6, EndLine: 12},
		{ID: "pkg/api.rs::CreateReq", Name: "CreateReq", Kind: graph.KindType, FilePath: "pkg/api.rs", StartLine: 3, EndLine: 3},
		{ID: "pkg/api.rs::CreateResp", Name: "CreateResp", Kind: graph.KindType, FilePath: "pkg/api.rs", StartLine: 4, EndLine: 4},
	}
	cs := (&HTTPExtractor{}).Extract("pkg/api.rs", src, nodes, nil)
	c := findContract(t, cs, "http::POST::/v1/users", RoleConsumer)
	assertMetaString(t, c, "request_type", "pkg/api.rs::CreateReq")
	assertMetaString(t, c, "response_type", "pkg/api.rs::CreateResp")
}

func TestShape_Rust_SerdeStruct(t *testing.T) {
	src := []byte(`#[derive(Serialize, Deserialize)]
pub struct UserResp {
    pub id: String,
    #[serde(rename = "emailAddress")]
    pub email: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub nickname: Option<String>,
    pub tags: Vec<String>,
    pub count: u64,
}
`)
	s := ExtractShape("pkg/resp.rs", src, 1, 10)
	if s == nil {
		t.Fatal("nil shape")
	}
	want := map[string]ShapeField{
		"id":           {Name: "id", Type: "String", Required: true},
		"emailAddress": {Name: "emailAddress", Type: "String", JSONTag: "emailAddress", Required: true},
		"nickname":     {Name: "nickname", Type: "String", Required: false},
		"tags":         {Name: "tags", Type: "String", Required: true, Repeated: true},
		"count":        {Name: "count", Type: "u64", Required: true},
	}
	assertShapeFields(t, s, want)
}
