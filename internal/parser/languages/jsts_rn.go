package languages

import (
	"regexp"
	"strings"
)

// React Native JS/TS native-module call recognition. A JS call into a
// native module — `NativeModules.Foo.bar(...)`, a binding to
// `requireNativeModule('Foo')`, or `TurboModuleRegistry.getEnforcing('Foo')`
// — is the consumer side of the RN bridge. This shared pass turns such a
// call into a placeholder edge the resolver's React-Native bridge
// synthesizer lands on the native implementation.

// rnNativeVia marks a JS→native React Native bridge call edge.
const rnNativeVia = "rn.native"

var (
	// const/let/var X = NativeModules.Foo
	rnVarNativeModulesRe = regexp.MustCompile(`(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*NativeModules\.([A-Za-z_$][\w$]*)`)
	// const/let/var X = requireNativeModule('Foo') | TurboModuleRegistry.get('Foo') | .getEnforcing('Foo')
	rnVarRequireRe = regexp.MustCompile(`(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:requireNativeModule|TurboModuleRegistry\.(?:get|getEnforcing))\(\s*['"]([^'"]+)['"]`)
	// const { Foo, Bar as Baz } = NativeModules
	rnDestructureRe = regexp.MustCompile(`(?:const|let|var)\s*\{([^}]*)\}\s*=\s*NativeModules\b`)
	// inline receiver `NativeModules.Foo`
	rnInlineNativeModulesRe = regexp.MustCompile(`^NativeModules\.([A-Za-z_$][\w$]*)$`)
	// inline receiver `requireNativeModule('Foo')` / `TurboModuleRegistry.get('Foo')`
	rnInlineRequireRe = regexp.MustCompile(`^(?:requireNativeModule|TurboModuleRegistry\.(?:get|getEnforcing))\(\s*['"]([^'"]+)['"]\s*\)$`)
)

// rnNativeModuleVars scans a JS/TS source for local bindings to a React
// Native native module and returns localName → moduleName, covering
// direct assignment, requireNativeModule / TurboModuleRegistry, and
// destructuring from NativeModules (with `as` aliases).
func rnNativeModuleVars(src []byte) map[string]string {
	s := string(src)
	out := map[string]string{}
	for _, m := range rnVarNativeModulesRe.FindAllStringSubmatch(s, -1) {
		out[m[1]] = m[2]
	}
	for _, m := range rnVarRequireRe.FindAllStringSubmatch(s, -1) {
		out[m[1]] = m[2]
	}
	for _, m := range rnDestructureRe.FindAllStringSubmatch(s, -1) {
		for _, raw := range strings.Split(m[1], ",") {
			name := strings.TrimSpace(raw)
			if name == "" {
				continue
			}
			if parts := strings.Fields(name); len(parts) == 3 && parts[1] == "as" {
				// `Foo as Baz` → local Baz binds module Foo.
				out[rnTrimIdent(parts[2])] = rnTrimIdent(parts[0])
				continue
			}
			if ident := rnTrimIdent(name); ident != "" {
				out[ident] = ident
			}
		}
	}
	return out
}

// rnTrimIdent returns the leading identifier of s, dropping any trailing
// `: Type` annotation or `= default` the destructure entry may carry.
func rnTrimIdent(s string) string {
	s = strings.TrimSpace(s)
	for i, r := range s {
		isIdent := r == '_' || r == '$' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9' && i > 0)
		if !isIdent {
			return s[:i]
		}
	}
	return s
}

// detectRNNativeModule returns the React Native module name a member
// call's receiver addresses, or "" when the receiver is not a native
// module. Handles inline receivers and bindings recorded in varMap.
func detectRNNativeModule(receiver string, varMap map[string]string) string {
	receiver = strings.TrimSpace(receiver)
	if m := rnInlineNativeModulesRe.FindStringSubmatch(receiver); m != nil {
		return m[1]
	}
	if m := rnInlineRequireRe.FindStringSubmatch(receiver); m != nil {
		return m[1]
	}
	if mod, ok := varMap[receiver]; ok {
		return mod
	}
	return ""
}

// rnNativePlaceholder is the unresolved target a JS native-module call is
// emitted onto for the React-Native bridge synthesizer to land on the
// native implementation.
func rnNativePlaceholder(module, method string) string {
	return "unresolved::rn::" + module + "::" + method
}
