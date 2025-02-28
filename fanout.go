// Package main implements a HTTP request fan-out service that can either:
// 1. Echo back the incoming requests (when TARGETS=localonly)
// 2. Fan out/multiplex requests to multiple configured endpoints
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"os/signal"
	"syscall"

	"github.com/dustin/go-humanize" // Used for human-readable size parsing
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	defaultMaxBodySize = 10 * 1024 * 1024 // 10MB - Default maximum request body size
	logQueueSize       = 10000            // Size of the async logging buffer queue
	maxLogPayload      = 1024             // Maximum payload size to log before truncation

	// Default timeout values
	defaultRequestTimeout = 30 * time.Second
	defaultClientTimeout  = 10 * time.Second

	defaultEndpointPath = "/fanout" // Default endpoint path

	defaultMaxRetries   = 3                      // Default number of retry attempts
	initialRetryBackoff = 100 * time.Millisecond // Initial backoff before first retry
	maxRetryBackoff     = 1 * time.Second        // Maximum retry backoff
)

var (
	// Async logging setup - Provides non-blocking log operations
	logQueue    = make(chan string, logQueueSize)
	logOnce     sync.Once // Ensures logging goroutine is initialized only once
	maxBodySize int64     // Maximum body size for incoming requests

	// Timeout configuration
	requestTimeout time.Duration // Global request timeout
	clientTimeout  time.Duration // Per-target timeout

	// Header monitoring - Headers that should trigger warnings when detected
	sensitiveHeaders = map[string]bool{
		"Authorization": true, // Authentication credentials
		"Cookie":        true, // Session information
	}

	endpointPath string // Configurable endpoint path

	// Metrics configuration
	metricsEnabled bool

	// Prometheus metrics
	requestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fanout_requests_total",
			Help: "The total number of processed requests",
		},
		[]string{"path", "method"},
	)

	targetRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fanout_target_requests_total",
			Help: "The total number of requests sent to targets",
		},
		[]string{"target", "status"},
	)

	requestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "fanout_request_duration_seconds",
			Help:    "The request latencies in seconds",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 10), // 10ms to ~10s
		},
		[]string{"target"},
	)

	activeRequests = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "fanout_active_requests",
			Help: "The number of requests currently being processed",
		},
	)

	bodySize = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "fanout_request_body_size_bytes",
			Help:    "Size of request bodies in bytes",
			Buckets: prometheus.ExponentialBuckets(1024, 2, 10), // 1KB to ~1MB
		},
		[]string{"path"},
	)

	maxRetries int // Maximum number of retry attempts

	retriesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fanout_retries_total",
			Help: "The total number of retry attempts",
		},
		[]string{"target", "status"},
	)

	retrySuccess = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fanout_retry_success_total",
			Help: "The total number of successful requests after retry",
		},
		[]string{"target", "attempts"},
	)

	// OpenTelemetry configuration
	otelEnabled bool
	tracer      trace.Tracer
)

