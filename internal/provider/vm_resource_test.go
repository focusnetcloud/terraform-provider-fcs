package provider

// Unit tests for the fcs_vm resource (CRUD handlers invoked directly,
// short poll intervals/timeouts — no terraform CLI, no TF_ACC).

import (
	"context"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	fwschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
	"github.com/focusnetcloud/terraform-provider-fcs/internal/mockapi"
)

// newUnitVmResource builds a vmResource with a fast poll interval against
// the mock.
func newUnitVmResource(t *testing.T, srv *mockapi.Server) *vmResource {
	t.Helper()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	r := NewVmResource().(*vmResource)
	r.client = c
	r.pollInterval = 2 * time.Millisecond
	return r
}

func vmSchema(t *testing.T, r *vmResource) fwschema.Schema {
	t.Helper()
	resp := resource.SchemaResponse{}
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %v", resp.Diagnostics)
	}
	return resp.Schema
}

// vmValue builds a full attribute map for the vm object type: computed
// attributes become unknown, everything else null, then overrides.
func vmValue(t *testing.T, s fwschema.Schema, attrs map[string]tftypes.Value) tftypes.Value {
	t.Helper()
	objType, ok := s.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("schema type is not an object")
	}
	computed := map[string]bool{
		"id": true, "name": true, "status": true, "vm_ip": true,
		"console_url": true, "cpu_cores": true, "memory_gb": true,
		"disk_gb": true, "nic_network": true, "running": true,
	}
	full := map[string]tftypes.Value{}
	for name, at := range objType.AttributeTypes {
		if computed[name] {
			full[name] = tftypes.NewValue(at, tftypes.UnknownValue)
		} else {
			full[name] = tftypes.NewValue(at, nil)
		}
	}
	for k, v := range attrs {
		full[k] = v
	}
	return tftypes.NewValue(objType, full)
}

// vmPlanDefaults mirrors the schema defaults the framework would apply
// during PlanResourceChange (defaults are not applied when invoking the
// CRUD handlers directly).
func vmPlanDefaults() map[string]tftypes.Value {
	return map[string]tftypes.Value{
		"cpu_cores":   tftypes.NewValue(tftypes.Number, 2),
		"memory_gb":   tftypes.NewValue(tftypes.Number, 4),
		"disk_gb":     tftypes.NewValue(tftypes.Number, 20),
		"nic_network": tftypes.NewValue(tftypes.String, "tenant"),
		"running":     tftypes.NewValue(tftypes.Bool, true),
	}
}

func runVmCreate(t *testing.T, r *vmResource, s fwschema.Schema, planRaw tftypes.Value) resource.CreateResponse {
	t.Helper()
	objType := s.Type().TerraformType(context.Background()).(tftypes.Object)
	req := resource.CreateRequest{
		Plan:   tfsdk.Plan{Schema: s, Raw: planRaw},
		Config: tfsdk.Config{Schema: s, Raw: planRaw},
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(objType, nil)}}
	r.Create(context.Background(), req, &resp)
	return resp
}

func vmCreateAttrs(envID string, overrides map[string]tftypes.Value) map[string]tftypes.Value {
	attrs := vmPlanDefaults()
	attrs["environment_id"] = tftypes.NewValue(tftypes.String, envID)
	attrs["image"] = tftypes.NewValue(tftypes.String, "ubuntu-22.04")
	for k, v := range overrides {
		attrs[k] = v
	}
	return attrs
}

func stateBool(t *testing.T, state tfsdk.State, attr string) bool {
	t.Helper()
	var v bool
	if diags := state.GetAttribute(context.Background(), path.Root(attr), &v); diags.HasError() {
		t.Fatalf("get %s: %v", attr, diags)
	}
	return v
}

func TestUnitVmCreatePollsToActive(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.VmReadyAfterGETs = 3 // poll must loop, not give up on the first GET
	envID := unitEnv(t, srv, "lab-vm-unit")
	r := newUnitVmResource(t, srv)
	s := vmSchema(t, r)

	resp := runVmCreate(t, r, s, vmValue(t, s, vmCreateAttrs(envID, map[string]tftypes.Value{
		"name": tftypes.NewValue(tftypes.String, "target"),
	})))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create diagnostics: %v", resp.Diagnostics)
	}
	if got := stateString(t, resp.State, "status"); got != "active" {
		t.Fatalf("expected status active after poll-to-ready, got %q", got)
	}
	if stateString(t, resp.State, "id") == "" {
		t.Fatal("expected computed id in state")
	}
	if stateString(t, resp.State, "vm_ip") == "" {
		t.Fatal("expected computed vm_ip in state once active")
	}
	if got := stateString(t, resp.State, "name"); got != "target" {
		t.Fatalf("expected configured name to stick, got %q", got)
	}
	if srv.VmCount() != 1 {
		t.Fatalf("expected 1 vm on the server, got %d", srv.VmCount())
	}
}

