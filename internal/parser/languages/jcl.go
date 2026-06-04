package languages

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// JCL (MVS/z-OS Job Control Language) extraction is line-based: every
// statement starts in column 1 with `//`. We model the job stream as a
// small call graph:
//
//	//JOBNAME  JOB  ...            -> KindFunction (the job)
//	//STEPNAME EXEC PGM=PROGRAM    -> KindMethod   (a job step) + EdgeCalls
//	//STEPNAME EXEC procname       -> KindMethod   (cataloged-proc step) + EdgeCalls
//	//DDNAME   DD   DSN=dataset    -> KindVariable (a DD) + EdgeReferences
//
// Continuation lines (operand field ending in a comma, next line `//` plus
// spaces) are folded into the owning statement on a best-effort basis so a
// PGM= or DSN= that spills to the next line is still captured.
var (
	// A JCL name-bearing statement: `//NAME OP rest`. NAME is the label,
	// OP is the operation (JOB / EXEC / DD / etc).
	jclStmtRe = regexp.MustCompile(`^//([A-Za-z$#@][A-Za-z0-9$#@]*)\s+(\S+)\s*(.*)$`)
	// PGM=program operand on an EXEC statement.
	jclPgmRe = regexp.MustCompile(`(?i)\bPGM=([A-Za-z$#@][A-Za-z0-9$#@.]*)`)
	// PROC=procname operand (explicit form of EXEC procname).
	jclProcRe = regexp.MustCompile(`(?i)\bPROC=([A-Za-z$#@][A-Za-z0-9$#@.]*)`)
	// DSN= / DSNAME= dataset operand on a DD statement.
	jclDSNRe = regexp.MustCompile(`(?i)\bDSN(?:AME)?=([A-Za-z0-9$#@.()'+\-]+)`)
)

// JCLExtractor extracts MVS/z-OS Job Control Language job streams.
type JCLExtractor struct{}

func NewJCLExtractor() *JCLExtractor { return &JCLExtractor{} }

func (e *JCLExtractor) Language() string     { return "jcl" }
func (e *JCLExtractor) Extensions() []string { return []string{".jcl", ".job"} }

func (e *JCLExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	rawLines := strings.Split(string(src), "\n")
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: len(rawLines),
		Language: "jcl",
	}
	result.Nodes = append(result.Nodes, fileNode)

	stmts := foldJCLStatements(rawLines)

	seen := make(map[string]bool)
	addNode := func(id string, kind graph.NodeKind, name string, line int, meta map[string]any) {
		if seen[id] {
			return
		}
		seen[id] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: kind, Name: name,
			FilePath: filePath, StartLine: line, EndLine: line,
			Language: "jcl", Meta: meta,
		})
	}
	addEdge := func(from, to string, kind graph.EdgeKind, line int) {
		result.Edges = append(result.Edges, &graph.Edge{
			From: from, To: to, Kind: kind, FilePath: filePath, Line: line,
		})
	}

	jobID := ""       // the most recent JOB node ID (parent for steps)
	curStepID := ""   // the most recent EXEC step node ID (parent for DDs)
	curStepName := "" // step name, used to namespace DD node IDs

	for _, st := range stmts {
		m := jclStmtRe.FindStringSubmatch(st.text)
		if m == nil {
			continue
		}
		name, op, rest := m[1], strings.ToUpper(m[2]), m[3]
		switch op {
		case "JOB":
			id := filePath + "::" + name
			addNode(id, graph.KindFunction, name, st.line,
				map[string]any{"jcl_kind": "job"})
			addEdge(fileNode.ID, id, graph.EdgeDefines, st.line)
			jobID = id
			curStepID = ""
			curStepName = ""

		case "EXEC":
			id := filePath + "::" + name
			meta := map[string]any{"jcl_kind": "step"}
			parent := fileNode.ID
			if jobID != "" {
				parent = jobID
			}
			if pm := jclPgmRe.FindStringSubmatch(rest); pm != nil {
				pgm := pm[1]
				meta["pgm"] = pgm
				addNode(id, graph.KindMethod, name, st.line, meta)
				addEdge(parent, id, graph.EdgeDefines, st.line)
				addEdge(id, "unresolved::program::"+pgm, graph.EdgeCalls, st.line)
			} else {
				// EXEC procname (cataloged procedure) — either bare
				// (first operand token) or the explicit PROC= form.
				proc := ""
				if pm := jclProcRe.FindStringSubmatch(rest); pm != nil {
					proc = pm[1]
				} else {
					proc = firstOperandToken(rest)
				}
				if proc != "" {
					meta["proc"] = proc
				}
				addNode(id, graph.KindMethod, name, st.line, meta)
				addEdge(parent, id, graph.EdgeDefines, st.line)
				if proc != "" {
					addEdge(id, "unresolved::proc::"+proc, graph.EdgeCalls, st.line)
				}
			}
			curStepID = id
			curStepName = name

		case "DD":
			// DD node id is namespaced by the enclosing step when known,
			// so duplicate DD names across steps don't collide.
			ddID := filePath + "::DD:" + name
			if curStepName != "" {
				ddID = filePath + "::" + curStepName + "." + name
			}
			meta := map[string]any{"jcl_kind": "dd"}
			dsn := ""
			if dm := jclDSNRe.FindStringSubmatch(rest); dm != nil {
				dsn = normalizeDSN(dm[1])
			}
			if dsn != "" {
				meta["dsn"] = dsn
			}
			addNode(ddID, graph.KindVariable, name, st.line, meta)
			parent := fileNode.ID
			if curStepID != "" {
				parent = curStepID
			}
			addEdge(parent, ddID, graph.EdgeDefines, st.line)
			// DUMMY / SYSOUT-only DDs carry no dataset — emit no
			// dataset reference for them.
			if dsn != "" {
				addEdge(parent, "unresolved::dataset::"+dsn, graph.EdgeReferences, st.line)
			}
		}
	}

	return result, nil
}

