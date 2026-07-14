package provider

// Unit tests for the poll-to-ready / poll-to-gone logic with short test
// timeouts — success, error-status, timeout and cancellation paths.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
	"github.com/focusnetcloud/terraform-provider-fcs/internal/mockapi"
)

const waitTestInterval = 5 * time.Millisecond

func newWaitFixture(t *testing.T, srv *mockapi.Server, kind string) (*client.Client, string, string) {
	t.Helper()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	env, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: "lab-wait-" + kind})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	cl, err := c.CreateCluster(context.Background(), env.ID, client.ClusterSpec{Kind: kind})
	if err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	return c, env.ID, cl.ID
}

func TestWaitForClusterActiveSuccess(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 3 // forces at least two poll sleeps
	c, envID, clusterID := newWaitFixture(t, srv, "business")

	cl, err := waitForClusterActive(context.Background(), c, envID, clusterID, waitTestInterval, 5*time.Second)
	if err != nil {
		t.Fatalf("waitForClusterActive: %v", err)
	}
	if cl.Status != "active" {
		t.Fatalf("expected active, got %q", cl.Status)
	}
}

func TestClusterMatchesResizeUsesConfiguredDedicatedKind(t *testing.T) {
	target := client.ClusterSizing{
		CPNodes: 3, CPVcpu: 4, CPRamGB: 8,
		WorkerNodes: 0, WorkerVcpu: 0, WorkerRamGB: 0, PVCStorageGB: 100,
	}
	cluster := &client.Cluster{
		Kind: "", CPNodes: 3, CPVcpu: 4, CPRamGB: 8,
		WorkerNodes: 0, WorkerVcpu: 0, WorkerRamGB: 0, PVCStorageGB: 100,
	}
	if !clusterMatchesResize(cluster, target, true) {
		t.Fatal("dedicated convergence must not depend on kind being present in the API response")
	}
	cluster.CPVcpu = 2
	if clusterMatchesResize(cluster, target, true) {
		t.Fatal("stale dedicated sizing must not be accepted as converged")
	}
}

func TestWaitForClusterResizedRejectsStaleActiveSizing(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		request := requests.Add(1)
		vcpu := 4
		if request > 1 {
			vcpu = 8
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"cluster-race","kind":"business","status":"active","vcpu":%d,"ram_gb":16,"storage_gb":250}`, vcpu)
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	cluster, err := waitForClusterResized(
		context.Background(), c, "env-race", "cluster-race",
		client.ClusterSizing{VCPU: 8, RAMGB: 16, StorageGB: 250}, false,
		waitTestInterval, time.Second,
	)
	if err != nil {
		t.Fatalf("waitForClusterResized: %v", err)
	}
	if cluster.VCPU != 8 || requests.Load() != 2 {
		t.Fatalf("wait must reject stale active sizing; cluster=%+v requests=%d", cluster, requests.Load())
	}
}

func TestWaitForClusterActiveRetriesPerRequestTimeout(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			time.Sleep(150 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cluster-timeout","kind":"flex","status":"active"}`))
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, unitToken, client.WithHTTPClient(&http.Client{Timeout: 50 * time.Millisecond}))
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	cl, err := waitForClusterActive(context.Background(), c, "env-timeout", "cluster-timeout", waitTestInterval, time.Second)
	if err != nil {
		t.Fatalf("transient per-request timeout must be retried: %v", err)
	}
	if cl.Status != "active" {
		t.Fatalf("expected active, got %q", cl.Status)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("expected exactly 2 GET requests, got %d", got)
	}
}

func TestWaitForClusterActiveTimeoutIncludesCurrentGETError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cluster-slow","kind":"flex","status":"active"}`))
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	_, err = waitForClusterActive(context.Background(), c, "env-slow", "cluster-slow", waitTestInterval, 20*time.Millisecond)
	if err == nil {
		t.Fatal("expected overall wait timeout")
	}
	if !containsAll(err.Error(), "last transient GET error", "context deadline exceeded") {
		t.Fatalf("timeout must preserve the GET error that observed the deadline, got: %v", err)
	}
}

