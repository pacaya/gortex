package contracts

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// EnvVarExtractor detects environment variable reads (consumers) and definitions (providers).
type EnvVarExtractor struct{}

var (
	// Consumer patterns.
	envConsumerPatterns = []*regexp.Regexp{
		// Go: os.Getenv("VAR"), os.LookupEnv("VAR")
		regexp.MustCompile(`os\.Getenv\(\s*"([^"]+)"`),
		regexp.MustCompile(`os\.LookupEnv\(\s*"([^"]+)"`),
		// TS: process.env.VAR
		regexp.MustCompile(`process\.env\.([A-Z][A-Z0-9_]+)`),
		// TS: process.env["VAR"]
		regexp.MustCompile(`process\.env\[\s*"([^"]+)"\s*\]`),
		// Python: os.environ["VAR"], os.getenv("VAR")
		regexp.MustCompile(`os\.environ\[\s*"([^"]+)"\s*\]`),
		regexp.MustCompile(`os\.getenv\(\s*"([^"]+)"`),
		// Java: System.getenv("VAR")
		regexp.MustCompile(`System\.getenv\(\s*"([^"]+)"`),
	}

	// Provider patterns.
	envProviderPatterns = []*regexp.Regexp{
		// Go: os.Setenv("VAR", value)
		regexp.MustCompile(`os\.Setenv\(\s*"([^"]+)"`),
	}

	// .env file lines: VAR=value
	envFileLineRe = regexp.MustCompile(`(?m)^([A-Z][A-Z0-9_]+)\s*=`)

	// docker-compose environment entries: - VAR=value or VAR: value
	dockerEnvRe = regexp.MustCompile(`(?m)^\s*-?\s*([A-Z][A-Z0-9_]+)\s*[=:]`)
)

func (e *EnvVarExtractor) SupportedLanguages() []string {
	return []string{"go", "typescript", "javascript", "python", "java", "env", "yaml"}
}

func (e *EnvVarExtractor) Extract(filePath string, src []byte, nodes []*graph.Node, edges []*graph.Edge) []Contract {
	var contracts []Contract

	if isEnvFile(filePath) {
		contracts = append(contracts, e.extractEnvFileProviders(filePath, src)...)
	} else if isDockerCompose(filePath) {
		contracts = append(contracts, e.extractDockerComposeProviders(filePath, src)...)
	} else {
		// Thread file nodes into consumer extraction so each os.Getenv
		// site anchors on its enclosing function — required for tools
		// like get_dependencies(handler) to trace which env vars a
		// request needs read.
		fileNodes := filterFileNodes(filePath, nodes)
		sort.Slice(fileNodes, func(i, j int) bool {
			return fileNodes[i].StartLine < fileNodes[j].StartLine
		})
		contracts = append(contracts, e.extractCodeConsumers(filePath, src, fileNodes)...)
		contracts = append(contracts, e.extractCodeProviders(filePath, src)...)
	}

	return contracts
}

func (e *EnvVarExtractor) extractCodeConsumers(filePath string, src []byte, fileNodes []*graph.Node) []Contract {
	var contracts []Contract
	text := string(src)
	lines := strings.Split(text, "\n")

	for _, re := range envConsumerPatterns {
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			varName := text[m[2]:m[3]]
			ln := lineNumber(lines, m[0])
			contracts = append(contracts, Contract{
				ID:         fmt.Sprintf("env::%s", varName),
				Type:       ContractEnv,
				Role:       RoleConsumer,
				SymbolID:   findEnclosingSymbol(fileNodes, ln),
				FilePath:   filePath,
				Line:       ln,
				Meta:       map[string]any{"var": varName},
				Confidence: 0.9,
			})
		}
	}

	return contracts
}

func (e *EnvVarExtractor) extractCodeProviders(filePath string, src []byte) []Contract {
	var contracts []Contract
	text := string(src)
	lines := strings.Split(text, "\n")

	for _, re := range envProviderPatterns {
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			varName := text[m[2]:m[3]]
			contracts = append(contracts, Contract{
				ID:         fmt.Sprintf("env::%s", varName),
				Type:       ContractEnv,
				Role:       RoleProvider,
				FilePath:   filePath,
				Line:       lineNumber(lines, m[0]),
				Meta:       map[string]any{"var": varName},
				Confidence: 0.9,
			})
		}
	}

	return contracts
}

func (e *EnvVarExtractor) extractEnvFileProviders(filePath string, src []byte) []Contract {
	var contracts []Contract
	text := string(src)
	lines := strings.Split(text, "\n")

	for _, m := range envFileLineRe.FindAllStringSubmatchIndex(text, -1) {
		varName := text[m[2]:m[3]]
		contracts = append(contracts, Contract{
			ID:         fmt.Sprintf("env::%s", varName),
			Type:       ContractEnv,
			Role:       RoleProvider,
			FilePath:   filePath,
			Line:       lineNumber(lines, m[0]),
			Meta:       map[string]any{"var": varName, "source": "dotenv"},
			Confidence: 0.95,
		})
	}

	return contracts
}

func (e *EnvVarExtractor) extractDockerComposeProviders(filePath string, src []byte) []Contract {
	var contracts []Contract
	text := string(src)
	lines := strings.Split(text, "\n")

	// Only look within environment: sections.
	envIdx := strings.Index(text, "environment:")
	if envIdx < 0 {
		return nil
	}

	section := text[envIdx:]
	for _, m := range dockerEnvRe.FindAllStringSubmatchIndex(section, -1) {
		varName := section[m[2]:m[3]]
		contracts = append(contracts, Contract{
			ID:         fmt.Sprintf("env::%s", varName),
			Type:       ContractEnv,
			Role:       RoleProvider,
			FilePath:   filePath,
			Line:       lineNumber(lines, envIdx+m[0]),
			Meta:       map[string]any{"var": varName, "source": "docker-compose"},
			Confidence: 0.9,
		})
	}

	return contracts
}

func isEnvFile(path string) bool {
	base := path
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		base = path[idx+1:]
	}
	return base == ".env" || strings.HasPrefix(base, ".env.")
}

func isDockerCompose(path string) bool {
	base := path
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		base = path[idx+1:]
	}
	return strings.HasPrefix(base, "docker-compose") && (strings.HasSuffix(base, ".yml") || strings.HasSuffix(base, ".yaml"))
}
