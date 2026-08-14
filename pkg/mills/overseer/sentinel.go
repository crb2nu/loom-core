package overseer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// Sentinel action vocabulary.
const (
	sentinelActor = "overseer.sentinel"

	actionSuppressAdmission = "suppress_admission" // close new-work admission while unhealthy
	actionFileIssue         = "file_issue"         // file/update the dedup-marked incident issue
	actionIncidentOpened    = "incident_opened"    // observation: probe tripped the threshold
	actionIncidentCleared   = "incident_cleared"   // observation: probe recovered

	sentinelSubjectKind = "probe"
)

// sentinelIssueKey is the synthetic backlog-shaped key embedded in the
// escalation dedup marker so the EXISTING GitLab dedup/auto-close machinery
// (marker match over open `mills-escalation` issues) works for sentinel
// incidents without new client capabilities.
func sentinelIssueKey(probe string) string { return "overseer-sentinel:" + probe }

// probeState is the per-probe trip counter + incident bookkeeping.
type probeState struct {
	consecutiveFails int
	incidentOpen     bool
	issueIID         int64
	lastErr          string
}

// Suppression is the TTL-bounded admission-suppression lease. It is
// re-asserted every tick while an incident is open; the TTL is the dead-man's
// switch — a sentinel that dies mid-incident can never suppress admission
// past its last lease.
type Suppression struct {
	Reason string    `json:"reason"`
	Until  time.Time `json:"until"`
}

// Sentinel is the deployment-health overseer: it probes the operator's hard
// dependencies (FlexInfer, GitLab, HUD spawn, Loki, …) and, after
// trips_to_open consecutive failures, opens an incident — optionally
// suppressing new work admission and filing a dedup-marked GitLab issue —
// then clears everything on the first success. No LLM involvement: probe
// truth is deterministic.
type Sentinel struct {
	Probes   []Probe
	Policy   func() *mills.Policy
	Recorder *ActionRecorder
	// Issues is optional; pipeline.DedupIssueClient / ClosableIssueClient
	// capabilities are type-asserted like the Escalator does.
	Issues pipeline.IssueClient
	Logger *slog.Logger
	Now    func() time.Time

	mu          sync.Mutex
	state       map[string]*probeState
	suppression atomic.Pointer[Suppression]
	// suppressPlanned dedups the dry-run "would suppress" event per incident
	// episode (set on plan, cleared when every incident clears). Guarded by mu.
	suppressPlanned bool
}

// Name implements Agent.
func (s *Sentinel) Name() string { return "sentinel" }

// SuppressAdmission reports whether new work admission should be closed.
// Fail-safe on every axis: no suppression, an expired lease, a disabled
// sentinel, a dry-run sentinel, or a withdrawn allow flag all read false.
func (s *Sentinel) SuppressAdmission() bool {
	if s == nil {
		return false
	}
	sup := s.suppression.Load()
	if sup == nil || !s.now().Before(sup.Until) {
		return false
	}
	pol := s.policy()
	if pol == nil || !pol.SentinelEnabled() || !pol.Overseers.Sentinel.Allow.SuppressAdmission {
		return false
	}
	if mills.DryRunOn(pol.Overseers.Sentinel.DryRun) {
		return false
	}
	return true
}

// CurrentSuppression returns the live lease (nil when none/expired) for the
// status endpoint.
func (s *Sentinel) CurrentSuppression() *Suppression {
	if s == nil {
		return nil
	}
	sup := s.suppression.Load()
	if sup == nil || !s.now().Before(sup.Until) {
		return nil
	}
	return sup
}

