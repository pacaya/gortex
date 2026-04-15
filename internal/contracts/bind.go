package contracts

import (
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// BindProviderSymbols resolves provider-contract SymbolIDs that were
// left empty at extraction time. Used for contract types whose
// provider source is a spec file without Go/TS symbols — .proto for
// gRPC, OpenAPI YAML/JSON for HTTP — where the actual implementation
// lives in a different file in the same repo. After binding, the
// matcher's bridge emission in ReconcileContractEdges can emit
// EdgeMatches that cross service boundaries to land on real business
// logic, not on the spec declaration.
//
// Mutates the registry in place. Returns the number of contracts
// bound. Providers already carrying SymbolID are skipped.
//
// Algorithm:
//  1. For each unbound provider contract, derive a candidate short
//     method name from the contract's Meta.
//  2. Restrict the search to nodes in the same RepoPrefix (binding
//     across repos is semantically wrong — a proto file in auth-proto
//     shouldn't bind to a method in an unrelated repo even if names
//     collide).
//  3. Prefer candidates whose receiver type matches a convention for
//     this contract kind (e.g. gRPC's `{Service}Server` pattern).
//  4. Tiebreak: prefer candidates in files that mention a registration
//     call like `pb.Register{Service}Server(` or `r.{HTTPVerb}(`.
//  5. Uniquely bind or skip (never guess among multiple).
func BindProviderSymbols(reg *Registry, g *graph.Graph) int {
	if reg == nil || g == nil {
		return 0
	}
	bound := 0
	for _, c := range reg.All() {
		if c.Role != RoleProvider || c.SymbolID != "" {
			continue
		}
		var newID string
		switch c.Type {
		case ContractGRPC:
			newID = bindGRPCProvider(c, g)
		case ContractOpenAPI:
			newID = bindOpenAPIProvider(c, g)
		}
		if newID == "" {
			continue
		}
		// Registry stores contracts by ID; rewriting a field on the
		// in-place record requires replacement via Add (which
		// overwrites by ID).
		c.SymbolID = newID
		reg.Add(c)
		// Also add the EdgeProvides from the symbol to the contract
		// node so downstream tools see the link.
		if g.GetNode(c.ID) != nil {
			g.AddEdge(&graph.Edge{
				From:     newID,
				To:       c.ID,
				Kind:     graph.EdgeProvides,
				FilePath: c.FilePath,
				Line:     c.Line,
			})
		}
		bound++
	}
	return bound
}

// bindGRPCProvider returns the node ID of the Go/TS method implementing
// the RPC, or "" if we can't uniquely pick one.
//
// Heuristic (highest-priority first):
//  1. Method named <contract.Meta["method"]> whose receiver type is
//     exactly `{Service}Server` (the common gRPC-generated server
//     interface naming).
//  2. Same method name, receiver type containing "Server" anywhere.
//  3. Same method name, in a file that contains a
//     `Register{Service}Server(` call.
//  4. Same method name, any receiver — only if there's exactly one
//     candidate in the repo.
func bindGRPCProvider(c Contract, g *graph.Graph) string {
	method, _ := c.Meta["method"].(string)
	service, _ := c.Meta["service"].(string)
	if method == "" || service == "" {
		return ""
	}
	candidates := filterSameRepoMethods(g.FindNodesByName(method), c.RepoPrefix)
	if len(candidates) == 0 {
		return ""
	}

	// Tier 1: exact "{Service}Server" receiver match.
	exactRecv := service + "Server"
	if sole := soleWithReceiver(candidates, exactRecv); sole != "" {
		return sole
	}

	// Tier 2: receiver contains "Server".
	if sole := soleWithReceiverContaining(candidates, "Server"); sole != "" {
		return sole
	}

	// Tier 3: file containing RegisterFooServer(.
	regRE := regexp.MustCompile(`Register` + regexp.QuoteMeta(service) + `Server\s*\(`)
	if sole := soleInFileMatching(candidates, regRE); sole != "" {
		return sole
	}

	// Tier 4: unique by name only.
	if len(candidates) == 1 {
		return candidates[0].ID
	}
	return ""
}

// bindOpenAPIProvider picks the Go/TS handler that implements the
// OpenAPI-declared operation. Since operationId conventions vary
// widely, this is lower-confidence than gRPC binding; a stricter
// implementation would also check the Gin/Echo route registration
// file, but v1 just name-matches. Returns "" if no unambiguous bind.
func bindOpenAPIProvider(c Contract, g *graph.Graph) string {
	op, _ := c.Meta["operationId"].(string)
	if op == "" {
		// Fall back to the last path segment; OpenAPI specs
		// commonly use /{collection}/{action} and a handler named
		// after the action (listUsers, createUser).
		path, _ := c.Meta["path"].(string)
		op = lastPathSegment(path)
	}
	if op == "" {
		return ""
	}
	candidates := filterSameRepoMethods(g.FindNodesByName(op), c.RepoPrefix)
	if len(candidates) == 1 {
		return candidates[0].ID
	}
	return ""
}

// filterSameRepoMethods retains only nodes in the given RepoPrefix
// that are Functions or Methods. Cross-repo binding is wrong — a
// spec-file provider in repo A should not bind to an unrelated
// same-named method in repo B.
func filterSameRepoMethods(nodes []*graph.Node, repoPrefix string) []*graph.Node {
	out := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if repoPrefix != "" && n.RepoPrefix != repoPrefix {
			continue
		}
		out = append(out, n)
	}
	return out
}

