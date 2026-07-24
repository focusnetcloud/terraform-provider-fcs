package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

var (
	_ resource.Resource                = (*harborRegistryBindingResource)(nil)
	_ resource.ResourceWithConfigure   = (*harborRegistryBindingResource)(nil)
	_ resource.ResourceWithImportState = (*harborRegistryBindingResource)(nil)
)

var kubernetesNamePattern = regexp.MustCompile(
	`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`,
)

// NewHarborRegistryBindingResource returns fcs_harbor_registry_binding.
func NewHarborRegistryBindingResource() resource.Resource {
	return &harborRegistryBindingResource{}
}

type harborRegistryBindingResource struct {
	client *client.Client
}

type harborRegistryBindingResourceModel struct {
	ID              types.String `tfsdk:"id"`
	EnvironmentID   types.String `tfsdk:"environment_id"`
	ClusterID       types.String `tfsdk:"cluster_id"`
	Namespace       types.String `tfsdk:"namespace"`
	SecretName      types.String `tfsdk:"secret_name"`
	ServiceAccounts types.Set    `tfsdk:"service_accounts"`
	RotationTrigger types.String `tfsdk:"rotation_trigger"`
	Project         types.String `tfsdk:"project"`
	Registry        types.String `tfsdk:"registry"`
	Status          types.String `tfsdk:"status"`
	CreatedAt       types.String `tfsdk:"created_at"`
	UpdatedAt       types.String `tfsdk:"updated_at"`
}

func (r *harborRegistryBindingResource) Metadata(
	_ context.Context,
	req resource.MetadataRequest,
	resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_harbor_registry_binding"
}

func (r *harborRegistryBindingResource) Schema(
	_ context.Context,
	_ resource.SchemaRequest,
	resp *resource.SchemaResponse,
) {
	requiresReplace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	kubernetesNameValidators := []validator.String{
		stringvalidator.LengthBetween(1, 63),
		stringvalidator.RegexMatches(
			kubernetesNamePattern,
			"must be a lowercase Kubernetes DNS label",
		),
	}
	resp.Schema = schema.Schema{
		Description: "Installs a server-managed pull-only Harbor credential in an existing tenant " +
			"cluster namespace. Credential material stays in Harbor and Kubernetes and is never " +
			"returned to or stored by Terraform.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "FCS registry binding ID (UUID).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"environment_id": schema.StringAttribute{
				Required:    true,
				Description: "Owning FCS environment UUID.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(uuidPattern, "must be a canonical UUID"),
				},
				PlanModifiers: requiresReplace,
			},
			"cluster_id": schema.StringAttribute{
				Required:    true,
				Description: "Tenant-owned business, flex or dedicated cluster UUID.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(uuidPattern, "must be a canonical UUID"),
				},
				PlanModifiers: requiresReplace,
			},
			"namespace": schema.StringAttribute{
				Required:      true,
				Description:   "Existing namespace in the tenant cluster.",
				Validators:    kubernetesNameValidators,
				PlanModifiers: requiresReplace,
			},
			"secret_name": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Default:       stringdefault.StaticString("fcs-harbor-pull"),
				Description:   "Name of the binding-owned dockerconfigjson Secret.",
				Validators:    kubernetesNameValidators,
				PlanModifiers: requiresReplace,
			},
			"service_accounts": schema.SetAttribute{
				Optional:    true,
				Computed:    true,
				ElementType: types.StringType,
				Default: setdefault.StaticValue(
					types.SetValueMust(
						types.StringType,
						[]attr.Value{types.StringValue("default")},
					),
				),
				Description: "ServiceAccounts whose imagePullSecrets receive this binding.",
				Validators: []validator.Set{
					setvalidator.SizeBetween(1, 20),
					setvalidator.NoNullValues(),
					setvalidator.ValueStringsAre(kubernetesNameValidators...),
				},
			},
			"rotation_trigger": schema.StringAttribute{
				Optional: true,
				Description: "Arbitrary provider-local marker. Changing it rotates the pull " +
					"credential entirely server-side without exposing it to Terraform state.",
			},
			"project": schema.StringAttribute{
				Computed:    true,
				Description: "Tenant Harbor project derived by the API.",
			},
			"registry": schema.StringAttribute{
				Computed:    true,
				Description: "Registry hostname.",
			},
			"status": schema.StringAttribute{
				Computed:    true,
				Description: "Binding lifecycle status.",
			},
			"created_at": schema.StringAttribute{
				Computed:    true,
				Description: "Creation timestamp.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"updated_at": schema.StringAttribute{
				Computed:    true,
				Description: "Last reconciliation timestamp.",
			},
		},
	}
}

func (r *harborRegistryBindingResource) Configure(
	_ context.Context,
	req resource.ConfigureRequest,
	resp *resource.ConfigureResponse,
) {
	if req.ProviderData == nil {
		return
	}
	apiClient, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected resource configure type",
			fmt.Sprintf("Expected *client.Client, got %T. This is a bug in the provider.", req.ProviderData),
		)
		return
	}
	r.client = apiClient
}

func bindingServiceAccounts(
	ctx context.Context,
	value types.Set,
) ([]string, error) {
	var serviceAccounts []string
	diagnostics := value.ElementsAs(ctx, &serviceAccounts, false)
	if diagnostics.HasError() {
		return nil, fmt.Errorf("decoding service_accounts: %s", diagnostics)
	}
	return serviceAccounts, nil
}