// init initializes the application settings.
// Sets up the async logging goroutine and parses environment variables.
func init() {
	// Initialize async logger - runs in a separate goroutine to avoid blocking
	logOnce.Do(func() {
		go func() {
			for entry := range logQueue {
				log.Print(entry) // Simply print the entry, no need to requeue
			}
		}()
	})

	// Parse and set maximum body size from environment variable
	sizeStr := os.Getenv("MAX_BODY_SIZE")
	if sizeStr == "" {
		maxBodySize = defaultMaxBodySize
		return
	}

	size, err := humanize.ParseBytes(sizeStr) // Convert human-readable size (e.g. "5MB") to bytes
	if err != nil {
		log.Printf("Invalid MAX_BODY_SIZE '%s', using default: %v", sizeStr, err)
		maxBodySize = defaultMaxBodySize
		return
	}
	maxBodySize = int64(size)

	// Parse timeout configurations
	if timeout := os.Getenv("REQUEST_TIMEOUT"); timeout != "" {
		if d, err := time.ParseDuration(timeout); err != nil {
			log.Printf("Invalid REQUEST_TIMEOUT '%s', using default: %v", timeout, err)
			requestTimeout = defaultRequestTimeout
		} else {
			requestTimeout = d
		}
	} else {
		requestTimeout = defaultRequestTimeout
	}

	if timeout := os.Getenv("CLIENT_TIMEOUT"); timeout != "" {
		if d, err := time.ParseDuration(timeout); err != nil {
			log.Printf("Invalid CLIENT_TIMEOUT '%s', using default: %v", timeout, err)
			clientTimeout = defaultClientTimeout
		} else {
			clientTimeout = d
		}
	} else {
		clientTimeout = defaultClientTimeout
	}

	// Parse endpoint path from environment
	if path := os.Getenv("ENDPOINT_PATH"); path != "" {
		// Ensure path starts with "/"
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		endpointPath = path
	} else {
		endpointPath = defaultEndpointPath
	}

	// Check if metrics are enabled
	metricsEnabled = strings.ToLower(os.Getenv("METRICS_ENABLED")) == "true"

	// Parse max retries from environment
	if retriesStr := os.Getenv("MAX_RETRIES"); retriesStr != "" {
		if retries, err := strconv.Atoi(retriesStr); err != nil || retries < 0 {
			log.Printf("Invalid MAX_RETRIES '%s', using default: %v", retriesStr, err)
			maxRetries = defaultMaxRetries
		} else {
			maxRetries = retries
		}
	} else {
		maxRetries = defaultMaxRetries
	}

	// Seed the random number generator
	rand.Seed(time.Now().UnixNano())

	// Check if OpenTelemetry is enabled
	otelEnabled = strings.ToLower(os.Getenv("OTEL_ENABLED")) == "true"

	// Initialize OpenTelemetry if enabled
	if otelEnabled {
		cleanup, err := initTracer()
		if err != nil {
			log.Printf("Failed to initialize OpenTelemetry: %v", err)
			otelEnabled = false
		} else {
			// Register cleanup on exit
			c := cleanup
			go func() {
				signals := make(chan os.Signal, 1)
				signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
				<-signals
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := c(ctx); err != nil {
					log.Printf("Failed to clean up OpenTelemetry: %v", err)
				}
			}()
		}
	}
}

// logAsync logs a message asynchronously to prevent blocking the request handler.
// Falls back to warning if the log queue is full.
// Parameters:
//   - format: Printf-style format string
//   - args: Arguments for the format string
func logAsync(format string, args ...interface{}) {
	entry := fmt.Sprintf(format, args...)
	select {
	case logQueue <- entry: // Non-blocking attempt to queue log entry
	default:
		log.Printf("WARNING: Log queue full, dropped entry: %s", entry)
	}
}

// cloneHeaders creates a copy of HTTP headers and flags sensitive headers.
// Parameters:
//   - original: The source HTTP headers to clone
//
// Returns:
//   - A new http.Header object with the same contents
func cloneHeaders(original http.Header) http.Header {
	cloned := make(http.Header)
	for k, vv := range original {
		if sensitiveHeaders[k] {
			logAsync("WARNING: Sensitive header detected: %s", k)
		}
		cloned[k] = vv
	}
	return cloned
}

// echoHandler responds to HTTP requests by echoing back the request details.
// Used in "localonly" mode for debugging or testing.
// Parameters:
//   - w: HTTP response writer
//   - r: HTTP request object
func echoHandler(w http.ResponseWriter, r *http.Request) {
	// Read limited body to prevent memory exhaustion attacks
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		logAsync("ERROR reading body: %v", err)
		http.Error(w, "Payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	defer r.Body.Close()

	// Prepare echo data - collects headers and body for response
	echoData := map[string]interface{}{
		"headers": r.Header,
		"body":    string(bodyBytes),
	}

	// Set response headers based on environment configuration
	if os.Getenv("ECHO_MODE_HEADER") == "true" {
		w.Header().Set("X-Echo-Mode", "active")
	}

	// Choose response format based on environment configuration
	switch os.Getenv("ECHO_MODE_RESPONSE") {
	case "full":
		// Return detailed request information
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(echoData)
	default:
		// Return simple acknowledgement
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"status": "echoed"})
	}

	// Async logging with truncation to prevent log flooding
	loggedBody := string(bodyBytes)
	if len(loggedBody) > maxLogPayload {
		loggedBody = loggedBody[:maxLogPayload] + "...[TRUNCATED]"
	}
	logAsync("ECHO REQUEST:\nHeaders: %+v\nBody: %s", r.Header, loggedBody)
}

