package languages

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Celery task-dispatch binding. A Celery task is a decorator-gated
// function (`@shared_task def send_email(): ...`) invoked asynchronously by
// `send_email.delay(...)` / `.apply_async(...)` / `.s(...)`, or by
// registered name through `current_app.send_task("emails.send")`. The
// static call graph cannot see the dispatch because the task runs out of
// process. This pass tags each task function and stamps a placeholder
// EdgeCalls per dispatch site, which the resolver's ResolveCeleryCalls
// binds at the typed framework tier (the decorator gate makes it precise).

// celeryViaTag must match resolver.celeryVia — the languages package does
// not import resolver, so the two agree by value.
const celeryViaTag = "celery-dispatch"

// celeryTaskDecorators are the Celery task decorators, matched on the last
// dotted segment so `@shared_task`, `@task`, `@app.task`, and
// `@celery.task` all qualify.
var celeryTaskDecorators = map[string]bool{"shared_task": true, "task": true}

// celeryDispatchMethods are the async-dispatch methods invoked on a task.
var celeryDispatchMethods = map[string]bool{"delay": true, "apply_async": true, "s": true}

var celeryNameKwargRe = regexp.MustCompile(`name\s*=\s*['"]([^'"]+)['"]`)

// captureCeleryDispatch tags task functions and stamps dispatch
// placeholders. Runs at the tail of Extract so the task function nodes
// already exist.
func captureCeleryDispatch(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
	if root == nil || result == nil {
		return
	}
	nodesByLine := map[int][]*graph.Node{}
	for _, n := range result.Nodes {
		if n != nil && (n.Kind == graph.KindFunction || n.Kind == graph.KindMethod) {
			nodesByLine[n.StartLine] = append(nodesByLine[n.StartLine], n)
		}
	}

	// Tag task functions defined in this file.
	celeryWalk(root, func(n *sitter.Node) {
		if n.Type() != "function_definition" {
			return
		}
		regName, ok := celeryTaskDecorator(n, src)
		if !ok {
			return
		}
		nameNode := n.ChildByFieldName("name")
		if nameNode == nil {
			return
		}
		funcName := nameNode.Content(src)
		line := int(n.StartPoint().Row) + 1
		for _, nd := range nodesByLine[line] {
			if nd.Name != funcName {
				continue
			}
			if nd.Meta == nil {
				nd.Meta = map[string]any{}
			}
			nd.Meta["celery_task"] = funcName
			if regName != "" {
				nd.Meta["celery_registered_name"] = regName
			}
			break
		}
	})

	// Stamp dispatch placeholders. Emitted regardless of whether this file
	// defines tasks — a task is typically dispatched from another module,
	// and the resolver binds each placeholder by task name cross-file.
	funcRanges := buildFuncRanges(result)
	seen := map[string]bool{}
	celeryWalk(root, func(call *sitter.Node) {
		if call.Type() != "call" {
			return
		}
		fn := call.ChildByFieldName("function")
		if fn == nil || fn.Type() != "attribute" {
			return
		}
		method := ""
		if a := fn.ChildByFieldName("attribute"); a != nil {
			method = a.Content(src)
		}
		base := fn.ChildByFieldName("object")
		line := int(call.StartPoint().Row) + 1
		from := findEnclosingFunc(funcRanges, line)
		if from == "" {
			return
		}
		switch {
		case celeryDispatchMethods[method]:
			task := celeryAttrLastName(base, src)
			if task == "" {
				return
			}
			celeryAppendPlaceholder(result, from, task, "", filePath, line, seen)
		case method == "send_task":
			reg := celeryFirstStringArg(call, src)
			if reg == "" {
				return
			}
			name := reg
			if i := strings.LastIndex(reg, "."); i >= 0 {
				name = reg[i+1:]
			}
			celeryAppendPlaceholder(result, from, name, reg, filePath, line, seen)
		}
	})
}

// celeryTaskDecorator reports whether a function_definition carries a
// Celery task decorator, returning any `name=` registered-name override.
func celeryTaskDecorator(funcDef *sitter.Node, src []byte) (regName string, ok bool) {
	for _, dec := range pyDecoratorNodes(funcDef) {
		name, args := pyDecoratorNameAndArgs(dec, src)
		if name == "" {
			continue
		}
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
		if !celeryTaskDecorators[name] {
			continue
		}
		if m := celeryNameKwargRe.FindStringSubmatch(args); m != nil {
			return m[1], true
		}
		return "", true
	}
	return "", false
}

func celeryAppendPlaceholder(result *parser.ExtractionResult, from, taskName, regName, filePath string, line int, seen map[string]bool) {
	if from == "" || taskName == "" {
		return
	}
	k := from + "\x00" + taskName + "\x00" + regName
	if seen[k] {
		return
	}
	seen[k] = true
	meta := map[string]any{"via": celeryViaTag, "celery_task": taskName}
	if regName != "" {
		meta["celery_registered_name"] = regName
	}
	result.Edges = append(result.Edges, &graph.Edge{
		From:     from,
		To:       "unresolved::*." + taskName,
		Kind:     graph.EdgeCalls,
		FilePath: filePath,
		Line:     line,
		Meta:     meta,
	})
}

// celeryAttrLastName returns the trailing identifier of a dispatch
// receiver: `send_email` for `send_email`, and for `tasks.send_email`.
func celeryAttrLastName(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	switch n.Type() {
	case "identifier":
		return n.Content(src)
	case "attribute":
		if a := n.ChildByFieldName("attribute"); a != nil {
			return a.Content(src)
		}
	}
	return ""
}

func celeryFirstStringArg(call *sitter.Node, src []byte) string {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		a := args.NamedChild(i)
		if a != nil && a.Type() == "string" {
			return pyStringLiteralContent(a, src)
		}
	}
	return ""
}

// celeryWalk visits n and all its named descendants.
func celeryWalk(n *sitter.Node, fn func(*sitter.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		celeryWalk(n.NamedChild(i), fn)
	}
}
