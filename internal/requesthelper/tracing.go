package requesthelper

import (
	"net/http"
	"time"

	restful "github.com/emicklei/go-restful/v3"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// TracingFilter returns a go-restful filter that turns each incoming request
// into a server span on the caller's trace and records HTTP RED metrics.
//
// Add it as a container-wide filter so every route is covered:
//
//	container.Filter(requesthelper.TracingFilter(serviceName))
//
// The filter extracts the upstream trace context from the request headers
// (W3C traceparent/tracestate + baggage) via the global propagator, so when
// the caller used a client wrapped by NewTransport the resulting span is a
// child of the caller's span and the whole path shows up as one trace.
func TracingFilter(serviceName string) restful.FilterFunction {
	tracer := otel.Tracer(scopeName)
	prop := otel.GetTextMapPropagator()
	inst := serverInstruments()

	return func(req *restful.Request, resp *restful.Response, chain *restful.FilterChain) {
		start := time.Now()
		r := req.Request
		ctx := prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))

		// A container filter runs before route selection, so the low-cardinality
		// route template is not known yet; fall back to the raw path and refine
		// the span name once the route has been chosen.
		route := req.SelectedRoutePath()
		if route == "" {
			route = r.URL.Path
		}

		ctx, span := tracer.Start(ctx, r.Method+" "+route,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(serverRequestAttrs(serviceName, r)...),
		)
		defer span.End()

		// Expose the span's context to downstream handlers so their spans and
		// log lines correlate to this request, and stash the arrival time so
		// response meta (respond.go) can report execution time.
		ctx = withStartTime(ctx, start)
		req.Request = r.WithContext(ctx)

		chain.ProcessFilter(req, resp)
		elapsed := time.Since(start).Seconds()

		// The matched route template is available now; prefer it for both the
		// span name and the metric label to keep cardinality bounded.
		if rp := req.SelectedRoutePath(); rp != "" {
			route = rp
			span.SetName(r.Method + " " + route)
			span.SetAttributes(semconv.HTTPRoute(route))
		}

		status := resp.StatusCode()
		if status == 0 {
			status = http.StatusOK
		}
		span.SetAttributes(semconv.HTTPResponseStatusCode(status))
		if status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, http.StatusText(status))
		}

		attrs := metric.WithAttributes(
			semconv.HTTPRequestMethodKey.String(r.Method),
			semconv.HTTPRoute(route),
			semconv.HTTPResponseStatusCode(status),
		)
		inst.requests.Add(ctx, 1, attrs)
		inst.duration.Record(ctx, elapsed, attrs)
	}
}

// serverRequestAttrs builds the OpenTelemetry HTTP server attributes for an
// incoming request, following the stable HTTP semantic conventions.
func serverRequestAttrs(serviceName string, r *http.Request) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		semconv.ServiceName(serviceName),
		semconv.HTTPRequestMethodKey.String(r.Method),
		semconv.URLPath(r.URL.Path),
		semconv.ServerAddress(r.Host),
	}
	if r.URL.Scheme != "" {
		attrs = append(attrs, semconv.URLScheme(r.URL.Scheme))
	}
	if ua := r.UserAgent(); ua != "" {
		attrs = append(attrs, semconv.UserAgentOriginal(ua))
	}
	if r.RemoteAddr != "" {
		attrs = append(attrs, semconv.ClientAddress(r.RemoteAddr))
	}
	return attrs
}
