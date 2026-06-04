package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// emitSwiftAnnotationEdges scans a declaration node for `modifiers` /
// `attribute` children and emits one EdgeAnnotated per attribute onto
// the synthetic `annotation::swift::<name>` node. Covers:
//
//	@objc                       → annotation::swift::objc
//	@objc(legacyName)           → annotation::swift::objc (args="legacyName")
//	@available(iOS 13.0, *)     → annotation::swift::available
//	@MainActor                  → annotation::swift::MainActor
//	@Published                  → annotation::swift::Published
//	@State / @Binding / @Environment — SwiftUI property wrappers
//	@objcMembers, @inlinable, @inline, @dynamicCallable, @propertyWrapper, …
//
// Property wrappers and actor attributes are dispatch-relevant — they
// change how the property is accessed (KVO, observation framework,
// main-thread isolation) — so making them queryable via `find_usages`
// on the synthetic annotation node lets agents answer "every @Published
// property in this module" with one hop.
//
// The Swift tree-sitter grammar nests attributes under a `modifiers`
// child of the declaration, so the scan walks that level. If the
// declaration has no modifiers child the function is a silent no-op.
func emitSwiftAnnotationEdges(
	defNode *sitter.Node, fromID, filePath string, src []byte,
	result *parser.ExtractionResult, seen map[string]bool,
) {
	if defNode == nil || fromID == "" {
		return
	}
	mods := defNode.ChildByFieldName("modifiers")
	if mods == nil {
		// Some declarations expose modifiers as a positional named
		// child rather than via a named field; scan top-level
		// children for a `modifiers` node as a fallback.
		for i := 0; i < int(defNode.NamedChildCount()); i++ {
			c := defNode.NamedChild(i)
			if c != nil && c.Type() == "modifiers" {
				mods = c
				break
			}
		}
	}
	if mods == nil {
		return
	}

	for i := 0; i < int(mods.NamedChildCount()); i++ {
		attr := mods.NamedChild(i)
		if attr == nil || attr.Type() != "attribute" {
			continue
		}
		name, args := swiftAttributeNameAndArgs(attr, src)
		if name == "" {
			continue
		}
		line := int(attr.StartPoint().Row) + 1
		EmitAnnotationEdge(fromID, "swift", name, args, filePath, line, result, seen)
	}
}

// swiftAttributeNameAndArgs reads an `attribute` AST node and returns
// (name, args). The name comes from the first `user_type` /
// `type_identifier` child (Swift's grammar wraps attribute names in a
// type position). Arguments come from any remaining named children
// joined by ", " — the verbatim form is preserved so route paths and
// availability shims stay queryable.
//
// For qualified attribute names (`@SomeModule.SomeAttr`) the trailing
// segment is returned so the synthetic annotation node groups every
// equivalent use regardless of import alias.
func swiftAttributeNameAndArgs(attr *sitter.Node, src []byte) (string, string) {
	if attr == nil {
		return "", ""
	}
	var name string
	var argParts []string
	for i := 0; i < int(attr.NamedChildCount()); i++ {
		c := attr.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "user_type":
			if name == "" {
				name = swiftUserTypeName(c, src)
			}
		case "type_identifier", "simple_identifier", "identifier":
			if name == "" {
				name = strings.TrimSpace(c.Content(src))
			} else {
				argParts = append(argParts, strings.TrimSpace(c.Content(src)))
			}
		default:
			argParts = append(argParts, strings.TrimSpace(c.Content(src)))
		}
	}
	args := strings.Join(argParts, ", ")
	return name, args
}

// swiftModifiers returns the `modifiers` child of a declaration node,
// trying the named field first and falling back to a positional scan —
// the two shapes Swift's tree-sitter grammar uses across versions.
func swiftModifiers(defNode *sitter.Node) *sitter.Node {
	if defNode == nil {
		return nil
	}
	if mods := defNode.ChildByFieldName("modifiers"); mods != nil {
		return mods
	}
	for i := 0; i < int(defNode.NamedChildCount()); i++ {
		if c := defNode.NamedChild(i); c != nil && c.Type() == "modifiers" {
			return c
		}
	}
	return nil
}

// swiftObjCAttr reports whether a declaration carries @objc and, if so,
// the explicit selector from an @objc(customSelector:) override (empty
// when @objc is bare). The explicit form is read verbatim from the
// attribute's parenthesised text so a full keyword selector
// (`@objc(moveFrom:to:)`) survives intact.
func swiftObjCAttr(defNode *sitter.Node, src []byte) (isObjC bool, explicit string) {
	mods := swiftModifiers(defNode)
	if mods == nil {
		return false, ""
	}
	for i := 0; i < int(mods.NamedChildCount()); i++ {
		attr := mods.NamedChild(i)
		if attr == nil || attr.Type() != "attribute" {
			continue
		}
		name, _ := swiftAttributeNameAndArgs(attr, src)
		if name != "objc" {
			continue
		}
		isObjC = true
		text := attr.Content(src)
		if open := strings.IndexByte(text, '('); open >= 0 {
			if closeIdx := strings.LastIndexByte(text, ')'); closeIdx > open {
				explicit = strings.TrimSpace(text[open+1 : closeIdx])
			}
		}
		return isObjC, explicit
	}
	return false, ""
}

