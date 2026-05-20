package search

import (
	"os"
	"sort"
	"strings"
	"sync"
)

// bigramIndexEnabled reports whether the bigram side index should be
// built on Add/Remove and consulted by the engine's typo-rescue tier.
// Disabled by default — it adds measurable per-symbol indexing cost
// (bigramize + atomic-map writes per token) that only earns its keep
// when agents actually typo. Users who want typo-tolerant recall opt
// in via GORTEX_BIGRAM_TYPOS=1 (or true / yes / on). Read once at
// backend construction so the flag can't toggle mid-session.
func bigramIndexEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GORTEX_BIGRAM_TYPOS"))) {
	case "1", "true", "yes", "on", "y":
		return true
	}
	return false
}

// bigramIndex is an inverted index from bigram key → docID set, built
// alongside BM25's primary index. Its single purpose is typo-tolerant
// recall: when BM25 returns zero hits for a query, the caller can fall
// back to bigram overlap to find the nearest-matching symbols. The index
// tracks both consecutive bigrams (chars i, i+1) and skip-1 bigrams
// (chars i, i+2) — the latter is the FFF trick that makes "validat_e"
// transpositions match "validate".
type bigramIndex struct {
	mu sync.RWMutex
	// bigrams keys each bigram (hi<<8 | lo) to the set of docIDs that
	// contain it at least once in any indexed token. We use a set
	// (map[string]struct{}) rather than a slice so Remove is O(1) and the
	// per-bigram densities at compress time come from a simple len().
	bigrams map[uint16]map[string]struct{}
	// perDoc tracks the bigrams each doc contributed, for clean Remove.
	perDoc map[string][]uint16
}

func newBigramIndex() *bigramIndex {
	return &bigramIndex{
		bigrams: make(map[uint16]map[string]struct{}),
		perDoc:  make(map[string][]uint16),
	}
}

// bigramize yields every consecutive and skip-1 bigram key for one
// lowercase token. Non-ASCII bytes are silently skipped so the key space
// stays in uint16 (one byte each side) — good enough for symbol names
// which are almost universally ASCII in real codebases.
func bigramize(token string) []uint16 {
	b := []byte(strings.ToLower(token))
	if len(b) < 2 {
		return nil
	}
	out := make([]uint16, 0, 2*len(b))
	// Consecutive pairs.
	for i := 1; i < len(b); i++ {
		a, c := b[i-1], b[i]
		if a > 127 || c > 127 {
			continue
		}
		out = append(out, uint16(a)<<8|uint16(c))
	}
	// Skip-1 pairs — typo resilience for single-char substitutions and
	// transpositions. FFF encodes these in a separate column; we pool
	// them into the same key space which costs some density but halves
	// the index footprint.
	for i := 2; i < len(b); i++ {
		a, c := b[i-2], b[i]
		if a > 127 || c > 127 {
			continue
		}
		out = append(out, uint16(a)<<8|uint16(c))
	}
	return out
}

// Add indexes one doc under all bigrams found in any of its tokens.
// Called from BM25Backend.Add with the same tokens that feed the BM25
// posting lists, so the two indexes stay in lockstep without a second
// tokenization pass.
func (bi *bigramIndex) Add(docID string, tokens []string) {
	if bi == nil || docID == "" || len(tokens) == 0 {
		return
	}
	seen := make(map[uint16]struct{}, 16)
	for _, t := range tokens {
		for _, k := range bigramize(t) {
			seen[k] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return
	}

	bi.mu.Lock()
	defer bi.mu.Unlock()

	// Drop any prior presence before re-indexing so a re-Add doesn't leave
	// orphan bigrams from the old token set.
	bi.removeLocked(docID)

	keys := make([]uint16, 0, len(seen))
	for k := range seen {
		set, ok := bi.bigrams[k]
		if !ok {
			set = make(map[string]struct{})
			bi.bigrams[k] = set
		}
		set[docID] = struct{}{}
		keys = append(keys, k)
	}
	bi.perDoc[docID] = keys
}

// Remove deletes a doc from every bigram's set and clears its perDoc list.
func (bi *bigramIndex) Remove(docID string) {
	if bi == nil || docID == "" {
		return
	}
	bi.mu.Lock()
	defer bi.mu.Unlock()
	bi.removeLocked(docID)
}

func (bi *bigramIndex) removeLocked(docID string) {
	keys := bi.perDoc[docID]
	for _, k := range keys {
		if set, ok := bi.bigrams[k]; ok {
			delete(set, docID)
			if len(set) == 0 {
				delete(bi.bigrams, k)
			}
		}
	}
	delete(bi.perDoc, docID)
}

// Candidates returns docIDs whose token bigram set overlaps the query by
// at least minOverlap distinct bigrams. Density-filtered: bigrams that
// appear in <lowDocPct or >highDocPct of all docs are ignored — very
// rare bigrams are noise, very common ones add no signal. Same defaults
// FFF uses (~3% / 90%).
func (bi *bigramIndex) Candidates(query string, minOverlap int) []string {
	if bi == nil || query == "" {
		return nil
	}
	keys := bigramize(query)
	if len(keys) == 0 {
		return nil
	}
	if minOverlap < 1 {
		minOverlap = 1
	}

	bi.mu.RLock()
	defer bi.mu.RUnlock()

	total := len(bi.perDoc)
	if total == 0 {
		return nil
	}

	// Density thresholds.
	const (
		lowDocPct  = 3
		highDocPct = 90
	)
	loBound := (total * lowDocPct) / 100
	if loBound < 1 {
		loBound = 1
	}
	hiBound := (total * highDocPct) / 100
	if hiBound < 1 {
		hiBound = total
	}

	// Overlap count per candidate doc.
	overlap := make(map[string]int)
	for _, k := range keys {
		set, ok := bi.bigrams[k]
		if !ok {
			continue
		}
		if len(set) < loBound || len(set) > hiBound {
			continue
		}
		for docID := range set {
			overlap[docID]++
		}
	}

	// Collect candidates above threshold along with their overlap count,
	// then sort by overlap descending so the caller can take top-N by
	// similarity rather than by Go's random map-iteration order — the
	// latter buried the best match past rank 20 on typo'd exact queries.
	type cand struct {
		id    string
		count int
	}
	cands := make([]cand, 0, len(overlap))
	for id, c := range overlap {
		if c >= minOverlap {
			cands = append(cands, cand{id, c})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].count != cands[j].count {
			return cands[i].count > cands[j].count
		}
		// Equal-overlap candidates tie-break on ID for stable ordering.
		return cands[i].id < cands[j].id
	})
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.id
	}
	return out
}

// Size reports the number of distinct bigrams currently indexed.
// Exposed for tests and stats.
func (bi *bigramIndex) Size() int {
	if bi == nil {
		return 0
	}
	bi.mu.RLock()
	defer bi.mu.RUnlock()
	return len(bi.bigrams)
}
