package provider

// Unit tests for fcs_dedicated_cluster (kind=dedicated): the CRUD handlers are
// invoked directly with short poll intervals/timeouts (no terraform CLI, no
// TF_ACC). They mirror the generic cluster unit tests but exercise the
// dedicated node-pool schema and spec mapping.

import (
	"context"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	fwschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
	"github.com/focusnetcloud/terraform-provider-fcs/internal/mockapi"
)

// newUnitDedicatedResource builds a dedicated clusterResource with a fast poll
// interval and a short default create timeout for tests.
func newUnitDedicatedResource(t *testing.T, srv *mockapi.Server, createTimeout time.Duration) *clusterResource {
	t.Helper()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	r := NewDedicatedClusterResource().(*clusterResource)
	r.client = c
	r.pollInterval = 2 * time.Millisecond
	r.cfg.defaultCreateTimeout = createTimeout
	return r
}

// dedicatedComputed are the attributes the cluster read endpoint computes
// server-side; in a unit-test plan value they must be unknown (everything else
// is null unless explicitly overridden). The dedicated node-pool defaults
// (cp_nodes/worker_*/pvc_storage_gb) are normally materialised by Terraform
// from the schema Default; the unit tests set the ones they assert on
// explicitly and leave the rest null (read as 0), which the mock accepts.
var dedicatedComputed = map[string]bool{
	"id": true, "status": true, "api_server_url": true,
	"cluster_cidr": true, "service_cidr": true,
	"provisioning_diagnostics": true,
}

// dedicatedValue builds a full attribute map for the dedicated cluster object
// type: computed contract attributes become unknown, everything else null,
// then overrides are applied.
func dedicatedValue(t *testing.T, s fwschema.Schema, attrs map[string]tftypes.Value) tftypes.Value {
	t.Helper()
	objType, ok := s.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("schema type is not an object")
	}
	full := map[string]tftypes.Value{}
	for name, at := range objType.AttributeTypes {
		if dedicatedComputed[name] {
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

// requiredDedicatedSizing is the minimal valid node-pool config (cp_vcpu and
// cp_ram_gb are Required).
func requiredDedicatedSizing() map[string]tftypes.Value {
	return map[string]tftypes.Value{
		"cp_vcpu":   tftypes.NewValue(tftypes.Number, 4),
		"cp_ram_gb": tftypes.NewValue(tftypes.Number, 8),
	}
}

func TestUnitDedicatedCreatePollsToActive(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.DedicatedClusterReadyAfterGETs = 3 // poll must loop, not give up on the first GET
	envID := unitEnv(t, srv, "lab-ded-unit")
	r := newUnitDedicatedResource(t, srv, 5*time.Second)
	s := clusterSchema(t, r)

	attrs := requiredDedicatedSizing()
	attrs["environment_id"] = tftypes.NewValue(tftypes.String, envID)
	attrs["cp_nodes"] = tftypes.NewValue(tftypes.Number, 3)
	attrs["worker_nodes"] = tftypes.NewValue(tftypes.Number, 2)

	resp := runClusterCreate(t, r, s, dedicatedValue(t, s, attrs))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create diagnostics: %v", resp.Diagnostics)
	}
	if got := stateString(t, resp.State, "status"); got != "active" {
		t.Fatalf("expected status active after poll-to-ready, got %q", got)
	}
	if stateString(t, resp.State, "id") == "" {
		t.Fatal("expected computed id in state")
	}
	// Contract: api_server_url = https://<public_ip>:6443 once active.
	if url := stateString(t, resp.State, "api_server_url"); url == "" {
		t.Fatal("expected computed api_server_url in state")
	}
	if srv.ClusterCount() != 1 {
		t.Fatalf("expected 1 cluster on the server, got %d", srv.ClusterCount())
	}
}

func TestUnitDedicatedModifyPlanMarksVolatileFieldsUnknownOnResize(t *testing.T) {
	r := NewDedicatedClusterResource().(*clusterResource)
	s := clusterSchema(t, r)
	stateAttrs := requiredDedicatedSizing()
	stateAttrs["environment_id"] = tftypes.NewValue(tftypes.String, "env-dedicated-plan")
	stateAttrs["id"] = tftypes.NewValue(tftypes.String, "cluster-dedicated-plan")
	stateAttrs["worker_nodes"] = tftypes.NewValue(tftypes.Number, 1)
	stateAttrs["worker_vcpu"] = tftypes.NewValue(tftypes.Number, 2)
	stateAttrs["worker_ram_gb"] = tftypes.NewValue(tftypes.Number, 4)
	stateAttrs["status"] = tftypes.NewValue(tftypes.String, "resizing")
	stateAttrs["provisioning_diagnostics"] = tftypes.NewValue(tftypes.String, "old diagnostics")
	stateRaw := dedicatedValue(t, s, stateAttrs)

	planAttrs := requiredDedicatedSizing()
	planAttrs["environment_id"] = tftypes.NewValue(tftypes.String, "env-dedicated-plan")
	planAttrs["id"] = tftypes.NewValue(tftypes.String, "cluster-dedicated-plan")
	planAttrs["worker_nodes"] = tftypes.NewValue(tftypes.Number, 2)
	planAttrs["worker_vcpu"] = tftypes.NewValue(tftypes.Number, 2)
	planAttrs["worker_ram_gb"] = tftypes.NewValue(tftypes.Number, 4)
	planAttrs["status"] = tftypes.NewValue(tftypes.String, "resizing")
	planAttrs["provisioning_diagnostics"] = tftypes.NewValue(tftypes.String, "old diagnostics")
	planRaw := dedicatedValue(t, s, planAttrs)

	resp := resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: s, Raw: planRaw}}
	r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		Plan:  tfsdk.Plan{Schema: s, Raw: planRaw},
		State: tfsdk.State{Schema: s, Raw: stateRaw},
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("modify plan: %v", resp.Diagnostics)
	}

	var values map[string]tftypes.Value
	if err := resp.Plan.Raw.As(&values); err != nil {
		t.Fatalf("decode modified plan: %v", err)
	}
	for _, attribute := range []string{"status", "provisioning_diagnostics"} {
		if values[attribute].IsKnown() {
			t.Fatalf("expected %s unknown in dedicated resize plan, got %v", attribute, values[attribute])
		}
	}
}

