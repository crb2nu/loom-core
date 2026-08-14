package muxstdio

import "log/slog"

// Option configures a Transport at construction time.
type Option func(*Transport)

// WithMetrics installs a Metrics implementation. A nil m is treated as a no-op
// metrics sink. The default sink is also no-op.
func WithMetrics(m Metrics) Option {
	return func(t *Transport) {
		if m == nil {
			t.metrics = nopMetrics{}
			return
		}
		t.metrics = m
	}
}

// WithLogger installs a slog.Logger for diagnostic output (dropped messages,
// transport-level warnings). A nil l falls back to slog.Default.
func WithLogger(l *slog.Logger) Option {
	return func(t *Transport) {
		if l == nil {
			t.logger = slog.Default()
			return
		}
		t.logger = l
	}
}
