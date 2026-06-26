// Package provider implements the fcs Terraform provider with
// terraform-plugin-framework.
package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

// tokenEnvVar is the environment fallback for the provider token.
const tokenEnvVar = "FCS_TOKEN"

var (
	_ provider.Provider                       = (*fcsProvider)(nil)
	_ provider.ProviderWithEphemeralResources = (*fcsProvider)(nil)
)

// New returns a provider factory; version is set at build time.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &fcsProvider{version: version}
	}
}

type fcsProvider struct {
	version string
}

type fcsProviderModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	Token    types.String `tfsdk:"token"`
}

func (p *fcsProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "fcs"
	resp.Version = p.version
}

func (p *fcsProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Interact with the FCS platform.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Required:    true,
				Description: "Base URL of the FCS API, e.g. https://api.focusnet.de.",
			},
			"token": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "Tenant-scoped FCS API token (fcs_...). May also be set via the " + tokenEnvVar + " environment variable; one of the two is required.",
			},
		},
	}
}

func (p *fcsProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config fcsProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if config.Endpoint.IsUnknown() || config.Token.IsUnknown() {
		resp.Diagnostics.AddError(
			"Unknown provider configuration",
			"The fcs provider requires endpoint and token to be known at configure time. "+
				"Set them to static values or values resolvable during plan.",
		)
		return
	}

	token := config.Token.ValueString()
	if token == "" {
		token = os.Getenv(tokenEnvVar)
	}
	if token == "" {
		resp.Diagnostics.AddError(
			"Missing FCS API token",
			"The provider requires a tenant-scoped API token. Set the \"token\" attribute "+
				"in the provider configuration or export the "+tokenEnvVar+" environment variable.",
		)
		return
	}

	c, err := client.New(
		config.Endpoint.ValueString(),
		token,
		client.WithUserAgent("terraform-provider-fcs/"+p.version),
	)
	if err != nil {
		resp.Diagnostics.AddError("Invalid provider configuration", err.Error())
		return
	}

	resp.ResourceData = c
	resp.DataSourceData = c
	resp.EphemeralResourceData = c
}

func (p *fcsProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewEnvironmentResource,
		NewBusinessClusterResource,
		NewFlexClusterResource,
		NewNamespaceResource,
		NewDedicatedClusterResource,
		NewIaasVdcResource,
		NewIaasNetworkResource,
		NewVmResource,
		NewPublishedAppResource,
		NewIngressResource,
		NewEgressResource,
	}
}

func (p *fcsProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewQuotaDataSource,
		NewImagesDataSource,
	}
}

// EphemeralResources registers values that must never be persisted to the
// Terraform plan or state (credentials). Requires Terraform >= 1.10 on the
// consumer side.
func (p *fcsProvider) EphemeralResources(_ context.Context) []func() ephemeral.EphemeralResource {
	return []func() ephemeral.EphemeralResource{
		NewClusterKubeconfigEphemeralResource,
	}
}
