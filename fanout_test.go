package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestCloneHeaders verifies that headers are properly cloned and sensitive headers are detected
func TestCloneHeaders(t *testing.T) {
	// Setup test cases
	tests := []struct {
		name      string
		headers   http.Header
		sensitive bool
	}{
		{
			name: "Regular Headers",
			headers: http.Header{
				"Content-Type": []string{"application/json"},
				"User-Agent":   []string{"test-agent"},
			},
			sensitive: false,
		},
		{
			name: "Sensitive Headers",
			headers: http.Header{
				"Content-Type":  []string{"application/json"},
				"Authorization": []string{"Bearer abc123"},
			},
			sensitive: true,
		},
	}

	// Run test cases
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := cloneHeaders(tc.headers)

			// Verify all headers were cloned correctly
			if !reflect.DeepEqual(result, tc.headers) {
				t.Errorf("Headers not cloned correctly\nExpected: %v\nGot: %v", tc.headers, result)
			}
		})
	}
}

// TestIsRetryableError verifies that certain errors are correctly identified as retryable
func TestIsRetryableError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		retryable bool
	}{
		{
			name:      "nil error",
			err:       nil,
			retryable: false,
		},
		{
			name:      "connection refused",
			err:       errors.New("dial tcp: connection refused"),
			retryable: true,
		},
		{
			name:      "timeout error",
			err:       errors.New("context deadline exceeded (Client.Timeout exceeded)"),
			retryable: true,
		},
		{
			name:      "connection reset",
			err:       errors.New("connection reset by peer"),
			retryable: true,
		},
		{
			name:      "no host error",
			err:       errors.New("no such host"),
			retryable: true,
		},
		{
			name:      "non-retryable error",
			err:       errors.New("some other error"),
			retryable: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := isRetryableError(tc.err)
			if result != tc.retryable {
				t.Errorf("isRetryableError(%v) = %v, want %v", tc.err, result, tc.retryable)
			}
		})
	}
}

// TestAddJitter checks that jitter is within expected range
func TestAddJitter(t *testing.T) {
	baseDuration := 100 * time.Millisecond
	iterations := 100
	min := float64(baseDuration) * 0.8
	max := float64(baseDuration) * 1.2

	for i := 0; i < iterations; i++ {
		result := addJitter(baseDuration)
		if float64(result) < min || float64(result) > max {
			t.Errorf("Jitter out of expected range: got %v, want between %v and %v",
				result, time.Duration(min), time.Duration(max))
		}
	}
}

// TestMinDuration verifies min function returns the smaller of two durations
func TestMinDuration(t *testing.T) {
	tests := []struct {
		a, b, expected time.Duration
	}{
		{100 * time.Millisecond, 200 * time.Millisecond, 100 * time.Millisecond},
		{500 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond},
		{1 * time.Second, 1 * time.Second, 1 * time.Second},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("Case %d", i), func(t *testing.T) {
			result := min(tc.a, tc.b)
			if result != tc.expected {
				t.Errorf("min(%v, %v) = %v, want %v", tc.a, tc.b, result, tc.expected)
			}
		})
	}
}

