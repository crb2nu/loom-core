package mills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// Durable identity of a mined classifier-signature proposal. Exported so the
// operator's read endpoint selects the same actor + kind the sweep writes.
const (
	// SignatureCandidateEventKind marks a proposed classifier signature: a
	// phrase shared by several escalations no live classifier explains. It is
	// DATA, never enforcement — promoting a candidate to a real signature stays
	// a reviewed code change in pkg/mills/pipeline.
	SignatureCandidateEventKind = "signature.candidate"
	// SignatureMinerActor is the sole writer of that kind, so the read path can
	// select on actor (an indexed window scan) and filter on kind.
	SignatureMinerActor = "reconciler.signature_miner"
	// signatureCandidateSubjectKind keys the first-writer dedup on the phrase
	// fingerprint: one candidate per distinct phrase, however many sweeps
	// re-observe the same cluster in an overlapping window.
	signatureCandidateSubjectKind = "signature_phrase"
)

// signatureMiningStopPhrases are normalized command fragments that recur in
// unrelated pipeline failures but do not describe the failure itself. Keep the
// entries in the same normalized form stored in candidate events so additions
// are easy to review alongside observed miner output.
var signatureMiningStopPhrases = map[string]struct{}{
	"go test <path>":               {},
	"go test <path> <path>":        {},
	"go test <path> <path> <path>": {},
}

const (
	// DefaultSignatureMiningInterval is how often the sweep runs when the
	// operator sets no interval. Six hours: the corpus is a two-week window of
	// human-paced escalations, so a faster cadence re-derives the same clusters
	// at real CPU cost and no new information.
	DefaultSignatureMiningInterval = 6 * time.Hour
	// defaultSignatureMiningLookback bounds the corpus. Two weeks is long
	// enough for a recurring external failure to repeat three times and short
	// enough that a shape already fixed stops being proposed.
	defaultSignatureMiningLookback = 336 * time.Hour
	// signatureMiningScanLimit bounds the evidence read; the n-gram index is
	// O(texts × tokens), so the corpus is capped rather than the sweep's time.
	signatureMiningScanLimit = 500
	// signatureMaxSamples / signatureSampleMaxChars bound the raw evidence
	// carried in a candidate payload. Enough to recognise the failure at a
	// glance; not a log archive in the events table.
	signatureMaxSamples     = 3
	signatureSampleMaxChars = 300
	// signatureMiningSweepTimeout reserves part of the tick budget for the
	// sweep's single store read plus in-process clustering.
	signatureMiningSweepTimeout = 30 * time.Second
)

// SignatureMiningSweepResult summarises one signature-candidate mining pass.
type SignatureMiningSweepResult struct {
	// TextsScanned is the number of escalations in the window that carried
	// usable free-text evidence, classified or not. It is the denominator the
	// shadow match count is measured against.
	TextsScanned int
	// Unclassified is how many of those no live classifier explains — the
	// corpus actually clustered.
	Unclassified int
	// Clustered is how many unclassified texts landed in some cluster.
	Clustered int
	// Candidates is the number of NEW candidates written this sweep;
	// re-proposing an already-recorded phrase does not count.
	Candidates int
	// Errored is the number of candidate writes that failed; the cluster is
	// re-derived on a later sweep.
	Errored int
}

