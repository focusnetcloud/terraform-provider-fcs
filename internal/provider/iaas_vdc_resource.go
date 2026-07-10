package provider

// fcs_iaas_vdc: service container for standalone IaaS VM networks. The
// backend owns all Harvester/KubeOVN mutations; Terraform only manages the
// stable vDC intent and reads computed gateway metadata.

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

var (
	_ resource.Resource              = (*iaasVdcResource)(nil)
	_ resource.ResourceWithConfigure = (*iaasVdcResource)(nil)
)

// NewIaasVdcResource returns the fcs_iaas_vdc resource.
func NewIaasVdcResource() resource.Resource {
	return &iaasVdcResource{}
}

type iaasVdcResource struct {
	client *client.Client
}

type iaasVdcResourceModel struct {
	ID             types.String `tfsdk:"id"`
	EnvironmentID  types.String `tfsdk:"environment_id"`
	Name           types.String `tfsdk:"name"`
	Labels         types.Map    `tfsdk:"labels"`
	Status         types.String `tfsdk:"status"`
	IaasVdcSeq     types.Int64  `tfsdk:"iaas_vdc_seq"`
	GatewayScope   types.String `tfsdk:"gateway_scope"`
	ScopeKey       types.String `tfsdk:"scope_key"`
	GatewayVPCName types.String `tfsdk:"gateway_vpc_name"`
	GatewayName    types.String `tfsdk:"gateway_name"`
	CreatedAt      types.String `tfsdk:"created_at"`
}

func (r *iaasVdcResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_iaas_vdc"
}

func (r *iaasVdcResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "An IaaS-vDC service container for standalone Harvester VMs. " +
			"The platform owns NAT gateway and KubeOVN state; Terraform manages the stable vDC intent.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Server-assigned IaaS-vDC ID (UUID).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"environment_id": schema.StringAttribute{
				Required: true,
				Description: "ID of the fcs_environment hosting this IaaS-vDC. " +
					"Changing it forces a new vDC.",
				Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Required: true,
				Description: "IaaS-vDC name; idempotency key per environment. " +
					"Changing it forces a new vDC.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 63),
					stringvalidator.RegexMatches(hostnamePrefixPattern, "must be a DNS-label style name"),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"labels": schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "Free-form labels. Changing them forces a new vDC.",
				PlanModifiers: []planmodifier.Map{
					mapplanmodifier.RequiresReplace(),
				},
			},
			"status": schema.StringAttribute{
				Computed:    true,
				Description: "Lifecycle status: planned | provisioning | active | error | destroying | destroyed.",
			},
			"iaas_vdc_seq": schema.Int64Attribute{
				Computed:    true,
				Description: "Short platform sequence used for service-scoped NAT gateway names.",
				Validators: []validator.Int64{
					int64validator.AtLeast(1),
				},
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"gateway_scope": schema.StringAttribute{
				Computed:    true,
				Description: "Gateway scope, always iaas_vdc for this resource.",
			},
			"scope_key": schema.StringAttribute{
				Computed:    true,
				Description: "NAT gateway scope key for this vDC.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"gateway_vpc_name": schema.StringAttribute{
				Computed:    true,
				Description: "KubeOVN VPC name owned by this vDC service gateway.",
			},
			"gateway_name": schema.StringAttribute{
				Computed:    true,
				Description: "KubeOVN VpcNatGateway name owned by this vDC.",
			},
			"created_at": schema.StringAttribute{
				Computed:    true,
				Description: "Creation timestamp (RFC 3339).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *iaasVdcResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *iaasVdcResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan iaasVdcResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	labels := map[string]string{}
	if !plan.Labels.IsNull() && !plan.Labels.IsUnknown() {
		resp.Diagnostics.Append(plan.Labels.ElementsAs(ctx, &labels, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}
	spec := client.IaasVdcSpec{Name: plan.Name.ValueString(), Labels: labels}
	created, err := r.client.CreateIaasVdc(ctx, plan.EnvironmentID.ValueString(), spec)
	if err != nil {
		resp.Diagnostics.AddError(
			"Could not create fcs_iaas_vdc",
			fmt.Sprintf("POST /v1/environments/%s/iaas-vdcs for name %q failed: %s",
				plan.EnvironmentID.ValueString(), spec.Name, err),
		)
		return
	}

	applyIaasVdc(created, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *iaasVdcResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state iaasVdcResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	vdc, err := r.client.GetIaasVdc(ctx, state.EnvironmentID.ValueString(), state.ID.ValueString())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError(
			"Could not read fcs_iaas_vdc",
			fmt.Sprintf("GET /v1/environments/%s/iaas-vdcs/%s failed: %s",
				state.EnvironmentID.ValueString(), state.ID.ValueString(), err),
		)
		return
	}
	if vdc.Status == "destroyed" {
		resp.State.RemoveResource(ctx)
		return
	}
	applyIaasVdc(vdc, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *iaasVdcResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError(
		"fcs_iaas_vdc does not support in-place updates",
		"All configurable attributes require replacement; Terraform should never call Update. This is a bug in the provider.",
	)
}

func (r *iaasVdcResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state iaasVdcResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.DeleteIaasVdc(ctx, state.EnvironmentID.ValueString(), state.ID.ValueString()); err != nil {
		resp.Diagnostics.AddError(
			"Could not delete fcs_iaas_vdc",
			fmt.Sprintf("DELETE /v1/environments/%s/iaas-vdcs/%s failed: %s",
				state.EnvironmentID.ValueString(), state.ID.ValueString(), err),
		)
	}
}

func applyIaasVdc(vdc *client.IaasVdc, model *iaasVdcResourceModel) {
	model.ID = types.StringValue(vdc.ID)
	model.Name = types.StringValue(vdc.Name)
	model.Status = types.StringValue(vdc.Status)
	model.IaasVdcSeq = types.Int64Value(vdc.IaasVdcSeq)
	model.GatewayScope = types.StringValue(vdc.GatewayScope)
	model.ScopeKey = types.StringValue(vdc.ScopeKey)
	model.GatewayVPCName = types.StringValue(vdc.GatewayVPCName)
	model.GatewayName = types.StringValue(vdc.GatewayName)
	model.CreatedAt = types.StringValue(vdc.CreatedAt)
}

func setNullableString(ctx context.Context, state *tfsdk.State, attr string, value string, diags *diag.Diagnostics) {
	if value == "" {
		diags.Append(state.SetAttribute(ctx, path.Root(attr), types.StringNull())...)
		return
	}
	diags.Append(state.SetAttribute(ctx, path.Root(attr), types.StringValue(value))...)
}
