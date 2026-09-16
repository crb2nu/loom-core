package overseer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

const sandboxDrillIssueKey = "overseer-sandbox-drill"

var throttlingPercentRE = regexp.MustCompile(`(?i)cpu\s+throttled[^%\n]*?([0-9]+(?:\.[0-9]+)?)%`)

type SandboxDrillCheck struct {
	Name       string
	Passed     bool
	ExitCode   *int
	OutputTail string
}

type SandboxDrillVerdict struct {
	Passed *bool
	Checks []SandboxDrillCheck
}

type SandboxDrillRequest struct {
	Project, AgentID          string
	Checks, ExtraTestCommands []string
	FailFast                  bool
}

type SandboxDrillClient interface {
	QualityGate(context.Context, SandboxDrillRequest) (SandboxDrillVerdict, error)
	Stop(context.Context, string, string) error
}

// SandboxDrill exercises isolated devbox gates concurrently and maintains one
// deduplicated substrate incident. It never reads or mutates Mills backlog data.
type SandboxDrill struct {
	Client SandboxDrillClient
	Policy func() *mills.Policy
	Issues pipeline.IssueClient
	Logger *slog.Logger
	Now    func() time.Time

	mu           sync.Mutex
	incidentOpen bool
	issueIID     int64
}

func (d *SandboxDrill) Name() string { return "sandbox_drill" }

type drillGateOutcome struct {
	agentID, class, detail string
	duration               time.Duration
	ok                     bool
}

func (d *SandboxDrill) Tick(ctx context.Context) (TickResult, error) {
	var result TickResult
	if d == nil || d.Client == nil || d.Policy == nil {
		return result, errors.New("sandbox drill: not configured")
	}
	pol := d.Policy()
	if pol == nil || !pol.SandboxDrillEnabled() {
		return result, nil
	}
	cfg := pol.Overseers.SandboxDrill
	n := cfg.GateConcurrency()
	outcomes := make([]drillGateOutcome, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			agentID := fmt.Sprintf("mills-drill-%d", i)
			started := d.now()
			gateCtx, cancel := context.WithTimeout(ctx, cfg.Budget())
			verdict, err := d.Client.QualityGate(gateCtx, SandboxDrillRequest{
				Project: cfg.DrillProject(), AgentID: agentID,
				Checks:            []string{"fmt", "lint"},
				ExtraTestCommands: []string{"GOWORK=off go test -count=1 ./pkg/mcperror"},
				FailFast:          false,
			})
			cancel()
			duration := d.now().Sub(started)
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
			stopErr := d.Client.Stop(cleanupCtx, cfg.DrillProject(), agentID)
			cleanupCancel()
			outcomes[i] = classifyDrillGate(agentID, duration, cfg.Budget(), cfg.ThrottlingCeiling(), verdict, err, stopErr)
		}(i)
	}
	wg.Wait()
	result.Inspected = n
	allGreen := true
	for _, outcome := range outcomes {
		mills.SandboxDrillSeconds.WithLabelValues(outcome.class).Observe(outcome.duration.Seconds())
		if !outcome.ok {
			allGreen = false
		}
	}
	if allGreen {
		mills.SandboxDrillLastSuccessTimestamp.Set(float64(d.now().Unix()))
		d.clearIncident(ctx, &result)
		return result, nil
	}
	d.openIncident(ctx, &result, outcomes)
	return result, nil
}

