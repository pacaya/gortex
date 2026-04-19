package contracts

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// GRPCExtractor detects gRPC service definitions (providers) and client usage (consumers).
type GRPCExtractor struct{}

var (
	// Proto service definitions: service Foo { rpc Bar(...) returns (...) }
	protoServiceRe = regexp.MustCompile(`(?m)service\s+(\w+)\s*\{`)
	protoRPCRe     = regexp.MustCompile(`(?m)rpc\s+(\w+)\s*\(`)

	// Richer RPC pattern that captures the request / response message
	// types along with optional `stream` modifiers on either side:
	//   rpc Foo(FooReq) returns (FooResp);
	//   rpc Stream(stream Req) returns (stream Resp);
	//   rpc X(pkg.Foo) returns (pkg.Bar);
	//
	// Groups:
	//   1 = method name
	//   2 = "stream" on request (or "")
	//   3 = request type  (may contain dots: pkg.Foo)
	//   4 = "stream" on response (or "")
	//   5 = response type
	protoRPCShapeRe = regexp.MustCompile(`(?m)rpc\s+(\w+)\s*\(\s*(stream\s+)?([\w.]+)\s*\)\s*returns\s*\(\s*(stream\s+)?([\w.]+)\s*\)`)

	// Go consumers
	// Pass 1: var := pb.NewUsersClient(conn)  — captures (varName, service).
	// Accepts any package selector or unqualified, not just "pb.".
	goGRPCNewClientAssignRe = regexp.MustCompile(`(?m)(\w+)\s*(?::=|=)\s*(?:[\w.]+\.)?New(\w+)Client\s*\(`)
	// Pass 2: varName.MethodName(...)  — cross-reference against the map.
	goGRPCCallRe = regexp.MustCompile(`(\w+)\s*\.\s*(\w+)\s*\(`)

	// Legacy Go "pb.NewServiceClient" pattern — kept so a client
	// construction with no assignment (e.g. used inline) still records
	// the service as a consumer contract with SymbolID on the
	// enclosing function, even when we can't resolve method calls.
	goGRPCNewClientRe = regexp.MustCompile(`(?:[\w.]+\.)?New(\w+)Client\s*\(`)

	// TypeScript consumers
	tsGRPCNewClientRe       = regexp.MustCompile(`new\s+(\w+)Client\(`)
	tsGRPCNewClientAssignRe = regexp.MustCompile(`(?m)(?:const|let|var)\s+(\w+)\s*(?::\s*\w+\s*)?=\s*new\s+(\w+)Client\(`)
	// stub.methodName( — camelCase per the TS proto generator.
	tsGRPCCallRe = regexp.MustCompile(`(\w+)\s*\.\s*(\w+)\s*\(`)

	// Python consumers
	pyGRPCStubRe       = regexp.MustCompile(`(\w+)Stub\(channel`)
	pyGRPCStubAssignRe = regexp.MustCompile(`(\w+)\s*=\s*\w*\.(\w+)Stub\(`)
	pyGRPCCallRe       = regexp.MustCompile(`(\w+)\s*\.\s*(\w+)\s*\(`)
)

func (e *GRPCExtractor) SupportedLanguages() []string {
	// "protobuf" matches the parser registry's Language() for .proto
	// files. "proto" is retained as a historical alias. Without the
	// protobuf entry the dispatch map in Indexer.buildPerFileContract
	// Extractors skips proto files entirely and the provider side of
	// the gRPC contract model goes missing.
	return []string{"protobuf", "proto", "go", "typescript", "python"}
}

func (e *GRPCExtractor) Extract(filePath string, src []byte, nodes []*graph.Node, edges []*graph.Edge) []Contract {
	var contracts []Contract

	if strings.HasSuffix(filePath, ".proto") {
		contracts = append(contracts, e.extractProtoProviders(filePath, src)...)
	} else {
		fileNodes := filterFileNodes(filePath, nodes)
		sort.Slice(fileNodes, func(i, j int) bool {
			return fileNodes[i].StartLine < fileNodes[j].StartLine
		})
		contracts = append(contracts, e.extractConsumers(filePath, src, fileNodes)...)
	}

	return contracts
}

