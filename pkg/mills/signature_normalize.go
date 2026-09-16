package mills

import "github.com/crb2nu/loom/pkg/mills/sigfp"

// The signature normalization moved to the leaf package pkg/mills/sigfp so
// the pipeline runner can stamp failure fingerprints at escalation time
// (pkg/mills imports pipeline; pipeline cannot import pkg/mills). These
// aliases keep the miner's call sites and behavior byte-identical.

func normalizeEvidenceTokens(text string) []string { return sigfp.NormalizeEvidenceTokens(text) }

func isSignaturePlaceholder(token string) bool { return sigfp.IsPlaceholder(token) }

const signatureMaxTokens = sigfp.MaxTokens
