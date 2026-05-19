package lsp

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/semantic"
)

// ServerSpec describes one LSP server's invocation, the languages it
// covers, and the file extensions that route requests to it. Specs are
// the source of truth for both default-config registration (used by
// `gortex daemon` / `gortex mcp` / `gortex server`) and the runtime
// router (which dials a per-extension provider on first touch).
type ServerSpec struct {
	// Name is the unique identifier used in config (e.g. "tsserver",
	// "pyright", "rust-analyzer", "clangd", "jdtls").
	Name string
	// Command is the executable the daemon spawns (e.g. "rust-analyzer",
	// "typescript-language-server", "pyright-langserver").
	Command string
	// Args is the argv tail. Most servers take stdio; servers that need
	// `--stdio` or similar carry it here.
	Args []string
	// Languages is the set of internal language codes this server
	// handles. Used by the manager's per-language priority routing.
	Languages []string
	// Extensions is the set of file extensions (with leading dot) that
	// route to this server. Drives the runtime router.
	Extensions []string
	// LanguageIDs maps an extension to the LSP `languageId` to use in
	// `textDocument/didOpen`. When an extension isn't listed, the first
	// entry of Languages is used as a fallback.
	LanguageIDs map[string]string
	// Priority is the default priority used when no user override
	// exists. Lower wins. Reserved 1-3 for user-tuned overrides; default
	// LSP servers use 5.
	Priority int
	// Daemon hints whether the LSP server is intended to be kept alive
	// across enrich passes. All known LSP servers benefit from this;
	// the field is here for explicit future tuning.
	Daemon bool
	// MaxParallel caps concurrent LSP requests for this server. 10 is a
	// safe starting point — gopls / rust-analyzer happily serve 10
	// in-flight requests without backpressure.
	MaxParallel int
	// AlternativeCommands lists fallback executables to try when the
	// primary `Command` is not on PATH. The first one that resolves
	// wins. Useful for ecosystems with multiple bundled drivers
	// (e.g. `tsserver` from `npm i -g typescript`, vs
	// `typescript-language-server`).
	AlternativeCommands []ServerAlt
	// Env carries extra KEY=VALUE environment entries for the server
	// subprocess, appended to the daemon's own environment. Empty for
	// every built-in spec; populated only when a user overrides a
	// server via .gortex.yaml (e.g. pinning JAVA_HOME for jdtls).
	Env []string
}

// ServerAlt is one fallback command form for a server.
type ServerAlt struct {
	Command string
	Args    []string
}

