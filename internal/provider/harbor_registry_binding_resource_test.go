package provider

import (
	"context"
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

const harborRegistryBindingID = "33333333-4444-4555-8666-777777777777"

func harborRegistryBindingSchema(
	t *testing.T,
	r *harborRegistryBindingResource,
) fwschema.Schema {
	t.Helper()
	resp := resource.SchemaResponse{}
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %v", resp.Diagnostics)
	}
	return resp.Schema
}

func TestApplyHarborBindingPreservesCreatedAtWhenUpdateOmitsIt(t *testing.T) {
	model := harborRegistryBindingResourceModel{
		CreatedAt: types.StringValue("2026-07-24T08:00:00Z"),
	}
	applyHarborRegistryBinding(&client.HarborRegistryBinding{
		ID:              harborRegistryBindingID,
		EnvironmentID:   "11111111-2222-4333-8444-555555555555",
		ClusterID:       "22222222-3333-4444-8555-666666666666",
		Namespace:       "applications",
		SecretName:      "fcs-harbor-pull",
		ServiceAccounts: []string{"default"},
	}, &model)
	if got := model.CreatedAt.ValueString(); got != "2026-07-24T08:00:00Z" {
		t.Fatalf("created_at changed to %q", got)
	}
}

func stringSet(values ...string) tftypes.Value {
	items := make([]tftypes.Value, 0, len(values))
	for _, value := range values {
		items = append(items, tftypes.NewValue(tftypes.String, value))
	}
	return tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, items)
}

func harborRegistryBindingValue(
	objectType tftypes.Object,
	attrs map[string]tftypes.Value,
) tftypes.Value {
	full := map[string]tftypes.Value{
		"id":               tftypes.NewValue(tftypes.String, nil),
		"environment_id":   tftypes.NewValue(tftypes.String, nil),
		"cluster_id":       tftypes.NewValue(tftypes.String, nil),
		"namespace":        tftypes.NewValue(tftypes.String, nil),
		"secret_name":      tftypes.NewValue(tftypes.String, nil),
		"service_accounts": tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, nil),
		"rotation_trigger": tftypes.NewValue(tftypes.String, nil),
		"project":          tftypes.NewValue(tftypes.String, nil),
		"registry":         tftypes.NewValue(tftypes.String, nil),
		"status":           tftypes.NewValue(tftypes.String, nil),
		"created_at":       tftypes.NewValue(tftypes.String, nil),
		"updated_at":       tftypes.NewValue(tftypes.String, nil),
	}
	for name, value := range attrs {
		full[name] = value
	}
	return tftypes.NewValue(objectType, full)
}

func TestHarborRegistryBindingSchemaHasNoCredentialStateAndRegistersResource(t *testing.T) {
	r := NewHarborRegistryBindingResource().(*harborRegistryBindingResource)
	schema := harborRegistryBindingSchema(t, r)
	for _, forbidden := range []string{"secret", "password", "username"} {
		if _, exists := schema.Attributes[forbidden]; exists {
			t.Fatalf("credential attribute %q must not exist", forbidden)
		}
	}
	for _, name := range []string{"environment_id", "cluster_id", "namespace"} {
		if !schema.Attributes[name].IsRequired() {
			t.Fatalf("%s must be required", name)
		}
	}
	var provider fcsProvider
	found := false
	for _, factory := range provider.Resources(context.Background()) {
		if _, ok := factory().(*harborRegistryBindingResource); ok {
			found = true
		}
	}
	if !found {
		t.Fatal("fcs_harbor_registry_binding is not registered")
	}
}

func TestHarborRegistryBindingImportValidatesUUID(t *testing.T) {
	r := &harborRegistryBindingResource{}
	schema := harborRegistryBindingSchema(t, r)
	valid := resource.ImportStateResponse{
		State: tfsdk.State{
			Schema: schema,
			Raw: tftypes.NewValue(
				schema.Type().TerraformType(context.Background()),
				nil,
			),
		},
	}
	r.ImportState(
		context.Background(),
		resource.ImportStateRequest{ID: harborRegistryBindingID},
		&valid,
	)
	if valid.Diagnostics.HasError() {
		t.Fatalf("valid import: %v", valid.Diagnostics)
	}
	var id string
	if diags := valid.State.GetAttribute(
		context.Background(),
		path.Root("id"),
		&id,
	); diags.HasError() {
		t.Fatalf("get imported id: %v", diags)
	}
	if id != harborRegistryBindingID {
		t.Fatalf("imported id = %q", id)
	}

	invalid := resource.ImportStateResponse{}
	r.ImportState(
		context.Background(),
		resource.ImportStateRequest{ID: "not-a-uuid"},
		&invalid,
	)
	if !invalid.Diagnostics.HasError() {
		t.Fatal("invalid import ID must fail")
	}
}

