package provider

// fcs_iaas_network: one VM network inside an IaaS-vDC. The API owns the
// KubeOVN/Harvester realization; Terraform manages the stable network intent.

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

var (
	_ resource.Resource              = (*iaasNetworkResource)(nil)
	_ resource.ResourceWithConfigure = (*iaasNetworkResource)(nil)
)

const defaultIaasNetworkCreateTimeout = 20 * time.Minute

// NewIaasNetworkResource returns the fcs_iaas_network resource.
func NewIaasNetworkResource() resource.Resource {
	return &iaasNetworkResource{pollInterval: defaultPollInterval}
}

type iaasNetworkResource struct {
	client       *client.Client
	pollInterval time.Duration // overridable in tests
}

type iaasNetworkResourceModel struct {
	ID                 types.String   `tfsdk:"id"`
	EnvironmentID      types.String   `tfsdk:"environment_id"`
	VdcID              types.String   `tfsdk:"vdc_id"`
	Name               types.String   `tfsdk:"name"`
	CIDR               types.String   `tfsdk:"cidr"`
	PolicyDefault      types.String   `tfsdk:"policy_default"`
	Status             types.String   `tfsdk:"status"`
	VLANID             types.Int64    `tfsdk:"vlan_id"`
	HarvesterNamespace types.String   `tfsdk:"harvester_namespace"`
	HarvesterNADName   types.String   `tfsdk:"harvester_nad_name"`
	KubeovnSubnetName  types.String   `tfsdk:"kubeovn_subnet_name"`
	DefaultGatewayIP   types.String   `tfsdk:"default_gateway_ip"`
	CreatedAt          types.String   `tfsdk:"created_at"`
	Timeouts           timeouts.Value `tfsdk:"timeouts"`
}

func (r *iaasNetworkResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_iaas_network"
}

func (r *iaasNetworkResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A VM network inside an fcs_iaas_vdc. The platform owns KubeOVN " +
			"subnet, Harvester NAD and NAT gateway realization; Terraform manages the stable network intent.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Server-assigned IaaS network ID (UUID).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"environment_id": schema.StringAttribute{
				Required: true,
				Description: "ID of the fcs_environment hosting the IaaS-vDC. " +
					"Changing it forces a new network.",
				Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"vdc_id": schema.StringAttribute{
				Required: true,
				Description: "ID of the fcs_iaas_vdc containing this network. " +
					"Changing it forces a new network.",
				Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Network name; idempotency key per vDC. Changing it forces a new network.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 63),
					stringvalidator.RegexMatches(hostnamePrefixPattern, "must be a DNS-label style name"),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"cidr": schema.StringAttribute{
				Required:    true,
				Description: "VM network CIDR. Changing it forces a new network.",
				Validators:  []validator.String{stringvalidator.LengthBetween(1, 64)},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"policy_default": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString("private"),
				Description: "Default east-west policy for this vDC network: private | allow. " +
					"Changing it forces a new network.",
				Validators: []validator.String{stringvalidator.OneOf("private", "allow")},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"status": schema.StringAttribute{
				Computed:    true,
				Description: "Lifecycle status: planned | provisioning | active | error | destroying | destroyed.",
			},
			"vlan_id": schema.Int64Attribute{
				Computed:    true,
				Description: "Platform-assigned VLAN ID once the network is realized.",
				Validators: []validator.Int64{
					int64validator.Between(1, 4094),
				},
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"harvester_namespace": schema.StringAttribute{
				Computed:    true,
				Description: "Harvester namespace for the realized network.",
			},
			"harvester_nad_name": schema.StringAttribute{
				Computed:    true,
				Description: "Harvester NetworkAttachmentDefinition name for the realized network.",
			},
			"kubeovn_subnet_name": schema.StringAttribute{
				Computed:    true,
				Description: "KubeOVN subnet name for the realized network.",
			},
			"default_gateway_ip": schema.StringAttribute{
				Computed:    true,
				Description: "Default gateway IP for VMs on this network.",
			},
			"created_at": schema.StringAttribute{
				Computed:    true,
				Description: "Creation timestamp (RFC 3339).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"timeouts": timeouts.Attributes(ctx, timeouts.Opts{
				Create: true,
				CreateDescription: fmt.Sprintf("How long to wait for the IaaS network to reach status=active "+
					"(default %s). Accepts a duration string such as \"30m\".", defaultIaasNetworkCreateTimeout),
				Delete: true,
				DeleteDescription: fmt.Sprintf("How long to wait for the network teardown to finish "+
					"(GET returns 404 or status=destroyed; default %s).", defaultDeleteTimeout),
			}),
		},
	}
}

func (r *iaasNetworkResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected resource configure type",
			fmt.Sprintf("Expected *client.Client, got %T. This is a bug in the provider.", req.ProviderData),
		)
		return
	}
	r.client = c
}