// Tick implements Agent: run every probe concurrently, advance trip
// counters, open/clear incidents, and (re-)assert or clear the suppression
// lease.
func (s *Sentinel) Tick(ctx context.Context) (TickResult, error) {
	res := TickResult{}
	if s == nil || s.Recorder == nil {
		return res, errors.New("sentinel: not configured")
	}
	pol := s.policy()
	if pol == nil || !pol.SentinelEnabled() {
		return res, nil
	}
	sp := pol.Overseers.Sentinel
	dryRun := mills.DryRunOn(sp.DryRun)
	if s.state == nil {
		s.state = make(map[string]*probeState, len(s.Probes))
	}

	type outcome struct {
		name string
		err  error
	}
	outcomes := make([]outcome, len(s.Probes))
	var wg sync.WaitGroup
	for i, p := range s.Probes {
		wg.Add(1)
		go func(i int, p Probe) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, sp.ProbeTimeout())
			defer cancel()
			outcomes[i] = outcome{name: p.Name(), err: p.Check(pctx)}
		}(i, p)
	}
	wg.Wait()
	res.Inspected = len(outcomes)

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, oc := range outcomes {
		st := s.state[oc.name]
		if st == nil {
			st = &probeState{}
			s.state[oc.name] = st
		}
		if oc.err == nil {
			if st.incidentOpen {
				s.clearIncident(ctx, &res, oc.name, st, dryRun)
			}
			st.consecutiveFails = 0
			st.lastErr = ""
			continue
		}
		st.consecutiveFails++
		st.lastErr = oc.err.Error()
		if s.Logger != nil {
			s.Logger.Warn("sentinel probe failed", "probe", oc.name,
				"consecutive", st.consecutiveFails, "error", oc.err)
		}
		if st.consecutiveFails >= sp.TripThreshold() && !st.incidentOpen {
			s.openIncident(ctx, &res, oc.name, st, sp, dryRun)
		}
	}

	s.reconcileSuppression(ctx, &res, sp, dryRun)
	mills.OverseerSuppressionActive.WithLabelValues(s.Name()).Set(boolGauge(s.suppression.Load() != nil))
	return res, nil
}

// openIncident marks the probe's incident open, records the observation, and
// performs the allowed actions (issue filing; suppression is asserted by
// reconcileSuppression from the aggregate incident set).
func (s *Sentinel) openIncident(ctx context.Context, res *TickResult, name string, st *probeState, sp mills.SentinelPolicy, dryRun bool) {
	st.incidentOpen = true
	if err := s.Recorder.Observe(ctx, actionIncidentOpened, sentinelSubjectKind, name, map[string]any{
		"consecutive_fails": st.consecutiveFails, "error": st.lastErr,
	}); err != nil {
		res.Errored++
	}
	if s.Logger != nil {
		s.Logger.Warn("sentinel incident opened", "probe", name, "error", st.lastErr)
	}
	if s.Issues == nil {
		return
	}
	payload := map[string]any{"error": st.lastErr, "allowed": sp.Allow.FileIssue}
	if dryRun {
		// Plan the would-be filing; the .dryrun kind keeps the committed
		// audit trail truthful.
		if err := s.Recorder.Record(ctx, actionFileIssue, sentinelSubjectKind, name, payload); err != nil {
			res.Errored++
		} else {
			res.Planned++
		}
		return
	}
	if !sp.Allow.FileIssue {
		res.Skipped++ // incident_opened already tells the story; no action event without an action
		return
	}
	iid, err := s.fileIssue(ctx, name, st.lastErr)
	if err != nil {
		res.Errored++
		if s.Logger != nil {
			s.Logger.Warn("sentinel issue filing failed", "probe", name, "error", err)
		}
		return
	}
	st.issueIID = iid
	payload["issue_iid"] = iid
	if err := s.Recorder.Record(ctx, actionFileIssue, sentinelSubjectKind, name, payload); err == nil {
		res.Acted++
	} else {
		res.Errored++
	}
}

// clearIncident records recovery and best-effort closes the incident issue.
func (s *Sentinel) clearIncident(ctx context.Context, res *TickResult, name string, st *probeState, dryRun bool) {
	st.incidentOpen = false
	if err := s.Recorder.Observe(ctx, actionIncidentCleared, sentinelSubjectKind, name, nil); err != nil {
		res.Errored++
	}
	if s.Logger != nil {
		s.Logger.Info("sentinel incident cleared", "probe", name)
	}
	if st.issueIID != 0 && !dryRun {
		if closer, ok := s.Issues.(pipeline.ClosableIssueClient); ok && closer != nil {
			if err := closer.CloseIssue(ctx, st.issueIID); err != nil && s.Logger != nil {
				s.Logger.Warn("sentinel issue close failed", "probe", name, "iid", st.issueIID, "error", err)
			}
		}
	}
	st.issueIID = 0
}

