package contracts

import (
	"testing"
)

// Helpers -------------------------------------------------------------------

func makeContract(role Role, repo, contractID string, meta map[string]any) Contract {
	return Contract{
		ID:         contractID,
		Type:       ContractHTTP,
		Role:       role,
		RepoPrefix: repo,
		Meta:       meta,
	}
}

func buildRegistry(contracts ...Contract) *Registry {
	r := NewRegistry()
	for _, c := range contracts {
		r.Add(c)
	}
	return r
}

// shapeMap is a trivial in-memory ShapeLookup keyed by symbol ID.
type shapeMap map[string]*Shape

func (sm shapeMap) lookup(id string) *Shape { return sm[id] }

func findIssue(t *testing.T, issues []ContractIssue, kind string, field string) *ContractIssue {
	t.Helper()
	for i := range issues {
		if issues[i].Kind == kind && issues[i].Field == field {
			return &issues[i]
		}
	}
	kinds := make([]string, 0, len(issues))
	for _, is := range issues {
		kinds = append(kinds, is.Kind+"("+is.Field+")")
	}
	t.Fatalf("missing %s(%s) in %v", kind, field, kinds)
	return nil
}

// Tests ---------------------------------------------------------------------

func TestValidate_OrphanConsumer(t *testing.T) {
	reg := buildRegistry(
		makeContract(RoleConsumer, "web", "http::GET::/ghost", map[string]any{}),
	)
	got := Validate(reg, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 issue, got %d", len(got))
	}
	if got[0].Kind != IssueOrphanConsumer {
		t.Errorf("kind = %q", got[0].Kind)
	}
	if got[0].Severity != SeverityWarning {
		t.Errorf("severity = %q, want warning", got[0].Severity)
	}
}

func TestValidate_OrphanProvider(t *testing.T) {
	reg := buildRegistry(
		makeContract(RoleProvider, "api", "http::GET::/health", map[string]any{}),
	)
	got := Validate(reg, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 issue, got %d", len(got))
	}
	if got[0].Kind != IssueOrphanProvider || got[0].Severity != SeverityInfo {
		t.Errorf("kind=%s severity=%s", got[0].Kind, got[0].Severity)
	}
}

// Response: provider removed a field that consumer still reads — breaking.
func TestValidate_Response_FieldRemoved_Breaking(t *testing.T) {
	reg := buildRegistry(
		makeContract(RoleProvider, "api", "http::GET::/users", map[string]any{
			"response_type": "api/resp.go::UserResp",
		}),
		makeContract(RoleConsumer, "web", "http::GET::/users", map[string]any{
			"response_type": "web/types.ts::UserResp",
		}),
	)
	shapes := shapeMap{
		"api/resp.go::UserResp": {
			Kind: "struct",
			Fields: []ShapeField{
				{Name: "id", Type: "string", Required: true},
			},
		},
		"web/types.ts::UserResp": {
			Kind: "interface",
			Fields: []ShapeField{
				{Name: "id", Type: "string", Required: true},
				{Name: "email", Type: "string", Required: true}, // consumer still expects it
			},
		},
	}
	issues := Validate(reg, shapes.lookup)
	iss := findIssue(t, issues, IssueResponseFieldRemoved, "email")
	if iss.Severity != SeverityBreaking {
		t.Errorf("severity = %q, want breaking", iss.Severity)
	}
}

// Response: consumer optionally reads a missing field — downgraded warning.
func TestValidate_Response_FieldRemoved_Optional_Warning(t *testing.T) {
	reg := buildRegistry(
		makeContract(RoleProvider, "api", "http::GET::/users", map[string]any{
			"response_type": "api/resp.go::UserResp",
		}),
		makeContract(RoleConsumer, "web", "http::GET::/users", map[string]any{
			"response_type": "web/types.ts::UserResp",
		}),
	)
	shapes := shapeMap{
		"api/resp.go::UserResp": {
			Fields: []ShapeField{{Name: "id", Type: "string", Required: true}},
		},
		"web/types.ts::UserResp": {
			Fields: []ShapeField{
				{Name: "id", Type: "string", Required: true},
				{Name: "nickname", Type: "string", Required: false},
			},
		},
	}
	issues := Validate(reg, shapes.lookup)
	iss := findIssue(t, issues, IssueResponseFieldRemoved, "nickname")
	if iss.Severity != SeverityWarning {
		t.Errorf("severity = %q, want warning (optional field absent)", iss.Severity)
	}
}

