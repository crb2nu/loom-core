package mrwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crb2nu/loom/pkg/mills/clients"
)

// The shepherd (slice M4) is the bounded-autonomy reconciler for the mrwatch
// registry. After every poll it inspects each classified merge request and
// takes at most one small, reversible auto-action per unhealthy MR, gated by a
// per-MR daily budget and a global kill switch. It NEVER rebases, force-pushes,
// merges directly, or touches a `conflict` / `ci_failed_deterministic` MR —
// those surface to humans/agents via the inbox + attention lane (M5).
//
// Action matrix (one action per MR per reconcile):
//
//	ci_failed_flaky      → retry the failed head pipeline once
//	pipeline_skipped     → create a fresh branch pipeline (re-point head), age>10m
//	awaiting_pipeline    → create a fresh branch pipeline (no head), age>10m
//	automerge_unarmed    → arm auto-merge (auto_merge=true&sha=<observed head>),
//	                       age>30m; refused outright when no head sha was observed
//	everything else      → no action
//
// The arm is always bound to the head SHA the poll observed. GitLab rejects the
// merge call with 409 when that SHA is no longer the head of the source branch,
// so a push landing between poll and action can never be auto-merged unreviewed:
// the shepherd records "head moved" and lets the next poll re-classify the new
// head before considering it again.
//
// The classifier already encodes the CI/draft/mwps preconditions into the
// state (e.g. automerge_unarmed ⇒ green-or-running CI, not draft, mwps false),
// so the shepherd only adds the age gate and the budget/kill-switch guards.
//
// Budget is tracked in memory and keyed by repo!iid + UTC day; it RESETS ON
// RESTART. That is deliberate for v1: a restart is rare, the budget's job is to
// stop a runaway loop within a single process lifetime, and persisting it would
// add a store dependency the shepherd otherwise does not need. A restart at
// worst grants one fresh budget window.

// Env var names for the shepherd. The kill switch defaults OFF: the shepherd
// only takes actions when LOOM_MRWATCH_SHEPHERD is explicitly set truthy, so a
// rollout enables autonomy as a deliberate env change, never by accident.
const (
	// EnvShepherd is the global kill switch. Truthy ("on"/"1"/"true"/"enabled"/
	// "yes") enables the shepherd; anything else (including unset and the
	// explicit "off"/"0"/"false") disables it. DEFAULT: OFF.
	EnvShepherd = "LOOM_MRWATCH_SHEPHERD"
	// EnvShepherdBudget overrides the per-MR daily action budget. Default 2.
	// A negative value clamps to 0 (audit-only, takes no actions).
	EnvShepherdBudget     = "LOOM_MRWATCH_SHEPHERD_BUDGET"
	EnvMillsOperatorURL   = "LOOM_MILLS_OPERATOR_URL"
	EnvMillsOperatorToken = "LOOM_MILLS_ADMIN_TOKEN"
)

// DefaultShepherdBudget is the per-MR daily action budget when unset.
const DefaultShepherdBudget = 2

// Action-age gates. A very fresh MR is left alone so CI has a chance to attach
// and settle before the shepherd intervenes.
const (
	// pipelineMinAge is how old an MR must be before the shepherd creates a
	// fresh pipeline for a skipped / missing head pipeline.
	pipelineMinAge = 10 * time.Minute
	// automergeMinAge is how old an MR must be before the shepherd arms
	// auto-merge (memory feedback_automerge_drops_on_push: MWPS re-arms after
	// pushes, so only arm once the MR has clearly settled).
	automergeMinAge = 30 * time.Minute
)

// defaultActionTimeout bounds each individual GitLab write so one slow call
// can't stall the poll goroutine the reconcile runs on.
const defaultActionTimeout = 20 * time.Second

// defaultAuditRing is the number of most-recent action records retained for
// GET /api/mrwatch/actions.
const defaultAuditRing = 200

// Action identifies which bounded write the shepherd selected.
type Action string

