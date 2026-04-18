package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestWireContractFingerprint is the schema-stability guard for the
// daemon's snapshot wire format. It fingerprints the exported fields of
// every struct that gets gob-encoded into the snapshot and compares the
// hash against a checked-in golden value. Any change to a field name,
// type, or set-membership shifts the hash.
//
// When this test fails:
//
//   1. Additive change (new field only, existing fields untouched) —
//      update the golden value below. Old snapshots still load because
//      gob decodes unknown fields as zero.
//
//   2. Breaking change (rename, remove, retype an existing field) —
//      bump snapshotSchemaVersion in daemon_snapshot.go AND register a
//      migration in snapshotMigrations, THEN update the golden value.
//      Without a migration, deployed daemons reading an old snapshot
//      will discard the cache on upgrade and pay the full-reindex cost.
//
// This replaces the "CI fingerprint check on snapshot-relevant structs"
// item from spec-daemon-resilience.md Phase 4 — runs as part of the
// existing `go test ./...` sweep, no extra CI infrastructure required.
func TestWireContractFingerprint(t *testing.T) {
	cases := []struct {
		name string
		typ  reflect.Type
		want string
	}{
		{"graph.Node", reflect.TypeOf(graph.Node{}), ""},
		{"graph.Edge", reflect.TypeOf(graph.Edge{}), ""},
		{"snapshotHeader", reflect.TypeOf(snapshotHeader{}), ""},
		{"snapshotRepo", reflect.TypeOf(snapshotRepo{}), ""},
	}

	// Golden values computed by fingerprintType against the current
	// struct definitions. When a change to any of these structs is
	// intentional, update this map with the new values (run the test
	// once, copy the "got" hash from the failure message).
	golden := map[string]string{
		"graph.Node":     wireContractGolden("graph.Node"),
		"graph.Edge":     wireContractGolden("graph.Edge"),
		"snapshotHeader": wireContractGolden("snapshotHeader"),
		"snapshotRepo":   wireContractGolden("snapshotRepo"),
	}

	for i := range cases {
		cases[i].want = golden[cases[i].name]
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fingerprintType(c.typ)
			if got != c.want {
				t.Errorf(`wire contract for %s changed.
  got:  %s
  want: %s

If the change is ADDITIVE (new field, existing fields untouched):
  • Old snapshots still decode cleanly (gob reads unknown fields as zero).
  • Update the golden fingerprint in wireContractGolden().

If the change RENAMES / REMOVES / RETYPES an existing field:
  • Old snapshots will fail to decode cleanly on the changed field.
  • Bump snapshotSchemaVersion in daemon_snapshot.go.
  • Register a migration in snapshotMigrations for the old→new version.
  • Then update the golden fingerprint.

Field set: %s`, c.name, got, c.want, describeFields(c.typ))
			}
		})
	}
}

// fingerprintType returns a stable SHA-256 over the exported-field set
// of t. Field order is NOT part of the fingerprint (gob identifies
// fields by name), but name + type are. Nested structs are captured by
// their type.String() — if a nested type's own shape changes, callers
// with their own fingerprint will catch it.
func fingerprintType(t reflect.Type) string {
	if t.Kind() != reflect.Struct {
		return ""
	}
	fields := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		fields = append(fields, f.Name+":"+f.Type.String())
	}
	sort.Strings(fields)
	h := sha256.Sum256([]byte(strings.Join(fields, "|")))
	return hex.EncodeToString(h[:])
}

// describeFields renders the exported-field set for failure messages.
// Matches fingerprintType's canonical form so the output is a drop-in
// replacement for the hash during debugging.
func describeFields(t reflect.Type) string {
	if t.Kind() != reflect.Struct {
		return "(not a struct)"
	}
	fields := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		fields = append(fields, f.Name+":"+f.Type.String())
	}
	sort.Strings(fields)
	return strings.Join(fields, ", ")
}

// wireContractGolden holds the expected fingerprint for each wire
// type. Updated intentionally when a struct changes; see the doc on
// TestWireContractFingerprint for the decision tree (additive update vs
// schema bump + migration). Values are pinned hashes — NOT recomputed
// from the live type — so a field-level drift will surface here.
func wireContractGolden(name string) string {
	switch name {
	case "graph.Node":
		return "684dfbc0222c41d813cbc4991e2bde4cfa6a8e9767096367270f671e046581a1"
	case "graph.Edge":
		return "6a543f0f3f663587fc6011ab641bbd91fe66bfc70f87fe548974411e89e871de"
	case "snapshotHeader":
		return "56dd534d9405217f4619616a1ee23014eca01b931727455023b4b574b695f669"
	case "snapshotRepo":
		return "8a78544c6e8d6c384f95b971df43408e7bc5f5ab4c7f2052d038bd3ffa4e1311"
	default:
		panic(fmt.Sprintf("no golden fingerprint for %s", name))
	}
}
