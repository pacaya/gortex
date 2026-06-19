package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/daemon"
)

// initDoctorCmd implements `gortex init doctor`. It re-runs every
// adapter's Detect + Plan against the current environment and
// reports drift — files we'd like to write, files that already
// exist, files whose contents look wrong. Zero writes: doctor
// never modifies disk.
//
// This is the zero-op form of the installer and the primary
// support-channel tool. Output defaults to a human-readable table;
// --json emits a machine-readable report for CI consumers.
var initDoctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Report the configured state of every Gortex integration without writing anything",
	Args:  cobra.NoArgs,
	RunE:  runInitDoctor,
}

var initDoctorJSON bool

func init() {
	initDoctorCmd.Flags().BoolVar(&initDoctorJSON, "json", false, "emit a structured JSON report on stdout")
	initCmd.AddCommand(initDoctorCmd)
}

// DoctorEnvironment captures the live wiring doctor can verify because
// Gortex owns the binary and the daemon — something a config-file-only
// integration cannot check: that the `gortex` the editor will launch is the
// one on PATH, and that the daemon it proxies to actually answers a handshake.
type DoctorEnvironment struct {
	BinaryOnPath  bool   `json:"binary_on_path"`
	BinaryPath    string `json:"binary_path,omitempty"`
	BinaryError   string `json:"binary_error,omitempty"`
	DaemonRunning bool   `json:"daemon_running"`
	DaemonSocket  string `json:"daemon_socket,omitempty"`
	DaemonVersion string `json:"daemon_version,omitempty"`
	DaemonError   string `json:"daemon_error,omitempty"`
}

// DoctorAgentReport is one agent's slice of the doctor output.
// Each planned file shows up in Files along with its observed
// status on disk (present, missing, …).
type DoctorAgentReport struct {
	Name       string             `json:"name"`
	DocsURL    string             `json:"docs_url,omitempty"`
	Detected   bool               `json:"detected"`
	Configured bool               `json:"configured"`
	Files      []DoctorFileStatus `json:"files,omitempty"`
}

// DoctorFileStatus is the observed state of one file the adapter
// would touch. Status is the union of "present", "missing",
// "unreadable", "would-create", "would-merge". The first three
// describe disk observation; the last two carry the adapter's
// planned action so reviewers see both "what's there" and "what
// init would do" side by side.
type DoctorFileStatus struct {
	Path     string            `json:"path"`
	Status   string            `json:"status"`
	Planned  agents.ActionKind `json:"planned_action,omitempty"`
	Keys     []string          `json:"keys,omitempty"`
	ByteSize int64             `json:"byte_size,omitempty"`
	Reason   string            `json:"reason,omitempty"`
	// StanzaStatus is set for a present mcpServers file carrying a
	// Gortex-authored entry: "current" when it matches the canonical stanza,
	// "stale" when Gortex wrote it but the shape has since changed. Empty for
	// files that are not Gortex MCP configs. StanzaHint carries the fix.
	StanzaStatus string `json:"stanza_status,omitempty"`
	StanzaHint   string `json:"stanza_hint,omitempty"`
}

func runInitDoctor(cmd *cobra.Command, _ []string) error {
	home, _ := os.UserHomeDir()
	root, err := filepath.Abs(".")
	if err != nil {
		return err
	}

	env := agents.Env{
		Root:         root,
		Home:         home,
		HookCommand:  "gortex hook",
		Mode:         agents.ModeProject,
		InstallHooks: true,
		Stderr:       nil, // suppress progress lines; doctor is read-only
	}

	doctorEnv := doctorEnvironment()

	registry := buildRegistry()
	reports := make([]DoctorAgentReport, 0, len(registry.All()))
	for _, a := range registry.All() {
		reports = append(reports, inspectAdapter(a, env))
	}

	if initDoctorJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"environment": doctorEnv, "agents": reports})
	}
	printDoctorEnvironment(cmd.OutOrStdout(), doctorEnv)
	printDoctorHuman(cmd.OutOrStdout(), reports)
	return nil
}

// doctorEnvironment probes the live wiring: whether `gortex` resolves on PATH
// and whether the daemon answers a handshake on its socket. Both are
// best-effort and never fail the command — doctor is a read-only diagnostic.
func doctorEnvironment() DoctorEnvironment {
	out := DoctorEnvironment{DaemonSocket: daemon.SocketPath()}
	if p, err := exec.LookPath("gortex"); err == nil {
		out.BinaryOnPath = true
		out.BinaryPath = p
	} else {
		out.BinaryError = err.Error()
	}
	// A real handshake (not just a socket stat) proves the daemon is alive and
	// speaking the protocol this binary expects.
	c, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeControl, ClientName: "init-doctor"})
	if err != nil {
		if errors.Is(err, daemon.ErrDaemonUnavailable) {
			out.DaemonError = "no daemon running (start it with `gortex daemon start`)"
		} else {
			out.DaemonError = err.Error()
		}
		return out
	}
	defer func() { _ = c.Close() }()
	out.DaemonRunning = true
	out.DaemonVersion = c.Ack.DaemonVersion
	return out
}

