package provider

// Unit tests for the fcs_environment CRUD logic. They invoke the resource
// handlers directly (no terraform CLI, no TF_ACC) against the contract mock.

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	fwschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
	"github.com/focusnetcloud/terraform-provider-fcs/internal/mockapi"
)

const unitToken = "fcs_unit_token"

func newUnitResource(t *testing.T, srv *mockapi.Server) *environmentResource {
	t.Helper()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return &environmentResource{client: c}
}

func environmentSchema(t *testing.T) fwschema.Schema {
	t.Helper()
	r := &environmentResource{}
	resp := resource.SchemaResponse{}
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %v", resp.Diagnostics)
	}
	return resp.Schema
}

func environmentObjectType(t *testing.T, s fwschema.Schema) tftypes.Object {
	t.Helper()
	objType, ok := s.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("schema type is not an object")
	}
	return objType
}

// environmentValue builds a full attribute map; nil entries become null,
// the sentinel unknown becomes unknown.
func environmentValue(objType tftypes.Object, attrs map[string]tftypes.Value) tftypes.Value {
	full := map[string]tftypes.Value{
		"id":             tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"name":           tftypes.NewValue(tftypes.String, nil),
		"ttl_seconds":    tftypes.NewValue(tftypes.Number, nil),
		"labels":         tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, nil),
		"status":         tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"ttl_expires_at": tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"created_at":     tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
	}
	for k, v := range attrs {
		full[k] = v
	}
	return tftypes.NewValue(objType, full)
}

func runCreate(t *testing.T, r *environmentResource, s fwschema.Schema, planRaw tftypes.Value) resource.CreateResponse {
	t.Helper()
	objType := environmentObjectType(t, s)
	req := resource.CreateRequest{
		Plan:   tfsdk.Plan{Schema: s, Raw: planRaw},
		Config: tfsdk.Config{Schema: s, Raw: planRaw},
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(objType, nil)}}
	r.Create(context.Background(), req, &resp)
	return resp
}

func TestUnitCreateNewEnvironment(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	r := newUnitResource(t, srv)
	s := environmentSchema(t)
	objType := environmentObjectType(t, s)

	plan := environmentValue(objType, map[string]tftypes.Value{
		"name":        tftypes.NewValue(tftypes.String, "lab-unit"),
		"ttl_seconds": tftypes.NewValue(tftypes.Number, int64(3600)),
		"labels": tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, map[string]tftypes.Value{
			"workshop": tftypes.NewValue(tftypes.String, "k8s"),
		}),
	})
	resp := runCreate(t, r, s, plan)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create diagnostics: %v", resp.Diagnostics)
	}

	var state environmentResourceModel
	if diags := resp.State.Get(context.Background(), &state); diags.HasError() {
		t.Fatalf("state get: %v", diags)
	}
	if state.ID.ValueString() == "" {
		t.Fatal("expected computed id in state")
	}
	if state.Name.ValueString() != "lab-unit" {
		t.Fatalf("unexpected name: %s", state.Name.ValueString())
	}
	if state.TTLSeconds.ValueInt64() != 3600 {
		t.Fatalf("ttl_seconds must stay at the planned value, got %d", state.TTLSeconds.ValueInt64())
	}
	// Environments are provisioned synchronously: active right away.
	if state.Status.ValueString() != "active" {
		t.Fatalf("expected status active after post-create read, got %q", state.Status.ValueString())
	}
	if state.TTLExpiresAt.IsNull() || state.CreatedAt.IsNull() {
		t.Fatal("expected ttl_expires_at and created_at to be set")
	}
	if srv.EnvironmentCount() != 1 {
		t.Fatalf("expected 1 environment on the server, got %d", srv.EnvironmentCount())
	}
}

