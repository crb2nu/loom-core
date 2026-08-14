package mcpotel

import (
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// NoopTracer returns a named tracer that never records or exports spans.
// Daemons use it when trace export was configured but could not be enabled,
// keeping request instrumentation safe without falling back to a process-wide
// provider that another component may have installed.
func NoopTracer(name string) trace.Tracer {
	return noop.NewTracerProvider().Tracer(name)
}
