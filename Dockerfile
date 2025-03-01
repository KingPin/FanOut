# Stage 1: Cross-compilation builder
FROM --platform=$BUILDPLATFORM golang:alpine AS builder

# Add necessary build dependencies
RUN apk add --no-cache \
    git \
    ca-certificates \
    tzdata

WORKDIR /src

# Set build arguments for cross-compilation
ARG TARGETOS TARGETARCH
ENV GOOS=$TARGETOS \
    GOARCH=$TARGETARCH \
    CGO_ENABLED=0

# Set build arguments for versioning
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown

# Copy only go.mod and go.sum first to leverage Docker cache
COPY go.mod go.sum ./
RUN go mod download

# Copy source code and build
COPY . .
RUN go build -trimpath \
    -ldflags="-w -s -X main.Version=${VERSION} -X main.GitCommit=${GIT_COMMIT} -X main.BuildTime=${BUILD_TIME}" \
    -o /app/fanout

# Create fanout user structure
RUN echo "fanout:x:1000:1000:FanOut Service:/:" > /etc/passwd && \
    echo "fanout:x:1000:" > /etc/group && \
    chown fanout:fanout /app/fanout

# Stage 2: Minimal production image
FROM scratch

# Copy necessary files from builder
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/passwd /etc/passwd
COPY --from=builder /etc/group /etc/group
COPY --from=builder /app/fanout /fanout

# Runtime configuration
USER fanout:fanout
EXPOSE 8080
ENV PORT=8080 \
    TARGETS="" \
    MAX_BODY_SIZE="10MB" \
    TZ=UTC

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/fanout", "-healthcheck"]

ENTRYPOINT ["/fanout"]