const (
	// ActionRetryPipeline retries a flaky-classified failed head pipeline once.
	ActionRetryPipeline Action = "retry_pipeline"
	// ActionCreatePipeline creates a fresh branch pipeline to re-point a
	// skipped / missing head pipeline.
	ActionCreatePipeline Action = "create_pipeline"
	// ActionArmAutoMerge arms auto-merge on a settled, unarmed MR.
	ActionArmAutoMerge Action = "arm_auto_merge"
)

// Outcome is the result of an attempted action.
type Outcome string

const (
	// OutcomeOK — the GitLab write succeeded.
	OutcomeOK Outcome = "ok"
	// OutcomeError — the write failed with a hard error (consumes budget so a
	// persistently-failing endpoint is not hammered).
	OutcomeError Outcome = "error"
	// OutcomeDeferred — an arm returned 405 while GitLab was still computing
	// mergeability. Not a failure: the shepherd retries on the next poll and
	// does NOT consume budget for it.
	OutcomeDeferred Outcome = "deferred_405"
	// OutcomeSkipped — the shepherd REFUSED to act because a safety
	// precondition was not met: today the only case is an auto-merge arm on an
	// MR whose head SHA the poll did not observe, which could not be pinned to
	// the reviewed head. No GitLab call is made and no budget is consumed; the
	// refusal is audited once per MR per day so it is visible without flooding.
	OutcomeSkipped Outcome = "skipped_no_head_sha"
	// OutcomeHeadMoved — GitLab rejected the arm with 409 because the observed
	// head SHA is no longer the head of the source branch (someone pushed
	// between the poll and the action). The new, unreviewed head is deliberately
	// NOT armed: the next poll re-observes and re-classifies it, and only then
	// may the shepherd arm that head. Consumes budget so a fast-moving branch
	// cannot make the shepherd loop on rejected arms.
	OutcomeHeadMoved Outcome = "head_moved"
)

// Actor is the bounded write surface the shepherd needs. The GitLab-backed
// implementation scopes each call to its project; tests supply a fake so no
// network or token is required.
type Actor interface {
	// RetryPipeline retries the pipeline by id in repo.
	RetryPipeline(ctx context.Context, repo string, pipelineID int64) error
	// CreatePipeline creates a fresh branch pipeline for ref in repo.
	CreatePipeline(ctx context.Context, repo, ref string) (int64, error)
	// ArmAutoMerge arms auto-merge on the MR in repo, bound to headSHA: the
	// implementation must send it as GitLab's merge `sha` precondition so the
	// arm can only apply to the head the caller observed. A 405 error means
	// "still checking, retry later"; a 409 means the head moved since headSHA
	// was observed — both surfaced via clients.GitLabHTTPStatus. Callers must
	// never pass an empty headSHA (the shepherd refuses before calling).
	ArmAutoMerge(ctx context.Context, repo string, mrIID int64, sourceBranch, targetBranch, headSHA string) error
}

// ActionRecord is one audit-log entry, emitted for every attempted action.
// All string/time fields are wire-safe; the slice returned by Actions() is
// always non-nil so the endpoint encodes [] never null.
type ActionRecord struct {
	Time    time.Time `json:"time"`
	Repo    string    `json:"repo"`
	MRIID   int64     `json:"mr_iid"`
	Branch  string    `json:"branch,omitempty"`
	State   string    `json:"state"`
	Action  string    `json:"action"`
	Outcome string    `json:"outcome"`
	Detail  string    `json:"detail,omitempty"`
}

// Shepherd reconciles unhealthy MRs into bounded auto-actions. Safe for
// concurrent use: Reconcile runs on the poll goroutine while Actions() serves
// HTTP readers.
type Shepherd struct {
	actor         Actor
	enabled       bool
	budget        int
	ringSize      int
	actionTimeout time.Duration
	now           func() time.Time
	logger        *slog.Logger

	mu      sync.Mutex
	spent   map[string]daySpend // per-MR budget consumed today
	refused map[string]string   // per-MR UTC day a SHA-less arm refusal was audited
	ring    []ActionRecord      // bounded audit log, chronological
}