func TestUnitVmCreateSendsIaasNetworkBinding(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.VmReadyAfterGETs = 1
	envName := "lab-vm-unit-iaas-binding"
	envID := unitEnv(t, srv, envName)
	r := newUnitVmResource(t, srv)
	s := vmSchema(t, r)

	resp := runVmCreate(t, r, s, vmValue(t, s, vmCreateAttrs(envID, map[string]tftypes.Value{
		"name":       tftypes.NewValue(tftypes.String, "target"),
		"vdc_id":     tftypes.NewValue(tftypes.String, "11111111-1111-4111-8111-111111111111"),
		"network_id": tftypes.NewValue(tftypes.String, "22222222-2222-4222-8222-222222222222"),
	})))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create diagnostics: %v", resp.Diagnostics)
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
	if got := stateString(t, resp.State, "vdc_id"); got != spec.VdcID {
		t.Fatalf("expected vdc_id in state, got %q", got)
	}
	if got := stateString(t, resp.State, "network_id"); got != spec.NetworkID {
		t.Fatalf("expected network_id in state, got %q", got)
	}
}

func TestUnitVmCreateRejectsPartialIaasNetworkBinding(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	envID := unitEnv(t, srv, "lab-vm-unit-partial-iaas-binding")
	r := newUnitVmResource(t, srv)
	s := vmSchema(t, r)

	resp := runVmCreate(t, r, s, vmValue(t, s, vmCreateAttrs(envID, map[string]tftypes.Value{
		"name":   tftypes.NewValue(tftypes.String, "target"),
		"vdc_id": tftypes.NewValue(tftypes.String, "11111111-1111-4111-8111-111111111111"),
	})))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected diagnostics for partial IaaS-vDC network binding")
	}
	if srv.VmCount() != 0 {
		t.Fatalf("expected no VM create API call, got %d VMs", srv.VmCount())
	}
}

func TestUnitVmCreateAdoptsServerGeneratedName(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.VmReadyAfterGETs = 1
	envID := unitEnv(t, srv, "lab-vm-unit-name")
	r := newUnitVmResource(t, srv)
	s := vmSchema(t, r)

	// name stays unknown (not configured) — the server generates one and
	// the provider must adopt it as the computed value.
	resp := runVmCreate(t, r, s, vmValue(t, s, vmCreateAttrs(envID, nil)))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create diagnostics: %v", resp.Diagnostics)
	}
	if got := stateString(t, resp.State, "name"); got == "" {
		t.Fatal("expected the server-generated name in state")
	}
}

func TestUnitVmCreateRunningFalsePollsToStopped(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.VmReadyAfterGETs = 2
	envID := unitEnv(t, srv, "lab-vm-unit-stopped")
	r := newUnitVmResource(t, srv)
	s := vmSchema(t, r)

	resp := runVmCreate(t, r, s, vmValue(t, s, vmCreateAttrs(envID, map[string]tftypes.Value{
		"running": tftypes.NewValue(tftypes.Bool, false),
	})))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create diagnostics: %v", resp.Diagnostics)
	}
	if got := stateString(t, resp.State, "status"); got != "stopped" {
		t.Fatalf("expected status stopped for running=false, got %q", got)
	}
}

func TestUnitVmCreateErrorStatusKeepsState(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.VmReadyAfterGETs = 2
	srv.FailVmProvisioning = true
	envID := unitEnv(t, srv, "lab-vm-unit-err")
	r := newUnitVmResource(t, srv)
	s := vmSchema(t, r)

	resp := runVmCreate(t, r, s, vmValue(t, s, vmCreateAttrs(envID, nil)))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected error diagnostics for status=error")
	}
	// The VM exists server-side: state must keep the id (tainted) instead
	// of orphaning it.
	if resp.State.Raw.IsNull() {
		t.Fatal("state must be kept on provisioning failure")
	}
	if stateString(t, resp.State, "id") == "" {
		t.Fatal("expected the created vm id in state")
	}
	if got := stateString(t, resp.State, "status"); got != "error" {
		t.Fatalf("expected last status error in state, got %q", got)
	}
}