func (e *GRPCExtractor) extractProtoProviders(filePath string, src []byte) []Contract {
	var contracts []Contract
	text := string(src)
	lines := strings.Split(text, "\n")

	// Build a fast lookup from (methodName → shape) so we can attach
	// request/response types to each RPC contract below. We run the
	// shape regex across the whole file once; services don't overlap
	// and method names are unique within a service, so this is safe.
	type shape struct {
		requestType    string
		responseType   string
		requestStream  bool
		responseStream bool
	}
	shapes := make(map[string]shape)
	for _, m := range protoRPCShapeRe.FindAllStringSubmatch(text, -1) {
		shapes[m[1]] = shape{
			requestType:    m[3],
			responseType:   m[5],
			requestStream:  strings.TrimSpace(m[2]) == "stream",
			responseStream: strings.TrimSpace(m[4]) == "stream",
		}
	}

	// Find service blocks and their RPC methods.
	serviceMatches := protoServiceRe.FindAllStringSubmatchIndex(text, -1)
	for _, sMatch := range serviceMatches {
		serviceName := text[sMatch[2]:sMatch[3]]
		// Find RPCs within the remainder of this service block.
		serviceStart := sMatch[0]
		rest := text[serviceStart:]
		rpcMatches := protoRPCRe.FindAllStringSubmatch(rest, -1)
		rpcLocs := protoRPCRe.FindAllStringIndex(rest, -1)
		for i, rpc := range rpcMatches {
			methodName := rpc[1]
			absOffset := serviceStart + rpcLocs[i][0]
			line := lineNumber(lines, absOffset)

			meta := map[string]any{
				"service":       serviceName,
				"method":        methodName,
				"schema_source": "none",
			}
			if s, ok := shapes[methodName]; ok {
				meta["request_type"] = s.requestType
				meta["response_type"] = s.responseType
				if s.requestStream {
					meta["request_stream"] = true
				}
				if s.responseStream {
					meta["response_stream"] = true
				}
				meta["schema_source"] = "extracted"
			}

			contracts = append(contracts, Contract{
				ID:         fmt.Sprintf("grpc::%s::%s", serviceName, methodName),
				Type:       ContractGRPC,
				Role:       RoleProvider,
				FilePath:   filePath,
				Line:       line,
				Meta:       meta,
				Confidence: 0.95,
			})
		}
	}

	return contracts
}

