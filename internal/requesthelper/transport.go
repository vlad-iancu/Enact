package requesthelper

import (
	"io"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// NewTransport wraps base with a RoundTripper that starts a client span for
// each outbound request and injects the active trace context into the request
// headers, so the callee's TracingFilter continues the same trace. Pass nil
// to wrap http.DefaultTransport.
//
// This is the client half of the tracing middleware: it is what physically
// propagates the trace across a service call.
func NewTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{base: base}
}

// WrapClient returns a copy of c whose Transport propagates trace context.
// The original client is not modified. A nil client is treated as a fresh
// http.Client with default settings.
func WrapClient(c *http.Client) *http.Client {
	if c == nil {
		return &http.Client{Transport: NewTransport(nil)}
	}
	clone := *c
	clone.Transport = NewTransport(c.Transport)
	return &clone
}

// Client returns a new http.Client that propagates trace context on every
// request. Use it (or WrapClient) for all service-to-service HTTP calls.
func Client() *http.Client {
	return &http.Client{Transport: NewTransport(nil)}
}

type transport struct {
	base http.RoundTripper
}

func (t *transport) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx := r.Context()
	tracer := otel.Tracer(scopeName)

	ctx, span := tracer.Start(ctx, r.Method+" "+r.URL.Path,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(clientRequestAttrs(r)...),
	)

	// Clone so header mutation and the new context never affect the caller's
	// request, then inject the trace context for the downstream service.
	r = r.Clone(ctx)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(r.Header))

	resp, err := t.base.RoundTrip(r)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}

	span.SetAttributes(semconv.HTTPResponseStatusCode(resp.StatusCode))
	if resp.StatusCode >= http.StatusBadRequest {
		span.SetStatus(codes.Error, http.StatusText(resp.StatusCode))
	}

	// End the span when the response body is closed so its duration covers the
	// full read, matching OpenTelemetry's HTTP client conventions.
	resp.Body = &spanEndingBody{ReadCloser: resp.Body, span: span}
	return resp, nil
}

func clientRequestAttrs(r *http.Request) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		semconv.HTTPRequestMethodKey.String(r.Method),
		semconv.URLFull(r.URL.String()),
		semconv.ServerAddress(r.URL.Hostname()),
	}
	if r.URL.Scheme != "" {
		attrs = append(attrs, semconv.URLScheme(r.URL.Scheme))
	}
	return attrs
}

// spanEndingBody ends the client span exactly once, when the response body is
// closed (or fully consumed).
type spanEndingBody struct {
	io.ReadCloser
	span  trace.Span
	ended bool
}

func (b *spanEndingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.end()
	}
	return n, err
}

func (b *spanEndingBody) Close() error {
	b.end()
	return b.ReadCloser.Close()
}

func (b *spanEndingBody) end() {
	if b.ended {
		return
	}
	b.ended = true
	b.span.End()
}
