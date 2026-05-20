package search

import (
	"math"
	"sort"
	"sync"
)

// BM25Backend is a custom in-memory inverted index with BM25 scoring.
// Optimal for repos up to ~50k symbols. Zero external dependencies.
type BM25Backend struct {
	mu       sync.RWMutex
	docs     map[string]*doc      // docID -> document
	inverted map[string][]posting // term -> postings list
	totalLen int                  // sum of all doc lengths (for avgLen)
	bigrams  *bigramIndex         // side index for typo-tolerant fallback recall
}

type doc struct {
	id    string
	len   int
	terms map[string]int // term -> frequency in this doc
}

type posting struct {
	docID string
	freq  int
}

// BM25 parameters.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// SizeBytes is a rough memory estimate for the BM25 in-memory index:
// every document stores an ID + term-frequency map, and every term in
// the inverted index carries a postings list. The per-doc and per-term
// constants are calibrated against live indexes and land within ~25%
// of actual heap delta.
func (b *BM25Backend) SizeBytes() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var bytes uint64
	for _, d := range b.docs {
		// doc struct + id string + terms map header
		bytes += 96 + uint64(len(d.id)) + 48
		// Each term entry: key string header + ~8 bytes for the int frequency.
		for term := range d.terms {
			bytes += uint64(len(term)) + 24
		}
	}
	for term, postings := range b.inverted {
		// term string + slice header + postings
		bytes += uint64(len(term)) + 24
		bytes += uint64(len(postings)) * 32 // docID string hdr + freq int + ptr
	}
	return bytes
}

// NewBM25 creates a new BM25 search backend. The bigram side index for
// typo-tolerant rescue is built only when GORTEX_BIGRAM_TYPOS is set;
// leaving it nil is cheap — every bigram method is nil-safe and returns
// the zero-cost branch, so the engine's typo-rescue tier becomes a no-op
// automatically.
func NewBM25() *BM25Backend {
	b := &BM25Backend{
		docs:     make(map[string]*doc),
		inverted: make(map[string][]posting),
	}
	if bigramIndexEnabled() {
		b.bigrams = newBigramIndex()
	}
	return b
}

func (b *BM25Backend) Add(id string, fields ...string) {
	// Tokenize all fields together.
	var allTokens []string
	for _, f := range fields {
		allTokens = append(allTokens, Tokenize(f)...)
	}

	// Stopword-filter + Porter-stem the posting tokens. Search()
	// runs the same normalization, so stemmed postings are always probed
	// with stemmed query terms. The bigram side index keeps the raw
	// tokens — its typo rescue bigramizes the raw query string, so
	// raw-against-raw stays consistent there.
	ftsTokens := NormalizeFTSTokens(allTokens)

	termFreq := make(map[string]int)
	for _, t := range ftsTokens {
		termFreq[t]++
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// Remove old version if exists.
	b.removeLocked(id)

	d := &doc{
		id:    id,
		len:   len(ftsTokens),
		terms: termFreq,
	}
	b.docs[id] = d
	b.totalLen += d.len

	for term, freq := range termFreq {
		b.inverted[term] = append(b.inverted[term], posting{id, freq})
	}

	// Keep the bigram side index in lockstep — same token set, same doc ID.
	b.bigrams.Add(id, allTokens)
}

func (b *BM25Backend) Remove(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.removeLocked(id)
}

func (b *BM25Backend) removeLocked(id string) {
	d, ok := b.docs[id]
	if !ok {
		return
	}

	b.totalLen -= d.len

	// Remove from inverted index.
	for term := range d.terms {
		postings := b.inverted[term]
		for i, p := range postings {
			if p.docID == id {
				b.inverted[term] = append(postings[:i], postings[i+1:]...)
				break
			}
		}
		if len(b.inverted[term]) == 0 {
			delete(b.inverted, term)
		}
	}

	delete(b.docs, id)
	b.bigrams.Remove(id)
}

func (b *BM25Backend) Search(query string, limit int) []SearchResult {
	queryTokens := NormalizeFTSTokens(TokenizeQuery(query))
	if len(queryTokens) == 0 {
		return nil
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	docCount := len(b.docs)
	if docCount == 0 {
		return nil
	}

	avgLen := float64(b.totalLen) / float64(docCount)
	scores := make(map[string]float64)

	for _, term := range queryTokens {
		postings, ok := b.inverted[term]
		if !ok {
			continue
		}
		df := float64(len(postings))
		idf := math.Log((float64(docCount)-df+0.5)/(df+0.5) + 1)

		for _, p := range postings {
			d := b.docs[p.docID]
			if d == nil {
				continue
			}
			tf := float64(p.freq)
			dl := float64(d.len)
			score := idf * (tf * (bm25K1 + 1)) / (tf + bm25K1*(1-bm25B+bm25B*dl/avgLen))
			scores[p.docID] += score
		}
	}

	if len(scores) == 0 {
		// The engine layer has its own fallback chain — exact-name match
		// then substring contains — that handles queries like "NewServer"
		// which the backend's camelCase-split tokenization misses. We stay
		// strict here so those higher-precision fallbacks can run; typo
		// rescue via bigram overlap belongs one level up, after those.
		return nil
	}

	// Sort by score descending.
	type scored struct {
		id    string
		score float64
	}
	results := make([]scored, 0, len(scores))
	for id, score := range scores {
		results = append(results, scored{id, score})
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		// Tie-break on doc ID so an equal-score run ships in a stable
		// order across calls — Go's map iteration is otherwise random.
		return results[i].id < results[j].id
	})

	if len(results) > limit {
		results = results[:limit]
	}

	out := make([]SearchResult, len(results))
	for i, r := range results {
		out[i] = SearchResult{ID: r.id, Score: r.score}
	}
	return out
}

// BigramCandidates exposes the bigram-overlap list for explicit typo-mode
// callers. minOverlap gates how similar a doc must be — the caller picks
// the strictness.
func (b *BM25Backend) BigramCandidates(query string, minOverlap int) []string {
	if b.bigrams == nil {
		return nil
	}
	return b.bigrams.Candidates(query, minOverlap)
}

func (b *BM25Backend) Count() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.docs)
}

func (b *BM25Backend) Close() {
	// No-op for in-memory backend.
}
