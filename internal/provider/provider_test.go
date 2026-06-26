package provider

// Unit tests for provider Configure (token/env fallback, validation) —
// no terraform CLI, no TF_ACC.

import (
	"context"
	"testing"

	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

func providerConfigValue(t *testing.T, endpoint, token tftypes.Value) tfsdk.Config {
	t.Helper()
	p := &fcsProvider{version: "test"}
	schemaResp := fwprovider.SchemaResponse{}
	p.Schema(context.Background(), fwprovider.SchemaRequest{}, &schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("provider schema: %v", schemaResp.Diagnostics)
	}
	s := schemaResp.Schema
	objType, ok := s.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("provider schema type is not an object")
	}
	raw := tftypes.NewValue(objType, map[string]tftypes.Value{
		"endpoint": endpoint,
		"token":    token,
	})
	return tfsdk.Config{Schema: s, Raw: raw}
}

func configure(t *testing.T, cfg tfsdk.Config) fwprovider.ConfigureResponse {
	t.Helper()
	p := &fcsProvider{version: "test"}
	resp := fwprovider.ConfigureResponse{}
	p.Configure(context.Background(), fwprovider.ConfigureRequest{Config: cfg}, &resp)
	return resp
}

func TestConfigureWithExplicitToken(t *testing.T) {
	t.Setenv(tokenEnvVar, "") // must not be needed
	cfg := providerConfigValue(t,
		tftypes.NewValue(tftypes.String, "https://api.focusnet.de"),
		tftypes.NewValue(tftypes.String, "fcs_explicit"),
	)
	resp := configure(t, cfg)
	if resp.Diagnostics.HasError() {
		t.Fatalf("configure: %v", resp.Diagnostics)
	}
	if _, ok := resp.ResourceData.(*client.Client); !ok {
		t.Fatalf("expected *client.Client as ResourceData, got %T", resp.ResourceData)
	}
}

func TestConfigureTokenFromEnv(t *testing.T) {
	t.Setenv(tokenEnvVar, "fcs_from_env")
	cfg := providerConfigValue(t,
		tftypes.NewValue(tftypes.String, "https://api.focusnet.de"),
		tftypes.NewValue(tftypes.String, nil), // token unset in config
	)
	resp := configure(t, cfg)
	if resp.Diagnostics.HasError() {
		t.Fatalf("configure with %s must succeed: %v", tokenEnvVar, resp.Diagnostics)
	}
}

func TestConfigureMissingTokenFails(t *testing.T) {
	t.Setenv(tokenEnvVar, "")
	cfg := providerConfigValue(t,
		tftypes.NewValue(tftypes.String, "https://api.focusnet.de"),
		tftypes.NewValue(tftypes.String, nil),
	)
	resp := configure(t, cfg)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected error when neither token attribute nor env var is set")
	}
}

func TestConfigureInvalidEndpointFails(t *testing.T) {
	cfg := providerConfigValue(t,
		tftypes.NewValue(tftypes.String, "not-a-url"),
		tftypes.NewValue(tftypes.String, "fcs_x"),
	)
	resp := configure(t, cfg)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected error for invalid endpoint URL")
	}
}
