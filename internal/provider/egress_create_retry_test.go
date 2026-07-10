package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

func TestCreateEgressWithRetryRecoversFromServiceUnavailable(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/environments/env-1/egress" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"detail":"gateway intent is not ready yet"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"egress-1","status":"provisioning"}`))
	}))
	defer srv.Close()

	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	egress, err := createEgressWithRetry(
		context.Background(),
		c,
		"env-1",
		client.EgressSpec{ClusterID: "cluster-1", DestinationCIDR: "1.1.1.1/32"},
		time.Millisecond,
		time.Second,
	)
	if err != nil {
		t.Fatalf("createEgressWithRetry: %v", err)
	}
	if egress.ID != "egress-1" {
		t.Fatalf("egress id = %q, want egress-1", egress.ID)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("POST calls = %d, want 3", got)
	}
}

func TestCreateEgressWithRetryDoesNotRetryPermanentAPIError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail":"invalid destination"}`))
	}))
	defer srv.Close()

	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	_, err = createEgressWithRetry(
		context.Background(), c, "env-1", client.EgressSpec{}, time.Millisecond, time.Second,
	)
	if err == nil || !strings.Contains(err.Error(), "HTTP 422") {
		t.Fatalf("expected HTTP 422, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("POST calls = %d, want 1", got)
	}
}

func TestCreateEgressWithRetryHonorsTimeout(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"detail":"gateway intent is not ready yet"}`))
	}))
	defer srv.Close()

	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	_, err = createEgressWithRetry(
		context.Background(), c, "env-1", client.EgressSpec{}, 2*time.Millisecond, 25*time.Millisecond,
	)
	if err == nil || !strings.Contains(err.Error(), "timed out after 25ms") {
		t.Fatalf("expected bounded timeout, got %v", err)
	}
	if !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("timeout must retain the last HTTP 503, got %v", err)
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("POST calls = %d, want at least 2", got)
	}
}

func TestCreateEgressWithRetryHonorsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, `{"detail":"gateway intent is not ready yet"}`)
	}))
	defer srv.Close()

	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = createEgressWithRetry(ctx, c, "env-1", client.EgressSpec{}, time.Second, time.Minute)
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}

func TestCreateEgressWithRetryHonorsCancellationBetweenAttempts(t *testing.T) {
	firstResponse := make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, `{"detail":"gateway intent is not ready yet"}`)
		select {
		case <-firstResponse:
		default:
			close(firstResponse)
		}
	}))
	defer srv.Close()

	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-firstResponse
		// Let the client decode the first HTTP 503 so cancellation happens in
		// the retry wait, not while the response body is still in flight.
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err = createEgressWithRetry(ctx, c, "env-1", client.EgressSpec{}, time.Minute, time.Hour)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation between attempts, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("POST calls = %d, want 1", got)
	}
}
