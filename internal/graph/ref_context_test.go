package graph

import "testing"

func TestRefContextOf(t *testing.T) {
	cases := []struct {
		name string
		kind EdgeKind
		from NodeKind
		meta map[string]any
		want string
	}{
		{"return", EdgeReturns, KindFunction, nil, RefContextReturnType},
		{"param", EdgeTypedAs, KindParam, nil, RefContextParameterType},
		{"field", EdgeTypedAs, KindField, nil, RefContextField},
		{"value_var", EdgeTypedAs, KindVariable, nil, RefContextValue},
		{"value_local", EdgeTypedAs, KindLocal, nil, RefContextValue},
		{"typed_other", EdgeTypedAs, KindType, nil, RefContextType},
		{"attribute", EdgeAnnotated, KindFunction, nil, RefContextAttribute},
		{"type_ref", EdgeReferences, KindFunction, nil, RefContextType},
		{"implements", EdgeImplements, KindType, nil, RefContextType},
		{"read", EdgeReads, KindFunction, nil, RefContextValue},
		{"call", EdgeCalls, KindFunction, nil, RefContextCall},
		{"instantiate", EdgeInstantiates, KindFunction, nil, RefContextCall},
		{"meta_override", EdgeReferences, KindFunction, map[string]any{"ref_context": RefContextGenericArg}, RefContextGenericArg},
		{"no_context", EdgeImports, KindFile, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := &Edge{Kind: c.kind, Meta: c.meta}
			if got := RefContextOf(e, c.from); got != c.want {
				t.Errorf("RefContextOf(%s, from=%s) = %q, want %q", c.kind, c.from, got, c.want)
			}
		})
	}
	if RefContextOf(nil, KindFunction) != "" {
		t.Error("nil edge must classify as empty context")
	}
}