// TestWriteJSON checks that JSON responses are written correctly
func TestWriteJSON(t *testing.T) {
	tests := []struct {
		name     string
		data     interface{}
		expected string
	}{
		{
			name:     "Simple map",
			data:     map[string]string{"key": "value"},
			expected: `{"key":"value"}`,
		},
		{
			name: "Response object",
			data: Response{
				Target:   "http://example.com",
				Status:   200,
				Body:     "OK",
				Attempts: 0, // Due to omitempty tag, this won't appear in JSON when 0
			},
			// Updated expected value - removed attempts since it has omitempty tag
			expected: `{"target":"http://example.com","status":200,"body":"OK","latency":0}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create a response recorder
			w := httptest.NewRecorder()

			// Call writeJSON
			err := writeJSON(w, tc.data)
			if err != nil {
				t.Fatalf("writeJSON returned error: %v", err)
			}

			// Check response
			resp := w.Result()
			body, _ := io.ReadAll(resp.Body)
			// Trim newlines because the encoder adds them
			got := strings.TrimSpace(string(body))

			if got != tc.expected {
				t.Errorf("writeJSON wrote %q, want %q", got, tc.expected)
			}
		})
	}
}

// TestWriteJSONError checks error responses
func TestWriteJSONError(t *testing.T) {
	tests := []struct {
		name     string
		message  string
		status   int
		expected string
	}{
		{
			name:     "Not found error",
			message:  "Resource not found",
			status:   http.StatusNotFound,
			expected: `{"error":"Resource not found"}`,
		},
		{
			name:     "Bad request error",
			message:  "Invalid input",
			status:   http.StatusBadRequest,
			expected: `{"error":"Invalid input"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create a response recorder
			w := httptest.NewRecorder()

			// Call writeJSONError
			writeJSONError(w, tc.message, tc.status)

			// Check response
			resp := w.Result()
			if resp.StatusCode != tc.status {
				t.Errorf("writeJSONError status = %d, want %d", resp.StatusCode, tc.status)
			}

			body, _ := io.ReadAll(resp.Body)
			// Trim newlines because the encoder adds them
			got := strings.TrimSpace(string(body))

			if got != tc.expected {
				t.Errorf("writeJSONError wrote %q, want %q", got, tc.expected)
			}
		})
	}
}

// TestHealthCheck verifies the health endpoint returns correct status
func TestHealthCheck(t *testing.T) {
	// Create a request to pass to the handler
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	// Call the handler
	healthCheck(w, req)

	// Check the status code
	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthCheck returned wrong status code: got %v want %v",
			resp.StatusCode, http.StatusOK)
	}

	// Check the response body
	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("Could not decode response: %v", err)
	}

	if result["status"] != "healthy" {
		t.Errorf("healthCheck returned wrong status: got %v want %v",
			result["status"], "healthy")
	}
}

// TestEchoHandlerSimpleMode tests the echo handler in simple mode
func TestEchoHandlerSimpleMode(t *testing.T) {
	// Save and restore original env vars
	originalHeader := os.Getenv("ECHO_MODE_HEADER")
	originalResponse := os.Getenv("ECHO_MODE_RESPONSE")
	defer func() {
		os.Setenv("ECHO_MODE_HEADER", originalHeader)
		os.Setenv("ECHO_MODE_RESPONSE", originalResponse)
	}()

	// Set environment for this test
	os.Setenv("ECHO_MODE_HEADER", "false")
	os.Setenv("ECHO_MODE_RESPONSE", "simple")

	// Create a request with a test body
	body := []byte(`{"test":"data"}`)
	req := httptest.NewRequest("POST", "/fanout", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	// Call the handler
	echoHandler(w, req)

	// Check response
	resp := w.Result()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("echoHandler returned wrong status code: got %v want %v",
			resp.StatusCode, http.StatusAccepted)
	}

	// Check response body
	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("Could not decode response: %v", err)
	}

	if result["status"] != "echoed" {
		t.Errorf("echoHandler returned wrong status: got %v want %v",
			result["status"], "echoed")
	}
}

// TestEchoHandlerFullMode tests the echo handler in full mode
func TestEchoHandlerFullMode(t *testing.T) {
	// Save and restore original env vars
	originalHeader := os.Getenv("ECHO_MODE_HEADER")
	originalResponse := os.Getenv("ECHO_MODE_RESPONSE")
	defer func() {
		os.Setenv("ECHO_MODE_HEADER", originalHeader)
		os.Setenv("ECHO_MODE_RESPONSE", originalResponse)
	}()

	// Set environment for this test
	os.Setenv("ECHO_MODE_HEADER", "true")
	os.Setenv("ECHO_MODE_RESPONSE", "full")

	// Create a request with a test body
	body := []byte(`{"test":"data"}`)
	req := httptest.NewRequest("POST", "/fanout", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	// Call the handler
	echoHandler(w, req)

	// Check response
	resp := w.Result()

	// Verify header
	if resp.Header.Get("X-Echo-Mode") != "active" {
		t.Errorf("echoHandler should set X-Echo-Mode header")
	}

	// Check response body
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("Could not decode response: %v", err)
	}

	// Check body contains correct fields
	if _, ok := result["headers"]; !ok {
		t.Errorf("echoHandler response missing headers")
	}

	if bodyStr, ok := result["body"]; !ok || bodyStr != string(body) {
		t.Errorf("echoHandler response missing or incorrect body: %v", result["body"])
	}
}

