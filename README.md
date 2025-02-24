# FanOut - Async HTTP Request Distributor

![Go Version](https://img.shields.io/badge/go-1.21%2B-blue)
![Docker Ready](https://img.shields.io/badge/docker-ready-green)

A high-performance HTTP request distributor that asynchronously fans out requests to multiple endpoints. Built for modern cloud-native architectures.

## Table of Contents
- [Features](#features)
- [Quick Start](#quick-start)
- [Configuration](#configuration)
- [Docker Deployment](#docker-deployment)
- [Architecture](#architecture)
- [Development](#development)
- [Contributing](#contributing)
- [License](#license)

## Features 🚀

- **True Async Forwarding**: Immediate 202 response with background processing
- **Intelligent Routing**:
  - Circuit breaker pattern for endpoint failures
  - Connection pooling and keep-alive
  - Header sanitization and propagation
- **Enterprise Security**:
  - Non-root Docker execution
  - Request size limiting (10MB default)
  - Sensitive header filtering
- **Production Grade**:
  - Health checks endpoint
  - Structured JSON logging
  - Resource constraints
  - Prometheus metrics (WIP)

## Quick Start 🚦

### Local Execution

```bash
export TARGETS="https://service1.example.com,https://service2.example.com"
go run main.go
```

### Docker Run

```bash
docker run -p 8080:8080 \
-e TARGETS="https://backup.example.com" \
ghcr.io/yourrepo/fanout:latest
```

## Configuration ⚙️

### Environment Variables
| Variable | Default | Description |
|----------|---------|-------------|
| `TARGETS` | Required | Comma-separated endpoints |
| `PORT` | 8080 | HTTP listener port |
| `MAX_BODY_SIZE` | 10485760 | Max request size (bytes) |
| `HTTP_TIMEOUT` | 10s | Downstream request timeout |

### Example .env File

```
TARGETS=https://analytics.service,https://audit.service
HTTP_TIMEOUT=15s
SENSITIVE_HEADERS=Authorization,X-API-Key
```

## Docker Deployment 🐳

### Production Stack


# docker-compose.prod.yml

```yml
services:
  fanout:
    image: ghcr.io/yourrepo/fanout:latest
    ports:
      - "8080:8080"
    environment:
      - TARGETS=https://primary.service,https://secondary.service
    healthcheck:
    test: ["CMD", "wget", "--spider", "http://localhost:8080/health"]
    interval: 30s
    timeout: 5s
    retries: 3
```

### Build Arguments

```bash
docker build \
--build-arg VERSION=1.3.0 \
--build-arg MAX_BODY_SIZE=20971520 \
-t fanout:custom .
```

## Architecture 📐

### Key Components:
1. **Request Ingestion**: Validate and sanitize inputs
2. **Async Dispatcher**: Goroutine-based forwarding engine
3. **Circuit Manager**: Monitor endpoint health
4. **Header Processor**: Filter and propagate headers
5. **Metrics Collector**: Track performance indicators (WIP)

## Development 🛠️

### Build & Test


# Run unit tests with race detection

go test -v -race ./...

# Build debug binary

go build -tags=debug -o fanout-debug

# Performance benchmark

wrk -t12 -c400 -d60s http://localhost:8080/fanout


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

MIT


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