func TestUnitVmCreateUnknownImage422(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	envID := unitEnv(t, srv, "lab-vm-unit-img")
	r := newUnitVmResource(t, srv)
	s := vmSchema(t, r)

	resp := runVmCreate(t, r, s, vmValue(t, s, vmCreateAttrs(envID, map[string]tftypes.Value{
		"image": tftypes.NewValue(tftypes.String, "nope"),
	})))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected error diagnostics for unknown image")
	}
	found := false
	for _, d := range resp.Diagnostics.Errors() {
		if containsAll(d.Detail(), "422", "ImageNotAllowed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 422 ImageNotAllowed detail, got: %v", resp.Diagnostics)
	}
}

func TestUnitVmCreateHonorsTimeoutsBlock(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.VmReadyAfterGETs = 1 << 30 // never ready
	envID := unitEnv(t, srv, "lab-vm-unit-tblock")
	r := newUnitVmResource(t, srv)
	s := vmSchema(t, r)
	objType := s.Type().TerraformType(context.Background()).(tftypes.Object)
	timeoutsType := objType.AttributeTypes["timeouts"].(tftypes.Object)

	start := time.Now()
	resp := runVmCreate(t, r, s, vmValue(t, s, vmCreateAttrs(envID, map[string]tftypes.Value{
		"timeouts": tftypes.NewValue(timeoutsType, map[string]tftypes.Value{
			"create": tftypes.NewValue(tftypes.String, "50ms"),
			"delete": tftypes.NewValue(tftypes.String, nil),
		}),
	})))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected timeout diagnostics from the timeouts block value")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("configured create timeout (50ms) was not honored, took %s", elapsed)
	}
	if resp.State.Raw.IsNull() || stateString(t, resp.State, "id") == "" {
		t.Fatal("state must keep the vm id on timeout (tainted)")
	}
}

func TestUnitVmUpdateTogglesPower(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.VmReadyAfterGETs = 1
	envID := unitEnv(t, srv, "lab-vm-unit-power")
	r := newUnitVmResource(t, srv)
	s := vmSchema(t, r)

	created := runVmCreate(t, r, s, vmValue(t, s, vmCreateAttrs(envID, map[string]tftypes.Value{
		"name": tftypes.NewValue(tftypes.String, "toggle"),
	})))
	if created.Diagnostics.HasError() {
		t.Fatalf("create: %v", created.Diagnostics)
	}
	vmID := stateString(t, created.State, "id")

	// Plan: running=false, status/vm_ip unknown (as ModifyPlan would set).
	planRaw := vmValue(t, s, vmCreateAttrs(envID, map[string]tftypes.Value{
		"name":    tftypes.NewValue(tftypes.String, "toggle"),
		"id":      tftypes.NewValue(tftypes.String, vmID),
		"running": tftypes.NewValue(tftypes.Bool, false),
	}))
	updResp := resource.UpdateResponse{State: tfsdk.State{Schema: s, Raw: created.State.Raw}}
	r.Update(context.Background(), resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: s, Raw: planRaw},
		State: created.State,
	}, &updResp)
	if updResp.Diagnostics.HasError() {
		t.Fatalf("update: %v", updResp.Diagnostics)
	}
	if got := stateString(t, updResp.State, "status"); got != "stopped" {
		t.Fatalf("expected status stopped after power stop, got %q", got)
	}
	if stateBool(t, updResp.State, "running") {
		t.Fatal("expected running=false in state after update")
	}

	// Toggle back: running=true -> power start -> active.
	planRaw = vmValue(t, s, vmCreateAttrs(envID, map[string]tftypes.Value{
		"name":    tftypes.NewValue(tftypes.String, "toggle"),
		"id":      tftypes.NewValue(tftypes.String, vmID),
		"running": tftypes.NewValue(tftypes.Bool, true),
	}))
	updResp2 := resource.UpdateResponse{State: tfsdk.State{Schema: s, Raw: updResp.State.Raw}}
	r.Update(context.Background(), resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: s, Raw: planRaw},
		State: updResp.State,
	}, &updResp2)
	if updResp2.Diagnostics.HasError() {
		t.Fatalf("update back: %v", updResp2.Diagnostics)
	}
	if got := stateString(t, updResp2.State, "status"); got != "active" {
		t.Fatalf("expected status active after power start, got %q", got)
	}
}

