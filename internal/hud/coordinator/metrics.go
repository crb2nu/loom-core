package coordinator

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/crb2nu/loom/pkg/llmusage"
)

// Metrics holds Prometheus metrics for the coordinator subsystem.
type Metrics struct {
	SubsystemRuns       *prometheus.CounterVec
	LLMCallDuration     *prometheus.HistogramVec
	PollDuration        prometheus.Histogram
	CircuitState        prometheus.Gauge
	CircuitTrips        prometheus.Counter
	Healthy             prometheus.Gauge
	ConsecutiveFailures prometheus.Gauge
	SummarizedSessions  prometheus.Gauge
	FallbackSummaries   prometheus.Counter
	CompactionOutcome   *prometheus.CounterVec
	CompactionDelta     *prometheus.HistogramVec

	// PromptTokens / CachedPromptTokens / CompletionTokens are read-only
	// token accounting from every coordinator LLM call. The cached counter is
	// usage.prompt_tokens_details.cached_tokens — the share of the prompt the
	// serving engine gave back for free. Divide cached by prompt for the warm
	// share; see docs/JOURNAL_ENGINE.md, "Reading the cache data".
	PromptTokens       *prometheus.CounterVec
	CachedPromptTokens *prometheus.CounterVec
	CompletionTokens   *prometheus.CounterVec

	registry *prometheus.Registry
}

// NewMetrics creates a Metrics instance with all metrics registered.
func NewMetrics() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
	}

	m.SubsystemRuns = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "loom",
			Subsystem: "coordinator",
			Name:      "subsystem_runs_total",
			Help:      "Total subsystem runs by subsystem name and status",
		},
		[]string{"subsystem", "status"},
	)

	m.LLMCallDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "loom",
			Subsystem: "coordinator",
			Name:      "llm_call_duration_seconds",
			Help:      "LLM call latency distribution per subsystem",
			Buckets:   prometheus.ExponentialBuckets(0.1, 2, 10), // 100ms to ~51s
		},
		[]string{"subsystem"},
	)

	m.PollDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "loom",
			Subsystem: "coordinator",
			Name:      "poll_duration_seconds",
			Help:      "Total poll cycle duration",
			Buckets:   prometheus.ExponentialBuckets(0.01, 2, 12), // 10ms to ~20s
		},
	)

	m.CircuitState = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "loom",
			Subsystem: "coordinator",
			Name:      "circuit_state",
			Help:      "Circuit breaker state: 0=closed, 1=open, 2=half-open",
		},
	)

	m.CircuitTrips = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: "loom",
			Subsystem: "coordinator",
			Name:      "circuit_trips_total",
			Help:      "Total circuit breaker open events",
		},
	)

	m.Healthy = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "loom",
			Subsystem: "coordinator",
			Name:      "healthy",
			Help:      "Coordinator health: 1=healthy, 0=unhealthy",
		},
	)

	m.ConsecutiveFailures = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "loom",
			Subsystem: "coordinator",
			Name:      "consecutive_failures",
			Help:      "Current consecutive poll failure count",
		},
	)

	m.SummarizedSessions = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "loom",
			Subsystem: "coordinator",
			Name:      "summarized_sessions",
			Help:      "Number of sessions in the in-memory summarized map",
		},
	)

	m.FallbackSummaries = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: "loom",
			Subsystem: "coordinator",
			Name:      "fallback_summaries_total",
			Help:      "Total extractive fallback summary count",
		},
	)

	m.CompactionOutcome = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "loom",
			Subsystem: "coordinator",
			Name:      "compaction_outcomes_total",
			Help:      "Compaction completions by trigger and prompt-pressure effectiveness",
		},
		[]string{"trigger", "effect"},
	)

	m.CompactionDelta = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "loom",
			Subsystem: "coordinator",
			Name:      "compaction_prompt_delta_tokens",
			Help:      "Positive prompt-token reductions observed after compaction",
			Buckets:   []float64{0, 128, 256, 512, 1024, 2048, 4096, 8192, 16384},
		},
		[]string{"trigger"},
	)

	m.PromptTokens = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "loom",
			Subsystem: "coordinator",
			Name:      "llm_prompt_tokens_total",
			Help:      "Cumulative prompt tokens sent to LLM backends (cached portion included), by component and served model",
		},
		[]string{"component", "model"},
	)

	m.CachedPromptTokens = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "loom",
			Subsystem: "coordinator",
			Name:      "llm_cached_prompt_tokens_total",
			Help:      "Cumulative prompt tokens served from the engine prefix cache, by component and served model. Divide by llm_prompt_tokens_total for the warm share; a flat 0 may mean the engine omits the field rather than a cold cache",
		},
		[]string{"component", "model"},
	)

	m.CompletionTokens = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "loom",
			Subsystem: "coordinator",
			Name:      "llm_completion_tokens_total",
			Help:      "Cumulative completion tokens generated by LLM backends, by component and served model",
		},
		[]string{"component", "model"},
	)

	m.registry.MustRegister(
		m.SubsystemRuns,
		m.LLMCallDuration,
		m.PollDuration,
		m.CircuitState,
		m.CircuitTrips,
		m.Healthy,
		m.ConsecutiveFailures,
		m.SummarizedSessions,
		m.FallbackSummaries,
		m.CompactionOutcome,
		m.CompactionDelta,
		m.PromptTokens,
		m.CachedPromptTokens,
		m.CompletionTokens,
	)

	return m
}

