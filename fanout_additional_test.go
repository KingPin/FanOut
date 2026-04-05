package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMultiplexNoGetBody ensures multiplex can handle requests without GetBody
func TestMultiplexNoGetBody(t *testing.T) {
	// Start a mock target server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer server.Close()

	// Set configuredTargets directly (TARGETS env var is cached at init time, not per-request)
	origTargets := configuredTargets
	defer func() { configuredTargets = origTargets }()
	configuredTargets = []string{server.URL}

	// http.NewRequest sets GetBody automatically for *bytes.Reader / *bytes.Buffer.
	// Wrap in io.NopCloser so the body is a plain io.ReadCloser and GetBody stays nil,
	// which forces multiplex into the pre-read code path.
	req, err := http.NewRequest("POST", "/fanout", io.NopCloser(strings.NewReader("hello")))
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "text/plain")

	w := httptest.NewRecorder()
	// Call multiplex handler directly
	multiplex(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200 OK from multiplex, got %d", resp.StatusCode)
	}
	var results []Response
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		t.Fatalf("Failed to decode multiplex response: %v", err)
	}
	if len(results) == 0 || results[0].Status != http.StatusOK {
		t.Fatalf("Unexpected target response: %v", results)
	}
}

// fakeNetErr implements net.Error for testing
type fakeNetErr struct{ msg string }

func (f fakeNetErr) Error() string   { return f.msg }
func (f fakeNetErr) Timeout() bool   { return true }
func (f fakeNetErr) Temporary() bool { return false }

func TestIsRetryableError_NetError(t *testing.T) {
	err := fakeNetErr{"timeout"}
	if !isRetryableError(err) {
		t.Errorf("expected fakeNetErr to be retryable")
	}
}