// Response: provider emits extra field consumer doesn't read — info.
func TestValidate_Response_FieldAdded_Info(t *testing.T) {
	reg := buildRegistry(
		makeContract(RoleProvider, "api", "http::GET::/x", map[string]any{"response_type": "api/resp.go::X"}),
		makeContract(RoleConsumer, "web", "http::GET::/x", map[string]any{"response_type": "web/resp.ts::X"}),
	)
	shapes := shapeMap{
		"api/resp.go::X": {Fields: []ShapeField{
			{Name: "id", Type: "string", Required: true},
			{Name: "notes", Type: "string", Required: false},
		}},
		"web/resp.ts::X": {Fields: []ShapeField{{Name: "id", Type: "string", Required: true}}},
	}
	issues := Validate(reg, shapes.lookup)
	iss := findIssue(t, issues, IssueResponseFieldAdded, "notes")
	if iss.Severity != SeverityInfo {
		t.Errorf("severity = %q, want info", iss.Severity)
	}
}

// Request: provider requires a field the consumer doesn't send — breaking.
func TestValidate_Request_RequiredFieldMissing_Breaking(t *testing.T) {
	reg := buildRegistry(
		makeContract(RoleProvider, "api", "http::POST::/users", map[string]any{
			"request_type": "api/req.go::CreateUser",
		}),
		makeContract(RoleConsumer, "web", "http::POST::/users", map[string]any{
			"request_type": "web/req.ts::CreateUser",
		}),
	)
	shapes := shapeMap{
		"api/req.go::CreateUser": {Fields: []ShapeField{
			{Name: "email", Type: "string", Required: true},
			{Name: "password", Type: "string", Required: true},
		}},
		"web/req.ts::CreateUser": {Fields: []ShapeField{
			{Name: "email", Type: "string", Required: true},
			// missing password
		}},
	}
	issues := Validate(reg, shapes.lookup)
	iss := findIssue(t, issues, IssueRequestFieldRequired, "password")
	if iss.Severity != SeverityBreaking {
		t.Errorf("severity = %q, want breaking", iss.Severity)
	}
}

// Request: consumer sends a field the provider doesn't accept — info.
func TestValidate_Request_ExtraField_Info(t *testing.T) {
	reg := buildRegistry(
		makeContract(RoleProvider, "api", "http::POST::/x", map[string]any{"request_type": "api/req.go::X"}),
		makeContract(RoleConsumer, "web", "http::POST::/x", map[string]any{"request_type": "web/req.ts::X"}),
	)
	shapes := shapeMap{
		"api/req.go::X": {Fields: []ShapeField{{Name: "id", Type: "string", Required: true}}},
		"web/req.ts::X": {Fields: []ShapeField{
			{Name: "id", Type: "string", Required: true},
			{Name: "debug", Type: "boolean"},
		}},
	}
	issues := Validate(reg, shapes.lookup)
	iss := findIssue(t, issues, IssueRequestFieldExtra, "debug")
	if iss.Severity != SeverityInfo {
		t.Errorf("severity = %q, want info", iss.Severity)
	}
}

