package indexer

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestFirstNonErrorReturn_AgreesOnSameType covers the interface +
// implementation stack: `h.emailSources.Update(...)` resolves to a
// receiver name that matches BOTH the postgres store and the mock
// store. Since both implement the same interface method, their
// signatures agree on the return type — our resolver should use it
// rather than refusing to pick one.
//
// This test exercises `parseFirstNonErrorReturnType` on the two
// signatures we'd see in the codebase and confirms they produce the
// same type.
func TestFirstNonErrorReturn_AgreesOnSameType(t *testing.T) {
	prod := "func (s *PostgresEmailSourceStore) Update(ctx context.Context, id, ownerID string, p UpdateEmailSourceParams) (*EmailSource, error)"
	mock := "func (m *MockEmailSourceStore) Update(ctx context.Context, id, ownerID string, p UpdateEmailSourceParams) (*EmailSource, error)"
	if got := parseFirstNonErrorReturnType(prod); got != "*EmailSource" {
		t.Errorf("prod sig → %q, want *EmailSource", got)
	}
	if got := parseFirstNonErrorReturnType(mock); got != "*EmailSource" {
		t.Errorf("mock sig → %q, want *EmailSource", got)
	}
}

// TestReceiverMatchesHint pins the disambiguation logic that picks
// the right `Update` method when the call is `h.tucks.Update(...)`
// and many store types have an Update method. The hint `tucks` from
// the call chain has to match the receiver type's name.
func TestReceiverMatchesHint(t *testing.T) {
	cases := []struct {
		name   string
		nodeID string
		hint   string
		want   bool
	}{
		{"plural receiver matches plural hint", "api/store/tucks.go::TucksStore.Update", "tucks", true},
		{"singular stem matches plural hint", "api/store/tucks.go::TuckStore.Update", "tucks", true},
		{"plural stem matches singular hint", "api/store/tuck.go::Tucks.Update", "tuck", true},
		{"unrelated receiver rejected", "api/store/email_sources.go::EmailSources.Update", "tucks", false},
		{"substring match on postfix store", "api/store/postgres_tucks.go::PostgresTuckStore.Update", "tucks", true},
		{"pointer receiver handled", "api/store/tucks.go::*TucksStore.Update", "tucks", true},
		{"empty hint never filters out", "api/store/email_sources.go::EmailSources.Update", "", true},
		{"plain function no receiver", "api/util.go::Helper", "tucks", true},
	}
	for _, c := range cases {
		n := &graph.Node{ID: c.nodeID, Kind: graph.KindMethod}
		got := receiverMatchesHint(n, c.hint)
		if got != c.want {
			t.Errorf("receiverMatchesHint(%q, %q) = %v, want %v", c.nodeID, c.hint, got, c.want)
		}
	}
}

// parseFirstNonErrorReturnType is the kernel of the graph-aware call-
// return-type tracing post-pass. It reads a Go function signature as
// stored in a graph node's meta and picks out the first return type
// that isn't `error`. Failure here would let the post-pass mis-resolve
// every contract whose provider returns via a helper.
func TestParseFirstNonErrorReturnType(t *testing.T) {
	tests := []struct {
		name, sig, want string
	}{
		{
			name: "pointer + error",
			sig:  "func ((s *Store)) Get(ctx context.Context, id string) (*EmailSource, error)",
			want: "*EmailSource",
		},
		{
			name: "slice + error",
			sig:  "func (w *Workspaces) List(ctx context.Context) ([]Workspace, error)",
			want: "[]Workspace",
		},
		{
			name: "single bare return",
			sig:  "func build() Config",
			want: "Config",
		},
		{
			name: "single pointer return",
			sig:  "func next() *Opt",
			want: "*Opt",
		},
		{
			name: "named return value",
			sig:  "func lookup() (result *User, err error)",
			want: "*User",
		},
		{
			name: "only error",
			sig:  "func save(x Foo) error",
			want: "",
		},
		{
			name: "void",
			sig:  "func noop()",
			want: "",
		},
		{
			name: "package-qualified return",
			sig:  "func(ctx context.Context) (*pkg.Resp, error)",
			want: "*pkg.Resp",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseFirstNonErrorReturnType(tt.sig)
			if got != tt.want {
				t.Errorf("parseFirstNonErrorReturnType(%q) = %q, want %q",
					tt.sig, got, tt.want)
			}
		})
	}
}
