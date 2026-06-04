package resolver

import "testing"

// TestIsLanguageStdlib_PerLanguage pins the language-aware stdlib filter
// across every ecosystem external-call qualification now covers. The JVM
// and .NET rows are the ones added so default-on synthesis doesn't
// materialise a node for every JDK / BCL call.
func TestIsLanguageStdlib_PerLanguage(t *testing.T) {
	cases := []struct {
		lang, path string
		want       bool
	}{
		// Go: dotless first segment is stdlib; a domain-led path is a dep.
		{"go", "fmt", true},
		{"go", "net/http", true},
		{"go", "github.com/foo/bar", false},
		// Python: stdlib top-level packages vs pip packages.
		{"python", "os.path", true},
		{"python", "requests", false},
		// Node: core modules (bare + node: form) vs npm packages.
		{"javascript", "node:fs", true},
		{"typescript", "fs", true},
		{"typescript", "react", false},
		// Rust: std distribution vs crates.
		{"rust", "std::collections", true},
		{"rust", "tokio::sync", false},
		// JVM: JDK / Jakarta / internal trees are platform; everything
		// else (incl. Kotlin/Scala stdlibs, which ship as Maven jars) is
		// a dependency.
		{"java", "java.util.List", true},
		{"java", "javax.servlet.http", true},
		{"java", "jakarta.persistence", true},
		{"java", "sun.misc.Unsafe", true},
		{"java", "com.sun.net.httpserver", true},
		{"java", "com.google.common.collect", false},
		{"kotlin", "java.io", true},
		{"kotlin", "org.jetbrains.exposed", false},
		// `javafx` must not be swallowed by the `java` prefix rule.
		{"java", "javafx.scene", false},
		// .NET: System.* / Microsoft.* are the BCL; vendor namespaces are
		// NuGet packages.
		{"csharp", "System.Collections.Generic", true},
		{"csharp", "Microsoft.Extensions.Logging", true},
		{"csharp", "mscorlib", true},
		{"csharp", "Newtonsoft.Json", false},
		// Unknown language: treat as external (one extra node beats
		// dropping a real edge).
		{"", "anything", false},
	}
	for _, c := range cases {
		if got := isLanguageStdlib(c.lang, c.path); got != c.want {
			t.Errorf("isLanguageStdlib(%q, %q) = %v, want %v", c.lang, c.path, got, c.want)
		}
	}
}

// TestExternalCallNodeID_StableCrossRepo documents the cross-repo
// identity property: the synthetic node ID is a pure function of
// (ecosystem, import path), so a call into the same package from two
// different repositories lands on one shared node — the basis for
// aggregating a service's external surface across repos.
func TestExternalCallNodeID_StableCrossRepo(t *testing.T) {
	repoA := externalCallNodeID("dep", "github.com/stripe/stripe-go")
	repoB := externalCallNodeID("dep", "github.com/stripe/stripe-go")
	if repoA != repoB {
		t.Fatalf("same package must share one node ID: %q != %q", repoA, repoB)
	}
	other := externalCallNodeID("dep", "github.com/aws/aws-sdk-go")
	if repoA == other {
		t.Fatalf("distinct packages must get distinct node IDs")
	}
}
