// Package clones implements MinHash + LSH near-duplicate ("clone")
// detection over function and method bodies.
//
// The pipeline has three stages, mirroring the classic SourcererCC /
// Moss design adapted to a graph-native model:
//
//  1. Tokenise — a generic, language-agnostic lexer reduces a function
//     body to a normalised token stream. Identifiers are collapsed to a
//     single placeholder so renamed-variable copies (Type-2 clones)
//     still match; a small universal keyword set is kept verbatim so
//     the control-flow skeleton survives normalisation.
//  2. Signature — the token stream is shingled into k-grams and hashed
//     into a fixed-width 64-slot MinHash signature. Signature agreement
//     count over the 64 slots is an unbiased estimator of the Jaccard
//     similarity of the two shingle sets.
//  3. LSH — signatures are banded so that only function pairs colliding
//     in at least one band become candidate pairs, turning the O(n²)
//     all-pairs comparison into a near-linear bucket scan (see lsh.go).
//
// The signature is computed once per function at index time and stored
// (base64-encoded) on the node's Meta so the graph-wide LSH pass is a
// pure graph walk with no file IO — which also makes it correct under
// incremental reindex and safe across multi-repo graphs.
package clones

import (
	"encoding/base64"
	"encoding/binary"
	"hash/fnv"
)

const (
	// NumHashes is the MinHash signature width. 64 slots gives a
	// standard error of ~1/sqrt(64) ≈ 12.5% on the Jaccard estimate —
	// the value the F9 spec calls for.
	NumHashes = 64
	// Bands / Rows partition the signature for LSH banding. Bands*Rows
	// must equal NumHashes. With 16 bands of 4 rows the approximate LSH
	// threshold (1/Bands)^(1/Rows) ≈ 0.5, so candidate pairs surface at
	// ~0.5 similarity and the exact Jaccard filter trims from there.
	Bands = 16
	Rows  = 4
	// ShingleK is the token k-gram width. 3 is the usual sweet spot:
	// large enough that incidental token co-occurrence is rare, small
	// enough that a few edited lines don't wipe out every shingle.
	ShingleK = 3
	// MinTokens is the smallest normalised token count a body must have
	// to be eligible for clone detection. Below this, functions are
	// dominated by boilerplate (a single return, a one-line delegation)
	// and produce nothing but noise.
	MinTokens = 24
	// DefaultThreshold is the Jaccard similarity at or above which a
	// candidate pair is reported as a clone. 0.82 keeps Type-1 (exact)
	// and Type-2 (renamed) clones while rejecting merely structurally
	// similar functions.
	DefaultThreshold = 0.82
)

func init() {
	if Bands*Rows != NumHashes {
		panic("clones: Bands*Rows must equal NumHashes")
	}
}

// Signature is a fixed-width MinHash signature for one function body.
type Signature [NumHashes]uint32

// minHashPrime is the largest prime below 2^32; the universal hash
// family (a*x + b) mod prime stays within uint32 once x is pre-reduced.
const minHashPrime = 4294967291

// hashParams holds the 64 (a, b) coefficient pairs for the universal
// hash family. They are generated deterministically so a signature is
// reproducible across processes and across daemon restarts.
var hashParams = func() [NumHashes][2]uint64 {
	var p [NumHashes][2]uint64
	// xorshift64* seeded with a fixed constant — deterministic, no
	// dependency on math/rand's global state.
	state := uint64(0x9E3779B97F4A7C15)
	next := func() uint64 {
		state ^= state >> 12
		state ^= state << 25
		state ^= state >> 27
		return state * 0x2545F4914F6CDD1D
	}
	for i := range p {
		// a must be non-zero mod prime; b is unconstrained.
		a := next()%(minHashPrime-1) + 1
		b := next() % minHashPrime
		p[i] = [2]uint64{a, b}
	}
	return p
}()

// ComputeSignature tokenises a function body and returns its MinHash
// signature. The bool result is false when the body has fewer than
// MinTokens normalised tokens, in which case the caller should skip
// clone detection for that symbol entirely.
//
// Wraps ComputeSignatureWithTokens; callers that need the token count
// for length-stratified LSH should call the *WithTokens variant
// directly.
func ComputeSignature(body string) (Signature, bool) {
	sig, _, ok := ComputeSignatureWithTokens(body)
	return sig, ok
}

// ComputeSignatureWithTokens is ComputeSignature plus the normalised-
// token count of the body. The token count is what
// DetectPairsStratifiedWithStats uses to length-bucket items: pairs at
// the 0.82 Jaccard threshold differ in token count by at most ~22%
// (Jaccard ≤ min/max ⇒ max ≤ min/0.82), so items in non-adjacent
// length classes cannot be real clones and skipping their cross-class
// comparisons is exact, not approximate.
func ComputeSignatureWithTokens(body string) (Signature, int, bool) {
	tokens := Tokenize(body)
	if len(tokens) < MinTokens {
		return Signature{}, 0, false
	}
	shingles := shingleSet(tokens)
	if len(shingles) == 0 {
		return Signature{}, 0, false
	}

	var sig Signature
	for i := range sig {
		sig[i] = ^uint32(0)
	}
	a := hashParams
	for sh := range shingles {
		// Pre-reduce the 64-bit shingle hash so a*x stays under 2^64.
		x := sh % minHashPrime
		for i := range NumHashes {
			h := uint32((a[i][0]*x + a[i][1]) % minHashPrime)
			if h < sig[i] {
				sig[i] = h
			}
		}
	}
	return sig, len(tokens), true
}

// shingleSet returns the deduplicated set of k-gram hashes for a token
// stream. When the stream is shorter than ShingleK the whole stream is
// treated as a single shingle so very short (but still ≥ MinTokens)
// bodies are not dropped.
func shingleSet(tokens []string) map[uint64]struct{} {
	set := make(map[uint64]struct{})
	if len(tokens) < ShingleK {
		h := fnv.New64a()
		for _, t := range tokens {
			h.Write([]byte(t))
			h.Write([]byte{0})
		}
		set[h.Sum64()] = struct{}{}
		return set
	}
	for i := 0; i+ShingleK <= len(tokens); i++ {
		h := fnv.New64a()
		for j := range ShingleK {
			h.Write([]byte(tokens[i+j]))
			h.Write([]byte{0})
		}
		set[h.Sum64()] = struct{}{}
	}
	return set
}

// EstimateJaccard returns the estimated Jaccard similarity of the two
// signatures — the fraction of the 64 MinHash slots that agree.
func EstimateJaccard(a, b Signature) float64 {
	agree := 0
	for i := range NumHashes {
		if a[i] == b[i] {
			agree++
		}
	}
	return float64(agree) / float64(NumHashes)
}

// EncodeSignature serialises a signature to a base64 string suitable
// for storage on a graph node's Meta map (JSON / gob friendly).
func EncodeSignature(sig Signature) string {
	buf := make([]byte, NumHashes*4)
	for i, v := range sig {
		binary.LittleEndian.PutUint32(buf[i*4:], v)
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// DecodeSignature reverses EncodeSignature. The bool result is false
// when the input is not a well-formed encoded signature.
func DecodeSignature(s string) (Signature, bool) {
	var sig Signature
	if s == "" {
		return sig, false
	}
	buf, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(buf) != NumHashes*4 {
		return sig, false
	}
	for i := range sig {
		sig[i] = binary.LittleEndian.Uint32(buf[i*4:])
	}
	return sig, true
}
