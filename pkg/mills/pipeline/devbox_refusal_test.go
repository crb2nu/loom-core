package pipeline

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
)

// liveDevboxQuotaRefusal is the 2026-09-13 tests-stage error shape; the pod
// name's hashed run token contains "429" on purpose.
const liveDevboxQuotaRefusal = `devbox quality_gate: mcphub: devbox/devbox_quality_gate reported error: ensure sandbox: start container: create pod: pods "devbox-loom-core-loom-m-4295f0a1" is forbidden: exceeded quota: devbox-quota, requested: limits.memory=6Gi, used: limits.memory=60Gi, limited: limits.memory=64Gi`

// quotaStoppingDevbox fails every gate with err and records the releases.
type quotaStoppingDevbox struct {
	fakeDevbox
	stops []string
}

func (f *quotaStoppingDevbox) Stop(_ context.Context, _, agentID string) error {
	f.stops = append(f.stops, agentID)
	return nil
}

// A quota-refused gate still releases its sandbox, is counted exactly once in
// mills_devbox_sandbox_refused_total{reason="quota"} and logged; any other gate
// failure is released but not counted.
func TestDevboxWorker_QuotaRefusalIsCountedAndReleased(t *testing.T) {
	db := &quotaStoppingDevbox{fakeDevbox: fakeDevbox{err: errors.New(liveDevboxQuotaRefusal)}}
	jc := sampleJobContext("tests")
	w := &DevboxWorker{Client: db, Project: "loom-core", AgentID: "loom-mills-operator"}
	mainID := devboxAgentID(w.AgentID, jc.Run.ID, "")
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	counter := DevboxSandboxRefusedTotal.WithLabelValues(devboxRefusalReasonQuota)
	before := testutil.ToFloat64(counter)

	if _, err := w.Run(context.Background(), jc); !errors.Is(err, db.err) || Classify(err) != ClassInfra {
		t.Fatalf("Run error = %v, want the quota refusal classified infra", err)
	}
	if got := logs.String(); !strings.Contains(got, "mills devbox sandbox refused") ||
		!strings.Contains(got, "reason=quota") || !strings.Contains(got, "agent_id="+mainID) {
		t.Fatalf("refusal was not logged: %s", got)
	}
	db.err = errors.New("gate transport failure")
	if _, err := w.Run(context.Background(), jc); err == nil {
		t.Fatal("expected the transport failure")
	}
	if got := testutil.ToFloat64(counter) - before; got != 1 {
		t.Fatalf("refused counter delta = %v, want 1 (a transport failure is not a refusal)", got)
	}
	if want := []string{mainID, mainID}; !reflect.DeepEqual(db.stops, want) {
		t.Fatalf("releases = %v, want %v", db.stops, want)
	}
}

// The escalation a quota-refused run ends in is retryable infra attributed to
// devbox_quota — like devbox_baseline, the dependency does not turn it into a
// generic external_dependency escalation.
func TestEscalationMetadata_DevboxQuotaRefusalNamesDependency(t *testing.T) {
	md := escalationMetadataFromEvidence(ClassInfra, "stage tests errored after 3 attempts [class=infra]: "+liveDevboxQuotaRefusal, "")
	routed := routeExternalDependencyEscalation(md, md.ExternalDependencyID, md.ExternalDependency)
	for _, got := range []store.EscalationMetadata{md, routed} {
		if got.EscalationClass != string(ClassInfra) || got.FailureClass != string(FailureInfrastructure) ||
			got.ExternalDependencyID != DevboxQuotaDependency || got.ExternalDependency != DevboxQuotaDependency ||
			got.Retryable == nil || !*got.Retryable {
			t.Fatalf("metadata = %+v, want retryable infra attributed to %s", got, DevboxQuotaDependency)
		}
	}
	other := routeExternalDependencyEscalation(md, "external_dependency.gitlab.auth_failure", "gitlab")
	plain := escalationMetadataFromEvidence(ClassInfra, "stage tests errored after 3 attempts [class=infra]: image build failed", "")
	if other.EscalationClass != telemetry.EscalationClassExternalDependency || plain.ExternalDependencyID == DevboxQuotaDependency {
		t.Fatalf("other = %+v, plain = %+v", other, plain)
	}
}

// Drive spaces quota-refused tests attempts a minute or more apart (1m, then
// 2m — never three inside one minute), lets them wait the quota out instead of
// tripping the identical-signature stop, spends the infra budget, and
// escalates as retryable infra attributed to devbox_quota.
func TestRunner_DevboxQuotaRefusalWaitsBetweenAttempts(t *testing.T) {
	st, run, item := newRunnerEnv(t)
	item.State = store.BacklogRunning
	if err := st.Backlog.Put(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	disp := &fakeDispatcher{errFor: map[string]error{"tests": errors.New(liveDevboxQuotaRefusal)}}
	r := &Runner{Store: st, Dispatcher: disp, Stages: []Stage{
		{ID: "implement", Type: "agent_spawn", State: store.PipelineImplementing},
		{ID: "tests", Type: "devbox", State: store.PipelineTesting},
		{ID: "post_tests_gate", Type: "auto_gate", State: store.PipelineTesting, RetryFrom: "implement"},
	}}
	var waits []time.Duration
	r.RetryWait = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatal(err)
	}
	if calls := strings.Join(disp.callsList(), ","); calls != "implement,tests,tests,tests" ||
		!reflect.DeepEqual(waits, []time.Duration{time.Minute, 2 * time.Minute}) {
		t.Fatalf("calls=%s waits=%v", calls, waits)
	}
	saved, err := st.Pipeline.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := (&Escalator{Store: st}).BuildRecord(context.Background(), saved, item, "quota")
	if err != nil {
		t.Fatal(err)
	}
	if saved.State != store.PipelineEscalated || saved.EscalationClass != string(ClassInfra) ||
		saved.ExternalDependencyID != DevboxQuotaDependency || saved.ExternalDependency != DevboxQuotaDependency ||
		saved.EscalationRetryable == nil || !*saved.EscalationRetryable ||
		rec.EscalationClass != string(ClassInfra) || rec.ExternalDependencyID != DevboxQuotaDependency ||
		rec.Retryable == nil || !*rec.Retryable {
		t.Fatalf("run=%+v record=%+v", saved, rec)
	}
}