func TestUnitDedicatedUpdateRejectsInvalidDesiredSpec(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.DedicatedClusterReadyAfterGETs = 1
	envID := unitEnv(t, srv, "lab-ded-unit-guard")
	r := newUnitDedicatedResource(t, srv, 5*time.Second)
	s := clusterSchema(t, r)

	attrs := requiredDedicatedSizing()
	attrs["environment_id"] = tftypes.NewValue(tftypes.String, envID)
	attrs["cp_nodes"] = tftypes.NewValue(tftypes.Number, 1)
	attrs["worker_nodes"] = tftypes.NewValue(tftypes.Number, 1)
	attrs["worker_vcpu"] = tftypes.NewValue(tftypes.Number, 2)
	attrs["worker_ram_gb"] = tftypes.NewValue(tftypes.Number, 4)
	attrs["pvc_storage_gb"] = tftypes.NewValue(tftypes.Number, 50)
	created := runClusterCreate(t, r, s, dedicatedValue(t, s, attrs))
	if created.Diagnostics.HasError() {
		t.Fatalf("create: %v", created.Diagnostics)
	}
	clusterID := stateString(t, created.State, "id")
	srv.ClusterResizeInvalidDesiredSpec = true

	planAttrs := requiredDedicatedSizing()
	planAttrs["environment_id"] = tftypes.NewValue(tftypes.String, envID)
	planAttrs["id"] = tftypes.NewValue(tftypes.String, clusterID)
	planAttrs["cp_nodes"] = tftypes.NewValue(tftypes.Number, 1)
	planAttrs["worker_nodes"] = tftypes.NewValue(tftypes.Number, 2)
	planAttrs["worker_vcpu"] = tftypes.NewValue(tftypes.Number, 2)
	planAttrs["worker_ram_gb"] = tftypes.NewValue(tftypes.Number, 4)
	planAttrs["pvc_storage_gb"] = tftypes.NewValue(tftypes.Number, 50)
	resp := resource.UpdateResponse{State: created.State}
	r.Update(context.Background(), resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: s, Raw: dedicatedValue(t, s, planAttrs)},
		State: created.State,
	}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected invalid dedicated desired_spec to fail closed")
	}
	if !containsAll(resp.Diagnostics.Errors()[0].Detail(), "invalid desired_spec", "dedicated sizing is incomplete") {
		t.Fatalf("unexpected diagnostic: %v", resp.Diagnostics)
	}
	if got := stateString(t, resp.State, "id"); got != clusterID {
		t.Fatalf("dedicated resize guard changed state identity: got %q, want %q", got, clusterID)
	}
}

