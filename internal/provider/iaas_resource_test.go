package provider

// Unit tests for fcs_iaas_vdc and fcs_iaas_network. They invoke resource
// handlers directly against the mock API, without terraform CLI/TF_ACC.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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

func newUnitIaasVdcResource(t *testing.T, srv *mockapi.Server) *iaasVdcResource {
	t.Helper()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	r := NewIaasVdcResource().(*iaasVdcResource)
	r.client = c
	return r
}

func newUnitIaasNetworkResource(t *testing.T, srv *mockapi.Server) *iaasNetworkResource {
	t.Helper()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	r := NewIaasNetworkResource().(*iaasNetworkResource)
	r.client = c
	r.pollInterval = 2 * time.Millisecond
	return r
}

func iaasVdcSchema(t *testing.T, r *iaasVdcResource) fwschema.Schema {
	t.Helper()
	resp := resource.SchemaResponse{}
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %v", resp.Diagnostics)
	}
	return resp.Schema
}

func iaasNetworkSchema(t *testing.T, r *iaasNetworkResource) fwschema.Schema {
	t.Helper()
	resp := resource.SchemaResponse{}
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %v", resp.Diagnostics)
	}
	return resp.Schema
}

func resourceObjectType(t *testing.T, s fwschema.Schema) tftypes.Object {
	t.Helper()
	objType, ok := s.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("schema type is not an object")
	}
	return objType
}

func valueForSchema(t *testing.T, s fwschema.Schema, computed map[string]bool, attrs map[string]tftypes.Value) tftypes.Value {
	t.Helper()
	objType := resourceObjectType(t, s)
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

func iaasVdcValue(t *testing.T, s fwschema.Schema, attrs map[string]tftypes.Value) tftypes.Value {
	t.Helper()
	return valueForSchema(t, s, map[string]bool{
		"id": true, "status": true, "iaas_vdc_seq": true, "gateway_scope": true,
		"scope_key": true, "gateway_vpc_name": true, "gateway_name": true, "created_at": true,
	}, attrs)
}

func iaasNetworkValue(t *testing.T, s fwschema.Schema, attrs map[string]tftypes.Value) tftypes.Value {
	t.Helper()
	return valueForSchema(t, s, map[string]bool{
		"id": true, "status": true, "vlan_id": true, "harvester_namespace": true,
		"harvester_nad_name": true, "kubeovn_subnet_name": true,
		"default_gateway_ip": true, "created_at": true,
	}, attrs)
}

func runIaasVdcCreate(t *testing.T, r *iaasVdcResource, s fwschema.Schema, planRaw tftypes.Value) resource.CreateResponse {
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

func runIaasNetworkCreate(t *testing.T, r *iaasNetworkResource, s fwschema.Schema, planRaw tftypes.Value) resource.CreateResponse {
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

func stateInt64(t *testing.T, state tfsdk.State, attr string) int64 {
	t.Helper()
	var v int64
	if diags := state.GetAttribute(context.Background(), path.Root(attr), &v); diags.HasError() {
		t.Fatalf("get %s: %v", attr, diags)
	}
	return v
}

func TestUnitIaasVdcCreateStoresGatewayMetadata(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	envID := unitEnv(t, srv, "lab-iaas-vdc-provider")
	r := newUnitIaasVdcResource(t, srv)
	s := iaasVdcSchema(t, r)

	resp := runIaasVdcCreate(t, r, s, iaasVdcValue(t, s, map[string]tftypes.Value{
		"environment_id": tftypes.NewValue(tftypes.String, envID),
		"name":           tftypes.NewValue(tftypes.String, "prod"),
		"labels": tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, map[string]tftypes.Value{
			"service": tftypes.NewValue(tftypes.String, "iaas"),
		}),
	}))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create diagnostics: %v", resp.Diagnostics)
	}
	if stateString(t, resp.State, "id") == "" {
		t.Fatal("expected computed id in state")
	}
	if got := stateString(t, resp.State, "status"); got != "planned" {
		t.Fatalf("expected status planned, got %q", got)
	}
	if got := stateInt64(t, resp.State, "iaas_vdc_seq"); got != 42 {
		t.Fatalf("expected iaas_vdc_seq 42, got %d", got)
	}
	if got := stateString(t, resp.State, "gateway_scope"); got != "iaas_vdc" {
		t.Fatalf("expected gateway_scope iaas_vdc, got %q", got)
	}
	if got := stateString(t, resp.State, "scope_key"); got != "42" {
		t.Fatalf("expected scope_key 42, got %q", got)
	}
	if got := stateString(t, resp.State, "gateway_vpc_name"); got != "iaas-42" {
		t.Fatalf("expected gateway_vpc_name iaas-42, got %q", got)
	}
	if got := stateString(t, resp.State, "gateway_name"); got != "gw-iaas-42" {
		t.Fatalf("expected gateway_name gw-iaas-42, got %q", got)
	}
	if srv.IaasVdcCount() != 1 {
		t.Fatalf("expected 1 IaaS-vDC, got %d", srv.IaasVdcCount())
	}
}

func TestUnitIaasVdcCreateUsesPostResponseForState(t *testing.T) {
	getCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			getCalls++
			http.Error(w, "GET must not be required after create", http.StatusInternalServerError)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/environments/env-1/iaas-vdcs" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{
				"id":"vdc-1",
				"environment_id":"env-1",
				"name":"prod",
				"status":"planned",
				"iaas_vdc_seq":42,
				"gateway_scope":"iaas_vdc",
				"scope_key":"42",
				"gateway_vpc_name":"iaas-42",
				"gateway_name":"gw-iaas-42",
				"created_at":"2026-06-20T09:45:00Z"
			}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	r := NewIaasVdcResource().(*iaasVdcResource)
	r.client = c
	s := iaasVdcSchema(t, r)

	resp := runIaasVdcCreate(t, r, s, iaasVdcValue(t, s, map[string]tftypes.Value{
		"environment_id": tftypes.NewValue(tftypes.String, "env-1"),
		"name":           tftypes.NewValue(tftypes.String, "prod"),
	}))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create must succeed from POST response without a follow-up GET: %v", resp.Diagnostics)
	}
	if getCalls != 0 {
		t.Fatalf("Create performed %d GET calls before writing state", getCalls)
	}
	if got := stateString(t, resp.State, "id"); got != "vdc-1" {
		t.Fatalf("expected id from POST response, got %q", got)
	}
	if got := stateString(t, resp.State, "gateway_name"); got != "gw-iaas-42" {
		t.Fatalf("expected gateway metadata from POST response, got %q", got)
	}
}