// swiftObjCSelector computes the Objective-C selector a Swift method is
// exposed under when it carries @objc, or "" when it is not @objc. An
// explicit @objc("sel:") override wins; otherwise the selector is derived
// from the method's base name and argument labels per Swift's bridging
// rules: the first argument label folds into the base name capitalised
// (`move(from:to:)` → `moveFrom:to:`), `init` inserts "With"
// (`init(frame:)` → `initWithFrame:`), an omitted label (`_`) contributes
// a bare colon (`insertSubview(_:at:)` → `insertSubview:at:`), and a
// no-argument method keeps its bare name (`viewDidLoad`).
func swiftObjCSelector(defNode *sitter.Node, baseName string, src []byte) string {
	isObjC, explicit := swiftObjCAttr(defNode, src)
	if !isObjC {
		return ""
	}
	if explicit != "" {
		return explicit
	}
	return buildSwiftObjCSelector(baseName, swiftArgLabels(swiftParamClause(defNode, src)))
}

// swiftParamClause returns the text inside a declaration's parameter
// parentheses, skipping any leading attribute parens (`@objc(x)`) by
// starting the scan after the `func` keyword.
func swiftParamClause(defNode *sitter.Node, src []byte) string {
	if defNode == nil {
		return ""
	}
	text := defNode.Content(src)
	from := 0
	if kw := strings.Index(text, "func"); kw >= 0 {
		from = kw + len("func")
	}
	open := strings.IndexByte(text[from:], '(')
	if open < 0 {
		return ""
	}
	open += from
	depth := 0
	for i := open; i < len(text); i++ {
		switch text[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return text[open+1 : i]
			}
		}
	}
	return ""
}

// swiftArgLabels splits a parameter clause at top-level commas and
// returns each parameter's external argument label, with "_" (omitted
// label) mapped to the empty string.
func swiftArgLabels(paramClause string) []string {
	paramClause = strings.TrimSpace(paramClause)
	if paramClause == "" {
		return nil
	}
	var labels []string
	for _, p := range splitSwiftTopLevelCommas(paramClause) {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		label := firstSwiftArgLabel(p)
		if label == "_" {
			label = ""
		}
		labels = append(labels, label)
	}
	return labels
}

// firstSwiftArgLabel returns the first identifier token of a parameter
// declaration — its external argument label — stripping a trailing colon
// from the single-name form (`x: Int` → "x").
func firstSwiftArgLabel(param string) string {
	end := len(param)
	for i, r := range param {
		if r == ' ' || r == '\t' || r == ':' {
			end = i
			break
		}
	}
	return param[:end]
}

// buildSwiftObjCSelector assembles the ObjC selector from a Swift base
// name and its ordered argument labels.
func buildSwiftObjCSelector(base string, labels []string) string {
	if len(labels) == 0 {
		return base
	}
	var sb strings.Builder
	sb.WriteString(base)
	for i, label := range labels {
		switch {
		case i == 0 && base == "init" && label != "":
			sb.WriteString("With")
			sb.WriteString(capitalizeFirst(label))
		case i == 0 && label != "":
			sb.WriteString(capitalizeFirst(label))
		case i > 0:
			sb.WriteString(label)
		}
		sb.WriteByte(':')
	}
	return sb.String()
}

// splitSwiftTopLevelCommas splits s at commas not nested inside (), <>,
// [], or {} — so a generic / closure / default-value argument stays one
// parameter.
func splitSwiftTopLevelCommas(s string) []string {
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '<', '[', '{':
			depth++
		case ')', '>', ']', '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// capitalizeFirst upper-cases the first ASCII letter of s.
func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	if s[0] >= 'a' && s[0] <= 'z' {
		return string(s[0]-'a'+'A') + s[1:]
	}
	return s
}

// swiftUserTypeName pulls the trailing `type_identifier` out of a
// `user_type` chain (`Foo.Bar.Baz` → "Baz") so qualified annotation
// references collapse onto the same synthetic node.
func swiftUserTypeName(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	// Walk forward and remember the last type_identifier; Swift's
	// user_type nests left-to-right so the last identifier is the
	// leaf.
	var last string
	for i := 0; i < int(node.NamedChildCount()); i++ {
		c := node.NamedChild(i)
		if c == nil {
			continue
		}
		if c.Type() == "type_identifier" {
			last = strings.TrimSpace(c.Content(src))
		}
		if c.Type() == "user_type" {
			if inner := swiftUserTypeName(c, src); inner != "" {
				last = inner
			}
		}
	}
	if last == "" {
		// Fallback: take the verbatim content and slice on the last
		// `.` separator so qualified names still surface a leaf.
		text := strings.TrimSpace(node.Content(src))
		if idx := strings.LastIndex(text, "."); idx >= 0 {
			text = text[idx+1:]
		}
		last = text
	}
	return last
}
