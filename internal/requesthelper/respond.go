package requesthelper

import (
	"context"
	"encoding/json"
	"time"

	restful "github.com/emicklei/go-restful/v3"
	"go.opentelemetry.io/otel/trace"
)

// Meta is the metadata block attached to every JSON API response as a "meta"
// field. It lets a caller correlate their response with the observability
// stack: the trace_id is directly searchable in Tempo/Grafana.
type Meta struct {
	// TraceID identifies the request's distributed trace; empty when
	// telemetry is disabled.
	TraceID string `json:"trace_id,omitempty"`
	// ExecutionTimeMS is the time spent serving the request so far,
	// measured from when the service received it, in milliseconds.
	ExecutionTimeMS float64 `json:"execution_time_ms"`
}

// startTimeKey carries the request arrival time on the context; a private
// type prevents collisions with other packages' context keys.
type startTimeKey struct{}

func withStartTime(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, startTimeKey{}, t)
}

// MetaFrom builds the response metadata for a request context: the active
// span's trace id and the elapsed time since TracingFilter admitted the
// request. Both degrade to zero values when the middleware did not run.
func MetaFrom(ctx context.Context) Meta {
	var m Meta
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		m.TraceID = sc.TraceID().String()
	}
	if start, ok := ctx.Value(startTimeKey{}).(time.Time); ok {
		m.ExecutionTimeMS = float64(time.Since(start).Microseconds()) / 1000
	}
	return m
}

// WriteJSON writes payload as the response body with a "meta" field injected
// at the top level. The payload must marshal to a JSON object (all enact
// response types do); anything else is written unchanged.
func WriteJSON(req *restful.Request, resp *restful.Response, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		_ = resp.WriteHeaderAndJson(status, payload, restful.MIME_JSON)
		return
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		_ = resp.WriteHeaderAndJson(status, payload, restful.MIME_JSON)
		return
	}
	metaRaw, err := json.Marshal(MetaFrom(req.Request.Context()))
	if err == nil {
		obj["meta"] = metaRaw
	}
	_ = resp.WriteHeaderAndJson(status, obj, restful.MIME_JSON)
}

// errorBody is the uniform error response shape: {"error": ..., "meta": ...}.
type errorBody struct {
	Error string `json:"error"`
	Meta  Meta   `json:"meta"`
}

// WriteError writes the platform's standard JSON error body, including the
// response metadata, so failures carry a trace id the caller can report.
func WriteError(req *restful.Request, resp *restful.Response, status int, msg string) {
	_ = resp.WriteHeaderAndJson(status, errorBody{Error: msg, Meta: MetaFrom(req.Request.Context())}, restful.MIME_JSON)
}