// healthCheck responds to HTTP requests with a health status.
// Parameters:
//   - w: HTTP response writer
//   - r: HTTP request object
func healthCheck(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
}

// Response represents the structure of responses from target endpoints
type Response struct {
	Target   string        `json:"target"`
	Status   int           `json:"status"`
	Body     string        `json:"body,omitempty"`
	Error    string        `json:"error,omitempty"`
	Latency  time.Duration `json:"latency"`
	Attempts int           `json:"attempts,omitempty"` // Number of attempts (including initial)
}

// multiplex fans out the incoming request to all configured targets
func multiplex(w http.ResponseWriter, r *http.Request) {
	if metricsEnabled {
		requestsTotal.WithLabelValues(r.URL.Path, r.Method).Inc()
		activeRequests.Inc()
		defer activeRequests.Dec()
	}

	// Get or create span context
	ctx := r.Context()
	var span trace.Span
	if otelEnabled {
		ctx, span = tracer.Start(ctx, "multiplex_fanout",
			trace.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.path", r.URL.Path),
			))
		defer span.End()
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	// Read and validate the request body
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		logAsync("ERROR reading body: %v", err)
		writeJSONError(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if metricsEnabled {
		bodySize.WithLabelValues(r.URL.Path).Observe(float64(len(bodyBytes)))
	}

	// Parse targets from environment
	targets := strings.Split(os.Getenv("TARGETS"), ",")
	if len(targets) == 0 || (len(targets) == 1 && targets[0] == "") {
		if otelEnabled {
			span.SetStatus(codes.Error, "No targets configured")
			span.RecordError(fmt.Errorf("no targets configured"))
		}
		writeJSONError(w, "No targets configured", http.StatusServiceUnavailable)
		return
	}

	if otelEnabled {
		span.SetAttributes(attribute.Int("fanout.targets.count", len(targets)))
	}

	// Create HTTP client with timeout and instrumentation
	var client *http.Client
	if otelEnabled {
		client = &http.Client{
			Timeout: clientTimeout,
			Transport: otelhttp.NewTransport(
				http.DefaultTransport,
				otelhttp.WithPropagators(otel.GetTextMapPropagator())),
		}
	} else {
		client = &http.Client{
			Timeout: clientTimeout,
		}
	}

	// Fan out requests
	responses := make([]Response, 0, len(targets))
	var wg sync.WaitGroup
	respChan := make(chan Response, len(targets))

	for _, target := range targets {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			start := time.Now()
			resp := sendRequest(ctx, client, target, r, bodyBytes)
			resp.Latency = time.Since(start)
			respChan <- resp
		}(target)
	}

	// Wait for all requests to complete
	go func() {
		wg.Wait()
		close(respChan)
	}()

	// Collect responses
	for resp := range respChan {
		responses = append(responses, resp)
	}

	// Send response
	w.Header().Set("Content-Type", "application/json")
	if err := writeJSON(w, responses); err != nil {
		logAsync("ERROR writing response: %v", err)
		return
	}
}