func (r *harborRegistryBindingResource) Create(
	ctx context.Context,
	req resource.CreateRequest,
	resp *resource.CreateResponse,
) {
	var plan harborRegistryBindingResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	serviceAccounts, err := bindingServiceAccounts(ctx, plan.ServiceAccounts)
	if err != nil {
		resp.Diagnostics.AddError("Invalid service_accounts", err.Error())
		return
	}
	binding, err := r.client.CreateHarborRegistryBinding(
		ctx,
		client.HarborRegistryBindingSpec{
			EnvironmentID:   plan.EnvironmentID.ValueString(),
			ClusterID:       plan.ClusterID.ValueString(),
			Namespace:       plan.Namespace.ValueString(),
			SecretName:      plan.SecretName.ValueString(),
			ServiceAccounts: serviceAccounts,
		},
	)
	if err != nil {
		resp.Diagnostics.AddError(
			"Could not create fcs_harbor_registry_binding",
			fmt.Sprintf("POST /v1/registry/bindings failed: %s", err),
		)
		return
	}
	applyHarborRegistryBinding(binding, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *harborRegistryBindingResource) Read(
	ctx context.Context,
	req resource.ReadRequest,
	resp *resource.ReadResponse,
) {
	var state harborRegistryBindingResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	binding, err := r.client.GetHarborRegistryBinding(ctx, state.ID.ValueString())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError(
			"Could not read fcs_harbor_registry_binding",
			fmt.Sprintf("GET /v1/registry/bindings/%s failed: %s", state.ID.ValueString(), err),
		)
		return
	}
	applyHarborRegistryBinding(binding, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *harborRegistryBindingResource) Update(
	ctx context.Context,
	req resource.UpdateRequest,
	resp *resource.UpdateResponse,
) {
	var plan harborRegistryBindingResourceModel
	var state harborRegistryBindingResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	serviceAccounts, err := bindingServiceAccounts(ctx, plan.ServiceAccounts)
	if err != nil {
		resp.Diagnostics.AddError("Invalid service_accounts", err.Error())
		return
	}
	binding, err := r.client.UpdateHarborRegistryBinding(
		ctx,
		state.ID.ValueString(),
		client.HarborRegistryBindingUpdate{ServiceAccounts: serviceAccounts},
	)
	if err != nil {
		resp.Diagnostics.AddError(
			"Could not update fcs_harbor_registry_binding",
			fmt.Sprintf("PATCH /v1/registry/bindings/%s failed: %s", state.ID.ValueString(), err),
		)
		return
	}
	applyHarborRegistryBinding(binding, &plan)
	if !plan.RotationTrigger.IsUnknown() &&
		!plan.RotationTrigger.Equal(state.RotationTrigger) {
		binding, err = r.client.RotateHarborRegistryBinding(
			ctx,
			state.ID.ValueString(),
		)
		if err != nil {
			resp.Diagnostics.AddError(
				"Could not rotate fcs_harbor_registry_binding",
				fmt.Sprintf(
					"POST /v1/registry/bindings/%s/rotations failed: %s",
					state.ID.ValueString(),
					err,
				),
			)
			return
		}
		applyHarborRegistryBinding(binding, &plan)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *harborRegistryBindingResource) Delete(
	ctx context.Context,
	req resource.DeleteRequest,
	resp *resource.DeleteResponse,
) {
	var state harborRegistryBindingResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.DeleteHarborRegistryBinding(
		ctx,
		state.ID.ValueString(),
	); err != nil {
		resp.Diagnostics.AddError(
			"Could not delete fcs_harbor_registry_binding",
			fmt.Sprintf("DELETE /v1/registry/bindings/%s failed: %s", state.ID.ValueString(), err),
		)
	}
}

func (r *harborRegistryBindingResource) ImportState(
	ctx context.Context,
	req resource.ImportStateRequest,
	resp *resource.ImportStateResponse,
) {
	id := strings.TrimSpace(req.ID)
	if !uuidPattern.MatchString(id) {
		resp.Diagnostics.AddError(
			"Invalid fcs_harbor_registry_binding import ID",
			"Expected a canonical UUID, for example 33333333-4444-4555-8666-777777777777.",
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
}

func applyHarborRegistryBinding(
	binding *client.HarborRegistryBinding,
	model *harborRegistryBindingResourceModel,
) {
	model.ID = types.StringValue(binding.ID)
	model.EnvironmentID = types.StringValue(binding.EnvironmentID)
	model.ClusterID = types.StringValue(binding.ClusterID)
	model.Namespace = types.StringValue(binding.Namespace)
	model.SecretName = types.StringValue(binding.SecretName)
	values := make([]attr.Value, 0, len(binding.ServiceAccounts))
	for _, serviceAccount := range binding.ServiceAccounts {
		values = append(values, types.StringValue(serviceAccount))
	}
	model.ServiceAccounts = types.SetValueMust(types.StringType, values)
	model.Project = types.StringValue(binding.Project)
	model.Registry = types.StringValue(binding.Registry)
	model.Status = types.StringValue(binding.Status)
	if binding.CreatedAt != "" {
		model.CreatedAt = types.StringValue(binding.CreatedAt)
	}
	model.UpdatedAt = nullableString(binding.UpdatedAt)
}