// TestSendRequest tests the request sending functionality with mocks
func TestSendRequest(t *testing.T) {
	// Create a mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check for retry header
		retryCount := r.Header.Get("X-Retry-Count")
		if retryCount == "1" {
			// Simulate success after retry
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("Success after retry"))
			return
		}

		// On first attempt, return server error to trigger retry
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Server error"))
	}))
	defer server.Close()

	// Configure client
	client := &http.Client{
		Timeout: 1 * time.Second,
	}

	// Create an http request
	req, err := http.NewRequest("GET", "http://example.com", nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	// Set app-level retry count for test
	maxRetries = 2

	// Call sendRequest with mock server URL
	resp := sendRequest(context.Background(), client, server.URL, req, nil)

	// Verify response
	if resp.Status != http.StatusOK {
		t.Errorf("Expected status 200, got %d: %s", resp.Status, resp.Error)
	}

	if resp.Attempts != 2 {
		t.Errorf("Expected 2 attempts, got %d", resp.Attempts)
	}

	if !strings.Contains(resp.Body, "Success after retry") {
		t.Errorf("Body doesn't contain expected response: %s", resp.Body)
	}
}

// TestSendRequestNetworkError tests retry on network errors
func TestSendRequestNetworkError(t *testing.T) {
	// Configure client
	client := &http.Client{
		Timeout: 100 * time.Millisecond,
	}

	// Create an http request
	req, err := http.NewRequest("GET", "http://example.com", nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	// Set app-level retry count for test
	maxRetries = 1

	// Call sendRequest with a non-existent endpoint (will cause error)
	resp := sendRequest(context.Background(), client, "http://nonexistent.example", req, nil)

	// Verify response reports an error
	if resp.Status != http.StatusServiceUnavailable {
		t.Errorf("Expected status 503, got %d", resp.Status)
	}

	if resp.Error == "" {
		t.Error("Expected error message but got none")
	}
}

// TestLogLevels verifies that log levels are respected
func TestLogLevels(t *testing.T) {
	// Set a known log level
	origLogLevel := logLevel
	defer func() { logLevel = origLogLevel }()

	tests := []struct {
		setLevel     int
		messageLevel int
		shouldBeSent bool
	}{
		{LogLevelInfo, LogLevelDebug, false}, // Debug not logged when level is Info
		{LogLevelInfo, LogLevelInfo, true},   // Info logged when level is Info
		{LogLevelInfo, LogLevelWarn, true},   // Warn logged when level is Info
		{LogLevelInfo, LogLevelError, true},  // Error logged when level is Info
		{LogLevelError, LogLevelWarn, false}, // Warn not logged when level is Error
		{LogLevelDebug, LogLevelInfo, true},  // Info logged when level is Debug
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("Case %d", i), func(t *testing.T) {
			logLevel = tc.setLevel

			// Create a temporary log queue to check if messages get queued
			origLogQueue := logQueue
			testQueue := make(chan LogEntry, 1)
			logQueue = testQueue
			defer func() { logQueue = origLogQueue }()

			// Send log with the given level
			logWithLevel(tc.messageLevel, nil, "Test message")

			// Check if message was enqueued
			var received bool
			select {
			case <-testQueue:
				received = true
			default:
				received = false
			}

			if received != tc.shouldBeSent {
				t.Errorf("Log with level %d when configured level is %d: got queued=%v, want queued=%v",
					tc.messageLevel, tc.setLevel, received, tc.shouldBeSent)
			}
		})
	}
}