// sendRequest sends a single request to a target
func sendRequest(ctx context.Context, client *http.Client, target string, originalReq *http.Request, body []byte) Response {
	resp := Response{Target: target}

	startTime := time.Now()

	// Create span for this target request
	var span trace.Span
	if otelEnabled {
		ctx, span = tracer.Start(ctx, "fanout_target_request",
			trace.WithAttributes(
				attribute.String("target.url", target),
				attribute.String("http.method", originalReq.Method),
				attribute.Int("request.body.size", len(body)),
			))
		defer span.End()
	}

	defer func() {
		// Record metrics and spans
		if metricsEnabled {
			duration := time.Since(startTime).Seconds()
			requestDuration.WithLabelValues(target).Observe(duration)

			status := strconv.Itoa(resp.Status)
			if resp.Status == 0 && resp.Error != "" {
				status = "error"
			}
			targetRequestsTotal.WithLabelValues(target, status).Inc()
		}

		if otelEnabled {
			// Set span status based on response
			if resp.Error != "" {
				span.SetStatus(codes.Error, resp.Error)
				span.RecordError(fmt.Errorf(resp.Error))
			} else {
				span.SetStatus(codes.Ok, "")
			}

			// Record response attributes
			span.SetAttributes(
				attribute.Int("http.status_code", resp.Status),
				attribute.Int("fanout.attempts", resp.Attempts),
				attribute.String("fanout.latency", resp.Latency.String()),
			)
		}
	}()

	// Track retries
	var err error
	var response *http.Response
	attempts := 0
	backoff := initialRetryBackoff

	for attempts <= maxRetries { // <= to include initial attempt
		if attempts > 0 {
			// This is a retry attempt
			logAsync("Retry %d/%d for %s after %v", attempts, maxRetries, target, backoff)
			if metricsEnabled {
				retriesTotal.WithLabelValues(target, "attempt").Inc()
			}

			// Apply backoff with jitter
			select {
			case <-time.After(addJitter(backoff)):
				// Continue with retry
			case <-ctx.Done():
				// Context timeout/cancellation during backoff
				resp.Status = http.StatusGatewayTimeout
				resp.Error = fmt.Sprintf("Context cancelled during retry: %v", ctx.Err())
				return resp
			}

			// Increase backoff for next iteration (exponential backoff)
			backoff = min(backoff*2, maxRetryBackoff)
		}

		// Create new request for this attempt
		req, err := http.NewRequestWithContext(ctx, originalReq.Method, target, bytes.NewReader(body))
		if err != nil {
			resp.Status = http.StatusInternalServerError
			resp.Error = fmt.Sprintf("Failed to create request: %v", err)
			return resp // Don't retry on request creation failures
		}

		// Clone headers
		req.Header = cloneHeaders(originalReq.Header)

		// Add retry attempt header for debugging
		if attempts > 0 {
			req.Header.Set("X-Retry-Count", strconv.Itoa(attempts))
		}

		// Send request
		response, err = client.Do(req)

		// Handle connection errors
		if err != nil {
			// Check if we should retry
			if isRetryableError(err) && attempts < maxRetries {
				attempts++
				continue // Try again
			}

			resp.Status = http.StatusServiceUnavailable
			resp.Error = fmt.Sprintf("Request failed: %v", err)
			return resp
		}

		// Check status code for retry
		if response.StatusCode >= 500 && attempts < maxRetries {
			// Server error, try again
			response.Body.Close() // Close body before retry
			attempts++
			continue
		}

		// Success or non-retryable status
		break
	}

	// Track successful retries
	if attempts > 0 && response != nil && response.StatusCode < 500 {
		if metricsEnabled {
			retrySuccess.WithLabelValues(target, strconv.Itoa(attempts)).Inc()
		}
		logAsync("Request succeeded after %d retries to %s", attempts, target)
	}

	defer response.Body.Close()

	// Read response body
	respBody, err := io.ReadAll(io.LimitReader(response.Body, maxBodySize))
	if err != nil {
		resp.Status = http.StatusInternalServerError
		resp.Error = fmt.Sprintf("Failed to read response: %v", err)
		return resp
	}

	resp.Status = response.StatusCode
	resp.Body = string(respBody)
	resp.Attempts = attempts + 1 // Include original attempt
	return resp
}

// Helper functions for retry mechanism
func isRetryableError(err error) bool {
	// Retry on network errors, timeouts, connection resets, etc.
	if err == nil {
		return false
	}

	// Check specific error types that should be retried
	if strings.Contains(err.Error(), "connection refused") ||
		strings.Contains(err.Error(), "timeout") ||
		strings.Contains(err.Error(), "connection reset") ||
		strings.Contains(err.Error(), "no such host") {
		return true
	}

	return false
}

func addJitter(d time.Duration) time.Duration {
	// Add up to 20% random jitter
	jitter := float64(d) * (0.8 + 0.4*rand.Float64())
	return time.Duration(jitter)
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// writeJSON writes JSON response with error handling
func writeJSON(w http.ResponseWriter, v interface{}) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		logAsync("ERROR encoding JSON: %v", err)
		return err
	}
	return nil
}

// writeJSONError writes a JSON error response
func writeJSONError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(w, map[string]string{"error": message})
}

