package languages

import (
	"path"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/hcl"
)

// HCLExtractor extracts HCL/Terraform files into graph nodes and edges.
// Top-level blocks (resource, data, module, variable, output, provider,
// locals, terraform, …) become KindType nodes; the block kind rides on
// Meta["block_type"] and its Terraform reference address on
// Meta["tf_address"]. Each `locals` declaration additionally yields a
// KindConstant node addressed `local.<key>`. Cross-block value
// expressions (var.x, local.y, module.m, data.t.n, aws_instance.web.id)
// produce EdgeReferences edges so a change-impact walk can answer "what
// breaks if this resource/variable changes?". Block node IDs are scoped
// to the file's directory — the Terraform module boundary — so a
// reference in one .tf file resolves to a block defined in a sibling .tf
// file of the same module by exact ID match.
type HCLExtractor struct {
	lang *sitter.Language
}

func NewHCLExtractor() *HCLExtractor {
	return &HCLExtractor{lang: hcl.GetLanguage()}
}

func (e *HCLExtractor) Language() string     { return "hcl" }
func (e *HCLExtractor) Extensions() []string { return []string{".tf", ".tfvars", ".hcl"} }

func (e *HCLExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	tree, err := parser.ParseFile(src, e.lang)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	root := tree.RootNode()
	result := &parser.ExtractionResult{}

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: int(root.EndPoint().Row) + 1,
		Language: "hcl",
	}
	result.Nodes = append(result.Nodes, fileNode)

	dir := hclModuleDir(filePath)
	seen := make(map[string]bool)    // block-name dedup, per file
	refSeen := make(map[string]bool) // (from\x00to) reference dedup
	e.walkTopLevel(root, src, filePath, dir, fileNode.ID, result, seen, refSeen)

	return result, nil
}

// hclModuleDir returns the directory holding the file — the Terraform
// module boundary. Every .tf file in a directory shares one address
// space, so block node IDs are scoped to the directory
// (hcl::<dir>::<address>); a reference in one file then resolves to a
// block defined in a sibling file of the same module by exact ID match.
func hclModuleDir(filePath string) string {
	d := path.Dir(filePath)
	if d == "" {
		return "."
	}
	return d
}

func hclNodeID(dir, address string) string { return "hcl::" + dir + "::" + address }

// walkTopLevel descends config_file / body wrappers and dispatches every
// TOP-LEVEL block (one not nested inside another block) to extractBlock.
// Nested blocks (ingress, lifecycle, dynamic, …) are not separate
// definition nodes — their value expressions are attributed to the
// enclosing top-level block as references.
func (e *HCLExtractor) walkTopLevel(node *sitter.Node, src []byte, filePath, dir, fileID string, result *parser.ExtractionResult, seen, refSeen map[string]bool) {
	if node == nil {
		return
	}
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "block":
			e.extractBlock(child, src, filePath, dir, fileID, result, seen, refSeen)
		case "config_file", "body":
			e.walkTopLevel(child, src, filePath, dir, fileID, result, seen, refSeen)
		}
	}
}

func (e *HCLExtractor) extractBlock(node *sitter.Node, src []byte, filePath, dir, fileID string, result *parser.ExtractionResult, seen, refSeen map[string]bool) {
	// A block is: identifier (block type), string_lit labels, then body.
	// E.g. resource "aws_instance" "web" { ... }
	var blockType string
	var labels []string
	var body *sitter.Node
	for i, _nc := 0, int(node.ChildCount()); i < _nc; i++ {
		child := node.Child(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "identifier":
			if blockType == "" {
				blockType = child.Content(src)
			}
		case "string_lit":
			if text := trimQuotes(child.Content(src)); text != "" {
				labels = append(labels, text)
			}
		case "body":
			body = child
		}
	}
	if blockType == "" {
		return
	}

	// Name keeps the block-type prefix (resource.aws_instance.web) for
	// human display; tf_address is the reference form other blocks use.
	name := blockType
	for _, l := range labels {
		name += "." + l
	}
	address := hclBlockAddress(blockType, labels)
	id := hclNodeID(dir, address)
	startLine := int(node.StartPoint().Row) + 1

	if !seen[name] {
		seen[name] = true
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindType, Name: name,
			FilePath: filePath, StartLine: startLine, EndLine: int(node.EndPoint().Row) + 1,
			Language: "hcl",
			Meta: map[string]any{
				"block_type": blockType,
				"labels":     labels,
				"tf_address": address,
			},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: startLine,
		})
	}

	// A locals block declares N independently-addressable values
	// (local.<key>); emit one KindConstant per key and resolve each
	// value's references from that key's node.
	if blockType == "locals" && body != nil {
		e.extractLocals(body, src, filePath, dir, fileID, result, refSeen)
		return
	}

	// Cross-block references: var.x, local.y, module.m, data.t.n, and
	// <resource_type>.<name> traversals anywhere in the block body.
	if body != nil {
		e.collectReferences(body, src, filePath, dir, id, result, refSeen)
	}
}