// daySpend records how many actions an MR has consumed and on which UTC day, so
// the budget resets at the day boundary without a background sweep.
type daySpend struct {
	day   string
	count int
}

// ShepherdOptions configures a Shepherd. Zero values fall back to defaults.
type ShepherdOptions struct {
	Enabled       bool
	Budget        int
	RingSize      int
	ActionTimeout time.Duration
	Now           func() time.Time
	Logger        *slog.Logger
}

// NewShepherd builds a Shepherd over the given Actor. A nil actor forces the
// shepherd disabled (it can still be constructed for the audit endpoint).
func NewShepherd(actor Actor, opts ShepherdOptions) *Shepherd {
	budget := opts.Budget
	if budget < 0 {
		budget = 0
	}
	ring := opts.RingSize
	if ring <= 0 {
		ring = defaultAuditRing
	}
	timeout := opts.ActionTimeout
	if timeout <= 0 {
		timeout = defaultActionTimeout
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Shepherd{
		actor:         actor,
		enabled:       opts.Enabled && actor != nil,
		budget:        budget,
		ringSize:      ring,
		actionTimeout: timeout,
		now:           nowFn,
		logger:        logger,
		spent:         make(map[string]daySpend),
		refused:       make(map[string]string),
		ring:          make([]ActionRecord, 0, ring),
	}
}

// NewShepherdFromEnv builds a Shepherd from the environment (kill switch +
// budget), backed by the GitLab client's write actions. Returns nil when actor
// is nil so callers can guard on GitLab config.
func NewShepherdFromEnv(base *clients.GitLabClient, logger *slog.Logger) *Shepherd {
	if base == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	enabled := ShepherdEnabledFromEnv()
	budget := ShepherdBudgetFromEnv(logger)
	logger.Info("mrwatch: shepherd configured",
		"enabled", enabled, "budget", budget)
	return NewShepherd(NewGitLabActor(base), ShepherdOptions{
		Enabled: enabled,
		Budget:  budget,
		Logger:  logger,
	})
}

// ShepherdEnabledFromEnv reports whether LOOM_MRWATCH_SHEPHERD is set to a
// truthy value. DEFAULT OFF: unset or any non-truthy value disables the
// shepherd.
func ShepherdEnabledFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvShepherd))) {
	case "on", "1", "true", "enabled", "enable", "yes":
		return true
	default:
		return false
	}
}

// ShepherdBudgetFromEnv parses LOOM_MRWATCH_SHEPHERD_BUDGET, defaulting to 2 on
// unset/unparseable input and clamping negatives to 0.
func ShepherdBudgetFromEnv(logger *slog.Logger) int {
	raw := strings.TrimSpace(os.Getenv(EnvShepherdBudget))
	if raw == "" {
		return DefaultShepherdBudget
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		if logger != nil {
			logger.Warn("mrwatch: invalid LOOM_MRWATCH_SHEPHERD_BUDGET; using default",
				"value", raw, "default", DefaultShepherdBudget)
		}
		return DefaultShepherdBudget
	}
	if n < 0 {
		return 0
	}
	return n
}

// Enabled reports whether the shepherd will take actions.
func (s *Shepherd) Enabled() bool {
	if s == nil {
		return false
	}
	return s.enabled
}

// Budget returns the configured per-MR daily action budget.
func (s *Shepherd) Budget() int {
	if s == nil {
		return 0
	}
	return s.budget
}

// actionPlan is the resolved action for one MR (nil-free value type). sha is the
// head commit the poll observed for the MR; the arm action binds to it so the
// write cannot land on a head the shepherd never classified.
type actionPlan struct {
	kind         Action
	repo         string
	branch       string
	targetBranch string
	iid          int64
	pipelineID   int64
	sha          string
}

