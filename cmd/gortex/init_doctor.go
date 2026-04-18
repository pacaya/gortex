package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/agents"
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

	registry := buildRegistry()
	reports := make([]DoctorAgentReport, 0, len(registry.All()))
	for _, a := range registry.All() {
		reports = append(reports, inspectAdapter(a, env))
	}

	if initDoctorJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"agents": reports})
	}
	printDoctorHuman(cmd.OutOrStdout(), reports)
	return nil
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
