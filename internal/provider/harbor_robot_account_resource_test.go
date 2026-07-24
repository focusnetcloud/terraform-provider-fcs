package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	fwschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

const harborRobotID = "11111111-2222-4333-8444-555555555555"

func harborRobotSchema(t *testing.T, r *harborRobotAccountResource) fwschema.Schema {
	t.Helper()
	resp := resource.SchemaResponse{}
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %v", resp.Diagnostics)
	}
	return resp.Schema
}

func TestApplyHarborRobotPreservesCreatedAtWhenUpdateOmitsIt(t *testing.T) {
	model := harborRobotAccountResourceModel{
		CreatedAt: types.StringValue("2026-07-24T08:00:00Z"),
	}
	applyHarborRobotAccount(&client.HarborRobotAccount{
		ID:                harborRobotID,
		Name:              "pipeline",
		PermissionProfile: "pull",
		ExpiresInDays:     90,
	}, &model)
	if got := model.CreatedAt.ValueString(); got != "2026-07-24T08:00:00Z" {
		t.Fatalf("created_at changed to %q", got)
	}
}

func harborRobotObjectType(t *testing.T, schema fwschema.Schema) tftypes.Object {
	t.Helper()
	objectType, ok := schema.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("schema type is not an object")
	}
	return objectType
}

func harborRobotValue(objectType tftypes.Object, attrs map[string]tftypes.Value) tftypes.Value {
	full := map[string]tftypes.Value{
		"id":                 tftypes.NewValue(tftypes.String, nil),
		"name":               tftypes.NewValue(tftypes.String, nil),
		"description":        tftypes.NewValue(tftypes.String, nil),
		"permission_profile": tftypes.NewValue(tftypes.String, nil),
		"expires_in_days":    tftypes.NewValue(tftypes.Number, nil),
		"disabled":           tftypes.NewValue(tftypes.Bool, nil),
		"rotation_trigger":   tftypes.NewValue(tftypes.String, nil),
		"project":            tftypes.NewValue(tftypes.String, nil),
		"registry":           tftypes.NewValue(tftypes.String, nil),
		"username":           tftypes.NewValue(tftypes.String, nil),
		"secret":             tftypes.NewValue(tftypes.String, nil),
		"status":             tftypes.NewValue(tftypes.String, nil),
		"expires_at":         tftypes.NewValue(tftypes.String, nil),
		"created_at":         tftypes.NewValue(tftypes.String, nil),
		"updated_at":         tftypes.NewValue(tftypes.String, nil),
	}
	for name, value := range attrs {
		full[name] = value
	}
	return tftypes.NewValue(objectType, full)
}

func TestHarborRobotSchemaProtectsSecretAndRegistersResource(t *testing.T) {
	r := NewHarborRobotAccountResource().(*harborRobotAccountResource)
	schema := harborRobotSchema(t, r)
	secret := schema.Attributes["secret"]
	if !secret.IsSensitive() || !secret.IsComputed() {
		t.Fatal("secret must be Sensitive and Computed")
	}
	if !schema.Attributes["name"].IsRequired() {
		t.Fatal("name must be required")
	}

	p := &fcsProvider{}
	found := false
	for _, factory := range p.Resources(context.Background()) {
		candidate := factory()
		resp := resource.MetadataResponse{}
		candidate.Metadata(context.Background(), resource.MetadataRequest{
			ProviderTypeName: "fcs",
		}, &resp)
		if resp.TypeName == "fcs_harbor_robot_account" {
			found = true
		}
	}
	if !found {
		t.Fatal("fcs_harbor_robot_account is not registered")
	}
}

func TestHarborRobotImportSetsUUIDOnly(t *testing.T) {
	r := NewHarborRobotAccountResource().(*harborRobotAccountResource)
	if _, ok := any(r).(resource.ResourceWithImportState); !ok {
		t.Fatal("resource must implement ResourceWithImportState")
	}
	schema := harborRobotSchema(t, r)
	objectType := harborRobotObjectType(t, schema)
	resp := resource.ImportStateResponse{
		State: tfsdk.State{
			Schema: schema,
			Raw:    tftypes.NewValue(objectType, nil),
		},
	}

	r.ImportState(context.Background(), resource.ImportStateRequest{ID: harborRobotID}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("import diagnostics: %v", resp.Diagnostics)
	}
	var id string
	if diags := resp.State.GetAttribute(context.Background(), path.Root("id"), &id); diags.HasError() {
		t.Fatalf("get imported ID: %v", diags)
	}
	if id != harborRobotID {
		t.Fatalf("imported id = %q", id)
	}

	bad := resource.ImportStateResponse{
		State: tfsdk.State{
			Schema: schema,
			Raw:    tftypes.NewValue(objectType, nil),
		},
	}
	r.ImportState(context.Background(), resource.ImportStateRequest{ID: "not-a-uuid"}, &bad)
	if !bad.Diagnostics.HasError() {
		t.Fatal("invalid import ID must fail")
	}
}

