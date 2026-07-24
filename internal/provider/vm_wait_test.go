package provider

// Unit tests for the fcs_vm poll-to-ready / poll-to-gone helpers against
// the mock status machine (no terraform CLI, no TF_ACC).

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

func newWaitVm(t *testing.T, srv *mockapi.Server, running bool) (*client.Client, string, string) {
	t.Helper()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	env, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: "lab-vm-wait"})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	vm, err := c.CreateVm(context.Background(), env.ID, client.VmSpec{Image: "ubuntu-22.04", Running: &running})
	if err != nil {
		t.Fatalf("create vm: %v", err)
	}
	return c, env.ID, vm.ID
}

func TestWaitForVmStatusActive(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.VmReadyAfterGETs = 3
	c, envID, vmID := newWaitVm(t, srv, true)

	vm, err := waitForVmStatus(context.Background(), c, envID, vmID, "active", time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("waitForVmStatus: %v", err)
	}
	if vm.Status != "active" || vm.VMIP == "" {
		t.Fatalf("expected active vm with ip, got %+v", vm)
	}
}

func TestWaitForVmStatusStoppedTarget(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.VmReadyAfterGETs = 2
	c, envID, vmID := newWaitVm(t, srv, false)

	vm, err := waitForVmStatus(context.Background(), c, envID, vmID, "stopped", time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("waitForVmStatus: %v", err)
	}
	if vm.Status != "stopped" {
		t.Fatalf("expected stopped, got %q", vm.Status)
	}
}

func TestWaitForVmPowerStateUsesLiveStatus(t *testing.T) {
	var statusRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/status"):
			request := statusRequests.Add(1)
			phase := "Running"
			if request >= 3 {
				phase = "Stopped"
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(
				w,
				`{"phase":%q,"reason":null,"message":"","platform_error":false}`,
				phase,
			)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	status, err := waitForVmPowerState(
		context.Background(),
		c,
		"env-power",
		"vm-power",
		false,
		time.Millisecond,
		time.Second,
	)
	if err != nil {
		t.Fatalf("waitForVmPowerState: %v", err)
	}
	if status.Phase != "Stopped" {
		t.Fatalf("expected Stopped phase, got %+v", status)
	}
	if got := statusRequests.Load(); got != 3 {
		t.Fatalf("expected three live status requests, got %d", got)
	}
}

func TestWaitForVmPowerStateFailedIsTerminal(t *testing.T) {
	var statusRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		request := statusRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if request == 1 {
			_, _ = w.Write([]byte(
				`{"phase":"Scheduling","reason":null,` +
					`"message":"","platform_error":false}`,
			))
			return
		}
		_, _ = w.Write([]byte(
			`{"phase":"Failed","reason":"ImagePullFailed",` +
				`"message":"digest unavailable","platform_error":true}`,
		))
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	status, err := waitForVmPowerState(
		context.Background(),
		c,
		"env-failed",
		"vm-failed",
		true,
		time.Millisecond,
		time.Second,
	)
	if err == nil ||
		!strings.Contains(err.Error(), "ImagePullFailed") ||
		!strings.Contains(err.Error(), "digest unavailable") {
		t.Fatalf("expected immediate live failure with reason and message, got %v", err)
	}
	if status == nil || status.Phase != "Failed" {
		t.Fatalf("expected failed live status, got %+v", status)
	}
	if got := statusRequests.Load(); got != 2 {
		t.Fatalf("terminal live failure must stop after transition, got %d requests", got)
	}
}

func TestWaitForVmPowerStateAllowsRemediationFromInitialFailure(t *testing.T) {
	var statusRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		request := statusRequests.Add(1)
		phase := "Failed"
		platformError := true
		if request >= 3 {
			phase = "Stopped"
			platformError = false
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(
			w,
			`{"phase":%q,"reason":"platform_error",`+
				`"message":"old crash","platform_error":%t}`,
			phase,
			platformError,
		)
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	status, err := waitForVmPowerState(
		context.Background(),
		c,
		"env-remediate",
		"vm-remediate",
		false,
		time.Millisecond,
		time.Second,
	)
	if err != nil {
		t.Fatalf("initial failure must not block a remediation stop: %v", err)
	}
	if status.Phase != "Stopped" {
		t.Fatalf("expected Stopped phase, got %+v", status)
	}
	if got := statusRequests.Load(); got != 3 {
		t.Fatalf("expected three live status requests, got %d", got)
	}
}

func TestWaitForVmPowerStateFailsAfterLeavingInitialFailure(t *testing.T) {
	var statusRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		request := statusRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch request {
		case 1:
			_, _ = w.Write([]byte(
				`{"phase":"Failed","reason":"OldFailure",` +
					`"message":"preexisting failure","platform_error":true}`,
			))
		case 2:
			_, _ = w.Write([]byte(
				`{"phase":"Scheduling","reason":null,` +
					`"message":"","platform_error":false}`,
			))
		default:
			_, _ = w.Write([]byte(
				`{"phase":"Failed","reason":"ImagePullFailed",` +
					`"message":"retry failed","platform_error":true}`,
			))
		}
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	status, err := waitForVmPowerState(
		context.Background(),
		c,
		"env-reentry",
		"vm-reentry",
		true,
		time.Millisecond,
		time.Second,
	)
	if err == nil ||
		!strings.Contains(err.Error(), "ImagePullFailed") ||
		!strings.Contains(err.Error(), "retry failed") {
		t.Fatalf("expected terminal failure after leaving initial failure, got %v", err)
	}
	if status == nil || status.Phase != "Failed" {
		t.Fatalf("expected failed live status, got %+v", status)
	}
	if got := statusRequests.Load(); got != 3 {
		t.Fatalf("expected failure on the third live status request, got %d", got)
	}
}

func TestWaitForVmPowerStateTargetWinsOverStickyErrorFlag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(
			`{"phase":"Running","reason":"platform_error",` +
				`"message":"stale flag","platform_error":true}`,
		))
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	status, err := waitForVmPowerState(
		context.Background(),
		c,
		"env-sticky",
		"vm-sticky",
		true,
		time.Millisecond,
		time.Second,
	)
	if err != nil {
		t.Fatalf("observed target phase must win over a sticky error flag: %v", err)
	}
	if status.Phase != "Running" {
		t.Fatalf("expected Running phase, got %+v", status)
	}
}