func TestUnitCreatePersistentNoTTL(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	r := newUnitResource(t, srv)
	s := environmentSchema(t)
	objType := environmentObjectType(t, s)

	// ttl_seconds OMITTED -> environmentValue defaults it to null -> PERSISTENT.
	plan := environmentValue(objType, map[string]tftypes.Value{
		"name": tftypes.NewValue(tftypes.String, "lab-persist"),
	})
	resp := runCreate(t, r, s, plan)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create diagnostics: %v", resp.Diagnostics)
	}

	var state environmentResourceModel
	if diags := resp.State.Get(context.Background(), &state); diags.HasError() {
		t.Fatalf("state get: %v", diags)
	}
	// The provider must NOT invent a default TTL; persistent stays null.
	if !state.TTLSeconds.IsNull() {
		t.Fatalf("persistent env must keep ttl_seconds null, got %d", state.TTLSeconds.ValueInt64())
	}
	// Persistent => server returns no expiry => state reads null (not "").
	if !state.TTLExpiresAt.IsNull() {
		t.Fatalf("persistent env must have null ttl_expires_at, got %q", state.TTLExpiresAt.ValueString())
	}
	if state.Status.ValueString() != "active" {
		t.Fatalf("expected status active, got %q", state.Status.ValueString())
	}
}

func TestUnitCreateIdempotentReapply(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	r := newUnitResource(t, srv)
	s := environmentSchema(t)
	objType := environmentObjectType(t, s)

	plan := environmentValue(objType, map[string]tftypes.Value{
		"name":        tftypes.NewValue(tftypes.String, "lab-idem"),
		"ttl_seconds": tftypes.NewValue(tftypes.Number, int64(14400)),
	})

	first := runCreate(t, r, s, plan)
	if first.Diagnostics.HasError() {
		t.Fatalf("first create: %v", first.Diagnostics)
	}
	second := runCreate(t, r, s, plan) // HTTP 200 path
	if second.Diagnostics.HasError() {
		t.Fatalf("idempotent re-apply (HTTP 200) must succeed: %v", second.Diagnostics)
	}

	var s1, s2 environmentResourceModel
	_ = first.State.Get(context.Background(), &s1)
	_ = second.State.Get(context.Background(), &s2)
	if s1.ID.ValueString() != s2.ID.ValueString() {
		t.Fatalf("idempotent create must return the same id: %s vs %s", s1.ID.ValueString(), s2.ID.ValueString())
	}
	if srv.EnvironmentCount() != 1 {
		t.Fatalf("expected exactly 1 environment, got %d", srv.EnvironmentCount())
	}
}

func TestUnitCreateQuotaExceeded(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.MaxEnvs = 1
	r := newUnitResource(t, srv)
	s := environmentSchema(t)
	objType := environmentObjectType(t, s)

	ok := runCreate(t, r, s, environmentValue(objType, map[string]tftypes.Value{
		"name":        tftypes.NewValue(tftypes.String, "lab-q1"),
		"ttl_seconds": tftypes.NewValue(tftypes.Number, int64(14400)),
	}))
	if ok.Diagnostics.HasError() {
		t.Fatalf("first create: %v", ok.Diagnostics)
	}

	over := runCreate(t, r, s, environmentValue(objType, map[string]tftypes.Value{
		"name":        tftypes.NewValue(tftypes.String, "lab-q2"),
		"ttl_seconds": tftypes.NewValue(tftypes.Number, int64(14400)),
	}))
	if !over.Diagnostics.HasError() {
		t.Fatal("expected quota error diagnostics")
	}
	found := false
	for _, d := range over.Diagnostics.Errors() {
		if containsAll(d.Detail(), "409", "QuotaExceeded") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 409/QuotaExceeded in diagnostics, got: %v", over.Diagnostics)
	}
}