func TestUnitDedicatedUpdateRecoversInFlightResizeWithoutSecondPatch(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.DedicatedClusterReadyAfterGETs = 1
	envID := unitEnv(t, srv, "lab-ded-unit-recover")
	r := newUnitDedicatedResource(t, srv, 5*time.Second)
	s := clusterSchema(t, r)

	attrs := requiredDedicatedSizing()
	attrs["environment_id"] = tftypes.NewValue(tftypes.String, envID)
	attrs["cp_nodes"] = tftypes.NewValue(tftypes.Number, 1)
	attrs["worker_nodes"] = tftypes.NewValue(tftypes.Number, 1)
	attrs["worker_vcpu"] = tftypes.NewValue(tftypes.Number, 2)
	attrs["worker_ram_gb"] = tftypes.NewValue(tftypes.Number, 4)
	attrs["pvc_storage_gb"] = tftypes.NewValue(tftypes.Number, 50)
	created := runClusterCreate(t, r, s, dedicatedValue(t, s, attrs))
	if created.Diagnostics.HasError() {
		t.Fatalf("create: %v", created.Diagnostics)
	}
	clusterID := stateString(t, created.State, "id")
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	srv.DedicatedClusterReadyAfterGETs = 2
	if _, err := c.ResizeCluster(context.Background(), envID, clusterID, client.ClusterResizeSpec{
		WorkerNodes: int64Pointer(2),
	}); err != nil {
		t.Fatalf("start dedicated resize: %v", err)
	}

	resizingState := created.State
	if diags := resizingState.SetAttribute(context.Background(), path.Root("worker_nodes"), int64(2)); diags.HasError() {
		t.Fatalf("set state worker_nodes: %v", diags)
	}
	if diags := resizingState.SetAttribute(context.Background(), path.Root("status"), "resizing"); diags.HasError() {
		t.Fatalf("set state status: %v", diags)
	}
	plan := tfsdk.Plan{Schema: s, Raw: resizingState.Raw}
	modifyResp := resource.ModifyPlanResponse{Plan: plan}
	r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{Plan: plan, State: resizingState}, &modifyResp)
	if modifyResp.Diagnostics.HasError() {
		t.Fatalf("modify recovery plan: %v", modifyResp.Diagnostics)
	}

	updateResp := resource.UpdateResponse{State: resizingState}
	r.Update(context.Background(), resource.UpdateRequest{Plan: modifyResp.Plan, State: resizingState}, &updateResp)
	if updateResp.Diagnostics.HasError() {
		t.Fatalf("recover dedicated update: %v", updateResp.Diagnostics)
	}
	if got := stateString(t, updateResp.State, "status"); got != "active" {
		t.Fatalf("dedicated recovery status = %q, want active", got)
	}
	if got := srv.ClusterPatchCount(); got != 1 {
		t.Fatalf("dedicated recovery sent a second PATCH: patch count = %d, want 1", got)
	}
}