func TestWaitForVmPowerStateRetriesTransientReadForStart(t *testing.T) {
	var statusRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if statusRequests.Add(1) == 1 {
			http.Error(w, `{"detail":"temporary origin failure"}`, http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(
			`{"phase":"Running","reason":null,"message":"","platform_error":false}`,
		))
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	status, err := waitForVmPowerState(
		context.Background(),
		c,
		"env-start",
		"vm-start",
		true,
		time.Millisecond,
		time.Second,
	)
	if err != nil {
		t.Fatalf("transient live status read must be retried: %v", err)
	}
	if status.Phase != "Running" {
		t.Fatalf("expected Running phase, got %+v", status)
	}
	if got := statusRequests.Load(); got != 2 {
		t.Fatalf("expected exactly two status requests, got %d", got)
	}
}

func TestWaitForVmPowerStateDoesNotRetryPermanentReadError(t *testing.T) {
	var statusRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		statusRequests.Add(1)
		http.Error(w, `{"detail":"VM not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	_, err = waitForVmPowerState(
		context.Background(),
		c,
		"env-gone",
		"vm-gone",
		false,
		time.Millisecond,
		time.Second,
	)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("expected permanent 404, got %v", err)
	}
	if got := statusRequests.Load(); got != 1 {
		t.Fatalf("permanent live status error must not be retried, got %d requests", got)
	}
}

func TestWaitForVmStatusErrorIsTerminal(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.VmReadyAfterGETs = 1
	srv.FailVmProvisioning = true
	c, envID, vmID := newWaitVm(t, srv, true)

	vm, err := waitForVmStatus(context.Background(), c, envID, vmID, "active", time.Millisecond, time.Second)
	if err == nil {
		t.Fatal("expected error for status=error")
	}
	if !strings.Contains(err.Error(), "error") || !strings.Contains(err.Error(), "/status") {
		t.Fatalf("expected error message pointing at the status debug endpoint, got %v", err)
	}
	if vm == nil || vm.Status != "error" {
		t.Fatalf("expected the last seen vm alongside the error, got %+v", vm)
	}
}

func TestWaitForVmStatusTimeout(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.VmReadyAfterGETs = 1 << 30
	c, envID, vmID := newWaitVm(t, srv, true)

	_, err := waitForVmStatus(context.Background(), c, envID, vmID, "active", time.Millisecond, 30*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got %v", err)
	}
	if !strings.Contains(err.Error(), `"provisioning"`) {
		t.Fatalf("expected last status in the timeout error, got %v", err)
	}
}

func TestWaitForVmStatusRetriesTransientHTTPStatus(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, `{"detail":"origin temporarily unavailable"}`, http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"vm-502","status":"active","vm_ip":"10.10.101.42"}`))
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	vm, err := waitForVmStatus(
		context.Background(), c, "env-502", "vm-502", "active",
		time.Millisecond, time.Second,
	)
	if err != nil {
		t.Fatalf("transient HTTP 502 must be retried: %v", err)
	}
	if vm.Status != "active" {
		t.Fatalf("expected active VM after retry, got %+v", vm)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("expected exactly 2 GET requests, got %d", got)
	}
}

