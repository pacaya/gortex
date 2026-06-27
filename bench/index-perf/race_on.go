//go:build race

package indexperf

// raceDetector reports whether the binary was built with the race detector.
// Race instrumentation inflates both wall-clock and allocation several-fold,
// so a race-mode measurement is not comparable to a baseline recorded without
// it. The harness records and prints the numbers under -race but skips the
// wall-clock regression gate, keeping `go test -race` green.
const raceDetector = true