// Shared field with different types across provider / consumer.
func TestValidate_TypeChanged_Breaking(t *testing.T) {
	reg := buildRegistry(
		makeContract(RoleProvider, "api", "http::GET::/thing", map[string]any{"response_type": "api/resp.go::Thing"}),
		makeContract(RoleConsumer, "web", "http::GET::/thing", map[string]any{"response_type": "web/resp.ts::Thing"}),
	)
	shapes := shapeMap{
		"api/resp.go::Thing": {Fields: []ShapeField{{Name: "id", Type: "int64", Required: true}}},
		"web/resp.ts::Thing": {Fields: []ShapeField{{Name: "id", Type: "string", Required: true}}},
	}
	issues := Validate(reg, shapes.lookup)
	iss := findIssue(t, issues, IssueResponseFieldTypeChanged, "id")
	if iss.Severity != SeverityBreaking {
		t.Errorf("severity = %q, want breaking", iss.Severity)
	}
	if iss.Details == "" {
		t.Error("expected details with provider/consumer types")
	}
}

// Shape comparison is tolerant across language boundaries:
//
//   - Nullable-optional variants: `*User` (Go) ≡ `User | null` (TS) ≡ `User | None` (Py).
//   - Collection variants: `[]User` ≡ `Array<User>` ≡ `List<User>` ≡ `User[]`.
//   - Package qualifier: `api.User` ≡ `User`.
//   - Cross-language primitive aliases: `string` ≡ `String` ≡ `str`,
//     `bool` ≡ `Boolean`, etc.
//
// Otherwise every cross-language contract would look broken.
func TestValidate_TypesCompatible_AcrossLanguages(t *testing.T) {
	cases := [][2]string{
		{"*User", "User | null"},
		{"User", "User | None"},
		{"[]User", "Array<User>"},
		{"List<User>", "User[]"},
		{"api.User", "User"},
		{"string", "String"},              // Go / Dart
		{"string", "str"},                 // Go / Python
		{"bool", "Boolean"},               // Go / Java / Kotlin
		{"int64", "Long"},                 // Go / Java
		{"float64", "Double"},             // Go / Java / Dart
		{"time.Time", "DateTime"},         // Go / Dart
		{"time.Time", "Instant"},          // Go / Java (java.time)
		{"time.Time", "datetime"},         // Go / Python
		{"time.Time", "OffsetDateTime"},   // Go / Java
		{"DateTime", "Instant"},           // Dart / Java
		{"time.Duration", "Duration"},     // Go / Dart / Java
		{"time.Duration", "timedelta"},    // Go / Python
		{"[]byte", "Uint8List"},           // Go / Dart
		{"[]byte", "bytes"},               // Go / Python
		{"UUID", "Uuid"},                  // Python / Rust
	}
	for _, c := range cases {
		if !canAssign(c[0], c[1]) {
			t.Errorf("canAssign(%q, %q) = false, want true (symmetric alias case)", c[0], c[1])
		}
		if !canAssign(c[1], c[0]) {
			t.Errorf("canAssign(%q, %q) = false, want true (symmetric alias case)", c[1], c[0])
		}
	}
}

// Directional numeric compatibility: int widens to float (JSON
// numbers coerce), float → int truncates and is breaking.
func TestValidate_CanAssign_NumericWidening(t *testing.T) {
	if !canAssign("float", "int") {
		t.Error("canAssign(float, int) = false, want true (JSON numbers widen)")
	}
	if canAssign("int", "float") {
		t.Error("canAssign(int, float) = true, want false (truncation risk)")
	}
	if !canAssign("Double", "int32") {
		t.Error("canAssign(Double, int32) across languages failed")
	}
	if canAssign("int64", "Double") {
		t.Error("canAssign(int64, Double) should fail — float → int is lossy")
	}
}

// `any` / `interface{}` / `Dynamic` / `object` are wire-level
// universals. A slot typed `any` can hold anything. A slot typed `X`
// can NOT safely hold `any` — you'd be trusting unchecked data.
func TestValidate_CanAssign_AnyType(t *testing.T) {
	if !canAssign("any", "User") {
		t.Error("canAssign(any, User) = false, want true")
	}
	if !canAssign("interface{}", "int") {
		t.Error("canAssign(interface{}, int) = false, want true")
	}
	if canAssign("User", "any") {
		t.Error("canAssign(User, any) = true, want false (can't trust unchecked data)")
	}
	if canAssign("int", "Dynamic") {
		t.Error("canAssign(int, Dynamic) = true, want false")
	}
}