func TestWaitForClusterActiveRetriesTransientHTTPStatus(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, `{"detail":"origin temporarily unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cluster-503","kind":"business","status":"active"}`))
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	if _, err := waitForClusterActive(context.Background(), c, "env-503", "cluster-503", waitTestInterval, time.Second); err != nil {
		t.Fatalf("transient HTTP 503 must be retried: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("expected exactly 2 GET requests, got %d", got)
	}
}

func TestWaitForClusterActiveDoesNotRetryPermanentHTTPStatus(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, `{"detail":"invalid token"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	_, err = waitForClusterActive(context.Background(), c, "env-401", "cluster-401", waitTestInterval, time.Second)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("expected permanent HTTP 401 error, got %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("permanent HTTP 401 must not be retried, got %d requests", got)
	}
}

func TestWaitForClusterActiveErrorStatus(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 2
	srv.FailClusterProvisioning = true
	c, envID, clusterID := newWaitFixture(t, srv, "flex")

	cl, err := waitForClusterActive(context.Background(), c, envID, clusterID, waitTestInterval, 5*time.Second)
	if err == nil {
		t.Fatal("expected error for status=error")
	}
	if !strings.Contains(err.Error(), `"error"`) {
		t.Fatalf("error must name the error status, got: %v", err)
	}
	if cl == nil || cl.Status != "error" {
		t.Fatalf("expected last cluster with status error, got %+v", cl)
	}
}

func TestWaitForClusterActiveTimeout(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 1 << 30 // never becomes active
	c, envID, clusterID := newWaitFixture(t, srv, "business")

	start := time.Now()
	_, err := waitForClusterActive(context.Background(), c, envID, clusterID, waitTestInterval, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), `"provisioning"`) {
		t.Fatalf("timeout error must mention timeout and last status, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout must be honored, took %s", elapsed)
	}
}

func TestWaitForClusterActiveContextCancel(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 1 << 30
	c, envID, clusterID := newWaitFixture(t, srv, "namespace")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := waitForClusterActive(ctx, c, envID, clusterID, waitTestInterval, time.Minute)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("expected context cancellation to surface, got: %v", err)
	}
}

func TestWaitForClusterGoneSuccess(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.ClusterGoneAfterGETs = 2 // destroyed readable — gone on the first GET
	c, envID, clusterID := newWaitFixture(t, srv, "business")

	if err := c.DeleteCluster(context.Background(), envID, clusterID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := waitForClusterGone(context.Background(), c, envID, clusterID, waitTestInterval, 5*time.Second); err != nil {
		t.Fatalf("waitForClusterGone: %v", err)
	}
	if srv.ClusterCount() != 0 {
		t.Fatalf("expected 0 live clusters, got %d", srv.ClusterCount())
	}
}

func TestWaitForClusterGoneRetriesTransientHTTPStatus(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, `{"detail":"gateway timeout"}`, http.StatusGatewayTimeout)
			return
		}
		http.Error(w, `{"detail":"gone"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	if err := waitForClusterGone(context.Background(), c, "env-504", "cluster-504", waitTestInterval, time.Second); err != nil {
		t.Fatalf("transient HTTP 504 must be retried before accepting 404: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("expected exactly 2 GET requests, got %d", got)
	}
}

// TestWaitForClusterGoneTimeout: the contract mock flips to destroyed (a
// gone signal) right after DELETE, so a *stuck* teardown — GET keeps
// returning a non-terminal status — is modeled with a static handler.
func TestWaitForClusterGoneTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"stuck","kind":"flex","status":"offboarding"}`))
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	err = waitForClusterGone(context.Background(), c, "env-stuck", "stuck", waitTestInterval, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), `"offboarding"`) {
		t.Fatalf("timeout error must mention timeout and last status, got: %v", err)
	}
}

func TestJitterStaysAroundInterval(t *testing.T) {
	base := 5 * time.Second
	for i := 0; i < 1000; i++ {
		d := jitter(base)
		if d < 4*time.Second || d > 6*time.Second {
			t.Fatalf("jitter out of ±20%% range: %s", d)
		}
	}
}
