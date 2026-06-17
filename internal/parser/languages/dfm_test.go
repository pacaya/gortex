package languages

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestDFMExtractor(t *testing.T) {
	const dfm = `object MainForm: TMainForm
  Left = 0
  Caption = 'Main'
  object SaveButton: TButton
    Caption = 'Save'
    OnClick = SaveButtonClick
  end
  object NameEdit: TEdit
    Text = ''
  end
end
`
	res, err := NewDFMExtractor().Extract("MainForm.dfm", []byte(dfm))
	if err != nil {
		t.Fatal(err)
	}

	var form, btn *graph.Node
	refs := map[string]bool{}
	for _, n := range res.Nodes {
		switch n.Name {
		case "MainForm":
			form = n
		case "SaveButton":
			btn = n
		}
	}
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeReferences && strings.HasPrefix(e.To, "unresolved::") {
			refs[strings.TrimPrefix(e.To, "unresolved::")] = true
		}
	}

	if form == nil || form.Kind != graph.KindType {
		t.Fatalf("top-level form node missing or not a type: %+v", form)
	}
	if form.Meta["dfm_type"] != "TMainForm" {
		t.Errorf("form dfm_type = %v, want TMainForm", form.Meta["dfm_type"])
	}
	if btn == nil || btn.Kind != graph.KindField {
		t.Fatalf("nested control 'SaveButton' missing or not a field: %+v", btn)
	}

	// Component class references and the event-handler reference.
	for _, want := range []string{"TMainForm", "TButton", "TEdit", "SaveButtonClick"} {
		if !refs[want] {
			t.Errorf("missing reference %q (refs: %v)", want, refs)
		}
	}

	// SaveButton is a member of the form.
	var memberEdge bool
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeMemberOf && e.From == "MainForm.dfm::SaveButton" && e.To == "MainForm.dfm::MainForm" {
			memberEdge = true
		}
	}
	if !memberEdge {
		t.Error("SaveButton should be member_of MainForm")
	}
}