// SweepSignatureMining proposes classifier signatures from the failures the
// factory could not explain.
//
// Each pass reads the escalations of the lookback window, drops the ones a live
// classifier already matches, normalizes the rest (ids, paths, numbers, and
// durations collapse to placeholders), and groups what is left by the longest
// token phrase at least three of them share. Every surviving cluster is written
// as a candidate event carrying the phrase, its support, sample evidence, and
// the number of escalations across the WHOLE window the phrase would match if
// it were promoted — the shadow evaluation that makes over-firing visible
// before a human writes the rule.
//
// Nothing here enforces anything: no run is reclassified, no retry decision
// changes. The output exists so growing the classifier is a data-driven code
// change instead of an incident-driven one.
//
// Exactly-once per phrase: the candidate is a first-writer event keyed on
// (kind, subject_kind=signature_phrase, subject_id=<phrase fingerprint>), so
// repeated sweeps over an overlapping window converge instead of accumulating
// duplicates.
//
// Best-effort: a nil classifier disables the sweep, a store read failure
// returns an error the caller logs (never wedging the tick), and a failed
// individual write is counted and retried on a later pass.
func (r *Reconciler) SweepSignatureMining(ctx context.Context) (SignatureMiningSweepResult, error) {
	res := SignatureMiningSweepResult{}
	if r == nil || r.Store == nil || r.Store.Events == nil || r.Store.Pipeline == nil {
		return res, errors.New("reconciler: not configured")
	}
	if r.SignatureEvidenceClassified == nil {
		// Without the real classifiers the sweep cannot tell an unexplained
		// failure from an explained one, and would propose signatures for
		// shapes the factory already handles. Off is the honest state.
		return res, nil
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}

	since := r.now().Add(-r.signatureMiningLookback())
	rows, err := r.Store.Pipeline.ListEscalationEvidence(ctx, since, signatureMiningScanLimit)
	if err != nil {
		if cancelErr := contextCancellationError(ctx, err); cancelErr != nil {
			return res, cancelErr
		}
		SignatureMiningErrorsTotal.WithLabelValues("list_evidence").Inc()
		return res, fmt.Errorf("list escalation evidence: %w", err)
	}

	// window holds every scanned text (classified or not): a proposed phrase is
	// scored against all of them, because a phrase that also fires on already
	// explained failures is a phrase that would mis-classify once promoted.
	var (
		window       []signatureDoc
		mined        []signatureDoc
		minedRecords []*store.EscalationEvidence
	)
	for _, row := range rows {
		if row == nil {
			continue
		}
		tokens := normalizeEvidenceTokens(row.Evidence)
		if len(tokens) < signatureMinPhraseTokens {
			continue
		}
		doc := signatureDoc{Tokens: tokens}
		window = append(window, doc)
		if row.Classified || r.SignatureEvidenceClassified(row.Evidence) {
			continue
		}
		mined = append(mined, doc)
		minedRecords = append(minedRecords, row)
	}
	res.TextsScanned = len(window)
	res.Unclassified = len(mined)
	SignatureMiningTextsScannedTotal.Add(float64(res.TextsScanned))
	if res.Unclassified < signatureMinClusterSize {
		return res, nil
	}

	for _, cluster := range clusterSignatureDocs(mined) {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.Clustered += len(cluster.Members)
		appended, err := r.appendSignatureCandidate(ctx, cluster, minedRecords, window)
		if err != nil {
			if cancelErr := contextCancellationError(ctx, err); cancelErr != nil {
				return res, cancelErr
			}
			res.Errored++
			continue
		}
		if appended {
			res.Candidates++
			SignatureCandidatesTotal.Inc()
		}
	}
	return res, nil
}