// inspectMCPStanza reads a present file, looks for a Gortex-authored entry
// under mcpServers, and reports whether it matches the canonical stanza
// ("current") or has drifted ("stale"). ok is false when the file is not a
// Gortex MCP config (not JSON, no mcpServers, or the entry is user-authored),
// so non-MCP planned files are left unannotated.
func inspectMCPStanza(path string) (status string, ok bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return "", false
	}
	servers, isMap := root["mcpServers"].(map[string]any)
	if !isMap {
		return "", false
	}
	entry, found := servers["gortex"]
	if !found {
		// Some configs key the server differently; accept any single
		// Gortex-authored entry.
		for _, v := range servers {
			if agents.IsGortexAuthoredMCPEntry(v) {
				entry, found = v, true
				break
			}
		}
	}
	if !found || !agents.IsGortexAuthoredMCPEntry(entry) {
		return "", false
	}
	if agents.MCPEntriesEqual(entry, agents.DefaultGortexMCPEntry()) {
		return "current", true
	}
	return "stale", true
}

// inspectAdapter runs Detect + Plan for one adapter, then stats
// each planned file to fill in the observed status. Never calls
// Apply, so disk is untouched.
func inspectAdapter(a agents.Adapter, env agents.Env) DoctorAgentReport {
	rep := DoctorAgentReport{Name: a.Name(), DocsURL: a.DocsURL()}
	detected, _ := a.Detect(env)
	rep.Detected = detected

	plan, err := a.Plan(env)
	if err != nil || plan == nil {
		return rep
	}

	anyPresent := false
	for _, pf := range plan.Files {
		status := DoctorFileStatus{Path: pf.Path, Planned: pf.Action, Keys: pf.Keys}
		info, err := os.Stat(pf.Path)
		switch {
		case err == nil:
			anyPresent = true
			status.Status = "present"
			status.ByteSize = info.Size()
			// For a present mcpServers config, verify the Gortex stanza is
			// the current canonical shape — Gortex owns the migration logic,
			// so doctor can flag an authored-but-outdated stanza and name the
			// one-command fix instead of silently leaving a broken wiring.
			if st, isStanza := inspectMCPStanza(pf.Path); isStanza {
				status.StanzaStatus = st
				if st == "stale" {
					status.StanzaHint = "Gortex-authored but outdated — run `gortex install` to migrate"
				}
			}
		case os.IsNotExist(err):
			status.Status = "missing"
		default:
			status.Status = "unreadable"
			status.Reason = err.Error()
		}
		rep.Files = append(rep.Files, status)
	}
	rep.Configured = anyPresent
	return rep
}

// printDoctorEnvironment renders the live-wiring preamble: the binary on PATH
// and the daemon handshake. These two lines answer "is the integration
// actually live", not just "is the config file present".
func printDoctorEnvironment(w io.Writer, env DoctorEnvironment) {
	_, _ = fmt.Fprintln(w, "Gortex init doctor — environment:")
	if env.BinaryOnPath {
		_, _ = fmt.Fprintf(w, "  %s gortex on PATH: %s\n", glyphCheck, env.BinaryPath)
	} else {
		_, _ = fmt.Fprintf(w, "  %s gortex not found on PATH (%s)\n", glyphCross, env.BinaryError)
	}
	if env.DaemonRunning {
		ver := env.DaemonVersion
		if ver == "" {
			ver = "ok"
		}
		_, _ = fmt.Fprintf(w, "  %s daemon handshake: %s (%s)\n", glyphCheck, ver, env.DaemonSocket)
	} else {
		_, _ = fmt.Fprintf(w, "  %s daemon handshake: %s\n", glyphCross, env.DaemonError)
	}
	_, _ = fmt.Fprintln(w)
}

// glyphCheck / glyphCross are the doctor's status markers.
const (
	glyphCheck = "✓"
	glyphCross = "✗"
)

// printDoctorHuman renders the human-readable summary. One row per
// agent, with a nested file list. Columns line up for copy-paste
// into issue reports.
func printDoctorHuman(w io.Writer, reports []DoctorAgentReport) {
	_, _ = fmt.Fprintln(w, "Gortex init doctor — observed state of every adapter's planned files:")
	_, _ = fmt.Fprintln(w)

	for _, r := range reports {
		detMark := "–"
		if r.Detected {
			detMark = "✓"
		}
		cfgMark := "–"
		if r.Configured {
			cfgMark = "✓"
		}
		_, _ = fmt.Fprintf(w, "  [%s detected] [%s any-file-present]  %s\n", detMark, cfgMark, r.Name)
		for _, f := range r.Files {
			statusSym := "?"
			switch f.Status {
			case "present":
				statusSym = "✓"
			case "missing":
				statusSym = "✗"
			case "unreadable":
				statusSym = "!"
			}
			extra := ""
			if f.Status == "present" && f.ByteSize > 0 {
				extra = fmt.Sprintf(" (%d bytes)", f.ByteSize)
			}
			switch f.StanzaStatus {
			case "stale":
				extra += " [stale stanza: run `gortex install` to migrate]"
			case "current":
				extra += " [stanza current]"
			}
			if f.Status == "missing" && f.Planned != "" {
				// Strip the "would-" prefix so we don't print
				// "init would would-create".
				verb := string(f.Planned)
				if len(verb) > len("would-") && verb[:len("would-")] == "would-" {
					verb = verb[len("would-"):]
				}
				extra = fmt.Sprintf(" (init would %s)", verb)
			}
			_, _ = fmt.Fprintf(w, "      %s %s%s\n", statusSym, f.Path, extra)
		}
		if r.DocsURL != "" {
			_, _ = fmt.Fprintf(w, "      docs: %s\n", r.DocsURL)
		}
		_, _ = fmt.Fprintln(w)
	}
}
