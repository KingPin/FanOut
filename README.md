# FanOut - Async HTTP Request Distributor

![Go Version](https://img.shields.io/badge/go-1.24%2B-blue)
![Docker Ready](https://img.shields.io/badge/docker-ready-green)

A high-performance HTTP request distributor that asynchronously fans out requests to multiple endpoints. Built for modern cloud-native architectures.

## Table of Contents
- [Features](#features-)
- [Quick Start](#quick-start-)
  - [Local Development](#local-development)
  - [Docker Deployment](#docker-deployment)
- [Configuration](#configuration-️)
  - [Environment Variables](#environment-variables)
  - [Operating Modes](#operating-modes)
- [Monitoring & Metrics](#monitoring--metrics-)
- [API Endpoints](#api-endpoints-️)
  - [Fan-out Endpoint](#fan-out-endpoint)
  - [Health Check](#health-check)
- [Docker Deployment](#docker-deployment-)
  - [Production Deployment](#production-deployment)
  - [Build Arguments](#build-arguments)
  - [Runtime Features](#runtime-features)
- [Architecture](#architecture-)
  - [Key Components](#key-components)
- [Development](#development-️)
  - [Prerequisites](#prerequisites)
  - [Build & Test](#build--test)
  - [Release Process](#release-process)
- [Contributing](#contributing-)
- [License](#license-)
- [Security](#security-)
- [FAQ](#faq-)

## Features 🚀

- **Async Request Processing**:
  - Concurrent request distribution
  - Local echo mode for testing
  - Configurable timeouts and retries
  - Request/Response logging
- **Security Features**:
  - Non-root container execution
  - Configurable request size limits
  - Sensitive header detection
- **Operational Excellence**:
  - Health check endpoint
  - Prometheus metrics and monitoring
  - Async logging with overflow protection
  - Docker health checks
  - Multi-arch container support (amd64, arm64)

## Quick Start 🚦

### Docker Deployment

```bash
# Pull the image
docker pull ghcr.io/yourorg/fanout:latest

# Run with configuration
docker run -p 8080:8080 \
  -e TARGETS="https://api1.example.com,https://api2.example.com" \
  -e MAX_BODY_SIZE="10MB" \
  ghcr.io/kingpin/fanout:latest
```

## Configuration ⚙️

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `TARGETS` | `""` (Required) | Comma-separated list of target URLs, or "localonly" for echo mode |
| `PORT` | `8080` | Server port |
| `MAX_BODY_SIZE` | `10MB` | Maximum request body size (human-readable format) |
| `TZ` | `UTC` | Container timezone |
| `ECHO_MODE_HEADER` | `false` | Add X-Echo-Mode header in echo responses |
| `ECHO_MODE_RESPONSE` | `simple` | Echo response format (`simple` or `full`) |
| `ENDPOINT_PATH` | `/fanout` | Configurable endpoint path |
| `REQUEST_TIMEOUT` | `30s` | Global request timeout (Go duration format) |
| `CLIENT_TIMEOUT` | `10s` | Per-target timeout (Go duration format) |
| `METRICS_ENABLED` | `false` | Enable Prometheus metrics endpoint |
| `MAX_RETRIES` | `3` | Maximum number of retry attempts for failed requests |
| `SENSITIVE_HEADERS` | `Authorization,Cookie` | Comma-separated list of headers that should trigger warnings |

### Example .env File

```
# Core configuration
TARGETS=https://analytics.service,https://audit.service
PORT=8080
MAX_BODY_SIZE=10MB

# Path and request handling
ENDPOINT_PATH=/api/v1/fanout REQUEST_TIMEOUT=15s
CLIENT_TIMEOUT=5s
MAX_RETRIES=2

# Security settings
SENSITIVE_HEADERS=Authorization,Cookie,X-API-Key

# Metrics and monitoring
METRICS_ENABLED=true

# Echo mode settings (for development)
TARGETS=localonly     # only use TARGETS ONCE, either localonly OR remote URLs. 
ECHO_MODE_HEADER=true
ECHO_MODE_RESPONSE=full
```

## Monitoring & Metrics 📊

FanOut provides built-in Prometheus metrics for real-time monitoring and alerting.

### Enabling Metrics

```bash
# Enable metrics endpoint
export METRICS_ENABLED=true
```

### Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `fanout_requests_total` | Counter | Total number of processed requests by path and method |
| `fanout_target_requests_total` | Counter | Requests sent to targets by target URL and status |
| `fanout_request_duration_seconds` | Histogram | Request latency distribution by target |
| `fanout_active_requests` | Gauge | Number of requests currently being processed |
| `fanout_request_body_size_bytes` | Histogram | Size of request bodies |
| `fanout_retries_total` | Counter | Number of retry attempts by target and status |
| `fanout_retry_success_total` | Counter | Number of successful requests after retry |

### Operating Modes

#### Normal Fan-out Mode
```bash
export TARGETS="https://api1.example.com,https://api2.example.com"
```

#### Echo Mode (Local Development)
```bash
# Enable echo mode for testing
export TARGETS="localonly"

# Optional: Configure echo behavior
export ECHO_MODE_HEADER="true"    # Add diagnostic headers
export ECHO_MODE_RESPONSE="full"  # Return detailed request info

# Example echo response
curl -X POST http://localhost:8080/fanout \
  -H "Content-Type: application/json" \
  -d '{"test":"data"}'

# Response (with ECHO_MODE_RESPONSE=full):
{
  "headers": {
    "Content-Type": ["application/json"]
  },
  "body": "{\"test\":\"data\"}"
}
```

## API Endpoints 🛣️

### Fan-out Endpoint
```bash
POST /fanout
Content-Type: application/json

# Returns
[
  {
    "target": "https://api1.example.com",
    "status": 200,
    "body": "...",
    "latency": "150ms"
    "attempts": 2  // Total attempts including the initial request
  }
]
```

### Health Check
```bash
GET /health

# Returns
{"status": "healthy"}
```

## Docker Deployment 🐳

### Production Deployment

The project includes a fully configured `compose.yml` file with all available options and detailed comments. 
To deploy in production:

```bash
# Clone the repository
wget https://raw.githubusercontent.com/KingPin/FanOut/refs/heads/main/compose.yml

# Start the service
docker compose up -d

# View logs
docker compose logs -f
```

See [compose.yml](./compose.yml) for all available configuration options and environment variables.

### Build Arguments

```bash
docker build \
--build-arg VERSION=1.3.0 \
--build-arg MAX_BODY_SIZE=20971520 \
-t fanout:custom .
```

### Building the Image

```bash
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t yourorg/fanout:latest .
```

### Runtime Features
- Non-root execution (UID 1000)
- Built-in health checks
- Timezone support
- CA certificates included
- Minimal scratch-based image

## Architecture 📐

### Key Components:
1. **Request Ingestion**: Validate and sanitize inputs
2. **Async Dispatcher**: Goroutine-based forwarding engine
3. **Circuit Manager**: Monitor endpoint health
4. **Header Processor**: Filter and propagate headers
5. **Metrics Collector**: Track performance indicators (WIP)

## Development 🛠️

### Prerequisites
- Go 1.24+
- Docker (for container builds)

### Build & Test

# Run unit tests with race detection

go test -v -race ./...

# Build debug binary

go build -tags=debug -o fanout-debug

# Performance benchmark

wrk -t12 -c400 -d60s http://localhost:8080/fanout

### Testing

```bash
# Run tests
go test -v -race ./...

# Local development with echo mode
export TARGETS=localonly
go run fanout.go
```

### Building

```bash
# Build binary
go build -trimpath -ldflags="-w -s" -o fanout

# Build container
docker build -t fanout:dev .
```

### Release Process
1. Update version in `VERSION` file
2. Run security scan: `gosec ./...`
3. Build multi-arch image: `docker buildx build --platform linux/amd64,linux/arm64`

## Contributing 🤝

We welcome contributions! Please follow these steps:
1. Fork the repository
2. Create feature branch (`git checkout -b feature/improvement`)
3. Commit changes (`git commit -am 'Add amazing feature'`)
4. Push to branch (`git push origin feature/improvement`)
5. Open Pull Request

## License 📄

[MIT License](https://github.com/KingPin/FanOut/blob/main/LICENSE)

## Security 🔒

- Sensitive headers are automatically detected and logged
- All requests are size-limited
- Non-root container execution
- TLS certificate handling included

## FAQ ❓

**Q: How to handle failed downstream services?**  
A: Circuit breakers automatically disable failing endpoints after 5 consecutive errors

**Q: Can I add custom middleware?**  
A: Yes! Implement the `Middleware` interface and register in `main.go`

**Q: What monitoring is supported?**  
A: Built-in Prometheus metrics at `/metrics` (enable with `METRICS_ENABLED=true`) (WIP)

**Q: Maximum supported targets?**  
A: Tested with 500+ endpoints - scale horizontally for higher loads (needs new testing after recent updates)

**Q: How to secure sensitive data?**  
A: Headers like Authorization are automatically filtered - configure others via env

**Q: How are failed requests handled?**  
A: FanOut uses smart retry logic with exponential backoff and jitter. Configure with MAX_RETRIES (default: 3). Only retries on server errors (5xx) and network issues.