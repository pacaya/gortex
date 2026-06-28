package languages

import (
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// goConfigMethod returns the (source, op, ok) classification for a
// recognised config-accessor method. The allowlist matches viper's
// public API exactly so domain methods like `Cache.Get` or
// `Logger.GetLevel` do not collide. Other config providers
// (envconfig, koanf, k8s configmap accessors) will land as
// additional cases when they're prioritised.
//
// Op is one of: "read", "write", "register".
var goViperReadMethods = map[string]struct{}{
	"Get":                     {},
	"GetBool":                 {},
	"GetDuration":             {},
	"GetFloat32":              {},
	"GetFloat64":              {},
	"GetInt":                  {},
	"GetInt32":                {},
	"GetInt64":                {},
	"GetIntSlice":             {},
	"GetSizeInBytes":          {},
	"GetString":               {},
	"GetStringMap":            {},
	"GetStringMapString":      {},
	"GetStringMapStringSlice": {},
	"GetStringSlice":          {},
	"GetTime":                 {},
	"GetUint":                 {},
	"GetUint16":               {},
	"GetUint32":               {},
	"GetUint64":               {},
	"IsSet":                   {},
}

func goConfigMethod(receiver, method string) (source, op string, ok bool) {
	// stdlib env-var lookups — disambiguated by the `os` package
	// receiver so domain types with similarly named methods don't
	// trigger false positives. This is the dominant pattern in
	// codebases that haven't adopted viper, so without it the
	// config_readers analyzer reports zero edges and looks broken.
	if receiver == "os" {
		switch method {
		case "Getenv":
			return "env", "read", true
		case "LookupEnv":
			return "env", "read", true
		case "Setenv":
			return "env", "write", true
		case "Unsetenv":
			return "env", "write", true
		}
	}
	if _, hit := goViperReadMethods[method]; hit {
		return "viper", "read", true
	}
	switch method {
	case "Set", "SetDefault", "SetEnvPrefix":
		return "viper", "write", true
	case "BindEnv", "BindPFlag", "BindFlagValue":
		return "viper", "register", true
	}
	return "", "", false
}

// goConfigSourceImported reports whether the file's import set carries
// the package that backs a config source. Used to suppress
// false-positive classifications where a method name overlaps with a
// viper method but the receiver is an unrelated domain type (e.g.
// `req.GetString("query")` against an mcp.CallToolRequest in a file
// that has nothing to do with viper).
func goConfigSourceImported(source string, imports map[string]string) bool {
	switch source {
	case "viper":
		for _, path := range imports {
			if strings.Contains(path, "spf13/viper") {
				return true
			}
		}
		return false
	default:
		// `env` (os.Getenv / os.Setenv) and future provider sources
		// without a distinguishing import: trust the receiver
		// disambiguation already done in goConfigMethod.
		return true
	}
}

// goConfigEvent is the deferred record emitted at capture time and
// resolved during the post-pass. Mirrors goObservabilityEvent and
// goFlagEvent.
type goConfigEvent struct {
	source string // "viper" today; future: "envconfig", "k8s_cm", …
	op     string // read / write / register
	method string // exact method name
	key    string // dotted path — first string-literal arg
	line   int    // 1-based line of the call expression
}

// detectGoConfigKey returns the config-source classification plus
// the resolved key when a callm.expr capture matches the config-
// method set and carries a string-literal key. Dynamic keys are
// skipped, mirroring the flag and observability extractors.
func detectGoConfigKey(callExpr *sitter.Node, receiver, method string, src []byte) (source, op, key string, ok bool) {
	if callExpr == nil {
		return "", "", "", false
	}
	source, op, ok = goConfigMethod(receiver, method)
	if !ok {
		return "", "", "", false
	}
	args := callExpr.ChildByFieldName("arguments")
	if args == nil {
		return "", "", "", false
	}
	for i, _nc := 0, int(args.NamedChildCount()); i < _nc; i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		if c.Type() != "interpreted_string_literal" && c.Type() != "raw_string_literal" {
			continue
		}
		text := strings.Trim(c.Content(src), "\"`")
		if text == "" {
			return "", "", "", false
		}
		return source, op, text, true
	}
	return "", "", "", false
}

// emitGoConfigKeys turns deferred config records into KindConfigKey
// nodes plus EdgeReadsConfig / EdgeWritesConfig edges. Read /
// register operations both produce a reads-config edge; only
// `Set*` produces writes-config. The op is also stamped on edge
// meta so consumers can scope by exact operation when needed.
//
// callerLookup mirrors the contract used by emitGoObservabilityEvents
// and emitGoFlagChecks.
func emitGoConfigKeys(events []goConfigEvent, callerLookup func(line int) string, filePath string, result *parser.ExtractionResult) {
	if len(events) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(events))
	for _, e := range events {
		callerID := callerLookup(e.line)
		if callerID == "" {
			continue
		}
		keyID := goConfigNodeID(e.source, e.key)
		if _, ok := seen[keyID]; !ok {
			seen[keyID] = struct{}{}
			result.Nodes = append(result.Nodes, &graph.Node{
				ID:       keyID,
				Kind:     graph.KindConfigKey,
				Name:     e.key,
				FilePath: filePath, // first sighting; not authoritative
				Language: "go",
				Meta: map[string]any{
					"source": e.source,
					"key":    e.key,
				},
			})
		}
		edgeKind := graph.EdgeReadsConfig
		if e.op == "write" {
			edgeKind = graph.EdgeWritesConfig
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From:     callerID,
			To:       keyID,
			Kind:     edgeKind,
			FilePath: filePath,
			Line:     e.line,
			Origin:   graph.OriginASTInferred,
			Meta: map[string]any{
				"op":     e.op,
				"method": e.method,
			},
		})
	}
}

// goConfigNodeID is the canonical ID for a config-key node. Uses
// the `cfg::` prefix from the spec's enumeration; the source
// segment carries the provider so two providers exposing the same
// key path stay distinguishable.
func goConfigNodeID(source, key string) string {
	if source == "" {
		source = "internal"
	}
	return "cfg::" + source + "::" + key
}
