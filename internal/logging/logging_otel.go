package logging

import (
	"context"
	"fmt"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	otellogglobal "go.opentelemetry.io/otel/log/global"
)

// otelBridgeEnabled gates whether log records are also emitted to the global
// OpenTelemetry LoggerProvider (and thus shipped to Loki). It is set once,
// during service startup, before any concurrent logging begins, so a plain
// bool without synchronisation is sufficient.
var otelBridgeEnabled bool

// bridgeScope names this bridge as the instrumentation scope on log records.
const bridgeScope = "enact/internal/logging"

// EnableOTelBridge turns on mirroring of every printed record to the global
// OpenTelemetry LoggerProvider. The service runtime calls this after
// telemetry.Init has installed a real provider; until then (and when
// telemetry is disabled) the global provider is a no-op and nothing ships.
func EnableOTelBridge() { otelBridgeEnabled = true }

// emitOTel forwards one record to the OpenTelemetry log pipeline. Passing ctx
// lets the SDK attach the active span's trace context to the record, so logs
// and traces line up in Grafana. It is a no-op unless the bridge is enabled.
func emitOTel(ctx context.Context, level Level, msg string, base, extra []field) {
	if !otelBridgeEnabled {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var rec otellog.Record
	rec.SetTimestamp(time.Now())
	rec.SetBody(otellog.StringValue(msg))
	rec.SetSeverity(toSeverity(level))
	rec.SetSeverityText(level.String())
	for _, f := range base {
		rec.AddAttributes(otellog.String(f.key, fmt.Sprint(f.value)))
	}
	for _, f := range extra {
		rec.AddAttributes(otellog.String(f.key, fmt.Sprint(f.value)))
	}

	otellogglobal.GetLoggerProvider().Logger(bridgeScope).Emit(ctx, rec)
}

// toSeverity maps an enact log level to an OpenTelemetry severity number.
func toSeverity(l Level) otellog.Severity {
	switch l {
	case LevelError:
		return otellog.SeverityError
	case LevelWarn:
		return otellog.SeverityWarn
	case LevelInfo:
		return otellog.SeverityInfo
	case LevelDebug:
		return otellog.SeverityDebug
	default:
		return otellog.SeverityInfo
	}
}
