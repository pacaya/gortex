package graph

import "testing"

func TestUnwrapCppSmartPointer(t *testing.T) {
	cases := map[string]string{
		"std::unique_ptr<Widget>":             "Widget",
		"unique_ptr<Widget>":                  "Widget",
		"std::shared_ptr<Widget>":             "Widget",
		"std::weak_ptr<Widget>":               "Widget",
		"std::optional<Widget>":               "Widget",
		"const std::shared_ptr<Widget> &":     "Widget",
		"std::unique_ptr<Widget, MyDeleter>":  "Widget",       // extra deleter arg dropped
		"std::optional<std::shared_ptr<Foo>>": "Foo",          // nested wrappers peeled
		"std::unique_ptr<ns::Widget>":         "ns::Widget",   // pointee namespace kept
		"std::vector<Widget>":                 "std::vector<Widget>", // non-wrapper unchanged
		"Widget":                              "Widget",
		"int":                                 "int",
	}
	for in, want := range cases {
		if got := UnwrapCppSmartPointer(in); got != want {
			t.Errorf("UnwrapCppSmartPointer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeCppType_SmartPointerPointee(t *testing.T) {
	cases := map[string]string{
		"std::unique_ptr<Widget>":              "Widget",
		"shared_ptr<Widget>":                   "Widget",
		"std::optional<ns::Widget>":            "Widget", // last :: segment, post-unwrap
		"std::unique_ptr<std::vector<Widget>>": "vector", // unwrap to vector<Widget>, then strip args
		"std::vector<Widget>":                  "vector", // non-wrapper still strips to container
	}
	for in, want := range cases {
		if got := NormalizeCppType(in); got != want {
			t.Errorf("NormalizeCppType(%q) = %q, want %q", in, got, want)
		}
	}
}