// main is the entrypoint for the application.
// Sets up HTTP routes and starts the server.
func main() {
	log.SetOutput(os.Stdout) // Direct logs to standard output

	// Determine handler based on TARGETS environment variable
	targets := os.Getenv("TARGETS")

	var fanoutHandler http.Handler
	if targets == "localonly" {
		fanoutHandler = http.HandlerFunc(echoHandler)
		log.Print("Running in ECHO MODE")
	} else {
		fanoutHandler = http.HandlerFunc(multiplex)
	}

	// Wrap handlers with OpenTelemetry instrumentation if enabled
	if otelEnabled {
		fanoutHandler = otelhttp.NewHandler(
			fanoutHandler,
			"fanout_request",
			otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
		)

		// Also wrap health check handler
		healthHandler := otelhttp.NewHandler(
			http.HandlerFunc(healthCheck),
			"health_check",
		)
		http.Handle("/health", healthHandler)
	} else {
		http.HandleFunc("/health", healthCheck)
	}

	// Register main handler
	http.Handle(endpointPath, fanoutHandler)

	// Add metrics endpoint if enabled
	if metricsEnabled {
		http.Handle("/metrics", promhttp.Handler())
		log.Print("Metrics enabled at /metrics endpoint")
	}

	// Determine port from environment or use default
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080" // Default port
	}

	// Health check mode - check args length first
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%s/health", port))
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}

	log.Printf("Server starting on :%s (Endpoint: %s, Max body: %s, Metrics: %v, Telemetry: %v, Max Retries: %d)",
		port,
		endpointPath,
		humanize.Bytes(uint64(maxBodySize)),
		metricsEnabled,
		otelEnabled,
		maxRetries)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// Add an initTracer function to set up OpenTelemetry
func initTracer() (func(context.Context) error, error) {
	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "fanout"
	}

	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:4317" // Default OTLP endpoint
	}

	useInsecure := true
	if insecureStr := os.Getenv("OTEL_EXPORTER_OTLP_INSECURE"); insecureStr != "" {
		useInsecure = strings.ToLower(insecureStr) != "false"
	}

	// Set up a connection to the collector
	var secureOption grpc.DialOption
	if useInsecure {
		secureOption = grpc.WithTransportCredentials(insecure.NewCredentials())
	} else {
		secureOption = grpc.WithTransportCredentials(grpc.WithTransportCredentials())
	}

	// Create OTLP exporter
	ctx := context.Background()
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithDialOption(secureOption),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP trace exporter: %w", err)
	}

	// Configure sampling rate
	samplingRatio := 1.0 // Default to 100% sampling
	if ratioStr := os.Getenv("OTEL_TRACE_SAMPLING_RATIO"); ratioStr != "" {
		if ratio, err := strconv.ParseFloat(ratioStr, 64); err == nil {
			if ratio >= 0 && ratio <= 1 {
				samplingRatio = ratio
			}
		}
	}

	// Create resource with service information
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(serviceName),
			semconv.ServiceVersionKey.String("v1.0.0"),
			attribute.String("environment", getEnvironment()),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// Configure trace provider
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(samplingRatio)),
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	// Configure propagators based on environment or use defaults
	propagatorsEnv := os.Getenv("OTEL_PROPAGATORS")
	if propagatorsEnv == "" {
		propagatorsEnv = "tracecontext,baggage" // Default propagators
	}

	// Set up configured propagators
	propagators := propagation.NewCompositeTextMapPropagator()
	for _, p := range strings.Split(propagatorsEnv, ",") {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "tracecontext":
			propagators = propagation.NewCompositeTextMapPropagator(
				propagation.TraceContext{},
				propagators,
			)
		case "baggage":
			propagators = propagation.NewCompositeTextMapPropagator(
				propagation.Baggage{},
				propagators,
			)
		case "b3":
			// If you need B3 format support, add the B3 propagator package and configure it here
			logAsync("B3 propagation requested but not implemented")
		}
	}
	otel.SetTextMapPropagator(propagators)

	// Initialize tracer
	tracer = otel.Tracer("fanout")

	// Return a cleanup function
	return func(ctx context.Context) error {
		// Shutdown will flush any remaining spans and shut down the exporter
		return tp.Shutdown(ctx)
	}, nil
}

// Helper function to determine environment
func getEnvironment() string {
	env := os.Getenv("ENVIRONMENT")
	if env == "" {
		env = "production" // Default to production
	}
	return env
}
