package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// interactiveChoice represents the outcome of the gortex init wizard —
// the mode the user picked plus the optional follow-up toggles for
// global mode. Filled by runInteractiveInit; consumed by runInit to
// branch to the right code path without duplicating logic.
type interactiveChoice struct {
	Global bool
	Track  bool
	Start  bool
	Hooks  bool
}

// runInteractiveInit prompts the user to pick between global and
// per-repo setup, along with the track/start follow-ups. The caller
// is responsible for gating this behind isInteractive() — separating
// the decision (caller) from the prompt body (this function) keeps
// the wizard unit-testable with a plain io.Reader.
//
// Returns the decided choice and whether the prompt completed
// successfully. A return of (_, false) means the user terminated the
// prompt early (EOF / Ctrl-D); the caller should fall back to
// historical defaults rather than making a guess.
//
// hooksPreset tells the wizard that --hooks / --no-hooks was already
// passed on the command line; in that case we skip the hooks prompt
// to avoid nagging the user about a decision they already made.
func runInteractiveInit(in io.Reader, out io.Writer, hooksPreset bool) (interactiveChoice, bool) {
	reader := bufio.NewReader(in)

	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "How should Gortex integrate with your AI tools?")
	_, _ = fmt.Fprintln(out, "  [1] Global daemon (recommended) — one graph across all projects,")
	_, _ = fmt.Fprintln(out, "      per-client session isolation, live file watching, user-level hooks")
	_, _ = fmt.Fprintln(out, "  [2] Per-repo — isolated server per project; each Claude Code window")
	_, _ = fmt.Fprintln(out, "      spawns its own indexer (current default)")
	_, _ = fmt.Fprint(out, "Choice [1/2] (default: 1): ")

	// Default Hooks=true matches the --hooks CLI default. Yes to everything
	// is the happy path we optimize the wizard for.
	choice := interactiveChoice{Global: true, Hooks: true}
	line, err := reader.ReadString('\n')
	if err != nil {
		// EOF / closed stdin — fall back to legacy per-repo behavior.
		return interactiveChoice{}, false
	}
	isPerRepo := false
	switch strings.TrimSpace(line) {
	case "2", "p", "per-repo":
		isPerRepo = true
	case "", "1", "g", "global":
		// default path
	default:
		_, _ = fmt.Fprintf(out, "Unrecognized %q — defaulting to global.\n", strings.TrimSpace(line))
	}

	if isPerRepo {
		// Per-repo mode: no Track/Start (they only make sense with the
		// daemon) but still ask about hooks — they're useful in either mode.
		choice.Global = false
		if !hooksPreset {
			_, _ = fmt.Fprint(out, "Install Claude Code hooks (PreToolUse + PreCompact + Stop)? [Y/n]: ")
			if ln, err := reader.ReadString('\n'); err == nil {
				choice.Hooks = !isNo(ln)
			}
		}
		_, _ = fmt.Fprintln(out)
		return choice, true
	}

	// Track-the-current-repo prompt. Declining is fine — the user can
	// always run `gortex track .` later.
	_, _ = fmt.Fprint(out, "Track this repository with the daemon now? [Y/n]: ")
	if ln, err := reader.ReadString('\n'); err == nil {
		choice.Track = !isNo(ln)
	}

	// Start-daemon prompt. If the user says no, the install still writes
	// config; they can spawn it later with `gortex daemon start --detach`.
	_, _ = fmt.Fprint(out, "Start the daemon now (detached)? [Y/n]: ")
	if ln, err := reader.ReadString('\n'); err == nil {
		choice.Start = !isNo(ln)
	}

	// Hooks prompt — skipped when --hooks / --no-hooks was explicit.
	if !hooksPreset {
		_, _ = fmt.Fprint(out, "Install Claude Code hooks (PreToolUse + PreCompact + Stop)? [Y/n]: ")
		if ln, err := reader.ReadString('\n'); err == nil {
			choice.Hooks = !isNo(ln)
		}
	}

	_, _ = fmt.Fprintln(out)
	return choice, true
}

// isNo returns true when the user answered "no" to a yes/no prompt.
// Blank input is treated as yes (the capital Y in "[Y/n]" sets the
// default). Anything else that starts with n/N is no.
func isNo(line string) bool {
	s := strings.ToLower(strings.TrimSpace(line))
	return strings.HasPrefix(s, "n")
}

// isInteractive reports whether stdin is a terminal — the gate that
// separates "user typed gortex init at a prompt" from "CI script ran
// gortex init." We only prompt in the former case.
func isInteractive() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