// Reconcile inspects every MR in the snapshot and applies at most one bounded
// action per unhealthy MR, subject to the kill switch and per-MR daily budget.
// It is safe to call on a nil or disabled shepherd (no-op). Runs on the poll
// goroutine via Poller.SetPostPoll.
func (s *Shepherd) Reconcile(ctx context.Context, snap Snapshot) {
	if s == nil || !s.enabled || s.actor == nil {
		return
	}
	now := s.now()
	for _, mr := range snap.MergeRequests {
		plan, ok := s.plan(mr, now)
		if !ok {
			continue
		}
		key := budgetKey(mr.Repo, mr.IID)

		// Fail closed: an arm we cannot pin to an observed head SHA is refused
		// outright. Without the sha precondition GitLab would arm whatever the
		// branch points at now, which may be a push that landed after this poll
		// classified the MR. Audited (once per MR per day so a persistently
		// sha-less MR can't flood the ring) and does not consume budget, so a
		// later poll that does observe a sha can still arm.
		if plan.kind == ActionArmAutoMerge && plan.sha == "" {
			s.refuseArmWithoutSHA(mr, now, key)
			continue
		}

		if s.spentToday(key, now) >= s.budget {
			s.logger.Debug("mrwatch shepherd: budget exhausted; skipping",
				"repo", mr.Repo, "mr_iid", mr.IID, "state", mr.State, "budget", s.budget)
			continue
		}

		outcome, detail := s.act(ctx, plan)

		// A benign arm-405 does not consume budget: the shepherd retries it on
		// the next poll once GitLab finishes computing mergeability. A refusal
		// issued no request at all, so it does not consume budget either. Every
		// other outcome — including a 409 head-moved rejection — does, which is
		// what stops a branch being pushed every poll from looping the shepherd
		// on rejected arms.
		if outcome != OutcomeDeferred && outcome != OutcomeSkipped {
			s.consume(key, now)
		}
		s.record(ActionRecord{
			Time:    now,
			Repo:    mr.Repo,
			MRIID:   mr.IID,
			Branch:  mr.SourceBranch,
			State:   string(mr.State),
			Action:  string(plan.kind),
			Outcome: string(outcome),
			Detail:  detail,
		})
		s.logger.Info("mrwatch shepherd: action",
			"repo", mr.Repo, "mr_iid", mr.IID, "state", mr.State,
			"action", plan.kind, "outcome", outcome)
	}
}

// plan selects the single bounded action for an MR, or reports ok=false when
// none applies. conflict, ci_failed_deterministic, ci_running, stale_branch,
// draft_idle and ok all fall through to the default (no action): the shepherd
// never touches a conflicted or deterministically-broken MR.
func (s *Shepherd) plan(mr MergeRequest, now time.Time) (actionPlan, bool) {
	targetBranch := strings.TrimSpace(mr.TargetBranch)
	base := actionPlan{
		repo:         mr.Repo,
		branch:       strings.TrimSpace(mr.SourceBranch),
		targetBranch: targetBranch,
		iid:          mr.IID,
		pipelineID:   mr.PipelineID,
		sha:          strings.TrimSpace(mr.SHA),
	}
	switch mr.State {
	case StateCIFailedFlaky:
		if mr.PipelineID <= 0 {
			return actionPlan{}, false // nothing to retry
		}
		base.kind = ActionRetryPipeline
		return base, true
	case StatePipelineSkipped:
		if mr.SourceBranch == "" || mrAge(mr, now) < pipelineMinAge {
			return actionPlan{}, false
		}
		base.kind = ActionCreatePipeline
		return base, true
	case StateAwaitingPipeline:
		// Only when there is genuinely no head pipeline (id 0). A non-zero id
		// here means an unknown-status pipeline exists; leave it to poll again
		// rather than spawn a duplicate.
		if mr.PipelineID != 0 || mr.SourceBranch == "" || mrAge(mr, now) < pipelineMinAge {
			return actionPlan{}, false
		}
		base.kind = ActionCreatePipeline
		return base, true
	case StateAutomergeUnarmed:
		if mrAge(mr, now) < automergeMinAge {
			return actionPlan{}, false
		}
		// A missing sha does NOT drop the plan here: Reconcile turns it into an
		// audited refusal so an MR the shepherd cannot safely arm is visible
		// rather than silently ignored.
		base.kind = ActionArmAutoMerge
		return base, true
	default:
		return actionPlan{}, false
	}
}

