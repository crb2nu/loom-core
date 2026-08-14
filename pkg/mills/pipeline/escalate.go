package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
)

// FailureRecord summarises why a pipeline run was escalated. It is the
// payload of the human-facing GitLab issue + agent handoff.
type FailureRecord struct {
	BacklogID            string                    `json:"backlog_id"`
	PipelineRunID        string                    `json:"pipeline_run_id"`
	Reason               string                    `json:"reason"`
	State                store.PipelineState       `json:"state"`
	CostUSD              float64                   `json:"cost_usd"`
	Attempts             int                       `json:"attempts"`
	EscalationMode       string                    `json:"escalation_mode,omitempty"`
	IncidentCount        int                       `json:"incident_count,omitempty"`
	IncidentWindow       string                    `json:"incident_window,omitempty"`
	Classification       *EscalationClassification `json:"classification,omitempty"`
	EscalationClass      string                    `json:"escalation_class,omitempty"`
	FailureClass         string                    `json:"failure_class,omitempty"`
	Retryable            *bool                     `json:"retryable,omitempty"`
	RetryExhausted       *bool                     `json:"retry_exhausted,omitempty"`
	ExternalDependencyID string                    `json:"external_dependency_id,omitempty"`
	ExternalDependency   string                    `json:"external_dependency,omitempty"`
	StageStack           []FailureStage            `json:"stage_stack"`
	GateVerdicts         []FailureGate             `json:"gate_verdicts"`
	LastLogTail          string                    `json:"last_log_tail,omitempty"`
	GeneratedAt          time.Time                 `json:"generated_at"`
}

// EscalationClassification is the machine-readable classification block emitted
// with a Mills escalation payload. The top-level fields on FailureRecord remain
// for backward compatibility; new consumers should prefer this grouped shape.
// Classifier, FreeRetry, and Terminal carry the full retry semantics of the
// failure taxonomy so downstream planners do not need to re-derive them from
// the class name.
type EscalationClassification struct {
	Classifier           string `json:"classifier,omitempty"`
	EscalationClass      string `json:"escalation_class,omitempty"`
	FailureClass         string `json:"failure_class,omitempty"`
	Retryable            *bool  `json:"retryable,omitempty"`
	RetryExhausted       *bool  `json:"retry_exhausted,omitempty"`
	FreeRetry            *bool  `json:"free_retry,omitempty"`
	Terminal             *bool  `json:"terminal,omitempty"`
	EscalationMode       string `json:"escalation_mode,omitempty"`
	IncidentCount        int    `json:"incident_count,omitempty"`
	IncidentWindow       string `json:"incident_window,omitempty"`
	ExternalDependencyID string `json:"external_dependency_id,omitempty"`
	ExternalDependency   string `json:"external_dependency,omitempty"`
}

// FailureStage is one row of the per-stage history attached to a record.
type FailureStage struct {
	Stage    string  `json:"stage"`
	Attempt  int     `json:"attempt"`
	Outcome  string  `json:"outcome,omitempty"`
	CostUSD  float64 `json:"cost_usd"`
	Duration string  `json:"duration,omitempty"`
}

// FailureGate is one row of the gate-verdict history.
type FailureGate struct {
	Gate       string   `json:"gate"`
	AfterStage string   `json:"after_stage"`
	Outcome    string   `json:"outcome"`
	Reasons    []string `json:"reasons,omitempty"`
}

// IssueClient opens a GitLab issue for the failure record. Implementations
// wrap mcp-gitlab.create_issue; tests inject a fake.
type IssueClient interface {
	CreateIssue(ctx context.Context, req IssueRequest) (IssueResponse, error)
}

// IssueRequest carries the issue title + description + labels.
type IssueRequest struct {
	BacklogID   string
	Title       string
	Description string
	Labels      []string
}

// IssueResponse reports the new issue URL + iid.
type IssueResponse struct {
	IID int64
	URL string
}

// IssueRef identifies an existing GitLab issue.
type IssueRef struct {
	IID int64
	URL string
}

// DedupIssueClient is an OPTIONAL capability an IssueClient may also implement
// so the escalator reuses an existing OPEN escalation issue for a backlog item
// instead of filing a duplicate every run (DEBT-073 / #167 — 67 of 100 open
// issues in loom-core were bot-filed `mills-escalation` incidents burying real
// signal). Escalator type-asserts for it; a client that only implements
// IssueClient keeps the always-create behavior, so this is backward compatible.
type DedupIssueClient interface {
	// FindOpenEscalation returns an existing OPEN escalation issue for the
	// backlog item, matched via EscalationDedupMarker. found=false with a nil
	// error means none exists. The escalator FAILS OPEN on a non-nil error
	// (files a fresh issue) so a lookup failure can never drop an escalation.
	FindOpenEscalation(ctx context.Context, backlogID string) (IssueRef, bool, error)
	// CommentIssue appends a note (the recurrence record) to an existing issue.
	CommentIssue(ctx context.Context, iid int64, body string) error
}

