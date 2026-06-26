package client_test

// Tests for the VM sub-API client and the mock's VM status machine
// (provisioning -> active|stopped after N GETs; power stop/start flips the
// status; DELETE -> destroyed -> 404; idempotent re-POST over name;
// unknown image -> 422).

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
	"github.com/focusnetcloud/terraform-provider-fcs/internal/mockapi"
)

func newEnvForVms(t *testing.T, c *client.Client, name string) string {
	t.Helper()
	env, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: name})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	return env.ID
}

func boolPtr(b bool) *bool { return &b }

func TestCreateVmNew(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForVms(t, c, "lab-vm-new")

	vm, err := c.CreateVm(context.Background(), envID, client.VmSpec{Image: "ubuntu-22.04", Name: "target"})
	if err != nil {
		t.Fatalf("CreateVm: %v", err)
	}
	if vm.ID == "" {
		t.Fatal("expected non-empty vm id")
	}
	if vm.Name != "target" {
		t.Fatalf("expected name echo target, got %q", vm.Name)
	}
	if vm.Status != "provisioning" {
		t.Fatalf("expected status provisioning, got %q", vm.Status)
	}
	if srv.VmCount() != 1 {
		t.Fatalf("expected 1 vm, got %d", srv.VmCount())
	}
}

func TestCreateVmSendsIaasNetworkBinding(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envName := "lab-vm-iaas-binding"
	envID := newEnvForVms(t, c, envName)

	_, err := c.CreateVm(context.Background(), envID, client.VmSpec{
		Image:     "ubuntu-22.04",
		Name:      "target",
		VdcID:     "11111111-1111-4111-8111-111111111111",
		NetworkID: "22222222-2222-4222-8222-222222222222",
	})
	if err != nil {
		t.Fatalf("CreateVm: %v", err)
	}
	spec, ok := srv.VmSpecByName(envName, "target")
	if !ok {
		t.Fatal("expected mock VM spec for target")
	}
	if spec.VdcID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("expected vdc_id in create payload, got %q", spec.VdcID)
	}
	if spec.NetworkID != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("expected network_id in create payload, got %q", spec.NetworkID)
	}
}

func TestCreateVmGeneratesNameWhenEmpty(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForVms(t, c, "lab-vm-gen")

	vm, err := c.CreateVm(context.Background(), envID, client.VmSpec{Image: "ubuntu-22.04"})
	if err != nil {
		t.Fatalf("CreateVm: %v", err)
	}
	if vm.Name == "" {
		t.Fatal("expected a server-generated name")
	}
}

func TestCreateVmIdempotentReapply(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForVms(t, c, "lab-vm-idem")
	spec := client.VmSpec{Image: "ubuntu-22.04", Name: "target"}

	first, err := c.CreateVm(context.Background(), envID, spec)
	if err != nil {
		t.Fatalf("first CreateVm: %v", err)
	}
	second, err := c.CreateVm(context.Background(), envID, spec)
	if err != nil {
		t.Fatalf("idempotent re-POST: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected the existing vm on re-POST, got %s != %s", first.ID, second.ID)
	}
	if srv.VmCount() != 1 {
		t.Fatalf("expected 1 vm after re-POST, got %d", srv.VmCount())
	}
}

func TestCreateVmUnknownImage422(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForVms(t, c, "lab-vm-img")

	_, err := c.CreateVm(context.Background(), envID, client.VmSpec{Image: "nope"})
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected APIError 422 for unknown image, got %v", err)
	}
	if srv.VmCount() != 0 {
		t.Fatalf("expected no vm after 422, got %d", srv.VmCount())
	}
}

// TestVmNullContractFieldsDecodeEmpty verifies the null mapping: the server
// (and the mock) serialize vm_ip/console_url as JSON null until they exist;
// the client must surface them as empty strings (which the provider then
// maps to null state attributes), not fail decoding.
func TestVmNullContractFieldsDecodeEmpty(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForVms(t, c, "lab-vm-null")

	vm, err := c.CreateVm(context.Background(), envID, client.VmSpec{Image: "ubuntu-22.04"})
	if err != nil {
		t.Fatalf("CreateVm: %v", err)
	}
	if vm.Status != "provisioning" {
		t.Fatalf("expected provisioning, got %q", vm.Status)
	}
	if vm.VMIP != "" {
		t.Fatalf("expected null vm_ip to decode as empty string, got %q", vm.VMIP)
	}
	if vm.ConsoleURL != "" {
		t.Fatalf("expected null console_url to decode as empty string, got %q", vm.ConsoleURL)
	}
}

func TestVmStatusMachineReadyAfterGETs(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	srv.VmReadyAfterGETs = 2
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForVms(t, c, "lab-vm-ready")

	vm, err := c.CreateVm(context.Background(), envID, client.VmSpec{Image: "ubuntu-22.04"})
	if err != nil {
		t.Fatalf("CreateVm: %v", err)
	}
	got, err := c.GetVm(context.Background(), envID, vm.ID)
	if err != nil {
		t.Fatalf("GetVm 1: %v", err)
	}
	if got.Status != "provisioning" {
		t.Fatalf("expected provisioning on first GET, got %q", got.Status)
	}
	got, err = c.GetVm(context.Background(), envID, vm.ID)
	if err != nil {
		t.Fatalf("GetVm 2: %v", err)
	}
	if got.Status != "active" {
		t.Fatalf("expected active on second GET, got %q", got.Status)
	}
	if got.VMIP == "" {
		t.Fatal("expected a vm_ip once active")
	}
}

