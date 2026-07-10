package provider

// fcs_quota: tenant usage vs. limits via GET /v1/quota. Read-only; all fields
// are server-computed.

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

var (
	_ datasource.DataSource              = (*quotaDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*quotaDataSource)(nil)
)

// NewQuotaDataSource returns the fcs_quota data source.
func NewQuotaDataSource() datasource.DataSource {
	return &quotaDataSource{}
}

type quotaDataSource struct {
	client *client.Client
}

type quotaDataSourceModel struct {
	MaxConcurrentEnvironments types.Int64 `tfsdk:"max_concurrent_environments"`
	UsedEnvironments          types.Int64 `tfsdk:"used_environments"`
	MaxVMs                    types.Int64 `tfsdk:"max_vms"`
	UsedVMs                   types.Int64 `tfsdk:"used_vms"`
	MaxVCPU                   types.Int64 `tfsdk:"max_vcpu"`
	UsedVCPU                  types.Int64 `tfsdk:"used_vcpu"`
	MaxRAMGB                  types.Int64 `tfsdk:"max_ram_gb"`
	UsedRAMGB                 types.Int64 `tfsdk:"used_ram_gb"`
	MaxPublicIPs              types.Int64 `tfsdk:"max_public_ips"`
	UsedPublicIPs             types.Int64 `tfsdk:"used_public_ips"`
}

func (d *quotaDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_quota"
}

func (d *quotaDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	intAttr := func(desc string) schema.Int64Attribute {
		return schema.Int64Attribute{Computed: true, Description: desc}
	}
	resp.Schema = schema.Schema{
		Description: "Tenant usage vs. limits (GET /v1/quota). Limits are enforced server-side " +
			"on every create; vCPU/RAM usage counts clusters and VMs together.",
		Attributes: map[string]schema.Attribute{
			"max_concurrent_environments": intAttr("Maximum number of concurrently active environments."),
			"used_environments":           intAttr("Currently active environments."),
			"max_vms":                     intAttr("Maximum number of non-terminal VMs."),
			"used_vms":                    intAttr("Currently provisioned (non-terminal) VMs."),
			"max_vcpu":                    intAttr("vCPU cap across clusters and VMs."),
			"used_vcpu":                   intAttr("vCPUs in use across clusters and VMs."),
			"max_ram_gb":                  intAttr("RAM cap in GiB across clusters and VMs."),
			"used_ram_gb":                 intAttr("RAM in use (GiB) across clusters and VMs."),
			"max_public_ips":              intAttr("Public IP cap for the tenant."),
			"used_public_ips":             intAttr("Public IPs bound by environment resources."),
		},
	}
}

func (d *quotaDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return // provider not configured yet (e.g. during validate)
	}
	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected data source configure type",
			fmt.Sprintf("Expected *client.Client, got %T. This is a bug in the provider.", req.ProviderData),
		)
		return
	}
	d.client = c
}

func (d *quotaDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	q, err := d.client.GetQuota(ctx)
	if err != nil {
		resp.Diagnostics.AddError(
			"Could not read fcs_quota",
			fmt.Sprintf("GET /v1/quota failed: %s", err),
		)
		return
	}

	model := quotaDataSourceModel{
		MaxConcurrentEnvironments: types.Int64Value(q.MaxConcurrentEnvironments),
		UsedEnvironments:          types.Int64Value(q.UsedEnvironments),
		MaxVMs:                    types.Int64Value(q.MaxVMs),
		UsedVMs:                   types.Int64Value(q.UsedVMs),
		MaxVCPU:                   types.Int64Value(q.MaxVCPU),
		UsedVCPU:                  types.Int64Value(q.UsedVCPU),
		MaxRAMGB:                  types.Int64Value(q.MaxRAMGB),
		UsedRAMGB:                 types.Int64Value(q.UsedRAMGB),
		MaxPublicIPs:              types.Int64Value(q.MaxPublicIPs),
		UsedPublicIPs:             types.Int64Value(q.UsedPublicIPs),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}
