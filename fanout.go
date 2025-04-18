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

	"github.com/dustin/go-humanize" // Used for human-readable size parsing
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Log levels
const (
	LogLevelDebug = iota
	LogLevelInfo
	LogLevelWarn
	LogLevelError

	defaultLogLevel  = LogLevelInfo // Default log level
	defaultLogFormat = "text"       // Default log format (text or json)
)

// LogEntry represents a structured log entry
type LogEntry struct {
	Time    time.Time         `json:"time"`
	Level   string            `json:"level"`
	Message string            `json:"message"`
	Context map[string]string `json:"context,omitempty"`
}

const (
	defaultMaxBodySize      = 10 * 1024 * 1024 // 10MB - Default maximum request body size
	logQueueSize            = 10000            // Size of the async logging buffer queue
	maxLogPayload           = 1024             // Maximum payload size to log before truncation
	defaultRequestTimeout   = 30 * time.Second
	defaultClientTimeout    = 10 * time.Second
	defaultEndpointPath     = "/fanout"
	defaultMaxRetries       = 3
	initialRetryBackoff     = 100 * time.Millisecond
	maxRetryBackoff         = 1 * time.Second
	defaultSensitiveHeaders = "Authorization,Cookie" // Default sensitive headers
)

var (
	logQueue         = make(chan LogEntry, logQueueSize)
	logOnce          sync.Once
	maxBodySize      int64
	requestTimeout   time.Duration
	clientTimeout    time.Duration
	sensitiveHeaders map[string]bool
	endpointPath     string
	metricsEnabled   bool
	httpClient       *http.Client

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
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 10),
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
			Buckets: prometheus.ExponentialBuckets(1024, 2, 10),
		},
		[]string{"path"},
	)

	maxRetries int

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

	logLevel    int
	logFormat   string
	logLevelMap = map[string]int{
		"debug": LogLevelDebug,
		"info":  LogLevelInfo,
		"warn":  LogLevelWarn,
		"error": LogLevelError,
	}
	logLevelNames = map[int]string{
		LogLevelDebug: "DEBUG",
		LogLevelInfo:  "INFO",
		LogLevelWarn:  "WARN",
		LogLevelError: "ERROR",
	}

	Version   = "dev"
	GitCommit = "unknown"
	BuildTime = "unknown"
)

func init() {
	logOnce.Do(func() {
		go func() {
			for entry := range logQueue {
				if logFormat == "json" {
					jsonBytes, err := json.Marshal(entry)
					if err != nil {
						log.Printf("ERROR: Failed to marshal log entry: %v", err)
					} else {
						log.Print(string(jsonBytes))
					}
				} else {
					contextStr := ""
					if len(entry.Context) > 0 {
						parts := make([]string, 0, len(entry.Context))
						for k, v := range entry.Context {
							parts = append(parts, fmt.Sprintf("%s=%s", k, v))
						}
						contextStr = " " + strings.Join(parts, " ")
					}
					log.Printf("[%s]%s %s", entry.Level, contextStr, entry.Message)
				}
			}
		}()
	})

	logLevelStr := strings.ToLower(os.Getenv("LOG_LEVEL"))
	if level, ok := logLevelMap[logLevelStr]; ok {
		logLevel = level
	} else {
		logLevel = defaultLogLevel
	}

	logFormat = strings.ToLower(os.Getenv("LOG_FORMAT"))
	if logFormat != "json" && logFormat != "text" {
		logFormat = defaultLogFormat
	}

	sizeStr := os.Getenv("MAX_BODY_SIZE")
	if sizeStr == "" {
		maxBodySize = defaultMaxBodySize
	} else {
		size, err := humanize.ParseBytes(sizeStr)
		if err != nil {
			log.Printf("Invalid MAX_BODY_SIZE '%s', using default: %v", sizeStr, err)
			maxBodySize = defaultMaxBodySize
		} else {
			maxBodySize = int64(size)
		}
	}

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

	if clientTimeout > requestTimeout {
		log.Printf("CLIENT_TIMEOUT (%s) is greater than REQUEST_TIMEOUT (%s), adjusting CLIENT_TIMEOUT to %s",
			clientTimeout, requestTimeout, requestTimeout)
		clientTimeout = requestTimeout
	}

	httpClient = &http.Client{
		Timeout: clientTimeout,
	}

	sensitiveHeaders = make(map[string]bool)
	headersStr := os.Getenv("SENSITIVE_HEADERS")
	if headersStr == "" {
		headersStr = defaultSensitiveHeaders
	}
	for _, h := range strings.Split(headersStr, ",") {
		trimmed := strings.TrimSpace(h)
		if trimmed != "" {
			sensitiveHeaders[http.CanonicalHeaderKey(trimmed)] = true
		}
	}

	if path := os.Getenv("ENDPOINT_PATH"); path != "" {
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		endpointPath = path
	} else {
		endpointPath = defaultEndpointPath
	}

	metricsEnabled = strings.ToLower(os.Getenv("METRICS_ENABLED")) == "true"

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

	rand.Seed(time.Now().UnixNano())
}

