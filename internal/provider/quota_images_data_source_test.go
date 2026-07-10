package provider

// Unit tests for the fcs_quota and fcs_images data sources (Read invoked
// directly — no terraform CLI, no TF_ACC).

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
	"github.com/focusnetcloud/terraform-provider-fcs/internal/mockapi"
)

func runDataSourceRead(t *testing.T, d datasource.DataSourceWithConfigure, srv *mockapi.Server) datasource.ReadResponse {
	t.Helper()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	cfgResp := datasource.ConfigureResponse{}
	d.Configure(context.Background(), datasource.ConfigureRequest{ProviderData: c}, &cfgResp)
	if cfgResp.Diagnostics.HasError() {
		t.Fatalf("configure: %v", cfgResp.Diagnostics)
	}

	schemaResp := datasource.SchemaResponse{}
	d.Schema(context.Background(), datasource.SchemaRequest{}, &schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", schemaResp.Diagnostics)
	}
	s := schemaResp.Schema
	objType := s.Type().TerraformType(context.Background()).(tftypes.Object)
	resp := datasource.ReadResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(objType, nil)}}
	d.Read(context.Background(), datasource.ReadRequest{
		Config: tfsdk.Config{Schema: s, Raw: tftypes.NewValue(objType, nil)},
	}, &resp)
	return resp
}

func TestUnitQuotaDataSourceRead(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.QuotaMaxVMs = 7
	srv.QuotaMaxPublicIPs = 3

	// One environment + one VM (4 vCPU / 8 GiB) of live usage.
	c, _ := client.New(srv.URL, unitToken)
	env, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: "lab-ds-quota"})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	if _, err := c.CreateVm(context.Background(), env.ID,
		client.VmSpec{Image: "ubuntu-22.04", CPUCores: 4, MemoryGB: 8}); err != nil {
		t.Fatalf("create vm: %v", err)
	}

	resp := runDataSourceRead(t, NewQuotaDataSource().(*quotaDataSource), srv)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics)
	}
	checks := map[string]int64{
		"max_vms":           7,
		"max_public_ips":    3,
		"used_environments": 1,
		"used_vms":          1,
		"used_vcpu":         4,
		"used_ram_gb":       8,
	}
	for attr, want := range checks {
		var got int64
		if diags := resp.State.GetAttribute(context.Background(), path.Root(attr), &got); diags.HasError() {
			t.Fatalf("get %s: %v", attr, diags)
		}
		if got != want {
			t.Fatalf("%s: got %d, want %d", attr, got, want)
		}
	}
}

func TestUnitImagesDataSourceRead(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.Images = []mockapi.Image{
		{Name: "ubuntu-22.04", DisplayName: "Ubuntu 22.04 LTS", Source: "catalog"},
		{Name: "lab-base", Source: "env"},
	}

	resp := runDataSourceRead(t, NewImagesDataSource().(*imagesDataSource), srv)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics)
	}

	var model imagesDataSourceModel
	if diags := resp.State.Get(context.Background(), &model); diags.HasError() {
		t.Fatalf("state get: %v", diags)
	}
	if len(model.Images) != 2 {
		t.Fatalf("expected 2 images, got %d", len(model.Images))
	}
	first := model.Images[0]
	if first.Name.ValueString() != "ubuntu-22.04" || first.DisplayName.ValueString() != "Ubuntu 22.04 LTS" || first.Source.ValueString() != "catalog" {
		t.Fatalf("unexpected first image: %+v", first)
	}
	second := model.Images[1]
	if second.Name.ValueString() != "lab-base" || second.Source.ValueString() != "env" {
		t.Fatalf("unexpected second image: %+v", second)
	}
	if !second.DisplayName.IsNull() {
		t.Fatalf("expected null display_name for env image, got %v", second.DisplayName)
	}
}