func TestVmStatusMachineRunningFalseEndsStopped(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	srv.VmReadyAfterGETs = 1
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForVms(t, c, "lab-vm-stopped")

	vm, err := c.CreateVm(context.Background(), envID,
		client.VmSpec{Image: "ubuntu-22.04", Running: boolPtr(false)})
	if err != nil {
		t.Fatalf("CreateVm: %v", err)
	}
	got, err := c.GetVm(context.Background(), envID, vm.ID)
	if err != nil {
		t.Fatalf("GetVm: %v", err)
	}
	if got.Status != "stopped" {
		t.Fatalf("expected stopped for running=false, got %q", got.Status)
	}
}

func TestPowerVmFlipsStatus(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	srv.VmReadyAfterGETs = 1
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForVms(t, c, "lab-vm-power")

	vm, err := c.CreateVm(context.Background(), envID, client.VmSpec{Image: "ubuntu-22.04"})
	if err != nil {
		t.Fatalf("CreateVm: %v", err)
	}
	if _, err := c.GetVm(context.Background(), envID, vm.ID); err != nil { // -> active
		t.Fatalf("GetVm: %v", err)
	}

	if err := c.PowerVm(context.Background(), envID, vm.ID, "stop"); err != nil {
		t.Fatalf("PowerVm stop: %v", err)
	}
	got, _ := c.GetVm(context.Background(), envID, vm.ID)
	if got.Status != "stopped" {
		t.Fatalf("expected stopped after power stop, got %q", got.Status)
	}

	if err := c.PowerVm(context.Background(), envID, vm.ID, "start"); err != nil {
		t.Fatalf("PowerVm start: %v", err)
	}
	got, _ = c.GetVm(context.Background(), envID, vm.ID)
	if got.Status != "active" {
		t.Fatalf("expected active after power start, got %q", got.Status)
	}

	if err := c.PowerVm(context.Background(), envID, vm.ID, "restart"); err != nil {
		t.Fatalf("PowerVm restart: %v", err)
	}
	got, _ = c.GetVm(context.Background(), envID, vm.ID)
	if got.Status != "active" {
		t.Fatalf("restart must keep the status, got %q", got.Status)
	}
}

func TestPowerVmInvalidAction422(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	srv.VmReadyAfterGETs = 1
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForVms(t, c, "lab-vm-badverb")

	vm, err := c.CreateVm(context.Background(), envID, client.VmSpec{Image: "ubuntu-22.04"})
	if err != nil {
		t.Fatalf("CreateVm: %v", err)
	}
	err = c.PowerVm(context.Background(), envID, vm.ID, "reboot")
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected APIError 422 for invalid power action, got %v", err)
	}
}

func TestDeleteVmDestroyedThenGone(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	srv.VmReadyAfterGETs = 1
	srv.VmGoneAfterGETs = 1
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForVms(t, c, "lab-vm-del")

	vm, err := c.CreateVm(context.Background(), envID, client.VmSpec{Image: "ubuntu-22.04"})
	if err != nil {
		t.Fatalf("CreateVm: %v", err)
	}
	if err := c.DeleteVm(context.Background(), envID, vm.ID); err != nil {
		t.Fatalf("DeleteVm: %v", err)
	}
	got, err := c.GetVm(context.Background(), envID, vm.ID)
	if err != nil {
		t.Fatalf("GetVm after delete: %v", err)
	}
	if got.Status != "destroyed" {
		t.Fatalf("expected destroyed after DELETE, got %q", got.Status)
	}
	// Second GET after the gone window: 404 — and DeleteVm tolerates it.
	_, err = c.GetVm(context.Background(), envID, vm.ID)
	if !client.IsNotFound(err) {
		t.Fatalf("expected 404 after gone window, got %v", err)
	}
	if err := c.DeleteVm(context.Background(), envID, vm.ID); err != nil {
		t.Fatalf("repeated DeleteVm must tolerate 404: %v", err)
	}
	if srv.VmCount() != 0 {
		t.Fatalf("expected 0 live vms, got %d", srv.VmCount())
	}
}

func TestEnvironmentDeleteCascadesVms(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForVms(t, c, "lab-vm-cascade")

	if _, err := c.CreateVm(context.Background(), envID, client.VmSpec{Image: "ubuntu-22.04"}); err != nil {
		t.Fatalf("CreateVm: %v", err)
	}
	if err := c.DeleteEnvironment(context.Background(), envID); err != nil {
		t.Fatalf("DeleteEnvironment: %v", err)
	}
	if srv.VmCount() != 0 {
		t.Fatalf("expected cascade to remove vms, got %d", srv.VmCount())
	}
}

func TestGetVmUnknown404(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForVms(t, c, "lab-vm-404")

	_, err := c.GetVm(context.Background(), envID, "00000000-0000-4000-8000-000000000000")
	if !client.IsNotFound(err) {
		t.Fatalf("expected IsNotFound, got %v", err)
	}
}