// extractConsumers detects gRPC client usage in a non-proto source file.
// For Go it uses a two-pass scan so per-method RPC calls can emit
// specific "grpc::Service::Method" contracts that match the provider ID
// format produced by extractProtoProviders. Without this, consumer IDs
// were "grpc::Service" while providers were "grpc::Service::Method" —
// so the matcher never paired any gRPC contract and no EdgeMatches
// bridge formed for gRPC. TS/Python stay at the service-level for now
// (client construction only); v1 scope.
func (e *GRPCExtractor) extractConsumers(filePath string, src []byte, fileNodes []*graph.Node) []Contract {
	var contracts []Contract
	text := string(src)
	lines := strings.Split(text, "\n")

	// ---- Go pass 1: client construction → varName → serviceName map.
	varToService := make(map[string]string)
	for _, m := range goGRPCNewClientAssignRe.FindAllStringSubmatchIndex(text, -1) {
		varName := text[m[2]:m[3]]
		svc := text[m[4]:m[5]]
		varToService[varName] = svc
	}

	// ---- Go pass 2: varName.Method( calls → grpc::Service::Method.
	// Track which lines we've already emitted for to avoid duplicates
	// when a single file has multiple consumer call sites on one line.
	seen := make(map[string]struct{})
	for _, m := range goGRPCCallRe.FindAllStringSubmatchIndex(text, -1) {
		recv := text[m[2]:m[3]]
		method := text[m[4]:m[5]]
		svc, ok := varToService[recv]
		if !ok {
			continue
		}
		ln := lineNumber(lines, m[0])
		key := fmt.Sprintf("%s::%s::%d", svc, method, ln)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		meta := map[string]any{"service": svc, "method": method, "lang": "go"}
		// The request message is the second positional argument
		// after the context: `client.Method(ctx, &pb.ReqType{...})`
		// or `client.Method(ctx, req)`. Capture either the inline
		// literal's type or the argument variable's declared type
		// from the surrounding window.
		if reqType := detectGoGRPCRequestType(text, m[1], fileNodes, ln); reqType != "" {
			meta["request_type"] = reqType
			meta["schema_source"] = "extracted"
		} else {
			meta["schema_source"] = "partial"
		}
		contracts = append(contracts, Contract{
			ID:         fmt.Sprintf("grpc::%s::%s", svc, method),
			Type:       ContractGRPC,
			Role:       RoleConsumer,
			SymbolID:   findEnclosingSymbol(fileNodes, ln),
			FilePath:   filePath,
			Line:       ln,
			Meta:       meta,
			Confidence: 0.9,
		})
	}

	// Fallback Go client-construction only (no method-level calls
	// resolvable, or the variable was used inline and never assigned).
	// Still records that the service is consumed somewhere in this
	// file, anchored on the enclosing function of the construction
	// site. Skipped when we already emitted a method-level contract
	// for this service (the method-level form is strictly more
	// informative).
	emittedServices := make(map[string]struct{})
	for _, c := range contracts {
		if svc, _ := c.Meta["service"].(string); svc != "" {
			emittedServices[svc] = struct{}{}
		}
	}
	for _, m := range goGRPCNewClientRe.FindAllStringSubmatchIndex(text, -1) {
		svc := text[m[2]:m[3]]
		if _, already := emittedServices[svc]; already {
			continue
		}
		ln := lineNumber(lines, m[0])
		contracts = append(contracts, Contract{
			ID:         fmt.Sprintf("grpc::%s", svc),
			Type:       ContractGRPC,
			Role:       RoleConsumer,
			SymbolID:   findEnclosingSymbol(fileNodes, ln),
			FilePath:   filePath,
			Line:       ln,
			Meta:       map[string]any{"service": svc, "lang": "go"},
			Confidence: 0.7,
		})
	}

	// TS: new ServiceNameClient() — service-level only in v1.
	// Also walk the rest of the file for per-method calls and emit
	// method-level contracts when we find them, carrying the
	// request type from the inline message literal.
	tsVarToService := make(map[string]string)
	for _, m := range tsGRPCNewClientAssignRe.FindAllStringSubmatch(text, -1) {
		tsVarToService[m[1]] = m[2]
	}
	tsSeen := make(map[string]struct{})
	for _, m := range tsGRPCCallRe.FindAllStringSubmatchIndex(text, -1) {
		recv := text[m[2]:m[3]]
		method := text[m[4]:m[5]]
		svc, ok := tsVarToService[recv]
		if !ok {
			continue
		}
		ln := lineNumber(lines, m[0])
		key := fmt.Sprintf("%s::%s::%d", svc, method, ln)
		if _, dup := tsSeen[key]; dup {
			continue
		}
		tsSeen[key] = struct{}{}
		meta := map[string]any{"service": svc, "method": method, "lang": "typescript"}
		if rt := detectTSGRPCRequestType(text, m[1]); rt != "" {
			meta["request_type"] = rt
			meta["schema_source"] = "extracted"
		} else {
			meta["schema_source"] = "partial"
		}
		contracts = append(contracts, Contract{
			ID:         fmt.Sprintf("grpc::%s::%s", svc, method),
			Type:       ContractGRPC,
			Role:       RoleConsumer,
			SymbolID:   findEnclosingSymbol(fileNodes, ln),
			FilePath:   filePath,
			Line:       ln,
			Meta:       meta,
			Confidence: 0.9,
		})
	}
	// Fallback service-level contracts for TS clients that don't
	// have resolvable method calls.
	tsEmitted := make(map[string]struct{})
	for _, c := range contracts {
		if s, _ := c.Meta["service"].(string); s != "" && c.Meta["lang"] == "typescript" {
			tsEmitted[s] = struct{}{}
		}
	}
	for _, m := range tsGRPCNewClientRe.FindAllStringSubmatchIndex(text, -1) {
		svc := text[m[2]:m[3]]
		if _, already := tsEmitted[svc]; already {
			continue
		}
		ln := lineNumber(lines, m[0])
		contracts = append(contracts, Contract{
			ID:         fmt.Sprintf("grpc::%s", svc),
			Type:       ContractGRPC,
			Role:       RoleConsumer,
			SymbolID:   findEnclosingSymbol(fileNodes, ln),
			FilePath:   filePath,
			Line:       ln,
			Meta:       map[string]any{"service": svc, "lang": "typescript"},
			Confidence: 0.85,
		})
	}

	// Python: ServiceNameStub(channel) — service-level + method-level
	// when stub.GetUser(request_pb2.GetUserRequest(...)) shows up.
	pyVarToService := make(map[string]string)
	for _, m := range pyGRPCStubAssignRe.FindAllStringSubmatch(text, -1) {
		pyVarToService[m[1]] = m[2]
	}
	pySeen := make(map[string]struct{})
	for _, m := range pyGRPCCallRe.FindAllStringSubmatchIndex(text, -1) {
		recv := text[m[2]:m[3]]
		method := text[m[4]:m[5]]
		svc, ok := pyVarToService[recv]
		if !ok {
			continue
		}
		ln := lineNumber(lines, m[0])
		key := fmt.Sprintf("%s::%s::%d", svc, method, ln)
		if _, dup := pySeen[key]; dup {
			continue
		}
		pySeen[key] = struct{}{}
		meta := map[string]any{"service": svc, "method": method, "lang": "python"}
		if rt := detectPyGRPCRequestType(text, m[1]); rt != "" {
			meta["request_type"] = rt
			meta["schema_source"] = "extracted"
		} else {
			meta["schema_source"] = "partial"
		}
		contracts = append(contracts, Contract{
			ID:         fmt.Sprintf("grpc::%s::%s", svc, method),
			Type:       ContractGRPC,
			Role:       RoleConsumer,
			SymbolID:   findEnclosingSymbol(fileNodes, ln),
			FilePath:   filePath,
			Line:       ln,
			Meta:       meta,
			Confidence: 0.9,
		})
	}
	pyEmitted := make(map[string]struct{})
	for _, c := range contracts {
		if s, _ := c.Meta["service"].(string); s != "" && c.Meta["lang"] == "python" {
			pyEmitted[s] = struct{}{}
		}
	}
	for _, m := range pyGRPCStubRe.FindAllStringSubmatchIndex(text, -1) {
		svc := text[m[2]:m[3]]
		if _, already := pyEmitted[svc]; already {
			continue
		}
		ln := lineNumber(lines, m[0])
		contracts = append(contracts, Contract{
			ID:         fmt.Sprintf("grpc::%s", svc),
			Type:       ContractGRPC,
			Role:       RoleConsumer,
			SymbolID:   findEnclosingSymbol(fileNodes, ln),
			FilePath:   filePath,
			Line:       ln,
			Meta:       map[string]any{"service": svc, "lang": "python"},
			Confidence: 0.85,
		})
	}

	return contracts
}