// appendSignatureCandidate writes the first-writer candidate event for one
// cluster. It returns (appended, err) mirroring AppendOnceBySubjectKind:
// appended is false when this phrase was already proposed, which is what keeps
// the counter and every downstream consumer exactly-once across sweeps.
func (r *Reconciler) appendSignatureCandidate(
	ctx context.Context,
	cluster signatureCluster,
	records []*store.EscalationEvidence,
	window []signatureDoc,
) (bool, error) {
	phrase := cluster.PhraseText()
	if isSignatureMiningStopPhrase(phrase) {
		return false, nil
	}
	var (
		firstSeen time.Time
		lastSeen  time.Time
		samples   = make([]string, 0, signatureMaxSamples)
	)
	for _, idx := range cluster.Members {
		rec := records[idx]
		if firstSeen.IsZero() || rec.StartedAt.Before(firstSeen) {
			firstSeen = rec.StartedAt
		}
		if rec.StartedAt.After(lastSeen) {
			lastSeen = rec.StartedAt
		}
		if len(samples) < signatureMaxSamples {
			samples = append(samples, truncateSignatureSample(rec.Evidence))
		}
	}

	// Shadow evaluation: how many escalations in the window this phrase would
	// match if promoted, over the WHOLE window rather than just the cluster.
	matches := 0
	for _, doc := range window {
		if containsPhrase(doc.Tokens, cluster.Phrase) {
			matches++
		}
	}

	appended, err := r.Store.Events.AppendOnceBySubjectKind(ctx, &store.Event{
		Actor:       SignatureMinerActor,
		Kind:        SignatureCandidateEventKind,
		SubjectKind: signatureCandidateSubjectKind,
		SubjectID:   signaturePhraseFingerprint(phrase),
		Payload: map[string]any{
			"phrase":       phrase,
			"member_count": len(cluster.Members),
			// Raw snippets, not the normalized form: a reviewer needs to see
			// the actual failure to judge whether the phrase describes it.
			"sample_evidence": samples,
			// RFC3339 strings, not time.Time: the payload round-trips through
			// JSON, and pinning the wire form here keeps the in-memory and
			// read-back shapes identical.
			"first_seen":         firstSeen.UTC().Format(time.RFC3339),
			"last_seen":          lastSeen.UTC().Format(time.RFC3339),
			"window_match_count": matches,
		},
	})
	// A cancelled sweep is not a mining failure — the caller unwinds and the
	// cluster is re-derived next pass — so it neither counts nor logs.
	if err != nil && contextCancellationError(ctx, err) == nil {
		SignatureMiningErrorsTotal.WithLabelValues("append_event").Inc()
		if r.Logger != nil {
			r.Logger.Warn("reconciler: append signature candidate failed",
				"phrase", phrase, "members", len(cluster.Members), "error", err)
		}
	}
	return appended, err
}

// isSignatureMiningStopPhrase normalizes its input before lookup so callers
// cannot bypass the guard with case, punctuation, or concrete path variants.
func isSignatureMiningStopPhrase(phrase string) bool {
	normalized := strings.Join(normalizeEvidenceTokens(phrase), " ")
	_, stopped := signatureMiningStopPhrases[normalized]
	return stopped
}

// signaturePhraseFingerprint is the durable identity of a proposal. It hashes
// the NORMALIZED phrase, so the same failure shape observed with different ids
// and paths converges on one candidate.
func signaturePhraseFingerprint(phrase string) string {
	sum := sha256.Sum256([]byte(phrase))
	return hex.EncodeToString(sum[:])[:16]
}

// truncateSignatureSample bounds one raw evidence snippet. Truncation is by
// bytes on a UTF-8 boundary: log tails are effectively ASCII, and a mid-rune
// cut would render as a replacement character in the operator UI.
func truncateSignatureSample(evidence string) string {
	if len(evidence) <= signatureSampleMaxChars {
		return evidence
	}
	cut := signatureSampleMaxChars
	for cut > 0 && !utf8.RuneStart(evidence[cut]) {
		cut--
	}
	return evidence[:cut]
}

func (r *Reconciler) signatureMiningLookback() time.Duration {
	if r != nil && r.SignatureMiningLookback > 0 {
		return r.SignatureMiningLookback
	}
	return defaultSignatureMiningLookback
}

func (r *Reconciler) signatureMiningInterval() time.Duration {
	if r != nil && r.SignatureMiningInterval > 0 {
		return r.SignatureMiningInterval
	}
	return DefaultSignatureMiningInterval
}

// signatureMiningDue reports whether the interval has elapsed since the last
// attempt. The schedule is process-local (ticks are serial, so no locking): a
// restart merely runs one extra pass, which is idempotent.
func (r *Reconciler) signatureMiningDue(now time.Time) bool {
	if r == nil || r.Store == nil || r.Store.Pipeline == nil || r.SignatureEvidenceClassified == nil {
		return false
	}
	return !now.Before(r.nextSignatureMining)
}
