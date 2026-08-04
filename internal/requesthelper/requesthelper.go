// Package requesthelper holds the HTTP request plumbing shared by every enact
// service: the tracing middleware that propagates distributed traces across
// service boundaries (so a request flowing through several services shows up
// in Tempo/Grafana as one connected trace), and the response writers that
// stamp every JSON reply with a "meta" block (trace id + execution time,
// respond.go) tying the response back to that trace.
//
// The middleware has two halves, one for each side of a service call:
//
//   - Server side: TracingFilter is a go-restful FilterFunction that EXTRACTS
//     the incoming W3C trace context from request headers, starts a server
//     span, records RED metrics, and puts the active span's context back on
//     the *http.Request so downstream handlers observe it.
//
//   - Client side: NewTransport / WrapClient / Client wrap an http.Client so
//     outbound calls to other services INJECT the active trace context into
//     their headers. This is what actually carries the trace across the wire.
//
// Together they close the loop: service A's handler runs under a span, an
// outbound call made with a wrapped client injects that span's context, and
// service B's TracingFilter extracts it and continues the same trace.
//
// The package relies on the OpenTelemetry globals installed by the telemetry
// package (tracer provider, meter provider, and the W3C trace-context +
// baggage propagator). If telemetry is disabled those globals are no-ops and
// this middleware degrades to near-zero overhead.
package requesthelper

// scopeName is the instrumentation scope reported for spans and metrics
// created here. It identifies this middleware in Tempo/Mimir.
const scopeName = "enact/internal/requesthelper"
