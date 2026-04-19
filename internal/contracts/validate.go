package contracts

import (
	"fmt"
	"sort"
	"strings"
)

// Severity rates how much a validation issue impacts running code.
//
//   * SeverityBreaking — the contract is broken for at least one side.
//     Code will fail at runtime, or serialisation will silently lose
//     data that the caller relied on.
//   * SeverityWarning  — drift that might become breaking later. Fields
//     added or removed that aren't load-bearing right now.
//   * SeverityInfo     — observational; no action required, but useful
//     for reviewers ("provider added a field, consumer may want to
//     start reading it").
type Severity string

const (
	SeverityBreaking Severity = "breaking"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

// ContractIssue describes one diff between a contract's provider and
// consumer shapes (or the absence of one side). The JSON form is what
// the MCP tool returns and what the UI renders in the validation
// panel.
type ContractIssue struct {
	ContractID   string   `json:"contract_id"`
	Kind         string   `json:"kind"`
	Severity     Severity `json:"severity"`
	Provider     string   `json:"provider,omitempty"`     // repo prefix
	Consumer     string   `json:"consumer,omitempty"`     // repo prefix
	Field        string   `json:"field,omitempty"`        // field name when relevant
	Details      string   `json:"details,omitempty"`      // human-readable explanation
	ProviderType string   `json:"provider_type,omitempty"`
	ConsumerType string   `json:"consumer_type,omitempty"`
}

// ShapeLookup resolves a type's symbol ID to its field-level Shape.
// Callers (normally the MCP server) implement this by consulting the
// graph — the shape lives on the type node's Meta["shape"]. Returning
// nil means the type isn't indexed in the current registry's scope,
// which is itself a validation signal ("we can't compare because
// we haven't seen the type").
type ShapeLookup func(symbolID string) *Shape

// Issue kind constants — kept as strings (not iota) so the MCP wire
// format and the UI's switch statements reference the same literal
// values the tests assert on.
const (
	IssueOrphanProvider          = "orphan_provider"
	IssueOrphanConsumer          = "orphan_consumer"
	IssueRequestTypeUnknown      = "request_type_unknown"
	IssueResponseTypeUnknown     = "response_type_unknown"
	IssueResponseFieldRemoved    = "response_field_removed"
	IssueResponseFieldTypeChanged = "response_field_type_changed"
	IssueResponseFieldAdded      = "response_field_added"
	IssueRequestFieldRequired    = "request_field_required_missing"
	IssueRequestFieldTypeChanged = "request_field_type_changed"
	IssueRequestFieldExtra       = "request_field_extra"
)

// Validate walks every contract ID in the registry and emits an issue
// list describing provider/consumer drift.
//
// Algorithm:
//   1. Group contracts by canonical ID.
//   2. If only one role is present → orphan issue.
//   3. For matched pairs, fetch shapes via lookup.
//   4. Diff response (provider -> consumer reads) and request
//      (consumer sends -> provider accepts) separately. Breaking-ness
//      is decided by direction: a field the reader relies on being
//      missing is always breaking; an extra field the writer sends
//      that the reader ignores is cosmetic.
//   5. Issues are sorted by severity (breaking first) then contract ID
//      for a stable output across calls.
func Validate(reg *Registry, lookup ShapeLookup) []ContractIssue {
	if lookup == nil {
		lookup = func(string) *Shape { return nil }
	}
	// Group by canonical contract ID.
	ids := reg.AllIDs()
	var out []ContractIssue
	for _, id := range ids {
		items := reg.ByID(id)
		if len(items) == 0 {
			continue
		}
		var providers, consumers []Contract
		for _, c := range items {
			switch c.Role {
			case RoleProvider:
				providers = append(providers, c)
			case RoleConsumer:
				consumers = append(consumers, c)
			}
		}
		// Orphan cases — easy.
		if len(providers) == 0 {
			for _, c := range consumers {
				out = append(out, ContractIssue{
					ContractID: id,
					Kind:       IssueOrphanConsumer,
					Severity:   SeverityWarning,
					Consumer:   c.RepoPrefix,
					Details:    "consumer calls a contract no indexed provider implements",
				})
			}
			continue
		}
		if len(consumers) == 0 {
			for _, c := range providers {
				out = append(out, ContractIssue{
					ContractID: id,
					Kind:       IssueOrphanProvider,
					Severity:   SeverityInfo,
					Provider:   c.RepoPrefix,
					Details:    "provider exposes a contract no indexed consumer calls",
				})
			}
			continue
		}

		// Pair every provider with every consumer. Usually n=1 on
		// each side; the loop handles multi-provider or multi-repo
		// fan-out correctly.
		for _, p := range providers {
			for _, cons := range consumers {
				out = append(out, diffPair(id, p, cons, lookup)...)
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return severityRank(out[i].Severity) < severityRank(out[j].Severity)
		}
		if out[i].ContractID != out[j].ContractID {
			return out[i].ContractID < out[j].ContractID
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// diffPair produces issues for one provider↔consumer pair. Returns an
// empty slice when everything lines up — zero-issue contracts mean
// the pair matches cleanly.
func diffPair(id string, p, cons Contract, lookup ShapeLookup) []ContractIssue {
	var issues []ContractIssue

	provReqID := metaStringContract(p.Meta, "request_type")
	provRespID := metaStringContract(p.Meta, "response_type")
	consReqID := metaStringContract(cons.Meta, "request_type")
	consRespID := metaStringContract(cons.Meta, "response_type")

	// Response direction: provider produces, consumer consumes.
	// Every field the consumer expects to read must be in the
	// provider's response. Extras on the provider are informational.
	if provRespID != "" && consRespID != "" {
		provShape := resolveShape(provRespID, lookup)
		consShape := resolveShape(consRespID, lookup)
		if provShape == nil && isSymbolID(provRespID) {
			issues = append(issues, ContractIssue{
				ContractID: id, Kind: IssueResponseTypeUnknown, Severity: SeverityWarning,
				Provider: p.RepoPrefix, ProviderType: provRespID,
				Details: "provider response type has no indexed shape — cross-repo diff skipped",
			})
		}
		if consShape == nil && isSymbolID(consRespID) {
			issues = append(issues, ContractIssue{
				ContractID: id, Kind: IssueResponseTypeUnknown, Severity: SeverityWarning,
				Consumer: cons.RepoPrefix, ConsumerType: consRespID,
				Details: "consumer response type has no indexed shape — cross-repo diff skipped",
			})
		}
		if provShape != nil && consShape != nil {
			issues = append(issues, diffResponseFields(id, p, cons, provShape, consShape, provRespID, consRespID)...)
		}
	}

	// Request direction: consumer sends, provider accepts. A required
	// field on the provider that the consumer doesn't send is a hard
	// break. A field the consumer sends that the provider doesn't
	// know about is ignored at runtime — information only.
	if provReqID != "" && consReqID != "" {
		provShape := resolveShape(provReqID, lookup)
		consShape := resolveShape(consReqID, lookup)
		if provShape == nil && isSymbolID(provReqID) {
			issues = append(issues, ContractIssue{
				ContractID: id, Kind: IssueRequestTypeUnknown, Severity: SeverityWarning,
				Provider: p.RepoPrefix, ProviderType: provReqID,
				Details: "provider request type has no indexed shape — cross-repo diff skipped",
			})
		}
		if consShape == nil && isSymbolID(consReqID) {
			issues = append(issues, ContractIssue{
				ContractID: id, Kind: IssueRequestTypeUnknown, Severity: SeverityWarning,
				Consumer: cons.RepoPrefix, ConsumerType: consReqID,
				Details: "consumer request type has no indexed shape — cross-repo diff skipped",
			})
		}
		if provShape != nil && consShape != nil {
			issues = append(issues, diffRequestFields(id, p, cons, provShape, consShape, provReqID, consReqID)...)
		}
	}

	return issues
}

// diffResponseFields compares what the provider produces against what
// the consumer declares it will read. The consumer is the authority
// on what's "expected" — its field list is the set of things the code
// destructures / decodes from the response.
func diffResponseFields(id string, p, cons Contract, provShape, consShape *Shape, provType, consType string) []ContractIssue {
	var issues []ContractIssue
	provFields := indexFields(provShape)
	consFields := indexFields(consShape)

	// Consumer reads field that provider no longer emits → breaking.
	for name, cf := range consFields {
		pf, ok := provFields[name]
		if !ok {
			sev := SeverityBreaking
			if !cf.Required {
				// Consumer reads optionally; downgrade to warning —
				// code might handle the missing field gracefully.
				sev = SeverityWarning
			}
			issues = append(issues, ContractIssue{
				ContractID: id, Kind: IssueResponseFieldRemoved, Severity: sev,
				Provider: p.RepoPrefix, Consumer: cons.RepoPrefix,
				Field: name, ProviderType: provType, ConsumerType: consType,
				Details: "consumer reads response field not present on provider",
			})
			continue
		}
		// Response direction: provider writes, consumer reads. Safe
		// iff the consumer's declared type can hold every value the
		// provider might emit — i.e. canAssign(consumerType, providerType).
		if !canAssign(cf.Type, pf.Type) {
			issues = append(issues, ContractIssue{
				ContractID: id, Kind: IssueResponseFieldTypeChanged, Severity: SeverityBreaking,
				Provider: p.RepoPrefix, Consumer: cons.RepoPrefix,
				Field: name, ProviderType: provType, ConsumerType: consType,
				Details: fmt.Sprintf("provider=%q consumer=%q", pf.Type, cf.Type),
			})
		}
	}

	// Provider emits field consumer doesn't read → informational.
	for name := range provFields {
		if _, ok := consFields[name]; !ok {
			issues = append(issues, ContractIssue{
				ContractID: id, Kind: IssueResponseFieldAdded, Severity: SeverityInfo,
				Provider: p.RepoPrefix, Consumer: cons.RepoPrefix,
				Field: name, ProviderType: provType, ConsumerType: consType,
				Details: "provider emits response field no indexed consumer reads",
			})
		}
	}

	return issues
}

// diffRequestFields compares what the provider accepts against what
// the consumer sends. The provider is the authority — its required
// fields must be present on every request.
func diffRequestFields(id string, p, cons Contract, provShape, consShape *Shape, provType, consType string) []ContractIssue {
	var issues []ContractIssue
	provFields := indexFields(provShape)
	consFields := indexFields(consShape)

	// Required provider field not in consumer payload → breaking.
	for name, pf := range provFields {
		cf, ok := consFields[name]
		if !ok {
			if pf.Required {
				issues = append(issues, ContractIssue{
					ContractID: id, Kind: IssueRequestFieldRequired, Severity: SeverityBreaking,
					Provider: p.RepoPrefix, Consumer: cons.RepoPrefix,
					Field: name, ProviderType: provType, ConsumerType: consType,
					Details: "provider requires field consumer doesn't send",
				})
			}
			continue
		}
		// Request direction: consumer writes, provider reads. Safe
		// iff the provider's declared type can hold every value the
		// consumer might send — canAssign(providerType, consumerType).
		if !canAssign(pf.Type, cf.Type) {
			issues = append(issues, ContractIssue{
				ContractID: id, Kind: IssueRequestFieldTypeChanged, Severity: SeverityBreaking,
				Provider: p.RepoPrefix, Consumer: cons.RepoPrefix,
				Field: name, ProviderType: provType, ConsumerType: consType,
				Details: fmt.Sprintf("provider=%q consumer=%q", pf.Type, cf.Type),
			})
		}
	}

	// Consumer sends field provider doesn't accept → info. Server
	// frameworks ignore unknown fields by default; still useful to
	// surface so the consumer can trim stale payload noise.
	for name := range consFields {
		if _, ok := provFields[name]; !ok {
			issues = append(issues, ContractIssue{
				ContractID: id, Kind: IssueRequestFieldExtra, Severity: SeverityInfo,
				Provider: p.RepoPrefix, Consumer: cons.RepoPrefix,
				Field: name, ProviderType: provType, ConsumerType: consType,
				Details: "consumer sends request field provider doesn't declare",
			})
		}
	}

	return issues
}

// indexFields keys a shape's field list by name (wire name) for O(1)
// lookup during diffing.
func indexFields(s *Shape) map[string]ShapeField {
	out := make(map[string]ShapeField, len(s.Fields))
	for _, f := range s.Fields {
		out[f.Name] = f
	}
	return out
}

// resolveShape returns the shape for a symbol ID. Bare names (no `::`)
// aren't in the graph and have no shape — we return nil for them
// rather than calling the lookup, since a bare name is by definition
// a type we couldn't index.
func resolveShape(typeRef string, lookup ShapeLookup) *Shape {
	if !isSymbolID(typeRef) {
		return nil
	}
	return lookup(typeRef)
}

func isSymbolID(s string) bool {
	return strings.Contains(s, "::")
}

// canAssign reports whether a value of type `from` can be safely held
// in a slot declared as type `to`. It's the direction-aware answer
// to "is this a breaking type change":
//
//   - Response direction (provider → consumer): canAssign(consumerType, providerType).
//   - Request  direction (consumer → provider): canAssign(providerType, consumerType).
//
// The core rules:
//
//   - Same canonical form (including cross-language primitive aliases) → compatible.
//     `string` ≡ `String` ≡ `str`; `bool` ≡ `Boolean`; `int32` ≡ `Int32` etc.
//   - Numeric widening is allowed: int → float (JSON numbers coerce up, no
//     precision loss for reasonable ranges). Reverse is breaking.
//   - `any` / `interface{}` / `object` / `Dynamic` can hold anything. A slot
//     typed `any` is always assignable-from anything. Conversely, a slot
//     typed `User` is NOT assignable-from `any` — you'd be trusting data
//     to match a stricter type you can't verify.
//   - User-defined types: checked by canonical name equality only. Deeper
//     structural comparison happens in the Stage 2 shape diff, not here.
func canAssign(to, from string) bool {
	toCanon := canonicalType(to)
	fromCanon := canonicalType(from)
	if toCanon == fromCanon {
		return true
	}
	toCat := primitiveCategory(toCanon)
	fromCat := primitiveCategory(fromCanon)
	// `any` / `interface{}` / `Dynamic` slots hold any value —
	// including user-defined types. Check before the
	// user-defined-type short-circuit below.
	if toCat == "any" {
		return true
	}
	if fromCat == "any" {
		// Assigning `any` into a stricter slot requires a runtime
		// check the wire doesn't enforce → breaking.
		return false
	}
	if toCat == "" || fromCat == "" {
		// At least one side is a user-defined type and neither was
		// `any`. Only exact canonical-name equality (already checked
		// above) qualifies as compatible — we don't do structural
		// matching here; that's the shape-diff's job.
		return false
	}
	if toCat == fromCat {
		return true
	}
	// Numeric widening: int fits in float (JSON numbers coerce).
	if toCat == "float" && fromCat == "int" {
		return true
	}
	// UUIDs are serialised as strings. A string slot can hold any
	// UUID verbatim — no decode step would refuse it. The reverse
	// is not safe: an arbitrary string isn't necessarily a UUID.
	if toCat == "str" && fromCat == "uuid" {
		return true
	}
	// Bytes round-trip as base64 strings on the wire; a str slot
	// can receive them unchanged. `str → bytes` isn't safe because
	// arbitrary text isn't guaranteed base64.
	if toCat == "str" && fromCat == "bytes" {
		return true
	}
	return false
}

// primitiveCategory collapses language-specific type aliases onto a
// single wire-format category. Returns "" for anything unrecognised
// (user types, collections, unknowns — handled by exact-name equality
// only).
//
// Categories tracked:
//
//   str        - string types across languages
//   bool       - boolean
//   int        - integer (all widths / sign combinations)
//   float      - floating-point + high-precision decimal
//   any        - opaque JSON-value / universal containers
//   time       - instant-in-time (serialises as ISO 8601 on the wire)
//   duration   - time intervals
//   bytes      - binary payloads (serialise as base64 strings)
//   uuid       - UUID types (serialise as hex-dashed strings)
//
// The time/duration/bytes/uuid categories are chosen to match what
// the wire actually carries: two fields that serialise to the same
// JSON form under default codecs should be compatible, even when
// the static types don't match syntactically. `time.Time` ↔
// `DateTime` ↔ `Instant` all marshal to ISO 8601 strings, so a
// consumer in any of the three can decode the provider's output.
func primitiveCategory(t string) string {
	switch t {
	// ---- primitives ----
	case "string", "String", "str", "text", "Text":
		return "str"
	case "bool", "Bool", "boolean", "Boolean":
		return "bool"
	case "int", "Int", "Integer", "Long", "long", "Short", "short",
		"Byte", "byte",
		"int8", "int16", "int32", "int64",
		"Int8", "Int16", "Int32", "Int64",
		"uint", "uint8", "uint16", "uint32", "uint64",
		"i8", "i16", "i32", "i64", "u8", "u16", "u32", "u64",
		"BigInt", "bigint":
		return "int"
	case "float", "Float", "double", "Double", "Number", "number",
		"float32", "float64", "f32", "f64", "Decimal", "decimal",
		"BigDecimal":
		return "float"

	// ---- opaque JSON value ----
	case "any", "Any", "object", "Object", "Dynamic", "dynamic",
		"interface{}", "JSON", "json", "Value", "JsonNode",
		"RawMessage":
		return "any"

	// ---- timestamps (all encode as ISO 8601 strings by default) ----
	//
	// `Time` is the canonicalised form of Go's `time.Time` (package
	// prefix stripped upstream). Same goes for Dart's `DateTime`,
	// Java's `Instant` / `LocalDateTime` / `ZonedDateTime` /
	// `OffsetDateTime` / `Date` (java.util.Date), Python's
	// `datetime`, Rust's `DateTime` / `NaiveDateTime`, Kotlin's
	// `Instant`. We fold `LocalDate` / `date` in too — the lossy
	// "just a date" variant of instants — because the wire form is
	// also a quoted string and real-world APIs use them
	// interchangeably. Precision-loss flags belong in a separate
	// warning category, not here.
	case "Time", "DateTime", "Instant",
		"LocalDateTime", "ZonedDateTime", "OffsetDateTime",
		"NaiveDateTime", "Date", "LocalDate",
		"datetime", "date", "Timestamp", "timestamp":
		return "time"

	// ---- durations ----
	case "Duration", "duration", "timedelta", "Period":
		return "duration"

	// ---- binary ----
	//
	// `[]byte` is stripped to `byte` by the collection unwrap, which
	// naturally lands on the `int` category (Go `byte` == uint8).
	// That would be wrong for wire compat: `[]byte` marshals as a
	// base64 string, not an array of numbers. `canonicalType` passes
	// `[]byte` through untouched when the stripped form matches a
	// registered binary alias (see canonicalType — we keep the full
	// form there). Rust `Vec<u8>` and Java `byte[]` collapse to
	// their aliases here.
	case "[]byte", "bytes", "Bytes", "ByteString",
		"Uint8List", "Vec<u8>", "byte[]":
		return "bytes"

	// ---- UUIDs — hex-dashed strings on the wire. Cross-assignable
	// with `str` in one direction only (see canAssign).
	case "UUID", "Uuid", "Guid", "GUID":
		return "uuid"
	}
	return ""
}

func canonicalType(t string) string {
	t = strings.TrimSpace(t)
	t = strings.TrimPrefix(t, "*")
	t = strings.TrimSuffix(t, "?")
	t = strings.ReplaceAll(t, " ", "")
	// Trim trailing optional / nullable union members.
	for _, suffix := range []string{"|null", "|None", "|undefined"} {
		t = strings.TrimSuffix(t, suffix)
	}
	// Byte-slice aliases round-trip as base64 strings on the wire,
	// not as arrays of numbers. Collapse to the `bytes` alias *before*
	// the generic collection unwrap would turn `[]byte` into `byte`
	// (which would miscategorise as int).
	switch t {
	case "[]byte", "byte[]", "Vec<u8>", "Uint8List":
		return "bytes"
	}
	// Unwrap the most common container types so `List<User>` compares
	// to `[]User`. This is loose on purpose — Stage 3 only signals at
	// the top-level type identity, not collection cardinality
	// (cardinality is already flagged via the `repeated` ShapeField
	// flag, which the caller can inspect separately).
	for {
		changed := false
		for _, pfx := range []string{"Array<", "List<", "Set<", "Collection<", "Optional<"} {
			if strings.HasPrefix(t, pfx) && strings.HasSuffix(t, ">") {
				t = t[len(pfx) : len(t)-1]
				changed = true
				break
			}
		}
		// Trailing `Foo[]` (TypeScript / Java).
		if strings.HasSuffix(t, "[]") {
			t = t[:len(t)-2]
			changed = true
		}
		// Leading `[]Foo` (Go slice).
		if strings.HasPrefix(t, "[]") {
			t = t[2:]
			changed = true
		}
		if !changed {
			break
		}
	}
	// Drop package qualifier: "pkg.User" → "User", `time.Time` →
	// `Time`. The primitive-category table keys on the stripped
	// form, so `time.Time` lands on the "time" category via "Time".
	if idx := strings.LastIndex(t, "."); idx >= 0 {
		t = t[idx+1:]
	}
	return t
}

func severityRank(s Severity) int {
	switch s {
	case SeverityBreaking:
		return 0
	case SeverityWarning:
		return 1
	case SeverityInfo:
		return 2
	}
	return 3
}

// metaStringContract keeps the contract-layer reader independent of
// the server-side helper of the same shape.
func metaStringContract(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}
