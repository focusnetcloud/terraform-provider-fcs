package provider

// Unit tests for the generic cluster resource (CRUD handlers invoked
// directly, short poll intervals/timeouts — no terraform CLI, no TF_ACC).

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

// newUnitClusterResource builds a business clusterResource with a fast poll
// interval and a short default create timeout for tests.
func newUnitClusterResource(t *testing.T, srv *mockapi.Server, createTimeout time.Duration) *clusterResource {
	t.Helper()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	r := NewBusinessClusterResource().(*clusterResource)
	r.client = c
	r.pollInterval = 2 * time.Millisecond
	r.cfg.defaultCreateTimeout = createTimeout
	return r
}

func clusterSchema(t *testing.T, r *clusterResource) fwschema.Schema {
	t.Helper()
	resp := resource.SchemaResponse{}
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %v", resp.Diagnostics)
	}
	return resp.Schema
}

// clusterValue builds a full attribute map for the cluster object type:
// computed attributes become unknown, everything else null, then overrides.
func clusterValue(t *testing.T, s fwschema.Schema, attrs map[string]tftypes.Value) tftypes.Value {
	t.Helper()
	objType, ok := s.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("schema type is not an object")
	}
	computed := map[string]bool{
		"id": true, "status": true, "api_server_url": true,
		"cluster_cidr": true, "service_cidr": true,
		"provisioning_diagnostics": true,
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

func runClusterCreate(t *testing.T, r *clusterResource, s fwschema.Schema, planRaw tftypes.Value) resource.CreateResponse {
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

func unitEnv(t *testing.T, srv *mockapi.Server, name string) string {
	t.Helper()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	env, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: name})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	return env.ID
}

func stateString(t *testing.T, state tfsdk.State, attr string) string {
	t.Helper()
	var v string
	if diags := state.GetAttribute(context.Background(), path.Root(attr), &v); diags.HasError() {
		t.Fatalf("get %s: %v", attr, diags)
	}
	return v
}

func TestUnitClusterCreatePollsToActive(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 3 // poll must loop, not give up on the first GET
	envID := unitEnv(t, srv, "lab-cl-unit")
	r := newUnitClusterResource(t, srv, 5*time.Second)
	s := clusterSchema(t, r)

	resp := runClusterCreate(t, r, s, clusterValue(t, s, map[string]tftypes.Value{
		"environment_id": tftypes.NewValue(tftypes.String, envID),
		"size":           tftypes.NewValue(tftypes.String, "S"),
	}))
	if resp.Diagnostics.HasError() {
		t.Fatalf("create diagnostics: %v", resp.Diagnostics)
	}
	if got := stateString(t, resp.State, "status"); got != "active" {
		t.Fatalf("expected status active after poll-to-ready, got %q", got)
	}
	if stateString(t, resp.State, "id") == "" {
		t.Fatal("expected computed id in state")
	}
	if stateString(t, resp.State, "api_server_url") == "" {
		t.Fatal("expected computed api_server_url in state")
	}
	if srv.ClusterCount() != 1 {
		t.Fatalf("expected 1 cluster on the server, got %d", srv.ClusterCount())
	}
}

func TestUnitClusterCreateErrorStatusKeepsState(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 2
	srv.FailClusterProvisioning = true
	srv.ClusterProvisioningDiagnostics = "service_gateway_scope=dedicated_k8s/13105; tenant_networking_pipeline(id=9493, status=failed)"
	envID := unitEnv(t, srv, "lab-cl-unit-err")
	r := newUnitClusterResource(t, srv, 5*time.Second)
	s := clusterSchema(t, r)

	resp := runClusterCreate(t, r, s, clusterValue(t, s, map[string]tftypes.Value{
		"environment_id": tftypes.NewValue(tftypes.String, envID),
	}))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected error diagnostics for status=error")
	}
	// The cluster exists server-side: state must keep the id (tainted)
	// instead of orphaning it.
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

