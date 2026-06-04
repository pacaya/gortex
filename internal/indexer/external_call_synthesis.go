package indexer

import (
	"os"
	"strings"
)

// externalCallSynthesisEnabled reports whether the resolver should
// synthesise placeholder nodes for calls into un-indexed external
// packages / sibling services so call-chain traversals keep the
// external hop visible. GORTEX_SYNTH_EXTERNAL_CALLS overrides the
// index.synthesize_external_calls config key. Off by default.
func (idx *Indexer) externalCallSynthesisEnabled() bool {
	if v := os.Getenv("GORTEX_SYNTH_EXTERNAL_CALLS"); v != "" {
		return v == "1" || strings.EqualFold(v, "true")
	}
	return idx.config.ExternalCallSynthesisEnabledOrDefault()
}
