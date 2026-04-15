package contracts

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// GraphQLExtractor detects GraphQL schema definitions (providers) and query usage (consumers).
type GraphQLExtractor struct{}

var (
	// Schema providers: type Query { field: Type } or type Mutation { ... }
	gqlTypeBlockRe = regexp.MustCompile(`(?m)type\s+(Query|Mutation|Subscription)\s*\{([^}]*)\}`)
	gqlFieldRe     = regexp.MustCompile(`(?m)^\s*(\w+)`)

	// Consumer query strings: query { users { ... } } or mutation { createUser(...) { ... } }
	gqlQueryOpRe    = regexp.MustCompile(`(?m)(query|mutation|subscription)\s*(?:\w+\s*)?\{([^}]*)\}`)
	gqlGqlTagRe     = regexp.MustCompile("(?s)gql`([^`]*)`")
	gqlTopFieldRe   = regexp.MustCompile(`(?m)^\s*(\w+)`)
)

func (e *GraphQLExtractor) SupportedLanguages() []string {
	return []string{"graphql", "go", "typescript", "javascript", "python"}
}

func (e *GraphQLExtractor) Extract(filePath string, src []byte, nodes []*graph.Node, edges []*graph.Edge) []Contract {
	var contracts []Contract

	if strings.HasSuffix(filePath, ".graphql") || strings.HasSuffix(filePath, ".gql") {
		contracts = append(contracts, e.extractSchemaProviders(filePath, src)...)
	}
	// Consumer queries can live in any source file. Thread file nodes
	// through so findEnclosingSymbol can anchor each consumer contract
	// on its calling function — required for EdgeMatches bridges.
	fileNodes := filterFileNodes(filePath, nodes)
	sort.Slice(fileNodes, func(i, j int) bool {
		return fileNodes[i].StartLine < fileNodes[j].StartLine
	})
	contracts = append(contracts, e.extractConsumers(filePath, src, fileNodes)...)

	return contracts
}

func (e *GraphQLExtractor) extractSchemaProviders(filePath string, src []byte) []Contract {
	var contracts []Contract
	text := string(src)
	lines := strings.Split(text, "\n")

	for _, blockMatch := range gqlTypeBlockRe.FindAllStringSubmatchIndex(text, -1) {
		typeName := text[blockMatch[2]:blockMatch[3]] // Query, Mutation, Subscription
		body := text[blockMatch[4]:blockMatch[5]]
		bodyOffset := blockMatch[4]

		for _, fLine := range strings.Split(body, "\n") {
			fLine = strings.TrimSpace(fLine)
			if fLine == "" || strings.HasPrefix(fLine, "#") {
				continue
			}
			fm := gqlFieldRe.FindStringSubmatch(fLine)
			if fm == nil {
				continue
			}
			fieldName := fm[1]
			// Approximate offset for line number.
			idx := strings.Index(text[bodyOffset:], fLine)
			line := 1
			if idx >= 0 {
				line = lineNumber(lines, bodyOffset+idx)
			}
			contracts = append(contracts, Contract{
				ID:         fmt.Sprintf("graphql::%s::%s", typeName, fieldName),
				Type:       ContractGraphQL,
				Role:       RoleProvider,
				FilePath:   filePath,
				Line:       line,
				Meta:       map[string]any{"operation": typeName, "field": fieldName},
				Confidence: 0.95,
			})
		}
	}

	return contracts
}

func (e *GraphQLExtractor) extractConsumers(filePath string, src []byte, fileNodes []*graph.Node) []Contract {
	var contracts []Contract
	text := string(src)
	lines := strings.Split(text, "\n")

	// Direct query/mutation/subscription operations.
	for _, m := range gqlQueryOpRe.FindAllStringSubmatchIndex(text, -1) {
		opType := text[m[2]:m[3]]
		body := text[m[4]:m[5]]
		contracts = append(contracts, e.fieldsFromBody(filePath, opType, body, lines, m[0], fileNodes)...)
	}

	// gql`` tagged template literals.
	for _, m := range gqlGqlTagRe.FindAllStringSubmatchIndex(text, -1) {
		inner := text[m[2]:m[3]]
		// Try to find the operation keyword inside the template.
		for _, opM := range gqlQueryOpRe.FindAllStringSubmatch(inner, -1) {
			contracts = append(contracts, e.fieldsFromBody(filePath, opM[1], opM[2], lines, m[0], fileNodes)...)
		}
	}

	return contracts
}

func (e *GraphQLExtractor) fieldsFromBody(filePath, opType, body string, lines []string, baseOffset int, fileNodes []*graph.Node) []Contract {
	var contracts []Contract
	opTypeCap := strings.ToUpper(opType[:1]) + opType[1:]
	ln := lineNumber(lines, baseOffset)
	sym := findEnclosingSymbol(fileNodes, ln)

	for _, fLine := range strings.Split(body, "\n") {
		fLine = strings.TrimSpace(fLine)
		if fLine == "" {
			continue
		}
		fm := gqlTopFieldRe.FindStringSubmatch(fLine)
		if fm == nil {
			continue
		}
		fieldName := fm[1]
		contracts = append(contracts, Contract{
			ID:         fmt.Sprintf("graphql::%s::%s", opTypeCap, fieldName),
			Type:       ContractGraphQL,
			Role:       RoleConsumer,
			SymbolID:   sym,
			FilePath:   filePath,
			Line:       ln,
			Meta:       map[string]any{"operation": opTypeCap, "field": fieldName},
			Confidence: 0.8,
		})
	}

	return contracts
}