func logDebug(format string, args ...interface{}) {
	logWithLevel(LogLevelDebug, nil, format, args...)
}

func logInfo(format string, args ...interface{}) {
	logWithLevel(LogLevelInfo, nil, format, args...)
}

func logWarn(format string, args ...interface{}) {
	logWithLevel(LogLevelWarn, nil, format, args...)
}

func logError(format string, args ...interface{}) {
	logWithLevel(LogLevelError, nil, format, args...)
}

func logDebugWithContext(ctx map[string]string, format string, args ...interface{}) {
	logWithLevel(LogLevelDebug, ctx, format, args...)
}

func logInfoWithContext(ctx map[string]string, format string, args ...interface{}) {
	logWithLevel(LogLevelInfo, ctx, format, args...)
}

func logWarnWithContext(ctx map[string]string, format string, args ...interface{}) {
	logWithLevel(LogLevelWarn, ctx, format, args...)
}

func logErrorWithContext(ctx map[string]string, format string, args ...interface{}) {
	logWithLevel(LogLevelError, ctx, format, args...)
}

func logWithLevel(level int, context map[string]string, format string, args ...interface{}) {
	if level < logLevel {
		return
	}

	entry := LogEntry{
		Time:    time.Now(),
		Level:   logLevelNames[level],
		Message: fmt.Sprintf(format, args...),
		Context: context,
	}

	select {
	case logQueue <- entry:
	default:
		if level >= LogLevelError {
			log.Printf("WARNING: Log queue full, logging ERROR directly: %s", entry.Message)
		}
	}
}

func cloneHeaders(original http.Header) http.Header {
	cloned := make(http.Header)
	for k, vv := range original {
		if sensitiveHeaders[http.CanonicalHeaderKey(k)] {
			logWarn("Sensitive header detected and propagated: %s", k)
		}
		cloned[k] = vv
	}
	return cloned
}

func echoHandler(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		logError("Error reading body: %v", err)
		http.Error(w, "Payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	defer r.Body.Close()

	echoData := map[string]interface{}{
		"headers": r.Header,
		"body":    string(bodyBytes),
	}

	if os.Getenv("ECHO_MODE_HEADER") == "true" {
		w.Header().Set("X-Echo-Mode", "active")
	}

	switch os.Getenv("ECHO_MODE_RESPONSE") {
	case "full":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(echoData)
	default:
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"status": "echoed"})
	}

	loggedBody := string(bodyBytes)
	if len(loggedBody) > maxLogPayload {
		loggedBody = loggedBody[:maxLogPayload] + "...[TRUNCATED]"
	}
	logDebugWithContext(
		map[string]string{
			"method": r.Method,
			"path":   r.URL.Path,
			"remote": r.RemoteAddr,
		},
		"Echo request received with body length %d: %s", len(bodyBytes), loggedBody)
}

func healthCheck(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
}

type Response struct {
	Target   string        `json:"target"`
	Status   int           `json:"status"`
	Body     string        `json:"body,omitempty"`
	Error    string        `json:"error,omitempty"`
	Latency  time.Duration `json:"latency"`
	Attempts int           `json:"attempts,omitempty"`
}