func TestUnitDedicatedCreateErrorStatusKeepsState(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 2
	srv.FailClusterProvisioning = true
	srv.ClusterProvisioningDiagnostics = "service_gateway_scope=dedicated_k8s/13105; service_gateway(status=ready); tenant_networking_pipeline(id=9493, status=failed)"
	envID := unitEnv(t, srv, "lab-ded-unit-err")
	r := newUnitDedicatedResource(t, srv, 5*time.Second)
	s := clusterSchema(t, r)

	attrs := requiredDedicatedSizing()
	attrs["environment_id"] = tftypes.NewValue(tftypes.String, envID)

	resp := runClusterCreate(t, r, s, dedicatedValue(t, s, attrs))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected error diagnostics for status=error")
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("state must be kept on provisioning failure")
	}
	if stateString(t, resp.State, "id") == "" {
		t.Fatal("expected the created cluster id in state")
	}
	if got := stateString(t, resp.State, "status"); got != "error" {
		t.Fatalf("expected last status error in state, got %q", got)
	}
	if got := stateString(t, resp.State, "provisioning_diagnostics"); got == "" {
		t.Fatal("expected provisioning diagnostics in state")
	}
	foundDiagnostics := false
	for _, d := range resp.Diagnostics.Errors() {
		if containsAll(d.Detail(), "provisioning diagnostics", "dedicated_k8s/13105", "9493") {
			foundDiagnostics = true
		}
	}
	if !foundDiagnostics {
		t.Fatalf("expected provisioning diagnostics in error detail, got: %v", resp.Diagnostics)
	}
}

func TestUnitDedicatedCreateTimeoutPath(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 1 << 30 // never active
	srv.ClusterProvisioningDiagnostics = "service_gateway_scope=dedicated_k8s/13106; tenant_networking_pipeline(id=9494, status=running)"
	envID := unitEnv(t, srv, "lab-ded-unit-timeout")
	r := newUnitDedicatedResource(t, srv, 50*time.Millisecond)
	s := clusterSchema(t, r)

	attrs := requiredDedicatedSizing()
	attrs["environment_id"] = tftypes.NewValue(tftypes.String, envID)

	resp := runClusterCreate(t, r, s, dedicatedValue(t, s, attrs))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected timeout diagnostics")
	}
	found := false
	for _, d := range resp.Diagnostics.Errors() {
		if containsAll(d.Detail(), "timed out", "provisioning", "dedicated_k8s/13106", "9494") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected timeout detail with last status, got: %v", resp.Diagnostics)
	}
	if resp.State.Raw.IsNull() || stateString(t, resp.State, "id") == "" {
		t.Fatal("state must keep the cluster id on timeout (tainted)")
	}
}

func TestUnitDedicatedDeleteGonePoll(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 1
	srv.ClusterGoneAfterGETs = 2 // destroyed readable — a gone signal for the poll
	envID := unitEnv(t, srv, "lab-ded-unit-del")
	r := newUnitDedicatedResource(t, srv, 5*time.Second)
	s := clusterSchema(t, r)

	attrs := requiredDedicatedSizing()
	attrs["environment_id"] = tftypes.NewValue(tftypes.String, envID)

	created := runClusterCreate(t, r, s, dedicatedValue(t, s, attrs))
	if created.Diagnostics.HasError() {
		t.Fatalf("create: %v", created.Diagnostics)
	}

	delResp := resource.DeleteResponse{}
	r.Delete(context.Background(), resource.DeleteRequest{State: created.State}, &delResp)
	if delResp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", delResp.Diagnostics)
	}
	if srv.ClusterCount() != 0 {
		t.Fatalf("expected 0 live clusters after gone-poll, got %d", srv.ClusterCount())
	}
}

