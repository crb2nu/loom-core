package mergequeue

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Queue telemetry, registered on the default registry so the operator's
// existing /metrics listener picks it up with no extra wiring (same
// convention as pkg/mills/metrics.go).
var (
	// DepthGauge tracks the total active (unsettled) queue entries across all
	// lanes. The spec's acceptance dashboard reads this.
	DepthGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mills_mergequeue_depth",
		Help: "Active merge queue entries across all lanes.",
	})

	// QueueWaitSeconds histograms enqueue→merged wall clock per candidate.
	// Buckets sized for 17–28 minute pipelines stacked a few deep.
	QueueWaitSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "mills_mergequeue_wait_seconds",
		Help:    "Wall-clock from enqueue to merged, per merged candidate.",
		Buckets: []float64{30, 120, 300, 600, 1200, 1800, 2700, 3600, 5400, 7200, 10800},
	})

	// EvictionsTotal counts terminal evictions by reason. A rising ci_red or
	// rebase_conflict rate is the queue doing its job; a rising head_moved or
	// merge_failed rate is a coordination problem worth an incident.
	EvictionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mills_mergequeue_evictions_total",
		Help: "Merge queue evictions by reason.",
	}, []string{"reason"})

	// MergedTotal counts candidates the queue landed.
	MergedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mills_mergequeue_merged_total",
		Help: "Merge queue candidates merged.",
	})
)