func multiplex(w http.ResponseWriter, r *http.Request) {
	if metricsEnabled {
		requestsTotal.WithLabelValues(r.URL.Path, r.Method).Inc()
		activeRequests.Inc()
		defer activeRequests.Dec()
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	if r.ContentLength > maxBodySize {
		logError("Request body size (%d) exceeds limit (%d)", r.ContentLength, maxBodySize)
		writeJSONError(w, "Payload too large", http.StatusRequestEntityTooLarge)
		return
	}

	getBody := r.GetBody

	var bodyBytes []byte
	var readErr error
	if r.ContentLength <= 0 {
		bodyReader, err := getBody()
		if err != nil {
			logError("Failed to get request body reader: %v", err)
			writeJSONError(w, "Failed to process request body", http.StatusInternalServerError)
			return
		}
		bodyBytes, readErr = io.ReadAll(io.LimitReader(bodyReader, maxBodySize+1))
		bodyReader.Close()

		if readErr != nil {
			logError("Error reading body for size check: %v", readErr)
			writeJSONError(w, "Failed to read request body", http.StatusBadRequest)
			return
		}
		if int64(len(bodyBytes)) > maxBodySize {
			logError("Request body size exceeds limit (%d bytes read)", len(bodyBytes))
			writeJSONError(w, "Payload too large", http.StatusRequestEntityTooLarge)
			return
		}
	} else {
		if metricsEnabled {
			bodySize.WithLabelValues(r.URL.Path).Observe(float64(r.ContentLength))
		}
	}

	targetsEnv := os.Getenv("TARGETS")
	targets := strings.Split(targetsEnv, ",")
	if len(targets) == 0 || (len(targets) == 1 && targets[0] == "") {
		logError("No targets configured (TARGETS env var is empty or not set)")
		writeJSONError(w, "No targets configured", http.StatusServiceUnavailable)
		return
	}

	responses := make([]Response, 0, len(targets))
	var wg sync.WaitGroup
	respChan := make(chan Response, len(targets))

	for _, target := range targets {
		targetURL := strings.TrimSpace(target)
		if targetURL == "" {
			logWarn("Skipping empty target URL found in TARGETS list")
			continue
		}
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			start := time.Now()
			resp := sendRequest(ctx, httpClient, target, r, getBody, bodyBytes)
			resp.Latency = time.Since(start)
			respChan <- resp
		}(targetURL)
	}

	go func() {
		wg.Wait()
		close(respChan)
	}()

	for resp := range respChan {
		responses = append(responses, resp)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := writeJSON(w, responses); err != nil {
		return
	}
}

func sendRequest(ctx context.Context, client *http.Client, target string, originalReq *http.Request, getBody func() (io.ReadCloser, error), preReadBody []byte) Response {
	resp := Response{Target: target}

	startTime := time.Now()
	defer func() {
		if metricsEnabled {
			duration := time.Since(startTime).Seconds()
			requestDuration.WithLabelValues(target).Observe(duration)

			status := strconv.Itoa(resp.Status)
			if resp.Status == 0 && resp.Error != "" {
				status = "error"
			}
			targetRequestsTotal.WithLabelValues(target, status).Inc()
		}
	}()

	var err error
	var response *http.Response
	attempts := 0
	backoff := initialRetryBackoff

	for attempts <= maxRetries {
		var bodyReader io.ReadCloser
		if len(preReadBody) > 0 && attempts == 0 {
			bodyReader = io.NopCloser(bytes.NewReader(preReadBody))
		} else {
			var getBodyErr error
			bodyReader, getBodyErr = getBody()
			if getBodyErr != nil {
				resp.Status = http.StatusInternalServerError
				resp.Error = fmt.Sprintf("Failed to get request body for attempt %d: %v", attempts+1, getBodyErr)
				logErrorWithContext(map[string]string{"target": target}, resp.Error)
				return resp
			}
		}

		req, err := http.NewRequestWithContext(ctx, originalReq.Method, target, bodyReader)
		if err != nil {
			resp.Status = http.StatusInternalServerError
			resp.Error = fmt.Sprintf("Failed to create request: %v", err)
			bodyReader.Close()
			logErrorWithContext(map[string]string{"target": target}, resp.Error)
			return resp
		}

		req.Header = cloneHeaders(originalReq.Header)
		if originalReq.ContentLength > 0 {
			req.ContentLength = originalReq.ContentLength
		} else if len(preReadBody) > 0 {
			req.ContentLength = int64(len(preReadBody))
		}

		if attempts > 0 {
			req.Header.Set("X-Retry-Count", strconv.Itoa(attempts))
		}

		response, err = client.Do(req)

		if err != nil {
			if isRetryableError(err) && attempts < maxRetries {
				logWarnWithContext(map[string]string{"target": target, "error": err.Error()},
					"Retryable error on attempt %d", attempts+1)
				attempts++
				continue
			}

			resp.Status = http.StatusServiceUnavailable
			resp.Error = fmt.Sprintf("Request failed after %d attempts: %v", attempts+1, err)
			logErrorWithContext(map[string]string{"target": target}, resp.Error)
			return resp
		}

		if response.StatusCode >= 500 && attempts < maxRetries {
			logWarnWithContext(map[string]string{"target": target, "status": strconv.Itoa(response.StatusCode)},
				"Retryable status code %d on attempt %d", response.StatusCode, attempts+1)
			response.Body.Close()
			attempts++
			continue
		}

		break
	}

	if response == nil {
		resp.Status = http.StatusServiceUnavailable
		resp.Error = fmt.Sprintf("Request failed after %d attempts (no response received)", attempts)
		logErrorWithContext(map[string]string{"target": target}, resp.Error)
		return resp
	}
	defer response.Body.Close()

	respBody, readErr := io.ReadAll(io.LimitReader(response.Body, maxBodySize))
	if readErr != nil {
		resp.Status = http.StatusInternalServerError
		resp.Error = fmt.Sprintf("Failed to read response body: %v", readErr)
		logErrorWithContext(map[string]string{"target": target, "status": strconv.Itoa(response.StatusCode)}, resp.Error)
		if response.StatusCode != 0 {
			resp.Status = response.StatusCode
		}
		return resp
	}

	resp.Status = response.StatusCode
	resp.Body = string(respBody)
	resp.Attempts = attempts + 1

	logDebugWithContext(
		map[string]string{
			"target":   target,
			"status":   strconv.Itoa(resp.Status),
			"attempts": strconv.Itoa(resp.Attempts),
			"latency":  resp.Latency.String(),
		},
		"Request completed successfully",
	)

	return resp
}

func isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	errMsg := strings.ToLower(err.Error())

	if strings.Contains(errMsg, "connection refused") ||
		strings.Contains(errMsg, "timeout") ||
		strings.Contains(errMsg, "deadline exceeded") ||
		strings.Contains(errMsg, "connection reset") ||
		strings.Contains(errMsg, "no such host") {
		return true
	}

	return false
}