// detectTSGRPCRequestType picks out the request message type from
// `stub.getUser(new GetUserRequest({...}))` or `stub.getUser(req)`
// where `req: GetUserRequest` is declared nearby.
func detectTSGRPCRequestType(text string, callEnd int) string {
	slice := grpcCallArgSlice(text, callEnd)
	if slice == "" {
		return ""
	}
	args := splitTopLevelArgs(slice)
	if len(args) == 0 {
		return ""
	}
	first := strings.TrimSpace(args[0])
	// `new TypeName(...)` or `new pkg.TypeName(...)`.
	if strings.HasPrefix(first, "new ") {
		rest := strings.TrimSpace(strings.TrimPrefix(first, "new"))
		if i := strings.IndexAny(rest, "("); i > 0 {
			return strings.TrimSpace(rest[:i])
		}
	}
	return ""
}

// detectPyGRPCRequestType picks out the request type from
// `stub.GetUser(users_pb2.GetUserRequest(id="x"))`.
func detectPyGRPCRequestType(text string, callEnd int) string {
	slice := grpcCallArgSlice(text, callEnd)
	if slice == "" {
		return ""
	}
	args := splitTopLevelArgs(slice)
	if len(args) == 0 {
		return ""
	}
	first := strings.TrimSpace(args[0])
	// `mod.TypeName(...)` — Python convention.
	if i := strings.Index(first, "("); i > 0 {
		// Strip package prefix so the bare name remains.
		head := strings.TrimSpace(first[:i])
		if dot := strings.LastIndex(head, "."); dot >= 0 {
			head = head[dot+1:]
		}
		return head
	}
	return ""
}