func TestWaitForVmStatusDoesNotRetryPermanentHTTPStatus(t *testing.T) {
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

	_, err = waitForVmStatus(
		context.Background(), c, "env-401", "vm-401", "active",
		time.Millisecond, time.Second,
	)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("expected permanent HTTP 401 error, got %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("permanent HTTP 401 must not be retried, got %d requests", got)
	}
}

func TestWaitForVmStatusTimeoutIncludesTransientGETError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"detail":"origin temporarily unavailable"}`, http.StatusBadGateway)
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	_, err = waitForVmStatus(
		context.Background(), c, "env-502", "vm-502", "active",
		time.Millisecond, 100*time.Millisecond,
	)
	if err == nil || !strings.Contains(err.Error(), "last transient GET error") ||
		!strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("timeout must preserve the last transient GET error, got %v", err)
	}
	if !strings.Contains(err.Error(), "terraform untaint") ||
		!strings.Contains(err.Error(), "<environment_id>/<vm_id>") {
		t.Fatalf("timeout must explain tracked and absent-state recovery, got %v", err)
	}
}

func TestWaitForVmGoneAcceptsDestroyedAnd404(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.VmReadyAfterGETs = 1
	srv.VmGoneAfterGETs = 1 << 30 // destroyed row stays readable
	c, envID, vmID := newWaitVm(t, srv, true)

	if err := c.DeleteVm(context.Background(), envID, vmID); err != nil {
		t.Fatalf("DeleteVm: %v", err)
	}
	// status=destroyed counts as gone (the server keeps the row readable).
	if err := waitForVmGone(context.Background(), c, envID, vmID, time.Millisecond, time.Second); err != nil {
		t.Fatalf("waitForVmGone on destroyed row: %v", err)
	}
	// And a hard 404 (unknown vm) is gone as well.
	if err := waitForVmGone(context.Background(), c, envID, "00000000-0000-4000-8000-000000000000", time.Millisecond, time.Second); err != nil {
		t.Fatalf("waitForVmGone on 404: %v", err)
	}
}

func TestWaitForVmGoneRetriesTransientHTTPStatus(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, `{"detail":"origin temporarily unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, nil)
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	if err := waitForVmGone(
		context.Background(), c, "env-503", "vm-503",
		time.Millisecond, time.Second,
	); err != nil {
		t.Fatalf("transient HTTP 503 must be retried during delete: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("expected exactly 2 GET requests, got %d", got)
	}
}

func TestWaitForVmGoneDoesNotRetryPermanentHTTPStatus(t *testing.T) {
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

	err = waitForVmGone(
		context.Background(), c, "env-401", "vm-401",
		time.Millisecond, time.Second,
	)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("expected permanent HTTP 401 error, got %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("permanent HTTP 401 must not be retried, got %d requests", got)
	}
}