// act performs the planned write with a bounded timeout and maps the result to
// an outcome + human-readable detail.
func (s *Shepherd) act(ctx context.Context, plan actionPlan) (Outcome, string) {
	actx, cancel := context.WithTimeout(ctx, s.actionTimeout)
	defer cancel()

	switch plan.kind {
	case ActionRetryPipeline:
		if err := s.actor.RetryPipeline(actx, plan.repo, plan.pipelineID); err != nil {
			return OutcomeError, errDetail(err)
		}
		return OutcomeOK, "retried pipeline " + strconv.FormatInt(plan.pipelineID, 10)
	case ActionCreatePipeline:
		id, err := s.actor.CreatePipeline(actx, plan.repo, plan.branch)
		if err != nil {
			return OutcomeError, errDetail(err)
		}
		return OutcomeOK, "created pipeline " + strconv.FormatInt(id, 10) + " on " + plan.branch
	case ActionArmAutoMerge:
		// Defence in depth: Reconcile already refuses a sha-less arm before it
		// reaches the budget, so this only fires if a future caller bypasses it.
		if plan.sha == "" {
			return OutcomeSkipped, detailNoHeadSHA
		}
		if plan.branch == "" || plan.targetBranch == "" {
			return OutcomeSkipped, "source and target branches are required"
		}
		if err := s.actor.ArmAutoMerge(actx, plan.repo, plan.iid, plan.branch, plan.targetBranch, plan.sha); err != nil {
			if code, ok := clients.GitLabHTTPStatus(err); ok {
				switch code {
				case 405:
					// GitLab still computing mergeability — defer, don't fail.
					return OutcomeDeferred, "405 checking mergeability; retry next poll"
				case 409:
					// The sha precondition rejected the arm: the branch moved
					// after this poll observed it. Do NOT re-arm the new head
					// here — the next poll observes and re-classifies it first.
					return OutcomeHeadMoved, "409 head moved past observed sha " + shortSHA(plan.sha) + "; re-planning on next poll"
				}
			}
			return OutcomeError, errDetail(err)
		}
		return OutcomeOK, "armed auto_merge at sha " + shortSHA(plan.sha)
	default:
		return OutcomeError, "unknown action"
	}
}

// detailNoHeadSHA is the audit detail for a refused arm. It names the reason
// explicitly so the /api/mrwatch/actions consumer can tell a refusal apart from
// a GitLab-side rejection.
const detailNoHeadSHA = "no observed head sha; refusing to arm auto_merge on an unpinned head"

// refuseArmWithoutSHA audits a refused (sha-less) arm at most once per MR per
// UTC day and logs it at warn level. It takes no GitLab action and consumes no
// budget: the MR is simply left for a human/agent, and any later poll that does
// observe a head sha may still arm it.
func (s *Shepherd) refuseArmWithoutSHA(mr MergeRequest, now time.Time, key string) {
	s.logger.Warn("mrwatch shepherd: refusing auto-merge arm without an observed head sha",
		"repo", mr.Repo, "mr_iid", mr.IID, "branch", mr.SourceBranch, "state", mr.State)
	if !s.markRefused(key, now) {
		return // already audited today; don't flood the ring on every poll
	}
	s.record(ActionRecord{
		Time:    now,
		Repo:    mr.Repo,
		MRIID:   mr.IID,
		Branch:  mr.SourceBranch,
		State:   string(mr.State),
		Action:  string(ActionArmAutoMerge),
		Outcome: string(OutcomeSkipped),
		Detail:  detailNoHeadSHA,
	})
}

// markRefused records that the MR's sha-less refusal was audited today,
// reporting false when one was already audited (same UTC day).
func (s *Shepherd) markRefused(key string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := dayOf(now)
	if s.refused[key] == day {
		return false
	}
	s.refused[key] = day
	return true
}

// spentToday returns how many actions the MR has consumed today (0 across a day
// boundary). Caller-serialized via s.mu.
func (s *Shepherd) spentToday(key string, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.spent[key]
	if !ok || v.day != dayOf(now) {
		return 0
	}
	return v.count
}

