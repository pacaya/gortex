package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestGoConstants_PlainConstEmitsKindConstant(t *testing.T) {
	src := `package foo

const Pi = 3.14
const Greeting string = "hello"
`
	fix := runGoExtract(t, src)

	consts := fix.nodesByKind[graph.KindConstant]
	if len(consts) != 2 {
		t.Fatalf("expected 2 KindConstant, got %d: %+v", len(consts), consts)
	}
	got := map[string]bool{}
	for _, c := range consts {
		got[c.Name] = true
	}
	if !got["Pi"] || !got["Greeting"] {
		t.Errorf("missing constants: %v", got)
	}

	// No const should leak as KindVariable now.
	for _, v := range fix.nodesByKind[graph.KindVariable] {
		if v.Name == "Pi" || v.Name == "Greeting" {
			t.Errorf("const %q surfaced as KindVariable", v.Name)
		}
	}
}

func TestGoConstants_IotaBlockEmitsEnumMembers(t *testing.T) {
	src := `package foo

type Color int

const (
	Red Color = iota
	Green
	Blue
)
`
	fix := runGoExtract(t, src)

	enums := fix.nodesByKind[graph.KindEnumMember]
	if len(enums) != 3 {
		t.Fatalf("expected 3 KindEnumMember, got %d: %+v", len(enums), enums)
	}
	got := map[string]bool{}
	for _, e := range enums {
		got[e.Name] = true
	}
	if !got["Red"] || !got["Green"] || !got["Blue"] {
		t.Errorf("missing enum members: %v", got)
	}

	// None of the iota members should leak as KindConstant or
	// KindVariable.
	for _, c := range fix.nodesByKind[graph.KindConstant] {
		if c.Name == "Red" || c.Name == "Green" || c.Name == "Blue" {
			t.Errorf("iota member %q surfaced as KindConstant", c.Name)
		}
	}
}

func TestGoConstants_IotaWordBoundary(t *testing.T) {
	// `iota` appears as a substring of an unrelated identifier — must
	// NOT trigger enum classification.
	src := `package foo

const Notiota = "no enum here"
`
	fix := runGoExtract(t, src)

	if len(fix.nodesByKind[graph.KindEnumMember]) != 0 {
		t.Errorf("expected no enum members, got %+v", fix.nodesByKind[graph.KindEnumMember])
	}
	if len(fix.nodesByKind[graph.KindConstant]) != 1 {
		t.Errorf("expected 1 KindConstant, got %d", len(fix.nodesByKind[graph.KindConstant]))
	}
}

func TestGoConstants_BlankIdentifierSkipped(t *testing.T) {
	src := `package foo

const _ = "skipped"
`
	fix := runGoExtract(t, src)
	if len(fix.nodesByKind[graph.KindConstant])+len(fix.nodesByKind[graph.KindEnumMember]) != 0 {
		t.Errorf("blank identifier should not produce a const/enum node")
	}
}

func TestGoConstants_VarStaysAsVariable(t *testing.T) {
	src := `package foo

var Counter int
`
	fix := runGoExtract(t, src)
	if len(fix.nodesByKind[graph.KindVariable]) != 1 {
		t.Fatalf("expected 1 KindVariable for var decl, got %d", len(fix.nodesByKind[graph.KindVariable]))
	}
	if len(fix.nodesByKind[graph.KindConstant]) != 0 {
		t.Errorf("var should not produce KindConstant")
	}
}
