package contracts

import (
	"fmt"
	"regexp"
	"strings"
)

// ContractType identifies the protocol or mechanism of a contract.
type ContractType string

const (
	ContractHTTP    ContractType = "http"
	ContractGRPC    ContractType = "grpc"
	ContractGraphQL ContractType = "graphql"
	ContractTopic   ContractType = "topic"
	ContractWS      ContractType = "ws"
	ContractEnv     ContractType = "env"
	ContractOpenAPI    ContractType = "openapi"
	ContractDependency ContractType = "dependency"
)

// Role indicates whether a symbol provides or consumes a contract.
type Role string

const (
	RoleProvider Role = "provider"
	RoleConsumer Role = "consumer"
)

// Contract represents a detected API contract (e.g., an HTTP route) attached
// to a symbol in the graph.
type Contract struct {
	ID         string            `json:"id"`
	Type       ContractType      `json:"type"`
	Role       Role              `json:"role"`
	SymbolID   string            `json:"symbol_id"`
	FilePath   string            `json:"file_path"`
	Line       int               `json:"line"`
	RepoPrefix string            `json:"repo_prefix,omitempty"`
	Meta       map[string]any `json:"meta,omitempty"`
	Confidence float64           `json:"confidence"`
}

// paramPatterns matches common path parameter styles and normalises them to {param}.
var paramPatterns = regexp.MustCompile(`:(\w+)|<(\w+(?::\w+)?)>|\{(\w+)\}`)

// NormalizeHTTPPath converts path parameters from various frameworks into the
// canonical {param} form.  Examples:
//
//	/users/:id        -> /users/{id}
//	/users/<int:id>   -> /users/{id}
//	/users/{id}       -> /users/{id}  (no change)
func NormalizeHTTPPath(path string) string {
	// Strip leading/trailing whitespace and quotes.
	path = strings.Trim(path, " \t\"'`")

	// Strip scheme + authority so a consumer URL like
	// "http://api.example.com/v1/users" matches a provider route like
	// "/v1/users". Without this, the consumer's Contract.ID includes the
	// host and never pairs with the provider's, so cross-service traversal
	// stops at the HTTP call site.
	if idx := strings.Index(path, "://"); idx >= 0 {
		rest := path[idx+3:]
		if slash := strings.Index(rest, "/"); slash >= 0 {
			path = rest[slash:]
		} else {
			path = "/"
		}
	}

	// Normalise parameter placeholders.
	path = paramPatterns.ReplaceAllStringFunc(path, func(m string) string {
		sub := paramPatterns.FindStringSubmatch(m)
		// sub[1] = :param, sub[2] = <param> (possibly typed), sub[3] = {param}
		for _, s := range sub[1:] {
			if s != "" {
				// Drop type prefix if present, e.g. "int:id" -> "id".
				if idx := strings.LastIndex(s, ":"); idx >= 0 {
					s = s[idx+1:]
				}
				return fmt.Sprintf("{%s}", s)
			}
		}
		return m
	})

	// Ensure leading slash.
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	// Remove trailing slash (except for root).
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}

	return path
}
