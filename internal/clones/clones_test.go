package clones

import (
	"strings"
	"testing"
)

func TestTokenizeKeywordsAndNormalisation(t *testing.T) {
	src := `func add(a int, b int) int { return a + b }`
	got := Tokenize(src)
	want := []string{
		"func", "v", "(", "v", "v", ",", "v", "v", ")", "v",
		"{", "return", "v", "+", "v", "}",
	}
	if len(got) != len(want) {
		t.Fatalf("token count: got %d %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("token %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTokenizeLiteralsAndOperators(t *testing.T) {
	got := Tokenize(`x := "hello" + 42 == 0x1F`)
	want := []string{"v", ":=", "s", "+", "0", "==", "0"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTokenizeStringWithEscapes(t *testing.T) {
	// The escaped quote must not terminate the literal early.
	got := Tokenize(`return "a\"b" + c`)
	want := []string{"return", "s", "+", "v"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

const bodyOriginal = `
	total := 0
	for i := 0; i < len(items); i++ {
		if items[i].Active {
			total += items[i].Weight * factor
		} else {
			total -= items[i].Penalty
		}
	}
	if total < 0 {
		total = 0
	}
	return total
`

// Same logic, every identifier renamed — a textbook Type-2 clone.
const bodyRenamed = `
	sum := 0
	for idx := 0; idx < len(records); idx++ {
		if records[idx].Enabled {
			sum += records[idx].Score * multiplier
		} else {
			sum -= records[idx].Fine
		}
	}
	if sum < 0 {
		sum = 0
	}
	return sum
`

// Unrelated function of comparable size.
const bodyDifferent = `
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()
	rows, err := conn.Query(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAll(rows)
`

func TestComputeSignatureTooSmall(t *testing.T) {
	if _, ok := ComputeSignature(`return a + b`); ok {
		t.Fatal("tiny body should not produce a signature")
	}
}

func TestComputeSignatureIdentical(t *testing.T) {
	a, ok := ComputeSignature(bodyOriginal)
	if !ok {
		t.Fatal("expected a signature for a substantial body")
	}
	b, _ := ComputeSignature(bodyOriginal)
	if a != b {
		t.Fatal("identical bodies must produce identical signatures")
	}
	if EstimateJaccard(a, b) != 1.0 {
		t.Fatalf("identical bodies must have Jaccard 1.0, got %v", EstimateJaccard(a, b))
	}
}

func TestComputeSignatureRenamedIsClone(t *testing.T) {
	a, ok1 := ComputeSignature(bodyOriginal)
	b, ok2 := ComputeSignature(bodyRenamed)
	if !ok1 || !ok2 {
		t.Fatal("both bodies should produce signatures")
	}
	sim := EstimateJaccard(a, b)
	if sim < DefaultThreshold {
		t.Fatalf("renamed-variable clone should score >= %v, got %v", DefaultThreshold, sim)
	}
}

func TestComputeSignatureUnrelatedIsNotClone(t *testing.T) {
	a, _ := ComputeSignature(bodyOriginal)
	c, ok := ComputeSignature(bodyDifferent)
	if !ok {
		t.Fatal("expected a signature for the unrelated body")
	}
	if sim := EstimateJaccard(a, c); sim >= DefaultThreshold {
		t.Fatalf("unrelated functions should not be clones, got similarity %v", sim)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	sig, ok := ComputeSignature(bodyOriginal)
	if !ok {
		t.Fatal("expected a signature")
	}
	enc := EncodeSignature(sig)
	if enc == "" {
		t.Fatal("encoded signature is empty")
	}
	back, ok := DecodeSignature(enc)
	if !ok {
		t.Fatal("decode failed")
	}
	if back != sig {
		t.Fatal("round-trip changed the signature")
	}
}

func TestDecodeSignatureRejectsGarbage(t *testing.T) {
	if _, ok := DecodeSignature(""); ok {
		t.Error("empty string should not decode")
	}
	if _, ok := DecodeSignature("not-base64!!!"); ok {
		t.Error("invalid base64 should not decode")
	}
	if _, ok := DecodeSignature("YWJj"); ok {
		t.Error("wrong-length payload should not decode")
	}
}

func mustSig(t *testing.T, body string) Signature {
	t.Helper()
	s, ok := ComputeSignature(body)
	if !ok {
		t.Fatalf("body did not produce a signature")
	}
	return s
}

func TestDetectPairs(t *testing.T) {
	items := []Item{
		{ID: "a", Sig: mustSig(t, bodyOriginal)},
		{ID: "b", Sig: mustSig(t, bodyRenamed)},
		{ID: "c", Sig: mustSig(t, bodyDifferent)},
	}
	pairs := DetectPairs(items, 0) // 0 → DefaultThreshold
	if len(pairs) != 1 {
		t.Fatalf("expected exactly one clone pair, got %d: %+v", len(pairs), pairs)
	}
	p := pairs[0]
	if p.A != "a" || p.B != "b" {
		t.Fatalf("expected pair (a,b), got (%s,%s)", p.A, p.B)
	}
	if p.Similarity < DefaultThreshold {
		t.Fatalf("pair similarity %v below threshold", p.Similarity)
	}
}

func TestDetectPairsCanonicalOrder(t *testing.T) {
	// IDs supplied out of order must still come back canonicalised.
	items := []Item{
		{ID: "zeta", Sig: mustSig(t, bodyOriginal)},
		{ID: "alpha", Sig: mustSig(t, bodyOriginal)},
	}
	pairs := DetectPairs(items, 0)
	if len(pairs) != 1 {
		t.Fatalf("expected one pair, got %d", len(pairs))
	}
	if pairs[0].A != "alpha" || pairs[0].B != "zeta" {
		t.Fatalf("pair not canonicalised: %+v", pairs[0])
	}
}

func TestClusterPairsTransitive(t *testing.T) {
	pairs := []Pair{
		{A: "a", B: "b", Similarity: 0.9},
		{A: "b", B: "c", Similarity: 0.95},
		{A: "x", B: "y", Similarity: 0.88},
	}
	clusters := ClusterPairs(pairs)
	if len(clusters) != 2 {
		t.Fatalf("expected 2 clusters, got %d: %+v", len(clusters), clusters)
	}
	// Largest cluster first.
	if clusters[0].Size != 3 {
		t.Fatalf("expected first cluster size 3, got %d", clusters[0].Size)
	}
	want := []string{"a", "b", "c"}
	for i, m := range want {
		if clusters[0].Members[i] != m {
			t.Fatalf("cluster members %v, want %v", clusters[0].Members, want)
		}
	}
	if clusters[0].AvgSimilarity <= 0 {
		t.Fatal("cluster average similarity should be positive")
	}
	if clusters[1].Size != 2 {
		t.Fatalf("expected second cluster size 2, got %d", clusters[1].Size)
	}
}

func TestDetectPairsEmptyInput(t *testing.T) {
	if pairs := DetectPairs(nil, 0); len(pairs) != 0 {
		t.Fatalf("no items should yield no pairs, got %+v", pairs)
	}
}
