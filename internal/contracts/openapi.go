package contracts

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// OpenAPIExtractor detects OpenAPI/Swagger spec definitions (providers).
type OpenAPIExtractor struct{}

var (
	// Detect OpenAPI YAML structure: paths with HTTP methods.
	openapiPathRe   = regexp.MustCompile(`(?m)^  (\/\S+)\s*:`)
	openapiMethodRe = regexp.MustCompile(`(?m)^\s{4}(get|post|put|patch|delete|head|options)\s*:`)

	// Detect OpenAPI JSON structure.
	openapiJSONPathRe   = regexp.MustCompile(`"(\/[^"]+)"\s*:\s*\{`)
	openapiJSONMethodRe = regexp.MustCompile(`"(get|post|put|patch|delete|head|options)"\s*:\s*\{`)
)

func (e *OpenAPIExtractor) SupportedLanguages() []string {
	return []string{"yaml", "json"}
}

func (e *OpenAPIExtractor) Extract(filePath string, src []byte, nodes []*graph.Node, edges []*graph.Edge) []Contract {
	text := string(src)

	// Quick check: must contain either "openapi" or "swagger" key.
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "openapi") && !strings.Contains(lower, "swagger") {
		return nil
	}

	if strings.HasSuffix(filePath, ".json") {
		return e.extractJSON(filePath, src)
	}
	return e.extractYAML(filePath, src)
}

func (e *OpenAPIExtractor) extractYAML(filePath string, src []byte) []Contract {
	var contracts []Contract
	text := string(src)
	lines := strings.Split(text, "\n")

	// Find the "paths:" section.
	pathsIdx := strings.Index(text, "\npaths:")
	if pathsIdx < 0 {
		if strings.HasPrefix(text, "paths:") {
			pathsIdx = 0
		} else {
			return nil
		}
	}
	pathsSection := text[pathsIdx:]

	// Find each path entry.
	pathMatches := openapiPathRe.FindAllStringSubmatchIndex(pathsSection, -1)
	for i, pm := range pathMatches {
		path := pathsSection[pm[2]:pm[3]]
		// Determine the sub-section for this path (up to next path or end).
		end := len(pathsSection)
		if i+1 < len(pathMatches) {
			end = pathMatches[i+1][0]
		}
		pathBlock := pathsSection[pm[0]:end]

		for _, mm := range openapiMethodRe.FindAllStringSubmatch(pathBlock, -1) {
			method := strings.ToUpper(mm[1])
			absOffset := pathsIdx + pm[0]
			// Emit the ID in HTTP form so OpenAPI-declared providers
			// pair with cross-repo HTTP consumers via the same
			// matcher path (T2.4 Phase C). Type stays ContractOpenAPI
			// for discoverability under `contracts list --type openapi`.
			normPath := NormalizeHTTPPath(path)
			contracts = append(contracts, Contract{
				ID:         fmt.Sprintf("http::%s::%s", method, normPath),
				Type:       ContractOpenAPI,
				Role:       RoleProvider,
				FilePath:   filePath,
				Line:       lineNumber(lines, absOffset),
				Meta:       map[string]any{"method": method, "path": normPath},
				Confidence: 0.95,
			})
		}
	}

	return contracts
}

func (e *OpenAPIExtractor) extractJSON(filePath string, src []byte) []Contract {
	var contracts []Contract
	text := string(src)
	lines := strings.Split(text, "\n")

	pathMatches := openapiJSONPathRe.FindAllStringSubmatchIndex(text, -1)
	for i, pm := range pathMatches {
		path := text[pm[2]:pm[3]]
		if !strings.HasPrefix(path, "/") {
			continue
		}
		end := len(text)
		if i+1 < len(pathMatches) {
			end = pathMatches[i+1][0]
		}
		pathBlock := text[pm[0]:end]

		for _, mm := range openapiJSONMethodRe.FindAllStringSubmatch(pathBlock, -1) {
			method := strings.ToUpper(mm[1])
			normPath := NormalizeHTTPPath(path)
			contracts = append(contracts, Contract{
				ID:         fmt.Sprintf("http::%s::%s", method, normPath),
				Type:       ContractOpenAPI,
				Role:       RoleProvider,
				FilePath:   filePath,
				Line:       lineNumber(lines, pm[0]),
				Meta:       map[string]any{"method": method, "path": normPath},
				Confidence: 0.9,
			})
		}
	}

	return contracts
}