// jclStatement is a logical JCL statement after continuation folding.
type jclStatement struct {
	text string // folded statement text (leading `//` + operands)
	line int    // 1-based line of the statement's first physical line
}

// foldJCLStatements collapses continuation lines into single logical
// statements. A statement continues when its operand field ends in a
// comma and the following line is `//` followed by whitespace (the
// continuation marker). Comment lines (`//*`) and the in-stream data
// delimiter (`/*`, `//`) are dropped. Best-effort: it folds the operand
// of the next continuation line onto the current statement so a PGM= /
// DSN= spilling to the next line is captured.
func foldJCLStatements(lines []string) []jclStatement {
	var out []jclStatement
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		// Comment line: `//*...`
		if strings.HasPrefix(line, "//*") {
			continue
		}
		// In-stream data delimiter `/*` or null statement `//`.
		if line == "//" || strings.HasPrefix(line, "/*") {
			continue
		}
		if !strings.HasPrefix(line, "//") {
			continue // in-stream data or sequence noise
		}
		// A continuation marker (`//` + spaces) only makes sense after a
		// statement; a leading one with no owner is skipped.
		afterSlashes := line[2:]
		if strings.TrimSpace(afterSlashes) == "" {
			continue
		}
		if afterSlashes[0] == ' ' || afterSlashes[0] == '\t' {
			// Orphan continuation (no preceding statement) — ignore.
			continue
		}

		startLine := i + 1
		// Drop the inline comment that JCL allows after a blank past the
		// operand field. We keep it simple: strip everything after the
		// first run of spaces that follows a space-terminated operand.
		text := stripJCLComment(line)
		// Fold continuation lines.
		for endsWithContinuation(text) && i+1 < len(lines) {
			next := strings.TrimRight(lines[i+1], "\r")
			if !strings.HasPrefix(next, "//") {
				break
			}
			cont := next[2:]
			if cont == "" || (cont[0] != ' ' && cont[0] != '\t') {
				break // next line is a new statement, not a continuation
			}
			operand := strings.TrimSpace(cont)
			// Comment-only continuation.
			text = strings.TrimRight(text, " ") + stripJCLComment(operand)
			i++
		}
		out = append(out, jclStatement{text: text, line: startLine})
	}
	return out
}

// endsWithContinuation reports whether a JCL operand field continues on
// the next line — true when the (comment-stripped) text ends in a comma.
func endsWithContinuation(text string) bool {
	t := strings.TrimRight(text, " \t")
	return strings.HasSuffix(t, ",")
}

// stripJCLComment removes a trailing free-form comment from a JCL line.
// JCL comments begin after the operand field at the first space that is
// not inside quotes or parentheses. This is best-effort and conservative:
// it only strips when a space is found outside quotes/parens past the
// operation field.
func stripJCLComment(line string) string {
	inQuote := false
	depth := 0
	// Locate the start of the operand field (the 3rd whitespace-delimited
	// token: after the label and the operation). The comment, if any,
	// begins at the first unquoted/unparenthesised space *within* the
	// operand field, so we must not start scanning before it.
	tokens := 0
	prevSpace := true
	operandStart := -1
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == ' ' || c == '\t' {
			prevSpace = true
		} else {
			if prevSpace {
				tokens++
				if tokens == 3 {
					operandStart = i
					break
				}
			}
			prevSpace = false
		}
	}
	if operandStart < 0 {
		return line // fewer than three tokens — no operand field to scan
	}
	for i := operandStart; i < len(line); i++ {
		c := line[i]
		switch c {
		case '\'':
			inQuote = !inQuote
		case '(':
			if !inQuote {
				depth++
			}
		case ')':
			if !inQuote {
				depth--
			}
		case ' ', '\t':
			if !inQuote && depth == 0 {
				return line[:i]
			}
		}
	}
	return line
}

// firstOperandToken returns the first comma-or-space-delimited operand
// token of a JCL operand field (used for `EXEC procname`).
func firstOperandToken(rest string) string {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return ""
	}
	end := len(rest)
	for i, c := range rest {
		if c == ',' || c == ' ' || c == '\t' || c == '(' {
			end = i
			break
		}
	}
	tok := rest[:end]
	// A bare token containing `=` is a keyword operand, not a proc name.
	if strings.Contains(tok, "=") {
		return ""
	}
	return tok
}

// normalizeDSN trims a DSN operand to the bare dataset name: it strips
// surrounding quotes and any member/GDG suffix in parentheses.
func normalizeDSN(dsn string) string {
	dsn = strings.Trim(dsn, "'")
	if idx := strings.IndexByte(dsn, '('); idx >= 0 {
		dsn = dsn[:idx]
	}
	return strings.TrimRight(dsn, ",")
}

var _ parser.Extractor = (*JCLExtractor)(nil)