// Same field on both sides where the provider emits a string and
// the consumer reads the same thing spelled differently by language.
// Previously this was a `response_field_type_changed` breaking issue.
// Now it's compatible and emits no issue.
func TestValidate_Response_PrimitiveAlias_NoIssue(t *testing.T) {
	reg := buildRegistry(
		makeContract(RoleProvider, "core-api", "http::GET::/x", map[string]any{
			"response_type": "core-api/resp.go::Thing",
		}),
		makeContract(RoleConsumer, "tuck_app", "http::GET::/x", map[string]any{
			"response_type": "tuck_app/resp.dart::Thing",
		}),
	)
	shapes := shapeMap{
		"core-api/resp.go::Thing": {Fields: []ShapeField{
			{Name: "tuckId", Type: "string", Required: true},
			{Name: "count", Type: "int", Required: true},
		}},
		"tuck_app/resp.dart::Thing": {Fields: []ShapeField{
			{Name: "tuckId", Type: "String", Required: true},
			// Consumer reads a float even though provider sends int —
			// widening is safe.
			{Name: "count", Type: "double", Required: true},
		}},
	}
	issues := Validate(reg, shapes.lookup)
	for _, is := range issues {
		if is.Kind == IssueResponseFieldTypeChanged {
			t.Errorf("unexpected response_field_type_changed for field %q: %s", is.Field, is.Details)
		}
	}
}

// UUIDs and bytes serialise as strings on the wire, so a `str` slot
// can hold them — but the reverse isn't safe. The validator should
// treat these widening directions and flag the narrow ones.
func TestValidate_CanAssign_WireString_WideningOnly(t *testing.T) {
	// Wide: str can hold UUID / bytes (serialised form is a string).
	if !canAssign("string", "UUID") {
		t.Error("canAssign(string, UUID) = false, want true (UUID serialises as string)")
	}
	if !canAssign("String", "[]byte") {
		t.Error("canAssign(String, []byte) = false, want true (bytes serialise as base64 string)")
	}
	// Narrow: a string slot accepting an arbitrary string on the
	// consumer side when the provider declared a UUID — the consumer
	// might receive a non-UUID and fail at runtime if it expected
	// one. Breaking.
	if canAssign("UUID", "string") {
		t.Error("canAssign(UUID, string) = true, want false (arbitrary strings aren't UUIDs)")
	}
	if canAssign("bytes", "string") {
		t.Error("canAssign(bytes, string) = true, want false (arbitrary strings aren't base64)")
	}
}

// Realistic cross-language contract: provider is Go (time.Time,
// []byte, UUID), consumer is Dart (DateTime, Uint8List, String).
// None of these should flag as breaking.
func TestValidate_Response_CrossLangTemporal_NoIssue(t *testing.T) {
	reg := buildRegistry(
		makeContract(RoleProvider, "core-api", "http::GET::/x", map[string]any{
			"response_type": "core-api/resp.go::Thing",
		}),
		makeContract(RoleConsumer, "tuck_app", "http::GET::/x", map[string]any{
			"response_type": "tuck_app/resp.dart::Thing",
		}),
	)
	shapes := shapeMap{
		"core-api/resp.go::Thing": {Fields: []ShapeField{
			{Name: "createdAt", Type: "time.Time", Required: true},
			{Name: "ttl", Type: "time.Duration", Required: true},
			{Name: "sessionId", Type: "uuid.UUID", Required: true},
			{Name: "payload", Type: "[]byte", Required: true},
		}},
		"tuck_app/resp.dart::Thing": {Fields: []ShapeField{
			{Name: "createdAt", Type: "DateTime", Required: true},
			{Name: "ttl", Type: "Duration", Required: true},
			// Consumer types sessionId as String — UUID serialises
			// that way and the slot accepts it. Not breaking.
			{Name: "sessionId", Type: "String", Required: true},
			{Name: "payload", Type: "String", Required: true}, // base64
		}},
	}
	issues := Validate(reg, shapes.lookup)
	for _, is := range issues {
		if is.Kind == IssueResponseFieldTypeChanged {
			t.Errorf("unexpected type-change on %q: provider=%s consumer=%s — %s",
				is.Field, is.ProviderType, is.ConsumerType, is.Details)
		}
	}
}

