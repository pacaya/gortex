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
	tsGRPCNewClientRe = regexp.MustCompile(`new\s+(\w+)Client\(`)

	// Python consumers
	pyGRPCStubRe = regexp.MustCompile(`(\w+)Stub\(channel`)
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
			contracts = append(contracts, Contract{
				ID:         fmt.Sprintf("grpc::%s::%s", serviceName, methodName),
				Type:       ContractGRPC,
				Role:       RoleProvider,
				FilePath:   filePath,
				Line:       line,
				Meta:       map[string]any{"service": serviceName, "method": methodName},
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
		contracts = append(contracts, Contract{
			ID:         fmt.Sprintf("grpc::%s::%s", svc, method),
			Type:       ContractGRPC,
			Role:       RoleConsumer,
			SymbolID:   findEnclosingSymbol(fileNodes, ln),
			FilePath:   filePath,
			Line:       ln,
			Meta:       map[string]any{"service": svc, "method": method, "lang": "go"},
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
	for _, m := range tsGRPCNewClientRe.FindAllStringSubmatchIndex(text, -1) {
		svc := text[m[2]:m[3]]
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

	// Python: ServiceNameStub(channel) — service-level only in v1.
	for _, m := range pyGRPCStubRe.FindAllStringSubmatchIndex(text, -1) {
		svc := text[m[2]:m[3]]
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
