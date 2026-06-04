package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestJCLExtractor_JobStream(t *testing.T) {
	src := []byte(`//PAYROLL  JOB (ACCT),'WEEKLY RUN',CLASS=A,MSGCLASS=X
//*  A simple two-step job
//STEP01   EXEC PGM=IEFBR14
//SYSPRINT DD SYSOUT=*
//OUTFILE  DD DSN=PROD.PAYROLL.MASTER,DISP=SHR
//STEP02   EXEC MYPROC
//INFILE   DD DSN=PROD.PAYROLL.INPUT,DISP=SHR
`)
	e := NewJCLExtractor()
	require.Equal(t, "jcl", e.Language())
	assert.Contains(t, e.Extensions(), ".jcl")
	assert.Contains(t, e.Extensions(), ".job")

	res, err := e.Extract("PAYROLL.jcl", src)
	require.NoError(t, err)

	// Job node.
	var jobNode, step01Node, step02Node *graph.Node
	var dd *graph.Node
	for _, n := range res.Nodes {
		switch n.ID {
		case "PAYROLL.jcl::PAYROLL":
			jobNode = n
		case "PAYROLL.jcl::STEP01":
			step01Node = n
		case "PAYROLL.jcl::STEP02":
			step02Node = n
		case "PAYROLL.jcl::STEP01.OUTFILE":
			dd = n
		}
	}
	require.NotNil(t, jobNode, "JOB node")
	assert.Equal(t, graph.KindFunction, jobNode.Kind)
	assert.Equal(t, "job", jobNode.Meta["jcl_kind"])

	require.NotNil(t, step01Node, "EXEC PGM= step node")
	assert.Equal(t, graph.KindMethod, step01Node.Kind)
	assert.Equal(t, "step", step01Node.Meta["jcl_kind"])
	assert.Equal(t, "IEFBR14", step01Node.Meta["pgm"])

	require.NotNil(t, step02Node, "EXEC procname step node")
	assert.Equal(t, "MYPROC", step02Node.Meta["proc"])

	require.NotNil(t, dd, "DD node namespaced by step")
	assert.Equal(t, graph.KindVariable, dd.Kind)
	assert.Equal(t, "dd", dd.Meta["jcl_kind"])
	assert.Equal(t, "PROD.PAYROLL.MASTER", dd.Meta["dsn"])

	// Edges.
	var jobDefinesStep, callsPgm, callsProc, refsDataset bool
	for _, ed := range res.Edges {
		switch {
		case ed.Kind == graph.EdgeDefines && ed.From == "PAYROLL.jcl::PAYROLL" && ed.To == "PAYROLL.jcl::STEP01":
			jobDefinesStep = true
		case ed.Kind == graph.EdgeCalls && ed.From == "PAYROLL.jcl::STEP01" && ed.To == "unresolved::program::IEFBR14":
			callsPgm = true
		case ed.Kind == graph.EdgeCalls && ed.From == "PAYROLL.jcl::STEP02" && ed.To == "unresolved::proc::MYPROC":
			callsProc = true
		case ed.Kind == graph.EdgeReferences && ed.From == "PAYROLL.jcl::STEP01" && ed.To == "unresolved::dataset::PROD.PAYROLL.MASTER":
			refsDataset = true
		}
	}
	assert.True(t, jobDefinesStep, "EdgeDefines JOB -> STEP")
	assert.True(t, callsPgm, "EdgeCalls STEP01 -> unresolved::program::IEFBR14")
	assert.True(t, callsProc, "EdgeCalls STEP02 -> unresolved::proc::MYPROC")
	assert.True(t, refsDataset, "EdgeReferences STEP01 -> unresolved::dataset::PROD.PAYROLL.MASTER")

	// SYSOUT-only DD must NOT produce a dataset reference.
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeReferences {
			assert.NotContains(t, ed.To, "SYSOUT")
		}
	}
}

func TestJCLExtractor_Continuation(t *testing.T) {
	// PGM= and DSN= spill onto continuation lines (`//` + spaces, prior
	// operand ends with a comma).
	src := []byte(`//RUNJOB  JOB (ACCT),'X'
//STEPA    EXEC PGM=MYPROG,
//             REGION=4M
//DD1      DD DSN=MY.DATA.SET,
//             DISP=SHR
`)
	res, err := NewJCLExtractor().Extract("CONT.jcl", src)
	require.NoError(t, err)

	var foldedPgm, foldedDSN bool
	for _, n := range res.Nodes {
		if n.ID == "CONT.jcl::STEPA" && n.Meta["pgm"] == "MYPROG" {
			foldedPgm = true
		}
		if n.Meta != nil && n.Meta["dsn"] == "MY.DATA.SET" {
			foldedDSN = true
		}
	}
	assert.True(t, foldedPgm, "PGM= captured across folding")
	assert.True(t, foldedDSN, "DSN= captured across folding")
}

func TestJCLExtractor_EmptyInput(t *testing.T) {
	res, err := NewJCLExtractor().Extract("e.jcl", []byte(""))
	require.NoError(t, err)
	require.Len(t, res.Nodes, 1)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
}