func TestHarborRobotReadPreservesSecretInState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method %s", r.Method)
		}
		_, _ = w.Write([]byte(`{
			"id":"11111111-2222-4333-8444-555555555555",
			"name":"pipeline",
			"description":"CI",
			"permission_profile":"push_pull",
			"expires_in_days":90,
			"disabled":false,
			"status":"active",
			"project":"t-8012",
			"registry":"harbor.cloud.focusnet.de",
			"username":"robot$t-8012+fcs-pipeline-11111111",
			"expires_at":"2026-10-22T08:00:00Z",
			"created_at":"2026-07-24T08:00:00Z",
			"updated_at":"2026-07-24T08:00:00Z"
		}`))
	}))
	defer server.Close()
	apiClient, err := client.New(server.URL, "fcs_test")
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	r := &harborRobotAccountResource{client: apiClient}
	schema := harborRobotSchema(t, r)
	objectType := harborRobotObjectType(t, schema)
	raw := harborRobotValue(objectType, map[string]tftypes.Value{
		"id":                 tftypes.NewValue(tftypes.String, harborRobotID),
		"name":               tftypes.NewValue(tftypes.String, "pipeline"),
		"permission_profile": tftypes.NewValue(tftypes.String, "push_pull"),
		"expires_in_days":    tftypes.NewValue(tftypes.Number, int64(90)),
		"disabled":           tftypes.NewValue(tftypes.Bool, false),
		"secret":             tftypes.NewValue(tftypes.String, "preserve-me"),
	})
	state := tfsdk.State{Schema: schema, Raw: raw}
	resp := resource.ReadResponse{State: state}

	r.Read(context.Background(), resource.ReadRequest{State: state}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read diagnostics: %v", resp.Diagnostics)
	}
	var model harborRobotAccountResourceModel
	if diags := resp.State.Get(context.Background(), &model); diags.HasError() {
		t.Fatalf("state get: %v", diags)
	}
	if model.Secret.ValueString() != "preserve-me" {
		t.Fatalf("secret changed during Read: %q", model.Secret.ValueString())
	}
}

