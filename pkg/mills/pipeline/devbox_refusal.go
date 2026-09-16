package pipeline

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// DevboxQuotaDependency is the external dependency a run is attributed to when
// the devbox namespace ResourceQuota refused its sandbox pod
// (bl-devbox-sandbox-quota-headroom-20260913): the fix is quota headroom, never
// the diff, so the escalation names it instead of reading as anonymous infra.
const DevboxQuotaDependency = "devbox_quota"

// devboxRefusalReasonQuota is the one refusal reason today; the label
// vocabulary is closed so the series stays bounded.
const devboxRefusalReasonQuota = "quota"

// DevboxSandboxRefusedTotal counts quality-gate calls the devbox refused before
// any check ran, by reason. Read against
// mills_pipeline_escalation_class_total{class="infra"} to see how much of the
// infra budget is quota headroom rather than broken images.
var DevboxSandboxRefusedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "mills_devbox_sandbox_refused_total",
	Help: "Devbox quality-gate calls refused before any check ran, by reason (quota).",
}, []string{"reason"})

// noteSandboxRefusal counts and logs a refused gate call. The tests stage calls
// it once per gate call (main and baseline), so the counter is one-per-refusal
// however many times the runner classifies the error afterwards.
func noteSandboxRefusal(jc JobContext, project, agentID string, err error) {
	if !isDevboxQuotaRefusal(err) {
		return
	}
	DevboxSandboxRefusedTotal.WithLabelValues(devboxRefusalReasonQuota).Inc()
	slog.Default().Warn("mills devbox sandbox refused", "reason", devboxRefusalReasonQuota,
		"run", jc.Run.ID, "project", project, "agent_id", agentID, "error", err)
}
