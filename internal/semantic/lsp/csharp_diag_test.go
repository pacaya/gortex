package lsp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestIsNuGetAdvisoryCode pins the advisory-code predicate: it must match the
// NU#### NuGet family (the audit/restore advisories we filter) and must NOT
// match real CS#### compiler codes — filtering those would hide genuine errors.
func TestIsNuGetAdvisoryCode(t *testing.T) {
	cases := []struct {
		code string
		want bool
	}{
		{"NU1902", true}, // the transitive-vuln advisory that bites csharp-ls
		{"NU1903", true},
		{"NU1605", true},
		{"NU1", true},
		{"nu1902", true},  // prefix is case-insensitive
		{"CS0246", false}, // real compiler error — must survive the filter
		{"CS1591", false},
		{"NU", false},     // no digits
		{"NUGET", false},  // letters after NU
		{"NU19A2", false}, // non-digit inside the number
		{"1902", false},
		{"N", false},
		{"", false},
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, isNuGetAdvisoryCode(c.code), "isNuGetAdvisoryCode(%q)", c.code)
	}
}

// TestDiagCodeString covers the wire-type normalisation: string codes pass
// through, json.Number renders, and anything else (incl. JSON-unmarshalled
// numeric float64) yields "" — sufficient because NU codes are always strings.
func TestDiagCodeString(t *testing.T) {
	assert.Equal(t, "NU1902", diagCodeString("NU1902"))
	assert.Equal(t, "1902", diagCodeString(json.Number("1902")))
	assert.Equal(t, "", diagCodeString(float64(1902)))
	assert.Equal(t, "", diagCodeString(1902))
	assert.Equal(t, "", diagCodeString(nil))
}

// TestFilterCSharpAdvisoryDiags verifies the filter drops only NU#### NuGet
// advisories, preserves real CS#### diagnostics and their order, and returns
// the input untouched when there is nothing to drop.
func TestFilterCSharpAdvisoryDiags(t *testing.T) {
	// No advisories → same slice handed back, unchanged.
	clean := []Diagnostic{
		{Code: "CS0246", Severity: DiagSeverityError, Message: "type or namespace not found"},
		{Code: "CS1591", Severity: DiagSeverityWarning, Message: "missing XML comment"},
	}
	assert.Equal(t, clean, filterCSharpAdvisoryDiags(clean))

	// Mixed: NU advisories dropped, CS diagnostics kept in original order.
	mixed := []Diagnostic{
		{Code: "NU1902", Severity: DiagSeverityError, Message: "package has a known vulnerability"},
		{Code: "CS0246", Severity: DiagSeverityError, Message: "type or namespace not found"},
		{Code: "NU1903", Severity: DiagSeverityWarning, Message: "high severity vulnerability"},
		{Code: "CS0103", Severity: DiagSeverityError, Message: "name does not exist"},
	}
	got := filterCSharpAdvisoryDiags(mixed)
	assert.Len(t, got, 2)
	assert.Equal(t, "CS0246", got[0].Code)
	assert.Equal(t, "CS0103", got[1].Code)

	// All advisories → empty result.
	assert.Empty(t, filterCSharpAdvisoryDiags([]Diagnostic{{Code: "NU1902"}, {Code: "NU1605"}}))

	// Empty / nil input is safe.
	assert.Empty(t, filterCSharpAdvisoryDiags(nil))
}

// TestServesCSharp checks the language-scoping guard used to confine the
// C#-specific behaviour to C# providers.
func TestServesCSharp(t *testing.T) {
	assert.True(t, (&Provider{languages: []string{"csharp"}}).servesCSharp())
	assert.True(t, (&Provider{languages: []string{"go", "csharp"}}).servesCSharp())
	assert.False(t, (&Provider{languages: []string{"go"}}).servesCSharp())
	assert.False(t, (&Provider{}).servesCSharp())
}

// TestCSharpDiagFilterEnabled confirms the filter is ON by default and only
// the explicit falsey values disable it.
func TestCSharpDiagFilterEnabled(t *testing.T) {
	t.Setenv(CSharpDiagFilterEnv, "") // unset-equivalent → default ON
	assert.True(t, csharpDiagFilterEnabled())

	for _, off := range []string{"0", "off", "false", "none", "OFF", "False"} {
		t.Setenv(CSharpDiagFilterEnv, off)
		assert.Falsef(t, csharpDiagFilterEnabled(), "value %q should disable the filter", off)
	}

	t.Setenv(CSharpDiagFilterEnv, "1")
	assert.True(t, csharpDiagFilterEnabled())
}

// TestCSharpPreRestoreEligible verifies the pre-restore gate: ON by default for
// a C# provider that is spawning, disabled only by an explicit falsey
// CSharpRestoreEnv, and never active for a non-C# provider or a passive attach
// (where the IDE owns restore).
func TestCSharpPreRestoreEligible(t *testing.T) {
	csharp := func() *Provider { return &Provider{languages: []string{"csharp"}} }

	t.Setenv(CSharpRestoreEnv, "") // unset-equivalent → default ON
	assert.True(t, csharp().csharpPreRestoreEligible(), "on by default for a spawning C# provider")

	for _, off := range []string{"0", "off", "false", "none", "OFF", "False"} {
		t.Setenv(CSharpRestoreEnv, off)
		assert.Falsef(t, csharp().csharpPreRestoreEligible(), "value %q should disable pre-restore", off)
	}

	t.Setenv(CSharpRestoreEnv, "1")
	assert.True(t, csharp().csharpPreRestoreEligible(), "explicitly enabled + serves C# + spawning")

	// Not a C# provider → never restore, even with restore enabled.
	assert.False(t, (&Provider{languages: []string{"go"}}).csharpPreRestoreEligible())

	// C# but passively attached (the IDE owns restore) → skip.
	p := csharp()
	p.connect = &ConnectSpec{}
	assert.False(t, p.csharpPreRestoreEligible(), "passive-attach must not trigger restore")
}