func TestHarborRobotCreateStoresOneTimeSecretAsSensitiveState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/registry/robot-accounts" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var payload client.HarborRobotAccountSpec
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if payload.Name != "pipeline" || payload.PermissionProfile != "push_pull" {
			t.Fatalf("unexpected payload: %#v", payload)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"id":"11111111-2222-4333-8444-555555555555",
			"name":"pipeline",
			"description":"CI",
			"permission_profile":"push_pull",
			"expires_in_days":90,
			"disabled":false,
			"status":"active",
			"project":"t-8012",
			"registry":"harbor.cloud.focusnet.de",
			"username":"robot$t-8012+fcs-pipeline-11111111",
			"secret":"one-time-secret"
		}`))
	}))
	defer server.Close()
	apiClient, err := client.New(server.URL, "fcs_test")
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	r := &harborRobotAccountResource{client: apiClient}
	schema := harborRobotSchema(t, r)
	objectType := harborRobotObjectType(t, schema)
	planRaw := harborRobotValue(objectType, map[string]tftypes.Value{
		"id":                 tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"name":               tftypes.NewValue(tftypes.String, "pipeline"),
		"description":        tftypes.NewValue(tftypes.String, "CI"),
		"permission_profile": tftypes.NewValue(tftypes.String, "push_pull"),
		"expires_in_days":    tftypes.NewValue(tftypes.Number, int64(90)),
		"disabled":           tftypes.NewValue(tftypes.Bool, false),
		"project":            tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"registry":           tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"username":           tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"secret":             tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"status":             tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"expires_at":         tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"created_at":         tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"updated_at":         tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
	})
	resp := resource.CreateResponse{
		State: tfsdk.State{Schema: schema, Raw: tftypes.NewValue(objectType, nil)},
	}

	r.Create(context.Background(), resource.CreateRequest{
		Plan:   tfsdk.Plan{Schema: schema, Raw: planRaw},
		Config: tfsdk.Config{Schema: schema, Raw: planRaw},
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create diagnostics: %v", resp.Diagnostics)
	}
	var model harborRobotAccountResourceModel
	if diags := resp.State.Get(context.Background(), &model); diags.HasError() {
		t.Fatalf("state get: %v", diags)
	}
	if model.Secret.ValueString() != "one-time-secret" {
		t.Fatalf("secret = %q", model.Secret.ValueString())
	}
	if model.Project.ValueString() != "t-8012" {
		t.Fatalf("project = %q", model.Project.ValueString())
	}
}

func TestHarborRobotUpdateRotatesOnlyWhenTriggerChanges(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPatch:
			_, _ = w.Write([]byte(`{
				"id":"11111111-2222-4333-8444-555555555555",
				"name":"pipeline",
				"description":"Deploy",
				"permission_profile":"pull",
				"expires_in_days":30,
				"disabled":true,
				"status":"active",
				"project":"t-8012",
				"registry":"harbor.cloud.focusnet.de",
				"username":"robot$t-8012+fcs-pipeline-11111111"
			}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/registry/robot-accounts/"+harborRobotID+"/rotations":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{
				"id":"11111111-2222-4333-8444-555555555555",
				"name":"pipeline",
				"description":"Deploy",
				"permission_profile":"pull",
				"expires_in_days":30,
				"disabled":true,
				"status":"active",
				"project":"t-8012",
				"registry":"harbor.cloud.focusnet.de",
				"username":"robot$t-8012+fcs-pipeline-11111111",
				"secret":"rotated-secret"
			}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	apiClient, err := client.New(server.URL, "fcs_test")
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	r := &harborRobotAccountResource{client: apiClient}
	schema := harborRobotSchema(t, r)
	objectType := harborRobotObjectType(t, schema)
	stateRaw := harborRobotValue(objectType, map[string]tftypes.Value{
		"id":                 tftypes.NewValue(tftypes.String, harborRobotID),
		"name":               tftypes.NewValue(tftypes.String, "pipeline"),
		"description":        tftypes.NewValue(tftypes.String, "CI"),
		"permission_profile": tftypes.NewValue(tftypes.String, "push_pull"),
		"expires_in_days":    tftypes.NewValue(tftypes.Number, int64(90)),
		"disabled":           tftypes.NewValue(tftypes.Bool, false),
		"rotation_trigger":   tftypes.NewValue(tftypes.String, "v1"),
		"project":            tftypes.NewValue(tftypes.String, "t-8012"),
		"registry":           tftypes.NewValue(tftypes.String, "harbor.cloud.focusnet.de"),
		"username":           tftypes.NewValue(tftypes.String, "robot$t-8012+fcs-pipeline-11111111"),
		"secret":             tftypes.NewValue(tftypes.String, "old-secret"),
		"status":             tftypes.NewValue(tftypes.String, "active"),
	})
	planRaw := harborRobotValue(objectType, map[string]tftypes.Value{
		"id":                 tftypes.NewValue(tftypes.String, harborRobotID),
		"name":               tftypes.NewValue(tftypes.String, "pipeline"),
		"description":        tftypes.NewValue(tftypes.String, "Deploy"),
		"permission_profile": tftypes.NewValue(tftypes.String, "pull"),
		"expires_in_days":    tftypes.NewValue(tftypes.Number, int64(30)),
		"disabled":           tftypes.NewValue(tftypes.Bool, true),
		"rotation_trigger":   tftypes.NewValue(tftypes.String, "v2"),
		"project":            tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"registry":           tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"username":           tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"secret":             tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"status":             tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
	})
	state := tfsdk.State{Schema: schema, Raw: stateRaw}
	resp := resource.UpdateResponse{State: state}

	r.Update(context.Background(), resource.UpdateRequest{
		State:  state,
		Plan:   tfsdk.Plan{Schema: schema, Raw: planRaw},
		Config: tfsdk.Config{Schema: schema, Raw: planRaw},
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update diagnostics: %v", resp.Diagnostics)
	}
	var model harborRobotAccountResourceModel
	if diags := resp.State.Get(context.Background(), &model); diags.HasError() {
		t.Fatalf("state get: %v", diags)
	}
	if model.Secret.ValueString() != "rotated-secret" {
		t.Fatalf("secret = %q", model.Secret.ValueString())
	}
	if model.RotationTrigger.ValueString() != "v2" {
		t.Fatalf("rotation_trigger = %q", model.RotationTrigger.ValueString())
	}
	if len(calls) != 2 {
		t.Fatalf("expected metadata update plus one rotation, got %v", calls)
	}
}