func TestUnitClusterCreateTimeoutPath(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 1 << 30 // never active
	envID := unitEnv(t, srv, "lab-cl-unit-timeout")
	r := newUnitClusterResource(t, srv, 50*time.Millisecond)
	s := clusterSchema(t, r)

	resp := runClusterCreate(t, r, s, clusterValue(t, s, map[string]tftypes.Value{
		"environment_id": tftypes.NewValue(tftypes.String, envID),
	}))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected timeout diagnostics")
	}
	found := false
	for _, d := range resp.Diagnostics.Errors() {
		if containsAll(d.Detail(), "timed out", "provisioning") {
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

func TestUnitClusterCreateHonorsTimeoutsBlock(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 1 << 30
	envID := unitEnv(t, srv, "lab-cl-unit-tblock")
	// Long default; the configured timeouts.create must win.
	r := newUnitClusterResource(t, srv, time.Hour)
	s := clusterSchema(t, r)
	objType := s.Type().TerraformType(context.Background()).(tftypes.Object)
	timeoutsType := objType.AttributeTypes["timeouts"].(tftypes.Object)

	start := time.Now()
	resp := runClusterCreate(t, r, s, clusterValue(t, s, map[string]tftypes.Value{
		"environment_id": tftypes.NewValue(tftypes.String, envID),
		"timeouts": tftypes.NewValue(timeoutsType, map[string]tftypes.Value{
			"create": tftypes.NewValue(tftypes.String, "50ms"),
			"delete": tftypes.NewValue(tftypes.String, nil),
		}),
	}))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected timeout diagnostics from the timeouts block value")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("configured create timeout (50ms) was not honored, took %s", elapsed)
	}
}

func TestUnitClusterDeleteGonePoll(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 1
	srv.ClusterGoneAfterGETs = 2 // destroyed readable — a gone signal for the poll
	envID := unitEnv(t, srv, "lab-cl-unit-del")
	r := newUnitClusterResource(t, srv, 5*time.Second)
	s := clusterSchema(t, r)

	created := runClusterCreate(t, r, s, clusterValue(t, s, map[string]tftypes.Value{
		"environment_id": tftypes.NewValue(tftypes.String, envID),
	}))
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

func TestUnitClusterReadRemovesOn404(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 1
	srv.ClusterGoneAfterGETs = 0
	envID := unitEnv(t, srv, "lab-cl-unit-read")
	r := newUnitClusterResource(t, srv, 5*time.Second)
	s := clusterSchema(t, r)

	created := runClusterCreate(t, r, s, clusterValue(t, s, map[string]tftypes.Value{
		"environment_id": tftypes.NewValue(tftypes.String, envID),
	}))
	if created.Diagnostics.HasError() {
		t.Fatalf("create: %v", created.Diagnostics)
	}

	// Remove server-side (reaper/cascade), then Read must drop state.
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

// TestUnitClusterReadRemovesOnDestroyed: the server may keep destroyed rows
// readable (status=destroyed) — Read must treat them like a 404 and drop
// the resource so Terraform plans a re-create instead of drifting.
func TestUnitClusterReadRemovesOnDestroyed(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 1
	srv.ClusterGoneAfterGETs = 1 << 30 // destroyed row stays readable (like the server DB)
	envID := unitEnv(t, srv, "lab-cl-unit-destroyed")
	r := newUnitClusterResource(t, srv, 5*time.Second)
	s := clusterSchema(t, r)

	created := runClusterCreate(t, r, s, clusterValue(t, s, map[string]tftypes.Value{
		"environment_id": tftypes.NewValue(tftypes.String, envID),
	}))
	if created.Diagnostics.HasError() {
		t.Fatalf("create: %v", created.Diagnostics)
	}

	// Out-of-band teardown (reaper/offboarding): GET returns status=destroyed.
	c, _ := client.New(srv.URL, unitToken)
	if err := c.DeleteCluster(context.Background(), envID, stateString(t, created.State, "id")); err != nil {
		t.Fatalf("out-of-band delete: %v", err)
	}
	readResp := resource.ReadResponse{State: created.State}
	r.Read(context.Background(), resource.ReadRequest{State: created.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read on a destroyed cluster must not error: %v", readResp.Diagnostics)
	}
	if !readResp.State.Raw.IsNull() {
		t.Fatal("expected resource to be removed from state for a destroyed cluster")
	}
}

func TestUnitClusterUpdateIsUnreachableGuard(t *testing.T) {
	r := NewBusinessClusterResource().(*clusterResource)
	resp := resource.UpdateResponse{}
	r.Update(context.Background(), resource.UpdateRequest{}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("Update must fail loudly: all attributes are RequiresReplace")
	}
}

func TestUnitFlexClusterDefaultCreateTimeoutCoversBackendReadiness(t *testing.T) {
	r := NewFlexClusterResource().(*clusterResource)
	if r.cfg.defaultCreateTimeout < 25*time.Minute {
		t.Fatalf("flex create timeout = %s, want at least 25m", r.cfg.defaultCreateTimeout)
	}
}

// TestUnitClusterSchemasPerKind: business/flex carry the sizing attributes,
// fcs_namespace must not.
func TestUnitClusterSchemasPerKind(t *testing.T) {
	sizingAttrs := []string{"size", "vcpu", "ram_gb", "storage_gb", "k8s_version"}
	commonAttrs := []string{"environment_id", "id", "status", "api_server_url", "cluster_cidr", "service_cidr", "provisioning_diagnostics", "timeouts"}

	for _, tc := range []struct {
		name   string
		res    resource.Resource
		sizing bool
	}{
		{"business", NewBusinessClusterResource(), true},
		{"flex", NewFlexClusterResource(), true},
		{"namespace", NewNamespaceResource(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := clusterSchema(t, tc.res.(*clusterResource))
			for _, a := range commonAttrs {
				if _, ok := s.Attributes[a]; !ok {
					t.Fatalf("missing common attribute %q", a)
				}
			}
			for _, a := range sizingAttrs {
				_, ok := s.Attributes[a]
				if ok != tc.sizing {
					t.Fatalf("attribute %q: present=%v, want %v", a, ok, tc.sizing)
				}
			}
		})
	}
}
