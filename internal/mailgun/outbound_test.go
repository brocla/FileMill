package mailgun

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestThreadingIDSuppressesSyntheticKey(t *testing.T) {
	if got := threadingID("mailgun:abc123"); got != "" {
		t.Fatalf("synthetic key must not thread; got %q", got)
	}
	if got := threadingID("<real@example.com>"); got != "<real@example.com>" {
		t.Fatalf("real Message-Id must thread; got %q", got)
	}
	if got := threadingID(""); got != "" {
		t.Fatalf("empty stays empty; got %q", got)
	}
}

func sendTestService(baseURL string, client *http.Client) *Service {
	return &Service{
		from:     "filemill@mill.test",
		domain:   "mill.test",
		apiKey:   "key",
		sendBase: baseURL,
		client:   client,
		log:      discardLogger(),
	}
}

// A hung Mailgun connection must not block the delivery loop forever — the
// send timeout turns it into an error the loop can log and retry.
func TestSendTimesOut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	s := sendTestService(server.URL, &http.Client{Timeout: 20 * time.Millisecond})
	if err := s.send(context.Background(), "to@x", "subj", "", "body", nil); err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
}

// A cancelled context (e.g. shutdown) must abort an in-flight send.
func TestSendHonorsCancelledContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := sendTestService(server.URL, &http.Client{Timeout: time.Second})
	if err := s.send(ctx, "to@x", "subj", "", "body", nil); err == nil {
		t.Fatal("expected a cancellation error, got nil")
	}
}

// A non-2xx from Mailgun must surface its response body, which carries the real
// reason a bare status code hides.
func TestSendIncludesMailgunErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"domain not verified"}`))
	}))
	defer server.Close()

	s := sendTestService(server.URL, &http.Client{Timeout: time.Second})
	err := s.send(context.Background(), "to@x", "subj", "", "body", nil)
	if err == nil {
		t.Fatal("expected an error for a 400, got nil")
	}
	if !strings.Contains(err.Error(), "domain not verified") {
		t.Fatalf("error should include Mailgun's body; got: %v", err)
	}
}
