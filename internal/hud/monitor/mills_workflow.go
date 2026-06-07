package monitor

import (
	"context"
	"log/slog"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// MillsWorkflowMonitor polls the Mills durable workflow journal (workflow_runs /
// workflow_steps, migration 004) directly off the *store.WorkflowDAO and caches
// a compact snapshot of active runs plus the most-recent step deltas. Its
// OnRefresh callback broadcasts a `hud.workflows` event so a step-log panel
// (plan .loom/134 §S4) can refresh on push instead of blind-polling.
//
// NAMING: this is distinct from WorkflowMonitor in workflows.go, which tracks
// *agent* workflows (the agent_workflow_* approval gates) over the AgentBridge.
// This monitor tracks the Mills durable-runtime journal — hence the `Mills`
// prefix.
//
// It mirrors MillsMonitor's shape (BaseMonitor embed + cached snapshot + a
// refresh that Updates), but the source differs by design: MillsMonitor polls
// the operator status over HTTP, whereas the operator that OWNS the journal can
// read the DAO in-process. That is why this monitor takes a DAO, not a URL, and
// is wired into the operator's errgroup (cmd/loom-mills-operator) alongside the
// workflow scheduler — it sits next to the source of truth.
//
// The operator has no browser SSE hub of its own; it wires OnRefresh to a
// structured log line. When the Mac-side HUD grows a workflows channel (S4b),
// the same MillsWorkflowSnapshot is what it will marshal onto `hud.workflows`.
type MillsWorkflowMonitor struct {
	BaseMonitor[MillsWorkflowSnapshot]
	dao         *store.WorkflowDAO
	recentSteps int
}

// MillsWorkflowRunBrief is the per-run summary in a snapshot: enough for a panel
// to list active runs without a detail fetch.
type MillsWorkflowRunBrief struct {
	ID        string     `json:"id"`
	BacklogID string     `json:"backlog_id,omitempty"`
	Engine    string     `json:"engine"`
	Template  string     `json:"template"`
	State     string     `json:"state"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	CostUSD   float64    `json:"cost_usd"`
}

// MillsWorkflowStepDelta is a recent journal step surfaced in the snapshot,
// carrying the same server-derived intent the detail endpoint exposes: the panel
// can render a live timeline tick without re-deriving cache-hit vs live.
type MillsWorkflowStepDelta struct {
	RunID       string     `json:"run_id"`
	StepKey     string     `json:"step_key"`
	EventType   string     `json:"event_type"`
	Status      string     `json:"status"`
	CostSource  string     `json:"cost_source,omitempty"`
	EffectCount int        `json:"effect_count"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
}

// MillsWorkflowSnapshot is the broadcast payload (`hud.workflows`). ActiveRuns is
// the set of running imperative runs; RecentSteps is the newest step deltas
// across those runs; the counters give a panel its headline numbers without a
// KPI round-trip.
type MillsWorkflowSnapshot struct {
	ActiveRuns       []MillsWorkflowRunBrief  `json:"active_runs"`
	RecentSteps      []MillsWorkflowStepDelta `json:"recent_steps"`
	ActiveRunCount   int                      `json:"active_run_count"`
	QuarantinedCount int                      `json:"quarantined_run_count"`
	GeneratedAt      time.Time                `json:"generated_at"`
}

const defaultMillsWorkflowRecentSteps = 25

// NewMillsWorkflowMonitor builds a monitor over the workflow DAO. A nil dao
// makes the monitor inert (refresh is a no-op) so a degraded boot — no store
// wired — keeps the operator's errgroup balanced without emitting empty events.
func NewMillsWorkflowMonitor(dao *store.WorkflowDAO, logger *slog.Logger) *MillsWorkflowMonitor {
	m := &MillsWorkflowMonitor{
		dao:         dao,
		recentSteps: defaultMillsWorkflowRecentSteps,
	}
	m.InitBase(logger, nil, "mills-workflow-monitor")
	return m
}

// Start begins the background polling loop at the given interval. Uses
// StartLoop (not Start) because the refresh keeps its own snapshot assembly and
// calls Update itself, matching the BaseMonitor "complex refresh" contract.
func (m *MillsWorkflowMonitor) Start(interval time.Duration) {
	m.StartLoop(interval, func() error { return m.Refresh() })
}

// Refresh forces an immediate poll + snapshot. Exposed for the operator's
// post-startup warm-up and for tests.
func (m *MillsWorkflowMonitor) Refresh() error {
	snap, err := m.collect(context.Background())
	if err != nil {
		return err
	}
	m.Update(snap)
	return nil
}

// collect reads the journal and assembles a snapshot. It is split out from
// Refresh so tests can assert the snapshot shape without driving the loop.
func (m *MillsWorkflowMonitor) collect(ctx context.Context) (MillsWorkflowSnapshot, error) {
	if m.dao == nil {
		return MillsWorkflowSnapshot{GeneratedAt: time.Now().UTC()}, nil
	}

	runs, err := m.dao.ListRunningImperativeRuns(ctx)
	if err != nil {
		return MillsWorkflowSnapshot{}, err
	}
	quarantined, err := m.dao.CountRunsByState(ctx, store.WorkflowRunQuarantined)
	if err != nil {
		return MillsWorkflowSnapshot{}, err
	}

	briefs := make([]MillsWorkflowRunBrief, 0, len(runs))
	var deltas []MillsWorkflowStepDelta
	for _, run := range runs {
		briefs = append(briefs, MillsWorkflowRunBrief{
			ID:        run.ID,
			BacklogID: run.BacklogID,
			Engine:    string(run.Engine),
			Template:  run.Template,
			State:     string(run.State),
			StartedAt: run.StartedAt,
			CostUSD:   run.CostUSD,
		})
		// Per-run step deltas: ListByRun is append-ordered, so the tail is the
		// newest. We collect across runs then trim to recentSteps below.
		steps, err := m.dao.ListByRun(ctx, run.ID)
		if err != nil {
			return MillsWorkflowSnapshot{}, err
		}
		for _, st := range steps {
			deltas = append(deltas, MillsWorkflowStepDelta{
				RunID:       st.RunID,
				StepKey:     st.StepKey,
				EventType:   string(st.EventType),
				Status:      string(st.Status),
				CostSource:  string(st.CostSource),
				EffectCount: st.EffectCount,
				EndedAt:     st.EndedAt,
			})
		}
	}
	// Keep the newest recentSteps deltas (tail of the concatenated, per-run
	// append-ordered lists). A small bound keeps the broadcast payload tiny.
	if m.recentSteps > 0 && len(deltas) > m.recentSteps {
		deltas = deltas[len(deltas)-m.recentSteps:]
	}

	return MillsWorkflowSnapshot{
		ActiveRuns:       briefs,
		RecentSteps:      deltas,
		ActiveRunCount:   len(briefs),
		QuarantinedCount: quarantined,
		GeneratedAt:      time.Now().UTC(),
	}, nil
}
