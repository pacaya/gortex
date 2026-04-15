package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// runWizard is a test harness that feeds canned stdin into
// runInteractiveInit. Returns the choice + the prompt output so
// assertions can pin both the control flow and the user-facing text.
//
// Note: runInteractiveInit gates on isInteractive() which reads from
// the real os.Stdin. We side-step that by calling the *bufio*-
// consuming half directly — the prompt body behaves identically when
// invoked with arbitrary io.Reader, we just need to bypass the tty
// check. So tests here invoke the internal logic via a thin wrapper
// that assumes interactive mode.
func runWizard(t *testing.T, input string) (interactiveChoice, string) {
	t.Helper()
	var out bytes.Buffer
	in := strings.NewReader(input)

	// Call the prompt body directly by inlining — this mirrors
	// runInteractiveInit but skips the isInteractive() gate so the
	// wizard is exercised under test without a real TTY.
	choice, _ := runInteractiveForTest(in, &out)
	return choice, out.String()
}

func TestInteractiveWizard_DefaultIsGlobal(t *testing.T) {
	// Pressing Enter to every prompt (global, track, start, hooks) must
	// pick the happy path: global=yes, track=yes, start=yes, hooks=yes.
	choice, out := runWizard(t, "\n\n\n\n")
	assert.True(t, choice.Global, "empty input must default to global")
	assert.True(t, choice.Track)
	assert.True(t, choice.Start)
	assert.True(t, choice.Hooks, "empty input on hooks prompt must default to yes")
	assert.Contains(t, out, "Global daemon (recommended)")
	assert.Contains(t, out, "Install Claude Code hooks")
}

func TestInteractiveWizard_ChoosePerRepo(t *testing.T) {
	// Per-repo still asks the hooks question (hooks are useful in either mode).
	choice, out := runWizard(t, "2\n\n")
	assert.False(t, choice.Global, "option 2 must map to per-repo mode")
	// Track / Start aren't asked when per-repo — they only make sense
	// in global mode.
	assert.False(t, choice.Track)
	assert.False(t, choice.Start)
	assert.True(t, choice.Hooks, "per-repo + empty hooks input must default to yes")
	assert.Contains(t, out, "Install Claude Code hooks")
}

func TestInteractiveWizard_PerRepoDeclineHooks(t *testing.T) {
	// Per-repo, explicitly decline hooks.
	choice, _ := runWizard(t, "2\nn\n")
	assert.False(t, choice.Global)
	assert.False(t, choice.Hooks, "'n' to hooks prompt must set Hooks=false")
}

func TestInteractiveWizard_DeclineTrack(t *testing.T) {
	// Global + decline-to-track + start-yes + hooks-yes. Makes sure each
	// follow-up prompt is handled independently.
	choice, _ := runWizard(t, "1\nn\ny\ny\n")
	assert.True(t, choice.Global)
	assert.False(t, choice.Track, "'n' to track must set Track=false")
	assert.True(t, choice.Start)
	assert.True(t, choice.Hooks)
}

func TestInteractiveWizard_DeclineBoth(t *testing.T) {
	// All four prompts: global / track-no / start-no / hooks-no.
	choice, _ := runWizard(t, "1\nn\nn\nn\n")
	assert.True(t, choice.Global)
	assert.False(t, choice.Track)
	assert.False(t, choice.Start)
	assert.False(t, choice.Hooks)
}

func TestInteractiveWizard_HooksPresetSkipsPrompt(t *testing.T) {
	// When --hooks / --no-hooks was already passed on the CLI, the wizard
	// must not re-ask. We only feed three newlines (global, track, start)
	// — if the hooks prompt fired, reader.ReadString would block/return
	// EOF and Hooks would fall back to the struct default.
	var out bytes.Buffer
	in := strings.NewReader("\n\n\n")
	choice, ok := runInteractiveInit(in, &out, true)
	assert.True(t, ok)
	assert.True(t, choice.Global)
	assert.True(t, choice.Track)
	assert.True(t, choice.Start)
	assert.NotContains(t, out.String(), "Install Claude Code hooks",
		"hooks prompt must be suppressed when hooksPreset=true")
}

func TestInteractiveWizard_UnrecognizedChoiceFallsBackToGlobal(t *testing.T) {
	// Unknown first-prompt answer prints a warning and defaults to
	// global. Users get out of trouble by pressing Enter next time.
	choice, out := runWizard(t, "zzz\n\n\n\n")
	assert.True(t, choice.Global)
	assert.Contains(t, out, "Unrecognized")
}

func TestIsNo(t *testing.T) {
	// Blank input is yes (default pick). Anything starting with n is no.
	// Case and trailing whitespace must not matter.
	assert.False(t, isNo(""))
	assert.False(t, isNo("\n"))
	assert.False(t, isNo("y\n"))
	assert.False(t, isNo("Y\n"))
	assert.False(t, isNo("yes\n"))
	assert.True(t, isNo("n\n"))
	assert.True(t, isNo("N\n"))
	assert.True(t, isNo("no\n"))
	assert.True(t, isNo("  no  \n"))
}

// runInteractiveForTest invokes the prompt body with an arbitrary
// io.Reader so tests can feed canned input. It duplicates the body of
// runInteractiveInit minus the isInteractive() gate — keeping the
// gate untested is fine since it just wraps os.Stdin.Stat().
//
// Defined in the _test.go file (not daemon_interactive.go) so the
// production binary doesn't ship a second entry point that bypasses
// the TTY check.
func runInteractiveForTest(in *strings.Reader, out *bytes.Buffer) (interactiveChoice, bool) {
	return runInteractiveInit(in, out, false)
}
