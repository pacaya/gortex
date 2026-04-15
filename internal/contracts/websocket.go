package contracts

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// WebSocketExtractor detects WebSocket event emit (provider) and listen (consumer) patterns.
type WebSocketExtractor struct{}

var (
	// Emit patterns (providers).
	wsEmitPatterns = []*regexp.Regexp{
		// socket.emit("event"
		regexp.MustCompile(`\.emit\(\s*"([^"]+)"`),
		// ws.send(JSON.stringify({type: "event"
		regexp.MustCompile(`\.send\(\s*JSON\.stringify\(\s*\{\s*type:\s*"([^"]+)"`),
		// conn.WriteJSON(map{"type": "event"
		regexp.MustCompile(`WriteJSON\([^)]*"type":\s*"([^"]+)"`),
	}

	// Listen patterns (consumers).
	wsListenPatterns = []*regexp.Regexp{
		// socket.on("event"
		regexp.MustCompile(`\.on\(\s*"([^"]+)"`),
		// ws.addEventListener("message"
		regexp.MustCompile(`\.addEventListener\(\s*"([^"]+)"`),
	}
)

func (e *WebSocketExtractor) SupportedLanguages() []string {
	return []string{"go", "typescript", "javascript", "python"}
}

func (e *WebSocketExtractor) Extract(filePath string, src []byte, nodes []*graph.Node, edges []*graph.Edge) []Contract {
	var contracts []Contract
	text := string(src)
	lines := strings.Split(text, "\n")

	fileNodes := filterFileNodes(filePath, nodes)
	sort.Slice(fileNodes, func(i, j int) bool {
		return fileNodes[i].StartLine < fileNodes[j].StartLine
	})

	for _, re := range wsEmitPatterns {
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			event := text[m[2]:m[3]]
			ln := lineNumber(lines, m[0])
			contracts = append(contracts, Contract{
				ID:         fmt.Sprintf("ws::%s", event),
				Type:       ContractWS,
				Role:       RoleProvider,
				SymbolID:   findEnclosingSymbol(fileNodes, ln),
				FilePath:   filePath,
				Line:       ln,
				Meta:       map[string]any{"event": event},
				Confidence: 0.85,
			})
		}
	}

	for _, re := range wsListenPatterns {
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			event := text[m[2]:m[3]]
			ln := lineNumber(lines, m[0])
			contracts = append(contracts, Contract{
				ID:         fmt.Sprintf("ws::%s", event),
				Type:       ContractWS,
				Role:       RoleConsumer,
				SymbolID:   findEnclosingSymbol(fileNodes, ln),
				FilePath:   filePath,
				Line:       ln,
				Meta:       map[string]any{"event": event},
				Confidence: 0.8,
			})
		}
	}

	return contracts
}
