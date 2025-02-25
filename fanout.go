// Package main implements a HTTP request fan-out service that can either:
// 1. Echo back the incoming requests (when TARGETS=localonly)
// 2. Fan out/multiplex requests to multiple configured endpoints
package main

import (
    "bytes"
    "encoding/json"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "strings"
    "sync"
    "time"
    "context"

    "github.com/dustin/go-humanize" // Used for human-readable size parsing
)

const (
    defaultMaxBodySize = 10 * 1024 * 1024 // 10MB - Default maximum request body size
    logQueueSize       = 10000            // Size of the async logging buffer queue
    maxLogPayload      = 1024             // Maximum payload size to log before truncation

    // Configuration for timeouts and retries
    requestTimeout = 30 * time.Second
    clientTimeout  = 10 * time.Second
)

var (
    // Async logging setup - Provides non-blocking log operations
    logQueue   = make(chan string, logQueueSize)
    logOnce    sync.Once // Ensures logging goroutine is initialized only once
    maxBodySize int64    // Maximum body size for incoming requests

    // Header monitoring - Headers that should trigger warnings when detected
    sensitiveHeaders = map[string]bool{
        "Authorization": true, // Authentication credentials
        "Cookie":        true, // Session information
    }
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
    Target string          `json:"target"`
    Status int             `json:"status"`
    Body   string         `json:"body,omitempty"`
    Error  string         `json:"error,omitempty"`
    Latency time.Duration `json:"latency"`
}

// multiplex fans out the incoming request to all configured targets
func multiplex(w http.ResponseWriter, r *http.Request) {
    // Create context with timeout
    ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
    defer cancel()

    // Read and validate the request body
    bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
    if err != nil {
        logAsync("ERROR reading body: %v", err)
        writeJSONError(w, "Failed to read request body", http.StatusBadRequest)
        return
    }
    defer r.Body.Close()

    // Parse targets from environment
    targets := strings.Split(os.Getenv("TARGETS"), ",")
    if len(targets) == 0 || (len(targets) == 1 && targets[0] == "") {
        writeJSONError(w, "No targets configured", http.StatusServiceUnavailable)
        return
    }

    // Create HTTP client with timeout
    client := &http.Client{
        Timeout: clientTimeout,
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

    // Create new request
    req, err := http.NewRequestWithContext(ctx, originalReq.Method, target, bytes.NewReader(body))
    if err != nil {
        resp.Status = http.StatusInternalServerError
        resp.Error = fmt.Sprintf("Failed to create request: %v", err)
        return resp
    }

    // Clone headers
    req.Header = cloneHeaders(originalReq.Header)

    // Send request
    response, err := client.Do(req)
    if err != nil {
        resp.Status = http.StatusServiceUnavailable
        resp.Error = fmt.Sprintf("Request failed: %v", err)
        return resp
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
    return resp
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
    if targets == "localonly" {
        http.HandleFunc("/fanout", echoHandler)
        log.Print("Running in ECHO MODE")
    } else {
        http.HandleFunc("/fanout", multiplex) // multiplex function handles fan-out to multiple targets
    }

    // Add health check route
    http.HandleFunc("/health", healthCheck)

    // Determine port from environment or use default
    port := os.Getenv("PORT")
    if port == "" {
        port = "8080" // Default port
    }

    // // Health check mode
    // if os.Args[1] == "-healthcheck" {
    //     resp, err := http.Get(fmt.Sprintf("http://localhost:%s/health", port))
    //     if err != nil || resp.StatusCode != http.StatusOK {
    //         os.Exit(1)
    //     }
    //     os.Exit(0)
    // }

		// Health check mode - check args length first
		if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
			resp, err := http.Get(fmt.Sprintf("http://localhost:%s/health", port))
			if err != nil || resp.StatusCode != http.StatusOK {
					os.Exit(1)
			}
			os.Exit(0)
		}

    log.Printf("Server starting on :%s (Max body: %s)", port, humanize.Bytes(uint64(maxBodySize)))
    log.Fatal(http.ListenAndServe(":"+port, nil))
}
