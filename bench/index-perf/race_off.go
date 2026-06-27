//go:build !race

package indexperf

// raceDetector reports whether the binary was built with the race detector.
// See race_on.go for why the regression gate is conditioned on it.
const raceDetector = false
