package requesthelper

import (
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// instruments holds the HTTP server RED metrics recorded by TracingFilter.
// They are exported to Mimir through the global meter provider.
type instruments struct {
	// requests counts handled HTTP requests (Rate + Errors, sliced by status).
	requests metric.Int64Counter
	// duration records request handling latency in seconds (Duration).
	duration metric.Float64Histogram
}

// serverInstruments lazily builds the server metrics once. The OpenTelemetry
// meter returns usable (no-op on error) instruments, so construction never
// fails in a way that would break request handling.
var serverInstruments = sync.OnceValue(func() *instruments {
	meter := otel.Meter(scopeName)
	requests, _ := meter.Int64Counter(
		"http.server.request.count",
		metric.WithDescription("Number of HTTP requests handled."),
		metric.WithUnit("{request}"),
	)
	duration, _ := meter.Float64Histogram(
		"http.server.request.duration",
		metric.WithDescription("Duration of HTTP request handling."),
		metric.WithUnit("s"),
	)
	return &instruments{requests: requests, duration: duration}
})
