package overseer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

type fakeDrillClient struct {
	mu        sync.Mutex
	started   chan struct{}
	release   chan struct{}
	responses map[string]SandboxDrillVerdict
	errs      map[string]error
	stops     map[string]int
}

func (f *fakeDrillClient) QualityGate(ctx context.Context, req SandboxDrillRequest) (SandboxDrillVerdict, error) {
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return SandboxDrillVerdict{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.responses[req.AgentID], f.errs[req.AgentID]
}
func (f *fakeDrillClient) Stop(_ context.Context, _, agent string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops[agent]++
	return nil
}

type fakeDrillIssues struct {
	mu      sync.Mutex
	created []pipeline.IssueRequest
	closed  []int64
	openIID int64
}

func (f *fakeDrillIssues) CreateIssue(_ context.Context, req pipeline.IssueRequest) (pipeline.IssueResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, req)
	return pipeline.IssueResponse{IID: 42}, nil
}
func (f *fakeDrillIssues) CloseIssue(_ context.Context, iid int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, iid)
	return nil
}
func (f *fakeDrillIssues) FindOpenEscalation(_ context.Context, _ string) (pipeline.IssueRef, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return pipeline.IssueRef{IID: f.openIID}, f.openIID != 0, nil
}
func (f *fakeDrillIssues) CommentIssue(_ context.Context, _ int64, _ string) error { return nil }

func boolp(v bool) *bool { return &v }
func intp(v int) *int    { return &v }
func greenVerdict(output string) SandboxDrillVerdict {
	return SandboxDrillVerdict{Passed: boolp(true), Checks: []SandboxDrillCheck{
		{Name: "fmt", Passed: true, ExitCode: intp(0)},
		{Name: "lint", Passed: true, ExitCode: intp(0), OutputTail: output},
		{Name: "extra_test_0", Passed: true, ExitCode: intp(0)},
	}}
}

func enabledDrillPolicy() *mills.Policy {
	return &mills.Policy{Overseers: mills.OverseersPolicy{Enabled: true, SandboxDrill: mills.SandboxDrillPolicy{Enabled: true, BudgetSeconds: 2, Concurrency: 2}}}
}

func TestSandboxDrillRunsGatesConcurrentlyAndStopsThem(t *testing.T) {
	f := &fakeDrillClient{started: make(chan struct{}, 2), release: make(chan struct{}), responses: map[string]SandboxDrillVerdict{"mills-drill-0": greenVerdict(""), "mills-drill-1": greenVerdict("")}, errs: map[string]error{}, stops: map[string]int{}}
	d := &SandboxDrill{Client: f, Policy: enabledDrillPolicy}
	done := make(chan error, 1)
	go func() { _, err := d.Tick(context.Background()); done <- err }()
	for range 2 {
		select {
		case <-f.started:
		case <-time.After(time.Second):
			t.Fatal("gates did not overlap")
		}
	}
	close(f.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(f.stops) != 2 || f.stops["mills-drill-0"] != 1 || f.stops["mills-drill-1"] != 1 {
		t.Fatalf("stops = %#v", f.stops)
	}
}

func TestSandboxDrillFailuresOpenDetailedIncidentAndRecoveryCloses(t *testing.T) {
	cases := []struct {
		name    string
		verdict SandboxDrillVerdict
		err     error
		want    string
	}{
		{name: "timeout", verdict: SandboxDrillVerdict{Passed: boolp(false), Checks: []SandboxDrillCheck{{Name: "lint", ExitCode: intp(124), OutputTail: "timed out after 900s"}}}, want: "timed out after 900s"},
		{name: "error", err: errors.New("hub unavailable"), want: "hub unavailable"},
		{name: "throttling", verdict: greenVerdict("cpu throttled 40% of periods"), want: "40.0% exceeds ceiling 25%"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mills.SandboxDrillLastSuccessTimestamp.Set(0)
			f := &fakeDrillClient{responses: map[string]SandboxDrillVerdict{"mills-drill-0": tc.verdict, "mills-drill-1": greenVerdict("")}, errs: map[string]error{"mills-drill-0": tc.err}, stops: map[string]int{}}
			issues := &fakeDrillIssues{}
			d := &SandboxDrill{Client: f, Policy: enabledDrillPolicy, Issues: issues}
			if _, err := d.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(issues.created) != 1 || !strings.Contains(issues.created[0].Description, tc.want) {
				t.Fatalf("incident = %#v, want %q", issues.created, tc.want)
			}
			if len(f.stops) != 2 {
				t.Fatalf("stops = %#v", f.stops)
			}
			if got := testutil.ToFloat64(mills.SandboxDrillLastSuccessTimestamp); got != 0 {
				t.Fatalf("last-success advanced after failed drill: %v", got)
			}
			f.responses["mills-drill-0"] = greenVerdict("")
			f.errs["mills-drill-0"] = nil
			if _, err := d.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(issues.closed) != 1 || issues.closed[0] != 42 {
				t.Fatalf("closed = %#v", issues.closed)
			}
			if got := testutil.ToFloat64(mills.SandboxDrillLastSuccessTimestamp); got <= 0 {
				t.Fatalf("last-success did not advance after recovery: %v", got)
			}
		})
	}
}

func TestSandboxDrillDisabledDoesNothing(t *testing.T) {
	f := &fakeDrillClient{responses: map[string]SandboxDrillVerdict{}, errs: map[string]error{}, stops: map[string]int{}}
	d := &SandboxDrill{Client: f, Policy: func() *mills.Policy { return &mills.Policy{} }}
	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.stops) != 0 {
		t.Fatalf("disabled drill stopped sandboxes: %#v", f.stops)
	}
}

func TestSandboxDrillRecoveryClosesIncidentFoundAfterRestart(t *testing.T) {
	f := &fakeDrillClient{responses: map[string]SandboxDrillVerdict{"mills-drill-0": greenVerdict(""), "mills-drill-1": greenVerdict("")}, errs: map[string]error{}, stops: map[string]int{}}
	issues := &fakeDrillIssues{openIID: 77}
	d := &SandboxDrill{Client: f, Policy: enabledDrillPolicy, Issues: issues}
	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(issues.closed) != 1 || issues.closed[0] != 77 {
		t.Fatalf("closed = %#v, want [77]", issues.closed)
	}
}

func TestClassifyDrillGateRequiresCompleteVerdict(t *testing.T) {
	o := classifyDrillGate("mills-drill-0", time.Second, 2*time.Second, 25, SandboxDrillVerdict{Passed: boolp(true), Checks: []SandboxDrillCheck{{Name: "fmt"}}}, nil, nil)
	if o.ok || o.class != "error" || !strings.Contains(o.detail, "exit_code=missing") {
		t.Fatalf("outcome = %+v", o)
	}
}
