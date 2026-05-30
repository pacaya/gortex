package codeowners_test

import (
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/codeowners"
)

// TestMatchFile_ConcurrentNoRace exercises MatchFile from many goroutines over
// a single shared rule list — the way the indexer's per-file coverage
// goroutines (applyCoverageDomains) call it. Pre-fix, matchPattern lazily
// compiled and cached r.matcher without synchronisation, so concurrent first
// calls raced on the shared *Rule (and on the half-published GitIgnore). Run
// under -race; it must be clean.
func TestMatchFile_ConcurrentNoRace(t *testing.T) {
	rules := codeowners.Parse([]byte(
		"*.go @gophers\n" +
			"/docs/ @writers\n" +
			"src/**/*.ts @frontend @core\n" +
			"*.md @docs\n",
	))
	paths := []string{"main.go", "docs/readme.md", "src/a/b/c.ts", "x/y/z.py", "pkg/foo.go", "README.md"}

	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			for i := range 200 {
				_ = codeowners.MatchFile(paths[i%len(paths)], rules)
			}
		})
	}
	wg.Wait()
}