// soleWithReceiver returns the ID of the unique candidate whose
// receiver type matches exactly, or "" if zero or many.
func soleWithReceiver(candidates []*graph.Node, recv string) string {
	hits := make([]string, 0, 2)
	for _, n := range candidates {
		if nodeReceiverType(n) == recv {
			hits = append(hits, n.ID)
			if len(hits) > 1 {
				return ""
			}
		}
	}
	if len(hits) == 1 {
		return hits[0]
	}
	return ""
}

func soleWithReceiverContaining(candidates []*graph.Node, sub string) string {
	hits := make([]string, 0, 2)
	for _, n := range candidates {
		if strings.Contains(nodeReceiverType(n), sub) {
			hits = append(hits, n.ID)
			if len(hits) > 1 {
				return ""
			}
		}
	}
	if len(hits) == 1 {
		return hits[0]
	}
	return ""
}

func soleInFileMatching(candidates []*graph.Node, re *regexp.Regexp) string {
	// We don't have the file bytes here; the best proxy is the
	// graph's defining file containing the hint in another symbol's
	// signature — a coarse filter. For now, return only when one
	// candidate exists and its file path uniquely identifies it;
	// otherwise skip. This tier becomes useful when we add a
	// registration-site scan later.
	if len(candidates) == 1 {
		return candidates[0].ID
	}
	return ""
}

// nodeReceiverType returns the declared receiver type for a method
// node (e.g. "UsersServer" from a method on *UsersServer). Prefers the
// extractor's pre-computed Meta["receiver"] since the signature format
// varies by language — Go can be stored as "func ((s *T))" or
// "func (s *T)" depending on the pipeline. Falls back to a signature
// scan only when the explicit meta is absent.
var goRecvFallbackRE = regexp.MustCompile(`func\s*\(+\s*\w+\s+\*?(\w+)\s*\)+`)

func nodeReceiverType(n *graph.Node) string {
	if recv, ok := n.Meta["receiver"].(string); ok && recv != "" {
		return recv
	}
	sig, _ := n.Meta["signature"].(string)
	if sig == "" {
		return ""
	}
	m := goRecvFallbackRE.FindStringSubmatch(sig)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// lastPathSegment returns the final slash-delimited segment of a path,
// stripping braces and parameters. "/v1/users/{id}" → "id";
// "/v1/users" → "users".
func lastPathSegment(p string) string {
	p = strings.TrimSuffix(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	p = strings.Trim(p, "{}")
	return p
}