// consume increments the MR's action count for today, resetting across the day
// boundary.
func (s *Shepherd) consume(key string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := dayOf(now)
	v := s.spent[key]
	if v.day != day {
		v = daySpend{day: day, count: 0}
	}
	v.count++
	s.spent[key] = v
}

// record appends an audit entry, trimming to the ring size (newest retained).
func (s *Shepherd) record(rec ActionRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ring = append(s.ring, rec)
	if len(s.ring) > s.ringSize {
		s.ring = append(s.ring[:0:0], s.ring[len(s.ring)-s.ringSize:]...)
	}
}

// Actions returns a chronological copy of the audit log (oldest→newest). Always
// non-nil so the endpoint encodes [] never null. A nil shepherd yields [].
func (s *Shepherd) Actions() []ActionRecord {
	if s == nil {
		return []ActionRecord{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ActionRecord, len(s.ring))
	copy(out, s.ring)
	return out
}

// mrAge is the effective age of an MR used for the action gates. It prefers the
// MR's created_at; when the source did not populate it, it falls back to the
// time the MR entered its current state (last_transition_at). If neither is
// known the age is zero, so an age-gated action is conservatively skipped.
func mrAge(mr MergeRequest, now time.Time) time.Duration {
	if !mr.CreatedAt.IsZero() {
		return now.Sub(mr.CreatedAt)
	}
	if !mr.LastTransitionAt.IsZero() {
		return now.Sub(mr.LastTransitionAt)
	}
	return 0
}

// budgetKey composes the per-MR budget key (repo!iid).
func budgetKey(repo string, iid int64) string {
	return repo + "!" + strconv.FormatInt(iid, 10)
}

// dayOf returns the UTC calendar day used as the budget reset boundary.
func dayOf(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// shortSHA abbreviates a commit SHA for audit details (GitLab's own short form).
// Anything already shorter is returned as-is.
func shortSHA(sha string) string {
	const short = 8
	if len(sha) <= short {
		return sha
	}
	return sha[:short]
}

// errDetail renders an error for the audit log, bounding its length so a
// verbose GitLab body can't bloat the ring buffer.
func errDetail(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	const max = 300
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// ----- GitLab-backed Actor -----

// gitlabActor implements Actor over the mills GitLab client, scoping each call
// to its target project via ForProject (the shared group token must be
// authorized on every watched repo).
type gitlabActor struct {
	base *clients.GitLabClient
}

// NewGitLabActor wraps a GitLab client as an Actor.
func NewGitLabActor(base *clients.GitLabClient) Actor {
	return gitlabActor{base: base}
}

func (a gitlabActor) RetryPipeline(ctx context.Context, repo string, pipelineID int64) error {
	return a.base.ForProject(repo).RetryPipeline(ctx, pipelineID)
}

func (a gitlabActor) CreatePipeline(ctx context.Context, repo, ref string) (int64, error) {
	return a.base.ForProject(repo).CreatePipelineForRef(ctx, ref)
}

func (a gitlabActor) ArmAutoMerge(ctx context.Context, repo string, mrIID int64, sourceBranch, targetBranch, headSHA string) error {
	endpoint := strings.TrimRight(strings.TrimSpace(os.Getenv(EnvMillsOperatorURL)), "/")
	if endpoint == "" {
		return fmt.Errorf("mills merge queue unavailable: %s is unset", EnvMillsOperatorURL)
	}
	key := fmt.Sprintf("mrwatch_shepherd:%s:%d:%s", repo, mrIID, headSHA)
	payload, _ := json.Marshal(map[string]any{"producer": "mrwatch_shepherd", "idempotency_key": key, "project": repo, "mr_iid": mrIID, "source_branch": sourceBranch, "target_branch": targetBranch, "observed_sha": headSHA})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/api/mills/merge-queue/enqueue", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token := strings.TrimSpace(os.Getenv(EnvMillsOperatorToken)); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mills merge queue returned HTTP %d", resp.StatusCode)
	}
	return nil
}
