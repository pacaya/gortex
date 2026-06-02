package search

import (
	"sort"
	"strings"
	"unicode"

	"github.com/zzet/gortex/internal/graph"
)

// AutoConcepts is a per-repository, LLM-free vocabulary of multi-word
// concepts mined from the symbol names in the graph. Where the curated
// EquivalenceTable bridges universal software vocabulary, AutoConcepts
// captures domain phrases specific to one codebase -- when many
// symbols pair the words "blast" and "radius" (handleBlastRadius,
// blastRadiusOf, BlastRadiusReport), "blast radius" is a concept, and
// a query for either word should also pull symbols built from the
// other.
//
// The vocabulary is built once per index pass (Build) and is cheap
// enough to recompute on every reindex: one tokenizing pass over node
// names plus a bounded co-occurrence count.
type AutoConcepts struct {
	// related maps a token to the set of tokens it concept-co-occurs
	// with strongly enough to be treated as siblings.
	related map[string][]string

	// vocab is the set of node-label tokens kept after the
	// document-frequency / vocabulary-cap pass -- the words that
	// actually appear in this repo's symbol names. Exposed via
	// InVocabulary so the query-expansion path can anchor an LLM's
	// freely-invented synonyms to terms the corpus can actually match.
	vocab map[string]struct{}
}

// Auto-concept mining bounds. The graph is ~31k nodes; these caps keep
// the co-occurrence map and the per-token sibling lists from growing
// without limit on a large monorepo.
const (
	// autoConceptMinPairCount is the minimum number of distinct symbol
	// names a token pair must co-occur in before it counts as a
	// concept. Two is too noisy; three filters one-off coincidences.
	autoConceptMinPairCount = 3
	// autoConceptMinTokenLen drops sub-3-char tokens ("of", "id") --
	// they co-occur with everything and carry no concept signal.
	autoConceptMinTokenLen = 3
	// autoConceptMaxSiblings caps how many siblings one token keeps,
	// strongest-first, so a hub word can't expand a query unboundedly.
	autoConceptMaxSiblings = 6
	// autoConceptMaxTokens caps the distinct-token vocabulary. Beyond
	// this the rarest tokens are dropped before pair counting.
	autoConceptMaxTokens = 4000
)

// autoConceptStopTokens are generic word fragments that pair with
// nearly every symbol name and would otherwise dominate the concept
// map. Mirrors expansionStoplist in spirit -- kept short.
var autoConceptStopTokens = map[string]struct{}{
	"get": {}, "set": {}, "new": {}, "is": {}, "to": {}, "of": {},
	"the": {}, "for": {}, "on": {}, "by": {}, "with": {}, "from": {},
	"handle": {}, "handler": {}, "func": {}, "fn": {}, "do": {},
	"run": {}, "make": {}, "init": {}, "test": {}, "impl": {},
	"data": {}, "value": {}, "item": {}, "result": {}, "error": {},
	"err": {}, "ctx": {}, "context": {}, "opts": {}, "options": {},
	"id": {}, "name": {}, "type": {}, "kind": {}, "list": {},
}

// BuildAutoConcepts mines the per-repo concept vocabulary from a
// graph. Only named code symbols (functions, methods, types,
// interfaces, constants, variables) contribute -- structural and
// pseudo nodes (files, imports, params) are skipped. A nil or empty
// graph yields an empty, safe-to-query AutoConcepts.
func BuildAutoConcepts(g graph.Reader) *AutoConcepts {
	ac := &AutoConcepts{related: map[string][]string{}, vocab: map[string]struct{}{}}
	if g == nil {
		return ac
	}

	// Pass 1: tokenize every eligible symbol name into its multi-word
	// component tokens, tally per-token document frequency, and keep
	// the per-symbol token sets for the pair-counting pass.
	docFreq := map[string]int{}
	var docs [][]string
	for _, n := range g.AllNodes() {
		if !autoConceptEligible(n.Kind) {
			continue
		}
		toks := autoConceptTokens(n.Name)
		if len(toks) < 2 {
			continue
		}
		docs = append(docs, toks)
		seen := map[string]struct{}{}
		for _, t := range toks {
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
			docFreq[t]++
		}
	}

	// Bound the vocabulary: when there are more distinct tokens than
	// the cap, keep the most frequent ones (rarest tokens cannot reach
	// the pair-count threshold anyway).
	keep := vocabularyCap(docFreq, autoConceptMaxTokens)
	// The kept token set is the repo's symbol-name vocabulary —
	// surface it so the expansion path can anchor LLM synonyms to
	// words the corpus can match. Share the map directly: keep is not
	// mutated after this point.
	ac.vocab = keep

	// Pass 2: count, per unordered token pair, how many symbol names
	// they co-occur in. Both tokens must be in the kept vocabulary.
	pairCount := map[[2]string]int{}
	for _, toks := range docs {
		uniq := dedupTokens(toks, keep)
		for i := 0; i < len(uniq); i++ {
			for j := i + 1; j < len(uniq); j++ {
				a, b := uniq[i], uniq[j]
				if a > b {
					a, b = b, a
				}
				pairCount[[2]string{a, b}]++
			}
		}
	}

	// Build the sibling lists from pairs that clear the threshold.
	type weighted struct {
		token string
		count int
	}
	siblings := map[string][]weighted{}
	for pair, c := range pairCount {
		if c < autoConceptMinPairCount {
			continue
		}
		siblings[pair[0]] = append(siblings[pair[0]], weighted{pair[1], c})
		siblings[pair[1]] = append(siblings[pair[1]], weighted{pair[0], c})
	}
	for tok, ws := range siblings {
		sort.Slice(ws, func(i, j int) bool {
			if ws[i].count != ws[j].count {
				return ws[i].count > ws[j].count
			}
			return ws[i].token < ws[j].token
		})
		if len(ws) > autoConceptMaxSiblings {
			ws = ws[:autoConceptMaxSiblings]
		}
		out := make([]string, 0, len(ws))
		for _, w := range ws {
			out = append(out, w.token)
		}
		ac.related[tok] = out
	}
	return ac
}