// fileIssue creates or updates the dedup-marked incident issue for a probe,
// reusing the Escalator's marker + capability pattern.
func (s *Sentinel) fileIssue(ctx context.Context, name, reason string) (int64, error) {
	key := sentinelIssueKey(name)
	if dedup, ok := s.Issues.(pipeline.DedupIssueClient); ok && dedup != nil {
		if ref, found, err := dedup.FindOpenEscalation(ctx, key); err == nil && found {
			note := fmt.Sprintf("Recurrence at %s: probe `%s` unhealthy — %s",
				s.now().Format(time.RFC3339), name, reason)
			if cerr := dedup.CommentIssue(ctx, ref.IID, note); cerr != nil {
				return ref.IID, cerr
			}
			return ref.IID, nil
		}
		// A lookup error fails open into a fresh issue, like the Escalator.
	}
	resp, err := s.Issues.CreateIssue(ctx, pipeline.IssueRequest{
		BacklogID: key,
		Title:     fmt.Sprintf("[mills-overseer] dependency unhealthy: %s", name),
		Description: fmt.Sprintf(
			"The Mills deployment-health sentinel opened an incident for probe `%s`.\n\n"+
				"Last error: `%s`\n\nThe incident auto-clears (and this issue auto-closes) on the first healthy probe.\n\n%s\n",
			name, reason, pipeline.EscalationDedupMarker(key)),
		Labels: []string{"mills-escalation", "mills-overseer"},
	})
	if err != nil {
		return 0, err
	}
	return resp.IID, nil
}

// reconcileSuppression derives the admission-suppression lease from the
// aggregate incident set each tick: any open incident (with the action
// allowed and dry-run off) re-asserts a fresh TTL lease; none clears it.
// Callers hold s.mu.
func (s *Sentinel) reconcileSuppression(ctx context.Context, res *TickResult, sp mills.SentinelPolicy, dryRun bool) {
	open := make([]string, 0, len(s.state))
	for name, st := range s.state {
		if st.incidentOpen {
			open = append(open, name)
		}
	}
	sort.Strings(open)

	if len(open) == 0 {
		s.suppressPlanned = false
		if s.suppression.Swap(nil) != nil {
			if err := s.Recorder.Observe(ctx, "suppression_cleared", sentinelSubjectKind, "admission", nil); err != nil {
				res.Errored++
			}
			if s.Logger != nil {
				s.Logger.Info("sentinel admission suppression cleared")
			}
		}
		return
	}

	reason := "unhealthy: " + strings.Join(open, ", ")
	if dryRun || !sp.Allow.SuppressAdmission {
		// Plan the suppression (dry-run only) on the transition into it, not
		// every tick; a disallowed action records nothing — incident_opened
		// already tells the story.
		if dryRun && !s.suppressPlanned {
			s.suppressPlanned = true
			if err := s.Recorder.Record(ctx, actionSuppressAdmission, sentinelSubjectKind, "admission", map[string]any{
				"reason": reason, "allowed": sp.Allow.SuppressAdmission,
			}); err != nil {
				res.Errored++
			} else {
				res.Planned++
			}
		} else if !dryRun {
			res.Skipped++
		}
		// Never hold a live lease in dry-run/disallowed mode (SuppressAdmission
		// would refuse it anyway; keep the stored state honest too).
		s.suppression.Store(nil)
		return
	}

	prev := s.suppression.Load()
	lease := &Suppression{Reason: reason, Until: s.now().Add(sp.SuppressionTTL())}
	s.suppression.Store(lease)
	if prev == nil {
		if err := s.Recorder.Record(ctx, actionSuppressAdmission, sentinelSubjectKind, "admission", map[string]any{
			"reason": reason, "until": lease.Until.Format(time.RFC3339), "allowed": true,
		}); err != nil {
			res.Errored++
		} else {
			res.Acted++
		}
		if s.Logger != nil {
			s.Logger.Warn("sentinel suppressing new work admission", "reason", reason, "until", lease.Until)
		}
	}
}

func (s *Sentinel) policy() *mills.Policy {
	if s.Policy == nil {
		return nil
	}
	return s.Policy()
}

func (s *Sentinel) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}