// grpcCallArgSlice returns the text between the `(` at callEnd-1 and
// its matching `)`, without the outer parens.
func grpcCallArgSlice(text string, callEnd int) string {
	if callEnd <= 0 || callEnd > len(text) {
		return ""
	}
	depth := 1
	for i := callEnd; i < len(text); i++ {
		switch text[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return text[callEnd:i]
			}
		}
	}
	return ""
}

// detectGoGRPCRequestType picks the request message type out of a Go
// gRPC client call. It walks the source from `callEnd` (the byte
// offset of the `(` after the method name) to the matching `)`,
// skipping the first arg (context) and inspecting the second. Two
// shapes handled:
//
//	client.Method(ctx, &pb.GetUserRequest{...})   → "pb.GetUserRequest"
//	client.Method(ctx, req)                       → look up `req`'s
//	                                                declared type in
//	                                                a short backward
//	                                                window
//
// Returns a bare type name ("pb.GetUserRequest" or "GetUserRequest").
// The module-wide upgrade pass turns it into a full symbol ID.
func detectGoGRPCRequestType(text string, callEnd int, fileNodes []*graph.Node, line int) string {
	if callEnd >= len(text) {
		return ""
	}
	// Find the matching `)` for the method call.
	depth := 1
	end := -1
	for i := callEnd; i < len(text); i++ {
		switch text[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return ""
	}
	// Split args at top-level commas.
	args := splitTopLevelArgs(text[callEnd:end])
	if len(args) < 2 {
		return ""
	}
	second := strings.TrimSpace(args[1])
	// `&pkg.Type{...}` or `pkg.Type{...}` → extract the type prefix.
	second = strings.TrimPrefix(second, "&")
	if braceIdx := strings.Index(second, "{"); braceIdx >= 0 {
		typ := strings.TrimSpace(second[:braceIdx])
		if typ != "" {
			return typ
		}
	}
	// Bare identifier — look up its declaration.
	if isGoIdent(second) {
		// Small backward scan within the enclosing function body.
		lines := strings.Split(text, "\n")
		if line <= 0 || line > len(lines) {
			return ""
		}
		for i := line - 1; i >= 0 && i >= line-30; i-- {
			if typ := findGoVarTypeInLine(lines[i], second); typ != "" {
				return typ
			}
		}
	}
	// File-scoped node lookup as last resort: same-file variable or
	// field declaration.
	_ = fileNodes
	return ""
}

// splitTopLevelArgs breaks a parenthesised argument list at commas
// that aren't nested inside other parens / braces / brackets.
func splitTopLevelArgs(s string) []string {
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	if last := strings.TrimSpace(s[start:]); last != "" {
		out = append(out, last)
	}
	return out
}

func isGoIdent(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

// findGoVarTypeInLine looks for `name := &Type{...}`, `name := Type{...}`,
// `var name Type`, or `var name *Type` on a single source line and
// returns the type if found.
func findGoVarTypeInLine(line, name string) string {
	ln := strings.TrimSpace(line)
	prefixes := []string{name + " :=", name + ":="}
	for _, pfx := range prefixes {
		if strings.HasPrefix(ln, pfx) {
			rest := strings.TrimSpace(strings.TrimPrefix(ln, pfx))
			rest = strings.TrimPrefix(rest, "&")
			if idx := strings.Index(rest, "{"); idx >= 0 {
				return strings.TrimSpace(rest[:idx])
			}
			return ""
		}
	}
	if strings.HasPrefix(ln, "var "+name+" ") {
		rest := strings.TrimSpace(strings.TrimPrefix(ln, "var "+name+" "))
		rest = strings.TrimPrefix(rest, "*")
		// Cut at `=` or end.
		if idx := strings.Index(rest, "="); idx >= 0 {
			rest = rest[:idx]
		}
		return strings.TrimSpace(rest)
	}
	return ""
}

// lineNumber returns the 1-based line number for the given byte offset.
func lineNumber(lines []string, offset int) int {
	pos := 0
	for i, l := range lines {
		end := pos + len(l) + 1 // +1 for newline
		if offset < end {
			return i + 1
		}
		pos = end
	}
	return len(lines)
}
