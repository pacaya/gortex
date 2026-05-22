package languages

import (
	"path/filepath"
	"strings"
	"unsafe"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/forest"
)

// defaultGrammarSymbol derives the tree-sitter C entry-point symbol
// for a language: `tree_sitter_<language>` with every non-alphanumeric
// character folded to an underscore — the convention upstream grammars
// follow (tree-sitter-c-sharp exports `tree_sitter_c_sharp`).
func defaultGrammarSymbol(language string) string {
	var b strings.Builder
	b.WriteString("tree_sitter_")
	for _, r := range strings.ToLower(language) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// normalizeGrammarExtensions trims each extension and ensures a
// leading dot, dropping blanks. Case is preserved — the registry's
// extension map is case-sensitive (it distinguishes .R from .r).
func normalizeGrammarExtensions(exts []string) []string {
	out := make([]string, 0, len(exts))
	for _, e := range exts {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		out = append(out, e)
	}
	return out
}

// firstClaimedExtension returns the first extension already mapped to
// a registered extractor, or "" when every extension is free.
func firstClaimedExtension(reg *parser.Registry, exts []string) string {
	for _, e := range exts {
		if _, exists := reg.GetByExtension(e); exists {
			return e
		}
	}
	return ""
}

// RegisterCustomGrammars loads every user-declared tree-sitter grammar
// and registers it as a signature-only forest extractor. A grammar
// whose name or any extension collides with a built-in extractor is
// skipped — built-in depth wins — as is one that fails to validate or
// load, each with a logged warning so a typo or a missing library
// never aborts startup. A relative library path resolves against the
// working directory.
func RegisterCustomGrammars(reg *parser.Registry, specs []config.GrammarSpec, log *zap.Logger) {
	if reg == nil {
		return
	}
	if log == nil {
		log = zap.NewNop()
	}
	for _, spec := range specs {
		lang := strings.TrimSpace(spec.Language)
		exts := normalizeGrammarExtensions(spec.Extensions)
		if lang == "" || strings.TrimSpace(spec.Library) == "" || len(exts) == 0 {
			log.Warn("custom grammar: skipped — language, library and extensions are all required",
				zap.String("language", spec.Language), zap.String("library", spec.Library))
			continue
		}
		if _, exists := reg.GetByLanguage(lang); exists {
			log.Warn("custom grammar: skipped — language already registered by a built-in extractor",
				zap.String("language", lang))
			continue
		}
		if conflict := firstClaimedExtension(reg, exts); conflict != "" {
			log.Warn("custom grammar: skipped — extension already claimed by a built-in extractor",
				zap.String("language", lang), zap.String("extension", conflict))
			continue
		}

		libPath := strings.TrimSpace(spec.Library)
		if !filepath.IsAbs(libPath) {
			if abs, err := filepath.Abs(libPath); err == nil {
				libPath = abs
			}
		}
		symbol := strings.TrimSpace(spec.Symbol)
		if symbol == "" {
			symbol = defaultGrammarSymbol(lang)
		}

		langPtr, err := loadGrammarLanguage(libPath, symbol)
		if err != nil {
			log.Warn("custom grammar: failed to load — skipped",
				zap.String("language", lang), zap.String("library", libPath),
				zap.String("symbol", symbol), zap.Error(err))
			continue
		}
		getLang := func() unsafe.Pointer { return langPtr }
		reg.Register(forest.New(lang, exts, getLang, nil))
		log.Info("custom grammar registered",
			zap.String("language", lang), zap.Strings("extensions", exts))
	}
}
