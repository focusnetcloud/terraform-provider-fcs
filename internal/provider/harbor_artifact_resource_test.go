package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	fwschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

const harborArtifactID = "66666666-7777-4888-8999-aaaaaaaaaaaa"

func harborArtifactSchema(t *testing.T, r *harborArtifactResource) fwschema.Schema {
	t.Helper()
	resp := resource.SchemaResponse{}
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %v", resp.Diagnostics)
	}
	return resp.Schema
}

func harborArtifactValue(objectType tftypes.Object, attrs map[string]tftypes.Value) tftypes.Value {
	full := map[string]tftypes.Value{
		"id":                  tftypes.NewValue(tftypes.String, nil),
		"repository":          tftypes.NewValue(tftypes.String, nil),
		"reference":           tftypes.NewValue(tftypes.String, nil),
		"kind":                tftypes.NewValue(tftypes.String, nil),
		"project":             tftypes.NewValue(tftypes.String, nil),
		"registry":            tftypes.NewValue(tftypes.String, nil),
		"digest":              tftypes.NewValue(tftypes.String, nil),
		"canonical_reference": tftypes.NewValue(tftypes.String, nil),
		"media_type":          tftypes.NewValue(tftypes.String, nil),
		"size_bytes":          tftypes.NewValue(tftypes.Number, nil),
		"status":              tftypes.NewValue(tftypes.String, nil),
		"created_at":          tftypes.NewValue(tftypes.String, nil),
		"updated_at":          tftypes.NewValue(tftypes.String, nil),
	}
	for name, value := range attrs {
		full[name] = value
	}
	return tftypes.NewValue(objectType, full)
}

func TestHarborArtifactSchemaRegistersForceNewResource(t *testing.T) {
	r := NewHarborArtifactResource().(*harborArtifactResource)
	schema := harborArtifactSchema(t, r)
	for _, name := range []string{"repository", "reference", "kind"} {
		if !schema.Attributes[name].IsRequired() {
			t.Fatalf("%s must be required", name)
		}
	}
	var provider fcsProvider
	found := false
	for _, factory := range provider.Resources(context.Background()) {
		if _, ok := factory().(*harborArtifactResource); ok {
			found = true
		}
	}
	if !found {
		t.Fatal("fcs_harbor_artifact is not registered")
	}
}

func TestHarborArtifactImportValidatesUUID(t *testing.T) {
	r := &harborArtifactResource{}
	valid := resource.ImportStateResponse{
		State: tfsdk.State{
			Schema: harborArtifactSchema(t, r),
			Raw: tftypes.NewValue(
				harborArtifactSchema(t, r).Type().TerraformType(context.Background()),
				nil,
			),
		},
	}
	r.ImportState(context.Background(), resource.ImportStateRequest{ID: harborArtifactID}, &valid)
	if valid.Diagnostics.HasError() {
		t.Fatalf("valid import: %v", valid.Diagnostics)
	}
	var id string
	if diags := valid.State.GetAttribute(context.Background(), path.Root("id"), &id); diags.HasError() {
		t.Fatalf("get imported id: %v", diags)
	}
	if id != harborArtifactID {
		t.Fatalf("imported id = %q", id)
	}

	invalid := resource.ImportStateResponse{}
	r.ImportState(context.Background(), resource.ImportStateRequest{ID: "not-a-uuid"}, &invalid)
	if !invalid.Diagnostics.HasError() {
		t.Fatal("invalid import ID must fail")
	}
}

func TestHarborArtifactCreateStoresResolvedDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/registry/artifacts" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{
			"id":%q,
			"repository":"apps/web",
			"reference":"latest",
			"kind":"container",
			"project":"t-8012",
			"registry":"harbor.cloud.focusnet.de",
			"digest":%q,
			"canonical_reference":%q,
			"media_type":"application/vnd.oci.image.manifest.v1+json",
			"size_bytes":1234,
			"status":"active"
		}`, harborArtifactID, digest, "harbor.cloud.focusnet.de/t-8012/apps/web@"+digest)
	}))
	defer server.Close()
	apiClient, err := client.New(server.URL, "fcs_test")
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	r := &harborArtifactResource{client: apiClient}
	schema := harborArtifactSchema(t, r)
	objectType := schema.Type().TerraformType(context.Background()).(tftypes.Object)
	planRaw := harborArtifactValue(objectType, map[string]tftypes.Value{
		"repository":          tftypes.NewValue(tftypes.String, "apps/web"),
		"reference":           tftypes.NewValue(tftypes.String, "latest"),
		"kind":                tftypes.NewValue(tftypes.String, "container"),
		"id":                  tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"project":             tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"registry":            tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"digest":              tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"canonical_reference": tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"media_type":          tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"size_bytes":          tftypes.NewValue(tftypes.Number, tftypes.UnknownValue),
		"status":              tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"created_at":          tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"updated_at":          tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
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
	var model harborArtifactResourceModel
	if diags := resp.State.Get(context.Background(), &model); diags.HasError() {
		t.Fatalf("state get: %v", diags)
	}
	if model.Digest.ValueString() != digest ||
		model.CanonicalReference.ValueString() !=
			"harbor.cloud.focusnet.de/t-8012/apps/web@"+digest {
		t.Fatalf("unexpected state: %#v", model)
	}
}
