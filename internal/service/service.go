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
//     it loads any local .env file, parses the service's own Config from
//     environment variables, invokes a caller-supplied Builder to construct
//     the application's web services (after .env values are visible to any
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
	"syscall"
	"time"

	restful "github.com/emicklei/go-restful/v3"
	"github.com/joho/godotenv"
	"github.com/sethvargo/go-envconfig"
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

// DefaultEnvFile is the dotenv path Run loads at startup. Missing files are
// not an error; malformed files are.
const DefaultEnvFile = ".env"

// Config holds the HTTP server's runtime settings. Values are populated from
// the environment via Load; sensible defaults are provided for every field
// so an empty environment works out of the box.
type Config struct {
	Port              int           `env:"SERVICE_PORT, default=8080"`
	ReadHeaderTimeout time.Duration `env:"SERVICE_READ_HEADER_TIMEOUT, default=10s"`
	ShutdownTimeout   time.Duration `env:"SERVICE_SHUTDOWN_TIMEOUT, default=15s"`
}

// Builder constructs the web services to mount on the HTTP server. It runs
// after .env has been loaded and the service Config has been parsed, so a
// Builder may freely call Load to populate its own service-specific config
// structs from the environment.
type Builder func(ctx context.Context) ([]*restful.WebService, error)

// Run is the standard entry point for enact binaries: load .env, parse the
// service Config, build the application's web services, and run the HTTP
// server until cancellation or signal.
func Run(ctx context.Context, build Builder) error {
	if err := godotenv.Load(DefaultEnvFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("service: load %s: %w", DefaultEnvFile, err)
	}
	var cfg Config
	if err := Load(&cfg); err != nil {
		return fmt.Errorf("service: load config: %w", err)
	}
	return RunWithConfig(ctx, cfg, build)
}

// RunWithConfig is the explicit-configuration variant of Run. It does NOT
// load .env (the caller is expected to have already arranged the
// environment) and uses the supplied Config verbatim. Useful for tests.
func RunWithConfig(ctx context.Context, cfg Config, build Builder) error {
	services, err := build(ctx)
	if err != nil {
		return fmt.Errorf("service: build services: %w", err)
	}

	container := restful.NewContainer()
	container.Add(healthWebService())
	for _, ws := range services {
		container.Add(ws)
	}

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
