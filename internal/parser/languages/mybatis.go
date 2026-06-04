package languages

import (
	"bytes"
	"encoding/xml"
	"io"
	"path"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// MyBatisExtractor indexes MyBatis mapper XML — the SQL-mapping layer of
// a Java/MyBatis application. A mapper file binds a Java DAO/Mapper
// interface (named by the `<mapper namespace="…">` FQCN) to a set of SQL
// statements, one per `<select>/<insert>/<update>/<delete>` element keyed
// by its `id`.
//
// What it surfaces:
//   - the file node, stamped `mybatis_namespace` (the bound interface FQCN);
//   - one node per statement element, with ID `<namespace>::<id>`
//     (e.g. namespace `com.app.UserMapper`, statement id `findUser` →
//     `com.app.UserMapper::findUser`). Each statement node is shaped like
//     a method (graph.KindMethod), carries the SQL kind
//     (select/insert/update/delete) in `mybatis_sql_kind` and the raw SQL
//     body in `mybatis_sql`, and emits an unresolved::-keyed EdgeCalls
//     placeholder back to the Java DAO method the synthesizer (see
//     internal/resolver/mybatis_calls.go::ResolveMyBatisCalls) lands.
//
// MyBatis mapper XML uses the plain `.xml` extension — shared with every
// other XML document — so the extractor is gated on the document content:
// IsMyBatisMapper recognises the `<mapper namespace=…>` root and/or the
// MyBatis DOCTYPE, and Extract returns just the file node (no statement
// nodes) for any XML that is not a mapper. The registry routes a `.xml`
// file here only when the content sniff matches, so ordinary XML is left
// to the generic XML extractor.
type MyBatisExtractor struct{}

// NewMyBatisExtractor constructs a MyBatisExtractor.
func NewMyBatisExtractor() *MyBatisExtractor { return &MyBatisExtractor{} }

func (e *MyBatisExtractor) Language() string     { return "mybatis" }
func (e *MyBatisExtractor) Extensions() []string { return []string{".xml"} }

// myBatisStatementElems is the set of mapper child elements that name an
// executable SQL statement. `<sql>` fragments and `<resultMap>` are
// deliberately excluded — they are reusable building blocks, not
// statements a DAO method invokes directly.
var myBatisStatementElems = map[string]bool{
	"select": true,
	"insert": true,
	"update": true,
	"delete": true,
}

// myBatisUnresolvedPrefix keys the placeholder EdgeCalls target the
// extractor emits from each statement node. The cross-file synthesizer
// (ResolveMyBatisCalls) rewrites it onto the matching Java DAO method.
const myBatisUnresolvedPrefix = "unresolved::mybatis::"

// IsMyBatisMapper reports whether src is a MyBatis mapper XML document.
// It accepts either the `<mapper namespace="…">` root element or the
// MyBatis DOCTYPE (`mybatis.org//DTD Mapper`). Only the document head is
// scanned, so the probe is cheap on large files.
func IsMyBatisMapper(src []byte) bool {
	head := src
	const headCap = 8 * 1024
	if len(head) > headCap {
		head = head[:headCap]
	}
	lower := bytes.ToLower(head)
	// DOCTYPE form: `<!DOCTYPE mapper PUBLIC "-//mybatis.org//DTD Mapper 3.0//EN" ...>`
	if bytes.Contains(lower, []byte("mybatis.org//dtd mapper")) {
		return true
	}
	// Root-element form: a `<mapper` start tag carrying a `namespace`
	// attribute. Tolerant of attribute ordering and whitespace.
	if i := bytes.Index(lower, []byte("<mapper")); i >= 0 {
		rest := lower[i:]
		if end := bytes.IndexByte(rest, '>'); end >= 0 {
			rest = rest[:end]
		}
		if bytes.Contains(rest, []byte("namespace")) {
			return true
		}
	}
	return false
}

// Extract parses a MyBatis mapper XML file. A document that is not a
// mapper (no `<mapper namespace>` root / MyBatis DOCTYPE) yields only the
// file node, so a misrouted plain-XML file degrades gracefully. A
// malformed mapper yields whatever was decoded before the error — never a
// hard failure.
func (e *MyBatisExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	result := &parser.ExtractionResult{}
	fileNode := &graph.Node{
		ID:       filePath,
		Kind:     graph.KindFile,
		Name:     path.Base(filePath),
		FilePath: filePath,
		Language: "mybatis",
	}
	result.Nodes = append(result.Nodes, fileNode)

	if !IsMyBatisMapper(src) {
		return result, nil
	}

	dec := xml.NewDecoder(bytes.NewReader(src))
	dec.Strict = false
	namespace := ""
	seenID := map[string]bool{}

	for {
		tok, err := dec.Token()
		if err == io.EOF || err != nil {
			break // EOF or malformed — keep what was decoded
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		local := strings.ToLower(se.Name.Local)

		if local == "mapper" {
			if ns := myBatisAttr(se, "namespace"); ns != "" {
				namespace = ns
				fileNode.Meta = map[string]any{"mybatis_namespace": ns}
			}
			continue
		}

		if !myBatisStatementElems[local] {
			continue
		}
		stmtID := myBatisAttr(se, "id")
		if stmtID == "" || namespace == "" || seenID[stmtID] {
			continue
		}
		seenID[stmtID] = true

		// The statement's SQL body is read from the source slice rather
		// than the decoder, so nested dynamic-SQL elements (<if>, <where>,
		// <foreach>, …) are flattened to their text content. The decoder
		// loop then walks those nested elements; none are statement elems,
		// so they are skipped by the filter above.
		startLine := 1 + bytes.Count(src[:clampOffset(dec.InputOffset(), len(src))], []byte{'\n'})
		sql := myBatisStatementSQL(src, startLine)

		stmtNodeID := namespace + "::" + stmtID
		stmt := &graph.Node{
			ID:        stmtNodeID,
			Kind:      graph.KindMethod,
			Name:      stmtID,
			QualName:  namespace + "." + stmtID,
			FilePath:  filePath,
			StartLine: startLine,
			EndLine:   startLine,
			Language:  "mybatis",
			Meta: map[string]any{
				"mybatis_namespace": namespace,
				"mybatis_statement": stmtID,
				"mybatis_sql_kind":  local,
				"mybatis_sql":       sql,
			},
		}
		result.Nodes = append(result.Nodes, stmt)

		// EdgeDefines from the file so the statement node is contained by
		// its mapper file (matches every other extractor's file→symbol
		// wiring).
		result.Edges = append(result.Edges, &graph.Edge{
			From: filePath, To: stmtNodeID, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: startLine,
		})

		// Placeholder call edge from the SQL statement back to the Java
		// DAO method that executes it. The synthesizer rewrites the
		// `To` onto the resolved Java method node; until then the edge
		// terminates on a deterministic unresolved:: placeholder keyed by
		// namespace::id.
		result.Edges = append(result.Edges, &graph.Edge{
			From: stmtNodeID,
			To:   myBatisUnresolvedPrefix + stmtNodeID,
			Kind: graph.EdgeCalls,
			Meta: map[string]any{
				"via":               "mybatis.mapper",
				"mybatis_namespace": namespace,
				"mybatis_statement": stmtID,
			},
			FilePath: filePath,
			Line:     startLine,
		})
	}
	return result, nil
}

// myBatisAttr returns the value of the attribute with the given local
// name (namespace-agnostic).
func myBatisAttr(se xml.StartElement, local string) string {
	for _, a := range se.Attr {
		if a.Name.Local == local {
			return strings.TrimSpace(a.Value)
		}
	}
	return ""
}

// myBatisStatementSQL returns the raw text body of the statement element
// whose start tag opens on line startLine1 (1-based). It scans the source
// for the element's `>` then captures up to the matching close tag,
// stripping nested dynamic-SQL tags so the result reads as plain SQL.
func myBatisStatementSQL(src []byte, startLine1 int) string {
	// Locate the byte offset of the start of startLine1.
	off := 0
	line := 1
	for off < len(src) && line < startLine1 {
		if src[off] == '\n' {
			line++
		}
		off++
	}
	// Find the end of the opening start tag ('>') from here.
	gt := bytes.IndexByte(src[off:], '>')
	if gt < 0 {
		return ""
	}
	bodyStart := off + gt + 1
	// Find the next matching statement close tag.
	rest := src[bodyStart:]
	endRel := -1
	for _, ct := range [][]byte{
		[]byte("</select>"), []byte("</insert>"), []byte("</update>"), []byte("</delete>"),
		[]byte("</SELECT>"), []byte("</INSERT>"), []byte("</UPDATE>"), []byte("</DELETE>"),
	} {
		if i := bytes.Index(rest, ct); i >= 0 && (endRel < 0 || i < endRel) {
			endRel = i
		}
	}
	if endRel < 0 {
		endRel = len(rest)
	}
	body := rest[:endRel]
	return normaliseMyBatisSQL(body)
}

// normaliseMyBatisSQL strips nested XML tags from a statement body and
// collapses whitespace so the stored SQL is a single readable string.
func normaliseMyBatisSQL(body []byte) string {
	var out []byte
	depth := 0 // inside a nested <…> tag
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c == '<':
			depth++
		case c == '>' && depth > 0:
			depth--
		case depth == 0:
			out = append(out, c)
		}
	}
	return strings.Join(strings.Fields(string(out)), " ")
}
