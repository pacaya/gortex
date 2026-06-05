// Package entrypoints detects framework entry points — symbols and
// files reachable only from a framework runtime, never from
// application code. Alembic migrations, Next.js pages / routes, and
// ASP.NET host files all look unreachable to a call-graph walk; left
// unmarked they become dead-code false positives. Detect stamps
// Meta["entry_point"] / Meta["entry_point_kind"] so the dead-code
// analyzer treats them as live roots.
package entrypoints

import (
	"path"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// MetaEntryPoint / MetaEntryKind are the Node.Meta keys this package
// stamps. Exported so the dead-code analyzer can read them.
const (
	MetaEntryPoint = "entry_point"
	MetaEntryKind  = "entry_point_kind"
)

// Detect inspects one file's extracted nodes (and edges, for the
// annotation-driven detectors) for framework entry points and stamps
// the file node plus the relevant symbols. It returns the number of
// nodes stamped.
func Detect(relPath, lang string, nodes []*graph.Node, edges []*graph.Edge) int {
	slashed := path.Clean(strings.ReplaceAll(relPath, "\\", "/"))
	switch lang {
	case "python":
		return detectAlembic(nodes)
	case "typescript", "javascript":
		return detectNextJS(slashed, nodes)
	case "csharp":
		return detectASPNet(slashed, nodes)
	case "java":
		return detectJava(nodes, edges)
	}
	return 0
}

// stamp marks a node as a framework entry point.
func stamp(n *graph.Node, kind string) {
	if n.Meta == nil {
		n.Meta = map[string]any{}
	}
	n.Meta[MetaEntryPoint] = true
	n.Meta[MetaEntryKind] = kind
}

func isFnOrMethod(k graph.NodeKind) bool {
	return k == graph.KindFunction || k == graph.KindMethod
}

// detectAlembic flags an Alembic migration: a Python module defining
// both upgrade() and downgrade() at module level. That pair is the
// distinctive migration signature (Alembic, Flask-Migrate, …) and is
// path-independent — Alembic version directories are configurable.
func detectAlembic(nodes []*graph.Node) int {
	var hasUpgrade, hasDowngrade bool
	for _, n := range nodes {
		switch {
		case n.Kind == graph.KindFunction && n.Name == "upgrade":
			hasUpgrade = true
		case n.Kind == graph.KindFunction && n.Name == "downgrade":
			hasDowngrade = true
		}
	}
	if !hasUpgrade || !hasDowngrade {
		return 0
	}
	count := 0
	for _, n := range nodes {
		if n.Kind == graph.KindFile ||
			(n.Kind == graph.KindFunction && (n.Name == "upgrade" || n.Name == "downgrade")) {
			stamp(n, "alembic:migration")
			count++
		}
	}
	return count
}

// nextAppSpecialFiles are the Next.js App Router special filenames.
var nextAppSpecialFiles = map[string]bool{
	"page": true, "layout": true, "route": true, "loading": true,
	"error": true, "not-found": true, "template": true,
	"default": true, "global-error": true,
}

// nextEntrySymbols are exported functions the Next.js runtime calls
// directly — they have no in-app caller.
var nextEntrySymbols = map[string]bool{
	"getServerSideProps":   true,
	"getStaticProps":       true,
	"getStaticPaths":       true,
	"generateStaticParams": true,
	"generateMetadata":     true,
	"middleware":           true,
}

// detectNextJS flags Next.js pages, API routes, and App Router special
// files. App Router detection keys on the distinctive special
// filenames so a generic `app/` directory does not over-match.
func detectNextJS(relPath string, nodes []*graph.Node) int {
	ext := path.Ext(relPath)
	switch ext {
	case ".js", ".jsx", ".ts", ".tsx":
	default:
		return 0
	}
	segs := strings.Split(relPath, "/")
	base := strings.TrimSuffix(path.Base(relPath), ext)

	kind := ""
	switch {
	case slices.Contains(segs, "app") && nextAppSpecialFiles[base]:
		if base == "route" {
			kind = "nextjs:route"
		} else {
			kind = "nextjs:page"
		}
	case slices.Contains(segs, "pages"):
		if slices.Contains(segs, "api") {
			kind = "nextjs:api"
		} else {
			kind = "nextjs:page"
		}
	default:
		return 0
	}

	count := 0
	for _, n := range nodes {
		if n.Kind == graph.KindFile {
			stamp(n, kind)
			count++
			continue
		}
		if isFnOrMethod(n.Kind) && nextEntrySymbols[n.Name] {
			stamp(n, kind)
			count++
		}
	}
	return count
}

// detectASPNet flags an ASP.NET host file — Program.cs / Startup.cs —
// and its lifecycle methods (Main / ConfigureServices / Configure),
// which the host invokes, not application code.
func detectASPNet(relPath string, nodes []*graph.Node) int {
	switch path.Base(relPath) {
	case "Program.cs", "Startup.cs":
	default:
		return 0
	}
	count := 0
	for _, n := range nodes {
		switch {
		case n.Kind == graph.KindFile:
			stamp(n, "aspnet:host")
			count++
		case isFnOrMethod(n.Kind) &&
			(n.Name == "Main" || n.Name == "ConfigureServices" || n.Name == "Configure"):
			stamp(n, "aspnet:host")
			count++
		}
	}
	return count
}

// javaAnnoPrefix is the synthetic annotation node-ID prefix the Java
// extractor emits (languages.AnnotationNodeID("java", name)); the bare
// annotation name is the suffix.
const javaAnnoPrefix = "annotation::java::"

// javaEntryClassAnnos maps a class/interface-level annotation to the
// entry-point kind it confers. These mark a *type* the framework
// instantiates and drives (Spring stereotypes, JAX-RS resources,
// annotated servlets / websocket endpoints).
var javaEntryClassAnnos = map[string]string{
	"RestController": "spring:controller",
	"Controller":     "spring:controller",
	"Service":        "spring:bean",
	"Component":      "spring:bean",
	"Repository":     "spring:bean",
	"Configuration":  "spring:config",
	"Path":           "jaxrs:resource",
	"WebServlet":     "servlet:endpoint",
	"ServerEndpoint": "websocket:endpoint",
}

// javaEntryMethodAnnos maps a method-level annotation to the entry-point
// kind it confers. These mark a *method* the framework invokes directly
// — request handlers, lifecycle callbacks, scheduled jobs, test cases —
// which therefore has no in-application caller.
var javaEntryMethodAnnos = map[string]string{
	"RequestMapping":    "spring:handler",
	"GetMapping":        "spring:handler",
	"PostMapping":       "spring:handler",
	"PutMapping":        "spring:handler",
	"DeleteMapping":     "spring:handler",
	"PatchMapping":      "spring:handler",
	"EventListener":     "spring:listener",
	"Scheduled":         "spring:scheduled",
	"Bean":              "spring:bean",
	"PostConstruct":     "lifecycle:init",
	"PreDestroy":        "lifecycle:destroy",
	"Test":              "junit:test",
	"ParameterizedTest": "junit:test",
	"RepeatedTest":      "junit:test",
	"BeforeEach":        "junit:fixture",
	"AfterEach":         "junit:fixture",
	"BeforeAll":         "junit:fixture",
	"AfterAll":          "junit:fixture",
	"GET":               "jaxrs:handler",
	"POST":              "jaxrs:handler",
	"PUT":               "jaxrs:handler",
	"DELETE":            "jaxrs:handler",
	"HEAD":              "jaxrs:handler",
}

// detectJava flags Java framework entry points from annotation edges:
// Spring stereotypes / request handlers, JAX-RS resources, annotated
// servlets, JUnit test methods, lifecycle callbacks, and the JVM
// `main` method. Unlike the path-based detectors it stamps the
// individual annotated symbols, NOT the file node — a Spring controller
// file can still hold genuinely-dead private helpers, and only the
// framework-invoked members should be treated as live roots.
func detectJava(nodes []*graph.Node, edges []*graph.Edge) int {
	// symbol ID → set of annotation names applied to it.
	annos := map[string]map[string]bool{}
	for _, e := range edges {
		if e.Kind != graph.EdgeAnnotated {
			continue
		}
		name, ok := strings.CutPrefix(e.To, javaAnnoPrefix)
		if !ok {
			continue
		}
		if annos[e.From] == nil {
			annos[e.From] = map[string]bool{}
		}
		annos[e.From][name] = true
	}

	count := 0
	for _, n := range nodes {
		switch n.Kind {
		case graph.KindType, graph.KindInterface:
			for a := range annos[n.ID] {
				if kind, ok := javaEntryClassAnnos[a]; ok {
					stamp(n, kind)
					count++
					break
				}
			}
		case graph.KindFunction, graph.KindMethod:
			kind := ""
			for a := range annos[n.ID] {
				if k, ok := javaEntryMethodAnnos[a]; ok {
					kind = k
					break
				}
			}
			if kind == "" && n.Name == "main" {
				kind = "java:main"
			}
			if kind != "" {
				stamp(n, kind)
				count++
			}
		}
	}
	return count
}