// ClassAwareDedupIssueClient is an additive capability that upgrades a legacy
// DedupIssueClient from backlog-only to (backlog, failure class) matching.
type ClassAwareDedupIssueClient interface {
	FindOpenEscalationByClass(ctx context.Context, backlogID, failureClass string) (IssueRef, bool, error)
}

// ClosableIssueClient is an OPTIONAL capability an IssueClient may also
// implement so the escalator can CLOSE the open escalation issue for a backlog
// item once a later run for that item succeeds (DEBT-073 / #167 auto-close —
// the complement to dedup: dedup stops NEW duplicates, auto-close reaps the
// issue when the item is finally green). A client without it disables
// auto-close (the dedup + recurrence behaviour is unaffected).
type ClosableIssueClient interface {
	// CloseIssue transitions an issue to the closed state.
	CloseIssue(ctx context.Context, iid int64) error
}

// OpenEscalationLister is an additive capability that lets success resolution
// close every class-specific and legacy escalation for one backlog item.
type OpenEscalationLister interface {
	ListOpenEscalations(ctx context.Context, backlogID string) ([]IssueRef, error)
}

// EscalationResolver is an OPTIONAL capability an EscalationHandler may
// implement to resolve (auto-close) the open escalation issue for a backlog
// item after a later pipeline run for that item SUCCEEDS. The runner
// type-asserts for it in markDone; a handler without it is a no-op. Best-effort
// — a resolve error is logged and must never fail the successful run.
type EscalationResolver interface {
	ResolveOnSuccess(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) error
}

// EscalationDedupMarker returns the stable, format-independent marker embedded
// (invisibly) in every escalation issue body so a later run for the same
// backlog item can find the existing open issue and append to it rather than
// file a duplicate. Kept as an HTML comment so it renders invisibly and
// survives human edits to the body. Shared by the renderer (which writes it)
// and any DedupIssueClient (which matches on it) so the two can't drift.
func EscalationDedupMarker(backlogID string) string {
	return fmt.Sprintf("<!-- mills-escalation:backlog=%s -->", backlogID)
}

// EscalationClassDedupMarker returns the class-aware marker for a classified
// escalation. Empty classes deliberately return the legacy backlog-only marker
// so older and unclassified producers retain their existing dedup contract.
func EscalationClassDedupMarker(backlogID, failureClass string) string {
	failureClass = strings.ToLower(strings.TrimSpace(failureClass))
	if failureClass == "" {
		return EscalationDedupMarker(backlogID)
	}
	return fmt.Sprintf("<!-- mills-escalation:backlog=%s;class=%s -->", backlogID, failureClass)
}

// EscalationClassDedupMarkerPrefix identifies every class-aware marker for one
// backlog item. It is used only for backlog-wide resolution after success.
func EscalationClassDedupMarkerPrefix(backlogID string) string {
	return fmt.Sprintf("<!-- mills-escalation:backlog=%s;class=", backlogID)
}

// HandoffClient creates an agent_handoff record. Production wraps
// mcp-agent-context's agent_handoff_create.
type HandoffClient interface {
	CreateHandoff(ctx context.Context, req HandoffRequest) (HandoffResponse, error)
}

// HandoffRequest is the bundle we send to agent_handoff_create.
type HandoffRequest struct {
	From        string         // "loom-mills-operator"
	To          string         // e.g. "human-on-call"
	Reason      string         // human-readable summary
	Context     map[string]any // structured failure record + run links
	BacklogID   string
	PipelineRun string
	IssueURL    string
}

// HandoffResponse reports the handoff id for audit.
type HandoffResponse struct {
	HandoffID string
}

// ContextRecorder writes one entry into the operator's long-lived
// agent-context session. Production wraps mcp-agent-context's
// agent_context_add (*clients.ContextRecorder); tests inject a fake.
//
// sessionID empty means "the recorder's own operator session" — the escalator
// has no session id of its own and does not need one, it just wants the entry
// to land where the handoff it is about to create is packaged from.
//
// Best-effort by contract: every caller logs and continues on error. Recording
// context must never fail an escalation.
type ContextRecorder interface {
	AddContextEntry(ctx context.Context, sessionID, entryType, title, content string, tags []string) error
}

// EscalationHandler is the contract Runner + Integrator call after they
// transition a run to PipelineEscalated. The handler should be best-effort
// (an issue-creation failure must not undo the escalated state).
type EscalationHandler interface {
	Handle(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem, reason string) error
}