func TestUnitVmDeleteGonePoll(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.VmReadyAfterGETs = 1
	srv.VmGoneAfterGETs = 2
	envID := unitEnv(t, srv, "lab-vm-unit-del")
	r := newUnitVmResource(t, srv)
	s := vmSchema(t, r)

	created := runVmCreate(t, r, s, vmValue(t, s, vmCreateAttrs(envID, nil)))
	if created.Diagnostics.HasError() {
		t.Fatalf("create: %v", created.Diagnostics)
	}

	delResp := resource.DeleteResponse{}
	r.Delete(context.Background(), resource.DeleteRequest{State: created.State}, &delResp)
	if delResp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", delResp.Diagnostics)
	}
	if srv.VmCount() != 0 {
		t.Fatalf("expected 0 live vms after gone-poll, got %d", srv.VmCount())
	}
}

func TestUnitVmReadRemovesOn404AndDestroyed(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.VmReadyAfterGETs = 1
	srv.VmGoneAfterGETs = 1 << 30 // destroyed row stays readable (like the server DB)
	envID := unitEnv(t, srv, "lab-vm-unit-read")
	r := newUnitVmResource(t, srv)
	s := vmSchema(t, r)

	created := runVmCreate(t, r, s, vmValue(t, s, vmCreateAttrs(envID, nil)))
	if created.Diagnostics.HasError() {
		t.Fatalf("create: %v", created.Diagnostics)
	}

	// Out-of-band delete (TTL reaper): GET still returns the destroyed
	// row — Read must treat it like a 404 and drop the resource.
	c, _ := client.New(srv.URL, unitToken)
	if err := c.DeleteVm(context.Background(), envID, stateString(t, created.State, "id")); err != nil {
		t.Fatalf("out-of-band delete: %v", err)
	}
	readResp := resource.ReadResponse{State: created.State}
	r.Read(context.Background(), resource.ReadRequest{State: created.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read after destroy must not error: %v", readResp.Diagnostics)
	}
	if !readResp.State.Raw.IsNull() {
		t.Fatal("expected resource to be removed from state for a destroyed vm")
	}
}

func TestUnitVmModifyPlanMarksStatusUnknownOnRunningChange(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.VmReadyAfterGETs = 1
	envID := unitEnv(t, srv, "lab-vm-unit-plan")
	r := newUnitVmResource(t, srv)
	s := vmSchema(t, r)

	created := runVmCreate(t, r, s, vmValue(t, s, vmCreateAttrs(envID, map[string]tftypes.Value{
		"name": tftypes.NewValue(tftypes.String, "plan"),
	})))
	if created.Diagnostics.HasError() {
		t.Fatalf("create: %v", created.Diagnostics)
	}

	// Build a plan from the created state with running flipped to false
	// but status/vm_ip still carrying the stale state values.
	vmID := stateString(t, created.State, "id")
	planRaw := vmValue(t, s, vmCreateAttrs(envID, map[string]tftypes.Value{
		"name":        tftypes.NewValue(tftypes.String, "plan"),
		"id":          tftypes.NewValue(tftypes.String, vmID),
		"running":     tftypes.NewValue(tftypes.Bool, false),
		"status":      tftypes.NewValue(tftypes.String, "active"),
		"vm_ip":       tftypes.NewValue(tftypes.String, stateString(t, created.State, "vm_ip")),
		"console_url": tftypes.NewValue(tftypes.String, nil),
	}))
	resp := resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: s, Raw: planRaw}}
	r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		Plan:  tfsdk.Plan{Schema: s, Raw: planRaw},
		State: tfsdk.State{Schema: s, Raw: created.State.Raw},
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("modify plan: %v", resp.Diagnostics)
	}

	var status, vmIP tftypes.Value
	if err := resp.Plan.Raw.As(&map[string]tftypes.Value{}); err == nil {
		var m map[string]tftypes.Value
		_ = resp.Plan.Raw.As(&m)
		status, vmIP = m["status"], m["vm_ip"]
	}
	if status.IsKnown() {
		t.Fatalf("expected status unknown in plan after running change, got %v", status)
	}
	if vmIP.IsKnown() {
		t.Fatalf("expected vm_ip unknown in plan after running change, got %v", vmIP)
	}
}
