# Copilot instructions — FanOut

This file gives targeted, repository-specific guidance for future Copilot sessions working on FanOut (Go, single-binary HTTP fan-out service).

---

## Build, test, and lint commands

Build (release):

    go build -trimpath -ldflags="-w -s" -o fanout

Build (debug):

    go build -tags=debug -o fanout-debug

Run locally (echo mode for development):

    TARGETS=localonly go run fanout.go

Run with production targets:

    TARGETS="https://a.example/,https://b.example/" PORT=8080 go run fanout.go

Run the full test suite (with race detector):

    go test -v -race ./...

Run a single test (exact name):

    go test -run '^TestSendRequest$' .

Run a single test with race and verbose output:

    go test -run '^TestSendRequest$' -v -race .

Formatting / vet:

    gofmt -w .
    go vet ./...

Security scan (used by README/CI):

    gosec ./...

Docker (local):

    docker build -t fanout:dev .

Multi-arch build (CI / release):

    docker buildx build --platform linux/amd64,linux/arm64 -t yourorg/fanout:latest .

CI workflows:

- .github/workflows/docker-image.yml
- .github/workflows/binary-release.yml

---

## High-level architecture (big picture)

- Entrypoint: `fanout.go` — sets up HTTP handlers and environment-based configuration in `init()`.

- Endpoints:
  - `ENDPOINT_PATH` (default `/fanout`) — main fan-out endpoint.
  - `/health` — simple health check.
  - `/version` — binary/version metadata.
  - `/metrics` — Prometheus handler (enabled when `METRICS_ENABLED=true`).

- Modes:
  - Echo mode: `TARGETS=localonly` — inbound requests are echoed back by `echoHandler`.
  - Multiplex mode: `TARGETS` contains comma-separated targets; `multiplex` spawns one goroutine per target.

- Dispatcher & concurrency:
  - `multiplex` launches a goroutine per configured target; responses are collected via a buffered channel and WaitGroup. Response order is not guaranteed.

- Request forwarding (`sendRequest`):
  - Re-creates the original request per target, clones headers via `cloneHeaders` (sensitive headers are logged), and sets Content-Length appropriately.
  - Implements retries for network errors and server (5xx) responses using exponential backoff + jitter.
  - Adds `X-Retry-Count` on retry attempts.

- Logging & metrics:
  - Asynchronous logger: `logQueue` is a buffered channel, format controlled by `LOG_FORMAT` (json/text) and `LOG_LEVEL`.
  - Prometheus metrics (prefixed `fanout_`) are recorded when `METRICS_ENABLED=true`.

---

## Key repository conventions and gotchas

- Configuration is environment-driven and read in `init()`; changing env vars requires restarting the process.

- Body handling / GetBody semantics:
  - The code uses a pre-read body optimization: when available, `preReadBody` is used for the first attempt; subsequent attempts call `getBody()`.
  - Tests use `httptest.NewRequest` which provides `GetBody`; when writing tests or mock requests, ensure `GetBody` is present or provide a pre-read body.

- Retry behavior:
  - Controlled via `MAX_RETRIES` (default: 3).
  - Network errors are detected by substring matching in `isRetryableError` (e.g., "connection refused", "timeout", "deadline exceeded", "connection reset", "no such host").
  - 5xx responses trigger retries up to the configured limit.

- Sensitive headers:
  - Configured via `SENSITIVE_HEADERS` (default `Authorization,Cookie`). `cloneHeaders` will log a warning when those are detected.

- Metrics naming and labels:
  - Prometheus metrics use fixed names (e.g., `fanout_requests_total`, `fanout_target_requests_total`). Avoid renaming these without updating monitoring.

- Concurrency expectations:
  - `multiplex` returns responses as they arrive. Do not rely on responses being in the same order as `TARGETS` unless ordering is explicitly implemented.

- Logging behavior:
  - Log entries are queued to `logQueue`; if the queue is full, entries may be dropped or logged directly when errors occur.

- Versioning variables:
  - `Version`, `GitCommit`, and `BuildTime` are populated at build time (defaults: dev/unknown). CI/release workflows set these.

---

## Where to look (short pointers)

- Core: `fanout.go` (single-file service implementation)
- Unit tests: `fanout_test.go`
- Container: `Dockerfile`, `compose.yml`
- CI: `.github/workflows/*`

---

## Repository workflow preferences

- Docker-only execution: All development, builds, tests and linters should be executed inside Docker containers, not on the host machine. This includes local runs, single-test runs, formatting, vetting, and CI-parity commands. Examples:

    # Run full test suite inside official Go container
    docker run --rm -v $(pwd):/src -w /src golang:1.24 go test -v -race ./...

    # Run a single test inside Docker
    docker run --rm -v $(pwd):/src -w /src golang:1.24 go test -run '^TestSendRequest$' -v -race .

    # Build inside Docker
    docker run --rm -v $(pwd):/src -w /src golang:1.24 go build -trimpath -ldflags="-w -s" -o fanout

  Prefer running via docker-compose (compose.yml) or CI-style containers so host toolchains are not required.

- Atomic commits: Make small, atomic commits for every logical change. Each commit should be self-contained and reversible. Use a separate branch per feature/bugfix and keep commit messages focused on a single purpose.

---

If something important is missing or you want additional coverage (examples, more test-run tips, or CI notes), ask and this file can be expanded.
