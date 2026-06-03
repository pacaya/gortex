package languages

import (
	"regexp"
	"strings"
)

// React Native JVM native-module support. A Java native module is a
// class whose @ReactMethod-annotated methods are callable from JS as
// `NativeModules.<module>.<method>(...)`. The JS module name is the
// string the class's getName() returns (or a @ReactModule(name=...)
// override), defaulting to the class name.

var (
	javaClassDeclRe     = regexp.MustCompile(`\bclass\s+([A-Za-z_]\w*)`)
	javaGetNameReturnRe = regexp.MustCompile(`getName\s*\(\s*\)\s*\{[^{}]*return\s+"([^"]+)"`)
	javaReactModuleNmRe = regexp.MustCompile(`@ReactModule\s*\(\s*name\s*=\s*"([^"]+)"`)
)

// extractJavaRNModuleNames maps each class name in src to the JS module
// name it exposes to React Native. Resolution order (later wins):
// class name, getName() string-literal return, @ReactModule(name=...).
func extractJavaRNModuleNames(src []byte) map[string]string {
	s := string(src)
	out := map[string]string{}
	prevEnd := 0
	for _, loc := range javaClassDeclRe.FindAllStringSubmatchIndex(s, -1) {
		className := s[loc[2]:loc[3]]
		module := className

		// Class body span (balanced braces) for getName() inspection.
		bodyOpen := strings.IndexByte(s[loc[1]:], '{')
		bodyEnd := len(s)
		if bodyOpen >= 0 {
			bodyOpen += loc[1]
			if e := javaMatchBrace(s, bodyOpen); e > bodyOpen {
				bodyEnd = e
				if m := javaGetNameReturnRe.FindStringSubmatch(s[bodyOpen:bodyEnd]); m != nil {
					module = m[1]
				}
			}
		}

		// @ReactModule(name=...) sits in the annotation band just before
		// the class keyword — search the gap since the previous class.
		if m := javaReactModuleNmRe.FindStringSubmatch(s[prevEnd:loc[0]]); m != nil {
			module = m[1]
		}

		out[className] = module
		prevEnd = bodyEnd
	}
	return out
}

// javaMatchBrace returns the index just past the '}' matching the '{' at
// openIdx, or -1 when unbalanced.
func javaMatchBrace(s string, openIdx int) int {
	if openIdx < 0 || openIdx >= len(s) || s[openIdx] != '{' {
		return -1
	}
	depth := 0
	for i := openIdx; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// javaHasReactMethod reports whether any annotation is @ReactMethod.
func javaHasReactMethod(annos []javaAnnotation) bool {
	for _, a := range annos {
		if a.name == "ReactMethod" {
			return true
		}
	}
	return false
}

// javaReactPropRe matches @ReactProp(name = "x") — the marker of a React
// Native view-manager property.
var javaReactPropRe = regexp.MustCompile(`@ReactProp\s*\(\s*name\s*=\s*"([^"]+)"`)

// javaFabricManager is a native view manager discovered in a Java file.
type javaFabricManager struct {
	component string
	props     []string
	line      int
}

// extractJavaFabricManagers finds classes carrying @ReactProp methods
// (React Native view managers) and resolves the JS component name from
// getName() / @ReactModule (rnModules) or the class name, stripping the
// RN "Manager" suffix. rnModules is the class→module map from
// extractJavaRNModuleNames.
func extractJavaFabricManagers(src []byte, rnModules map[string]string) []javaFabricManager {
	s := string(src)
	propLocs := javaReactPropRe.FindAllStringSubmatchIndex(s, -1)
	if len(propLocs) == 0 {
		return nil
	}
	type span struct {
		name       string
		start, end int
	}
	var spans []span
	for _, loc := range javaClassDeclRe.FindAllStringSubmatchIndex(s, -1) {
		className := s[loc[2]:loc[3]]
		end := len(s)
		if bodyOpen := strings.IndexByte(s[loc[1]:], '{'); bodyOpen >= 0 {
			bodyOpen += loc[1]
			if e := javaMatchBrace(s, bodyOpen); e > bodyOpen {
				end = e
			}
		}
		spans = append(spans, span{name: className, start: loc[0], end: end})
	}
	classForOff := func(off int) (string, bool) {
		for _, sp := range spans {
			if off >= sp.start && off < sp.end {
				return sp.name, true
			}
		}
		return "", false
	}
	propsByClass := map[string][]string{}
	lineByClass := map[string]int{}
	var order []string
	for _, loc := range propLocs {
		cls, ok := classForOff(loc[0])
		if !ok {
			continue
		}
		if _, seen := propsByClass[cls]; !seen {
			order = append(order, cls)
			lineByClass[cls] = lineAt(src, loc[0])
		}
		propsByClass[cls] = append(propsByClass[cls], s[loc[2]:loc[3]])
	}
	var out []javaFabricManager
	for _, cls := range order {
		comp := cls
		if m := rnModules[cls]; m != "" {
			comp = m
		}
		out = append(out, javaFabricManager{
			component: objcComponentName(comp), // shared "Manager"-suffix stripper
			props:     propsByClass[cls],
			line:      lineByClass[cls],
		})
	}
	return out
}
