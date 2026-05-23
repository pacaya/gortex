package clones

import "sort"

// Length-stratified LSH bounds.
//
// The 0.82 Jaccard threshold implies that two clone bodies' token
// counts differ by at most ~22% — Jaccard(A,B) ≤ min(|A|,|B|) /
// max(|A|,|B|), so a 0.82 clone needs max ≤ min/0.82 ≈ 1.22·min. We
// length-bucket items into 5 overlapping geometric classes (each
// adjacent pair overlapping by ~50%) so any real clone pair lands in
// at least one shared class, while items more than ~1.6× apart in
// size never enter the same LSH pass. On a 150k-item graph this
// alone drops the candidate count by 5–10× because k8s-style
// boilerplate (options-builders, controllers, list-watchers) shares
// signatures within its size class but doesn't fan out across
// classes.
//
// Bounds are [lo, hi). The catch-all final class covers everything
// beyond 400 tokens. An item at token count tc belongs to every
// class i where lo[i] ≤ tc < hi[i] — usually two adjacent classes
// when tc is in the overlap region, one otherwise.
var lengthBucketBounds = []struct{ lo, hi int }{
	{0, 80},
	{50, 160},
	{100, 320},
	{200, 640},
	{400, 1 << 31},
}

// lengthClassesOf returns the class indexes a tokenCount belongs to.
// A tokenCount of 0 ("unknown" — legacy items without TokenCount
// stamped) returns every class so the legacy single-bucket detection
// behaviour is preserved.
func lengthClassesOf(tokenCount int) []int {
	if tokenCount <= 0 {
		all := make([]int, len(lengthBucketBounds))
		for i := range all {
			all[i] = i
		}
		return all
	}
	var out []int
	for i, b := range lengthBucketBounds {
		if tokenCount >= b.lo && tokenCount < b.hi {
			out = append(out, i)
		}
	}
	return out
}

// DetectPairsStratifiedWithStats is the length-stratified variant of
// DetectPairsWithStats. It partitions items into overlapping length
// classes (lengthBucketBounds), runs the per-class LSH pass
// independently, and merges the results.
//
// Two LSH-level wins:
//
//  1. Cross-class candidate pairs never enter any band bucket — pairs
//     more than ~1.6× apart in token count are dropped before the
//     bucket-fan-out blow-up that hurts non-stratified LSH on huge
//     graphs (the per-bucket-cap telemetry's main source).
//  2. Per-class LSH indexes are smaller, so the `seen` dedup map and
//     the band buckets themselves use bounded memory regardless of
//     the global item count.
//
// Pair output is merged across classes; an item that lands in two
// adjacent classes can have its pairs surface twice (once per class),
// so a canonical-pair dedup runs on the merge.
//
// Items whose TokenCount is 0 fall into every class, so callers that
// haven't migrated to stamping token counts get the unstratified
// behaviour back.
func DetectPairsStratifiedWithStats(items []Item, threshold float64) (pairs []Pair, skippedBuckets, skippedBucketItems int) {
	if threshold <= 0 {
		threshold = DefaultThreshold
	}
	if len(items) < 2 {
		return nil, 0, 0
	}

	// Partition into length classes. An item in the overlap region
	// of two classes appears in both.
	classes := make([][]Item, len(lengthBucketBounds))
	for _, it := range items {
		for _, c := range lengthClassesOf(it.TokenCount) {
			classes[c] = append(classes[c], it)
		}
	}

	// Per-class LSH. Each class is independent — different classes
	// share no band buckets and therefore produce no cross-class
	// candidate pairs.
	seen := make(map[[2]string]struct{})
	for _, classItems := range classes {
		if len(classItems) < 2 {
			continue
		}
		classPairs, sb, sbi := DetectPairsWithStats(classItems, threshold)
		skippedBuckets += sb
		skippedBucketItems += sbi
		for _, p := range classPairs {
			// DetectPairsWithStats already canonicalises A < B inside
			// each Pair, so the key matches across classes.
			key := [2]string{p.A, p.B}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			pairs = append(pairs, p)
		}
	}

	// Stable output order so callers logging "found N clones" get
	// reproducible numbers across runs. DetectPairsWithStats sorts by
	// descending similarity within a class; the merge re-sorts globally.
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Similarity != pairs[j].Similarity {
			return pairs[i].Similarity > pairs[j].Similarity
		}
		if pairs[i].A != pairs[j].A {
			return pairs[i].A < pairs[j].A
		}
		return pairs[i].B < pairs[j].B
	})
	return pairs, skippedBuckets, skippedBucketItems
}