func classifyDrillGate(agentID string, duration, budget time.Duration, ceiling int, verdict SandboxDrillVerdict, callErr, stopErr error) drillGateOutcome {
	o := drillGateOutcome{agentID: agentID, duration: duration, class: "verdict", ok: true}
	parts := []string{fmt.Sprintf("duration=%s", duration.Round(time.Millisecond))}
	if callErr != nil {
		o.class, o.ok = "error", false
		parts = append(parts, "gate_error="+callErr.Error())
	} else if duration > budget {
		o.class, o.ok = "timeout", false
		parts = append(parts, fmt.Sprintf("wall clock exceeded budget %s", budget))
	} else if verdict.Passed == nil {
		o.class, o.ok = "error", false
		parts = append(parts, "missing passed verdict")
	}
	if len(verdict.Checks) == 0 && callErr == nil {
		o.class, o.ok = "error", false
		parts = append(parts, "no check results")
	}
	for _, check := range verdict.Checks {
		exit := "missing"
		if check.ExitCode != nil {
			exit = strconv.Itoa(*check.ExitCode)
		} else {
			o.class, o.ok = "error", false
		}
		parts = append(parts, fmt.Sprintf("%s exit_code=%s output=%q", check.Name, exit, check.OutputTail))
		if !check.Passed {
			o.ok = false
		}
		lower := strings.ToLower(check.OutputTail)
		if strings.Contains(lower, "timed out") {
			o.class, o.ok = "timeout", false
		}
		if pct, found := throttlingPercent(check.OutputTail); found && pct > float64(ceiling) {
			o.ok = false
			parts = append(parts, fmt.Sprintf("cpu throttling %.1f%% exceeds ceiling %d%%", pct, ceiling))
		}
	}
	if verdict.Passed != nil && !*verdict.Passed {
		o.ok = false
	}
	if stopErr != nil {
		o.class, o.ok = "error", false
		parts = append(parts, "stop_error="+stopErr.Error())
	}
	o.detail = strings.Join(parts, "; ")
	return o
}

func throttlingPercent(text string) (float64, bool) {
	m := throttlingPercentRE.FindStringSubmatch(text)
	if len(m) != 2 {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	return v, err == nil
}

func (d *SandboxDrill) openIncident(ctx context.Context, res *TickResult, outcomes []drillGateOutcome) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.incidentOpen {
		return
	}
	d.incidentOpen = true
	if d.Issues == nil {
		return
	}
	details := make([]string, 0, len(outcomes))
	for _, o := range outcomes {
		details = append(details, fmt.Sprintf("- `%s`: %s", o.agentID, o.detail))
	}
	sort.Strings(details)
	description := "The scheduled sandbox drill did not complete cleanly.\n\n" + strings.Join(details, "\n") +
		"\n\nThe incident auto-closes after the next green drill. Gate interference can indicate residual agent-id truncation.\n\n" + pipeline.EscalationDedupMarker(sandboxDrillIssueKey)
	if dedup, ok := d.Issues.(pipeline.DedupIssueClient); ok {
		if ref, found, err := dedup.FindOpenEscalation(ctx, sandboxDrillIssueKey); err == nil && found {
			d.issueIID = ref.IID
			if err := dedup.CommentIssue(ctx, ref.IID, description); err != nil {
				res.Errored++
			}
			return
		}
	}
	resp, err := d.Issues.CreateIssue(ctx, pipeline.IssueRequest{BacklogID: sandboxDrillIssueKey, Title: "[mills-overseer] sandbox drill failed", Description: description, Labels: []string{"mills-escalation", "mills-overseer"}})
	if err != nil {
		res.Errored++
		return
	}
	d.issueIID = resp.IID
	res.Acted++
}

func (d *SandboxDrill) clearIncident(ctx context.Context, res *TickResult) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// The operator may restart between opening an incident and the recovery
	// drill. Reconcile the dedup marker so recovery still closes that issue
	// when the in-memory incident state has been lost.
	if d.issueIID == 0 {
		if dedup, ok := d.Issues.(pipeline.DedupIssueClient); ok && dedup != nil {
			if ref, found, err := dedup.FindOpenEscalation(ctx, sandboxDrillIssueKey); err != nil {
				res.Errored++
			} else if found {
				d.issueIID = ref.IID
			}
		}
	}
	if !d.incidentOpen && d.issueIID == 0 {
		return
	}
	d.incidentOpen = false
	if d.issueIID != 0 {
		if closer, ok := d.Issues.(pipeline.ClosableIssueClient); ok {
			if err := closer.CloseIssue(ctx, d.issueIID); err != nil {
				res.Errored++
			}
		}
	}
	d.issueIID = 0
}

func (d *SandboxDrill) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}