// extractLocals emits a KindConstant node per declaration in a `locals`
// block (addressed local.<key>) and links each to the blocks its value
// expression references.
func (e *HCLExtractor) extractLocals(body *sitter.Node, src []byte, filePath, dir, fileID string, result *parser.ExtractionResult, refSeen map[string]bool) {
	for i, _nc := 0, int(body.ChildCount()); i < _nc; i++ {
		attr := body.Child(i)
		if attr == nil || attr.Type() != "attribute" {
			continue
		}
		var key string
		var expr *sitter.Node
		for j, _nc := 0, int(attr.ChildCount()); j < _nc; j++ {
			c := attr.Child(j)
			if c == nil {
				continue
			}
			switch c.Type() {
			case "identifier":
				if key == "" {
					key = c.Content(src)
				}
			case "expression":
				expr = c
			}
		}
		if key == "" {
			continue
		}
		address := "local." + key
		id := hclNodeID(dir, address)
		line := int(attr.StartPoint().Row) + 1
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindConstant, Name: address,
			FilePath: filePath, StartLine: line, EndLine: int(attr.EndPoint().Row) + 1,
			Language: "hcl",
			Meta:     map[string]any{"block_type": "local", "tf_address": address},
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: fileID, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: line,
		})
		if expr != nil {
			e.collectReferences(expr, src, filePath, dir, id, result, refSeen)
		}
	}
}

// collectReferences walks an expression subtree and emits an
// EdgeReferences from fromID to every block/variable/local/module/data
// address it traverses. A traversal is a variable_expr (the head
// identifier) immediately followed by get_attr children within the same
// parent — var.region, aws_instance.web.id, data.aws_ami.ubuntu.id,
// module.vpc.subnet_ids. The recursion reaches traversals nested inside
// templates ("web-${var.region}"), objects, function-call args, and
// for-expressions.
func (e *HCLExtractor) collectReferences(node *sitter.Node, src []byte, filePath, dir, fromID string, result *parser.ExtractionResult, refSeen map[string]bool) {
	if node == nil {
		return
	}
	cc := int(node.ChildCount())
	for i := 0; i < cc; i++ {
		child := node.Child(i)
		if child == nil || child.Type() != "variable_expr" {
			continue
		}
		head := hclIdentText(child, src)
		if head == "" {
			continue
		}
		var attrs []string
		for j := i + 1; j < cc; j++ {
			sib := node.Child(j)
			if sib == nil || sib.Type() != "get_attr" {
				break // stop the chain at the first index ([0]) or operator
			}
			if a := hclGetAttrName(sib, src); a != "" {
				attrs = append(attrs, a)
			}
		}
		addr := hclRefAddress(head, attrs)
		if addr == "" {
			continue
		}
		to := hclNodeID(dir, addr)
		if to == fromID {
			continue
		}
		key := fromID + "\x00" + to
		if refSeen[key] {
			continue
		}
		refSeen[key] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From: fromID, To: to, Kind: graph.EdgeReferences,
			FilePath: filePath, Line: int(child.StartPoint().Row) + 1,
			Origin: graph.OriginASTResolved,
		})
	}
	for i := 0; i < cc; i++ {
		e.collectReferences(node.Child(i), src, filePath, dir, fromID, result, refSeen)
	}
}

// hclBlockAddress returns the Terraform reference address for a block —
// the form other blocks use to refer to it: resource → <type>.<name>
// (no leading "resource."), data → data.<type>.<name>, variable →
// var.<name>, module/output/provider → <type>.<name>; everything else
// (locals, terraform, moved, import, check, …) is addressed by its type
// plus any labels.
func hclBlockAddress(blockType string, labels []string) string {
	switch blockType {
	case "resource":
		if len(labels) >= 2 {
			return labels[0] + "." + labels[1]
		}
	case "data":
		if len(labels) >= 2 {
			return "data." + labels[0] + "." + labels[1]
		}
	case "variable":
		if len(labels) >= 1 {
			return "var." + labels[0]
		}
	case "module", "output", "provider":
		if len(labels) >= 1 {
			return blockType + "." + labels[0]
		}
	}
	addr := blockType
	for _, l := range labels {
		addr += "." + l
	}
	return addr
}

// hclRefAddress maps a parsed traversal (head identifier + get_attr chain)
// to the Terraform address of the block it refers to, or "" when the head
// is a built-in scope (each/count/self/path/terraform) or the traversal
// is too short to name a block.
func hclRefAddress(head string, attrs []string) string {
	switch head {
	case "each", "count", "self", "path", "terraform":
		return ""
	case "var":
		if len(attrs) >= 1 {
			return "var." + attrs[0]
		}
	case "local":
		if len(attrs) >= 1 {
			return "local." + attrs[0]
		}
	case "module":
		if len(attrs) >= 1 {
			return "module." + attrs[0]
		}
	case "data":
		if len(attrs) >= 2 {
			return "data." + attrs[0] + "." + attrs[1]
		}
	default:
		if len(attrs) >= 1 {
			// Resource reference: <type>.<name>[.attr…] → <type>.<name>.
			return head + "." + attrs[0]
		}
	}
	return ""
}

// hclIdentText returns the identifier text of a variable_expr node.
func hclIdentText(varExpr *sitter.Node, src []byte) string {
	for i, _nc := 0, int(varExpr.ChildCount()); i < _nc; i++ {
		c := varExpr.Child(i)
		if c != nil && c.Type() == "identifier" {
			return c.Content(src)
		}
	}
	return strings.TrimSpace(varExpr.Content(src))
}

// hclGetAttrName returns the attribute name of a get_attr node (".id" → "id").
func hclGetAttrName(getAttr *sitter.Node, src []byte) string {
	for i, _nc := 0, int(getAttr.ChildCount()); i < _nc; i++ {
		c := getAttr.Child(i)
		if c != nil && c.Type() == "identifier" {
			return c.Content(src)
		}
	}
	return strings.TrimPrefix(strings.TrimSpace(getAttr.Content(src)), ".")
}

func trimQuotes(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}
