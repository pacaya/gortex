package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Sidekiq job-dispatch binding. A Sidekiq worker is a class that
// `include Sidekiq::Job` / `Sidekiq::Worker` and defines `perform`; jobs are
// enqueued by `EmailJob.perform_async(...)` / `.perform_in(...)` /
// `.perform_at(...)`, which the static call graph cannot connect to
// `perform` because the dispatch is asynchronous. This pass tags each
// worker's perform method and stamps a placeholder per dispatch site, which
// the resolver's ResolveSidekiqCalls binds (include-gated, so typed tier).

// sidekiqViaTag must match resolver.sidekiqVia — the languages package does
// not import resolver, so the two agree by value.
const sidekiqViaTag = "sidekiq-dispatch"

// sidekiqIncludes are the module mixins that mark a class as a Sidekiq
// worker.
var sidekiqIncludes = map[string]bool{"Sidekiq::Job": true, "Sidekiq::Worker": true}

// sidekiqDispatchMethods are the enqueue methods invoked on a worker class.
var sidekiqDispatchMethods = map[string]bool{
	"perform_async":      true,
	"perform_in":         true,
	"perform_at":         true,
	"perform_bulk":       true,
	"perform_async_bulk": true,
}

// captureSidekiqDispatch tags worker perform methods and stamps dispatch
// placeholders. Runs at the tail of Extract so the method nodes exist.
func captureSidekiqDispatch(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
	if root == nil || result == nil {
		return
	}

	// Pass 1: collect worker class simple names.
	workerSimple := map[string]bool{}
	sidekiqWalk(root, func(n *sitter.Node) {
		if n.Type() != "class" || !sidekiqWorkerClass(n, src) {
			return
		}
		if name := n.ChildByFieldName("name"); name != nil {
			workerSimple[sidekiqSimpleName(name.Content(src))] = true
		}
	})
	if len(workerSimple) > 0 {
		for _, nd := range result.Nodes {
			if nd == nil || nd.Kind != graph.KindMethod || nd.Name != "perform" || nd.Meta == nil {
				continue
			}
			recv, _ := nd.Meta["receiver"].(string)
			if recv == "" || !workerSimple[sidekiqSimpleName(recv)] {
				continue
			}
			nd.Meta["sidekiq_worker"] = recv
		}
	}

	// Pass 2: dispatch sites.
	funcRanges := buildFuncRanges(result)
	seen := map[string]bool{}
	sidekiqWalk(root, func(call *sitter.Node) {
		if call.Type() != "call" {
			return
		}
		mn := call.ChildByFieldName("method")
		if mn == nil || !sidekiqDispatchMethods[mn.Content(src)] {
			return
		}
		recv := call.ChildByFieldName("receiver")
		worker := sidekiqConstName(recv, src)
		if worker == "" {
			return
		}
		line := int(call.StartPoint().Row) + 1
		from := findEnclosingFunc(funcRanges, line)
		if from == "" {
			return
		}
		k := from + "\x00" + worker
		if seen[k] {
			return
		}
		seen[k] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From:     from,
			To:       "unresolved::*.perform",
			Kind:     graph.EdgeCalls,
			FilePath: filePath,
			Line:     line,
			Meta:     map[string]any{"via": sidekiqViaTag, "sidekiq_worker": worker},
		})
	})
}

// sidekiqWorkerClass reports whether a class body contains an
// `include Sidekiq::Job` / `Sidekiq::Worker` mixin.
func sidekiqWorkerClass(classDecl *sitter.Node, src []byte) bool {
	body := classDecl.ChildByFieldName("body")
	if body == nil {
		return false
	}
	for i, _nc := 0, int(body.NamedChildCount()); i < _nc; i++ {
		c := body.NamedChild(i)
		if c == nil || (c.Type() != "call" && c.Type() != "command") {
			continue
		}
		mn := c.ChildByFieldName("method")
		if mn == nil || mn.Content(src) != "include" {
			continue
		}
		args := c.ChildByFieldName("arguments")
		if args == nil {
			continue
		}
		for j, _nc := 0, int(args.NamedChildCount()); j < _nc; j++ {
			a := args.NamedChild(j)
			if a != nil && sidekiqIncludes[strings.TrimSpace(a.Content(src))] {
				return true
			}
		}
	}
	return false
}

// sidekiqConstName returns the constant-path text of a dispatch receiver
// (`EmailJob`, `Workers::EmailJob`), or "" when it is not a constant.
func sidekiqConstName(recv *sitter.Node, src []byte) string {
	if recv == nil {
		return ""
	}
	switch recv.Type() {
	case "constant", "scope_resolution":
		return strings.TrimSpace(recv.Content(src))
	}
	return ""
}

// sidekiqSimpleName returns the last `::` segment of a Ruby constant path.
func sidekiqSimpleName(s string) string {
	if i := strings.LastIndex(s, "::"); i >= 0 {
		return s[i+2:]
	}
	return s
}

// sidekiqWalk visits n and all its named descendants.
func sidekiqWalk(n *sitter.Node, fn func(*sitter.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
		sidekiqWalk(n.NamedChild(i), fn)
	}
}