func TestUnitDedicatedReadRemovesOn404(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 1
	srv.ClusterGoneAfterGETs = 0
	envID := unitEnv(t, srv, "lab-ded-unit-read")
	r := newUnitDedicatedResource(t, srv, 5*time.Second)
	s := clusterSchema(t, r)

	attrs := requiredDedicatedSizing()
	attrs["environment_id"] = tftypes.NewValue(tftypes.String, envID)

	created := runClusterCreate(t, r, s, dedicatedValue(t, s, attrs))
	if created.Diagnostics.HasError() {
		t.Fatalf("create: %v", created.Diagnostics)
	}

	c, _ := client.New(srv.URL, unitToken)
	if err := c.DeleteCluster(context.Background(), envID, stateString(t, created.State, "id")); err != nil {
		t.Fatalf("out-of-band delete: %v", err)
	}
	readResp := resource.ReadResponse{State: created.State}
	r.Read(context.Background(), resource.ReadRequest{State: created.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read after 404 must not error: %v", readResp.Diagnostics)
	}
	if !readResp.State.Raw.IsNull() {
		t.Fatal("expected resource to be removed from state after 404")
	}
}

// TestUnitDedicatedSchemaHasNodePoolAttrs: the dedicated schema carries its own
// node-pool attributes and must NOT carry the generic t-shirt sizing attrs.
func TestUnitDedicatedSchemaHasNodePoolAttrs(t *testing.T) {
	r := NewDedicatedClusterResource().(*clusterResource)
	s := clusterSchema(t, r)

	commonAttrs := []string{"environment_id", "id", "status", "api_server_url", "cluster_cidr", "service_cidr", "provisioning_diagnostics", "timeouts"}
	nodePoolAttrs := []string{"cp_nodes", "cp_vcpu", "cp_ram_gb", "worker_nodes", "worker_vcpu", "worker_ram_gb", "pvc_storage_gb", "rke2_version"}
	genericSizing := []string{"size", "vcpu", "ram_gb", "storage_gb", "k8s_version"}

	for _, a := range commonAttrs {
		if _, ok := s.Attributes[a]; !ok {
			t.Fatalf("missing common attribute %q", a)
		}
	}
	for _, a := range nodePoolAttrs {
		if _, ok := s.Attributes[a]; !ok {
			t.Fatalf("missing dedicated node-pool attribute %q", a)
		}
	}
	for _, a := range genericSizing {
		if _, ok := s.Attributes[a]; ok {
			t.Fatalf("dedicated schema must not carry the generic sizing attribute %q", a)
		}
	}

	// cp_vcpu and cp_ram_gb are required; cp_nodes/worker_*/pvc_storage_gb are
	// optional (defaulted); every node-pool attribute requires replacement.
	mustBeRequired := map[string]bool{"cp_vcpu": true, "cp_ram_gb": true}
	for _, a := range nodePoolAttrs {
		at := s.Attributes[a]
		if mustBeRequired[a] && !at.IsRequired() {
			t.Fatalf("attribute %q must be Required", a)
		}
		if !mustBeRequired[a] && at.IsRequired() {
			t.Fatalf("attribute %q must be Optional, not Required", a)
		}
	}
}

func validateDedicatedConfig(t *testing.T, attrs map[string]tftypes.Value) resource.ValidateConfigResponse {
	t.Helper()
	r := NewDedicatedClusterResource().(*clusterResource)
	s := clusterSchema(t, r)
	resp := resource.ValidateConfigResponse{}
	r.ValidateConfig(context.Background(),
		resource.ValidateConfigRequest{
			Config: tfsdk.Config{Schema: s, Raw: dedicatedValue(t, s, attrs)},
		},
		&resp,
	)
	return resp
}

func TestUnitDedicatedValidateConfigSizingFloors(t *testing.T) {
	cases := []struct {
		name      string
		attrs     map[string]tftypes.Value
		wantError bool
	}{
		{
			name: "three_control_planes_allow_two_vcpu",
			attrs: map[string]tftypes.Value{
				"cp_nodes":  tftypes.NewValue(tftypes.Number, 3),
				"cp_vcpu":   tftypes.NewValue(tftypes.Number, 2),
				"cp_ram_gb": tftypes.NewValue(tftypes.Number, 8),
			},
		},
		{
			name: "single_combined_control_plane_needs_four_vcpu",
			attrs: map[string]tftypes.Value{
				"cp_nodes":     tftypes.NewValue(tftypes.Number, 1),
				"cp_vcpu":      tftypes.NewValue(tftypes.Number, 2),
				"cp_ram_gb":    tftypes.NewValue(tftypes.Number, 8),
				"worker_nodes": tftypes.NewValue(tftypes.Number, 0),
			},
			wantError: true,
		},
		{
			name: "single_control_plane_with_worker_pool_keeps_two_vcpu_valid",
			attrs: map[string]tftypes.Value{
				"cp_nodes":      tftypes.NewValue(tftypes.Number, 1),
				"cp_vcpu":       tftypes.NewValue(tftypes.Number, 2),
				"cp_ram_gb":     tftypes.NewValue(tftypes.Number, 8),
				"worker_nodes":  tftypes.NewValue(tftypes.Number, 1),
				"worker_vcpu":   tftypes.NewValue(tftypes.Number, 2),
				"worker_ram_gb": tftypes.NewValue(tftypes.Number, 4),
			},
		},
		{
			name: "pvc_storage_must_use_fifty_gb_steps",
			attrs: map[string]tftypes.Value{
				"cp_vcpu":        tftypes.NewValue(tftypes.Number, 2),
				"cp_ram_gb":      tftypes.NewValue(tftypes.Number, 8),
				"pvc_storage_gb": tftypes.NewValue(tftypes.Number, 75),
			},
			wantError: true,
		},
		{
			name: "worker_pool_needs_nonzero_sizing",
			attrs: map[string]tftypes.Value{
				"cp_vcpu":       tftypes.NewValue(tftypes.Number, 2),
				"cp_ram_gb":     tftypes.NewValue(tftypes.Number, 8),
				"worker_nodes":  tftypes.NewValue(tftypes.Number, 1),
				"worker_vcpu":   tftypes.NewValue(tftypes.Number, 0),
				"worker_ram_gb": tftypes.NewValue(tftypes.Number, 0),
			},
			wantError: true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			resp := validateDedicatedConfig(t, c.attrs)
			if got := resp.Diagnostics.HasError(); got != c.wantError {
				t.Fatalf("HasError=%v want %v (diags: %v)", got, c.wantError, resp.Diagnostics)
			}
		})
	}
}

