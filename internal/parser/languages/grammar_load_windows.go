//go:build windows

package languages

import (
	"fmt"
	"syscall"
	"unsafe"
)

// loadGrammarLanguage loads a compiled tree-sitter grammar DLL, looks
// up the named entry point, invokes it, and returns the resulting
// TSLanguage pointer. The DLL handle is intentionally leaked: the
// TSLanguage must outlive every parse, and the set of custom grammars
// is fixed for the process lifetime.
func loadGrammarLanguage(libPath, symbol string) (unsafe.Pointer, error) {
	dll, err := syscall.LoadDLL(libPath)
	if err != nil {
		return nil, fmt.Errorf("LoadDLL: %w", err)
	}
	proc, err := dll.FindProc(symbol)
	if err != nil {
		_ = dll.Release()
		return nil, fmt.Errorf("FindProc %s: %w", symbol, err)
	}
	// A tree-sitter grammar entry point is `const TSLanguage *fn(void)`;
	// its return value arrives in r1 as the TSLanguage pointer.
	r1, _, _ := proc.Call()
	if r1 == 0 {
		_ = dll.Release()
		return nil, fmt.Errorf("entry point %s returned a nil TSLanguage", symbol)
	}
	return unsafe.Pointer(r1), nil //nolint:govet,unsafeptr // r1 is a C pointer returned by the grammar entry point
}