// RecordUsage implements llmusage.Sink so the flexinfer client can report each
// completion's token accounting into the coordinator's own registry.
//
// Zero-guarded per counter: a response that carried no usage block must not
// create a label pair that only ever holds zero, because an all-zero series is
// indistinguishable from a measured 0% hit rate on a dashboard.
func (m *Metrics) RecordUsage(component, model string, u llmusage.Usage) {
	if m == nil {
		return
	}
	if u.PromptTokens > 0 {
		m.PromptTokens.WithLabelValues(component, model).Add(float64(u.PromptTokens))
	}
	if u.CachedPromptTokens > 0 {
		m.CachedPromptTokens.WithLabelValues(component, model).Add(float64(u.CachedPromptTokens))
	}
	if u.CompletionTokens > 0 {
		m.CompletionTokens.WithLabelValues(component, model).Add(float64(u.CompletionTokens))
	}
}

// Handler returns an HTTP handler for the /metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
}

// RecordSubsystemRun records a subsystem execution with success or error status.
func (m *Metrics) RecordSubsystemRun(subsystem string, err error) {
	status := "success"
	if err != nil {
		status = "error"
	}
	m.SubsystemRuns.WithLabelValues(subsystem, status).Inc()
}

// RecordLLMCall records the duration of an LLM call for a subsystem.
func (m *Metrics) RecordLLMCall(subsystem string, duration time.Duration) {
	m.LLMCallDuration.WithLabelValues(subsystem).Observe(duration.Seconds())
}

// RecordPollCycle records the total duration of a poll cycle.
func (m *Metrics) RecordPollCycle(duration time.Duration) {
	m.PollDuration.Observe(duration.Seconds())
}

// RecordCompactionResult records prompt-pressure effectiveness for a compaction run.
func (m *Metrics) RecordCompactionResult(result *CompactionResult) {
	if result == nil {
		return
	}
	trigger := result.Trigger
	if trigger == "" {
		trigger = "unknown"
	}

	effect := "unmeasured"
	measured := result.PromptTokensBefore > 0 && (result.PromptTokensAfter > 0 || result.PromptTokensDelta != 0)
	if measured {
		if result.PromptTokensDelta > 0 {
			effect = "reduced"
			m.CompactionDelta.WithLabelValues(trigger).Observe(float64(result.PromptTokensDelta))
		} else {
			effect = "flat_or_increased"
		}
	}

	m.CompactionOutcome.WithLabelValues(trigger, effect).Inc()
}

// UpdateHealth updates the healthy gauge and consecutive failure count.
func (m *Metrics) UpdateHealth(healthy bool, failures int) {
	if healthy {
		m.Healthy.Set(1)
	} else {
		m.Healthy.Set(0)
	}
	m.ConsecutiveFailures.Set(float64(failures))
}

// UpdateCircuit updates the circuit state gauge. Increments the trip
// counter when the state transitions to open.
func (m *Metrics) UpdateCircuit(state CircuitState) {
	m.CircuitState.Set(float64(state))
	if state == StateOpen {
		m.CircuitTrips.Inc()
	}
}