// Expand returns the auto-mined concept siblings of token -- tokens
// that co-occur with it strongly across this repo's symbol names.
// Returns nil when the token has no mined siblings. The token itself
// is never included. Lookup is case-insensitive.
func (ac *AutoConcepts) Expand(token string) []string {
	if ac == nil {
		return nil
	}
	tok := strings.ToLower(strings.TrimSpace(token))
	if tok == "" {
		return nil
	}
	sib := ac.related[tok]
	if len(sib) == 0 {
		return nil
	}
	out := make([]string, len(sib))
	copy(out, sib)
	return out
}

// TokenCount reports the number of tokens that have at least one
// mined sibling. Used by tests and diagnostics.
func (ac *AutoConcepts) TokenCount() int {
	if ac == nil {
		return 0
	}
	return len(ac.related)
}

// InVocabulary reports whether token appears in this repo's mined
// symbol-name vocabulary. Lookup is case-insensitive. A nil
// AutoConcepts, or one mined from an empty graph, has an empty
// vocabulary and returns false for everything -- callers MUST treat an
// empty vocabulary as "no anchor available" and degrade to
// unconstrained behaviour rather than filtering every term away.
func (ac *AutoConcepts) InVocabulary(token string) bool {
	if ac == nil || len(ac.vocab) == 0 {
		return false
	}
	tok := strings.ToLower(strings.TrimSpace(token))
	if tok == "" {
		return false
	}
	_, ok := ac.vocab[tok]
	return ok
}

// VocabularySize reports the number of distinct tokens in the mined
// symbol-name vocabulary. Used by the expansion path to decide whether
// a vocabulary anchor is available at all (size 0 => degrade to
// unconstrained) and by tests / diagnostics.
func (ac *AutoConcepts) VocabularySize() int {
	if ac == nil {
		return 0
	}
	return len(ac.vocab)
}

// autoConceptEligible reports whether a node kind contributes its
// name to concept mining. Only genuine code symbols do.
func autoConceptEligible(k graph.NodeKind) bool {
	switch k {
	case graph.KindFunction, graph.KindMethod, graph.KindType,
		graph.KindInterface, graph.KindConstant, graph.KindVariable:
		return true
	default:
		return false
	}
}

// autoConceptTokens splits a symbol name into lowercased component
// tokens on camelCase, snake_case, and digit boundaries, dropping
// stop-tokens and sub-threshold-length fragments.
func autoConceptTokens(name string) []string {
	var (
		out []string
		cur strings.Builder
	)
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		w := strings.ToLower(cur.String())
		cur.Reset()
		if len(w) < autoConceptMinTokenLen {
			return
		}
		if _, stop := autoConceptStopTokens[w]; stop {
			return
		}
		out = append(out, w)
	}
	var prev rune
	for i, r := range name {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if i > 0 {
				if unicode.IsUpper(r) && unicode.IsLower(prev) {
					flush()
				} else if unicode.IsDigit(r) != unicode.IsDigit(prev) && cur.Len() > 0 {
					flush()
				}
			}
			cur.WriteRune(r)
		default:
			flush()
		}
		prev = r
	}
	flush()
	return out
}

// vocabularyCap returns the set of tokens to keep when the distinct
// token count exceeds max -- the `max` most frequent tokens. When the
// count is within budget every token is kept.
func vocabularyCap(docFreq map[string]int, max int) map[string]struct{} {
	keep := make(map[string]struct{}, len(docFreq))
	if len(docFreq) <= max {
		for t := range docFreq {
			keep[t] = struct{}{}
		}
		return keep
	}
	type tf struct {
		token string
		freq  int
	}
	all := make([]tf, 0, len(docFreq))
	for t, f := range docFreq {
		all = append(all, tf{t, f})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].freq != all[j].freq {
			return all[i].freq > all[j].freq
		}
		return all[i].token < all[j].token
	})
	for _, e := range all[:max] {
		keep[e.token] = struct{}{}
	}
	return keep
}

// dedupTokens returns the distinct tokens of toks that are in the
// kept vocabulary, preserving first-seen order.
func dedupTokens(toks []string, keep map[string]struct{}) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(toks))
	for _, t := range toks {
		if _, ok := keep[t]; !ok {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}
