package graph

import "strings"

// NormalizeCppType reduces a C++ type spelling to a stable comparison key used
// by both the extractor (stamping parameter types) and the overload resolver
// (normalizing call-site argument hints), so the two always compare in the same
// space. It strips template arguments, cv-qualifiers, ref/ptr punctuation, and
// namespace qualifiers, and canonicalises a few stdlib aliases — while keeping
// the integer/float ladder distinct (int vs long) so genuinely different
// overloads stay rankable.
func NormalizeCppType(raw string) string {
	// Reduce a smart-pointer / optional wrapper to its pointee before stripping
	// template arguments — `unique_ptr<Widget>` normalises to `Widget`, not
	// `unique_ptr` — so member access through the wrapper lands on the pointee.
	raw = UnwrapCppSmartPointer(raw)
	s := stripCppTemplateArgs(raw)
	s = strings.ReplaceAll(s, "&&", " ")
	s = strings.ReplaceAll(s, "&", " ")
	s = strings.ReplaceAll(s, "*", " ")
	fields := strings.Fields(s)
	kept := fields[:0]
	for _, f := range fields {
		if f == "const" || f == "volatile" {
			continue
		}
		kept = append(kept, f)
	}
	s = strings.Join(kept, " ")
	if i := strings.LastIndex(s, "::"); i >= 0 {
		s = s[i+2:]
	}
	s = strings.TrimSpace(s)
	switch s {
	case "string", "basic_string", "string_view":
		return "string"
	case "unsigned", "unsigned int", "uint", "size_t", "uint32_t", "uint64_t":
		return "unsigned"
	}
	return s
}

// cppSmartPointerWrappers names the single-pointee standard wrappers whose
// `<T>` is the type a member access (`p->m()`) or dereference (`*p`) actually
// reaches — each forwards operator-> / operator* (or, for optional, value
// access) to its pointee.
var cppSmartPointerWrappers = map[string]bool{
	"unique_ptr": true,
	"shared_ptr": true,
	"weak_ptr":   true,
	"optional":   true,
}

// UnwrapCppSmartPointer reduces a smart-pointer / optional wrapper to its
// pointee type text — `std::unique_ptr<Widget>` → `Widget`,
// `optional<shared_ptr<const Foo>>` → `const Foo`, `unique_ptr<Widget, Del>` →
// `Widget` — so a member access through the wrapper resolves on the pointee
// rather than the wrapper type. A non-wrapper type, including other generics
// like `vector<T>`, is returned unchanged. const / volatile qualifiers and
// ref / pointer suffixes on the wrapper are tolerated; nested wrappers are
// peeled until a non-wrapper type remains.
func UnwrapCppSmartPointer(s string) string {
	for {
		t := strings.TrimSpace(s)
		t = strings.TrimPrefix(t, "const ")
		t = strings.TrimPrefix(t, "volatile ")
		t = strings.TrimRight(strings.TrimSpace(t), " &*")
		t = strings.TrimSpace(t)
		lt := strings.IndexByte(t, '<')
		if lt <= 0 || !strings.HasSuffix(t, ">") {
			return s
		}
		head := strings.TrimSpace(t[:lt])
		if i := strings.LastIndex(head, "::"); i >= 0 {
			head = head[i+2:]
		}
		if !cppSmartPointerWrappers[head] {
			return s
		}
		s = cppFirstTemplateArg(strings.TrimSpace(t[lt+1 : len(t)-1]))
	}
}

// cppFirstTemplateArg returns the first comma-separated template argument,
// respecting nested `<…>` so `Widget, Deleter` yields `Widget` and
// `map<int, Foo>` stays whole.
func cppFirstTemplateArg(args string) string {
	depth := 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				return strings.TrimSpace(args[:i])
			}
		}
	}
	return strings.TrimSpace(args)
}

func stripCppTemplateArgs(s string) string {
	depth := 0
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}