// Request direction: provider expects int, consumer sends float →
// breaking. The widening rule is direction-sensitive, and a value
// that's "compatible" as a response read isn't as a request write.
func TestValidate_Request_NarrowingBreaks(t *testing.T) {
	reg := buildRegistry(
		makeContract(RoleProvider, "api", "http::POST::/x", map[string]any{
			"request_type": "api/req.go::X",
		}),
		makeContract(RoleConsumer, "web", "http::POST::/x", map[string]any{
			"request_type": "web/req.ts::X",
		}),
	)
	shapes := shapeMap{
		"api/req.go::X": {Fields: []ShapeField{{Name: "n", Type: "int", Required: true}}},
		"web/req.ts::X": {Fields: []ShapeField{{Name: "n", Type: "float", Required: true}}},
	}
	issues := Validate(reg, shapes.lookup)
	found := false
	for _, is := range issues {
		if is.Kind == IssueRequestFieldTypeChanged && is.Field == "n" {
			found = true
			if is.Severity != SeverityBreaking {
				t.Errorf("want breaking, got %s", is.Severity)
			}
		}
	}
	if !found {
		t.Error("expected request_field_type_changed for n (float→int narrowing)")
	}
}

// Missing shape on one side → warning that diff was skipped, not breaking.
func TestValidate_TypeUnknown_Warning(t *testing.T) {
	reg := buildRegistry(
		makeContract(RoleProvider, "api", "http::GET::/x", map[string]any{"response_type": "api/resp.go::X"}),
		makeContract(RoleConsumer, "web", "http::GET::/x", map[string]any{"response_type": "web/resp.ts::X"}),
	)
	// Only provider shape is known; consumer type is a symbol ID we
	// can't resolve (cross-repo, not re-indexed).
	shapes := shapeMap{
		"api/resp.go::X": {Fields: []ShapeField{{Name: "id", Type: "string", Required: true}}},
	}
	issues := Validate(reg, shapes.lookup)
	found := false
	for _, is := range issues {
		if is.Kind == IssueResponseTypeUnknown && is.Severity == SeverityWarning {
			found = true
		}
	}
	if !found {
		t.Errorf("expected response_type_unknown warning, got %#v", issues)
	}
}

// Matched pair with identical shapes → zero issues.
func TestValidate_MatchingShapes_NoIssues(t *testing.T) {
	reg := buildRegistry(
		makeContract(RoleProvider, "api", "http::POST::/users", map[string]any{
			"request_type":  "api/req.go::User",
			"response_type": "api/resp.go::User",
		}),
		makeContract(RoleConsumer, "web", "http::POST::/users", map[string]any{
			"request_type":  "web/req.ts::User",
			"response_type": "web/resp.ts::User",
		}),
	)
	shape := &Shape{Fields: []ShapeField{
		{Name: "id", Type: "string", Required: true},
		{Name: "email", Type: "string", Required: true},
	}}
	shapes := shapeMap{
		"api/req.go::User":  shape,
		"api/resp.go::User": shape,
		"web/req.ts::User":  shape,
		"web/resp.ts::User": shape,
	}
	issues := Validate(reg, shapes.lookup)
	if len(issues) != 0 {
		t.Errorf("want 0 issues, got %d: %#v", len(issues), issues)
	}
}