func TestHarborRegistryBindingCreateStoresOnlyMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/registry/bindings" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"id":"33333333-4444-4555-8666-777777777777",
			"environment_id":"11111111-2222-4333-8444-555555555555",
			"cluster_id":"22222222-3333-4444-8555-666666666666",
			"namespace":"applications",
			"secret_name":"fcs-harbor-pull",
			"service_accounts":["default"],
			"project":"t-8012",
			"registry":"harbor.cloud.focusnet.de",
			"status":"active"
		}`))
	}))
	defer server.Close()
	apiClient, err := client.New(server.URL, "fcs_test")
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	r := &harborRegistryBindingResource{client: apiClient}
	schema := harborRegistryBindingSchema(t, r)
	objectType := schema.Type().TerraformType(context.Background()).(tftypes.Object)
	planRaw := harborRegistryBindingValue(objectType, map[string]tftypes.Value{
		"environment_id":   tftypes.NewValue(tftypes.String, "11111111-2222-4333-8444-555555555555"),
		"cluster_id":       tftypes.NewValue(tftypes.String, "22222222-3333-4444-8555-666666666666"),
		"namespace":        tftypes.NewValue(tftypes.String, "applications"),
		"secret_name":      tftypes.NewValue(tftypes.String, "fcs-harbor-pull"),
		"service_accounts": stringSet("default"),
		"id":               tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"project":          tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"registry":         tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"status":           tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"created_at":       tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"updated_at":       tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
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
	var model harborRegistryBindingResourceModel
	if diags := resp.State.Get(context.Background(), &model); diags.HasError() {
		t.Fatalf("state get: %v", diags)
	}
	if model.Project.ValueString() != "t-8012" ||
		model.Registry.ValueString() != "harbor.cloud.focusnet.de" {
		t.Fatalf("unexpected state: %#v", model)
	}
}

func TestHarborRegistryBindingUpdateRotatesOnlyWhenTriggerChanges(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPatch:
			_, _ = w.Write([]byte(`{
				"id":"33333333-4444-4555-8666-777777777777",
				"environment_id":"11111111-2222-4333-8444-555555555555",
				"cluster_id":"22222222-3333-4444-8555-666666666666",
				"namespace":"applications",
				"secret_name":"fcs-harbor-pull",
				"service_accounts":["default","builder"],
				"project":"t-8012",
				"registry":"harbor.cloud.focusnet.de",
				"status":"active"
			}`))
		case r.Method == http.MethodPost &&
			r.URL.Path == "/v1/registry/bindings/"+harborRegistryBindingID+"/rotations":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{
				"id":"33333333-4444-4555-8666-777777777777",
				"environment_id":"11111111-2222-4333-8444-555555555555",
				"cluster_id":"22222222-3333-4444-8555-666666666666",
				"namespace":"applications",
				"secret_name":"fcs-harbor-pull",
				"service_accounts":["default","builder"],
				"project":"t-8012",
				"registry":"harbor.cloud.focusnet.de",
				"status":"active"
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
	r := &harborRegistryBindingResource{client: apiClient}
	schema := harborRegistryBindingSchema(t, r)
	objectType := schema.Type().TerraformType(context.Background()).(tftypes.Object)
	base := map[string]tftypes.Value{
		"id":               tftypes.NewValue(tftypes.String, harborRegistryBindingID),
		"environment_id":   tftypes.NewValue(tftypes.String, "11111111-2222-4333-8444-555555555555"),
		"cluster_id":       tftypes.NewValue(tftypes.String, "22222222-3333-4444-8555-666666666666"),
		"namespace":        tftypes.NewValue(tftypes.String, "applications"),
		"secret_name":      tftypes.NewValue(tftypes.String, "fcs-harbor-pull"),
		"project":          tftypes.NewValue(tftypes.String, "t-8012"),
		"registry":         tftypes.NewValue(tftypes.String, "harbor.cloud.focusnet.de"),
		"status":           tftypes.NewValue(tftypes.String, "active"),
		"rotation_trigger": tftypes.NewValue(tftypes.String, "v1"),
		"service_accounts": stringSet("default"),
	}
	stateRaw := harborRegistryBindingValue(objectType, base)
	planAttrs := make(map[string]tftypes.Value, len(base))
	for key, value := range base {
		planAttrs[key] = value
	}
	planAttrs["rotation_trigger"] = tftypes.NewValue(tftypes.String, "v2")
	planAttrs["service_accounts"] = stringSet("default", "builder")
	planAttrs["status"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	planAttrs["updated_at"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	planRaw := harborRegistryBindingValue(objectType, planAttrs)
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
	if len(calls) != 2 ||
		calls[0] != "PATCH /v1/registry/bindings/"+harborRegistryBindingID ||
		calls[1] != "POST /v1/registry/bindings/"+harborRegistryBindingID+"/rotations" {
		t.Fatalf("calls = %v", calls)
	}
}