// TestUnitDedicatedSpecMapping proves the planned node-pool values reach the
// ClusterSpec sent to the API (cp_*/worker_*/pvc_storage_gb/rke2_version).
func TestUnitDedicatedSpecMapping(t *testing.T) {
	r := NewDedicatedClusterResource().(*clusterResource)
	s := clusterSchema(t, r)

	attrs := map[string]tftypes.Value{
		"cp_nodes":       tftypes.NewValue(tftypes.Number, 3),
		"cp_vcpu":        tftypes.NewValue(tftypes.Number, 4),
		"cp_ram_gb":      tftypes.NewValue(tftypes.Number, 8),
		"worker_nodes":   tftypes.NewValue(tftypes.Number, 5),
		"worker_vcpu":    tftypes.NewValue(tftypes.Number, 8),
		"worker_ram_gb":  tftypes.NewValue(tftypes.Number, 16),
		"pvc_storage_gb": tftypes.NewValue(tftypes.Number, 250),
		"rke2_version":   tftypes.NewValue(tftypes.String, "v1.31.5+rke2r1"),
	}
	planRaw := dedicatedValue(t, s, attrs)
	plan := tfsdk.Plan{Schema: s, Raw: planRaw}

	var spec client.ClusterSpec
	var diags diag.Diagnostics
	readDedicatedSizing(context.Background(), &plan, &spec, &diags)
	if diags.HasError() {
		t.Fatalf("readDedicatedSizing diagnostics: %v", diags)
	}
	if spec.CPNodes != 3 || spec.CPVcpu != 4 || spec.CPRamGB != 8 {
		t.Fatalf("control-plane sizing not mapped: %+v", spec)
	}
	if spec.WorkerNodes != 5 || spec.WorkerVcpu != 8 || spec.WorkerRamGB != 16 {
		t.Fatalf("worker sizing not mapped: %+v", spec)
	}
	if spec.PVCStorageGB != 250 {
		t.Fatalf("pvc_storage_gb not mapped: %d", spec.PVCStorageGB)
	}
	if spec.RKE2Version != "v1.31.5+rke2r1" {
		t.Fatalf("rke2_version not mapped: %q", spec.RKE2Version)
	}
}
