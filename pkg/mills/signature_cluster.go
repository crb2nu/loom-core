package mills

import (
	"sort"
	"strings"
)

const (
	// signatureMinClusterSize is how many distinct escalations must share a
	// phrase before it is worth proposing. Two is a coincidence; three is the
	// smallest number that reads as a recurring shape.
	signatureMinClusterSize = 3
	// signatureMinPhraseTokens / signatureMaxPhraseTokens bound the n-gram
	// length. Below three tokens a phrase is generic enough to match unrelated
	// failures; above eight it starts carrying run-specific prose that would
	// stop matching the next occurrence.
	signatureMinPhraseTokens = 3
	signatureMaxPhraseTokens = 8
	// signatureMinPhraseWords is how many of a phrase's tokens must be real
	// words rather than collapse placeholders. Without it the miner happily
	// proposes "<num> <path> <num>", which matches everything and explains
	// nothing.
	signatureMinPhraseWords = 2
)

// signatureDoc is one normalized evidence text the miner works over.
type signatureDoc struct {
	Tokens []string
}

// signatureCluster is a proposed signature: the shared phrase and the indices
// of the documents that carry it. Members are disjoint across clusters, so one
// escalation contributes to at most one proposal per sweep.
type signatureCluster struct {
	Phrase  []string
	Members []int
}

// PhraseText renders the cluster's phrase as the space-joined normalized form
// stored in the candidate event.
func (c signatureCluster) PhraseText() string { return strings.Join(c.Phrase, " ") }

// clusterSignatureDocs groups documents by their longest shared token n-gram.
//
// The algorithm is deliberately literal: index every 3..8-token n-gram of every
// document, keep the ones carried by at least signatureMinClusterSize distinct
// documents, then take them longest-first and let each claim the documents no
// longer phrase has already claimed. No embeddings, no similarity threshold —
// the output has to be readable by whoever reviews the promotion, and "these N
// escalations all contain exactly this phrase" is the only evidence that
// survives being argued with.
func clusterSignatureDocs(docs []signatureDoc) []signatureCluster {
	if len(docs) < signatureMinClusterSize {
		return nil
	}
	index := map[string][]int{}
	for i, doc := range docs {
		for phrase := range signaturePhrases(doc.Tokens) {
			index[phrase] = append(index[phrase], i)
		}
	}

	type candidate struct {
		phrase  string
		tokens  []string
		members []int
	}
	candidates := make([]candidate, 0, len(index))
	for phrase, members := range index {
		if len(members) < signatureMinClusterSize {
			continue
		}
		candidates = append(candidates, candidate{
			phrase:  phrase,
			tokens:  strings.Split(phrase, " "),
			members: members,
		})
	}
	// Longest phrase first (the most specific description of the shape), then
	// widest support, then lexicographic so the sweep is deterministic across
	// runs and across Go map iteration orders.
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if len(a.tokens) != len(b.tokens) {
			return len(a.tokens) > len(b.tokens)
		}
		if len(a.members) != len(b.members) {
			return len(a.members) > len(b.members)
		}
		return a.phrase < b.phrase
	})

	claimed := make([]bool, len(docs))
	var out []signatureCluster
	for _, cand := range candidates {
		members := make([]int, 0, len(cand.members))
		for _, idx := range cand.members {
			if !claimed[idx] {
				members = append(members, idx)
			}
		}
		if len(members) < signatureMinClusterSize {
			continue
		}
		sort.Ints(members)
		for _, idx := range members {
			claimed[idx] = true
		}
		out = append(out, signatureCluster{Phrase: cand.tokens, Members: members})
	}
	return out
}

// signaturePhrases returns the distinct 3..8-token n-grams of one document that
// carry at least signatureMinPhraseWords real words.
func signaturePhrases(tokens []string) map[string]struct{} {
	out := map[string]struct{}{}
	for n := signatureMinPhraseTokens; n <= signatureMaxPhraseTokens; n++ {
		for start := 0; start+n <= len(tokens); start++ {
			window := tokens[start : start+n]
			if !hasEnoughWords(window) {
				continue
			}
			out[strings.Join(window, " ")] = struct{}{}
		}
	}
	return out
}

func hasEnoughWords(tokens []string) bool {
	words := 0
	for _, token := range tokens {
		if !isSignaturePlaceholder(token) {
			words++
			if words >= signatureMinPhraseWords {
				return true
			}
		}
	}
	return false
}

// containsPhrase reports whether phrase appears in tokens as a contiguous
// subsequence. This is the same match a promoted classifier signature would
// perform, which is what makes the shadow count an honest prediction.
func containsPhrase(tokens, phrase []string) bool {
	if len(phrase) == 0 || len(phrase) > len(tokens) {
		return false
	}
	for start := 0; start+len(phrase) <= len(tokens); start++ {
		matched := true
		for i, want := range phrase {
			if tokens[start+i] != want {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}
