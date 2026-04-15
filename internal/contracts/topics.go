package contracts

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// TopicExtractor detects message topic publish (provider) and subscribe (consumer) patterns.
type TopicExtractor struct{}

var (
	// Publish patterns (providers).
	topicPublishPatterns = []*regexp.Regexp{
		// Go: producer.Produce("topic", nc.Publish("topic", ch.Publish("exchange", "routing.key"
		regexp.MustCompile(`\.Produce\(\s*"([^"]+)"`),
		regexp.MustCompile(`\.Publish\(\s*"([^"]+)"`),
		// TS: producer.send({ topic: "name"
		regexp.MustCompile(`\.send\(\s*\{\s*topic:\s*"([^"]+)"`),
		// TS: channel.publish("topic"
		regexp.MustCompile(`channel\.publish\(\s*"([^"]+)"`),
		// Python: producer.produce("topic", channel.basic_publish(routing_key="topic"
		regexp.MustCompile(`\.produce\(\s*"([^"]+)"`),
		regexp.MustCompile(`basic_publish\([^)]*routing_key\s*=\s*"([^"]+)"`),
	}

	// Subscribe patterns (consumers).
	topicSubscribePatterns = []*regexp.Regexp{
		// Go: consumer.Subscribe("topic", nc.Subscribe("topic"
		regexp.MustCompile(`\.Subscribe\(\s*"([^"]+)"`),
		// TS: consumer.run({ topics: ["name"]
		regexp.MustCompile(`topics:\s*\[\s*"([^"]+)"`),
		// TS: channel.consume("queue"
		regexp.MustCompile(`\.consume\(\s*"([^"]+)"`),
		// Python: consumer.subscribe(["topic"])
		regexp.MustCompile(`\.subscribe\(\s*\[\s*"([^"]+)"`),
		// Python: channel.basic_consume(queue="queue"
		regexp.MustCompile(`basic_consume\([^)]*queue\s*=\s*"([^"]+)"`),
	}
)

func (e *TopicExtractor) SupportedLanguages() []string {
	return []string{"go", "typescript", "javascript", "python"}
}

func (e *TopicExtractor) Extract(filePath string, src []byte, nodes []*graph.Node, edges []*graph.Edge) []Contract {
	var contracts []Contract
	text := string(src)
	lines := strings.Split(text, "\n")

	fileNodes := filterFileNodes(filePath, nodes)
	sort.Slice(fileNodes, func(i, j int) bool {
		return fileNodes[i].StartLine < fileNodes[j].StartLine
	})

	for _, re := range topicPublishPatterns {
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			topic := text[m[2]:m[3]]
			ln := lineNumber(lines, m[0])
			contracts = append(contracts, Contract{
				ID:         fmt.Sprintf("topic::%s", topic),
				Type:       ContractTopic,
				Role:       RoleProvider,
				SymbolID:   findEnclosingSymbol(fileNodes, ln),
				FilePath:   filePath,
				Line:       ln,
				Meta:       map[string]any{"topic": topic},
				Confidence: 0.85,
			})
		}
	}

	for _, re := range topicSubscribePatterns {
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			topic := text[m[2]:m[3]]
			ln := lineNumber(lines, m[0])
			contracts = append(contracts, Contract{
				ID:         fmt.Sprintf("topic::%s", topic),
				Type:       ContractTopic,
				Role:       RoleConsumer,
				SymbolID:   findEnclosingSymbol(fileNodes, ln),
				FilePath:   filePath,
				Line:       ln,
				Meta:       map[string]any{"topic": topic},
				Confidence: 0.85,
			})
		}
	}

	return contracts
}
