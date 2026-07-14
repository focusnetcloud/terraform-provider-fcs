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

func TestUnitClusterCreateHonorsTimeoutsObject(t *testing.T) {
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
			"update": tftypes.NewValue(tftypes.String, nil),
		}),
	}))
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected timeout diagnostics from the timeouts object value")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("configured create timeout (50ms) was not honored, took %s", elapsed)
	}
}

func TestUnitClusterModifyPlanMarksVolatileFieldsUnknownOnResize(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 1
	envID := unitEnv(t, srv, "lab-cl-unit-plan")
	r := newUnitClusterResource(t, srv, 5*time.Second)
	s := clusterSchema(t, r)

	created := runClusterCreate(t, r, s, clusterValue(t, s, map[string]tftypes.Value{
		"environment_id": tftypes.NewValue(tftypes.String, envID),
		"size":           tftypes.NewValue(tftypes.String, "S"),
	}))
	if created.Diagnostics.HasError() {
		t.Fatalf("create: %v", created.Diagnostics)
	}

	planRaw := clusterValue(t, s, map[string]tftypes.Value{
		"environment_id":           tftypes.NewValue(tftypes.String, envID),
		"id":                       tftypes.NewValue(tftypes.String, stateString(t, created.State, "id")),
		"size":                     tftypes.NewValue(tftypes.String, "M"),
		"status":                   tftypes.NewValue(tftypes.String, "resizing"),
		"provisioning_diagnostics": tftypes.NewValue(tftypes.String, "old diagnostics"),
	})
	resp := resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: s, Raw: planRaw}}
	r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		Plan:  tfsdk.Plan{Schema: s, Raw: planRaw},
		State: created.State,
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
			t.Fatalf("expected %s unknown in plan after sizing change, got %v", attribute, values[attribute])
		}
	}
}

func TestUnitClusterUpdateRejectsUnverifiableResizeResponses(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configure  func(*mockapi.Server)
		wantDetail []string
	}{
		{
			name: "missing desired spec",
			configure: func(srv *mockapi.Server) {
				srv.ClusterResizeOmitDesiredSpec = true
			},
			wantDetail: []string{"omitted desired_spec", "safely verify convergence"},
		},
		{
			name: "changed identity",
			configure: func(srv *mockapi.Server) {
				srv.ClusterResizeResponseID = "unexpected-cluster-id"
			},
			wantDetail: []string{"Expected cluster ID", "unexpected-cluster-id"},
		},
		{
			name: "wrong kind",
			configure: func(srv *mockapi.Server) {
				srv.ClusterResizeResponseKind = "flex"
			},
			wantDetail: []string{"Expected kind", "business", "flex"},
		},
		{
			name: "invalid desired spec",
			configure: func(srv *mockapi.Server) {
				srv.ClusterResizeInvalidDesiredSpec = true
			},
			wantDetail: []string{"invalid desired_spec", "business sizing is incomplete"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := mockapi.New(unitToken)
			defer srv.Close()
			srv.ClusterReadyAfterGETs = 1
			envID := unitEnv(t, srv, "lab-cl-unit-guard")
			r := newUnitClusterResource(t, srv, 5*time.Second)
			s := clusterSchema(t, r)

			created := runClusterCreate(t, r, s, clusterValue(t, s, map[string]tftypes.Value{
				"environment_id": tftypes.NewValue(tftypes.String, envID),
				"size":           tftypes.NewValue(tftypes.String, "S"),
			}))
			if created.Diagnostics.HasError() {
				t.Fatalf("create: %v", created.Diagnostics)
			}
			tc.configure(srv)

			clusterID := stateString(t, created.State, "id")
			planRaw := clusterValue(t, s, map[string]tftypes.Value{
				"environment_id": tftypes.NewValue(tftypes.String, envID),
				"id":             tftypes.NewValue(tftypes.String, clusterID),
				"size":           tftypes.NewValue(tftypes.String, "M"),
			})
			resp := resource.UpdateResponse{State: created.State}
			r.Update(context.Background(), resource.UpdateRequest{
				Plan:  tfsdk.Plan{Schema: s, Raw: planRaw},
				State: created.State,
			}, &resp)
			if !resp.Diagnostics.HasError() {
				t.Fatal("expected resize response guard to fail")
			}
			if got := stateString(t, resp.State, "id"); got != clusterID {
				t.Fatalf("resize guard changed state identity: got %q, want %q", got, clusterID)
			}
			detail := resp.Diagnostics.Errors()[0].Detail()
			for _, want := range tc.wantDetail {
				if !containsAll(detail, want) {
					t.Fatalf("diagnostic %q does not contain %q", detail, want)
				}
			}
		})
	}
}

