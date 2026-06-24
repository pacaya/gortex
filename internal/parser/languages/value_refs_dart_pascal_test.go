package languages

import (
	"testing"
)

// TestValueRefKind_DartConst pins that a distinctive top-level Dart `const`
// declaration is kinded as a constant and its read is captured as a value-ref
// candidate.
func TestValueRefKind_DartConst(t *testing.T) {
	src := []byte(`const MAX_RETRIES = 3;

void useIt() {
  print(MAX_RETRIES);
}
`)
	res, err := NewDartExtractor().Extract("cfg.dart", src)
	if err != nil {
		t.Fatal(err)
	}
	if constNode(res, "MAX_RETRIES") == nil {
		t.Error("Dart distinctive const MAX_RETRIES should be KindConstant")
	}
	if !valueRefRead(res, "MAX_RETRIES") {
		t.Error("read of MAX_RETRIES should be captured as a value-ref candidate")
	}
}

// TestValueRefKind_DartFinal pins that a distinctive top-level Dart `final`
// declaration (immutable by grammar) is kinded as a constant and its read is
// captured.
func TestValueRefKind_DartFinal(t *testing.T) {
	src := []byte(`final API_BASE = 'https://x';

void useIt() {
  print(API_BASE);
}
`)
	res, err := NewDartExtractor().Extract("cfg.dart", src)
	if err != nil {
		t.Fatal(err)
	}
	if constNode(res, "API_BASE") == nil {
		t.Error("Dart distinctive final API_BASE should be KindConstant")
	}
	if !valueRefRead(res, "API_BASE") {
		t.Error("read of API_BASE should be captured as a value-ref candidate")
	}
}

// TestValueRefKind_DartLowerVarNotConst pins the precision boundary: an ordinary
// lowerCamelCase top-level `var` stays a KindVariable and emits no value-ref
// candidate (the distinctive-name gate filters it out).
func TestValueRefKind_DartLowerVarNotConst(t *testing.T) {
	src := []byte(`var counter = 0;

void useIt() {
  print(counter);
}
`)
	res, err := NewDartExtractor().Extract("cfg.dart", src)
	if err != nil {
		t.Fatal(err)
	}
	if constNode(res, "counter") != nil {
		t.Error("ordinary var counter must not be re-kinded to KindConstant")
	}
	if valueRefRead(res, "counter") {
		t.Error("non-distinctive var counter must not be captured as a value-ref candidate")
	}
}

// TestValueRefKind_PascalConst pins that a Pascal `const` declaration is kinded
// as a constant and its read is captured as a value-ref candidate.
func TestValueRefKind_PascalConst(t *testing.T) {
	src := []byte(`unit Foo;
interface
const
  MAX_RETRIES = 3;
implementation
procedure UseIt;
begin
  WriteLn(MAX_RETRIES);
end;
end.
`)
	res, err := NewPascalExtractor().Extract("foo.pas", src)
	if err != nil {
		t.Fatal(err)
	}
	if constNode(res, "MAX_RETRIES") == nil {
		t.Error("Pascal const MAX_RETRIES should be KindConstant")
	}
	if !valueRefRead(res, "MAX_RETRIES") {
		t.Error("read of MAX_RETRIES should be captured as a value-ref candidate")
	}
}