// Escalator builds a failure record and posts the human-handoff
// artifacts. It is wired onto Runner and Integrator by the operator at
// startup; absence of an Escalator falls back to the bare state
// transition the runner already does.
type Escalator struct {
	Store   *store.Store
	Issue   IssueClient
	Handoff HandoffClient
	// Recorder, when set, writes the escalation decision into the operator's
	// agent-context session BEFORE the handoff is created, so the handoff
	// packages a session that actually holds the reasoning (pre-Recorder
	// escalation handoffs shipped with entry_count: 0). Nil disables recording;
	// the escalation itself is unaffected.
	Recorder ContextRecorder
	Project  string // GitLab project slug or numeric id; recorded in issue labels
	HandTo   string // e.g. "human-on-call"; default "human"
	Logger   *slog.Logger
	Clock    func() time.Time
	// LogTailMaxLines caps how many trailing lines we attach. Default 200.
	LogTailMaxLines int
}

const (
	ExternalIncidentDegradedMode           = "external_dependency_degraded"
	externalIncidentDegradedModeThreshold  = 3
	externalIncidentDegradedModeWindow     = 24 * time.Hour
	externalIncidentDegradedModeWindowText = "24h"
)

// NewEscalator constructs an Escalator with sensible defaults.
func NewEscalator(s *store.Store, issue IssueClient, handoff HandoffClient) *Escalator {
	return &Escalator{
		Store:           s,
		Issue:           issue,
		Handoff:         handoff,
		Logger:          slog.Default(),
		Clock:           time.Now,
		LogTailMaxLines: 200,
		HandTo:          "human",
	}
}

// Handle satisfies EscalationHandler.
func (e *Escalator) Handle(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem, reason string) error {
	if e == nil || e.Store == nil {
		return errors.New("escalator: not configured")
	}
	rec, err := e.BuildRecord(ctx, run, item, reason)
	if err != nil {
		return fmt.Errorf("build failure record: %w", err)
	}
	if e.Store.Pipeline != nil {
		md := rec.EscalationMetadata()
		if err := e.Store.Pipeline.SetEscalationMetadata(ctx, run.ID, md); err != nil {
			e.logger().Warn("escalator: persist escalation metadata failed", "run", run.ID, "error", err)
		}
	}
	e.applyExternalIncidentDegradedMode(ctx, run, rec)

	issueURL := ""
	if e.Issue != nil {
		issueURL = e.publishIssue(ctx, run, item, rec)
	}

	// Record the decision before the handoff so the packaged session is
	// non-empty. Best-effort: a recorder failure must never undo an escalation.
	e.recordEscalationDecision(ctx, run, item, rec, issueURL)

	if e.Handoff != nil {
		hto := e.HandTo
		if hto == "" {
			hto = "human"
		}
		hctx := map[string]any{"failure_record": rec, "issue_url": issueURL}
		if rec.Classification != nil {
			hctx["classification"] = *rec.Classification
		}
		req := HandoffRequest{
			From:        "loom-mills-operator",
			To:          hto,
			Reason:      reason,
			Context:     hctx,
			BacklogID:   item.ID,
			PipelineRun: run.ID,
			IssueURL:    issueURL,
		}
		if _, herr := e.Handoff.CreateHandoff(ctx, req); herr != nil {
			e.logger().Warn("escalator: create handoff failed", "error", herr, "run", run.ID)
		} else {
			mills.EscalationHandoffCreatedTotal.Inc()
		}
	}

	if e.Store.Events != nil {
		payload := map[string]any{
			"run":       run.ID,
			"backlog":   item.ID,
			"issue_url": issueURL,
			"reason":    reason,
			"outcome":   "ok",
		}
		if rec.EscalationMode != "" {
			payload["escalation_mode"] = rec.EscalationMode
			payload["incident_count"] = rec.IncidentCount
			payload["incident_window"] = rec.IncidentWindow
		}
		rec.addClassificationToPayload(payload)
		_ = e.Store.Events.Append(ctx, &store.Event{
			Actor:   "escalator",
			Kind:    "pipeline.escalation.published",
			Payload: payload,
		})
	}
	return nil
}

