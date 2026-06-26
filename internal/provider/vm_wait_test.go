package provider

// Unit tests for the fcs_vm poll-to-ready / poll-to-gone helpers against
// the mock status machine (no terraform CLI, no TF_ACC).

import (
	"context"
	"strings"
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
