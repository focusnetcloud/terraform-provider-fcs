package provider

// fcs_cluster_kubeconfig mints cluster-scoped credentials via
// POST /v1/environments/{id}/clusters/{clusterId}/kubeconfig.
//
// It is deliberately an EPHEMERAL RESOURCE (not a data source): ephemeral
// values are never persisted to the Terraform plan or state, so the minted
// kubeconfig/sa_token cannot leak through a state file. This matters because
// the server may return long-lived kubeconfig material; `expires_at` is
// advisory until short-lived token issuance is available for every backend.
// Each Open mints fresh credentials; they are sensitive and never logged.

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

var (
	_ ephemeral.EphemeralResource              = (*clusterKubeconfigEphemeralResource)(nil)
	_ ephemeral.EphemeralResourceWithConfigure = (*clusterKubeconfigEphemeralResource)(nil)
)

// NewClusterKubeconfigEphemeralResource returns the fcs_cluster_kubeconfig
// ephemeral resource.
func NewClusterKubeconfigEphemeralResource() ephemeral.EphemeralResource {
	return &clusterKubeconfigEphemeralResource{}
}

type clusterKubeconfigEphemeralResource struct {
	client *client.Client
}

type clusterKubeconfigEphemeralModel struct {
	EnvironmentID types.String `tfsdk:"environment_id"`
	ClusterID     types.String `tfsdk:"cluster_id"`
	APIServerURL  types.String `tfsdk:"api_server_url"`
	Kubeconfig    types.String `tfsdk:"kubeconfig"`
	SAToken       types.String `tfsdk:"sa_token"`
	ExpiresAt     types.String `tfsdk:"expires_at"`
}

func (r *clusterKubeconfigEphemeralResource) Metadata(_ context.Context, req ephemeral.MetadataRequest, resp *ephemeral.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cluster_kubeconfig"
}

func (r *clusterKubeconfigEphemeralResource) Schema(_ context.Context, _ ephemeral.SchemaRequest, resp *ephemeral.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Mints cluster-scoped credentials for a cluster in an environment. " +
			"This is an **ephemeral resource** (requires Terraform >= 1.10): the credentials are " +
			"held in memory for the duration of the run and are **never written to the Terraform " +
			"plan or state**. Use the result only as an ephemeral input, e.g. for `kubernetes`/" +
			"`helm` provider configuration blocks (provider configurations accept ephemeral " +
			"values).\n\n" +
			"**Security note:** depending on backend capabilities, minted credentials may " +
			"remain valid beyond the current Terraform run. Treat `expires_at` as an " +
			"advisory timestamp unless your FCS tenant explicitly documents enforced " +
			"credential expiry. The ephemeral, never-persisted delivery path is mandatory " +
			"for these credentials.",
		Attributes: map[string]schema.Attribute{
			"environment_id": schema.StringAttribute{
				Required:    true,
				Description: "ID of the fcs_environment hosting the cluster.",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"cluster_id": schema.StringAttribute{
				Required:    true,
				Description: "ID of the cluster (fcs_business_cluster / fcs_flex_cluster / fcs_dedicated_cluster). Shared namespaces have no independent Kubernetes API server.",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"api_server_url": schema.StringAttribute{
				Computed:    true,
				Description: "Kube-API endpoint the credentials are valid for.",
			},
			"kubeconfig": schema.StringAttribute{
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "Rendered kubeconfig. Sensitive and ephemeral — never " +
					"persisted to plan/state.",
			},
			"sa_token": schema.StringAttribute{
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "Service-account bearer token. Sensitive and ephemeral; " +
					"null when the backend does not return one.",
			},
			"expires_at": schema.StringAttribute{
				Computed: true,
				Description: "Expiry timestamp of the minted credentials (RFC 3339). " +
					"May be advisory depending on backend capabilities.",
			},
		},
	}
}

func (r *clusterKubeconfigEphemeralResource) Configure(_ context.Context, req ephemeral.ConfigureRequest, resp *ephemeral.ConfigureResponse) {
	if req.ProviderData == nil {
		return // provider not configured yet (e.g. during validate)
	}
	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected ephemeral resource configure type",
			fmt.Sprintf("Expected *client.Client, got %T. This is a bug in the provider.", req.ProviderData),
		)
		return
	}
	r.client = c
}

func (r *clusterKubeconfigEphemeralResource) Open(ctx context.Context, req ephemeral.OpenRequest, resp *ephemeral.OpenResponse) {
	var config clusterKubeconfigEphemeralModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	envID := config.EnvironmentID.ValueString()
	clusterID := config.ClusterID.ValueString()

	creds, err := r.client.MintKubeconfig(ctx, envID, clusterID)
	if err != nil {
		resp.Diagnostics.AddError(
			"Could not mint fcs_cluster_kubeconfig",
			fmt.Sprintf("POST /v1/environments/%s/clusters/%s/kubeconfig failed: %s", envID, clusterID, err),
		)
		return
	}
	if creds.Kubeconfig == "" && creds.SAToken == "" {
		resp.Diagnostics.AddError(
			"Empty fcs_cluster_kubeconfig response",
			fmt.Sprintf("The kubeconfig endpoint for cluster %s returned neither a kubeconfig nor an sa_token.", clusterID),
		)
		return
	}

	config.APIServerURL = types.StringValue(creds.APIServerURL)
	config.ExpiresAt = types.StringValue(creds.ExpiresAt)
	config.Kubeconfig = types.StringNull()
	if creds.Kubeconfig != "" {
		config.Kubeconfig = types.StringValue(creds.Kubeconfig)
	}
	config.SAToken = types.StringNull()
	if creds.SAToken != "" {
		config.SAToken = types.StringValue(creds.SAToken)
	}

	resp.Diagnostics.Append(resp.Result.Set(ctx, &config)...)
}