func (r *iaasNetworkResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan iaasNetworkResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	var timeoutsVal timeouts.Value
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("timeouts"), &timeoutsVal)...)
	if resp.Diagnostics.HasError() {
		return
	}
	createTimeout, diags := timeoutsVal.Create(ctx, defaultIaasNetworkCreateTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	spec := client.IaasNetworkSpec{
		Name:          plan.Name.ValueString(),
		CIDR:          plan.CIDR.ValueString(),
		PolicyDefault: plan.PolicyDefault.ValueString(),
	}
	created, err := r.client.CreateIaasNetwork(
		ctx,
		plan.EnvironmentID.ValueString(),
		plan.VdcID.ValueString(),
		spec,
	)
	if err != nil {
		resp.Diagnostics.AddError(
			"Could not create fcs_iaas_network",
			fmt.Sprintf("POST /v1/environments/%s/iaas-vdcs/%s/networks for name %q failed: %s",
				plan.EnvironmentID.ValueString(), plan.VdcID.ValueString(), spec.Name, err),
		)
		return
	}

	resp.State.Raw = req.Plan.Raw
	applyIaasNetwork(ctx, &resp.State, created, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	final, err := waitForIaasNetworkActive(
		ctx,
		r.client,
		plan.EnvironmentID.ValueString(),
		plan.VdcID.ValueString(),
		created.ID,
		r.pollInterval,
		createTimeout,
	)
	if err != nil {
		if final != nil {
			applyIaasNetwork(ctx, &resp.State, final, &resp.Diagnostics)
		}
		resp.Diagnostics.AddError(
			"fcs_iaas_network did not become active",
			fmt.Sprintf("IaaS network %s in environment %s/vDC %s: %s",
				created.ID, plan.EnvironmentID.ValueString(), plan.VdcID.ValueString(), err),
		)
		return
	}

	applyIaasNetwork(ctx, &resp.State, final, &resp.Diagnostics)
}

func (r *iaasNetworkResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var envID, vdcID, id string
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("environment_id"), &envID)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("vdc_id"), &vdcID)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("id"), &id)...)
	if resp.Diagnostics.HasError() {
		return
	}
	network, err := r.client.GetIaasNetwork(ctx, envID, vdcID, id)
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError(
			"Could not read fcs_iaas_network",
			fmt.Sprintf("GET /v1/environments/%s/iaas-vdcs/%s/networks/%s failed: %s",
				envID, vdcID, id, err),
		)
		return
	}
	if network.Status == "destroyed" {
		resp.State.RemoveResource(ctx)
		return
	}
	applyIaasNetwork(ctx, &resp.State, network, &resp.Diagnostics)
}

func (r *iaasNetworkResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError(
		"fcs_iaas_network does not support in-place updates",
		"All configurable attributes require replacement; Terraform should never call Update. This is a bug in the provider.",
	)
}

func (r *iaasNetworkResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var envID, vdcID, id string
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("environment_id"), &envID)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("vdc_id"), &vdcID)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("id"), &id)...)
	var timeoutsVal timeouts.Value
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("timeouts"), &timeoutsVal)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteTimeout, diags := timeoutsVal.Delete(ctx, defaultDeleteTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.DeleteIaasNetwork(ctx, envID, vdcID, id); err != nil {
		resp.Diagnostics.AddError(
			"Could not delete fcs_iaas_network",
			fmt.Sprintf("DELETE /v1/environments/%s/iaas-vdcs/%s/networks/%s failed: %s",
				envID, vdcID, id, err),
		)
		return
	}
	if err := waitForIaasNetworkGone(ctx, r.client, envID, vdcID, id, r.pollInterval, deleteTimeout); err != nil {
		resp.Diagnostics.AddError(
			"fcs_iaas_network teardown did not finish",
			fmt.Sprintf("IaaS network %s in environment %s/vDC %s: %s", id, envID, vdcID, err),
		)
	}
}

func applyIaasNetwork(ctx context.Context, state *tfsdk.State, network *client.IaasNetwork, diags *diag.Diagnostics) {
	diags.Append(state.SetAttribute(ctx, path.Root("id"), network.ID)...)
	diags.Append(state.SetAttribute(ctx, path.Root("status"), network.Status)...)
	diags.Append(state.SetAttribute(ctx, path.Root("policy_default"), network.PolicyDefault)...)
	if network.VLANID == nil {
		diags.Append(state.SetAttribute(ctx, path.Root("vlan_id"), types.Int64Null())...)
	} else {
		diags.Append(state.SetAttribute(ctx, path.Root("vlan_id"), types.Int64Value(*network.VLANID))...)
	}
	setNullableString(ctx, state, "harvester_namespace", network.HarvesterNamespace, diags)
	setNullableString(ctx, state, "harvester_nad_name", network.HarvesterNADName, diags)
	setNullableString(ctx, state, "kubeovn_subnet_name", network.KubeovnSubnetName, diags)
	setNullableString(ctx, state, "default_gateway_ip", network.DefaultGatewayIP, diags)
	setNullableString(ctx, state, "created_at", network.CreatedAt, diags)
}