func addJitter(d time.Duration) time.Duration {
	jitter := float64(d) * (0.8 + 0.4*rand.Float64())
	return time.Duration(jitter)
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func writeJSON(w http.ResponseWriter, v interface{}) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		logError("Error encoding JSON: %v", err)
		return err
	}
	return nil
}

func writeJSONError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(w, map[string]string{"error": message})
}

func versionHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string]string{
		"version":    Version,
		"git_commit": GitCommit,
		"build_time": BuildTime,
	})
}

func main() {
	log.SetOutput(os.Stdout)

	logInfo("Starting FanOut service version %s (commit: %s, built: %s)", Version, GitCommit, BuildTime)
	logInfo("Log level: '%s', Log format: '%s'", logLevelNames[logLevel], logFormat)

	targets := os.Getenv("TARGETS")
	if targets == "localonly" {
		http.HandleFunc(endpointPath, echoHandler)
		logInfo("Running in ECHO MODE (Endpoint: %s)", endpointPath)
	} else {
		targetList := strings.Split(targets, ",")
		validTargets := 0
		for _, t := range targetList {
			if strings.TrimSpace(t) != "" {
				validTargets++
			}
		}
		if validTargets == 0 {
			logError("FATAL: No valid target URLs configured in TARGETS environment variable.")
			os.Exit(1)
		}
		http.HandleFunc(endpointPath, multiplex)
		logInfo("Running in MULTIPLEX MODE (Endpoint: %s) with %d targets", endpointPath, validTargets)
	}

	http.HandleFunc("/health", healthCheck)
	logInfo("Health check endpoint enabled at /health")

	if metricsEnabled {
		http.Handle("/metrics", promhttp.Handler())
		logInfo("Metrics endpoint enabled at /metrics")
	}

	http.HandleFunc("/version", versionHandler)
	logInfo("Version endpoint enabled at /version")

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	if len(os.Args) > 1 && os.Args[1] == "-version" {
		fmt.Printf("FanOut %s (commit: %s, built: %s)\n", Version, GitCommit, BuildTime)
		os.Exit(0)
	}

	logInfo("Server starting on :%s (Max body: %s, Request Timeout: %s, Client Timeout: %s, Max Retries: %d)",
		port,
		humanize.Bytes(uint64(maxBodySize)),
		requestTimeout,
		clientTimeout,
		maxRetries)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