func TestUnitIaasNetworkCreatePollsToActiveAndStoresRealizationFields(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.IaasNetworkReadyAfterGETs = 2
	envID := unitEnv(t, srv, "lab-iaas-net-provider")
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	vdc, err := c.CreateIaasVdc(context.Background(), envID, client.IaasVdcSpec{Name: "prod"})
	if err != nil {
		t.Fatalf("CreateIaasVdc: %v", err)
	}

	r := newUnitIaasNetworkResource(t, srv)
	s := iaasNetworkSchema(t, r)
	resp := runIaasNetworkCreate(t, r, s, iaasNetworkValue(t, s, map[string]tftypes.Value{
		"environment_id": tftypes.NewValue(tftypes.String, envID),
		"vdc_id":         tftypes.NewValue(tftypes.String, vdc.ID),
		"name":           tftypes.NewValue(tftypes.String, "web"),
		"cidr":           tftypes.NewValue(tftypes.String, "10.80.1.0/24"),
		"policy_default": tftypes.NewValue(tftypes.String, "private"),
	}))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create diagnostics: %v", resp.Diagnostics)
	}
	if stateString(t, resp.State, "id") == "" {
		t.Fatal("expected computed id in state")
	}
	if got := stateString(t, resp.State, "status"); got != "active" {
		t.Fatalf("expected status active after poll-to-ready, got %q", got)
	}
	if got := stateString(t, resp.State, "policy_default"); got != "private" {
		t.Fatalf("expected policy_default private, got %q", got)
	}
	if got := stateInt64(t, resp.State, "vlan_id"); got < 1 {
		t.Fatalf("expected realized vlan_id, got %d", got)
	}
	for _, attr := range []string{"harvester_namespace", "harvester_nad_name", "kubeovn_subnet_name", "default_gateway_ip"} {
		if got := stateString(t, resp.State, attr); got == "" {
			t.Fatalf("expected realized %s in state", attr)
		}
	}
	if srv.IaasNetworkCount() != 1 {
		t.Fatalf("expected 1 IaaS network, got %d", srv.IaasNetworkCount())
	}
}

