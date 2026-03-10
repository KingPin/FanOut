package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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

	// Set TARGETS to the mock server
	origTargets := os.Getenv("TARGETS")
	defer os.Setenv("TARGETS", origTargets)
	os.Setenv("TARGETS", server.URL)

	// Create a request WITHOUT GetBody (http.NewRequest leaves GetBody nil)
	body := []byte("hello")
	req, err := http.NewRequest("POST", "/fanout", bytes.NewReader(body))
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
