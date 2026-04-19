package contracts

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestHTTPEnrich_CSharp_ASPNET_AttributeRouting(t *testing.T) {
	src := []byte(`using Microsoft.AspNetCore.Mvc;

public class CreateUserReq { public string Email { get; set; } }
public class UserResp { public string Id { get; set; } }

[ApiController]
[Route("users")]
public class UsersController : ControllerBase {
    [HttpPost("/")]
    [ProducesResponseType(StatusCodes.Status201Created)]
    public async Task<ActionResult<UserResp>> Create(
        [FromBody] CreateUserReq req,
        [FromQuery(Name = "tenant")] string tenant
    ) {
        return Ok(new UserResp());
    }
}
`)
	nodes := []*graph.Node{
		{ID: "pkg/UsersController.cs::UsersController", Name: "UsersController", Kind: graph.KindType, FilePath: "pkg/UsersController.cs", StartLine: 8, EndLine: 17},
		{ID: "pkg/UsersController.cs::UsersController.Create", Name: "Create", Kind: graph.KindMethod, FilePath: "pkg/UsersController.cs", StartLine: 9, EndLine: 16},
		{ID: "pkg/UsersController.cs::CreateUserReq", Name: "CreateUserReq", Kind: graph.KindType, FilePath: "pkg/UsersController.cs", StartLine: 3, EndLine: 3},
		{ID: "pkg/UsersController.cs::UserResp", Name: "UserResp", Kind: graph.KindType, FilePath: "pkg/UsersController.cs", StartLine: 4, EndLine: 4},
	}
	cs := (&HTTPExtractor{}).Extract("pkg/UsersController.cs", src, nodes, nil)
	c := findContract(t, cs, "http::POST::/", RoleProvider)

	assertMetaString(t, c, "request_type", "pkg/UsersController.cs::CreateUserReq")
	assertMetaString(t, c, "response_type", "pkg/UsersController.cs::UserResp")
	assertMetaStrings(t, c, "query_params", []string{"tenant"})
	assertMetaInts(t, c, "status_codes", []int{200, 201})
	assertMetaString(t, c, "schema_source", "extracted")
}

func TestHTTPEnrich_CSharp_MinimalAPI(t *testing.T) {
	src := []byte(`var app = WebApplication.CreateBuilder().Build();

app.MapGet("/health", () => Results.Ok(new { ok = true }));
`)
	cs := (&HTTPExtractor{}).Extract("pkg/Program.cs", src, nil, nil)
	c := findContract(t, cs, "http::GET::/health", RoleProvider)
	if c.ID == "" {
		t.Fatal("expected /health contract")
	}
}

func TestShape_CSharp_ClassProperties(t *testing.T) {
	src := []byte(`public class UserResp
{
    [JsonPropertyName("id")]
    public Guid Id { get; set; }

    public string Email { get; set; }

    [JsonPropertyName("pw")]
    public string Password { get; set; }

    public string? Nickname { get; set; }

    public List<string> Tags { get; set; }

    [JsonIgnore]
    public string Internal { get; set; }
}
`)
	s := ExtractShape("pkg/UserResp.cs", src, 1, 17)
	if s == nil {
		t.Fatal("nil shape")
	}
	want := map[string]ShapeField{
		"id":       {Name: "id", Type: "Guid", JSONTag: "id", Required: true},
		"Email":    {Name: "Email", Type: "string", Required: true},
		"pw":       {Name: "pw", Type: "string", JSONTag: "pw", Required: true},
		"Nickname": {Name: "Nickname", Type: "string", Required: false},
		"Tags":     {Name: "Tags", Type: "List", Required: true, Repeated: true},
		// Internal omitted (JsonIgnore).
	}
	assertShapeFields(t, s, want)
}

func TestShape_CSharp_PositionalRecord(t *testing.T) {
	src := []byte(`public record UserResp(string Id, string Email, string? Nickname);
`)
	s := ExtractShape("pkg/UserResp.cs", src, 1, 1)
	if s == nil {
		t.Fatal("nil shape")
	}
	want := map[string]ShapeField{
		"Id":       {Name: "Id", Type: "string", Required: true},
		"Email":    {Name: "Email", Type: "string", Required: true},
		"Nickname": {Name: "Nickname", Type: "string", Required: false},
	}
	assertShapeFields(t, s, want)
}