func TestUnitClusterUpdateRecoversInFlightResizeWithoutSecondPatch(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 1
	envID := unitEnv(t, srv, "lab-cl-unit-recover")
	r := newUnitClusterResource(t, srv, 5*time.Second)
	s := clusterSchema(t, r)

	created := runClusterCreate(t, r, s, clusterValue(t, s, map[string]tftypes.Value{
		"environment_id": tftypes.NewValue(tftypes.String, envID),
		"size":           tftypes.NewValue(tftypes.String, "S"),
	}))
	if created.Diagnostics.HasError() {
		t.Fatalf("create: %v", created.Diagnostics)
	}
	clusterID := stateString(t, created.State, "id")
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	srv.ClusterReadyAfterGETs = 2
	if _, err := c.ResizeCluster(context.Background(), envID, clusterID, client.ClusterResizeSpec{Size: "M"}); err != nil {
		t.Fatalf("start resize: %v", err)
	}

	resizingState := created.State
	if diags := resizingState.SetAttribute(context.Background(), path.Root("size"), "M"); diags.HasError() {
		t.Fatalf("set state size: %v", diags)
	}
	if diags := resizingState.SetAttribute(context.Background(), path.Root("status"), "resizing"); diags.HasError() {
		t.Fatalf("set state status: %v", diags)
	}
	plan := tfsdk.Plan{Schema: s, Raw: resizingState.Raw}
	modifyResp := resource.ModifyPlanResponse{Plan: plan}
	r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		Plan:  plan,
		State: resizingState,
	}, &modifyResp)
	if modifyResp.Diagnostics.HasError() {
		t.Fatalf("modify recovery plan: %v", modifyResp.Diagnostics)
	}
	var planStatus tftypes.Value
	var values map[string]tftypes.Value
	if err := modifyResp.Plan.Raw.As(&values); err != nil {
		t.Fatalf("decode recovery plan: %v", err)
	}
	planStatus = values["status"]
	if planStatus.IsKnown() {
		t.Fatalf("expected recovery status unknown, got %v", planStatus)
	}

	updateResp := resource.UpdateResponse{State: resizingState}
	r.Update(context.Background(), resource.UpdateRequest{
		Plan:  modifyResp.Plan,
		State: resizingState,
	}, &updateResp)
	if updateResp.Diagnostics.HasError() {
		t.Fatalf("recover update: %v", updateResp.Diagnostics)
	}
	if got := stateString(t, updateResp.State, "status"); got != "active" {
		t.Fatalf("recovery status = %q, want active", got)
	}
	if got := srv.ClusterPatchCount(); got != 1 {
		t.Fatalf("recovery sent a second PATCH: patch count = %d, want 1", got)
	}
}