func TestUnitIaasNetworkCreateErrorStatusKeepsState(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.IaasNetworkReadyAfterGETs = 2
	srv.FailIaasNetworkProvisioning = true
	envID := unitEnv(t, srv, "lab-iaas-net-provider-error")
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	vdc, err := c.CreateIaasVdc(context.Background(), envID, client.IaasVdcSpec{Name: "prod"})
	if err != nil {
		t.Fatalf("CreateIaasVdc: %v", err)
	}
	r := newUnitIaasNetworkResource(t, srv)
	s := iaasNetworkSchema(t, r)

	resp := runIaasNetworkCreate(t, r, s, iaasNetworkValue(t, s, map[string]tftypes.Value{
		"environment_id": tftypes.NewValue(tftypes.String, envID),
		"vdc_id":         tftypes.NewValue(tftypes.String, vdc.ID),
		"name":           tftypes.NewValue(tftypes.String, "web"),
		"cidr":           tftypes.NewValue(tftypes.String, "10.80.1.0/24"),
		"policy_default": tftypes.NewValue(tftypes.String, "private"),
	}))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected diagnostics for status=error")
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("state must be kept on provisioning failure")
	}
	if stateString(t, resp.State, "id") == "" {
		t.Fatal("expected the created network id in state")
	}
	if got := stateString(t, resp.State, "status"); got != "error" {
		t.Fatalf("expected last status error in state, got %q", got)
	}
	found := false
	for _, d := range resp.Diagnostics.Errors() {
		if strings.Contains(d.Detail(), `status "error"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected error-status detail, got: %v", resp.Diagnostics)
	}
}

func TestUnitIaasNetworkCreateHonorsTimeoutsBlock(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.IaasNetworkReadyAfterGETs = 1 << 30
	envID := unitEnv(t, srv, "lab-iaas-net-provider-timeout")
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	vdc, err := c.CreateIaasVdc(context.Background(), envID, client.IaasVdcSpec{Name: "prod"})
	if err != nil {
		t.Fatalf("CreateIaasVdc: %v", err)
	}
	r := newUnitIaasNetworkResource(t, srv)
	s := iaasNetworkSchema(t, r)
	objType := s.Type().TerraformType(context.Background()).(tftypes.Object)
	timeoutsType := objType.AttributeTypes["timeouts"].(tftypes.Object)

	start := time.Now()
	resp := runIaasNetworkCreate(t, r, s, iaasNetworkValue(t, s, map[string]tftypes.Value{
		"environment_id": tftypes.NewValue(tftypes.String, envID),
		"vdc_id":         tftypes.NewValue(tftypes.String, vdc.ID),
		"name":           tftypes.NewValue(tftypes.String, "web"),
		"cidr":           tftypes.NewValue(tftypes.String, "10.80.1.0/24"),
		"policy_default": tftypes.NewValue(tftypes.String, "private"),
		"timeouts": tftypes.NewValue(timeoutsType, map[string]tftypes.Value{
			"create": tftypes.NewValue(tftypes.String, "50ms"),
			"delete": tftypes.NewValue(tftypes.String, nil),
		}),
	}))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected timeout diagnostics")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("configured create timeout (50ms) was not honored, took %s", elapsed)
	}
}

func TestUnitIaasNetworkCreateConflictKeepsStateEmpty(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	envID := unitEnv(t, srv, "lab-iaas-net-provider-conflict")
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	vdc, err := c.CreateIaasVdc(context.Background(), envID, client.IaasVdcSpec{Name: "prod"})
	if err != nil {
		t.Fatalf("CreateIaasVdc: %v", err)
	}
	if _, err := c.CreateIaasNetwork(context.Background(), envID, vdc.ID, client.IaasNetworkSpec{
		Name: "web",
		CIDR: "10.80.1.0/24",
	}); err != nil {
		t.Fatalf("seed CreateIaasNetwork: %v", err)
	}

	r := newUnitIaasNetworkResource(t, srv)
	s := iaasNetworkSchema(t, r)
	resp := runIaasNetworkCreate(t, r, s, iaasNetworkValue(t, s, map[string]tftypes.Value{
		"environment_id": tftypes.NewValue(tftypes.String, envID),
		"vdc_id":         tftypes.NewValue(tftypes.String, vdc.ID),
		"name":           tftypes.NewValue(tftypes.String, "web"),
		"cidr":           tftypes.NewValue(tftypes.String, "10.80.2.0/24"),
		"policy_default": tftypes.NewValue(tftypes.String, "private"),
	}))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected diagnostics for conflicting CIDR")
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("conflicting create must not persist partial state: %s", resp.State.Raw.String())
	}
	if srv.IaasNetworkCount() != 1 {
		t.Fatalf("expected only seed network after conflict, got %d", srv.IaasNetworkCount())
	}
}