func TestUnitReadRefreshesAndRemovesOn404(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	r := newUnitResource(t, srv)
	s := environmentSchema(t)
	objType := environmentObjectType(t, s)

	created := runCreate(t, r, s, environmentValue(objType, map[string]tftypes.Value{
		"name":        tftypes.NewValue(tftypes.String, "lab-read"),
		"ttl_seconds": tftypes.NewValue(tftypes.Number, int64(14400)),
	}))
	if created.Diagnostics.HasError() {
		t.Fatalf("create: %v", created.Diagnostics)
	}

	readReq := resource.ReadRequest{State: created.State}
	readResp := resource.ReadResponse{State: created.State}
	r.Read(context.Background(), readReq, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read: %v", readResp.Diagnostics)
	}
	var state environmentResourceModel
	_ = readResp.State.Get(context.Background(), &state)
	if state.Status.ValueString() != "active" {
		t.Fatalf("expected refreshed status active, got %q", state.Status.ValueString())
	}

	// Simulate a completed reap (hard 404), then Read must drop the
	// resource from state.
	if !srv.DeleteByName("lab-read") {
		t.Fatal("mock delete failed")
	}
	goneResp := resource.ReadResponse{State: readResp.State}
	r.Read(context.Background(), resource.ReadRequest{State: readResp.State}, &goneResp)
	if goneResp.Diagnostics.HasError() {
		t.Fatalf("read after 404 must not error: %v", goneResp.Diagnostics)
	}
	if !goneResp.State.Raw.IsNull() {
		t.Fatal("expected resource to be removed from state after 404")
	}
}

// TestUnitReadRemovesOnDestroyed: the server may keep destroyed rows
// readable (status=destroyed) — Read must treat them like a 404 and drop
// the resource so Terraform plans a re-create instead of drifting.
func TestUnitReadRemovesOnDestroyed(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.EnvGoneAfterGETs = 1 << 30 // destroyed row stays readable (like the server DB)
	r := newUnitResource(t, srv)
	s := environmentSchema(t)
	objType := environmentObjectType(t, s)

	created := runCreate(t, r, s, environmentValue(objType, map[string]tftypes.Value{
		"name":        tftypes.NewValue(tftypes.String, "lab-destroyed"),
		"ttl_seconds": tftypes.NewValue(tftypes.Number, int64(14400)),
	}))
	if created.Diagnostics.HasError() {
		t.Fatalf("create: %v", created.Diagnostics)
	}

	// Out-of-band soft-destroy (TTL reaper): GET returns status=destroyed.
	if !srv.DestroyByName("lab-destroyed") {
		t.Fatal("mock destroy failed")
	}
	readResp := resource.ReadResponse{State: created.State}
	r.Read(context.Background(), resource.ReadRequest{State: created.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read on a destroyed environment must not error: %v", readResp.Diagnostics)
	}
	if !readResp.State.Raw.IsNull() {
		t.Fatal("expected resource to be removed from state for a destroyed environment")
	}
}

func TestUnitDeleteToleratesReaper(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	r := newUnitResource(t, srv)
	s := environmentSchema(t)
	objType := environmentObjectType(t, s)

	created := runCreate(t, r, s, environmentValue(objType, map[string]tftypes.Value{
		"name":        tftypes.NewValue(tftypes.String, "lab-del"),
		"ttl_seconds": tftypes.NewValue(tftypes.Number, int64(14400)),
	}))
	if created.Diagnostics.HasError() {
		t.Fatalf("create: %v", created.Diagnostics)
	}

	delResp := resource.DeleteResponse{}
	r.Delete(context.Background(), resource.DeleteRequest{State: created.State}, &delResp)
	if delResp.Diagnostics.HasError() {
		t.Fatalf("delete (202): %v", delResp.Diagnostics)
	}
	if srv.EnvironmentCount() != 0 {
		t.Fatalf("expected 0 environments after delete, got %d", srv.EnvironmentCount())
	}

	// Second delete: already destroyed (or the reaper was faster) — the
	// idempotent 202/404 is tolerated either way.
	againResp := resource.DeleteResponse{}
	r.Delete(context.Background(), resource.DeleteRequest{State: created.State}, &againResp)
	if againResp.Diagnostics.HasError() {
		t.Fatalf("delete must tolerate 404: %v", againResp.Diagnostics)
	}
}

func TestUnitUpdateIsUnreachableGuard(t *testing.T) {
	r := &environmentResource{}
	resp := resource.UpdateResponse{}
	r.Update(context.Background(), resource.UpdateRequest{}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("Update must fail loudly: all attributes are RequiresReplace")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
