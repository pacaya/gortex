package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestCobolExtractor_Program(t *testing.T) {
	src := []byte(`       IDENTIFICATION DIVISION.
       PROGRAM-ID. HELLO-WORLD.
       DATA DIVISION.
       WORKING-STORAGE SECTION.
       COPY COMMONLIB.
       PROCEDURE DIVISION.
       MAIN SECTION.
           CALL 'GREET' USING NAME.
           STOP RUN.
`)
	e := NewCobolExtractor()
	require.Equal(t, "cobol", e.Language())

	res, err := e.Extract("HELLO.cob", src)
	require.NoError(t, err)

	var gotProg, gotDiv, gotSection bool
	for _, n := range res.Nodes {
		switch n.Name {
		case "HELLO-WORLD":
			gotProg = true
		case "PROCEDURE-DIVISION":
			gotDiv = true
		case "MAIN-SECTION", "WORKING-STORAGE-SECTION":
			gotSection = true
		}
	}
	var gotCall, gotCopy bool
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeCalls && ed.To == "unresolved::GREET" {
			gotCall = true
		}
		if ed.Kind == graph.EdgeImports && ed.To == "unresolved::import::COMMONLIB" {
			gotCopy = true
		}
	}
	assert.True(t, gotProg)
	assert.True(t, gotDiv)
	assert.True(t, gotSection)
	assert.True(t, gotCall)
	assert.True(t, gotCopy)
}

func TestCobolExtractor_Paragraphs(t *testing.T) {
	// Paragraph labels sit in area A (column 8, i.e. 7 leading spaces in
	// fixed format); statements sit in area B (column 12+). MAIN-PARA
	// PERFORMs SECOND-PARA, which is defined later in the file.
	src := []byte(`       IDENTIFICATION DIVISION.
       PROGRAM-ID. PARADEMO.
       PROCEDURE DIVISION.
       MAIN-PARA.
           DISPLAY 'START'.
           PERFORM SECOND-PARA.
           GO TO EXIT-PARA.
       SECOND-PARA.
           DISPLAY 'SECOND'.
       EXIT-PARA.
           STOP RUN.
`)
	e := NewCobolExtractor()
	res, err := e.Extract("PARA.cob", src)
	require.NoError(t, err)

	paras := map[string]bool{}
	for _, n := range res.Nodes {
		if n.Kind == graph.KindFunction && n.Meta != nil && n.Meta["cobol_kind"] == "paragraph" {
			paras[n.Name] = true
		}
	}
	assert.True(t, paras["MAIN-PARA"], "MAIN-PARA paragraph node")
	assert.True(t, paras["SECOND-PARA"], "SECOND-PARA paragraph node")
	assert.True(t, paras["EXIT-PARA"], "EXIT-PARA paragraph node")
	// PROGRAM-ID is also KindFunction but must NOT be tagged a paragraph.
	for _, n := range res.Nodes {
		if n.Name == "PARADEMO" {
			assert.True(t, n.Meta == nil || n.Meta["cobol_kind"] != "paragraph",
				"PROGRAM-ID must not be a paragraph")
		}
	}

	var performEdge, goToEdge bool
	for _, ed := range res.Edges {
		if ed.Kind != graph.EdgeCalls {
			continue
		}
		if ed.From == "PARA.cob::MAIN-PARA" && ed.To == "PARA.cob::SECOND-PARA" {
			performEdge = true
		}
		if ed.From == "PARA.cob::MAIN-PARA" && ed.To == "PARA.cob::EXIT-PARA" {
			goToEdge = true
		}
	}
	assert.True(t, performEdge, "EdgeCalls MAIN-PARA -> SECOND-PARA (PERFORM)")
	assert.True(t, goToEdge, "EdgeCalls MAIN-PARA -> EXIT-PARA (GO TO)")
}

func TestCobolExtractor_PerformThru(t *testing.T) {
	src := []byte(`       PROGRAM-ID. THRUDEMO.
       PROCEDURE DIVISION.
       DRIVER.
           PERFORM STEP-1 THRU STEP-2.
       STEP-1.
           DISPLAY 'A'.
       STEP-2.
           DISPLAY 'B'.
`)
	res, err := NewCobolExtractor().Extract("THRU.cob", src)
	require.NoError(t, err)
	var thru1, thru2 bool
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeCalls && ed.From == "THRU.cob::DRIVER" {
			if ed.To == "THRU.cob::STEP-1" {
				thru1 = true
			}
			if ed.To == "THRU.cob::STEP-2" {
				thru2 = true
			}
		}
	}
	assert.True(t, thru1, "PERFORM ... THRU emits edge to first paragraph")
	assert.True(t, thru2, "PERFORM ... THRU emits edge to range-end paragraph")
}

func TestCobolExtractor_EmptyInput(t *testing.T) {
	res, err := NewCobolExtractor().Extract("e.cbl", []byte(""))
	require.NoError(t, err)
	require.Len(t, res.Nodes, 1)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
}
