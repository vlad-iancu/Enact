# syntax=docker/dockerfile:1
# One Dockerfile for every enact service: pass the cmd/ directory name as
# SERVICE (e.g. --build-arg SERVICE=enact-kb-api). The build stage runs on
# the builder's native architecture and Go cross-compiles for the target —
# pure Go, CGO off, so no emulation is ever needed.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG SERVICE
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${SERVICE}

# distroless static: CA certificates (Aiven/AWS TLS) and tzdata included,
# no shell. The binary lands at the fixed path /app because exec-form
# ENTRYPOINT cannot expand ${SERVICE}; deployments must therefore set
# SERVICE_NAME explicitly (os.Args[0] no longer carries the service name).
# No dotenv files ship in the image, so container env is the sole config
# source. Liveness: GET /healthz (no HEALTHCHECK here — no shell; compose
# uses restart policies instead).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]
