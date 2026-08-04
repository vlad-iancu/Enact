// Package telemetry wires the enact services into the LGTM observability
// stack (Loki, Grafana, Tempo, Mimir) using OpenTelemetry.
//
// It initialises the three OpenTelemetry signal providers and installs them
// as the process-wide globals, exporting each signal directly (OTLP/HTTP) to
// its backing store:
//
//   - Traces  -> Tempo   (OTEL_EXPORTER_OTLP_TRACES_ENDPOINT)
//   - Metrics -> Mimir   (OTEL_EXPORTER_OTLP_METRICS_ENDPOINT)
//   - Logs    -> Loki    (OTEL_EXPORTER_OTLP_LOGS_ENDPOINT)
//
// Grafana sits on top of all three for querying and correlation. Because the
// providers are registered as OpenTelemetry globals, the rest of the codebase
// obtains tracers/meters via otel.Tracer / otel.Meter and the logging package
// bridges its records to the global LoggerProvider — no component needs a
// direct reference to anything constructed here.
//
// Typical use lives in the service runtime:
//
//	providers, err := telemetry.Init(ctx, cfg.Telemetry, cfg.Name)
//	if err != nil { /* log and continue with no-op telemetry */ }
//	defer providers.Shutdown(context.Background())
//
// Init is best-effort: if a backend is unreachable at startup the exporters
// still construct (they connect lazily and buffer), so a missing LGTM stack
// never prevents a service from serving traffic.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otellogglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Config holds the environment-driven OpenTelemetry settings. Every field has
// a default that targets the local LGTM stack from
// deploy/docker-compose.lgtm.yml, so an empty environment "just works" once
// that stack is up. The endpoints follow the OpenTelemetry specification: a
// per-signal endpoint is the FULL URL including the signal path
// (e.g. .../v1/traces), unlike the base OTEL_EXPORTER_OTLP_ENDPOINT.
type Config struct {
	// Disabled turns telemetry off entirely. Init then returns a no-op set of
	// providers and installs no globals. Honours the OTel-standard variable.
	Disabled bool `env:"OTEL_SDK_DISABLED"`

	// ServiceVersion is reported as the service.version resource attribute.
	ServiceVersion string `env:"OTEL_SERVICE_VERSION, default=dev"`

	// Environment is reported as deployment.environment.name.
	Environment string `env:"OTEL_DEPLOYMENT_ENVIRONMENT, default=local"`

	// Per-signal OTLP/HTTP endpoints (full URLs, including the /v1/... path).
	TracesEndpoint  string `env:"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT, default=http://localhost:4318/v1/traces"`
	MetricsEndpoint string `env:"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT, default=http://localhost:9009/otlp/v1/metrics"`
	LogsEndpoint    string `env:"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT, default=http://localhost:3100/otlp/v1/logs"`

	// SampleRatio is the head-sampling probability for root spans (0..1).
	// Non-root spans inherit their parent's decision (ParentBased).
	SampleRatio float64 `env:"OTEL_TRACES_SAMPLER_ARG, default=1.0"`

	// MetricInterval is how often metrics are pushed to Mimir.
	MetricInterval time.Duration `env:"OTEL_METRIC_EXPORT_INTERVAL, default=15s"`
}

// Providers bundles the shutdown hooks for everything Init constructed.
type Providers struct {
	shutdownFuncs []func(context.Context) error
}

// Shutdown flushes and stops every provider Init created, aggregating any
// errors. It is safe to call on the zero value (no-op).
func (p *Providers) Shutdown(ctx context.Context) error {
	var err error
	for _, fn := range p.shutdownFuncs {
		if fn == nil {
			continue
		}
		err = errors.Join(err, fn(ctx))
	}
	return err
}

// Init builds the trace, metric and log providers, exports each signal
// directly to its LGTM backend over OTLP/HTTP, and installs them as the
// OpenTelemetry globals along with the W3C trace-context + baggage
// propagators used by the requesthelper middleware to carry context across
// service calls.
//
// serviceName is stamped onto every signal as the service.name resource
// attribute; it should be the same name the service reports elsewhere so
// traces, metrics and logs line up in Grafana.
//
// On any exporter construction error Init tears down whatever it already
// built and returns the error; callers may treat that as non-fatal and run
// without telemetry.
func Init(ctx context.Context, cfg Config, serviceName string) (*Providers, error) {
	p := &Providers{}
	if cfg.Disabled {
		return p, nil
	}

	res, err := newResource(ctx, cfg, serviceName)
	if err != nil {
		return p, err
	}

	// Propagators first: even if a provider fails, cross-service propagation
	// should be in place.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if err := p.initTraces(ctx, cfg, res); err != nil {
		_ = p.Shutdown(ctx)
		return &Providers{}, fmt.Errorf("telemetry: init traces: %w", err)
	}
	if err := p.initMetrics(ctx, cfg, res); err != nil {
		_ = p.Shutdown(ctx)
		return &Providers{}, fmt.Errorf("telemetry: init metrics: %w", err)
	}
	if err := p.initLogs(ctx, cfg, res); err != nil {
		_ = p.Shutdown(ctx)
		return &Providers{}, fmt.Errorf("telemetry: init logs: %w", err)
	}
	return p, nil
}

func newResource(ctx context.Context, cfg Config, serviceName string) (*resource.Resource, error) {
	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		resource.WithProcess(),
		resource.WithFromEnv(), // honours OTEL_RESOURCE_ATTRIBUTES
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
			attribute.String("deployment.environment.name", cfg.Environment),
		),
	)
	if err != nil {
		// resource.New returns a usable resource alongside a partial error
		// (e.g. a schema-URL merge conflict); prefer to proceed.
		if res == nil {
			return nil, fmt.Errorf("telemetry: build resource: %w", err)
		}
	}
	return res, nil
}

func (p *Providers) initTraces(ctx context.Context, cfg Config, res *resource.Resource) error {
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(cfg.TracesEndpoint))
	if err != nil {
		return err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	)
	otel.SetTracerProvider(tp)
	p.shutdownFuncs = append(p.shutdownFuncs, tp.Shutdown)
	return nil
}

func (p *Providers) initMetrics(ctx context.Context, cfg Config, res *resource.Resource) error {
	exp, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(cfg.MetricsEndpoint))
	if err != nil {
		return err
	}
	reader := sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(cfg.MetricInterval))
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)
	p.shutdownFuncs = append(p.shutdownFuncs, mp.Shutdown)
	return nil
}

func (p *Providers) initLogs(ctx context.Context, cfg Config, res *resource.Resource) error {
	exp, err := otlploghttp.New(ctx, otlploghttp.WithEndpointURL(cfg.LogsEndpoint))
	if err != nil {
		return err
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
		sdklog.WithResource(res),
	)
	otellogglobal.SetLoggerProvider(lp)
	p.shutdownFuncs = append(p.shutdownFuncs, lp.Shutdown)
	return nil
}
