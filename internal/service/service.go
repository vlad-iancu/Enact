// Package service provides the unified runtime scaffolding for enact
// services: loading configuration from environment variables (via
// github.com/sethvargo/go-envconfig) and running an HTTP server that hosts
// one or more go-restful WebServices.
//
// Two responsibilities live here:
//
//   - Config loading. Use Load / LoadContext / MustLoad to populate any
//     struct tagged with `env:"NAME[,option...]"` (see go-envconfig for the
//     supported options).
//
//   - Service lifecycle. Use Run as the single entry point a binary needs:
//     it loads .env then the per-service "{binary-name}.env" (which overrides
//     both shell env and .env), parses the service's own Config from
//     environment variables, invokes a caller-supplied Builder to construct
//     the application's web services (after env values are visible to any
//     Load calls the Builder makes), mounts them on a restful.Container, and
//     runs an http.Server that shuts down gracefully on context cancellation
//     or SIGINT/SIGTERM.
package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"syscall"
	"time"

	restful "github.com/emicklei/go-restful/v3"
	"github.com/joho/godotenv"
	"github.com/sethvargo/go-envconfig"

	"enact/internal/identity"
	"enact/internal/logging"
	"enact/internal/requesthelper"
	"enact/internal/telemetry"
)

// ---------------------------------------------------------------------------
// Config loading
// ---------------------------------------------------------------------------

// Load reads environment variables into the struct pointed to by v using a
// background context. Use LoadContext when a caller-supplied context is
// required (e.g. for custom lookupers that perform I/O).
func Load(v any) error {
	return LoadContext(context.Background(), v)
}

// LoadContext reads environment variables into v using the provided context.
func LoadContext(ctx context.Context, v any) error {
	return envconfig.Process(ctx, v)
}

// MustLoad is the panic-on-error variant of Load, intended for use in main()
// where a missing required variable should abort startup.
func MustLoad(v any) {
	if err := Load(v); err != nil {
		panic(err)
	}
}

// ---------------------------------------------------------------------------
// Service lifecycle
// ---------------------------------------------------------------------------

// SharedEnvFile is the cross-service dotenv path Run loads first. Per-service
// values (loaded after, from "{binary-name}.env") override anything here.
// Missing files are not an error; malformed files are.
const SharedEnvFile = ".env"

// Config holds the HTTP server's runtime settings. Values are populated from
// the environment via Load; sensible defaults are provided for every field
// so an empty environment works out of the box.
type Config struct {
	// Name identifies the service. Shown in the home page title and the
	// OpenAPI / Swagger UI titles. If unset, defaults to the binary name
	// (filepath.Base(os.Args[0])).
	Name              string        `env:"SERVICE_NAME"`
	Port              int           `env:"SERVICE_PORT, default=8080"`
	ReadHeaderTimeout time.Duration `env:"SERVICE_READ_HEADER_TIMEOUT, default=10s"`
	ShutdownTimeout   time.Duration `env:"SERVICE_SHUTDOWN_TIMEOUT, default=15s"`

	// Telemetry configures the OpenTelemetry export to the LGTM stack. Its
	// env-tagged fields are populated by the same Load call as the rest of
	// Config, so every service picks up telemetry settings automatically.
	Telemetry telemetry.Config
}

// Builder constructs the web services to mount on the HTTP server. It runs
// after .env has been loaded and the service Config has been parsed, so a
// Builder may freely call Load to populate its own service-specific config
// structs from the environment.
type Builder func(ctx context.Context) ([]*restful.WebService, error)

// Run is the standard entry point for enact binaries: load .env then the
// per-service "{binary-name}.env" override, populate the caller-supplied
// configuration from environment variables, build the application's web
// services, and run the HTTP server until cancellation or signal.
//
// cfg must be a non-nil pointer to a struct that either IS service.Config or
// embeds it. The embedded service.Config is used to drive the HTTP server;
// any sibling fields the caller adds (e.g. their own service-specific
// configuration) are populated by the same Load call and are available to
// the Builder via closure capture.
//
// Example:
//
//	type MyConfig struct {
//	    service.Config
//	    internal.BedrockConfig
//	}
//	var cfg MyConfig
//	service.Run(ctx, &cfg, func(ctx context.Context) ([]*restful.WebService, error) {
//	    // cfg is populated by the time this runs
//	})
func Run(ctx context.Context, cfg any, build Builder) error {
	if err := godotenv.Load(SharedEnvFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("service: load %s: %w", SharedEnvFile, err)
	}
	perServiceFile := filepath.Base(os.Args[0]) + ".env"
	if err := godotenv.Overload(perServiceFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("service: load %s: %w", perServiceFile, err)
	}
	if err := Load(cfg); err != nil {
		return fmt.Errorf("service: load config: %w", err)
	}
	sc, err := extractServiceConfig(cfg)
	if err != nil {
		return err
	}
	if sc.Name == "" {
		sc.Name = filepath.Base(os.Args[0])
	}
	return RunWithConfig(ctx, *sc, build)
}

// extractServiceConfig returns a pointer to the service.Config inside cfg.
// cfg must be a non-nil pointer to a struct that either IS service.Config or
// embeds (anonymously includes) it.
func extractServiceConfig(cfg any) (*Config, error) {
	v := reflect.ValueOf(cfg)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return nil, errors.New("service: cfg must be a non-nil pointer to a struct")
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return nil, errors.New("service: cfg must be a non-nil pointer to a struct")
	}
	configType := reflect.TypeOf(Config{})
	if v.Type() == configType {
		return v.Addr().Interface().(*Config), nil
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous && f.Type == configType {
			return v.Field(i).Addr().Interface().(*Config), nil
		}
	}
	return nil, errors.New("service: cfg must be *service.Config or embed service.Config")
}

// RunWithConfig is the explicit-configuration variant of Run. It does NOT
// load .env (the caller is expected to have already arranged the
// environment) and uses the supplied Config verbatim. Useful for tests.
func RunWithConfig(ctx context.Context, cfg Config, build Builder) error {
	// Bring telemetry up before building services so any spans/logs they emit
	// during construction are captured. Failure is non-fatal: the service must
	// serve traffic even if the LGTM stack is unreachable.
	providers, err := telemetry.Init(ctx, cfg.Telemetry, cfg.Name)
	if err != nil {
		log.Printf("service: telemetry init failed, continuing without it: %v", err)
	} else if !cfg.Telemetry.Disabled {
		logging.EnableOTelBridge()
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := providers.Shutdown(flushCtx); err != nil {
			log.Printf("service: telemetry shutdown: %v", err)
		}
	}()

	services, err := build(ctx)
	if err != nil {
		return fmt.Errorf("service: build services: %w", err)
	}

	container := restful.NewContainer()
	// Container-wide filters run for every route. Tracing goes first so the
	// identity filter (and everything after it) executes inside the request's
	// span and enriches the already-traced context.
	container.Filter(requesthelper.TracingFilter(cfg.Name))
	container.Filter(identity.Filter)
	container.Add(healthWebService())
	container.Add(homeWebService(cfg.Name))
	for _, ws := range services {
		container.Add(ws)
	}
	// Swagger introspects the container's already-registered WebServices, so
	// it must be added last.
	container.Add(swaggerWebService(container, cfg.Name))
	registerSwaggerUI(container, cfg.Name)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           container,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	}

	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("service: listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	select {
	case <-ctx.Done():
		log.Println("service: shutdown signal received")
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("service: server error: %w", err)
		}
		return nil
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("service: graceful shutdown: %w", err)
	}
	return nil
}