// recordEscalationDecision writes the escalation as a `decision` entry in the
// operator's agent-context session. It is the operator's half of the handoff
// contract: the handoff created immediately after references this session, so
// without the entry the receiving agent gets an empty package.
//
// No-op without a Recorder. Never returns an error — a recording failure is
// logged and swallowed, exactly like the issue/handoff steps around it.
func (e *Escalator) recordEscalationDecision(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem, rec *FailureRecord, issueURL string) {
	if e == nil || e.Recorder == nil || run == nil || item == nil || rec == nil {
		return
	}
	title := fmt.Sprintf("Escalated %s: %s", item.ID, truncate(rec.Reason, 100))

	var b strings.Builder
	fmt.Fprintf(&b, "Backlog item: %s — %s\n", item.ID, item.Title)
	fmt.Fprintf(&b, "Pipeline run: %s (state=%s, attempts=%d, cost=$%.2f)\n",
		run.ID, rec.State, rec.Attempts, rec.CostUSD)
	if stage := escalationStage(run, rec); stage != "" {
		fmt.Fprintf(&b, "Stage: %s\n", stage)
	}
	if rec.FailureClass != "" || rec.EscalationClass != "" {
		fmt.Fprintf(&b, "Classification: failure_class=%s escalation_class=%s",
			orNone(rec.FailureClass), orNone(rec.EscalationClass))
		if rec.Retryable != nil {
			fmt.Fprintf(&b, " retryable=%t", *rec.Retryable)
		}
		b.WriteString("\n")
	}
	if rec.ExternalDependency != "" {
		fmt.Fprintf(&b, "External dependency: %s\n", rec.ExternalDependency)
	}
	if rec.EscalationMode != "" {
		fmt.Fprintf(&b, "Escalation mode: %s (%d incidents in %s)\n",
			rec.EscalationMode, rec.IncidentCount, rec.IncidentWindow)
	}
	if failed := failedGateSummary(rec); failed != "" {
		fmt.Fprintf(&b, "Failing gates: %s\n", failed)
	}
	if run.MRIID != nil && *run.MRIID > 0 {
		fmt.Fprintf(&b, "Merge request: !%d\n", *run.MRIID)
	}
	if issueURL != "" {
		fmt.Fprintf(&b, "Escalation issue: %s\n", issueURL)
	}
	fmt.Fprintf(&b, "Reason: %s\n", rec.Reason)

	tags := []string{"mills", "escalation", "backlog:" + item.ID}
	if rec.FailureClass != "" {
		tags = append(tags, "failure_class:"+rec.FailureClass)
	}
	if err := e.Recorder.AddContextEntry(ctx, "", "decision", title, b.String(), tags); err != nil {
		e.logger().Warn("escalator: record context entry failed",
			"error", err, "run", run.ID, "backlog", item.ID)
	}
}

// escalationStage names the stage the run died on: the run's current stage when
// set, else the last stage recorded in the failure record.
func escalationStage(run *store.PipelineRun, rec *FailureRecord) string {
	if s := strings.TrimSpace(run.CurrentStage); s != "" {
		return s
	}
	if n := len(rec.StageStack); n > 0 {
		return rec.StageStack[n-1].Stage
	}
	return ""
}

