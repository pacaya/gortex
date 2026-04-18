// Package internalutil holds helpers shared across agent adapters.
// Kept separate from `agents` itself so adapters don't create
// import cycles by depending on logging helpers that in turn import
// something agent-specific.
package internalutil

import (
	"fmt"
	"io"
)

// Logf writes a progress line to w when w is non-nil. Adapter
// packages use this for consistency — every "[gortex init] …"
// message goes through the same helper.
func Logf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}

// Warnf writes a "[gortex init] warning: …" prefixed line.
func Warnf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	_, _ = fmt.Fprintf(w, "[gortex init] warning: "+format+"\n", args...)
}