// Servers is the canonical list of LSP servers Gortex knows how to
// route. Order is meaningful only for the seed config — the runtime
// router uses the per-extension lookup table.
//
// Adding a new server: add the entry here and add its file extensions
// to extToSpec at init time. The default config picks it up
// automatically; users override priority / command via .gortex.yaml.
var Servers = []ServerSpec{
	{
		Name:        "gopls",
		Command:     "gopls",
		Args:        []string{"-remote=auto"},
		Languages:   []string{"go"},
		Extensions:  []string{".go"},
		LanguageIDs: map[string]string{".go": "go"},
		Priority:    3,
		Daemon:      true,
		MaxParallel: 10,
	},
	{
		Name:      "typescript-language-server",
		Command:   "typescript-language-server",
		Args:      []string{"--stdio"},
		Languages: []string{"typescript", "javascript"},
		Extensions: []string{
			".ts", ".tsx", ".mts", ".cts",
			".js", ".jsx", ".mjs", ".cjs",
		},
		LanguageIDs: map[string]string{
			".ts":  "typescript",
			".tsx": "typescriptreact",
			".mts": "typescript",
			".cts": "typescript",
			".js":  "javascript",
			".jsx": "javascriptreact",
			".mjs": "javascript",
			".cjs": "javascript",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 10,
	},
	{
		// tsgo is the native-Go TypeScript compiler (the
		// @typescript/native-preview package). Its LSP mode is offered
		// at a lower precedence than typescript-language-server so the
		// default routing is unchanged for anyone who has both — a
		// user opts in by raising its priority in .gortex.yaml.
		Name:      "tsgo",
		Command:   "tsgo",
		Args:      []string{"--lsp", "--stdio"},
		Languages: []string{"typescript", "javascript"},
		Extensions: []string{
			".ts", ".tsx", ".mts", ".cts",
			".js", ".jsx", ".mjs", ".cjs",
		},
		LanguageIDs: map[string]string{
			".ts":  "typescript",
			".tsx": "typescriptreact",
			".mts": "typescript",
			".cts": "typescript",
			".js":  "javascript",
			".jsx": "javascriptreact",
			".mjs": "javascript",
			".cjs": "javascript",
		},
		Priority:    6,
		Daemon:      true,
		MaxParallel: 10,
	},
	{
		Name:       "pyright",
		Command:    "pyright-langserver",
		Args:       []string{"--stdio"},
		Languages:  []string{"python"},
		Extensions: []string{".py", ".pyi"},
		LanguageIDs: map[string]string{
			".py":  "python",
			".pyi": "python",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 10,
		AlternativeCommands: []ServerAlt{
			// jedi-language-server / pylsp are common alternates.
			{Command: "jedi-language-server"},
			{Command: "pylsp"},
		},
	},
	{
		// pyrefly is Meta's Rust-based Python type checker (the Pyre
		// successor). Offered at a lower precedence than pyright so the
		// default routing is unchanged for anyone who has both — a user
		// opts in by raising its priority in .gortex.yaml.
		Name:       "pyrefly",
		Command:    "pyrefly",
		Args:       []string{"lsp"},
		Languages:  []string{"python"},
		Extensions: []string{".py", ".pyi"},
		LanguageIDs: map[string]string{
			".py":  "python",
			".pyi": "python",
		},
		Priority:    6,
		Daemon:      true,
		MaxParallel: 10,
	},
	{
		Name:       "rust-analyzer",
		Command:    "rust-analyzer",
		Languages:  []string{"rust"},
		Extensions: []string{".rs"},
		LanguageIDs: map[string]string{
			".rs": "rust",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 8,
	},
	{
		Name:    "clangd",
		Command: "clangd",
		// `--background-index` keeps a project-wide symbol index hot in
		// the daemon, which is essential for type-hierarchy precision in
		// large C++ trees. `--header-insertion=never` avoids tactical
		// edits when we only want graph signal.
		Args:      []string{"--background-index", "--header-insertion=never"},
		Languages: []string{"c", "cpp", "objc", "objcpp"},
		Extensions: []string{
			".c", ".h",
			".cc", ".cpp", ".cxx", ".c++",
			".hh", ".hpp", ".hxx", ".h++",
			".m", ".mm",
		},
		LanguageIDs: map[string]string{
			".c":   "c",
			".h":   "c",
			".cc":  "cpp",
			".cpp": "cpp",
			".cxx": "cpp",
			".c++": "cpp",
			".hh":  "cpp",
			".hpp": "cpp",
			".hxx": "cpp",
			".h++": "cpp",
			".m":   "objective-c",
			".mm":  "objective-cpp",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 6,
	},
	{
		Name:       "jdtls",
		Command:    "jdtls",
		Languages:  []string{"java"},
		Extensions: []string{".java"},
		LanguageIDs: map[string]string{
			".java": "java",
		},
		Priority:    6, // jdtls is heavyweight; lower priority than scip-java.
		Daemon:      true,
		MaxParallel: 6,
	},
	{
		Name:       "kotlin-language-server",
		Command:    "kotlin-language-server",
		Languages:  []string{"kotlin"},
		Extensions: []string{".kt", ".kts"},
		LanguageIDs: map[string]string{
			".kt":  "kotlin",
			".kts": "kotlin",
		},
		Priority:    6,
		Daemon:      true,
		MaxParallel: 6,
	},
	{
		Name:       "omnisharp",
		Command:    "omnisharp",
		Args:       []string{"-lsp"},
		Languages:  []string{"csharp"},
		Extensions: []string{".cs"},
		LanguageIDs: map[string]string{
			".cs": "csharp",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 6,
	},
	{
		Name:       "ruby-lsp",
		Command:    "ruby-lsp",
		Languages:  []string{"ruby"},
		Extensions: []string{".rb", ".rake"},
		LanguageIDs: map[string]string{
			".rb":   "ruby",
			".rake": "ruby",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 6,
		AlternativeCommands: []ServerAlt{
			{Command: "solargraph", Args: []string{"stdio"}},
		},
	},
	{
		Name:       "phpactor",
		Command:    "phpactor",
		Args:       []string{"language-server"},
		Languages:  []string{"php"},
		Extensions: []string{".php"},
		LanguageIDs: map[string]string{
			".php": "php",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 6,
		AlternativeCommands: []ServerAlt{
			// intelephense ships as a Node CLI.
			{Command: "intelephense", Args: []string{"--stdio"}},
		},
	},
	{
		Name:       "lua-language-server",
		Command:    "lua-language-server",
		Languages:  []string{"lua"},
		Extensions: []string{".lua"},
		LanguageIDs: map[string]string{
			".lua": "lua",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 4,
	},
	{
		Name:       "sourcekit-lsp",
		Command:    "sourcekit-lsp",
		Languages:  []string{"swift"},
		Extensions: []string{".swift"},
		LanguageIDs: map[string]string{
			".swift": "swift",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 6,
	},
	{
		Name:       "haskell-language-server",
		Command:    "haskell-language-server-wrapper",
		Args:       []string{"--lsp"},
		Languages:  []string{"haskell"},
		Extensions: []string{".hs", ".lhs"},
		LanguageIDs: map[string]string{
			".hs":  "haskell",
			".lhs": "haskell",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 4,
	},
	{
		Name:       "elixir-ls",
		Command:    "elixir-ls",
		Languages:  []string{"elixir"},
		Extensions: []string{".ex", ".exs"},
		LanguageIDs: map[string]string{
			".ex":  "elixir",
			".exs": "elixir",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 4,
	},
	{
		Name:       "ocamllsp",
		Command:    "ocamllsp",
		Languages:  []string{"ocaml"},
		Extensions: []string{".ml", ".mli"},
		LanguageIDs: map[string]string{
			".ml":  "ocaml",
			".mli": "ocaml",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 4,
	},
	{
		Name:       "zls",
		Command:    "zls",
		Languages:  []string{"zig"},
		Extensions: []string{".zig", ".zon"},
		LanguageIDs: map[string]string{
			".zig": "zig",
			".zon": "zig",
		},
		Priority:    5,
		Daemon:      true,
		MaxParallel: 4,
	},
	{
		Name:       "terraform-ls",
		Command:    "terraform-ls",
		Args:       []string{"serve"},
		Languages:  []string{"hcl"},
		Extensions: []string{".tf", ".tfvars"},
		LanguageIDs: map[string]string{
			".tf":     "terraform",
			".tfvars": "terraform-vars",
		},
		Priority:    6,
		Daemon:      true,
		MaxParallel: 4,
	},
	{
		Name:       "yaml-language-server",
		Command:    "yaml-language-server",
		Args:       []string{"--stdio"},
		Languages:  []string{"yaml"},
		Extensions: []string{".yaml", ".yml"},
		LanguageIDs: map[string]string{
			".yaml": "yaml",
			".yml":  "yaml",
		},
		Priority:    7,
		Daemon:      true,
		MaxParallel: 4,
	},
	{
		Name:       "vscode-json-language-server",
		Command:    "vscode-json-language-server",
		Args:       []string{"--stdio"},
		Languages:  []string{"json"},
		Extensions: []string{".json", ".jsonc"},
		LanguageIDs: map[string]string{
			".json":  "json",
			".jsonc": "jsonc",
		},
		Priority:    7,
		Daemon:      true,
		MaxParallel: 4,
	},
	{
		Name:       "bash-language-server",
		Command:    "bash-language-server",
		Args:       []string{"start"},
		Languages:  []string{"bash"},
		Extensions: []string{".sh", ".bash"},
		LanguageIDs: map[string]string{
			".sh":   "shellscript",
			".bash": "shellscript",
		},
		Priority:    7,
		Daemon:      true,
		MaxParallel: 4,
	},
}

// extToSpec resolves a file extension (with leading dot, lower case) to
// its preferred LSP spec. Built once at init from `Servers`.
var extToSpec map[string]*ServerSpec

// nameToSpec resolves a server name (e.g. "rust-analyzer") to its spec.
var nameToSpec map[string]*ServerSpec

func init() {
	extToSpec = make(map[string]*ServerSpec, 64)
	nameToSpec = make(map[string]*ServerSpec, len(Servers))
	for i := range Servers {
		spec := &Servers[i]
		nameToSpec[spec.Name] = spec
		for _, ext := range spec.Extensions {
			lower := strings.ToLower(ext)
			// First registration wins; entries earlier in `Servers`
			// take precedence when extensions overlap. (Used to keep
			// gopls authoritative for `.go` even if a future entry
			// claims it.)
			if _, exists := extToSpec[lower]; !exists {
				extToSpec[lower] = spec
			}
		}
	}

	// Contribute every known LSP spec to semantic.DefaultConfig()
	// so the daemon picks them up automatically when the binary is
	// on PATH. Each spec becomes a disabled-only-when-binary-missing
	// ProviderConfig at the spec's default priority.
	semantic.RegisterDefaultProviders(func() []semantic.ProviderConfig {
		out := make([]semantic.ProviderConfig, 0, len(Servers))
		for i := range Servers {
			s := &Servers[i]
			out = append(out, semantic.ProviderConfig{
				Name:        s.Name,
				Command:     s.Command,
				Args:        s.Args,
				Languages:   s.Languages,
				Priority:    s.Priority,
				Enabled:     true,
				Daemon:      s.Daemon,
				MaxParallel: s.MaxParallel,
			})
		}
		return out
	})
}

// SpecForExtension returns the ServerSpec preferred for the given file
// extension (with or without leading dot). Returns nil when no spec
// covers the extension.
func SpecForExtension(ext string) *ServerSpec {
	if ext == "" {
		return nil
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return extToSpec[strings.ToLower(ext)]
}

// SpecForPath returns the ServerSpec covering the file's extension.
// Returns nil when no spec covers it.
func SpecForPath(path string) *ServerSpec {
	return SpecForExtension(filepath.Ext(path))
}

// SpecByName returns the ServerSpec with the given name, or nil.
func SpecByName(name string) *ServerSpec {
	return nameToSpec[name]
}

// SpecWithOverrides returns a copy of base with non-empty command /
// args / env overrides applied — the path by which .gortex.yaml tunes
// a heavyweight server (notably pinning a JRE + jdtls launcher args).
// base is never mutated, so the package-global Servers slice stays
// pristine across daemons and tests.
func SpecWithOverrides(base *ServerSpec, command string, args, env []string) *ServerSpec {
	if base == nil {
		return nil
	}
	cp := *base
	if command != "" {
		cp.Command = command
	}
	if len(args) > 0 {
		cp.Args = append([]string(nil), args...)
	}
	if len(env) > 0 {
		cp.Env = append([]string(nil), env...)
	}
	return &cp
}

// LanguageIDForPath returns the LSP `languageId` for the given file
// path using the preferred spec's mapping. Returns an empty string
// when no spec covers the extension.
func LanguageIDForPath(path string) string {
	spec := SpecForPath(path)
	if spec == nil {
		return ""
	}
	ext := strings.ToLower(filepath.Ext(path))
	if id, ok := spec.LanguageIDs[ext]; ok {
		return id
	}
	if len(spec.Languages) > 0 {
		return spec.Languages[0]
	}
	return ""
}

// AllSpecs returns the canonical list of known specs in registration
// order.
func AllSpecs() []*ServerSpec {
	out := make([]*ServerSpec, 0, len(Servers))
	for i := range Servers {
		out = append(out, &Servers[i])
	}
	return out
}