// failedGateSummary renders the non-passing gate verdicts as a compact
// "gate=outcome" list. Empty when every gate passed (or none ran).
func failedGateSummary(rec *FailureRecord) string {
	var parts []string
	for _, g := range rec.GateVerdicts {
		if strings.EqualFold(g.Outcome, "pass") {
			continue
		}
		part := fmt.Sprintf("%s=%s", g.Gate, g.Outcome)
		if len(g.Reasons) > 0 {
			part += " (" + strings.Join(g.Reasons, "; ") + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// publishIssue files a new escalation issue for the run, OR — when the issue
// client supports dedup (DedupIssueClient) and an OPEN escalation issue already
// exists for this backlog item and failure class — appends a recurrence note to
// that issue instead of filing a duplicate. Returns the issue URL ("" if none
// could be published). Best-effort and FAIL-OPEN throughout: any dedup-path
// error falls back to creating a fresh issue so an escalation is never silently
// dropped.
func (e *Escalator) publishIssue(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem, rec *FailureRecord) string {
	if dc, ok := e.Issue.(DedupIssueClient); ok {
		var ref IssueRef
		var found bool
		var err error
		if classAware, ok := e.Issue.(ClassAwareDedupIssueClient); ok {
			ref, found, err = classAware.FindOpenEscalationByClass(ctx, item.ID, rec.FailureClass)
			// UPGRADE SEAM: an item whose earlier escalations persisted no
			// classification carries the legacy backlog-only marker. Now that
			// unmarked escalations classify (they used to stamp nothing —
			// classifyUnmarkedEscalation), a class-aware lookup for the SAME
			// item would miss that open legacy issue and file a duplicate. Fall
			// back to the backlog-only lookup so the recurrence lands on the
			// existing thread instead. Same item, same failure lineage — this
			// is what the legacy marker is for. Errors stay fail-open.
			if err == nil && !found && rec.FailureClass != "" {
				ref, found, err = dc.FindOpenEscalation(ctx, item.ID)
			}
		} else {
			ref, found, err = dc.FindOpenEscalation(ctx, item.ID)
		}
		switch {
		case err != nil:
			// Fail open — fall through to CreateIssue below.
			e.logger().Warn("escalator: dedup lookup failed; filing a new issue",
				"error", err, "run", run.ID, "backlog", item.ID)
		case found:
			if cerr := dc.CommentIssue(ctx, ref.IID, e.renderRecurrenceNote(run, rec)); cerr != nil {
				// The existing issue still stands and its URL is valid, so this
				// is a soft failure: the recurrence just isn't recorded on it.
				e.logger().Warn("escalator: comment existing issue failed",
					"error", cerr, "issue", ref.IID, "run", run.ID)
			}
			mills.EscalationIssueDedupedTotal.Inc()
			return ref.URL
		}
	}

	title := fmt.Sprintf("[mills] %s escalated: %s", run.ID, truncate(rec.Reason, 80))
	req := IssueRequest{
		BacklogID:   item.ID,
		Title:       title,
		Description: e.renderIssueBody(rec),
		Labels:      escalationIssueLabels(item, rec),
	}
	resp, ierr := e.Issue.CreateIssue(ctx, req)
	if ierr != nil {
		e.logger().Warn("escalator: create issue failed", "error", ierr, "run", run.ID)
		return ""
	}
	mills.EscalationIssueCreatedTotal.Inc()
	return resp.URL
}

// renderRecurrenceNote is the short markdown appended to an existing escalation
// issue when the same backlog item escalates again, so one open issue accretes
// its recurrences instead of the tracker filling with duplicates.
func (e *Escalator) renderRecurrenceNote(run *store.PipelineRun, rec *FailureRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Recurred — run `%s` (%s)\n\n", run.ID, e.now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "- **State**: `%s`\n", rec.State)
	fmt.Fprintf(&b, "- **Cost so far**: $%.2f\n", rec.CostUSD)
	writeClassificationMarkdown(&b, rec)
	writeDegradedModeMarkdown(&b, rec)
	fmt.Fprintf(&b, "- **Reason**: %s\n", rec.Reason)
	if rec.LastLogTail != "" {
		b.WriteString("\n<details><summary>Last log tail</summary>\n\n```\n")
		b.WriteString(rec.LastLogTail)
		b.WriteString("\n```\n</details>\n")
	}
	return b.String()
}

// writeClassificationMarkdown renders the classification bullet lines shared by
// the issue body and the recurrence note. Lines are omitted when the record
// carries no value for them so unclassified escalations render unchanged.
func writeClassificationMarkdown(b *strings.Builder, rec *FailureRecord) {
	if rec.FailureClass != "" {
		fmt.Fprintf(b, "- **Failure class**: `%s`\n", rec.FailureClass)
	}
	if rec.Retryable != nil {
		fmt.Fprintf(b, "- **Retryable**: `%t`\n", *rec.Retryable)
	}
	if c := rec.Classification; c != nil {
		if c.FreeRetry != nil {
			fmt.Fprintf(b, "- **Free retry**: `%t`\n", *c.FreeRetry)
		}
		if c.Terminal != nil {
			fmt.Fprintf(b, "- **Terminal**: `%t`\n", *c.Terminal)
		}
		if c.Classifier != "" {
			fmt.Fprintf(b, "- **Classifier**: `%s`\n", c.Classifier)
		}
	}
	if rec.ExternalDependency != "" {
		fmt.Fprintf(b, "- **External dependency**: `%s`\n", rec.ExternalDependency)
	}
}

func writeDegradedModeMarkdown(b *strings.Builder, rec *FailureRecord) {
	if rec == nil || rec.EscalationMode == "" {
		return
	}
	fmt.Fprintf(b, "- **Escalation mode**: `%s`\n", rec.EscalationMode)
	if rec.IncidentCount > 0 {
		fmt.Fprintf(b, "- **Matching incidents**: `%d`", rec.IncidentCount)
		if rec.IncidentWindow != "" {
			fmt.Fprintf(b, " in `%s`", rec.IncidentWindow)
		}
		b.WriteString("\n")
	}
}

// ResolveOnSuccess satisfies EscalationResolver: when a run for the item
// succeeds, close the open escalation issue that a prior failed run filed for
// it (DEBT-073 / #167 auto-close). No-op when the item never escalated, when
// the issue client can't look up / close issues, or when no open escalation
// exists. Best-effort — the runner treats any returned error as advisory and
// never fails the successful run over it.
func (e *Escalator) ResolveOnSuccess(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem) error {
	if e == nil || e.Issue == nil || item == nil {
		return nil
	}
	dc, ok := e.Issue.(DedupIssueClient)
	if !ok {
		return nil // can't find the open escalation
	}
	cc, ok := e.Issue.(ClosableIssueClient)
	if !ok {
		return nil // can't close
	}
	var refs []IssueRef
	if lister, ok := e.Issue.(OpenEscalationLister); ok {
		var err error
		refs, err = lister.ListOpenEscalations(ctx, item.ID)
		if err != nil {
			return fmt.Errorf("resolve on success: list open escalations: %w", err)
		}
	} else {
		ref, found, err := dc.FindOpenEscalation(ctx, item.ID)
		if err != nil {
			return fmt.Errorf("resolve on success: find open escalation: %w", err)
		}
		if found {
			refs = []IssueRef{ref}
		}
	}
	if len(refs) == 0 {
		return nil // item never escalated (or its issue is already closed)
	}
	// Record the resolution on the issue before closing so the thread explains
	// why it closed. A comment failure is soft — still close.
	note := fmt.Sprintf(
		"### Resolved — run `%s` succeeded for this item (%s)\n\nAuto-closing: a later pipeline run for the same backlog item merged, so this escalation is stale.\n",
		run.ID, e.now().UTC().Format(time.RFC3339))
	var closeErrors []error
	for _, ref := range refs {
		if cerr := dc.CommentIssue(ctx, ref.IID, note); cerr != nil {
			e.logger().Warn("escalator: resolution comment failed", "error", cerr, "issue", ref.IID, "run", run.ID)
		}
		if err := cc.CloseIssue(ctx, ref.IID); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close issue %d: %w", ref.IID, err))
			continue
		}
		mills.EscalationIssueAutoClosedTotal.Inc()
		e.logger().Info("escalator: auto-closed escalation on success", "issue", ref.IID, "run", run.ID, "backlog", item.ID)
	}
	return errors.Join(closeErrors...)
}

// BuildRecord assembles a FailureRecord from store rows. Exported for
// tests + the HUD's escalation drawer (slice 5.x).
func (e *Escalator) BuildRecord(ctx context.Context, run *store.PipelineRun, item *store.BacklogItem, reason string) (*FailureRecord, error) {
	stages, err := e.Store.Pipeline.ListStages(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	gates, err := e.Store.Pipeline.ListGates(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	rec := &FailureRecord{
		BacklogID:     item.ID,
		PipelineRunID: run.ID,
		Reason:        reason,
		State:         run.State,
		CostUSD:       run.CostUSD,
		Attempts:      run.Attempts,
		GeneratedAt:   e.now(),
	}
	for _, sr := range stages {
		fs := FailureStage{
			Stage:   sr.Stage,
			Attempt: sr.Attempt,
			CostUSD: sr.CostUSD,
		}
		if sr.Outcome != nil {
			fs.Outcome = string(*sr.Outcome)
		}
		if sr.EndedAt != nil && !sr.EndedAt.IsZero() {
			fs.Duration = sr.EndedAt.Sub(sr.StartedAt).Round(time.Millisecond).String()
		}
		rec.StageStack = append(rec.StageStack, fs)
	}
	for _, g := range gates {
		rec.GateVerdicts = append(rec.GateVerdicts, FailureGate{
			Gate:       g.GateName,
			AfterStage: g.AfterStage,
			Outcome:    string(g.Outcome),
			Reasons:    g.Reasons,
		})
	}
	rec.LastLogTail = e.lastLogTail(stages)
	md := escalationMetadataFromEvidence(ErrorClass(run.EscalationClass), reason, rec.LastLogTail)
	md.RetryExhausted = run.RetryExhausted
	if run.ExternalDependencyID != "" {
		md = routeExternalDependencyEscalation(md, run.ExternalDependencyID, run.ExternalDependency)
		if run.FailureClass != "" {
			md.FailureClass = run.FailureClass
			md.Retryable = run.EscalationRetryable
		}
	}
	rec.SetClassification(md)
	return rec, nil
}

func routeExternalDependencyEscalation(md store.EscalationMetadata, id, dependency string) store.EscalationMetadata {
	if strings.TrimSpace(id) == "" {
		return md
	}
	md.EscalationClass = telemetry.EscalationClassExternalDependency
	md.ExternalDependencyID = id
	md.ExternalDependency = dependency
	return md
}

// SetClassification attaches structured classification metadata while keeping
// the legacy top-level fields populated for existing consumers.
func (r *FailureRecord) SetClassification(md store.EscalationMetadata) {
	if r == nil {
		return
	}
	r.EscalationClass = md.EscalationClass
	r.FailureClass = md.FailureClass
	r.ExternalDependencyID = md.ExternalDependencyID
	r.ExternalDependency = md.ExternalDependency
	r.Retryable = md.Retryable
	r.RetryExhausted = md.RetryExhausted
	if md.EscalationClass == "" && md.FailureClass == "" &&
		md.ExternalDependencyID == "" && md.ExternalDependency == "" &&
		md.Retryable == nil && md.RetryExhausted == nil {
		r.Classification = nil
		return
	}
	r.Classification = &EscalationClassification{
		EscalationClass:      md.EscalationClass,
		FailureClass:         md.FailureClass,
		ExternalDependencyID: md.ExternalDependencyID,
		ExternalDependency:   md.ExternalDependency,
		Retryable:            md.Retryable,
		RetryExhausted:       md.RetryExhausted,
	}
	if md.FailureClass != "" {
		fc := failureClassificationForClass(FailureClassFromString(md.FailureClass))
		r.Classification.Classifier = fc.Classifier
		r.Classification.FreeRetry = &fc.FreeRetry
		r.Classification.Terminal = &fc.Terminal
	}
}

func (r *FailureRecord) SetDegradedMode(mode string, count int, window string) {
	if r == nil || mode == "" {
		return
	}
	r.EscalationMode = mode
	r.IncidentCount = count
	r.IncidentWindow = window
	if r.Classification == nil {
		r.Classification = &EscalationClassification{}
	}
	r.Classification.EscalationMode = mode
	r.Classification.IncidentCount = count
	r.Classification.IncidentWindow = window
}

// EscalationMetadata returns the store representation of the record's
// classification. It prefers the grouped payload when present and falls back to
// the legacy top-level fields for records built before the grouped shape.
func (r *FailureRecord) EscalationMetadata() store.EscalationMetadata {
	if r == nil {
		return store.EscalationMetadata{}
	}
	if r.Classification != nil {
		return store.EscalationMetadata{
			EscalationClass:      r.Classification.EscalationClass,
			FailureClass:         r.Classification.FailureClass,
			ExternalDependencyID: r.Classification.ExternalDependencyID,
			ExternalDependency:   r.Classification.ExternalDependency,
			Retryable:            r.Classification.Retryable,
			RetryExhausted:       r.Classification.RetryExhausted,
		}
	}
	return store.EscalationMetadata{
		EscalationClass:      r.EscalationClass,
		FailureClass:         r.FailureClass,
		ExternalDependencyID: r.ExternalDependencyID,
		ExternalDependency:   r.ExternalDependency,
		Retryable:            r.Retryable,
		RetryExhausted:       r.RetryExhausted,
	}
}

func (r *FailureRecord) addClassificationToPayload(payload map[string]any) {
	if r == nil || payload == nil {
		return
	}
	md := r.EscalationMetadata()
	if md.EscalationClass == "" && md.FailureClass == "" &&
		md.ExternalDependencyID == "" && md.ExternalDependency == "" &&
		md.Retryable == nil {
		return
	}
	// Prefer the grouped block already on the record: it carries the enriched
	// classifier/free_retry/terminal fields the store metadata does not.
	cls := r.Classification
	if cls == nil {
		cls = &EscalationClassification{
			EscalationClass:      md.EscalationClass,
			FailureClass:         md.FailureClass,
			ExternalDependencyID: md.ExternalDependencyID,
			ExternalDependency:   md.ExternalDependency,
			Retryable:            md.Retryable,
			RetryExhausted:       md.RetryExhausted,
		}
		if md.FailureClass != "" {
			fc := failureClassificationForClass(FailureClassFromString(md.FailureClass))
			cls.Classifier = fc.Classifier
			cls.FreeRetry = &fc.FreeRetry
			cls.Terminal = &fc.Terminal
		}
	}
	payload["classification"] = *cls
	if md.EscalationClass != "" {
		payload["escalation_class"] = md.EscalationClass
	}
	if md.FailureClass != "" {
		payload["failure_class"] = md.FailureClass
	}
	if md.Retryable != nil {
		payload["retryable"] = *md.Retryable
	}
	if md.RetryExhausted != nil {
		payload["retry_exhausted"] = *md.RetryExhausted
	}
	if cls.EscalationMode != "" {
		payload["escalation_mode"] = cls.EscalationMode
	}
	if cls.IncidentCount > 0 {
		payload["incident_count"] = cls.IncidentCount
	}
	if cls.IncidentWindow != "" {
		payload["incident_window"] = cls.IncidentWindow
	}
	if cls.FreeRetry != nil {
		payload["free_retry"] = *cls.FreeRetry
	}
	if cls.Terminal != nil {
		payload["terminal"] = *cls.Terminal
	}
	if cls.Classifier != "" {
		payload["classifier"] = cls.Classifier
	}
	if md.ExternalDependencyID != "" {
		payload["external_dependency_id"] = md.ExternalDependencyID
	}
	if md.ExternalDependency != "" {
		payload["external_dependency"] = md.ExternalDependency
	}
}

// lastLogTail returns the trailing N lines from the most recent stage
// result that has a LogTail. The runner truncates upstream so this is
// usually a no-op cap.
func (e *Escalator) lastLogTail(stages []*store.StageResult) string {
	if len(stages) == 0 {
		return ""
	}
	for i := len(stages) - 1; i >= 0; i-- {
		if stages[i].LogTail == "" {
			continue
		}
		return capLines(stages[i].LogTail, e.LogTailMaxLines)
	}
	return ""
}

func capLines(s string, max int) string {
	if max <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= max {
		return s
	}
	return strings.Join(lines[len(lines)-max:], "\n")
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// renderIssueBody is a tiny markdown renderer for the GitLab issue body.
func (e *Escalator) renderIssueBody(rec *FailureRecord) string {
	var b strings.Builder
	b.WriteString("## Pipeline Escalation\n\n")
	fmt.Fprintf(&b, "- **Backlog item**: `%s`\n", rec.BacklogID)
	fmt.Fprintf(&b, "- **Pipeline run**: `%s`\n", rec.PipelineRunID)
	fmt.Fprintf(&b, "- **State**: `%s`\n", rec.State)
	fmt.Fprintf(&b, "- **Cost so far**: $%.2f\n", rec.CostUSD)
	writeClassificationMarkdown(&b, rec)
	writeDegradedModeMarkdown(&b, rec)
	fmt.Fprintf(&b, "- **Reason**: %s\n\n", rec.Reason)

	if rec.EscalationMode == ExternalIncidentDegradedMode {
		b.WriteString("### Degraded external dependency mode\n\n")
		fmt.Fprintf(&b, "This dependency has produced `%d` matching escalations in `%s`; keep handling in degraded mode until the dependency recovers and a requeue succeeds.\n\n", rec.IncidentCount, rec.IncidentWindow)
		b.WriteString("- Reuse this incident thread for additional matching failures instead of opening speculative remediation work.\n")
		b.WriteString("- Pause local retry churn for this dependency unless new evidence identifies a repository-owned change.\n")
		b.WriteString("- Link upstream provider or dependency-owner status updates back to this escalation.\n\n")
	}

	if rendered, ok := council.FormatExternalEscalation(council.ExternalEscalationRenderInput{
		Reason:      rec.Reason,
		LastLogTail: rec.LastLogTail,
	}); ok {
		b.WriteString(rendered.Markdown)
		b.WriteString("\n\n")
	}

	b.WriteString("### Stage history\n\n")
	b.WriteString("| Stage | Attempt | Outcome | Cost | Duration |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, s := range rec.StageStack {
		fmt.Fprintf(&b, "| `%s` | %d | %s | $%.2f | %s |\n",
			s.Stage, s.Attempt, s.Outcome, s.CostUSD, s.Duration)
	}

	if len(rec.GateVerdicts) > 0 {
		b.WriteString("\n### Gate verdicts\n\n")
		for _, g := range rec.GateVerdicts {
			fmt.Fprintf(&b, "- `%s` after `%s` → **%s**", g.Gate, g.AfterStage, g.Outcome)
			if len(g.Reasons) > 0 {
				fmt.Fprintf(&b, ": %s", strings.Join(g.Reasons, "; "))
			}
			b.WriteString("\n")
		}
	}

	if rec.LastLogTail != "" {
		b.WriteString("\n### Last log tail\n\n```\n")
		b.WriteString(rec.LastLogTail)
		b.WriteString("\n```\n")
	}
	// Stable, invisible dedup marker so a later run for the same backlog item
	// and failure class finds THIS issue and appends a recurrence note instead
	// of filing a duplicate. Unclassified records keep the legacy backlog-only
	// marker (see EscalationClassDedupMarker / publishIssue).
	fmt.Fprintf(&b, "\n%s\n", EscalationClassDedupMarker(rec.BacklogID, rec.FailureClass))
	return b.String()
}

func (e *Escalator) applyExternalIncidentDegradedMode(ctx context.Context, run *store.PipelineRun, rec *FailureRecord) {
	if e == nil || e.Store == nil || e.Store.Pipeline == nil || run == nil || rec == nil {
		return
	}
	if rec.ExternalDependencyID == "" && rec.ExternalDependency == "" {
		return
	}
	since := e.now().UTC().Add(-externalIncidentDegradedModeWindow)
	count, err := e.Store.Pipeline.CountRecentExternalDependencyIncidents(ctx, store.ExternalIncidentQuery{
		ExternalDependencyID: rec.ExternalDependencyID,
		ExternalDependency:   rec.ExternalDependency,
		Since:                since,
	})
	if err != nil {
		e.logger().Warn("escalator: external dependency incident recurrence lookup failed",
			"run", run.ID, "external_dependency_id", rec.ExternalDependencyID,
			"external_dependency", rec.ExternalDependency, "error", err)
		return
	}
	if run.State != store.PipelineEscalated {
		count++
	}
	if count < externalIncidentDegradedModeThreshold {
		return
	}
	rec.SetDegradedMode(ExternalIncidentDegradedMode, count, externalIncidentDegradedModeWindowText)
}

func escalationIssueLabels(item *store.BacklogItem, rec *FailureRecord) []string {
	priority := ""
	if item != nil {
		priority = string(item.Priority)
	}
	labels := []string{"mills-escalation", "kind/incident", "priority/" + priority}
	if rec != nil && rec.EscalationMode == ExternalIncidentDegradedMode {
		labels = append(labels, "mode/degraded", "incident/external-dependency")
		if rec.ExternalDependency != "" {
			labels = append(labels, "dependency/"+rec.ExternalDependency)
		}
	}
	return labels
}

func (e *Escalator) now() time.Time {
	if e.Clock != nil {
		return e.Clock()
	}
	return time.Now()
}

func (e *Escalator) logger() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}