func TestUnitClusterResizeRecoveryRejectsUnverifiableGetResponses(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configure  func(*mockapi.Server)
		wantDetail []string
	}{
		{
			name: "missing desired spec",
			configure: func(srv *mockapi.Server) {
				srv.ClusterGetOmitDesiredSpec = true
			},
			wantDetail: []string{"omitted desired_spec", "safely verify convergence"},
		},
		{
			name: "invalid desired spec",
			configure: func(srv *mockapi.Server) {
				srv.ClusterGetInvalidDesiredSpec = true
			},
			wantDetail: []string{"invalid desired_spec", "business sizing is incomplete"},
		},
		{
			name: "changed identity",
			configure: func(srv *mockapi.Server) {
				srv.ClusterGetResponseID = "unexpected-recovery-id"
			},
			wantDetail: []string{"Expected cluster ID", "unexpected-recovery-id"},
		},
		{
			name: "wrong kind",
			configure: func(srv *mockapi.Server) {
				srv.ClusterGetResponseKind = "flex"
			},
			wantDetail: []string{"Expected kind", "business", "flex"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := mockapi.New(unitToken)
			defer srv.Close()
			srv.ClusterReadyAfterGETs = 1
			envID := unitEnv(t, srv, "lab-cl-unit-recovery-guard")
			r := newUnitClusterResource(t, srv, 50*time.Millisecond)
			s := clusterSchema(t, r)
			created := runClusterCreate(t, r, s, clusterValue(t, s, map[string]tftypes.Value{
				"environment_id": tftypes.NewValue(tftypes.String, envID),
				"size":           tftypes.NewValue(tftypes.String, "S"),
			}))
			if created.Diagnostics.HasError() {
				t.Fatalf("create: %v", created.Diagnostics)
			}
			clusterID := stateString(t, created.State, "id")
			c, err := client.New(srv.URL, unitToken)
			if err != nil {
				t.Fatalf("client.New: %v", err)
			}
			srv.ClusterReadyAfterGETs = 1 << 30
			if _, err := c.ResizeCluster(context.Background(), envID, clusterID, client.ClusterResizeSpec{Size: "M"}); err != nil {
				t.Fatalf("start resize: %v", err)
			}
			tc.configure(srv)

			resizingState := created.State
			if diags := resizingState.SetAttribute(context.Background(), path.Root("size"), "M"); diags.HasError() {
				t.Fatalf("set state size: %v", diags)
			}
			if diags := resizingState.SetAttribute(context.Background(), path.Root("status"), "resizing"); diags.HasError() {
				t.Fatalf("set state status: %v", diags)
			}
			plan := tfsdk.Plan{Schema: s, Raw: resizingState.Raw}
			resp := resource.UpdateResponse{State: resizingState}
			r.Update(context.Background(), resource.UpdateRequest{Plan: plan, State: resizingState}, &resp)
			if !resp.Diagnostics.HasError() {
				t.Fatal("expected recovery response guard to fail")
			}
			detail := resp.Diagnostics.Errors()[0].Detail()
			for _, want := range tc.wantDetail {
				if !containsAll(detail, want) {
					t.Fatalf("diagnostic %q does not contain %q", detail, want)
				}
			}
			if got := stateString(t, resp.State, "id"); got != clusterID {
				t.Fatalf("recovery guard changed state identity: got %q, want %q", got, clusterID)
			}
		})
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

func TestUnitClusterReadRejectsWrongKind(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 1
	envID := unitEnv(t, srv, "lab-cl-unit-kind")
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	flex, err := c.CreateCluster(context.Background(), envID, client.ClusterSpec{Kind: "flex"})
	if err != nil {
		t.Fatalf("create flex cluster: %v", err)
	}

	r := newUnitClusterResource(t, srv, 5*time.Second) // business resource
	s := clusterSchema(t, r)
	state := tfsdk.State{Schema: s, Raw: clusterValue(t, s, map[string]tftypes.Value{
		"environment_id": tftypes.NewValue(tftypes.String, envID),
		"id":             tftypes.NewValue(tftypes.String, flex.ID),
	})}
	resp := resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, &resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("business resource must reject a flex cluster during Read/import refresh")
	}
	if !containsAll(resp.Diagnostics.Errors()[0].Detail(), "business", "flex") {
		t.Fatalf("wrong-kind diagnostic must name expected and actual kinds: %v", resp.Diagnostics)
	}
}

func TestUnitFlexClusterDefaultCreateTimeoutCoversBackendReadiness(t *testing.T) {
	r := NewFlexClusterResource().(*clusterResource)
	if r.cfg.defaultCreateTimeout < 25*time.Minute {
		t.Fatalf("flex create timeout = %s, want at least 25m", r.cfg.defaultCreateTimeout)
	}
}

func TestUnitClusterImportSetsCompositeIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  resource.Resource
	}{
		{name: "business", res: NewBusinessClusterResource()},
		{name: "flex", res: NewFlexClusterResource()},
		{name: "namespace", res: NewNamespaceResource()},
		{name: "dedicated", res: NewDedicatedClusterResource()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := tc.res.(*clusterResource)
			if !ok {
				t.Fatalf("unexpected resource type %T", tc.res)
			}
			importer, ok := tc.res.(resource.ResourceWithImportState)
			if !ok {
				t.Fatalf("%s cluster does not implement ResourceWithImportState", tc.name)
			}
			s := clusterSchema(t, r)
			objType := s.Type().TerraformType(context.Background()).(tftypes.Object)
			resp := resource.ImportStateResponse{
				State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(objType, nil)},
			}

			importer.ImportState(context.Background(), resource.ImportStateRequest{
				ID: "env-123/cluster-456",
			}, &resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("import diagnostics: %v", resp.Diagnostics)
			}
			if got := stateString(t, resp.State, "environment_id"); got != "env-123" {
				t.Fatalf("environment_id = %q, want env-123", got)
			}
			if got := stateString(t, resp.State, "id"); got != "cluster-456" {
				t.Fatalf("id = %q, want cluster-456", got)
			}
		})
	}
}

func TestUnitClusterImportRejectsInvalidCompositeIdentity(t *testing.T) {
	r := NewFlexClusterResource().(*clusterResource)
	s := clusterSchema(t, r)
	objType := s.Type().TerraformType(context.Background()).(tftypes.Object)

	for _, id := range []string{"", "cluster-only", "/cluster", "env/", "env/cluster/extra"} {
		t.Run(id, func(t *testing.T) {
			resp := resource.ImportStateResponse{
				State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(objType, nil)},
			}
			r.ImportState(context.Background(), resource.ImportStateRequest{ID: id}, &resp)
			if !resp.Diagnostics.HasError() {
				t.Fatalf("expected invalid import ID %q to fail", id)
			}
		})
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
